package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type delegationFakeBackend struct {
	name string
	mu   sync.Mutex
	task CodingBackendTask
}

func (backend *delegationFakeBackend) Run(_ context.Context, task CodingBackendTask, observer CodingBackendObserver) (CodingBackendCompletion, error) {
	backend.mu.Lock()
	backend.task = task
	backend.mu.Unlock()
	if observer != nil {
		observer.ObserveCodingBackendEvent(CodingBackendEvent{TaskID: task.Task.TaskID, ParentID: task.Task.ParentID, Type: "progress", Text: strings.Repeat("progress ", 100)})
	}
	packet := ChildCompletionPacket{TaskID: task.Task.TaskID, ParentID: task.Task.ParentID, Status: ChildSucceeded, Summary: strings.Repeat("done ", 2000), Evidence: []string{strings.Repeat("evidence ", 1000)}}
	return CodingBackendCompletion{ChildCompletionPacket: packet, Backend: backend.name}, nil
}

func (backend *delegationFakeBackend) captured() CodingBackendTask {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.task
}

type delegationWorkspaceFake struct {
	root string
	mu   sync.Mutex
	got  WorkspaceRequest
}

func (workspace *delegationWorkspaceFake) Acquire(_ context.Context, request WorkspaceRequest) (WorkspaceLease, error) {
	workspace.mu.Lock()
	workspace.got = request
	workspace.mu.Unlock()
	scope := filepath.Join(workspace.root, "worktree", request.TaskID, request.AttemptID)
	artifact := filepath.Join(workspace.root, "artifact", request.TaskID, request.AttemptID)
	if err := os.MkdirAll(artifact, 0o700); err != nil {
		return WorkspaceLease{}, err
	}
	if err := os.MkdirAll(scope, 0o700); err != nil {
		return WorkspaceLease{}, err
	}
	return WorkspaceLease{TaskID: request.TaskID, AttemptID: request.AttemptID, RepositoryRoot: request.RepositoryRoot, BaseCommit: request.BaseCommit, WorkspacePath: scope, ScopePath: scope, ChildArtifactDir: artifact, State: WorkspaceLeaseActive}, nil
}

func (workspace *delegationWorkspaceFake) Inspect(context.Context, WorkspaceLease) (WorkspaceInspection, error) {
	return WorkspaceInspection{}, nil
}
func (workspace *delegationWorkspaceFake) CollectProposal(_ context.Context, lease WorkspaceLease, packet ChildCompletionPacket) (IntegrationProposal, error) {
	return IntegrationProposal{TaskID: lease.TaskID, AttemptID: lease.AttemptID, WorkspacePath: lease.WorkspacePath, Tests: packet.Tests, Evidence: packet.Evidence, ReadyForReview: true}, nil
}
func (workspace *delegationWorkspaceFake) Preserve(context.Context, WorkspaceLease) error { return nil }
func (workspace *delegationWorkspaceFake) Cleanup(context.Context, WorkspaceLease) error  { return nil }

func delegationPlan() *TaskPlan {
	return &TaskPlan{ID: "plan-1", Goal: "bounded coding", Inputs: []string{"repo"}, Outputs: []string{"change"}, Steps: []string{"inspect", "implement", "test"}, Acceptance: []string{"tests pass"}}
}

