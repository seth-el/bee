package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScratchpadPersistsExpiresAndScopesToSession(t *testing.T) {
	root := t.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	one, err := sessions.Create(SessionMetadata{ID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := sessions.Create(SessionMetadata{ID: "two"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewScratchpadStore(filepath.Join(root, "scratchpad"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	ctxOne := withArtifactContext(context.Background(), one.ID, "run")
	if _, err := store.Set(ctxOne, "draft", "hello", time.Hour); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Get(ctxOne, "draft")
	if err != nil || entry.Value != "hello" {
		t.Fatalf("entry=%#v err=%v", entry, err)
	}
	ctxTwo := withArtifactContext(context.Background(), two.ID, "run")
	if _, err := store.Get(ctxTwo, "draft"); !errors.Is(err, ErrScratchpadNotFound) {
		t.Fatalf("session isolation err=%v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := store.Get(ctxOne, "draft"); !errors.Is(err, ErrScratchpadNotFound) {
		t.Fatalf("expiry err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "scratchpad", "one.json")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "scratchpad", "one.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode=%v", st.Mode().Perm())
	}
}

func TestScratchpadRejectsMissingContextSecretsAndUnknownArgs(t *testing.T) {
	root := t.TempDir()
	sessions, _ := NewSessionStore(filepath.Join(root, "sessions"))
	session, _ := sessions.Create(SessionMetadata{ID: "s"})
	store, _ := NewScratchpadStore(filepath.Join(root, "scratch"), sessions)
	if _, err := store.Set(context.Background(), "k", "v", 0); !errors.Is(err, ErrScratchpadSessionRequired) {
		t.Fatalf("missing context err=%v", err)
	}
	ctx := withArtifactContext(context.Background(), session.ID, "run")
	if _, err := store.Set(ctx, "k", "api_key=secret", 0); err == nil {
		t.Fatal("secret accepted")
	}
	tool := ScratchSetTool{Store: store}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"key":"k","value":"v","extra":true}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	if _, err := store.Set(ctx, "k", "v", ScratchpadMaxTTL+time.Second); err == nil {
		t.Fatal("oversized ttl accepted")
	}
}

func TestScratchpadSymlinkRootRejected(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	sessions, _ := NewSessionStore(filepath.Join(root, "sessions"))
	if _, err := NewScratchpadStore(filepath.Join(link, "scratch"), sessions); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}
