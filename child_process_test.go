package bee

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func processChildConfig(t *testing.T, mode string) ChildProcessConfig {
	t.Helper()
	t.Setenv("BEE_CHILD_HELPER", mode)
	return ChildProcessConfig{
		Command:               os.Args[0],
		Args:                  []string{"-test.run=TestProcessChildHelper", "--"},
		EnvAllowlist:          []string{"BEE_CHILD_HELPER"},
		StartupTimeoutSeconds: 2,
		RequestTimeoutSeconds: 2,
		MaxFrameBytes:         1 << 20,
		MaxMessageBytes:       1 << 20,
	}
}

func processChildApp(t *testing.T, provider Provider, executor *Executor) *Application {
	t.Helper()
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = executor
	return app
}

func TestProcessChildRunnerForwardsChildEvents(t *testing.T) {
	root := t.TempDir()
	runner, err := NewProcessChildRunner(processChildApp(t, &childTestProvider{}, nil), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	collector := &childObservationCollector{}
	packet, err := runner.RunObserved(context.Background(), childTask(root), collector)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded {
		t.Fatalf("packet = %#v", packet)
	}
	types := collector.Types()
	for _, want := range []string{"started", "provider_delta", "assistant_message", "usage", "completed"} {
		found := false
		for _, got := range types {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("observations = %#v, missing %q", types, want)
		}
	}
}

func TestProcessChildRunnerPoolRunsIndependentProcessesConcurrently(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	provider := &poolBarrierProvider{started: make(chan struct{}, 2), release: make(chan struct{})}
	pool, err := NewProcessChildRunnerPool(processChildApp(t, provider, nil), processChildConfig(t, "normal"), 2)
	if err != nil {
		t.Fatal(err)
	}
	plan := OrchestrationPlan{
		OrchestrationID: "orch-process-pool",
		ParentID:        "parent-1",
		MaxConcurrent:   2,
		Tasks: []PlannedChild{
			{Task: poolTask(rootA, "a")},
			{Task: poolTask(rootB, "b")},
		},
	}
	resultCh := make(chan OrchestrationResult, 1)
	go func() {
		result, _ := mustRunOrchestration(pool, plan)
		resultCh <- result
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-provider.started:
		case <-time.After(3 * time.Second):
			t.Fatal("process child did not reach provider barrier")
		}
	}
	close(provider.release)
	select {
	case result := <-resultCh:
		if result.Status != OrchestrationSucceeded {
			t.Fatalf("result=%#v", result)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("process orchestration did not complete")
	}
	if provider.Peak() != 2 {
		t.Fatalf("peak provider concurrency=%d, want 2", provider.Peak())
	}
	for _, check := range []struct{ root, task string }{{rootA, "a"}, {rootB, "b"}} {
		if _, err := os.Stat(filepath.Join(check.root, "generated", "children", check.task, "completion.json")); err != nil {
			t.Fatalf("completion artifact for %s: %v", check.task, err)
		}
	}
}

func TestProcessChildRunnerSuccessAndProviderBroker(t *testing.T) {
	root := t.TempDir()
	provider := &childTestProvider{}
	runner, err := NewProcessChildRunner(processChildApp(t, provider, nil), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := runner.Run(context.Background(), childTask(root))
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded || packet.Summary != "child completed" {
		t.Fatalf("packet = %#v", packet)
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(root, "generated", "children", "task-1", "completion.json")); err != nil {
		t.Fatalf("completion artifact: %v", err)
	}
}

func TestProcessChildRunnerApprovalStaysParentControlled(t *testing.T) {
	root := t.TempDir()
	var approvalsMu sync.Mutex
	approvals := 0
	registry, err := NewCodingRegistry(ExecutionScope{Root: root, TimeoutSeconds: 2, MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{
		Registry: registry,
		Scope:    ExecutionScope{Root: root, TimeoutSeconds: 2, MaxOutputBytes: 1024},
		Policy:   DefaultCodingPolicy{},
		Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
			approvalsMu.Lock()
			approvals++
			approvalsMu.Unlock()
			return ApprovalApproved, nil
		}),
	}
	provider := &processApprovalProvider{}
	task := childTask(root)
	task.AllowedTools = []string{"write_file"}
	runner, err := NewProcessChildRunner(processChildApp(t, provider, executor), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded {
		t.Fatalf("packet = %#v", packet)
	}
	approvalsMu.Lock()
	count := approvals
	approvalsMu.Unlock()
	if count != 1 {
		t.Fatalf("parent approvals = %d, want 1", count)
	}
	if data, err := os.ReadFile(filepath.Join(root, "approved.txt")); err != nil || string(data) != "approved" {
		t.Fatalf("approved file data=%q err=%v", data, err)
	}
}

func TestProcessChildRunnerRejectsUnsupportedParentPolicyAndAudit(t *testing.T) {
	root := t.TempDir()
	registry, err := NewCodingRegistry(ExecutionScope{Root: root, TimeoutSeconds: 2, MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{
		Registry: registry,
		Scope:    ExecutionScope{Root: root, TimeoutSeconds: 2, MaxOutputBytes: 1024},
		Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
			return PolicyAllow, nil
		}),
	}
	task := childTask(root)
	task.AllowedTools = []string{"read_file"}
	runner, err := NewProcessChildRunner(processChildApp(t, &childTestProvider{}, executor), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), task); err == nil || !strings.Contains(err.Error(), "custom policy serialization") {
		t.Fatalf("custom policy error = %v", err)
	}

	executor.Policy = DefaultCodingPolicy{}
	executor.Auditor = AuditorFunc(func(context.Context, AuditEvent) error { return nil })
	runner, err = NewProcessChildRunner(processChildApp(t, &childTestProvider{}, executor), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), task); err == nil || !strings.Contains(err.Error(), "audit brokering") {
		t.Fatalf("auditor error = %v", err)
	}
}

func TestProcessChildRunnerCancellation(t *testing.T) {
	root := t.TempDir()
	provider := &childTestProvider{mode: "timeout"}
	runner, err := NewProcessChildRunner(processChildApp(t, provider, nil), processChildConfig(t, "normal"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	packet, err := runner.Run(ctx, childTask(root))
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildTimedOut && packet.Status != ChildCancelled {
		t.Fatalf("packet = %#v", packet)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took too long: %s", elapsed)
	}
}

func TestProcessChildRunnerMalformedCrashAndOversizedIsolation(t *testing.T) {
	for _, mode := range []string{"crash", "malformed", "oversized", "unknown", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			runner, err := NewProcessChildRunner(processChildApp(t, &childTestProvider{}, nil), processChildConfig(t, mode))
			if err != nil {
				t.Fatal(err)
			}
			packet, err := runner.Run(context.Background(), childTask(root))
			if err != nil {
				t.Fatal(err)
			}
			if packet.Status != ChildMalformed {
				t.Fatalf("packet = %#v", packet)
			}
		})
	}
}

func TestChildProcessConfigRejectsSecretEnvironmentAndUnboundedWorkingDir(t *testing.T) {
	if err := (ChildProcessConfig{Command: "bee", EnvAllowlist: []string{"OPENAI_API_KEY"}}).Validate(); err == nil || !strings.Contains(err.Error(), "secret-shaped") {
		t.Fatalf("secret env validation = %v", err)
	}
	root := t.TempDir()
	config := processChildConfig(t, "normal")
	config.WorkingDir = filepath.Dir(root)
	runner, err := NewProcessChildRunner(processChildApp(t, &childTestProvider{}, nil), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), childTask(root)); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("working dir error = %v", err)
	}
}

