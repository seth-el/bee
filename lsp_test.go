package bee

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLSPHelperProcess(t *testing.T) {
	mode := os.Getenv("BEE_LSP_HELPER")
	if mode == "" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	write := func(value any) {
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
	}
	for {
		frame, err := readLSPFrame(reader, 4<<20)
		if err != nil {
			os.Exit(0)
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(frame, &request) != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "initialized":
			if mode == "crash" {
				os.Exit(0)
			}
		case "textDocument/didOpen", "textDocument/didChange":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(request.Params, &params)
			write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{
				"uri": params.TextDocument.URI,
				"diagnostics": []any{map[string]any{
					"range":    map[string]any{"start": map[string]int{"line": 0, "character": 0}, "end": map[string]int{"line": 0, "character": 3}},
					"severity": 2, "source": "fixture", "message": "fixture diagnostic",
				}},
			}})
		case "textDocument/hover", "textDocument/definition", "textDocument/references":
			if mode == "oversize" {
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": strings.Repeat("x", 4096)})
				continue
			}
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"fixture": request.Method}})
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": nil})
		case "exit":
			os.Exit(0)
		}
	}
}

func lspFixtureConfig(t *testing.T, root, mode string) LSPConfig {
	t.Helper()
	t.Setenv("BEE_LSP_HELPER", mode)
	return LSPConfig{Servers: []LSPServerConfig{{
		Name: "fixture", Enabled: true, Command: os.Args[0],
		Args: []string{"-test.run=^TestLSPHelperProcess$"}, WorkingDir: root,
		LanguageID: "go", FilePatterns: []string{"*.go"}, EnvAllowlist: []string{"BEE_LSP_HELPER"},
		StartupTimeoutSeconds: 2, RequestTimeoutSeconds: 2,
	}}}
}

func TestLSPPostEditDiagnosticsAndReadOnlyQueries(t *testing.T) {
	root := t.TempDir()
	scope := ExecutionScope{Root: root, WriteRoot: root}
	manager, err := NewLSPManager(lspFixtureConfig(t, root, "normal"), scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if manager.hosts[0].started {
		t.Fatal("language server started before first use")
	}
	runner, err := NewLifecycleHookRunner(LifecycleHooksConfig{})
	if err != nil {
		t.Fatal(err)
	}
	runner.SetPostToolObserver(manager.PostEditDiagnostics)
	registry, err := NewRegistry(WriteFileTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Registry: registry, Scope: scope, Hooks: runner.BindTools(nil, scope)}
	result, err := executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"main.go","content":"package main\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.HookObservation, "fixture diagnostic") {
		t.Fatalf("hook observation = %q", result.HookObservation)
	}
	if data, readErr := os.ReadFile(filepath.Join(root, "main.go")); readErr != nil || string(data) != "package main\n" {
		t.Fatalf("written file = %q, %v", data, readErr)
	}
	for _, kind := range []string{"hover", "definition", "references"} {
		tool := LSPQueryTool{Manager: manager, Kind: kind}
		output, queryErr := tool.Execute(context.Background(), json.RawMessage(`{"path":"main.go","line":0,"character":1}`))
		if queryErr != nil {
			t.Fatalf("%s: %v", kind, queryErr)
		}
		if !strings.Contains(output, "textDocument/"+kind) {
			t.Fatalf("%s output = %q", kind, output)
		}
		if tool.Definition().Mutability != ToolMutabilityReadOnly {
			t.Fatalf("%s mutability = %q", kind, tool.Definition().Mutability)
		}
	}
}

func TestLSPMissingOrCrashedServerDoesNotFailCommittedMutation(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		mode    string
	}{
		{name: "missing", command: filepath.Join(t.TempDir(), "missing-lsp"), mode: "normal"},
		{name: "crashed", command: os.Args[0], mode: "crash"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			config := lspFixtureConfig(t, root, test.mode)
			config.Servers[0].Command = test.command
			scope := ExecutionScope{Root: root, WriteRoot: root}
			manager, err := NewLSPManager(config, scope)
			if err != nil {
				t.Fatal(err)
			}
			runner, _ := NewLifecycleHookRunner(LifecycleHooksConfig{})
			runner.SetPostToolObserver(manager.PostEditDiagnostics)
			registry, _ := NewRegistry(WriteFileTool{Scope: scope})
			executor := Executor{Registry: registry, Scope: scope, Hooks: runner.BindTools(nil, scope)}
			result, executeErr := executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"main.go","content":"package main\n"}`))
			if executeErr != nil {
				t.Fatalf("write failed because LSP failed: %v", executeErr)
			}
			if result.HookObservation != "" {
				t.Fatalf("unexpected failed-LSP observation %q", result.HookObservation)
			}
			if _, statErr := os.Stat(filepath.Join(root, "main.go")); statErr != nil {
				t.Fatal(statErr)
			}
			_ = manager.Close(context.Background())
		})
	}
}

func TestLSPConfigBoundsWorkspaceAndOutput(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	config := lspFixtureConfig(t, outside, "normal")
	if _, err := NewLSPManager(config, ExecutionScope{Root: root, Roots: []string{outside}, WriteRoot: root}); err == nil || !strings.Contains(err.Error(), "write root") {
		t.Fatalf("outside working dir error = %v", err)
	}
	config = lspFixtureConfig(t, root, "oversize")
	config.Servers[0].MaxOutputBytes = 256
	manager, err := NewLSPManager(config, ExecutionScope{Root: root, WriteRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = (LSPQueryTool{Manager: manager, Kind: "hover"}).Execute(context.Background(), json.RawMessage(`{"path":"main.go","line":0,"character":0}`))
	if err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("oversize result error = %v", err)
	}
}

func TestLSPConfigurationLoadsStrictlyAndCopiesSlices(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	document := fmt.Sprintf(`{"provider":{"type":"openai-compatible","base_url":"http://127.0.0.1:8081","model":"local","timeout_seconds":0},"generation":{"temperature":0.2,"max_tokens":1024},"lsp":{"servers":[{"name":"go","enabled":true,"command":"gopls","working_dir":%q,"language_id":"go","file_patterns":["*.go"],"env_allowlist":["PATH"]}]}}`, root)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.LSP.Servers) != 1 || config.LSP.Servers[0].LanguageID != "go" {
		t.Fatalf("LSP config = %#v", config.LSP)
	}
	config.LSP.Servers[0].FilePatterns[0] = "changed"
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.LSP.Servers[0].FilePatterns[0] != "*.go" {
		t.Fatalf("reloaded LSP config = %#v, %v", reloaded.LSP, err)
	}
	bad := strings.Replace(document, `"file_patterns":["*.go"]`, `"file_patterns":[]`, 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected empty file_patterns validation failure")
	}
}

func TestReadLSPFrameRejectsOversizeAndMalformedHeaders(t *testing.T) {
	for _, input := range []string{"X: y\r\n\r\n{}", "Content-Length: 20\r\n\r\n{}", "Content-Length: 1\r\nContent-Length: 1\r\n\r\n{}"} {
		reader := bufio.NewReader(strings.NewReader(input))
		if _, err := readLSPFrame(reader, 8); err == nil {
			t.Fatalf("readLSPFrame(%q) unexpectedly succeeded", input)
		}
	}
}
