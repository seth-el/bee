package bee

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryRemotesCheckReportsUnpushedCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	git := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	check := func() (CheckStatus, string) {
		var status CheckStatus
		var detail string
		addHistoryRemotesCheck(home, func(name string, s CheckStatus, d string) {
			if name == "history_remotes" {
				status, detail = s, d
			}
		})
		return status, detail
	}
	if status, _ := check(); status != "" {
		t.Fatalf("no repos should add no check, got %v", status)
	}
	workspace := filepath.Join(home, "workspace")
	history := filepath.Join(home, "history")
	remote := filepath.Join(home, "remote.git")
	for _, dir := range []string{workspace, history} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git(dir, "add", "a.txt")
		git(dir, "commit", "-q", "-m", "one")
	}
	git(home, "init", "-q", "--bare", remote)
	if status, detail := check(); status != CheckPass || !strings.Contains(detail, "workspace: local only") {
		t.Fatalf("local only = %v %q", status, detail)
	}
	git(history, "remote", "add", "origin", remote)
	if status, detail := check(); status != CheckWarn || !strings.Contains(detail, "no upstream") {
		t.Fatalf("no upstream = %v %q", status, detail)
	}
	git(history, "push", "-q", "-u", "origin", "main")
	if status, detail := check(); status != CheckPass || !strings.Contains(detail, "history: pushed") {
		t.Fatalf("pushed = %v %q", status, detail)
	}
	if err := os.WriteFile(filepath.Join(history, "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(history, "add", "b.txt")
	git(history, "commit", "-q", "-m", "two")
	if status, detail := check(); status != CheckWarn || !strings.Contains(detail, "history: 1 commit(s) not pushed") {
		t.Fatalf("unpushed = %v %q", status, detail)
	}
}
