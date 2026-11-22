package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestSpeakableTextFlattensMarkdownAndBoundsLength(t *testing.T) {
	text, ok := SpeakableText("[emotion:smile] **Sure!** See [the docs](https://x.y/z) or https://a.b/c.\n\n```go\nfmt.Println(1)\n```\n- use `bee inbox`", 0)
	if !ok || strings.ContainsAny(text, "*`[]#") || strings.Contains(text, "Println") || strings.Contains(text, "https") {
		t.Fatalf("speakable = %q", text)
	}
	for _, want := range []string{"Sure!", "the docs", "a link", "code is in the text", "use bee inbox"} {
		if !strings.Contains(text, want) {
			t.Fatalf("speakable %q missing %q", text, want)
		}
	}
	long, ok := SpeakableText(strings.Repeat("This is a sentence. ", 200), 300)
	if !ok || len([]rune(long)) > 340 || !strings.HasSuffix(long, "The rest is in the text.") {
		t.Fatalf("long = %q", long)
	}
	if _, ok := SpeakableText("```\nonly code\n```", 0); !ok {
		t.Fatal("code-only reply should still say the code is in the text")
	}
	if _, ok := SpeakableText("  [emotion:smile]  ", 0); ok {
		t.Fatal("marker-only reply is not speakable")
	}
}

func TestWhisperRecognizerPostsFileAndHandlesBusy(t *testing.T) {
	busy := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if busy {
			w.WriteHeader(http.StatusConflict)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("file part: %v", err)
			return
		}
		defer file.Close()
		if header.Filename != "voice.ogg" {
			t.Errorf("filename = %q", header.Filename)
		}
		fmt.Fprint(w, `{"text":"  hello there  ","language":"en"}`)
	}))
	defer server.Close()
	store, err := NewVoiceArtifactStore(filepath.Join(t.TempDir(), "in"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), "voice.ogg")
	if err := os.WriteFile(path, []byte("OggS fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := &VoiceArtifact{store: store, path: path, contentType: "audio/ogg"}
	recognizer, err := NewWhisperRecognizer(server.URL + "/v1/audio/transcriptions")
	if err != nil {
		t.Fatal(err)
	}
	if text, err := recognizer.RecognizeArtifact(context.Background(), artifact); err != nil || text != "hello there" {
		t.Fatalf("transcript = %q, %v", text, err)
	}
	busy = true
	if _, err := recognizer.RecognizeArtifact(context.Background(), artifact); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy error = %v", err)
	}
}

type fakeFFmpeg struct {
	mu   sync.Mutex
	args []string
}

func (f *fakeFFmpeg) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	f.args = append([]string{name}, args...)
	f.mu.Unlock()
	return nil, nil, os.WriteFile(args[len(args)-1], []byte("OggS opus"), 0o600)
}

func TestTTSVoiceSynthesizerSendsVoiceIDAndStagesOgg(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("form: %v", err)
		}
		if r.FormValue("voice_id") != "narrator" || r.FormValue("text") != "Hi there." || r.FormValue("cfg_scale") != "1.6" || r.FormValue("instruction") != "Calm." {
			t.Errorf("form = %v", r.MultipartForm.Value)
		}
		w.Header().Set("X-Sample-Rate", "16000")
		w.Header().Set("X-Sample-Format", "s16le")
		_, _ = w.Write(make([]byte, 32000)) // one second at 16 kHz
	}))
	defer server.Close()
	store, err := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	synthesizer, err := NewTTSVoiceSynthesizer(server.URL, "narrator", "Calm.", 1.6, store)
	if err != nil {
		t.Fatal(err)
	}
	ffmpeg := &fakeFFmpeg{}
	synthesizer.CommandRunner = ffmpeg
	synthesizer.Sleep = func(context.Context, time.Duration) error { return nil }
	output, err := synthesizer.Synthesize(context.Background(), "Hi there.")
	if err != nil {
		t.Fatal(err)
	}
	if output.DurationMS != 1000 || output.ContentType != "audio/ogg" {
		t.Fatalf("output = %+v", output)
	}
	if got := strings.Join(ffmpeg.args, " "); !strings.Contains(got, "-ar 16000") || !strings.Contains(got, "libopus") {
		t.Fatalf("ffmpeg args = %q", got)
	}
}

type recordingSynthesizer struct {
	store *VoiceOutputArtifactStore
	texts []string
	fail  bool
}

func (r *recordingSynthesizer) Synthesize(ctx context.Context, text string) (bee.VoiceOutput, error) {
	r.texts = append(r.texts, text)
	if r.fail {
		return bee.VoiceOutput{}, errors.New("tts down")
	}
	return r.store.Stage(ctx, []byte("OggS voice"), "audio/ogg", 500)
}

