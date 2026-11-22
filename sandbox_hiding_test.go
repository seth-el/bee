package bee

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The write sandbox must not leave a route to an unsandboxed process: Unix
// sockets in the user runtime directory (systemd user bus, tmux, editors)
// accept connections on a read-only mount. Only an ssh-agent socket stays.
func TestExecSandboxHidesUserRuntimeDirButKeepsSSHAgent(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runtimeDir := filepath.Join(root, "run-user")
	for _, dir := range []string{workspace, filepath.Join(runtimeDir, "systemd")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bus := filepath.Join(runtimeDir, "bus")
	agent := filepath.Join(runtimeDir, "ssh-agent.sock")
	for _, path := range []string{bus, agent} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("SSH_AUTH_SOCK", agent)
	scope := execTestScope(root)
	scope.WriteRoot = workspace
	scope.EnforceWriteRoots = true
	command := "test -S " + shellSingleQuote(agent) + " && printf agent-visible; test -e " + shellSingleQuote(bus) + " && printf ' bus-visible'; test -e " + shellSingleQuote(filepath.Join(runtimeDir, "systemd")) + " && printf ' systemd-visible'; printf ' :done'"
	output, err := ExecTool{Scope: scope}.Execute(context.Background(), mustJSON(map[string]any{"command": command}))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if !strings.Contains(result.Output, "agent-visible") || strings.Contains(result.Output, "bus-visible") || strings.Contains(result.Output, "systemd-visible") || !strings.Contains(result.Output, ":done") {
		t.Fatalf("runtime dir not hidden correctly: %q", result.Output)
	}
}

// Provider keys and serve tokens are read by the harness itself, never by a
// model-driven process, so no sandboxed command may read them, even when a
// write root contains the secrets directory.
func TestSandboxedCommandsCannotReadBeeSecrets(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	home := filepath.Join(t.TempDir(), "bee")
	t.Setenv("BEE_HOME", home)
	secrets := filepath.Join(home, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(secrets, "provider-key")
	if err := os.WriteFile(key, []byte("sk-sandbox-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, writeRoot := range map[string]string{
		"disjoint write root":           filepath.Join(home, "workspace"),
		"write root containing secrets": home,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(writeRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			scope := execTestScope(home)
			scope.WriteRoot = writeRoot
			scope.EnforceWriteRoots = true
			for mode, tool := range map[string]ExecTool{"write": {Scope: scope}, "read-only": {Scope: scope, readOnly: true}} {
				output, err := tool.Execute(context.Background(), mustJSON(map[string]any{"command": "cat " + shellSingleQuote(key) + " 2>/dev/null; printf 'entries=%s' \"$(ls -A " + shellSingleQuote(secrets) + " 2>/dev/null | wc -l)\""}))
				if err != nil {
					t.Fatal(err)
				}
				result := decodeExecResult(t, output)
				if strings.Contains(result.Output, "sk-sandbox-canary") || !strings.Contains(result.Output, "entries=0") {
					t.Fatalf("%s sandbox exposed secrets: %q", mode, result.Output)
				}
			}
		})
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "sk-sandbox-canary" {
		t.Fatalf("host secret changed: %q %v", data, err)
	}
}
