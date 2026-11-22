package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionsExportHistoryCommitsOnlyChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	t.Setenv("BEE_SESSION_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	sessions := filepath.Join(home, "state", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "s1.jsonl"), []byte(`{"type":"message","content":"token=abc123secretvalue"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		var out, errOut bytes.Buffer
		if err := runSessions([]string{"export-history", "--commit"}, &out, &errOut); err != nil {
			t.Fatalf("export: %v %s", err, errOut.String())
		}
		return out.String()
	}
	if first := run(); !strings.Contains(first, "1 written") || !strings.Contains(first, "committed ") {
		t.Fatalf("first = %s", first)
	}
	if second := run(); !strings.Contains(second, "nothing to commit") {
		t.Fatalf("second = %s", second)
	}
	history := filepath.Join(home, "history")
	log, err := exec.Command("git", "-C", history, "log", "--oneline").Output()
	if err != nil || strings.Count(strings.TrimSpace(string(log)), "\n") != 0 {
		t.Fatalf("log = %q, %v", log, err)
	}
	tracked, _ := exec.Command("git", "-C", history, "show", "HEAD:sessions/s1.jsonl").Output()
	if strings.Contains(string(tracked), "abc123secretvalue") || !strings.Contains(string(tracked), "[REDACTED]") {
		t.Fatalf("committed copy = %s", tracked)
	}
}