func TestChannelAddsSpokenCopyOnlyForMarkedReplies(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":42},"date":1}}`)
	}))
	defer server.Close()
	config := testConfig(t)
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, _ := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "out"))
	synth := &recordingSynthesizer{store: store}
	replies := NewVoiceReplies()
	var logged []string
	channel, err := NewTelegramChannel(client, config, ChannelOptions{VoiceSynthesizer: synth, VoiceOutputStore: store, VoiceReplies: replies, VoiceErrorLog: func(line string) { logged = append(logged, line) }})
	if err != nil {
		t.Fatal(err)
	}
	inbound := bee.InboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-p42", MessageID: "tg-msg-p5", Text: "x"}
	if err := channel.SendText(context.Background(), inbound, "plain reply"); err != nil {
		t.Fatal(err)
	}
	replies.Mark(inbound.MessageID)
	if err := channel.SendText(context.Background(), inbound, "**spoken** reply"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(calls, ",")
	mu.Unlock()
	if got != "sendMessage,sendMessage,sendChatAction,sendVoice" || len(synth.texts) != 1 || synth.texts[0] != "spoken reply" {
		t.Fatalf("calls = %s, synthesized %q", got, synth.texts)
	}
	// A voice failure is logged, never returned: the text was delivered.
	synth.fail = true
	replies.Mark(inbound.MessageID)
	if err := channel.SendText(context.Background(), inbound, "again"); err != nil || len(logged) != 1 {
		t.Fatalf("send = %v, logged %q", err, logged)
	}
}

func TestVoiceConfigValidation(t *testing.T) {
	config := testConfig(t)
	config.Voice = &VoiceConfig{Enabled: true}
	if err := config.Validate(); err != nil || !config.VoiceEnabled() {
		t.Fatalf("minimal voice config: %v", err)
	}
	for _, bad := range []VoiceConfig{
		{Enabled: true, ReplyWithVoice: "sometimes"},
		{Enabled: true, MaxSpokenChars: -1},
		{Enabled: true, CfgScale: 11},
		{Enabled: true, ArtifactRoot: "relative"},
	} {
		bad := bad
		config.Voice = &bad
		if err := config.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestVoiceRepliesAreBoundedAndTakenOnce(t *testing.T) {
	replies := NewVoiceReplies()
	for i := 0; i < maxVoiceReplies+10; i++ {
		replies.Mark(bee.ChannelMessageID(fmt.Sprintf("m%d", i)))
	}
	if replies.Take("m0") {
		t.Fatal("oldest entry not evicted")
	}
	last := bee.ChannelMessageID(fmt.Sprintf("m%d", maxVoiceReplies+9))
	if !replies.Take(last) || replies.Take(last) {
		t.Fatal("take must succeed once")
	}
}

func TestTTSVoiceSynthesizerSpeaksLongRepliesWholeInParts(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("form: %v", err)
		}
		mu.Lock()
		texts = append(texts, r.FormValue("text"))
		mu.Unlock()
		w.Header().Set("X-Sample-Rate", "16000")
		_, _ = w.Write(make([]byte, 32000)) // one second per part
	}))
	defer server.Close()
	store, err := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	synthesizer, err := NewTTSVoiceSynthesizer(server.URL, "narrator", "", 0, store)
	if err != nil {
		t.Fatal(err)
	}
	synthesizer.CommandRunner = &fakeFFmpeg{}

	// About 6000 characters: four times the old 1500-character cutoff.
	paragraph := strings.Repeat("Here is one more sentence about the cluster. ", 20)
	reply := strings.TrimSpace(strings.Repeat(paragraph+"\n\n", 7)) + " Final words."
	spoken, ok := SpeakableText(reply, 0)
	if !ok || strings.Contains(spoken, "The rest is in the text") {
		t.Fatalf("default limit still truncates a %d-character reply", len(reply))
	}
	output, err := synthesizer.Synthesize(context.Background(), spoken)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) < 4 {
		t.Fatalf("parts = %d, want the reply split into several requests", len(texts))
	}
	for _, part := range texts {
		if n := len([]rune(part)); n > ttsChunkChars || n == 0 {
			t.Fatalf("part of %d runes", n)
		}
		if !strings.HasSuffix(part, ".") {
			t.Fatalf("part not cut at a sentence: %q", part[max(0, len(part)-40):])
		}
	}
	joined := strings.Join(texts, " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(spoken), " ") {
		t.Fatal("parts do not add up to the whole reply")
	}
	if output.DurationMS != int64(len(texts))*1000 {
		t.Fatalf("duration = %d ms for %d parts: audio was not joined", output.DurationMS, len(texts))
	}
}

func TestSplitSpeechHandlesUnbrokenText(t *testing.T) {
	parts := splitSpeech(strings.Repeat("x", 4000), 1500)
	if len(parts) != 3 || len(parts[0]) != 1500 || len(parts[2]) != 1000 {
		t.Fatalf("parts = %d", len(parts))
	}
	if got := splitSpeech("  short  ", 1500); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short = %q", got)
	}
}
