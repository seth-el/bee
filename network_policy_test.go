package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkOffSandboxCannotReachALocalListener(t *testing.T) {
	if !requireBubblewrapOrSkip(t) {
		t.Skip("needs bubblewrap outside a Bee sandbox")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	root := t.TempDir()
	scope := ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true}
	probe := []string{"-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port)}
	run := func(scope ExecutionScope) error {
		program, args, err := sandboxCommandInvocation(scope, bash, probe, root)
		if err != nil {
			return err
		}
		return exec.Command(program, args...).Run()
	}
	if err := run(scope); err != nil {
		t.Fatalf("network on: listener unreachable: %v", err)
	}
	scope.NetworkOff = true
	if err := run(scope); err == nil {
		t.Fatal("network off: sandboxed command reached the host listener")
	}
}

func TestNetworkOffRefusesWhenTheSandboxCannotRemoveIt(t *testing.T) {
	root := t.TempDir()
	_, _, err := sandboxCommandInvocation(ExecutionScope{Root: root, WriteRoot: root, NetworkOff: true}, "/bin/sh", nil, root)
	if !errors.Is(err, ErrNetworkOff) || !strings.Contains(err.Error(), "unsafe_disable_process_sandbox") {
		t.Fatalf("unsandboxed: err = %v", err)
	}
	t.Setenv(processSandboxMarker, processSandboxMarkerValue)
	_, _, err = sandboxCommandInvocation(ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true, NetworkOff: true}, "/bin/sh", nil, root)
	if !errors.Is(err, ErrNetworkOff) || !strings.Contains(err.Error(), "already inside a sandbox") {
		t.Fatalf("nested: err = %v", err)
	}
}

type namedTestTool struct{ name string }

func (tool namedTestTool) Definition() ToolDefinition {
	return ToolDefinition{Name: tool.name, Parameters: json.RawMessage(`{"type":"object"}`), Mutability: ToolMutabilityReadOnly}
}
func (tool namedTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "ran", nil
}

func TestNetworkOffRefusesNetworkToolsAtCallTime(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web_fetch", "web_search", HostExecToolName, BrowserToolName, "repo_clone", "run_command", "read_file"} {
		if err := registry.Register(namedTestTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	executor := Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyAllow, nil
	})}
	off := withNetworkOff(context.Background(), true)
	for _, call := range []struct {
		name, args string
		refused    bool
	}{
		{"web_fetch", `{}`, true}, {"web_search", `{}`, true}, {HostExecToolName, `{}`, true}, {BrowserToolName, `{}`, true}, {"repo_clone", `{}`, true},
		{"run_command", `{"target":"ssh"}`, true}, {"run_command", `{"target":"local"}`, false}, {"read_file", `{}`, false},
	} {
		result, err := executor.Execute(off, call.name, json.RawMessage(call.args))
		if call.refused != errors.Is(err, ErrNetworkOff) {
			t.Errorf("%s %s: err = %v, output = %q", call.name, call.args, err, result.Output)
		}
		if _, err := executor.Execute(context.Background(), call.name, json.RawMessage(call.args)); err != nil {
			t.Errorf("%s with network on: %v", call.name, err)
		}
	}
	// The tool list is unchanged (prompt-cache prefix stays stable).
	if _, ok := registry.Lookup("web_fetch"); !ok {
		t.Fatal("tool removed from registry")
	}
}

