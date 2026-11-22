package bee

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionRootEnv isolates a typical environment: no BEE_HOME, HOME
// and XDG_DATA_HOME set, so the legacy store is $XDG_DATA_HOME/bee/sessions.
func sessionRootEnv(t *testing.T) (configured, legacy string) {
	t.Helper()
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", "")
	t.Setenv("BEE_SESSION_ROOT", "")
	t.Setenv("XDG_DATA_HOME", data)
	return filepath.Join(home, ".bee", "state", "sessions"), filepath.Join(data, "bee", "sessions")
}

func writeSessionFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSessionRootIgnoresXDGDataHome(t *testing.T) {
	configured, legacy := sessionRootEnv(t)
	if got, err := DefaultBeeSessionRoot(); err != nil || got != configured {
		t.Fatalf("root = %q, %v; want %q", got, err, configured)
	}
	if got := LegacyBeeSessionRoot(); got != legacy {
		t.Fatalf("legacy = %q", got)
	}
	// An empty legacy directory does not pull the default back.
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := DefaultBeeSessionRoot(); got != configured {
		t.Fatalf("empty legacy store chosen: %q", got)
	}
}

func TestExplicitSessionRootOverrideWins(t *testing.T) {
	_, legacy := sessionRootEnv(t)
	writeSessionFile(t, filepath.Join(legacy, "a.jsonl"), 0o600)
	override := filepath.Join(t.TempDir(), "custom")
	t.Setenv("BEE_SESSION_ROOT", override)
	if got, err := DefaultBeeSessionRoot(); err != nil || got != override {
		t.Fatalf("override = %q, %v", got, err)
	}
	if LegacyBeeSessionRoot() != "" {
		t.Fatal("legacy store should not apply with an explicit override")
	}
	t.Setenv("BEE_SESSION_ROOT", "relative/path")
	if _, err := DefaultBeeSessionRoot(); err == nil {
		t.Fatal("relative override accepted")
	}
}

func TestLegacyStoreUsedUntilMigrated(t *testing.T) {
	configured, legacy := sessionRootEnv(t)
	writeSessionFile(t, filepath.Join(legacy, "a.jsonl"), 0o600)
	if got, _ := DefaultBeeSessionRoot(); got != legacy {
		t.Fatalf("unmigrated install should keep its store, got %q", got)
	}
	writeSessionFile(t, filepath.Join(configured, "b.jsonl"), 0o600)
	if got, _ := DefaultBeeSessionRoot(); got != configured {
		t.Fatalf("once the new store has sessions it wins, got %q", got)
	}
	roots := strings.Join(SessionStoreRoots(), "\n")
	if !strings.Contains(roots, configured) || !strings.Contains(roots, legacy) {
		t.Fatalf("guarded roots = %s", roots)
	}
}

func TestMigrateLegacySessionStoreMovesEverythingOnce(t *testing.T) {
	configured, legacy := sessionRootEnv(t)
	store, err := NewSessionStore(legacy)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "before the move"})
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID
	writeSessionFile(t, filepath.Join(legacy, string(id)+".jsonl.search.idx"), 0o600)
	writeSessionFile(t, filepath.Join(legacy, "mutations", "scope", "journal.jsonl"), 0o600)
	writeSessionFile(t, filepath.Join(legacy, "scratchpad", "x.json"), 0o600)

	dry, err := MigrateLegacySessionStore(false)
	if err != nil || dry.Status != "ready" || dry.Sessions != 1 || dry.Applied {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	if _, err := os.Stat(configured); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run touched the destination")
	}
	moved, err := MigrateLegacySessionStore(true)
	if err != nil || moved.Status != "moved" || !moved.Applied || moved.From != legacy || moved.To != configured {
		t.Fatalf("apply = %#v, %v", moved, err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy store still present")
	}
	for _, rel := range []string{string(id) + ".jsonl", string(id) + ".jsonl.search.idx", "mutations/scope/journal.jsonl", "scratchpad/x.json"} {
		info, err := os.Stat(filepath.Join(configured, rel))
		if err != nil {
			t.Fatalf("%s missing after move: %v", rel, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", rel, info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(configured); info.Mode().Perm() != 0o700 {
		t.Fatalf("store mode = %v", info.Mode().Perm())
	}
	if got, _ := DefaultBeeSessionRoot(); got != configured {
		t.Fatalf("default after move = %q", got)
	}
	reopened, err := NewDefaultSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(id); err != nil {
		t.Fatalf("session not loadable after move: %v", err)
	}
	again, err := MigrateLegacySessionStore(true)
	if err != nil || again.Status != "already_migrated" || again.Applied {
		t.Fatalf("second run = %#v, %v", again, err)
	}
}

func TestMigrateRefusesWhileLockHeldOrDestinationBusy(t *testing.T) {
	configured, legacy := sessionRootEnv(t)
	writeSessionFile(t, filepath.Join(legacy, "a.jsonl"), 0o600)
	lock, err := tryAcquireSessionFileLock(filepath.Join(legacy, "a.jsonl.lock"), ErrSessionBusy)
	if err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		if _, err := MigrateLegacySessionStore(apply); !errors.Is(err, ErrSessionStoreInUse) {
			t.Fatalf("apply=%v with held lock: %v", apply, err)
		}
	}
	if _, err := os.Stat(filepath.Join(legacy, "a.jsonl")); err != nil {
		t.Fatal("refused migration moved files")
	}
	_ = lock.Close()
	writeSessionFile(t, filepath.Join(configured, "stray.txt"), 0o600)
	if _, err := MigrateLegacySessionStore(true); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "a.jsonl")); err != nil {
		t.Fatal("refused migration moved files")
	}
}

func TestDoctorReportsLegacySessionStore(t *testing.T) {
	configured, legacy := sessionRootEnv(t)
	check := func() (CheckStatus, string) {
		var status CheckStatus
		var detail string
		addSessionStoreCheck(func(name string, s CheckStatus, d string) {
			if name == "session_store" {
				status, detail = s, d
			}
		})
		return status, detail
	}
	if status, _ := check(); status != CheckPass {
		t.Fatalf("fresh install = %v", status)
	}
	writeSessionFile(t, filepath.Join(legacy, "a.jsonl"), 0o600)
	if status, detail := check(); status != CheckWarn || !strings.Contains(detail, "bee sessions migrate") {
		t.Fatalf("unmigrated = %v %q", status, detail)
	}
	writeSessionFile(t, filepath.Join(configured, "b.jsonl"), 0o600)
	if status, detail := check(); status != CheckWarn || !strings.Contains(detail, "no longer reads") {
		t.Fatalf("leftover = %v %q", status, detail)
	}
}
