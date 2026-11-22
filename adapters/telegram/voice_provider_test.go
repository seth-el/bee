package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type providerCommandRunner struct{}

func (providerCommandRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(args) == 0 {
		return nil, nil, errors.New("missing args")
	}
	output := args[len(args)-1]
	if strings.HasSuffix(output, ".wav") {
		if err := os.WriteFile(output, []byte("RIFF----WAVEfmt "), 0o600); err != nil {
			return nil, nil, err
		}
	} else if strings.HasSuffix(output, ".ogg") {
		if err := os.WriteFile(output, append([]byte("OggS"), bytes.Repeat([]byte{1}, 32)...), 0o600); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, nil
}

func privateInputArtifact(t *testing.T, payload []byte) *VoiceArtifact {
	t.Helper()
	store, err := NewVoiceArtifactStore(filepath.Join(t.TempDir(), "input"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(store.Root(), ".voice-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audio")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	input := validBeeVoiceInput()
	return &VoiceArtifact{store: store, path: path, input: input, contentType: "audio/ogg", sizeBytes: int64(len(payload)), durationMS: input.DurationMS}
}

func TestGemmaArtifactRecognizerRequestAndBounds(t *testing.T) {
	artifact := privateInputArtifact(t, append([]byte("OggS"), bytes.Repeat([]byte{2}, 32)...))
	var got struct {
		Model              string          `json:"model"`
		Temperature        int             `json:"temperature"`
		MaxTokens          int             `json:"max_tokens"`
		ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs"`
		Messages           []struct {
			Content []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				InputAudio *struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-key" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":[{"type":"text","text":" hello from gemma "}]}}]}`)
	}))
	defer server.Close()
	recognizer, err := NewGemmaArtifactRecognizer(GemmaArtifactRecognizerConfig{Endpoint: server.URL, Model: "gemma-test", APIKey: "local-key", CommandRunner: providerCommandRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := recognizer.RecognizeArtifact(context.Background(), artifact)
	if err != nil || transcript != "hello from gemma" {
		t.Fatalf("transcript=%q err=%v", transcript, err)
	}
	if got.Model != "gemma-test" || got.Temperature != 0 || got.MaxTokens != DefaultGemmaMaxTokens || got.ChatTemplateKwargs["enable_thinking"] {
		t.Fatalf("request = %#v", got)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 || got.Messages[0].Content[1].InputAudio == nil || got.Messages[0].Content[1].InputAudio.Format != "wav" {
		t.Fatalf("audio request = %#v", got.Messages)
	}
	if got.Messages[0].Content[1].InputAudio.Data == "" || strings.Contains(got.Messages[0].Content[1].InputAudio.Data, artifact.Path()) {
		t.Fatal("request exposed local artifact path")
	}
}

func TestGemmaArtifactRecognizerCancellationAndProviderValidation(t *testing.T) {
	if _, err := NewGemmaArtifactRecognizer(GemmaArtifactRecognizerConfig{Endpoint: "file:///secret", Model: "x"}); err == nil {
		t.Fatal("expected endpoint validation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recognizer := &GemmaArtifactRecognizer{httpClient: http.DefaultClient, commandRunner: providerCommandRunner{}}
	if _, err := recognizer.RecognizeArtifact(ctx, privateInputArtifact(t, []byte("audio"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestOmniVoiceSynthesizerMultipartConversionAndCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "output")
	store, err := NewVoiceOutputArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	if err := os.WriteFile(refAudio, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	var refData []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		fields = map[string]string{}
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "ref_audio" {
				refData = data
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF----WAVEfmt "))
	}))
	defer server.Close()
	synthesizer, err := NewOmniVoiceSynthesizer(OmniVoiceSynthesizerConfig{Endpoint: server.URL, RefAudioPath: refAudio, RefText: "reference text", ArtifactStore: store, CommandRunner: providerCommandRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := synthesizer.Synthesize(context.Background(), "hello voice")
	if err != nil {
		t.Fatal(err)
	}
	if output.ContentType != "audio/ogg" || !strings.HasPrefix(output.Reference, "tg-voice-output-v1-") {
		t.Fatalf("output=%#v", output)
	}
	if fields["text"] != "hello voice" || fields["ref_text"] != "reference text" || fields["response_format"] != "wav" || !bytes.Equal(refData, []byte("reference")) {
		t.Fatalf("multipart fields=%#v ref=%q", fields, refData)
	}
	artifact, err := store.Resolve(output)
	if err != nil {
		t.Fatal(err)
	}
	file, err := artifact.Open()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(file)
	_ = file.Close()
	if string(data[:4]) != "OggS" {
		t.Fatalf("converted data=%q", data)
	}
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(output); err == nil {
		t.Fatal("output artifact survived cleanup")
	}
}

func TestOmniVoiceSynthesizerRejectsOversizedTextAndCancellation(t *testing.T) {
	store, _ := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	_ = os.WriteFile(refAudio, []byte("reference"), 0o600)
	synthesizer, err := NewOmniVoiceSynthesizer(OmniVoiceSynthesizerConfig{Endpoint: "http://127.0.0.1", RefAudioPath: refAudio, RefText: "ref", ArtifactStore: store, MaxTextBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synthesizer.Synthesize(context.Background(), "too long"); err == nil {
		t.Fatal("expected text bound failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := synthesizer.Synthesize(ctx, "ok"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestTTSSynthesizerMultipartPCMConversionAndCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "output")
	store, err := NewVoiceOutputArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	if err := os.WriteFile(refAudio, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "tts.lock")
	var fields map[string]string
	var refData []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		fields = map[string]string{}
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "ref_audio" {
				refData = data
			} else {
				fields[part.FormName()] = string(data)
			}
		}
		_, _ = w.Write(bytes.Repeat([]byte{0}, 4800)) // 100 ms of 24 kHz mono s16 PCM.
	}))
	defer server.Close()
	synthesizer, err := NewTTSSynthesizer(TTSSynthesizerConfig{
		Endpoint: server.URL, RefAudioPath: refAudio, RefText: "reference text", Instruction: "warm and clear.",
		CfgScale: 2.5, Seed: 7, ArtifactStore: store, CommandRunner: providerCommandRunner{}, LockPath: lockPath,
		RetryBase: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := synthesizer.Synthesize(context.Background(), "hello tts")
	if err != nil {
		t.Fatal(err)
	}
	if output.ContentType != "audio/ogg" || output.DurationMS != 100 || !strings.HasPrefix(output.Reference, "tg-voice-output-v1-") {
		t.Fatalf("output=%#v", output)
	}
	if fields["text"] != "hello tts" || fields["ref_text"] != "reference text" || fields["instruction"] != "warm and clear." || fields["cfg_scale"] != "2.5" || fields["seed"] != "7" || !bytes.Equal(refData, []byte("reference")) {
		t.Fatalf("multipart fields=%#v ref=%q", fields, refData)
	}
	artifact, err := store.Resolve(output)
	if err != nil {
		t.Fatal(err)
	}
	file, err := artifact.Open()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(file)
	_ = file.Close()
	if string(data[:4]) != "OggS" {
		t.Fatalf("converted data=%q", data)
	}
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(output); err == nil {
		t.Fatal("output artifact survived cleanup")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TTS lock remains: %v", err)
	}
}

func TestTTSSynthesizerRetries409AndReclaimsStaleLock(t *testing.T) {
	store, err := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	if err := os.WriteFile(refAudio, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "tts.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath, time.Now().Add(-2*time.Second), time.Now().Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte{0}, 4800))
	}))
	defer server.Close()
	synthesizer, err := NewTTSSynthesizer(TTSSynthesizerConfig{Endpoint: server.URL, RefAudioPath: refAudio, RefText: "ref", ArtifactStore: store, CommandRunner: providerCommandRunner{}, LockPath: lockPath, LockStale: time.Second, RetryBase: time.Millisecond, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synthesizer.Synthesize(context.Background(), "ok"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("TTS request count=%d, want 3", calls)
	}
}

func TestTTSSynthesizerBoundsAndCancellation(t *testing.T) {
	if _, err := NewTTSSynthesizer(TTSSynthesizerConfig{Endpoint: "http://127.0.0.1", RefAudioPath: "relative.wav", RefText: "ref", ArtifactStore: &VoiceOutputArtifactStore{}}); err == nil {
		t.Fatal("expected reference path validation")
	}
	store, _ := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	_ = os.WriteFile(refAudio, []byte("reference"), 0o600)
	synthesizer, err := NewTTSSynthesizer(TTSSynthesizerConfig{Endpoint: "http://127.0.0.1", RefAudioPath: refAudio, RefText: "ref", ArtifactStore: store, CommandRunner: providerCommandRunner{}, MaxTextBytes: 4, LockPath: filepath.Join(t.TempDir(), "tts.lock")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synthesizer.Synthesize(context.Background(), "too long"); err == nil {
		t.Fatal("expected text bound failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := synthesizer.Synthesize(ctx, "ok"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestTTSSynthesizerDefaultsStayInsideBeeHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "vm-user", ".bee")
	t.Setenv("BEE_HOME", home)
	store, err := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	refAudio := filepath.Join(t.TempDir(), "ref.wav")
	if err := os.WriteFile(refAudio, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	synthesizer, err := NewTTSSynthesizer(TTSSynthesizerConfig{RefAudioPath: refAudio, RefText: "ref", ArtifactStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if synthesizer.endpoint != "http://127.0.0.1:8881/v1/audio/speech" {
		t.Fatalf("default TTS endpoint=%q", synthesizer.endpoint)
	}
	wantLock := filepath.Join(home, "state", "runtime", "tts.lock")
	if synthesizer.lockPath != wantLock {
		t.Fatalf("default TTS lock=%q, want %q", synthesizer.lockPath, wantLock)
	}
}

func TestTTSRequestLockCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "tts.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withTTSRequestLock(ctx, lockPath, time.Second, time.Second, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock cancellation error=%v", err)
	}
}
