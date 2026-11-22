package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A workspace restored from git comes back with 0644 files, and BEE.md or
// SOUL.md that are not owner-only make every turn fail. Legacy installs with
// XDG_DATA_HOME set used to skip the layout repair entirely.
func TestDefaultSessionStoreRepairsRestoredPromptFilesUnderXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", "")
	t.Setenv("HOME", home)
	data := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", data)
	workspace := filepath.Join(home, ".bee", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"BEE.md", "SOUL.md"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("# restored\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewDefaultSessionStore()
	if err != nil {
		t.Fatal(err)
	}
	// Since G6 XDG_DATA_HOME no longer chooses the store (no legacy
	// sessions exist here, so there is nothing to keep using).
	if store.Root() != filepath.Join(home, ".bee", "state", "sessions") {
		t.Fatalf("session root = %q", store.Root())
	}
	for path, want := range map[string]os.FileMode{
		workspace:                           0o700,
		filepath.Join(workspace, "BEE.md"):  0o600,
		filepath.Join(workspace, "SOUL.md"): 0o600,
		filepath.Join(workspace, "memory", DefaultMemoryDashboardDir): 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode=%o want %o", path, info.Mode().Perm(), want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "SOUL.md")); err != nil || string(data) != "# restored\n" {
		t.Fatalf("SOUL.md content changed: %q, %v", data, err)
	}
}

func TestLayoutNeverCreatesSoulAndRejectsSymlinkedSoul(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee")
	if _, err := ensureBeeLayoutAt(home); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(home, DefaultSystemPromptFile)
	if _, err := os.Lstat(soul); !os.IsNotExist(err) {
		t.Fatalf("layout created SOUL.md: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, soul); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureBeeLayoutAt(home); err == nil {
		t.Fatal("symlinked SOUL.md accepted")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target was modified: %v %v", info, err)
	}
}

func TestPromptFilePermissionErrorSaysHowToFix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "BEE.md")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadPrivatePromptFile(path, "Bee workspace context file", MaxWorkspaceContextFileBytes)
	if err == nil || !strings.Contains(err.Error(), "owner-only") || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err=%v", err)
	}
}

func TestDoctorShowsConfigCauseAndPermissionFixes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee")
	if _, err := ensureBeeLayoutAt(home); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{"assistant":{"requre_plan":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(home, DefaultSystemPromptFile)
	check := func(report DoctorReport, name string) DoctorCheck {
		t.Helper()
		for _, check := range report.Checks {
			if check.Name == name {
				return check
			}
		}
		t.Fatalf("doctor lacks %s: %#v", name, report.Checks)
		return DoctorCheck{}
	}
	report := RunDoctor(home, config)
	if got := check(report, "config_validity"); got.Status != CheckFail || !strings.Contains(got.Detail, `unknown field "requre_plan"`) {
		t.Fatalf("config_validity=%#v", got)
	}
	if got := check(report, "system_prompt_file"); got.Status != CheckPass || !strings.Contains(got.Detail, "optional") {
		t.Fatalf("absent SOUL=%#v", got)
	}
	if err := os.WriteFile(soul, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, "workspace", "BEE.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	report = RunDoctor(home, config)
	for _, name := range []string{"system_prompt_file", "workspace_context"} {
		if got := check(report, name); got.Status != CheckFail || !strings.Contains(got.Detail, "chmod 600 ") {
			t.Fatalf("%s=%#v", name, got)
		}
	}
	if err := os.Chmod(filepath.Join(home, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := check(RunDoctor(home, config), "workspace"); got.Status != CheckFail || !strings.Contains(got.Detail, "chmod 700 ") {
		t.Fatalf("workspace=%#v", got)
	}
}

func TestDoctorConfigCauseIsRedacted(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee")
	if _, err := ensureBeeLayoutAt(home); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.json")
	// A malformed document whose decode error could echo nearby text.
	if err := os.WriteFile(config, []byte(`{"token=sk-live-secret-value": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, check := range RunDoctor(home, config).Checks {
		if strings.Contains(check.Detail, "sk-live-secret-value") {
			t.Fatalf("doctor echoed a secret-like value: %#v", check)
		}
	}
}

func TestShellQuotePath(t *testing.T) {
	if got := shellQuotePath("/home/user/.bee/workspace/BEE.md"); got != "/home/user/.bee/workspace/BEE.md" {
		t.Fatalf("plain=%q", got)
	}
	if got := shellQuotePath("/tmp/it's here"); got != `'/tmp/it'\''s here'` {
		t.Fatalf("quoted=%q", got)
	}
}
