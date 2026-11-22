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

func acpTestConfig(t *testing.T, mode string) ACPProcessConfig {
	t.Helper()
	t.Setenv("BEE_ACP_HELPER", mode)
	return ACPProcessConfig{
		Command:               os.Args[0],
		Args:                  []string{"-test.run=TestACPHelperProcess", "--"},
		EnvAllowlist:          []string{"BEE_ACP_HELPER"},
		StartupTimeoutSeconds: 2,
		RequestTimeoutSeconds: 2,
		MaxFrameBytes:         1 << 20,
		MaxMessageBytes:       1 << 20,
	}
}

func acpTestTask(root string) CodingBackendTask {
	return CodingBackendTask{Task: ChildTask{
		TaskID:             "acp-task",
		ParentID:           "acp-parent",
		Objective:          "make the requested change",
		Prompt:             "make the requested change",
		Scope:              ExecutionScope{Root: root, TimeoutSeconds: 3, MaxOutputBytes: 4096},
		Budget:             ChildBudget{TimeoutSeconds: 2, MaxOutputBytes: 4096},
		ArtifactDir:        filepath.Join("generated", "children", "acp-task"),
		AcceptanceCriteria: []string{"tests pass"},
	}, Plan: []string{"inspect", "change", "test"}, Backend: "fake-acp"}
}

func standardInit() ACPInitializeParams {
	return ACPInitializeParams{
		ProtocolVersion: ACPProtocolVersion,
		ClientCapabilities: ACPClientCapabilities{
			FS:   ACPFileSystemCapabilities{},
			Auth: ACPAuthCapabilities{},
		},
		ClientInfo: ACPImplementation{Name: "test", Version: "test"},
	}
}

