package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type failingSpeechSynthesizer struct{}

func (failingSpeechSynthesizer) Synthesize(context.Context, string) (bee.VoiceOutput, error) {
	return bee.VoiceOutput{}, errors.New("synthesis failed")
}

func TestTelegramClientSendVoiceAndChannelExplicitVoiceFallback(t *testing.T) {
	payload := append([]byte("OggS"), []byte("voice payload")...)
	store, err := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := store.Stage(context.Background(), payload, "audio/ogg", 100)
	if err != nil {
		t.Fatal(err)
	}
	var voiceCalls atomic.Int32
	var textCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendVoice"):
			voiceCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
				t.Errorf("content type=%q", r.Header.Get("Content-Type"))
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart=%v", err)
			}
			file, header, err := r.FormFile("voice")
			if err != nil {
				t.Errorf("voice part=%v", err)
			} else {
				_ = file.Close()
				if header.Filename != "voice.ogg" {
					t.Errorf("filename=%q", header.Filename)
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			textCalls.Add(1)
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":10}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "123:secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	config.APIBaseURL = server.URL
	channel, err := NewTelegramChannel(client, config, ChannelOptions{RetryDelay: 0, Sleep: func(context.Context, time.Duration) error { return nil }, VoiceOutputStore: store, FallbackToTextOnVoiceFailure: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	message := bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-n100123", ReplyTo: "tg-msg-p5", Text: "text fallback", Voice: &output}
	if err := channel.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if voiceCalls.Load() != 1 || textCalls.Load() != 1 {
		t.Fatalf("voice calls=%d text calls=%d", voiceCalls.Load(), textCalls.Load())
	}
	if _, err := store.Resolve(output); err == nil {
		t.Fatal("voice artifact was not cleaned after send")
	}
}

func TestTelegramChannelVoiceProviderFailureFallsBackToText(t *testing.T) {
	var textCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected path=%s", r.URL.Path)
		}
		textCalls.Add(1)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":11}}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	config := testConfig(t)
	config.APIBaseURL = server.URL
	store, _ := NewVoiceOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	channel, err := NewTelegramChannel(client, config, ChannelOptions{VoiceSynthesizer: failingSpeechSynthesizer{}, VoiceOutputStore: store, SynthesizeTextToVoice: true, FallbackToTextOnVoiceFailure: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Send(context.Background(), bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-n100123", Text: "fallback"}); err != nil {
		t.Fatal(err)
	}
	if textCalls.Load() != 1 {
		t.Fatalf("text calls=%d", textCalls.Load())
	}
}
