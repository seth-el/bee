package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type fallbackBackend struct {
	name   string
	status ChildStatus
	err    error
	edit   bool
	mu     sync.Mutex
	calls  int
	scopes []string
}

func (backend *fallbackBackend) Run(_ context.Context, task CodingBackendTask, _ CodingBackendObserver) (CodingBackendCompletion, error) {
	backend.mu.Lock()
	backend.calls++
	backend.scopes = append(backend.scopes, task.Task.Scope.Root)
	backend.mu.Unlock()
	if backend.edit {
		_ = os.WriteFile(filepath.Join(task.Task.Scope.Root, "primary-attempt.txt"), []byte("failed\n"), 0o600)
	}
	status := backend.status
	if status == "" {
		status = ChildSucceeded
	}
	packet := ChildCompletionPacket{TaskID: task.Task.TaskID, ParentID: task.Task.ParentID, Status: status, Summary: backend.name}
	return CodingBackendCompletion{ChildCompletionPacket: packet, Backend: backend.name}, backend.err
}

func (backend *fallbackBackend) count() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.calls
}

func (backend *fallbackBackend) capturedScopes() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.scopes...)
}

type fallbackChild struct {
	mu     sync.Mutex
	calls  int
	scopes []string
}

func (child *fallbackChild) Run(_ context.Context, task ChildTask) (ChildCompletionPacket, error) {
	child.mu.Lock()
	child.calls++
	child.scopes = append(child.scopes, task.Scope.Root)
	child.mu.Unlock()
	if err := os.WriteFile(filepath.Join(task.Scope.Root, "local-child.txt"), []byte("fallback\n"), 0o600); err != nil {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildFailed, Summary: "local child write failed", Blockers: []string{err.Error()}}, err
	}
	return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildSucceeded, Summary: "local child fallback"}, nil
}

func (child *fallbackChild) count() int {
	child.mu.Lock()
	defer child.mu.Unlock()
	return child.calls
}

func TestCodingFallbackPrimarySuccessDoesNotRunLocalChild(t *testing.T) {
	root := t.TempDir()
	primary := &fallbackBackend{name: "acp-primary"}
	localChild := &fallbackChild{}
	executor := &codingFallbackExecutor{primary: &ACPChildExecutor{CodingBackend: primary}, fallback: localChild, requestedName: "acp-primary", fallbackName: CodingBackendNameLocalChild}
	packet, err := executor.Run(context.Background(), ChildTask{TaskID: "success", ParentID: "parent", Objective: "inspect", Prompt: "inspect", Scope: ExecutionScope{Root: root}, AcceptanceCriteria: []string{"done"}})
	if err != nil || packet.Status != ChildSucceeded {
		t.Fatalf("packet=%#v err=%v", packet, err)
	}
	if localChild.count() != 0 || packet.FallbackUsed || packet.EffectiveBackend != "acp-primary" {
		t.Fatalf("unexpected fallback: packet=%#v localChild=%d", packet, localChild.count())
	}
}

