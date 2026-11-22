package bee

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// codingFallbackExecutor is deliberately narrow: it falls back only when the
// requested backend is unavailable. A failed, timed out, cancelled, denied,
// or malformed task is terminal evidence and is never retried by the local child.
type codingFallbackExecutor struct {
	primary       ChildExecutor
	fallback      ChildExecutor
	workspace     WorkspaceManager
	baseCommit    string
	requestedName string
	fallbackName  string
}

func (executor *codingFallbackExecutor) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return executor.RunObserved(ctx, task, nil)
}

func (executor *codingFallbackExecutor) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	if executor == nil || executor.primary == nil {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildMalformed, Summary: "coding primary backend is not configured"}, errors.New("coding primary backend is not configured")
	}
	primaryPacket, primaryErr := runChildWithObserver(ctx, executor.primary, task, observer)
	primaryKind := codingBackendFailureKind(primaryErr, primaryPacket)
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			primaryKind = BackendFailureTimeout
		} else {
			primaryKind = BackendFailureCancelled
		}
	}
	primaryAttempt := CodingBackendAttempt{Backend: executor.requestedName, Status: primaryPacket.Status, FailureKind: primaryKind, Summary: primaryPacket.Summary}
	if task.Workspace != nil {
		primaryAttempt.AttemptID = task.Workspace.AttemptID
		primaryAttempt.WorkspacePath = task.Workspace.WorkspacePath
	}
	if primaryKind != BackendFailureUnavailable || executor.fallback == nil {
		return annotateCodingPacket(primaryPacket, executor.requestedName, executor.requestedName, false, "", []CodingBackendAttempt{primaryAttempt}), primaryErr
	}

	// Preserve the failed attempt before acquiring another lease. This is the
	// critical isolation rule: the local child never receives the dirty primary worktree.
	if task.Workspace != nil && executor.workspace != nil {
		if err := executor.workspace.Preserve(ctx, *task.Workspace); err != nil {
			primaryPacket.Blockers = appendUnique(primaryPacket.Blockers, err.Error())
			return annotateCodingPacket(primaryPacket, executor.requestedName, executor.requestedName, false, "failed attempt could not be preserved", []CodingBackendAttempt{primaryAttempt}), err
		}
	}

	fallbackTask := task
	fallbackAttempt := ""
	if task.Workspace != nil && executor.workspace != nil {
		scopeRoot := task.ParentScopeRoot
		if scopeRoot == "" {
			scopeRoot = task.Scope.Root
		}
		repositoryRoot := task.Workspace.RepositoryRoot
		artifactDir := ""
		if relative, err := filepath.Rel(task.Workspace.ScopePath, task.Workspace.ChildArtifactDir); err == nil && relative != "." && relative != ".." {
			artifactDir = filepath.Join(".", relative)
		}
		fallbackAttempt = fmt.Sprintf("%s-fallback-%d", task.TaskID, time.Now().UnixNano())
		lease, err := executor.workspace.Acquire(ctx, WorkspaceRequest{RepositoryRoot: repositoryRoot, ScopeRoot: scopeRoot, ArtifactDir: artifactDir, BaseCommit: executor.baseCommit, TaskID: task.TaskID, AttemptID: fallbackAttempt})
		if err != nil {
			primaryPacket.Blockers = appendUnique(primaryPacket.Blockers, "local fallback workspace acquisition failed: "+err.Error())
			return annotateCodingPacket(primaryPacket, executor.requestedName, executor.requestedName, false, "local fallback workspace unavailable", []CodingBackendAttempt{primaryAttempt}), err
		}
		fallbackTask.Scope.Root = lease.ScopePath
		fallbackTask.ArtifactDir = lease.ChildArtifactDir
		fallbackTask.Workspace = &lease
		fallbackTask.ParentScopeRoot = scopeRoot
		defer func() { _ = executor.workspace.Preserve(context.Background(), lease) }()
	}
	fallbackPacket, fallbackErr := runChildWithObserver(ctx, executor.fallback, fallbackTask, observer)
	if fallbackErr != nil && fallbackPacket.Status == "" {
		fallbackPacket.Status = ChildFailed
		fallbackPacket.Summary = "local fallback failed: " + truncateText(fallbackErr.Error(), 1024)
	}
	fallbackKind := codingBackendFailureKind(fallbackErr, fallbackPacket)
	if fallbackPacket.FailureKind == "" && (fallbackErr != nil || fallbackPacket.Status != ChildSucceeded) {
		fallbackPacket.FailureKind = fallbackKind
	}
	fallbackEvidence := CodingBackendAttempt{Backend: executor.fallbackName, Status: fallbackPacket.Status, FailureKind: fallbackKind, Summary: fallbackPacket.Summary}
	if fallbackTask.Workspace != nil {
		fallbackEvidence.AttemptID = fallbackTask.Workspace.AttemptID
		fallbackEvidence.WorkspacePath = fallbackTask.Workspace.WorkspacePath
	}
	if executor.workspace != nil && fallbackTask.Workspace != nil {
		proposal, proposalErr := executor.workspace.CollectProposal(ctx, *fallbackTask.Workspace, fallbackPacket)
		if proposal.DiffArtifact != "" {
			fallbackPacket.Artifacts = appendUnique(fallbackPacket.Artifacts, proposal.DiffArtifact)
		}
		if proposal.ProposalArtifact != "" {
			fallbackPacket.Artifacts = appendUnique(fallbackPacket.Artifacts, proposal.ProposalArtifact)
		}
		if proposal.CompletionManifest != nil {
			fallbackPacket.Artifacts = appendUnique(fallbackPacket.Artifacts, proposal.CompletionManifest.Path)
		}
		if proposalErr != nil {
			fallbackPacket.Blockers = appendUnique(fallbackPacket.Blockers, proposalErr.Error())
			if fallbackErr == nil {
				fallbackErr = proposalErr
			}
		}
	}
	reason := "requested backend unavailable"
	return annotateCodingPacket(fallbackPacket, executor.requestedName, executor.fallbackName, true, reason, []CodingBackendAttempt{primaryAttempt, fallbackEvidence}), fallbackErr
}

func annotateCodingPacket(packet ChildCompletionPacket, requested, effective string, fallbackUsed bool, reason string, attempts []CodingBackendAttempt) ChildCompletionPacket {
	packet.RequestedBackend = requested
	packet.EffectiveBackend = effective
	packet.FallbackUsed = fallbackUsed
	packet.FallbackReason = reason
	packet.Attempts = append([]CodingBackendAttempt(nil), attempts...)
	return boundCompletion(packet)
}
