package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

type testSpeechRecognizer struct {
	transcript string
	err        error
	called     bool
}

func (r *testSpeechRecognizer) Recognize(_ context.Context, _ bee.VoiceInput) (string, error) {
	r.called = true
	return r.transcript, r.err
}

type testSpeechSynthesizer struct {
	output bee.VoiceOutput
	err    error
	input  string
}

func (s *testSpeechSynthesizer) Synthesize(_ context.Context, text string) (bee.VoiceOutput, error) {
	s.input = text
	return s.output, s.err
}

func validTelegramVoice() Voice {
	return Voice{FileID: "AwACAgQAAx", Duration: 3, MimeType: "audio/ogg", FileSize: 1024}
}

func validBeeVoiceInput() bee.VoiceInput {
	input, err := VoiceInputFromRaw(validTelegramVoice())
	if err != nil {
		panic(err)
	}
	return input
}

func TestVoiceInputFromRawIsDeterministicAndOpaque(t *testing.T) {
	raw := validTelegramVoice()
	first, err := VoiceInputFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := VoiceInputFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reference != second.Reference || first.ContentType != second.ContentType || first.DurationMS != second.DurationMS {
		t.Fatalf("mapping is not deterministic: %#v != %#v", first, second)
	}
	if first.Reference == "" || strings.Contains(first.Reference, raw.FileID) || strings.Contains(first.Reference, "/") || strings.Contains(first.Reference, "\\") {
		t.Fatalf("reference exposes unsafe/raw value: %q", first.Reference)
	}
	if first.ContentType != raw.MimeType || first.DurationMS != raw.Duration*1000 {
		t.Fatalf("normalized voice = %#v", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceRejectsUnsafeAndOversizedMetadata(t *testing.T) {
	cases := []Voice{
		{FileID: "../token", Duration: 1},
		{FileID: " token", Duration: 1},
		{FileID: strings.Repeat("x", MaxTelegramVoiceFileIDBytes+1), Duration: 1},
		{FileID: "valid", Duration: 0},
		{FileID: "valid", Duration: MaxTelegramVoiceDurationSec + 1},
		{FileID: "valid", Duration: 1, MimeType: "text/plain"},
		{FileID: "valid", Duration: 1, FileSize: MaxTelegramVoiceFileSize + 1},
	}
	for index, voice := range cases {
		if err := voice.Validate(); err == nil {
			t.Fatalf("case %d unexpectedly validated: %#v", index, voice)
		}
		if _, err := VoiceInputFromRaw(voice); err == nil {
			t.Fatalf("case %d unexpectedly mapped: %#v", index, voice)
		}
	}
}

func TestRecognizeValidatesOutputAndPropagatesCancellationFailureAndEmpty(t *testing.T) {
	input := validBeeVoiceInput()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	recognizer := &testSpeechRecognizer{transcript: "ignored"}
	if _, err := Recognize(cancelled, recognizer, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if recognizer.called {
		t.Fatal("recognizer called after cancellation")
	}

	providerErr := errors.New("provider failed")
	if _, err := Recognize(context.Background(), &testSpeechRecognizer{err: providerErr}, input); !errors.Is(err, providerErr) {
		t.Fatalf("provider error = %v", err)
	}
	if _, err := Recognize(context.Background(), &testSpeechRecognizer{transcript: " \t"}, input); !errors.Is(err, ErrEmptyTranscript) {
		t.Fatalf("empty transcript error = %v", err)
	}
	if got, err := Recognize(context.Background(), &testSpeechRecognizer{transcript: "  hello\n"}, input); err != nil || got != "hello" {
		t.Fatalf("normalized transcript = %q, %v", got, err)
	}
	if _, err := Recognize(context.Background(), nil, input); !errors.Is(err, ErrSpeechRecognitionUnavailable) {
		t.Fatalf("nil recognizer error = %v", err)
	}
	if _, err := Recognize(context.Background(), &testSpeechRecognizer{transcript: strings.Repeat("x", MaxTelegramTranscriptBytes+1)}, input); err == nil {
		t.Fatal("expected oversized transcript failure")
	}
}

func TestSynthesizeValidatesInputAndOutput(t *testing.T) {
	synthesizer := &testSpeechSynthesizer{output: bee.VoiceOutput{Reference: "tg-output-v1", ContentType: "audio/ogg", DurationMS: 120}}
	output, err := Synthesize(context.Background(), synthesizer, "  hello  ")
	if err != nil {
		t.Fatal(err)
	}
	if synthesizer.input != "hello" || output.Reference != "tg-output-v1" {
		t.Fatalf("input/output = %q, %#v", synthesizer.input, output)
	}
	for _, text := range []string{"", strings.Repeat("x", MaxTelegramSynthesisBytes+1), "bad\x00text"} {
		if _, err := Synthesize(context.Background(), synthesizer, text); err == nil {
			t.Fatalf("expected synthesis input failure for %q", text)
		}
	}
	invalidOutputs := []bee.VoiceOutput{
		{Reference: "/tmp/audio.ogg", ContentType: "audio/ogg"},
		{Reference: "tg-output-v1", ContentType: "text/plain"},
		{Reference: "tg-output-v1", ContentType: "audio/ogg", DurationMS: bee.MaxChannelVoiceDurationMS + 1},
	}
	for index, invalid := range invalidOutputs {
		if _, err := Synthesize(context.Background(), &testSpeechSynthesizer{output: invalid}, "hello"); err == nil {
			t.Fatalf("invalid output %d unexpectedly accepted", index)
		}
	}
	if _, err := Synthesize(context.Background(), nil, "hello"); !errors.Is(err, ErrSpeechSynthesisUnavailable) {
		t.Fatalf("nil synthesizer error = %v", err)
	}
}

func TestVoiceOnlyTelegramUpdateRemainsIgnoredByTextPoller(t *testing.T) {
	config := testConfig(t)
	update := Update{UpdateID: 101, Message: &Message{
		MessageID: 9,
		From:      &User{ID: 7},
		Chat:      Chat{ID: 42},
		Voice:     &Voice{FileID: "AwACAgQAAx", Duration: 2, MimeType: "audio/ogg", FileSize: 16},
	}}
	normalized, err := NormalizeUpdate(config, update)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized.Ignored || normalized.Reason != "unsupported or media-only message" {
		t.Fatalf("voice-only update normalized = %#v", normalized)
	}
	if _, err := VoiceInputFromMessage(*update.Message); err != nil {
		t.Fatalf("explicit voice seam mapping failed: %v", err)
	}
}

func TestVoiceDownloadIsExplicitlyDeferred(t *testing.T) {
	if !errors.Is(DownloadVoice(context.Background(), nil, validTelegramVoice()), ErrTelegramVoiceDownload) {
		t.Fatal("expected explicit no-download error")
	}
}
