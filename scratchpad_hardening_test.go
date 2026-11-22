package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newScratchFixture(t *testing.T) (*SessionStore, *ScratchpadStore, SessionID, context.Context) {
	t.Helper()
	root := t.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{ID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDefaultScratchpadStore(sessions)
	if err != nil {
		t.Fatal(err)
	}
	return sessions, store, session.ID, withArtifactContext(context.Background(), session.ID, "run")
}

func TestScratchpadRawTTLValidationAndDefault(t *testing.T) {
	_, store, _, ctx := newScratchFixture(t)
	tool := ScratchSetTool{Store: store}
	for _, raw := range []string{`{"key":"a","value":"v","ttl_seconds":-1}`, `{"key":"a","value":"v","ttl_seconds":2147483647}`} {
		if _, err := tool.Execute(ctx, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid ttl: %s", raw)
		}
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"key":"zero","value":"v","ttl_seconds":0}`)); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Get(ctx, "zero")
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.ExpiresAt.Sub(entry.UpdatedAt); got != ScratchpadDefaultTTL {
		t.Fatalf("default ttl=%s", got)
	}
}

func TestScratchpadRestartExpiryPrunesOnDiskAndDeletedSessionFails(t *testing.T) {
	sessions, store, sessionID, ctx := newScratchFixture(t)
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	if _, err := store.Set(ctx, "ephemeral", "value", time.Hour); err != nil {
		t.Fatal(err)
	}
	second, err := NewScratchpadStore(store.Root(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	second.SetClock(func() time.Time { return now })
	if _, err := second.Get(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := second.List(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root(), string(sessionID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap scratchpadSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 0 {
		t.Fatalf("expired entry remained on disk: %#v", snap.Entries)
	}
	if err := sessions.Delete(sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := second.List(ctx); err == nil {
		t.Fatal("deleted session remained usable")
	}
}

func TestScratchpadConcurrentInstancesQuotasAndPermissions(t *testing.T) {
	sessions, store, _, ctx := newScratchFixture(t)
	second, err := NewScratchpadStore(store.Root(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+i))
			_, _ = second.Set(ctx, key, "value", time.Hour)
		}(i)
	}
	wg.Wait()
	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 8 {
		t.Fatalf("concurrent entries=%d", len(list))
	}
	large := strings.Repeat("x", ScratchpadMaxValueBytes)
	quotaErr := error(nil)
	for i := 0; i < ScratchpadMaxEntries; i++ {
		if _, err := store.Set(ctx, "large"+string(rune('a'+i)), large, time.Hour); err != nil {
			quotaErr = err
			break
		}
	}
	if quotaErr == nil {
		t.Fatal("byte quota unexpectedly accepted all entries")
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("root mode=%o", rootInfo.Mode().Perm())
	}
	path := filepath.Join(store.Root(), "fixture.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode=%o", info.Mode().Perm())
	}
	lockInfo, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode=%o", lockInfo.Mode().Perm())
	}
}

func TestScratchpadCorruptSnapshotAndSymlinkFailClosed(t *testing.T) {
	_, store, sessionID, ctx := newScratchFixture(t)
	path := filepath.Join(store.Root(), string(sessionID)+".json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{"k":{"key":"other","value":"safe","updated_at":"2026-09-22T00:00:00Z","expires_at":"2026-09-23T00:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "k"); err == nil {
		t.Fatal("map/key mismatch accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{"k":{"key":"k","value":"api_key=secret","updated_at":"2026-09-22T00:00:00Z","expires_at":"2026-09-23T00:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "k"); err == nil {
		t.Fatal("secret snapshot accepted")
	}
	_ = os.Remove(path)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "k"); err == nil {
		t.Fatal("symlink snapshot accepted")
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".lock")
	if err := os.Symlink(target, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "k"); err == nil {
		t.Fatal("symlink lock accepted")
	}
}
