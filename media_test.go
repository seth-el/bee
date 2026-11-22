package bee

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

type recordedMediaCommand struct {
	name string
	args []string
}

type fakeMediaRunner struct {
	mu            sync.Mutex
	calls         []recordedMediaCommand
	probeDuration string
}

func (runner *fakeMediaRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	runner.calls = append(runner.calls, recordedMediaCommand{name: name, args: append([]string(nil), args...)})
	runner.mu.Unlock()
	switch name {
	case "yt-dlp":
		if err := os.WriteFile(filepath.Join(dir, "source.webm"), []byte("source-video"), 0o600); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "source.jpg"), data, 0o600); err != nil {
			return nil, err
		}
	case "ffmpeg":
		if len(args) == 0 {
			return nil, errors.New("missing ffmpeg output")
		}
		if err := os.WriteFile(args[len(args)-1], []byte("browser-video"), 0o600); err != nil {
			return nil, err
		}
	case "ffprobe":
		duration := runner.probeDuration
		if duration == "" {
			duration = "2"
		}
		return []byte(`{"streams":[{"codec_type":"video"}],"format":{"duration":"` + duration + `"}}`), nil
	default:
		return nil, errors.New("unexpected command " + name)
	}
	return nil, nil
}

func newMediaTestStore(t *testing.T) (*SessionStore, *ArtifactStore, SessionID) {
	t.Helper()
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "media"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	return sessions, artifacts, session.ID
}

func TestValidatePublicMediaURLRejectsPrivateAndUnsafeTargets(t *testing.T) {
	for _, raw := range []string{
		"file:///tmp/image.png",
		"http://127.0.0.1/image.png",
		"http://169.254.169.254/latest/meta-data",
		"https://user:pass@example.com/image.png",
	} {
		if _, err := validatePublicMediaURL(raw); err == nil {
			t.Fatalf("unsafe media URL accepted: %q", raw)
		}
	}
}

func TestMediaImageFetchIsBoundedAndCreatesImmutableArtifact(t *testing.T) {
	payload, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "image/png; charset=utf-8")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	_, artifacts, sessionID := newMediaTestStore(t)
	service := NewMediaService(artifacts)
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	result, err := service.Execute(context.Background(), sessionID, "run-image", MediaCommand{Kind: CommandImage, URL: server.URL + "/bee.png"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Kind != "binary" || result[0].MediaType != "image/png" || result[0].Filename != "bee.png" {
		t.Fatalf("image result = %#v", result)
	}
	if _, _, err := artifacts.OpenPayload(result[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestMediaImageRejectsNonImageAndOversizedResponses(t *testing.T) {
	_, artifacts, sessionID := newMediaTestStore(t)
	service := NewMediaService(artifacts)
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	for _, test := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "text", contentType: "text/plain", body: []byte("not an image")},
		{name: "oversized", contentType: "image/png", body: make([]byte, MaxMediaImageBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				_, _ = writer.Write(test.body)
			}))
			defer server.Close()
			_, err := service.Execute(context.Background(), sessionID, "run-image", MediaCommand{Kind: CommandImage, URL: server.URL + "/image"}, nil)
			if err == nil {
				t.Fatal("unsafe image response was accepted")
			}
		})
	}
}

