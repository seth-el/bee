package bee

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBeePathDefaultsUseBeeHomeAndWorkspaceMemory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	t.Setenv("BEE_HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "legacy-data"))
	gotHome, err := DefaultBeeHome()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		t.Fatal(err)
	}
	memory, err := DefaultBeeMemoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	plugins, err := DefaultBeePluginsRoot()
	if err != nil {
		t.Fatal(err)
	}
	state, err := DefaultBeeStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	config, err := DefaultBeeConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if gotHome != home || workspace != filepath.Join(home, "workspace") || memory != filepath.Join(home, "workspace", "memory") || plugins != filepath.Join(home, "plugins") || state != filepath.Join(home, "state") || config != filepath.Join(home, "config.json") {
		t.Fatalf("paths home=%q workspace=%q memory=%q plugins=%q state=%q config=%q", gotHome, workspace, memory, plugins, state, config)
	}
}

func TestResolveBeeHomePathSupportsPortableAndUserHomePaths(t *testing.T) {
	beeHome := filepath.Join(t.TempDir(), "portable", ".bee")
	userHome := filepath.Join(t.TempDir(), "user")
	t.Setenv("BEE_HOME", beeHome)
	t.Setenv("HOME", userHome)

	for input, want := range map[string]string{
		"workspace/memory":   filepath.Join(beeHome, "workspace", "memory"),
		"~/.bee/secrets/key": filepath.Join(beeHome, "secrets", "key"),
		"~/outside-bee":      filepath.Join(userHome, "outside-bee"),
		"~":                  userHome,
	} {
		got, err := ResolveBeeHomePath(input)
		if err != nil {
			t.Fatalf("resolve %q: %v", input, err)
		}
		if got != want {
			t.Fatalf("resolve %q = %q, want %q", input, got, want)
		}
	}
}

func TestEnsureBeeLayoutCreatesPrivateRoots(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	t.Setenv("BEE_HOME", home)
	if _, err := EnsureBeeLayout(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, filepath.Join(home, "skills"), filepath.Join(home, "plugins"), filepath.Join(home, "workspace"), filepath.Join(home, "workspace", "memory"), filepath.Join(home, "state"), filepath.Join(home, "state", "sessions"), filepath.Join(home, "state", "runtime"), filepath.Join(home, "secrets")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("path=%s mode=%o dir=%t", path, info.Mode().Perm(), info.IsDir())
		}
	}
}