func TestACPStdioTransportStandardV1InitializeSessionPromptAndUpdates(t *testing.T) {
	root := t.TempDir()
	config := acpTestConfig(t, "normal")
	config.WorkingDir = root
	transport, err := NewACPStdioTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	initialized, err := transport.Initialize(context.Background(), standardInit())
	if err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion != 1 || initialized.AgentInfo == nil || initialized.AgentInfo.Name != "fake" {
		t.Fatalf("initialize = %#v", initialized)
	}
	session, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: root, MCPServers: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "session-1" {
		t.Fatalf("session = %#v", session)
	}
	result, err := transport.Prompt(context.Background(), ACPPromptParams{SessionID: session.SessionID, Prompt: []ACPContentBlock{{Type: "text", Text: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("prompt result = %#v", result)
	}
	select {
	case update := <-transport.Updates():
		if update.Type != "agent_message_chunk" || update.Text != "hello from ACP" || update.SessionID != session.SessionID {
			t.Fatalf("update = %#v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ACP update")
	}
}

func TestACPStdioTransportWritesStandardWireShapes(t *testing.T) {
	config := acpTestConfig(t, "capture")
	config.WorkingDir = t.TempDir()
	transport, err := NewACPStdioTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	if _, err := transport.Initialize(context.Background(), standardInit()); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: t.TempDir(), MCPServers: []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Prompt(context.Background(), ACPPromptParams{SessionID: "session-1", Prompt: []ACPContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestACPStdioTransportAuthenticateWireShape(t *testing.T) {
	config := acpTestConfig(t, "auth")
	transport, err := NewACPStdioTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	initialized, err := transport.Initialize(context.Background(), standardInit())
	if err != nil {
		t.Fatal(err)
	}
	if len(initialized.AuthMethods) != 1 || initialized.AuthMethods[0].ID != ACPAuthMethodAPIKey {
		t.Fatalf("auth methods = %#v", initialized.AuthMethods)
	}
	if err := transport.Authenticate(context.Background(), ACPAuthenticateParams{MethodID: ACPAuthMethodAPIKey, Meta: ACPAuthenticateMeta{APIKey: ACPAPIKeyAuth{APIKey: "wire-secret"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: t.TempDir(), MCPServers: []any{}}); err != nil {
		t.Fatal(err)
	}
}

func TestACPStdioTransportCancellationNotification(t *testing.T) {
	config := acpTestConfig(t, "cancel")
	transport, err := NewACPStdioTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	if _, err := transport.Initialize(context.Background(), standardInit()); err != nil {
		t.Fatal(err)
	}
	session, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: t.TempDir(), MCPServers: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	promptCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = transport.Prompt(promptCtx, ACPPromptParams{SessionID: session.SessionID, Prompt: []ACPContentBlock{{Type: "text", Text: "wait"}}})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("prompt error = %v", err)
	}
	if err := transport.Cancel(context.Background(), session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-transport.Updates():
		if update.Type != "agent_message_chunk" || update.Text != "cancel observed" {
			t.Fatalf("update = %#v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("fake ACP did not observe cancellation")
	}
}

func TestBoundedACPPermissionHandlerSelectsOnlyAgentOptions(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "worktree")
	task := acpTestTask(root)
	task.Task.Mutability = TaskMutable
	task.PermissionPolicy = ACPPermissionPolicy{AutoApprovePermissions: true, Role: CodingRoleImplementer}
	task.Role = CodingRoleImplementer
	task.Workspace = &WorkspaceLease{TaskID: task.Task.TaskID, RepositoryRoot: root, WorkspacePath: workspace, ScopePath: workspace}
	handler := NewBoundedACPPermissionHandler(ACPProcessConfig{AutoApprovePermissions: true}, task)
	response := handler.HandleACPPermission(context.Background(), ACPPermissionRequest{SessionID: "session-1", ToolCall: json.RawMessage(`{"name":"run_command"}`), Options: []ACPPermissionOption{
		{OptionID: "reject", Kind: "reject_once"},
		{OptionID: "proceed", Kind: "proceed_once"},
		{OptionID: "allow", Kind: "allow_once"},
	}})
	if response.Outcome.Outcome != "selected" || response.Outcome.OptionID != "allow" {
		t.Fatalf("response = %#v", response)
	}
	readOnly := task
	readOnly.Task.Mutability = TaskReadOnly
	response = NewBoundedACPPermissionHandler(ACPProcessConfig{AutoApprovePermissions: true}, readOnly).HandleACPPermission(context.Background(), ACPPermissionRequest{SessionID: "session-1", ToolCall: json.RawMessage(`{}`), Options: []ACPPermissionOption{{OptionID: "allow", Kind: "allow_once"}}})
	if response.Outcome.Outcome != "cancelled" || response.Outcome.OptionID != "" {
		t.Fatalf("read-only response = %#v", response)
	}
}

func TestACPStdioTransportPermissionRequestUsesStandardResponseFraming(t *testing.T) {
	root := t.TempDir()
	config := acpTestConfig(t, "permission")
	config.WorkingDir = root
	config.AutoApprovePermissions = true
	transport, err := NewACPStdioTransport(config)
	if err != nil {
		t.Fatal(err)
	}
	task := acpTestTask(root)
	task.Task.Mutability = TaskMutable
	task.Role = CodingRoleImplementer
	task.PermissionPolicy = ACPPermissionPolicy{AutoApprovePermissions: true, Role: CodingRoleImplementer}
	task.Workspace = &WorkspaceLease{TaskID: task.Task.TaskID, RepositoryRoot: root, WorkspacePath: filepath.Join(root, "worktree"), ScopePath: filepath.Join(root, "worktree")}
	transport.SetACPPermissionHandler(NewBoundedACPPermissionHandler(config, task))
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	if _, err := transport.Initialize(context.Background(), standardInit()); err != nil {
		t.Fatal(err)
	}
	session, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: root, MCPServers: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := transport.Prompt(context.Background(), ACPPromptParams{SessionID: session.SessionID, Prompt: []ACPContentBlock{{Type: "text", Text: "permission"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("prompt result = %#v", result)
	}
}

func TestACPStdioTransportMalformedAndStandardErrors(t *testing.T) {
	t.Run("malformed update", func(t *testing.T) {
		config := acpTestConfig(t, "malformed")
		transport, err := NewACPStdioTransport(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer transport.Close(context.Background())
		if _, err := transport.Initialize(context.Background(), standardInit()); err != nil {
			t.Fatal(err)
		}
		if _, err := transport.NewSession(context.Background(), ACPSessionNewParams{Cwd: t.TempDir(), MCPServers: []any{}}); err != nil {
			t.Fatal(err)
		}
		if _, err := transport.Prompt(context.Background(), ACPPromptParams{SessionID: "session-1", Prompt: []ACPContentBlock{{Type: "text", Text: "bad"}}}); err == nil {
			t.Fatal("expected malformed update failure")
		}
		if transport.Failure() == nil {
			t.Fatal("expected malformed update transport failure")
		}
	})
	t.Run("standard error", func(t *testing.T) {
		config := acpTestConfig(t, "error")
		transport, err := NewACPStdioTransport(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer transport.Close(context.Background())
		_, err = transport.Initialize(context.Background(), standardInit())
		if err == nil || !strings.Contains(err.Error(), "ACP RPC error -32602") {
			t.Fatalf("initialize error = %v", err)
		}
	})
}

func TestACPBackendCapturesCompletionDiffAndEvidence(t *testing.T) {
	root := t.TempDir()
	config := acpTestConfig(t, "normal")
	backend, err := NewACPBackend(config)
	if err != nil {
		t.Fatal(err)
	}
	backend.BackendName = "fake-acp"
	backend.Evidence = CodingEvidenceCollectorFunc(func(_ context.Context, _ CodingBackendTask, _ ChildCompletionPacket) (CodingEvidence, error) {
		return CodingEvidence{Artifacts: []string{"review.diff"}, Notes: []string{"fake evidence hook"}}, nil
	})
	var events []CodingBackendEvent
	completion, err := backend.Run(context.Background(), acpTestTask(root), codingBackendObserverFunc(func(event CodingBackendEvent) { events = append(events, event) }))
	if err != nil {
		t.Fatal(err)
	}
	if completion.Status != ChildSucceeded || completion.Summary != "hello from ACP" {
		t.Fatalf("completion = %#v", completion)
	}
	if len(completion.ChangedFiles) == 0 || completion.ChangedFiles[0] != "changed.txt" {
		t.Fatalf("changed files = %#v", completion.ChangedFiles)
	}
	if !acpContainsString(completion.Artifacts, "review.diff") || !acpContainsString(completion.Evidence, "fake evidence hook") {
		t.Fatalf("evidence = %#v artifacts = %#v", completion.Evidence, completion.Artifacts)
	}
	if _, err := os.Stat(filepath.Join(root, "generated", "children", "acp-task", "completion.json")); err != nil {
		t.Fatal(err)
	}
	if completion.Manifest == nil {
		t.Fatal("ACP completion manifest is missing")
	}
	if manifest, err := ReadCompletionManifest(filepath.Join(root, "generated", "children", "acp-task", CompletionManifestFilename)); err != nil {
		t.Fatal(err)
	} else if manifest.Kind != CompletionManifestACP || manifest.Provider.Model != "fake-acp" {
		t.Fatalf("ACP completion manifest = %#v", manifest)
	}
	if len(events) < 3 {
		t.Fatalf("events = %#v", events)
	}
}

func TestACPBackendTimeoutAndProcessFailure(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		root := t.TempDir()
		backend, err := NewACPBackend(acpTestConfig(t, "hang"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		completion, err := backend.Run(ctx, acpTestTask(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		if completion.Status != ChildTimedOut && completion.Status != ChildCancelled {
			t.Fatalf("completion = %#v", completion)
		}
	})
	t.Run("process failure", func(t *testing.T) {
		root := t.TempDir()
		backend, err := NewACPBackend(acpTestConfig(t, "crash"))
		if err != nil {
			t.Fatal(err)
		}
		completion, err := backend.Run(context.Background(), acpTestTask(root), nil)
		if err != nil {
			t.Fatal(err)
		}
		if completion.Status != ChildMalformed {
			t.Fatalf("completion = %#v", completion)
		}
	})
}

func TestACPBackendRejectsWorkingDirectoryOutsideScope(t *testing.T) {
	root := t.TempDir()
	backend, err := NewACPBackend(acpTestConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	backend.Config.WorkingDir = filepath.Dir(root)
	_, err = backend.Run(context.Background(), acpTestTask(root), nil)
	if err == nil || !strings.Contains(err.Error(), "within task scope") {
		t.Fatalf("working directory error = %v", err)
	}
}

func TestCodingBackendTaskLeaseTranslatesScopeWithoutWidening(t *testing.T) {
	root := t.TempDir()
	leased := filepath.Join(root, "leased")
	artifact := filepath.Join(leased, "artifacts")
	if err := os.MkdirAll(artifact, 0o700); err != nil {
		t.Fatal(err)
	}
	task := acpTestTask(root)
	task.Workspace = &WorkspaceLease{TaskID: task.Task.TaskID, ScopePath: leased, ChildArtifactDir: artifact}
	effective, err := task.EffectiveTask()
	if err != nil {
		t.Fatal(err)
	}
	if effective.Scope.Root != leased || effective.ArtifactDir != artifact {
		t.Fatalf("effective task = %#v", effective)
	}
	outside := task
	outside.Workspace = &WorkspaceLease{TaskID: task.Task.TaskID, ScopePath: filepath.Dir(root), ChildArtifactDir: filepath.Join(filepath.Dir(root), "artifacts")}
	if _, err := outside.EffectiveTask(); err == nil {
		t.Fatal("expected lease scope widening rejection")
	}
}

func TestACPHelperProcess(t *testing.T) {
	mode := os.Getenv("BEE_ACP_HELPER")
	if mode == "" {
		return
	}
	if mode == "crash" {
		os.Exit(3)
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	permissionPromptID := ""
	emitPermissionCompletion := func(promptID string) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": ACPMethodUpdate, "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "message-1", "content": map[string]any{"type": "text", "text": "permission granted"}}}})
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": promptID, "result": map[string]any{"stopReason": "end_turn"}})
	}
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      string          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return
		}
		if request.Method == "" && request.ID == "permission-1" {
			var response struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			if err := json.Unmarshal(request.Result, &response); err != nil {
				return
			}
			if response.Outcome.Outcome == "selected" && response.Outcome.OptionID == "allow-permission" {
				emitPermissionCompletion(permissionPromptID)
			} else {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": permissionPromptID, "result": map[string]any{"stopReason": "refusal"}})
			}
			continue
		}
		switch request.Method {
		case ACPMethodInitialize:
			if mode == "error" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32602, "message": "bad initialize"}})
				continue
			}
			result := map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "agentInfo": map[string]any{"name": "fake", "version": "1"}}
			if mode == "auth" {
				result["authMethods"] = []map[string]any{{"id": ACPAuthMethodAPIKey, "name": "API Key"}}
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		case ACPMethodAuthenticate:
			var params ACPAuthenticateParams
			if err := json.Unmarshal(request.Params, &params); err != nil || params.Validate() != nil || (mode == "auth" && params.Meta.APIKey.APIKey != "wire-secret") {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32602, "message": "bad authenticate"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{}})
		case ACPMethodNewSession:
			var params struct {
				Cwd        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil || params.Cwd == "" || params.MCPServers == nil {
				return
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"sessionId": "session-1"}})
		case ACPMethodPrompt:
			if mode == "hang" {
				time.Sleep(5 * time.Second)
				return
			}
			if mode == "malformed" {
				_, _ = fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":not-json}}`)
				return
			}
			if mode == "cancel" {
				continue
			}
			if mode == "permission" {
				permissionPromptID = request.ID
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": ACPMethodRequestPermission, "params": map[string]any{
					"sessionId": "session-1",
					"toolCall":  map[string]any{"name": "run_command", "input": map[string]any{"argv": []string{"go", "test"}}},
					"options":   []map[string]any{{"optionId": "reject-permission", "kind": "reject_once"}, {"optionId": "allow-permission", "kind": "allow_once"}},
				}})
				continue
			}
			var params struct {
				SessionID string            `json:"sessionId"`
				Prompt    []ACPContentBlock `json:"prompt"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil || len(params.Prompt) != 1 || params.Prompt[0].Type != "text" {
				return
			}
			if err := os.WriteFile("changed.txt", []byte("changed\n"), 0o600); err != nil {
				return
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": ACPMethodUpdate, "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "message-1", "content": map[string]any{"type": "text", "text": "hello from ACP"}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": ACPMethodUpdate, "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "test", "status": "completed"}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"stopReason": "end_turn"}})
		case ACPMethodCancel:
			if mode == "cancel" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": ACPMethodUpdate, "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "cancel observed"}}}})
				return
			}
		}
	}
}

func acpContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