func TestMediaVideoUsesFixedBoundedCommandsAndIngestsArtifacts(t *testing.T) {
	_, artifacts, sessionID := newMediaTestStore(t)
	runner := &fakeMediaRunner{}
	service := NewMediaService(artifacts)
	service.CommandRun = runner
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	result, err := service.Execute(context.Background(), sessionID, "run-video", MediaCommand{Kind: CommandVideo, URL: "https://example.com/video"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].MediaType != "video/mp4" || result[0].Filename != "video.mp4" {
		t.Fatalf("video artifacts = %#v", result)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 4 || runner.calls[0].name != "yt-dlp" || runner.calls[1].name != "ffprobe" || runner.calls[2].name != "ffmpeg" || runner.calls[3].name != "ffprobe" {
		t.Fatalf("media calls = %#v", runner.calls)
	}
	joined := strings.Join(runner.calls[0].args, "\x00")
	for _, marker := range []string{"--no-playlist", "--no-progress", "-f", "height<=240", "filesize_approx<=" + strconv.FormatInt(MaxMediaVideoBytes, 10), "ba[abr<=64]", "--max-filesize", "--restrict-filenames"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("yt-dlp args missing %q: %#v", marker, runner.calls[0].args)
		}
	}
	if strings.Contains(joined, "--write-thumbnail") || strings.Contains(joined, "--convert-thumbnails") {
		t.Fatalf("yt-dlp thumbnail generation was not removed: %#v", runner.calls[0].args)
	}
	for _, arg := range runner.calls[0].args {
		if arg == "sh" || arg == "-c" || arg == "bash" {
			t.Fatalf("video command unexpectedly contains shell execution markers: %#v", runner.calls[0].args)
		}
	}
}

func TestMediaCommandErrorExcerptIsBoundedAndSanitized(t *testing.T) {
	data := []byte("ERROR: Requested format is not available; url=https://example.com/watch?v=secret token=abc123 path=/tmp/bee-video/source.webm " + strings.Repeat("x", MaxMediaErrorExcerptBytes+100))
	excerpt := mediaCommandErrorExcerpt(data)
	if len(excerpt) > MaxMediaErrorExcerptBytes+3 {
		t.Fatalf("excerpt exceeds cap: %d", len(excerpt))
	}
	for _, forbidden := range []string{"https://example.com", "abc123", "/tmp/bee-video"} {
		if strings.Contains(excerpt, forbidden) {
			t.Fatalf("excerpt leaked %q: %s", forbidden, excerpt)
		}
	}
	if !strings.Contains(excerpt, "Requested format is not available") {
		t.Fatalf("excerpt lost useful tool reason: %s", excerpt)
	}
}

func TestMediaVideoRejectsDurationBeforeTranscoding(t *testing.T) {
	_, artifacts, sessionID := newMediaTestStore(t)
	runner := &fakeMediaRunner{probeDuration: strconv.Itoa(int(MaxMediaDuration.Seconds()) + 1)}
	service := NewMediaService(artifacts)
	service.CommandRun = runner
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	if _, err := service.Execute(context.Background(), sessionID, "run-video", MediaCommand{Kind: CommandVideo, URL: "https://example.com/video"}, nil); err == nil {
		t.Fatal("over-duration video was accepted")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, call := range runner.calls {
		if call.name == "ffmpeg" {
			t.Fatalf("ffmpeg ran before duration validation: %#v", runner.calls)
		}
	}
}

func TestMediaOutputValidationRejectsPathEscapeAndSymlink(t *testing.T) {
	workdir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateRegularMediaFile(workdir, outside); err == nil {
		t.Fatal("path outside private workdir accepted")
	}
	inside := filepath.Join(workdir, "video.mp4")
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	if err := validateRegularMediaFile(workdir, inside); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func TestApplicationMediaCommandEmitsProgressAndArtifactEvents(t *testing.T) {
	payload, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	sessions, artifacts, sessionID := newMediaTestStore(t)
	app, err := NewApplication(DefaultConfig(), &commandCountingProvider{}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	service := NewMediaService(artifacts)
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	app.ArtifactStore = artifacts
	app.Media = service
	events, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "/image " + server.URL + "/bee.png", MediaEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawLocal, sawProgress, sawArtifact, sawEnd bool
	for event := range events {
		switch event.Type {
		case RunLocalCommand:
			sawLocal = event.Command != nil && event.Command.Media != nil
		case RunMediaProgress:
			sawProgress = true
		case RunArtifact:
			sawArtifact = event.Artifact != nil && event.Artifact.MediaType == "image/png"
		case RunEnd:
			sawEnd = true
			if event.Error != "" {
				t.Fatalf("media run failed: %s", event.Error)
			}
		}
	}
	if !sawLocal || !sawProgress || !sawArtifact || !sawEnd {
		t.Fatalf("media lifecycle local=%t progress=%t artifact=%t end=%t", sawLocal, sawProgress, sawArtifact, sawEnd)
	}
}

func TestApplicationMediaFailureEmitsFailureAndEnd(t *testing.T) {
	sessions, _, sessionID := newMediaTestStore(t)
	app, err := NewApplication(DefaultConfig(), &commandCountingProvider{}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "/image https://example.com/image.png", MediaEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure, sawEnd bool
	for event := range events {
		if event.Type == RunFailure {
			sawFailure = event.Error != ""
		}
		if event.Type == RunEnd {
			sawEnd = event.Error != ""
		}
	}
	if !sawFailure || !sawEnd {
		t.Fatalf("media failure lifecycle failure=%t end=%t", sawFailure, sawEnd)
	}
}