func TestProcessChildHelper(t *testing.T) {
	mode := os.Getenv("BEE_CHILD_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "crash":
		os.Exit(3)
	case "malformed":
		_, _ = fmt.Fprintln(os.Stdout, "{not-json")
		return
	case "oversized":
		_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", 2<<20))
		return
	case "unknown", "duplicate":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return
		}
		var request struct {
			JSONRPC string `json:"jsonrpc"`
			ID      string `json:"id"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		response := map[string]any{"jsonrpc": "2.0", "id": "unknown", "result": map[string]any{"protocol": ChildProtocolVersion, "version": "6B", "task_id": "task-1"}}
		if mode == "duplicate" {
			response["id"] = request.ID
		}
		encoder := json.NewEncoder(os.Stdout)
		_ = encoder.Encode(response)
		if mode == "duplicate" {
			_ = encoder.Encode(response)
		}
		return
	}
	_ = ServeChildStdio(context.Background(), os.Stdin, os.Stdout)
}

type processApprovalProvider struct {
	mu    sync.Mutex
	calls int
}

func (provider *processApprovalProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.Stream(ctx, request, nil)
}

func (provider *processApprovalProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.mu.Unlock()
	if call == 1 {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "write-1", Name: "write_file", Arguments: `{"path":"approved.txt","content":"approved"}`}}}}, nil
	}
	if onDelta != nil {
		if err := onDelta("approved"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "approved"}}, nil
}
