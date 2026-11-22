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
	"time"
)

func mcpTestHost(t *testing.T, mode string) *MCPHost {
	t.Helper()
	t.Setenv("BEE_MCP_TEST_MODE", mode)
	host, err := NewMCPHost(MCPServerConfig{
		Name:                  "helper",
		Enabled:               true,
		Command:               os.Args[0],
		Args:                  []string{"-test.run=TestMCPHelperProcess", "--"},
		EnvAllowlist:          []string{"BEE_MCP_TEST_MODE"},
		StartupTimeoutSeconds: 2,
		RequestTimeoutSeconds: 2,
	}, ExecutionScope{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	return host
}

func TestMCPHandshakeAndRequest(t *testing.T) {
	host := mcpTestHost(t, "normal")
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !host.Available() {
		t.Fatal("host is unavailable after handshake")
	}
	response, err := host.requestWithTimeout(context.Background(), "test/echo", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := decodeMCPResult(response, &result); err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello" {
		t.Fatalf("text = %q, want hello", result.Text)
	}
}

func TestMCPToolListProjectionAndInvocation(t *testing.T) {
	host := mcpTestHost(t, "normal")
	host.config.AllowTools = []string{"echo", "dangerous"}
	host.config.DenyTools = []string{"dangerous"}
	host.config.ReadOnlyTools = []string{"echo"}
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if names := host.ToolNames(); len(names) != 1 || names[0] != "mcp_helper_echo" {
		t.Fatalf("tool names = %#v", names)
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	registered, ok := registry.Lookup("mcp_helper_echo")
	if !ok || !strings.Contains(registered.Definition().Description, "untrusted data") {
		t.Fatalf("projected description = %q", registered.Definition().Description)
	}
	executor := Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyPrompted}}
	result, err := executor.Execute(context.Background(), "mcp_helper_echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, `"text":"hello"`) {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestMCPMalformedToolDeclarationRejectsServer(t *testing.T) {
	host := mcpTestHost(t, "malformed-tool")
	err := host.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "input schema type must be object") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPMutatingToolUsesOrdinaryApprovalGate(t *testing.T) {
	host := mcpTestHost(t, "normal")
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	var audits []AuditEvent
	executor := Executor{
		Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyPrompted},
		Auditor: AuditorFunc(func(_ context.Context, event AuditEvent) error {
			audits = append(audits, event)
			return nil
		}),
	}
	_, err = executor.Execute(context.Background(), "mcp_helper_echo", json.RawMessage(`{"text":"blocked"}`))
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("error = %v, want approval requirement", err)
	}
	executor.Approval = ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalApproved, nil })
	if _, err := executor.Execute(context.Background(), "mcp_helper_echo", json.RawMessage(`{"text":"approved-secret"}`)); err != nil {
		t.Fatal(err)
	}
	for _, event := range audits {
		if strings.Contains(string(event.Args), "approved-secret") {
			t.Fatalf("MCP arguments reached audit: %#v", event)
		}
	}
}

func TestMCPToolCallAfterCrashIsUnavailable(t *testing.T) {
	host := mcpTestHost(t, "crash-after-list")
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MCP server crash")
	}
	executor := Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}
	_, err = executor.Execute(context.Background(), "mcp_helper_echo", json.RawMessage(`{"text":"ignored"}`))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