func TestCodingFallbackUnavailableUsesLocalChildOnlyOnFreshWorkspace(t *testing.T) {
	repo := t.TempDir()
	managed := t.TempDir()
	workspace := &delegationWorkspaceFake{root: managed}
	primary := &fallbackBackend{name: "acp-primary", status: ChildFailed, err: newCodingBackendFailure(BackendFailureUnavailable, errors.New("ACP adapter missing")), edit: true}
	localChild := &fallbackChild{}
	executor := &codingFallbackExecutor{primary: &ACPChildExecutor{CodingBackend: primary}, fallback: localChild, workspace: workspace, baseCommit: "base", requestedName: "acp-primary", fallbackName: CodingBackendNameLocalChild}
	first := ChildTask{TaskID: "fallback", ParentID: "parent", Objective: "change", Prompt: "change", Mutability: TaskMutable, Scope: ExecutionScope{Root: repo}, ParentScopeRoot: repo, Workspace: &WorkspaceLease{TaskID: "fallback", AttemptID: "fallback-attempt-1", RepositoryRoot: repo, WorkspacePath: filepath.Join(managed, "first"), ScopePath: filepath.Join(managed, "first"), ChildArtifactDir: filepath.Join(managed, "first", "artifacts"), State: WorkspaceLeaseActive}}
	if err := os.MkdirAll(first.Scope.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(first.Workspace.ChildArtifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	packet, err := executor.Run(context.Background(), first)
	if err != nil || packet.Status != ChildSucceeded {
		t.Fatalf("packet=%#v err=%v", packet, err)
	}
	if !packet.FallbackUsed || packet.RequestedBackend != "acp-primary" || packet.EffectiveBackend != CodingBackendNameLocalChild || len(packet.Attempts) != 2 {
		t.Fatalf("fallback evidence=%#v", packet)
	}
	if localChild.count() != 1 || len(workspace.got.AttemptID) == 0 || workspace.got.AttemptID == first.Workspace.AttemptID {
		t.Fatalf("workspace isolation got=%#v localChild=%d", workspace.got, localChild.count())
	}
	if len(primary.capturedScopes()) != 1 || filepath.Clean(primary.capturedScopes()[0]) == filepath.Clean(localChild.scopes[0]) {
		t.Fatalf("primary/local child scopes were reused: primary=%#v localChild=%#v", primary.capturedScopes(), localChild.scopes)
	}
}

func TestCodingFallbackDoesNotRetryTaskFailureTimeoutOrCancellation(t *testing.T) {
	cases := []struct {
		name   string
		status ChildStatus
		err    error
	}{
		{name: "task failure", status: ChildFailed, err: errors.New("task failed")},
		{name: "timeout", status: ChildTimedOut, err: context.DeadlineExceeded},
		{name: "cancel", status: ChildCancelled, err: context.Canceled},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			primary := &fallbackBackend{name: "acp-primary", status: test.status, err: test.err}
			localChild := &fallbackChild{}
			executor := &codingFallbackExecutor{primary: &ACPChildExecutor{CodingBackend: primary}, fallback: localChild, requestedName: "acp-primary", fallbackName: CodingBackendNameLocalChild}
			packet, err := executor.Run(context.Background(), ChildTask{TaskID: "terminal", ParentID: "parent", Objective: "run", Prompt: "run", Scope: ExecutionScope{Root: root}, AcceptanceCriteria: []string{"done"}})
			if localChild.count() != 0 || packet.FallbackUsed || codingBackendFailureKind(err, packet) == BackendFailureUnavailable {
				t.Fatalf("unexpected retry packet=%#v err=%v localChild=%d", packet, err, localChild.count())
			}
		})
	}
}

func TestCodingFallbackClassifiesAvailabilitySeparately(t *testing.T) {
	if got := acpFailureKind(context.Background(), nil, errors.New("ACP stdout closed")); got != BackendFailureUnavailable {
		t.Fatalf("startup failure kind=%q", got)
	}
	if got := acpFailureKind(context.Background(), nil, errors.New("ACP prompt failed: tests failed")); got != BackendFailureTask {
		t.Fatalf("task failure kind=%q", got)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-deadline.Done()
	if got := acpFailureKind(deadline, nil, context.DeadlineExceeded); got != BackendFailureTimeout {
		t.Fatalf("timeout failure kind=%q", got)
	}
}

func TestCodingFallbackConfigIsPerRoleAndLocalOnly(t *testing.T) {
	config := DefaultCodingBackendConfig()
	if config.EffectiveFallbacks().Implementer != CodingBackendNameLocalChild || config.EffectiveFallbacks().Reviewer != CodingBackendNameLocalChild || config.EffectiveFallbacks().Scout != CodingBackendNameLocalChild {
		t.Fatalf("default fallbacks=%#v", config.EffectiveFallbacks())
	}
	config.FallbackImplementer = "remote"
	if err := config.Validate(); err == nil {
		t.Fatal("expected non-local fallback rejection")
	}
}
