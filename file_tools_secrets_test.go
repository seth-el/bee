package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileToolsCannotReachBeeSecrets(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee")
	t.Setenv("BEE_HOME", home)
	secrets := filepath.Join(home, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(secrets, "provider-key")
	if err := os.WriteFile(key, []byte("needle sk-file-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(notes, []byte("needle in notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "innocent-link")
	if err := os.Symlink(secrets, link); err != nil {
		t.Fatal(err)
	}
	registry, err := NewCodingRegistry(ExecutionScope{Root: home, WriteRoot: home})
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Registry: registry, Scope: ExecutionScope{Root: home, WriteRoot: home}}
	call := func(name, args string) (ToolResult, error) {
		return executor.Execute(context.Background(), name, json.RawMessage(args))
	}
	for _, args := range []string{`{"path":` + strconvQuote(key) + `}`, `{"path":"secrets/provider-key"}`, `{"path":"innocent-link/provider-key"}`} {
		if result, err := call("read_file", args); err == nil || strings.Contains(result.Output, "canary") {
			t.Fatalf("read_file %s = %#v, %v", args, result, err)
		}
	}
	if _, err := call("write_file", `{"path":"secrets/planted","content":"x","operation":"create"}`); err == nil {
		t.Fatal("write_file into secrets succeeded")
	}
	grep, err := call("grep", `{"path":".","query":"needle"}`)
	if err != nil || strings.Contains(grep.Output, "canary") || strings.Contains(grep.Output, "provider-key") || !strings.Contains(grep.Output, "notes.txt") {
		t.Fatalf("grep = %#v, %v", grep, err)
	}
	found, err := call("find_files", `{"path":".","pattern":"*"}`)
	if err != nil || strings.Contains(found.Output, "provider-key") || !strings.Contains(found.Output, "notes.txt") {
		t.Fatalf("find_files = %#v, %v", found, err)
	}
	if listed, err := call("list_files", `{"path":"secrets"}`); err == nil || strings.Contains(listed.Output, "provider-key") {
		t.Fatalf("list_files secrets = %#v, %v", listed, err)
	}
}