func TestMCPCrashDegradesToUnavailable(t *testing.T) {
	host := mcpTestHost(t, "crash")
	if err := host.Start(context.Background()); err == nil {
		t.Fatal("startup unexpectedly succeeded after MCP server crash")
	}
	if host.Available() {
		t.Fatal("crashed host still reports available")
	}
	_, err := host.requestWithTimeout(context.Background(), "test/echo", map[string]string{"text": "ignored"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

func TestMCPRejectsUnsupportedProtocolVersion(t *testing.T) {
	host := mcpTestHost(t, "wrong-version")
	err := host.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported MCP protocol version") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPConfigValidation(t *testing.T) {
	tests := []MCPServerConfig{
		{Enabled: true, Command: "helper"},
		{Name: "helper", Enabled: true},
		{Name: "helper", Enabled: true, Transport: "http", Command: "helper"},
		{Name: "helper", Enabled: true, Command: "helper", EnvAllowlist: []string{"TOKEN", "TOKEN"}},
	}
	for _, config := range tests {
		if err := config.Validate(); err == nil {
			t.Fatalf("config %#v unexpectedly validated", config)
		}
	}
}

func TestMCPStartRejectsInvalidExecutionScope(t *testing.T) {
	host, err := NewMCPHost(MCPServerConfig{Name: "helper", Enabled: true, Command: os.Args[0]}, ExecutionScope{})
	if err != nil {
		t.Fatal(err)
	}
	err = host.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "execution scope root is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPApplicationLifecycleAndFailureIsolation(t *testing.T) {
	t.Setenv("BEE_MCP_TEST_MODE", "normal")
	config := DefaultConfig()
	config.MCP.Servers = []MCPServerConfig{{
		Name: "good", Enabled: true, Command: os.Args[0],
		Args: []string{"-test.run=TestMCPHelperProcess", "--"}, EnvAllowlist: []string{"BEE_MCP_TEST_MODE"},
	}, {
		Name: "missing", Enabled: true, Command: filepath.Join(t.TempDir(), "does-not-exist"),
	}}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Config: config, Executor: &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}}
	if err := app.StartExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(app.MCPHosts) != 1 || app.MCPHosts[0].Config().Name != "good" {
		t.Fatalf("started MCP hosts = %#v", app.MCPHosts)
	}
	if app.MCPFailures["missing"] != "startup failed" {
		t.Fatalf("MCP failures = %#v", app.MCPFailures)
	}
	if _, ok := registry.Lookup("mcp_good_echo"); !ok {
		t.Fatal("healthy MCP server tool was not registered")
	}
	if err := app.CloseExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup("mcp_good_echo"); ok {
		t.Fatal("MCP tool remained registered after shutdown")
	}
}

func TestMCPApplicationRemovesToolsAfterServerCrash(t *testing.T) {
	t.Setenv("BEE_MCP_TEST_MODE", "crash-after-list")
	config := DefaultConfig()
	config.MCP.Servers = []MCPServerConfig{{
		Name: "crasher", Enabled: true, Command: os.Args[0],
		Args: []string{"-test.run=TestMCPHelperProcess", "--"}, EnvAllowlist: []string{"BEE_MCP_TEST_MODE"},
	}}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Config: config, Executor: &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}}
	if err := app.StartExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(app.MCPHosts) != 1 {
		t.Fatalf("started MCP hosts = %d, want 1", len(app.MCPHosts))
	}
	if _, ok := registry.Lookup("mcp_crasher_echo"); !ok {
		t.Fatal("MCP tool was not registered before crash")
	}
	select {
	case <-app.MCPHosts[0].Done():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MCP crash")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := registry.Lookup("mcp_crasher_echo"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed MCP server tool remained registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := app.CloseExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMCPConfigLoadsFromPrimaryDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"schema_version":1,"mcp":{"servers":[{"name":"docs","enabled":false,"transport":"stdio","command":"example","allow_tools":["search"]}]}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.MCP.Servers) != 1 || config.MCP.Servers[0].AllowTools[0] != "search" {
		t.Fatalf("MCP config = %#v", config.MCP)
	}
	if err := (MCPConfig{Servers: []MCPServerConfig{{Name: "same"}, {Name: "same"}}}).Validate(); err == nil {
		t.Fatal("duplicate MCP server names unexpectedly validated")
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("BEE_MCP_TEST_MODE") == "" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	mode := os.Getenv("BEE_MCP_TEST_MODE")
	for reader.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      string          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(reader.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			version := MCPProtocolVersion
			if mode == "wrong-version" {
				version = "1900-01-01"
			}
			_ = writer.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"protocolVersion": version,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "test-helper", "version": "1"},
				},
			})
		case "notifications/initialized":
			if mode == "crash" {
				time.Sleep(25 * time.Millisecond)
				os.Exit(7)
			}
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]string{"type": "string"}}}
			if mode == "malformed-tool" {
				schema = map[string]any{"type": "string"}
			}
			_ = writer.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{"tools": []any{
					map[string]any{"name": "echo", "description": "Echo bounded text.", "inputSchema": schema},
					map[string]any{"name": "dangerous", "description": "A mutating test tool.", "inputSchema": map[string]string{"type": "object"}},
				}},
			})
			if mode == "crash-after-list" {
				time.Sleep(100 * time.Millisecond)
				os.Exit(8)
			}
		case "tools/call":
			var input struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &input)
			_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []any{map[string]string{"type": "text", "text": input.Arguments["text"]}}}})
		case "test/echo":
			var input map[string]string
			_ = json.Unmarshal(request.Params, &input)
			_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"text": input["text"]}})
		default:
			if request.ID != "" {
				_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unknown method %s", request.Method)}})
			}
		}
	}
	os.Exit(0)
}
