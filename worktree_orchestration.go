package bee

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// WorktreeOrchestrator is the smallest Phase 6E seam. Read-only plans retain
// ParallelOrchestrator behavior. Mutable tasks are translated to isolated Git
// workspaces and return review-only proposals; nothing is merged automatically.
type WorktreeOrchestrator struct {
	Executor   ChildExecutor
	Workspace  WorkspaceManager
	BaseCommit string
	Trace      TraceSink
}

func NewWorktreeOrchestrator(executor ChildExecutor, workspace WorkspaceManager, baseCommit string, trace TraceSink) (*WorktreeOrchestrator, error) {
	if executor == nil {
		return nil, errors.New("child executor is required")
	}
	return &WorktreeOrchestrator{Executor: executor, Workspace: workspace, BaseCommit: baseCommit, Trace: trace}, nil
}

func (orchestrator *WorktreeOrchestrator) Run(ctx context.Context, plan OrchestrationPlan) (OrchestrationResult, error) {
	result := OrchestrationResult{OrchestrationID: plan.OrchestrationID, ParentID: plan.ParentID}
	if err := plan.Validate(); err != nil {
		return result, err
	}
	originals := make(map[string]ChildTask, len(plan.Tasks))
	hasMutable := false
	for _, planned := range plan.Tasks {
		mutability, err := parallelTaskMutability(planned.Task)
		if err != nil {
			return result, fmt.Errorf("task %q mutability: %w", planned.Task.TaskID, err)
		}
		originals[planned.Task.TaskID] = planned.Task
		if mutability == TaskMutable {
			hasMutable = true
		}
	}
	if !hasMutable {
		parallel, err := NewParallelOrchestrator(orchestrator.Executor, orchestrator.Trace)
		if err != nil {
			return result, err
		}
		return parallel.Run(ctx, plan)
	}
	if orchestrator.Workspace == nil {
		return result, errors.New("mutable tasks require a workspace manager")
	}
	if orchestrator.BaseCommit == "" {
		return result, errors.New("mutable tasks require a base commit")
	}

	// ParallelOrchestrator deliberately rejects mutable tasks generically. This
	// adapter proves isolation first, then presents a sanitized read-only view
	// to that scheduler while the wrapper restores each original task inside its
	// leased workspace. No caller-visible task contract is widened.
	executionPlan := plan
	executionPlan.Tasks = append([]PlannedChild(nil), plan.Tasks...)
	for index := range executionPlan.Tasks {
		if executionPlan.Tasks[index].Task.Mutability == TaskMutable {
			executionPlan.Tasks[index].Task.Mutability = TaskReadOnly
			executionPlan.Tasks[index].Task.AllowedTools = nil
		}
	}
	wrapped := &workspaceTaskExecutor{base: orchestrator.Executor, manager: orchestrator.Workspace, baseCommit: orchestrator.BaseCommit, originals: originals}
	parallel, err := NewParallelOrchestrator(wrapped, orchestrator.Trace)
	if err != nil {
		return result, err
	}
	return parallel.Run(ctx, executionPlan)
}

type workspaceTaskExecutor struct {
	base       ChildExecutor
	manager    WorkspaceManager
	baseCommit string
	originals  map[string]ChildTask
	mu         sync.Mutex
}

var _ ChildObservableExecutor = (*workspaceTaskExecutor)(nil)

func (executor *workspaceTaskExecutor) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return executor.run(ctx, task, nil)
}

func (executor *workspaceTaskExecutor) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	return executor.run(ctx, task, observer)
}

func (executor *workspaceTaskExecutor) run(ctx context.Context, scheduled ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	executor.mu.Lock()
	original, ok := executor.originals[scheduled.TaskID]
	executor.mu.Unlock()
	if !ok {
		return ChildCompletionPacket{TaskID: scheduled.TaskID, ParentID: scheduled.ParentID, Status: ChildMalformed, Summary: "workspace task was not in the parent plan"}, errors.New("workspace task was not in the parent plan")
	}
	if original.Mutability != TaskMutable {
		return runChildWithObserver(ctx, executor.base, scheduled, observer)
	}
	attemptID := original.TaskID + "-attempt-1"
	repositoryRoot := ""
	if provider, ok := executor.manager.(interface{ RepositoryRoot() string }); ok {
		repositoryRoot = provider.RepositoryRoot()
	}
	request := WorkspaceRequest{RepositoryRoot: repositoryRoot, ScopeRoot: original.Scope.Root, ArtifactDir: original.ArtifactDir, BaseCommit: executor.baseCommit, TaskID: original.TaskID, AttemptID: attemptID}
	lease, err := executor.manager.Acquire(ctx, request)
	if err != nil {
		packet := ChildCompletionPacket{TaskID: original.TaskID, ParentID: original.ParentID, Status: ChildFailed, Summary: "mutable workspace acquisition failed", Blockers: []string{err.Error()}}
		return packet, err
	}
	workspaceTask := original
	workspaceTask.Scope.Root = lease.ScopePath
	workspaceTask.ArtifactDir = lease.ChildArtifactDir
	workspaceTask.Workspace = &lease
	workspaceTask.ParentScopeRoot = original.Scope.Root
	packet, runErr := runChildWithObserver(ctx, executor.base, workspaceTask, observer)
	if packet.TaskID == "" {
		packet.TaskID = original.TaskID
	}
	if packet.ParentID == "" {
		packet.ParentID = original.ParentID
	}
	proposal, proposalErr := executor.manager.CollectProposal(ctx, lease, packet)
	if proposal.DiffArtifact != "" {
		packet.Artifacts = appendUnique(packet.Artifacts, proposal.DiffArtifact)
	}
	if proposal.ProposalArtifact != "" {
		packet.Artifacts = appendUnique(packet.Artifacts, proposal.ProposalArtifact)
	}
	if proposal.CompletionManifest != nil {
		packet.Artifacts = appendUnique(packet.Artifacts, proposal.CompletionManifest.Path)
	}
	if proposalErr != nil {
		packet.Blockers = appendUnique(packet.Blockers, proposalErr.Error())
		if runErr == nil {
			runErr = proposalErr
		}
	}
	if !proposal.ReadyForReview {
		for _, problem := range proposal.Errors {
			packet.Blockers = appendUnique(packet.Blockers, problem)
		}
		if packet.Status == ChildSucceeded {
			packet.Status = ChildFailed
		}
	}
	// Preserve is the default for both successful and failed/cancelled children;
	// callers must opt into Cleanup explicitly after human inspection.
	if preserveErr := executor.manager.Preserve(ctx, lease); preserveErr != nil {
		packet.Blockers = appendUnique(packet.Blockers, preserveErr.Error())
		if packet.Status == ChildSucceeded {
			packet.Status = ChildFailed
		}
		if runErr == nil {
			runErr = preserveErr
		}
	}
	if packet.Status == "" {
		packet.Status = ChildFailed
	}
	if packet.Summary == "" {
		packet.Summary = "mutable child completed"
	}
	return packet, runErr
}

func runChildWithObserver(ctx context.Context, executor ChildExecutor, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	if observable, ok := executor.(ChildObservableExecutor); ok {
		return observable.RunObserved(ctx, task, observer)
	}
	return executor.Run(ctx, task)
}