func networkTestApp(t *testing.T) (*Application, *SessionStore) {
	t.Helper()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Specialist.Enabled = false
	app, err := NewApplication(config, &fakeProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	return app, store
}

func TestNetworkCommandPersistsAndRecordsToggles(t *testing.T) {
	app, store := networkTestApp(t)
	session, err := store.Create(SessionMetadata{Title: "net"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dispatch := func(text string) CommandResult {
		return DispatchCommand(ctx, text, CommandContext{Application: app, SessionID: session.ID})
	}
	if got := dispatch("/network").Text; got != "network: on" {
		t.Fatalf("status = %q", got)
	}
	if got := dispatch("/network off").Text; !strings.HasPrefix(got, "network off:") {
		t.Fatalf("off = %q", got)
	}
	if got := dispatch("/network off").Text; got != "network is already off" {
		t.Fatalf("repeat = %q", got)
	}
	// A fresh load (resume) keeps it, and a run starts with it applied.
	if enabled, err := app.SessionNetworkEnabled(session.ID); err != nil || enabled {
		t.Fatalf("after reload enabled=%v err=%v", enabled, err)
	}
	events, err := app.Run(ctx, RunRequest{Prompt: "hi", SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	sawOff := false
	for event := range events {
		if event.Type == RunStart && event.NetworkOff {
			sawOff = true
		}
	}
	if !sawOff {
		t.Fatal("run_start did not carry network_off")
	}
	if got := dispatch("/network on").Text; !strings.HasPrefix(got, "network on:") {
		t.Fatalf("on = %q", got)
	}
	loaded, _ := store.Load(session.ID)
	var toggles []bool
	for _, event := range loaded.Events {
		if event.Type == EventNetworkPolicy {
			payload, _ := decodeEventPayload[NetworkPolicyEvent](event, EventNetworkPolicy)
			if payload.Actor != "owner" {
				t.Fatalf("actor = %q", payload.Actor)
			}
			toggles = append(toggles, payload.Enabled)
		}
	}
	if fmt.Sprint(toggles) != "[false true]" {
		t.Fatalf("toggles = %v", toggles)
	}
	if !strings.Contains(dispatch("/network sideways").Text, "usage") {
		t.Fatal("bad argument accepted")
	}
	if !strings.Contains(DispatchCommand(ctx, "/network off", CommandContext{Application: app, SessionID: session.ID, RunActive: true}).Text, "while a response is running") {
		t.Fatal("changed during an active run")
	}
}

func TestNetworkOffBeforeTheSessionAppliesToTheNextOne(t *testing.T) {
	app, store := networkTestApp(t)
	if got := DispatchCommand(context.Background(), "/network off", CommandContext{Application: app}).Text; !strings.Contains(got, "next session") {
		t.Fatalf("pending = %q", got)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var id SessionID
	for event := range events {
		if event.SessionID != "" {
			id = event.SessionID
		}
	}
	if enabled, _ := app.SessionNetworkEnabled(id); enabled {
		t.Fatal("pending /network off not applied to the new session")
	}
	if second, _ := store.Create(SessionMetadata{}); second != nil {
		if enabled, _ := app.SessionNetworkEnabled(second.ID); !enabled {
			t.Fatal("pending applied twice")
		}
	}
}

func TestNoModelToolCanChangeTheNetwork(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range app.Executor.Registry.Definitions() {
		if strings.Contains(definition.Name, "network") {
			t.Fatalf("model-facing tool %q could touch the network switch", definition.Name)
		}
	}
}

func TestNetworkOffClosesOwnTerminalsAndBlocksNetworkedOnes(t *testing.T) {
	manager := testTerminalManagerWithBuffer(t, 4096)
	defer manager.CloseAll()
	app, store := networkTestApp(t)
	app.Terminal = manager
	mine, _ := store.Create(SessionMetadata{})
	other, _ := store.Create(SessionMetadata{})
	inSession := func(id SessionID) context.Context { return withArtifactContext(context.Background(), id, "run") }
	own, err := manager.Open(inSession(mine.ID), "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := manager.Open(inSession(other.ID), "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_, closed, err := app.SetSessionNetwork(context.Background(), mine.ID, false)
	if err != nil || closed != 1 {
		t.Fatalf("closed = %d, err = %v", closed, err)
	}
	if info, _ := manager.Inspect(own.ID); info.Status == TerminalSessionRunning {
		t.Fatal("own networked terminal still running")
	}
	offCtx := withNetworkOff(inSession(mine.ID), true)
	if err := manager.Write(offCtx, foreign.ID, "echo hi\n"); !errors.Is(err, ErrNetworkOff) {
		t.Fatalf("driving another session's networked terminal: %v", err)
	}
	if _, err := manager.Read(offCtx, foreign.ID, 0); err != nil {
		t.Fatalf("reading should stay allowed: %v", err)
	}
	if info, _ := manager.Inspect(foreign.ID); info.Status != TerminalSessionRunning {
		t.Fatal("another session's terminal was closed")
	}
	// The test manager is unsandboxed, so a terminal can't be opened without network.
	if _, err := manager.Open(offCtx, "", 80, 24); !errors.Is(err, ErrNetworkOff) {
		t.Fatalf("unsandboxed open with network off: %v", err)
	}
}

func TestOpenAIHTTPCommandTogglesNetwork(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	session := bindOpenAIWebCommandSession(t, store, routes)
	app := handler.handlers.Application
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedCommandRequest(`{"name":"network","args":"off"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var event LocalCommandEvent
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil || event.Name != CommandNetwork {
		t.Fatalf("event = %#v, %v", event, err)
	}
	if enabled, err := app.SessionNetworkEnabled(session.ID); err != nil || enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
}
