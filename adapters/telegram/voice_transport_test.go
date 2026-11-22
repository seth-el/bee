package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestGetFileAndBoundedDownload(t *testing.T) {
	const token = "123:secret-token"
	payload := append([]byte("OggS"), bytes.Repeat([]byte{0x01}, 127)...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			fmt.Fprint(w, `{"ok":true,"result":{"file_id":"AwACAgQAAx","file_size":131,"file_path":"voices/a.ogg"}}`)
		case strings.HasSuffix(r.URL.Path, "/voices/a.ogg"):
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	file, err := client.GetFile(context.Background(), "AwACAgQAAx")
	if err != nil {
		t.Fatal(err)
	}
	if file.FilePath != "voices/a.ogg" || file.FileSize != int64(len(payload)) {
		t.Fatalf("file = %#v", file)
	}
	var destination bytes.Buffer
	written, err := client.DownloadFile(context.Background(), file.FilePath, &destination, int64(len(payload)))
	if err != nil || written != int64(len(payload)) || !bytes.Equal(destination.Bytes(), payload) {
		t.Fatalf("download written=%d err=%v bytes=%d", written, err, destination.Len())
	}
	if _, err := client.DownloadFile(context.Background(), file.FilePath, io.Discard, int64(len(payload)-1)); err == nil {
		t.Fatal("expected bounded download failure")
	}
	if _, err := client.GetFile(context.Background(), "../secret"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe file id error = %v", err)
	}
}

func TestGetFileRejectsMalformedAndOversizedResponsesAndPaths(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"ok":true,"result":{"file_id":"valid"}}`},
		{name: "path traversal", body: `{"ok":true,"result":{"file_id":"valid","file_path":"../outside"}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testCase.body) }))
			defer server.Close()
			client, _ := NewClient(server.URL, "123:secret-token", server.Client())
			if _, err := client.GetFile(context.Background(), "valid"); err == nil {
				t.Fatal("expected malformed getFile failure")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":` + `"` + strings.Repeat("x", 100) + `"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	client.MaxResponseBytes = 16
	if _, err := client.GetFile(context.Background(), "valid"); err == nil {
		t.Fatal("expected oversized getFile response failure")
	}
}

func TestVoiceArtifactPermissionsContainmentAndCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewVoiceArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if mode := mustFileMode(t, root); mode != 0o700 {
		t.Fatalf("root mode=%o", mode)
	}
	payload := append([]byte("OggS"), bytes.Repeat([]byte{0x02}, 31)...)
	server := voiceFileServer(payload)
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	raw := Voice{FileID: "AwACAgQAAx", Duration: 3, MimeType: "audio/ogg", FileSize: int64(len(payload))}
	artifact, err := store.Download(context.Background(), client, raw)
	if err != nil {
		t.Fatal(err)
	}
	path := artifact.Path()
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		t.Fatalf("artifact escaped root: %q", path)
	}
	if mode := mustFileMode(t, path); mode != 0o600 {
		t.Fatalf("artifact mode=%o", mode)
	}
	if artifact.SizeBytes() != int64(len(payload)) || artifact.ContentType() != "audio/ogg" {
		t.Fatalf("artifact metadata: size=%d type=%q", artifact.SizeBytes(), artifact.ContentType())
	}
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact directory still exists: %v", err)
	}
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeVoiceSuccessFailureAndCancellation(t *testing.T) {
	payload := append([]byte("OggS"), bytes.Repeat([]byte{0x03}, 31)...)
	server := voiceFileServer(payload)
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	raw := Voice{FileID: "AwACAgQAAx", Duration: 3, MimeType: "audio/ogg", FileSize: int64(len(payload))}
	root := filepath.Join(t.TempDir(), "artifacts")
	store, _ := NewVoiceArtifactStore(root)
	var seenPath string
	transcript, err := TranscribeVoice(context.Background(), client, store, ArtifactSpeechRecognizerFunc(func(_ context.Context, artifact *VoiceArtifact) (string, error) {
		seenPath = artifact.Path()
		if _, err := artifact.Open(); err != nil {
			return "", err
		}
		return "  hello from voice  ", nil
	}), raw)
	if err != nil || transcript != "hello from voice" {
		t.Fatalf("transcript=%q err=%v", transcript, err)
	}
	if seenPath == "" {
		t.Fatal("recognizer did not receive artifact")
	}
	if _, err := os.Stat(seenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("success artifact not cleaned: %v", err)
	}
	providerErr := errors.New("recognizer failed")
	if _, err := TranscribeVoice(context.Background(), client, store, ArtifactSpeechRecognizerFunc(func(context.Context, *VoiceArtifact) (string, error) { return "", providerErr }), raw); !errors.Is(err, providerErr) {
		t.Fatalf("provider error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TranscribeVoice(ctx, client, store, ArtifactSpeechRecognizerFunc(func(context.Context, *VoiceArtifact) (string, error) {
		t.Fatal("recognizer called after cancellation")
		return "", nil
	}), raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestVoiceAwareHandlerRoutesTranscriptWithoutChangingTextNormalizer(t *testing.T) {
	payload := append([]byte("OggS"), bytes.Repeat([]byte{0x04}, 31)...)
	server := voiceFileServer(payload)
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	config := testConfig(t)
	config.APIBaseURL = server.URL
	store, _ := NewVoiceArtifactStore(filepath.Join(t.TempDir(), "artifacts"))
	var handled bee.InboundMessage
	var calls atomic.Int32
	handler, err := NewVoiceAwareHandler(config, bee.ChannelHandlerFunc(func(_ context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		handled = message
		calls.Add(1)
		return bee.ChannelRouteResult{}, nil
	}), client, store, ArtifactSpeechRecognizerFunc(func(context.Context, *VoiceArtifact) (string, error) { return "voice text", nil }))
	if err != nil {
		t.Fatal(err)
	}
	update := Update{UpdateID: 501, Message: &Message{MessageID: 9, From: &User{ID: 7}, Chat: Chat{ID: 42}, Voice: &Voice{FileID: "AwACAgQAAx", Duration: 3, MimeType: "audio/ogg", FileSize: int64(len(payload))}}}
	if _, err := handler.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || handled.Text != "voice text" || handled.Voice == nil {
		t.Fatalf("handled=%#v calls=%d", handled, calls.Load())
	}
	textOnly, err := NormalizeUpdate(config, update)
	if err != nil || !textOnly.Ignored || textOnly.Reason != "unsupported or media-only message" {
		t.Fatalf("text normalizer changed: %#v err=%v", textOnly, err)
	}
}

func TestDownloadCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.DownloadFile(ctx, "voices/a.ogg", io.Discard, 1024)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download did not cancel")
	}
}

func voiceFileServer(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"AwACAgQAAx","file_size":%d,"file_path":"voices/a.ogg"}}`, len(payload))
		case strings.HasSuffix(r.URL.Path, "/voices/a.ogg"):
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
}

func mustFileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
