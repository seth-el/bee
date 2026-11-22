package bee

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sandboxTmpTestRoot returns a write root outside /tmp. A write root below
// /tmp intentionally keeps the per-invocation behavior, so these tests need
// a location elsewhere; the package directory is used and removed after.
func sandboxTmpTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".sandbox-tmp-test-")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(abs) })
	if strings.HasPrefix(abs, "/tmp/") {
		t.Skip("package directory is below /tmp")
	}
	return abs
}

func TestExecSandboxTmpPersistsWithinSessionOnly(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := sandboxTmpTestRoot(t)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: workspace, WriteRoot: workspace, EnforceWriteRoots: true, TrustedForeground: true, SandboxTmpRoot: filepath.Join(root, "sandbox-tmp")}
	run := func(ctx context.Context, tool ExecTool, command string) string {
		t.Helper()
		output, err := tool.Execute(ctx, mustJSON(map[string]any{"command": command}))
		if err != nil {
			t.Fatal(err)
		}
		return decodeExecResult(t, output).Output
	}
	write := ExecTool{Scope: scope}
	inspect := ExecTool{Scope: scope, readOnly: true}
	marker := "bee-sandbox-tmp-" + filepath.Base(root)
	sessionA := withArtifactContext(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "run-1")
	sessionB := withArtifactContext(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "run-2")

	run(sessionA, write, "printf kept > /tmp/"+marker)
	if got := run(sessionA, write, "cat /tmp/"+marker+" 2>&1"); got != "kept" {
		t.Fatalf("second exec call in the same session: %q", got)
	}
	if got := run(sessionA, inspect, "cat /tmp/"+marker+" 2>&1"); got != "kept" {
		t.Fatalf("read-only exec in the same session: %q", got)
	}
	if got := run(sessionA, inspect, "{ printf x > /tmp/ro-"+marker+"; } 2>/dev/null && printf writable || printf read-only"); got != "read-only" {
		t.Fatalf("read-only exec could write the session /tmp: %q", got)
	}
	if got := run(sessionB, write, "cat /tmp/"+marker+" 2>/dev/null || printf absent"); got != "absent" {
		t.Fatalf("another session saw the file: %q", got)
	}
	if _, err := os.Stat("/tmp/" + marker); !os.IsNotExist(err) {
		t.Fatalf("sandbox /tmp leaked into host /tmp: %v", err)
	}
	dir := filepath.Join(root, "sandbox-tmp", "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("session tmp dir=%v err=%v", info, err)
	}
	// No session in context: one directory for this process.
	run(context.Background(), write, "printf p > /tmp/"+marker)
	if _, err := os.Stat(filepath.Join(root, "sandbox-tmp", "process-"+strconv.Itoa(os.Getpid()), marker)); err != nil {
		t.Fatalf("process tmp dir: %v", err)
	}
}

func TestExecSandboxWithoutTmpRootKeepsPerCallTmpfs(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := sandboxTmpTestRoot(t)
	scope := ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true, TrustedForeground: true}
	tool := ExecTool{Scope: scope}
	for index, command := range []string{"printf x > /tmp/per-call-note", "cat /tmp/per-call-note 2>/dev/null || printf absent"} {
		output, err := tool.Execute(context.Background(), mustJSON(map[string]any{"command": command}))
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeExecResult(t, output).Output; index == 1 && got != "absent" {
			t.Fatalf("tmpfs persisted without a tmp root: %q", got)
		}
	}
}

func TestPruneSandboxTmpRemovesOnlyIdleSessionDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{"session-old": 8 * 24 * time.Hour, "session-new": time.Hour, "process-1": 9 * 24 * time.Hour, "unrelated": 30 * 24 * time.Hour} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	PruneSandboxTmp(root, MaxSandboxTmpAge, now)
	for name, kept := range map[string]bool{"session-old": false, "session-new": true, "process-1": false, "unrelated": true} {
		if _, err := os.Stat(filepath.Join(root, name)); (err == nil) != kept {
			t.Fatalf("%s kept=%v err=%v", name, kept, err)
		}
	}
}
