package bee

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactStorePersistsImmutableMetadataPayloadAndSessionEvent(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "artifacts"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Create(context.Background(), session.ID, "run-1", ArtifactInput{Filename: "report.txt", Content: []byte("hello artifact")})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID == "" || strings.ContainsAny(artifact.ID, `/\\`) || artifact.Size != 14 || artifact.SHA256 == "" || artifact.Kind != "text" {
		t.Fatalf("artifact metadata=%#v", artifact)
	}
	loaded, err := store.Get(artifact.ID)
	if err != nil || loaded != artifact {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	_, file, err := store.OpenPayload(artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, artifact.Size)
	_, _ = file.Read(data)
	_ = file.Close()
	if string(data) != "hello artifact" {
		t.Fatalf("payload=%q", data)
	}
	loadedSession, err := sessions.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range loadedSession.EventsSnapshot() {
		if event.Type == EventArtifact {
			found = true
		}
	}
	if !found {
		t.Fatal("artifact event was not durable in session JSONL")
	}
	list, err := store.List(session.ID)
	if err != nil || len(list) != 1 || list[0].ID != artifact.ID {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

func TestArtifactStoreRejectsUnsafeNamesOversizeAndSymlinkPayload(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewArtifactStore(root, sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.txt", "sub/file.txt", "", strings.Repeat("x", MaxArtifactFilenameBytes+1)} {
		if _, err := store.Create(context.Background(), session.ID, "", ArtifactInput{Filename: name, Content: []byte("x")}); err == nil {
			t.Fatalf("unsafe filename %q was accepted", name)
		}
	}
	if _, err := store.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "large.bin", Content: make([]byte, MaxArtifactBytes+1)}); err == nil {
		t.Fatal("oversized artifact was accepted")
	}
	artifact, err := store.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "safe.txt", Content: []byte("safe")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, artifact.ID, "payload")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, artifact.ID, "payload")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OpenPayload(artifact.ID); err == nil {
		t.Fatal("symlink payload was followed")
	}
	if !errors.Is(storeErr(store, "missing"), ErrArtifactNotFound) {
		t.Fatal("missing artifact did not return ErrArtifactNotFound")
	}
}

func TestCreateArtifactToolUsesBoundSessionAndRunContext(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withArtifactContext(context.Background(), session.ID, "run-bound")
	output, err := (CreateArtifactTool{Store: store}).Execute(ctx, []byte(`{"filename":"output.md","content":"# output"}`))
	if err != nil {
		t.Fatal(err)
	}
	var artifact Artifact
	if err := json.Unmarshal([]byte(output), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.SessionID != session.ID || artifact.RunID != "run-bound" {
		t.Fatalf("tool artifact context=%#v", artifact)
	}
}

func TestArtifactStoreCreateFromFileStreamsAndHashesPrivatePayload(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "video.mp4")
	content := []byte("streamed video payload")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateFromFile(context.Background(), session.ID, "run-file", ArtifactFileInput{Path: source, Filename: "video.mp4", MediaType: "video/mp4", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Size != int64(len(content)) || artifact.MediaType != "video/mp4" || artifact.RunID != "run-file" {
		t.Fatalf("artifact=%#v", artifact)
	}
	_, payload, err := store.OpenPayload(artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	got, err := io.ReadAll(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("payload=%q", got)
	}
}

func TestArtifactStoreCreateFromFileRejectsLargeAndCleansPartialDirectory(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewArtifactStore(root, sessions)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "too-large.mp4")
	if err := os.WriteFile(source, []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateFromFile(context.Background(), session.ID, "run-file", ArtifactFileInput{Path: source, Filename: "video.mp4", MediaType: "video/mp4", MaxBytes: 2}); err == nil {
		t.Fatal("oversized file was accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial artifact directories remain: %#v", entries)
	}
	public := filepath.Join(t.TempDir(), "public.mp4")
	if err := os.WriteFile(public, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateFromFile(context.Background(), session.ID, "run-file", ArtifactFileInput{Path: public, Filename: "video.mp4", MediaType: "video/mp4", MaxBytes: 1 << 20}); err == nil {
		t.Fatal("non-private source was accepted")
	}
}

func storeErr(store *ArtifactStore, id string) error {
	_, err := store.Get(id)
	return err
}