func delegationRegistry(t *testing.T, first, second *delegationFakeBackend) *CodingBackendRegistry {
	t.Helper()
	config := DefaultCodingBackendConfig()
	config.Enabled = true
	config.DefaultImplementer = "first"
	config.DefaultReviewer = "first"
	config.DefaultScout = "first"
	config.Backends = []CodingBackendDefinition{{Name: "first", Kind: CodingBackendKindACP, Enabled: true, Command: "unused"}, {Name: "second", Kind: CodingBackendKindACP, Enabled: true, Command: "unused"}}
	registry, err := NewCodingBackendRegistryWithFactories(config, map[string]CodingBackendFactory{
		"first":  func(CodingBackendDefinition) (CodingBackend, error) { return first, nil },
		"second": func(CodingBackendDefinition) (CodingBackend, error) { return second, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func delegationRequest(root string) CodingDelegationRequest {
	return CodingDelegationRequest{Role: CodingRoleImplementer, Goal: "implement bounded change", Prompt: "inspect, implement, and test", ProjectRoot: root, AcceptanceCriteria: []string{"tests pass"}, Budget: CodingDelegationBudget{MaxSteps: 8, MaxOutputBytes: 8192, TimeoutSeconds: 2}}
}

func TestCodingDelegationDisabledRouting(t *testing.T) {
	root := t.TempDir()
	service, err := NewCodingDelegationService(mustDisabledCodingRegistry(t), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	service.PlanProvider = delegationPlan
	_, err = service.Delegate(context.Background(), CodingDelegationRequest{Role: CodingRoleScout, Goal: "inspect", ProjectRoot: root, AcceptanceCriteria: []string{"report"}})
	if !errors.Is(err, ErrCodingBackendsDisabled) {
		t.Fatalf("error = %v", err)
	}
}

func TestCodingDelegationRoleBackendOverride(t *testing.T) {
	root := t.TempDir()
	first, second := &delegationFakeBackend{name: "first"}, &delegationFakeBackend{name: "second"}
	service, err := NewCodingDelegationService(delegationRegistry(t, first, second), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	service.PlanProvider = delegationPlan
	request := delegationRequest(root)
	request.Role = CodingRoleScout
	request.Backend = "second"
	result, err := service.Delegate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != "second" || result.Completion.Status != ChildSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if second.captured().Task.TaskID == "" || first.captured().Task.TaskID != "" {
		t.Fatalf("backend selection first=%#v second=%#v", first.captured(), second.captured())
	}
}

func TestCodingDelegationRequiresParentPlan(t *testing.T) {
	root := t.TempDir()
	backend := &delegationFakeBackend{name: "first"}
	service, err := NewCodingDelegationService(delegationRegistry(t, backend, backend), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Delegate(context.Background(), delegationRequest(root))
	if !errors.Is(err, ErrCodingDelegationPlanRequired) {
		t.Fatalf("error = %v", err)
	}
}

func TestCodingDelegationWorktreeIsolationAndAuthorityStripping(t *testing.T) {
	root := t.TempDir()
	managed := t.TempDir()
	backend := &delegationFakeBackend{name: "first"}
	workspace := &delegationWorkspaceFake{root: managed}
	service, err := NewCodingDelegationService(delegationRegistry(t, backend, backend), ExecutionScope{Root: root}, workspace, "base")
	if err != nil {
		t.Fatal(err)
	}
	var events []CodingDelegationEvent
	service.Observer = func(event CodingDelegationEvent) { events = append(events, event) }
	service.PlanProvider = delegationPlan
	result, err := service.Delegate(context.Background(), delegationRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	task := backend.captured().Task
	if result.Completion.Status != ChildSucceeded || filepath.Clean(task.Scope.Root) == filepath.Clean(root) || !scopeWithin(managed, task.Scope.Root) {
		t.Fatalf("result=%#v task=%#v", result, task)
	}
	if task.Scope.TrustedForeground || task.Scope.RemoteGrant != nil || task.Scope.EnvAllowlist != nil || task.MemoryProjection != nil {
		t.Fatalf("parent authority leaked into child task: %#v", task)
	}
	for _, tool := range task.AllowedTools {
		if strings.Contains(tool, "memory") || strings.Contains(tool, "ssh") || strings.Contains(tool, "job") {
			t.Fatalf("authority-bearing tool leaked: %q", tool)
		}
	}
	if len(events) < 3 {
		t.Fatalf("delegation progress events = %#v", events)
	}
	if events[0].Type != "started" || events[len(events)-1].Type != "completed" {
		t.Fatalf("delegation event lifecycle = %#v", events)
	}
}

func TestCodingDelegationResolvesRelativeProjectRootWithinParentScope(t *testing.T) {
	root := t.TempDir()
	backend := &delegationFakeBackend{name: "first"}
	service, err := NewCodingDelegationService(delegationRegistry(t, backend, backend), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	service.PlanProvider = delegationPlan
	service.ProjectRoot = root
	request := delegationRequest(root)
	request.Role = CodingRoleScout
	request.ProjectRoot = "."
	result, err := service.Delegate(context.Background(), request)
	if err != nil || result.Completion.Status != ChildSucceeded {
		t.Fatalf("relative project root result=%#v err=%v", result, err)
	}
	if filepath.Clean(backend.captured().Task.Scope.Root) != filepath.Clean(root) {
		t.Fatalf("resolved project root=%q want %q", backend.captured().Task.Scope.Root, root)
	}
}

func TestCodingDelegationCompletionPacketIsBounded(t *testing.T) {
	root := t.TempDir()
	backend := &delegationFakeBackend{name: "first"}
	service, err := NewCodingDelegationService(delegationRegistry(t, backend, backend), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	service.PlanProvider = delegationPlan
	service.Limits.MaxResultBytes = 4096
	result, err := service.Delegate(context.Background(), CodingDelegationRequest{Role: CodingRoleScout, Goal: "inspect", ProjectRoot: root, AcceptanceCriteria: []string{"report"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.bounded(4096))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 4096 || len(result.Completion.Summary) > 4096 {
		t.Fatalf("unbounded completion: bytes=%d result=%#v", len(encoded), result)
	}
}

func TestDelegateCodingToolPlanGate(t *testing.T) {
	root := t.TempDir()
	backend := &delegationFakeBackend{name: "first"}
	service, err := NewCodingDelegationService(delegationRegistry(t, backend, backend), ExecutionScope{Root: root}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(NewCodingDelegationTool(service))
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: root}, planGate: &PlanGate{}}
	_, err = executor.Execute(context.Background(), CodingDelegationToolName, []byte(`{"role":"scout","goal":"inspect","project_root":"`+root+`","acceptance_criteria":["report"]}`))
	if !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("error = %v", err)
	}
}

func mustDisabledCodingRegistry(t *testing.T) *CodingBackendRegistry {
	t.Helper()
	registry, err := NewCodingBackendRegistry(DefaultCodingBackendConfig())
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
