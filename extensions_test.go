package bee

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func extensionTestHost(t *testing.T, mode string) *ExtensionHost {
	t.Helper()
	t.Setenv("BEE_EXTENSION_MODE", mode)
	config := ExtensionConfig{
		Name:                  "helper",
		Enabled:               true,
		Command:               os.Args[0],
		Args:                  []string{"-test.run=TestExtensionHelperProcess", "--"},
		WorkingDir:            t.TempDir(),
		EnvAllowlist:          []string{"BEE_EXTENSION_MODE", "VISIBLE_EXTENSION_VALUE"},
		StartupTimeoutSeconds: 2,
		RequestTimeoutSeconds: 2,
	}
	host, err := NewExtensionHost(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	return host
}

func TestExtensionHandshakeCapabilitiesAndToolCall(t *testing.T) {
	host := extensionTestHost(t, "normal")
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.Manifest().Name != "helper" || host.Manifest().Protocol != ExtensionProtocolVersion {
		t.Fatalf("manifest = %#v", host.Manifest())
	}
	capabilities := host.Capabilities()
	if len(capabilities.Tools) != 1 || capabilities.Tools[0].Name != "echo" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	result, err := host.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil || result.Output != "hello" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestComposeGeneralAssistantExtensionLifecycle(t *testing.T) {
	t.Setenv("BEE_EXTENSION_MODE", "normal")
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	memoryRoot := t.TempDir()
	if err := os.Chmod(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Extensions = []ExtensionConfig{{
		Name:                  "helper",
		Enabled:               true,
		Command:               os.Args[0],
		Args:                  []string{"-test.run=TestExtensionHelperProcess", "--"},
		EnvAllowlist:          []string{"BEE_EXTENSION_MODE"},
		StartupTimeoutSeconds: 2,
		RequestTimeoutSeconds: 2,
	}}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{
		Store:        store,
		ScopeRoot:    t.TempDir(),
		MemoryRoot:   memoryRoot,
		JobStorePath: filepath.Join(t.TempDir(), "jobs.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.StartExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(app.ExtensionHosts) != 1 {
		t.Fatalf("extension hosts = %d, want 1", len(app.ExtensionHosts))
	}
	if _, ok := app.Executor.Registry.Lookup("echo"); !ok {
		t.Fatal("configured extension tool was not registered in the assistant registry")
	}
	if err := app.CloseExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(app.ExtensionHosts) != 0 {
		t.Fatal("extension hosts were not cleared on shutdown")
	}
	if _, ok := app.Executor.Registry.Lookup("echo"); ok {
		t.Fatal("extension tool remained registered after shutdown")
	}
}

func TestExtensionNotificationAndFilteredEnvironment(t *testing.T) {
	t.Setenv("VISIBLE_EXTENSION_VALUE", "visible")
	t.Setenv("HIDDEN_EXTENSION_VALUE", "hidden")
	notificationCh := make(chan ExtensionNotification, 1)
	host := extensionTestHost(t, "environment")
	host.SetNotificationHandler(func(notification ExtensionNotification) { notificationCh <- notification })
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(host.Manifest().Description, "visible=visible") || strings.Contains(host.Manifest().Description, "hidden=hidden") {
		t.Fatalf("filtered environment leaked: %q", host.Manifest().Description)
	}
	select {
	case notification := <-notificationCh:
		if notification.Method != "helper/ready" {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for extension notification")
	}
}

func TestExtensionToolErrorsAndExecutorPolicy(t *testing.T) {
	host := extensionTestHost(t, "error")
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`)); err == nil || !strings.Contains(err.Error(), "extension RPC error") {
		t.Fatalf("error = %v", err)
	}

	high := extensionTestHost(t, "high")
	if err := high.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := high.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: t.TempDir()}
	_, err = (&Executor{Registry: registry, Scope: scope, Policy: DefaultCodingPolicy{}}).Execute(context.Background(), "echo", json.RawMessage(`{"text":"blocked"}`))
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("policy error = %v", err)
	}
}

type extensionWriteError struct {
	host *ExtensionHost
	err  error
}

func (writer extensionWriteError) Write([]byte) (int, error) {
	writer.host.mu.Lock()
	writer.host.failure = errors.New("extension response has unknown or duplicate id late")
	writer.host.mu.Unlock()
	return 0, writer.err
}
func (extensionWriteError) Close() error { return nil }

func TestExtensionRequestPrefersRecordedFailureOverWriteError(t *testing.T) {
	writeErr := errors.New("write |1: broken pipe")
	host := &ExtensionHost{
		config:  ExtensionConfig{Name: "test"},
		started: true,
		pending: make(map[string]chan extensionResponse),
		done:    make(chan struct{}),
	}
	host.stdin = extensionWriteError{host: host, err: writeErr}
	_, err := host.request(context.Background(), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown or duplicate") {
		t.Fatalf("error=%v, want recorded protocol failure", err)
	}
	if errors.Is(err, writeErr) {
		t.Fatalf("error=%v unexpectedly exposed teardown write error", err)
	}
	host.mu.RLock()
	pending := len(host.pending)
	host.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("pending requests leaked: %d", pending)
	}
}

func waitForExtensionFailure(t *testing.T, host *ExtensionHost) error {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-host.done:
		host.mu.RLock()
		defer host.mu.RUnlock()
		return host.failure
	case <-timer.C:
		t.Fatal("timed out waiting for extension failure")
		return nil
	}
}

func TestExtensionMalformedUnknownDuplicateAndOversizedFrames(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"malformed", "malformed JSON"},
		{"unknown", "unknown or duplicate"},
		{"duplicate", "unknown or duplicate"},
	}
	for _, test := range cases {
		t.Run(test.mode, func(t *testing.T) {
			host := extensionTestHost(t, test.mode)
			err := host.Start(context.Background())
			if test.mode == "duplicate" && err == nil {
				failure := waitForExtensionFailure(t, host)
				_, err = host.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"after-duplicate"}`))
				if !errors.Is(err, failure) {
					t.Fatalf("error=%v, want recorded failure %v", err, failure)
				}
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	config := ExtensionConfig{Name: "large", Enabled: true, Command: os.Args[0], Args: []string{"-test.run=TestExtensionHelperProcess", "--"}, WorkingDir: t.TempDir(), EnvAllowlist: []string{"BEE_EXTENSION_MODE"}, MaxFrameBytes: 64, MaxMessageBytes: 128}
	t.Setenv("BEE_EXTENSION_MODE", "oversized")
	host, err := NewExtensionHost(config)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	if err := host.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "max frame size") {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestExtensionTimeoutCrashCancellationAndShutdown(t *testing.T) {
	crash := extensionTestHost(t, "crash")
	if err := crash.Start(context.Background()); err == nil {
		t.Fatal("expected crash detection")
	}
	slow := extensionTestHost(t, "slow-call")
	if err := slow.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := slow.CallTool(ctx, "echo", json.RawMessage(`{"text":"slow"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if err := slow.Close(context.Background()); err != nil {
		t.Fatalf("close after cancellation: %v", err)
	}

	shutdown := extensionTestHost(t, "normal")
	if err := shutdown.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := shutdown.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionHostCannotStartAfterClose(t *testing.T) {
	host := extensionTestHost(t, "normal")
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after close error = %v", err)
	}
}

func TestRegisterToolsRollsBackPartialRegistration(t *testing.T) {
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "conflict"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	host := &ExtensionHost{config: ExtensionConfig{Name: "test"}, started: true, capabilities: ExtensionCapabilities{Tools: []ToolDefinition{
		{Name: "first"},
		{Name: "conflict"},
	}}}
	if err := host.RegisterTools(registry); err == nil {
		t.Fatal("expected registration conflict")
	}
	if _, ok := registry.Lookup("first"); ok {
		t.Fatal("partial extension registration was not rolled back")
	}
}

func TestExtensionConfigValidationAndLoading(t *testing.T) {
	if err := (ExtensionConfig{Name: "x", Enabled: true}).Validate(); err == nil {
		t.Fatal("expected enabled command validation")
	}
	config := DefaultConfig()
	config.Extensions = []ExtensionConfig{{Name: "x", Enabled: false}, {Name: "y", Enabled: false}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtensionHosts([]ExtensionConfig{{Name: "x", Enabled: false}, {Name: "x", Enabled: false}}); err == nil {
		t.Fatal("expected duplicate extension validation")
	}
}

// TestExtensionHelperProcess is an in-process test helper used as an external
// subprocess by the tests above. It exits immediately when not explicitly
// enabled so ordinary `go test` runs remain unchanged.
func TestExtensionHelperProcess(t *testing.T) {
	if os.Getenv("BEE_EXTENSION_MODE") == "" {
		return
	}
	mode := os.Getenv("BEE_EXTENSION_MODE")
	if mode == "crash" {
		os.Exit(3)
	}
	if mode == "slow-init" {
		time.Sleep(time.Second)
	}
	reader := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for reader.Scan() {
		line := reader.Bytes()
		if mode == "malformed" {
			_, _ = os.Stdout.WriteString("{not-json\n")
			return
		}
		if mode == "oversized" {
			_, _ = os.Stdout.WriteString(strings.Repeat("x", 256) + "\n")
			return
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      string          `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			return
		}
		switch request.Method {
		case ExtensionMethodInitialize:
			if mode == "unknown" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "unknown", "result": map[string]any{}})
				return
			}
			if mode == "duplicate" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": helperInitResult(mode)})
				time.Sleep(50 * time.Millisecond)
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": helperInitResult(mode)})
				continue
			}
			if mode == "environment" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": helperInitResult(mode)})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "helper/ready", "params": map[string]string{"ok": "true"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": helperInitResult(mode)})
		case ExtensionMethodCancelRequest:
			// The host sends cancellation as a notification after its context ends.
		case ExtensionMethodToolsCall:
			if mode == "slow-call" {
				time.Sleep(time.Second)
			}
			if mode == "error" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": 4001, "message": "extension rejected call"}})
				continue
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			var args struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(params.Arguments, &args)
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]string{"output": args.Text}})
		case ExtensionMethodShutdown:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]bool{"ok": true}})
			return
		}
	}
}

func helperInitResult(mode string) map[string]any {
	name := "helper"
	if configured := strings.TrimSpace(os.Getenv("BEE_EXTENSION_NAME")); configured != "" {
		name = configured
	}
	description := "test helper"
	if mode == "environment" {
		description = fmt.Sprintf("visible=%s hidden=%s", os.Getenv("VISIBLE_EXTENSION_VALUE"), os.Getenv("HIDDEN_EXTENSION_VALUE"))
	}
	risk := RiskLow
	if mode == "high" {
		risk = RiskHigh
	}
	return map[string]any{
		"manifest":     map[string]any{"name": name, "version": "1.0.0", "protocol": ExtensionProtocolVersion, "description": description},
		"capabilities": map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo", "parameters": map[string]any{"type": "object"}, "risk": risk}}},
	}
}
