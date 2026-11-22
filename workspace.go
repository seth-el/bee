package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	workspaceMetadataVersion = 1
	workspaceCommandLimit    = 1 << 20
	workspaceDiffLimit       = 512 << 10
)

// WorkspaceLeaseState is the conservative lifecycle state of one isolated
// mutable task workspace. Workspaces are preserved unless Cleanup is called.
type WorkspaceLeaseState string

const (
	WorkspaceLeaseActive    WorkspaceLeaseState = "active"
	WorkspaceLeasePreserved WorkspaceLeaseState = "preserved"
	WorkspaceLeaseRemoved   WorkspaceLeaseState = "removed"
)

// WorkspaceRequest is the provider-neutral input to a workspace manager.
// ScopeRoot and ArtifactDir describe the original task before isolation.
type WorkspaceRequest struct {
	RepositoryRoot string `json:"repository_root"`
	ScopeRoot      string `json:"scope_root"`
	ArtifactDir    string `json:"artifact_dir,omitempty"`
	BaseCommit     string `json:"base_commit"`
	TaskID         string `json:"task_id"`
	AttemptID      string `json:"attempt_id"`
}

// WorkspaceLease identifies the isolated paths a child may use. ScopePath and
// ChildArtifactDir are the translated values to place in ChildTask.
type WorkspaceLease struct {
	LeaseID          string              `json:"lease_id"`
	TaskID           string              `json:"task_id"`
	AttemptID        string              `json:"attempt_id"`
	RepositoryRoot   string              `json:"repository_root"`
	BaseCommit       string              `json:"base_commit"`
	WorkspacePath    string              `json:"workspace_path"`
	ScopePath        string              `json:"scope_path"`
	ChildArtifactDir string              `json:"child_artifact_dir"`
	ProposalArtifact string              `json:"proposal_artifact"`
	State            WorkspaceLeaseState `json:"state"`
	CreatedAt        time.Time           `json:"created_at"`
}

// WorkspaceInspection is bounded evidence collected without modifying the
// workspace or the parent repository.
type WorkspaceInspection struct {
	ChangedFiles   []string `json:"changed_files,omitempty"`
	UntrackedFiles []string `json:"untracked_files,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
	Diff           []byte   `json:"-"`
	DiffTruncated  bool     `json:"diff_truncated,omitempty"`
	Errors         []string `json:"errors,omitempty"`
}

// IntegrationProposal is review-only output. Bee never applies, cherry-picks,
// merges, or otherwise executes this proposal automatically.
type IntegrationProposal struct {
	TaskID             string                       `json:"task_id"`
	AttemptID          string                       `json:"attempt_id"`
	BaseCommit         string                       `json:"base_commit"`
	WorkspacePath      string                       `json:"workspace_path"`
	ChangedFiles       []string                     `json:"changed_files,omitempty"`
	UntrackedFiles     []string                     `json:"untracked_files,omitempty"`
	DiffArtifact       string                       `json:"diff_artifact,omitempty"`
	ProposalArtifact   string                       `json:"proposal_artifact,omitempty"`
	Conflicts          []string                     `json:"conflicts,omitempty"`
	Errors             []string                     `json:"errors,omitempty"`
	Tests              []string                     `json:"tests,omitempty"`
	Evidence           []string                     `json:"evidence,omitempty"`
	Artifacts          []string                     `json:"artifacts,omitempty"`
	ReadyForReview     bool                         `json:"ready_for_review"`
	CompletionManifest *CompletionManifestReference `json:"completion_manifest,omitempty"`
}

// WorkspaceManager is the provider-neutral lease boundary for mutable work.
type WorkspaceManager interface {
	Acquire(context.Context, WorkspaceRequest) (WorkspaceLease, error)
	Inspect(context.Context, WorkspaceLease) (WorkspaceInspection, error)
	CollectProposal(context.Context, WorkspaceLease, ChildCompletionPacket) (IntegrationProposal, error)
	Preserve(context.Context, WorkspaceLease) error
	Cleanup(context.Context, WorkspaceLease) error
}

// GitWorkspaceManager implements WorkspaceManager using only bounded argv
// invocations of git. It owns a dedicated Bee-managed root containing
// worktrees, review artifacts, and lease metadata.
type GitWorkspaceManager struct {
	repositoryRoot string
	managedRoot    string
	worktreeRoot   string
	artifactRoot   string
	metadataRoot   string
	trace          TraceSink
	mu             sync.Mutex
}

var _ WorkspaceManager = (*GitWorkspaceManager)(nil)

// NewGitWorkspaceManager creates or opens a dedicated Bee-managed root. An
// existing directory is accepted only when it has Bee's marker file; this
// prevents accidental adoption of arbitrary user data.
func NewGitWorkspaceManager(repositoryRoot, managedRoot string, trace TraceSink) (*GitWorkspaceManager, error) {
	repo, err := absoluteExistingDir(repositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("repository root: %w", err)
	}
	managed, err := absoluteManagedRoot(managedRoot)
	if err != nil {
		return nil, fmt.Errorf("managed workspace root: %w", err)
	}
	if filepath.Clean(repo) == filepath.Clean(managed) || scopeWithin(managed, repo) {
		return nil, errors.New("managed workspace root must not contain or equal repository root")
	}
	if err := ensureManagedRoot(managed); err != nil {
		return nil, err
	}
	manager := &GitWorkspaceManager{
		repositoryRoot: repo,
		managedRoot:    managed,
		worktreeRoot:   filepath.Join(managed, "worktrees"),
		artifactRoot:   filepath.Join(managed, "artifacts"),
		metadataRoot:   filepath.Join(managed, "leases"),
		trace:          trace,
	}
	for _, dir := range []string{manager.worktreeRoot, manager.artifactRoot, manager.metadataRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create managed directory: %w", err)
		}
	}
	return manager, nil
}

// RepositoryRoot exposes the validated repository identity for adapters that
// need to populate a provider-neutral WorkspaceRequest.
func (manager *GitWorkspaceManager) RepositoryRoot() string {
	if manager == nil {
		return ""
	}
	return manager.repositoryRoot
}

func (manager *GitWorkspaceManager) Acquire(ctx context.Context, request WorkspaceRequest) (WorkspaceLease, error) {
	var lease WorkspaceLease
	if manager == nil {
		return lease, errors.New("workspace manager is nil")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateWorkspaceRequest(request); err != nil {
		return lease, err
	}
	if err := manager.validateRepository(ctx, request); err != nil {
		return lease, err
	}
	if err := manager.validateLeaseMetadata(); err != nil {
		return lease, err
	}
	workspacePath := filepath.Join(manager.worktreeRoot, request.TaskID, request.AttemptID)
	artifactPath := filepath.Join(manager.artifactRoot, request.TaskID, request.AttemptID)
	metadataPath := manager.metadataPath(request.TaskID, request.AttemptID)
	for _, path := range []string{workspacePath, artifactPath, metadataPath} {
		if _, err := os.Lstat(path); err == nil {
			return lease, fmt.Errorf("workspace lease path already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return lease, fmt.Errorf("inspect workspace lease path %s: %w", path, err)
		}
	}
	if err := manager.validateRegisteredWorktrees(); err != nil {
		return lease, err
	}
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0o700); err != nil {
		return lease, fmt.Errorf("create workspace parent: %w", err)
	}
	if err := os.MkdirAll(artifactPath, 0o700); err != nil {
		return lease, fmt.Errorf("create proposal artifact directory: %w", err)
	}
	result, err := runGit(ctx, manager.repositoryRoot, "worktree", "add", "--detach", "--", workspacePath, request.BaseCommit)
	if err != nil {
		_ = os.RemoveAll(artifactPath)
		_ = os.Remove(workspacePath)
		return lease, fmt.Errorf("create git worktree: %w", err)
	}
	if result.Truncated {
		_ = os.RemoveAll(artifactPath)
		_ = os.Remove(workspacePath)
		return lease, errors.New("git worktree creation output exceeded bound")
	}
	scopeRoot, err := filepath.EvalSymlinks(request.ScopeRoot)
	if err != nil {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.RemoveAll(artifactPath)
		return lease, fmt.Errorf("resolve task scope: %w", err)
	}
	relScope, err := filepath.Rel(manager.repositoryRoot, filepath.Clean(scopeRoot))
	if err != nil || relScope == ".." || strings.HasPrefix(relScope, ".."+string(filepath.Separator)) {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.RemoveAll(artifactPath)
		return lease, errors.New("task scope is outside repository root")
	}
	translatedScope := filepath.Join(workspacePath, relScope)
	artifactOriginal, err := resolveArtifactPath(scopeRoot, request.ArtifactDir, request.TaskID)
	if err != nil {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.RemoveAll(artifactPath)
		return lease, fmt.Errorf("task artifacts: %w", err)
	}
	relArtifact, err := filepath.Rel(scopeRoot, artifactOriginal)
	if err != nil || relArtifact == ".." || strings.HasPrefix(relArtifact, ".."+string(filepath.Separator)) {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.RemoveAll(artifactPath)
		return lease, errors.New("task artifact directory is outside task scope")
	}
	lease = WorkspaceLease{
		LeaseID:          request.TaskID + "--" + request.AttemptID,
		TaskID:           request.TaskID,
		AttemptID:        request.AttemptID,
		RepositoryRoot:   manager.repositoryRoot,
		BaseCommit:       request.BaseCommit,
		WorkspacePath:    workspacePath,
		ScopePath:        translatedScope,
		ChildArtifactDir: filepath.Join(translatedScope, relArtifact),
		ProposalArtifact: filepath.Join(artifactPath, "integration-proposal.json"),
		State:            WorkspaceLeaseActive,
		CreatedAt:        time.Now().UTC(),
	}
	if err := manager.writeMetadata(lease); err != nil {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.RemoveAll(artifactPath)
		return WorkspaceLease{}, err
	}
	if err := manager.recordTrace(ctx, lease, "workspace_lease_created", ""); err != nil {
		_ = manager.removeGitWorktree(ctx, workspacePath)
		_ = os.Remove(manager.metadataPath(request.TaskID, request.AttemptID))
		_ = os.RemoveAll(artifactPath)
		return WorkspaceLease{}, err
	}
	return lease, nil
}

func (manager *GitWorkspaceManager) Inspect(ctx context.Context, lease WorkspaceLease) (WorkspaceInspection, error) {
	if manager == nil {
		return WorkspaceInspection{}, errors.New("workspace manager is nil")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := manager.validateLease(lease); err != nil {
		return WorkspaceInspection{}, err
	}
	if err := manager.validateRepositoryClean(ctx); err != nil {
		return WorkspaceInspection{}, err
	}
	if err := manager.validateRegisteredWorktrees(); err != nil {
		return WorkspaceInspection{}, err
	}
	if _, err := os.Stat(lease.WorkspacePath); err != nil {
		return WorkspaceInspection{}, fmt.Errorf("workspace is unavailable: %w", err)
	}
	changed, err := runGit(ctx, lease.WorkspacePath, "diff", "--name-only", "--no-renames", lease.BaseCommit, "--")
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("inspect changed files: %w", err)
	}
	untracked, err := runGit(ctx, lease.WorkspacePath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("inspect untracked files: %w", err)
	}
	conflicts, err := runGit(ctx, lease.WorkspacePath, "diff", "--name-only", "--diff-filter=U", "--")
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("inspect conflicts: %w", err)
	}
	diffResult, err := runGitWithLimit(ctx, lease.WorkspacePath, workspaceDiffLimit, "diff", "--binary", lease.BaseCommit, "--")
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("collect diff: %w", err)
	}
	inspection := WorkspaceInspection{
		ChangedFiles:   splitLines(changed.Stdout),
		UntrackedFiles: splitLines(untracked.Stdout),
		Conflicts:      splitLines(conflicts.Stdout),
		Diff:           []byte(diffResult.Stdout),
		DiffTruncated:  diffResult.Truncated,
	}
	if err := manager.recordTrace(ctx, lease, "workspace_inspected", fmt.Sprintf("changed=%d untracked=%d", len(inspection.ChangedFiles), len(inspection.UntrackedFiles))); err != nil {
		return inspection, err
	}
	return inspection, nil
}

func (manager *GitWorkspaceManager) CollectProposal(ctx context.Context, lease WorkspaceLease, completion ChildCompletionPacket) (IntegrationProposal, error) {
	if manager == nil {
		return IntegrationProposal{}, errors.New("workspace manager is nil")
	}
	proposal := IntegrationProposal{TaskID: lease.TaskID, AttemptID: lease.AttemptID, BaseCommit: lease.BaseCommit, WorkspacePath: lease.WorkspacePath, Tests: append([]string(nil), completion.Tests...), Evidence: append([]string(nil), completion.Evidence...), Artifacts: append([]string(nil), completion.Artifacts...)}
	inspection, err := manager.Inspect(ctx, lease)
	if err != nil {
		proposal.Errors = append(proposal.Errors, err.Error())
	} else {
		proposal.ChangedFiles = append([]string(nil), inspection.ChangedFiles...)
		proposal.UntrackedFiles = append([]string(nil), inspection.UntrackedFiles...)
		proposal.Conflicts = append([]string(nil), inspection.Conflicts...)
		if inspection.DiffTruncated {
			proposal.Errors = append(proposal.Errors, "bounded diff was truncated")
		}
	}
	if completion.Status != ChildSucceeded {
		proposal.Errors = append(proposal.Errors, fmt.Sprintf("child status is %s", completion.Status))
	}
	proposal.Errors = append(proposal.Errors, completion.Blockers...)
	proposal.DiffArtifact = filepath.Join(filepath.Dir(lease.ProposalArtifact), "workspace.diff")
	proposal.ProposalArtifact = lease.ProposalArtifact
	diffWritten := false
	if err == nil {
		data := append([]byte(nil), inspection.Diff...)
		if len(inspection.UntrackedFiles) > 0 {
			data = append(data, []byte("\n# Untracked files (content not included)\n")...)
			for _, name := range inspection.UntrackedFiles {
				data = append(data, []byte(name+"\n")...)
			}
		}
		if writeErr := writePrivateFile(proposal.DiffArtifact, data); writeErr != nil {
			proposal.Errors = append(proposal.Errors, writeErr.Error())
		} else {
			diffWritten = true
		}
	}
	proposal.ReadyForReview = len(proposal.Errors) == 0 && len(proposal.Conflicts) == 0 && proposal.DiffArtifact != ""
	manifest, manifestErr := writeIntegrationCompletionManifest(lease, completion, proposal, diffWritten)
	if manifestErr != nil {
		proposal.Errors = append(proposal.Errors, "write integration completion manifest: "+manifestErr.Error())
		proposal.ReadyForReview = false
	} else {
		proposal.CompletionManifest = manifest
		proposal.Artifacts = appendUnique(proposal.Artifacts, manifest.Path)
	}
	if writeErr := writeJSONPrivateFile(proposal.ProposalArtifact, proposal); writeErr != nil {
		proposal.Errors = append(proposal.Errors, writeErr.Error())
		proposal.ReadyForReview = false
		return proposal, writeErr
	}
	if manifestErr != nil {
		return proposal, manifestErr
	}
	if err := manager.recordTrace(ctx, lease, "integration_proposal_collected", fmt.Sprintf("ready=%t", proposal.ReadyForReview)); err != nil {
		return proposal, err
	}
	return proposal, nil
}

func (manager *GitWorkspaceManager) Preserve(ctx context.Context, lease WorkspaceLease) error {
	if manager == nil {
		return errors.New("workspace manager is nil")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := manager.validateLease(lease); err != nil {
		return err
	}
	lease.State = WorkspaceLeasePreserved
	if err := manager.writeMetadata(lease); err != nil {
		return err
	}
	return manager.recordTrace(ctx, lease, "workspace_preserved", "preserved for review")
}

func (manager *GitWorkspaceManager) Cleanup(ctx context.Context, lease WorkspaceLease) error {
	if manager == nil {
		return errors.New("workspace manager is nil")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := manager.validateLease(lease); err != nil {
		return err
	}
	if err := manager.removeGitWorktree(ctx, lease.WorkspacePath); err != nil {
		return err
	}
	lease.State = WorkspaceLeaseRemoved
	if err := manager.writeMetadata(lease); err != nil {
		return err
	}
	if err := manager.recordTrace(ctx, lease, "workspace_removed", "explicit cleanup"); err != nil {
		return err
	}
	return nil
}

func (manager *GitWorkspaceManager) validateRepository(ctx context.Context, request WorkspaceRequest) error {
	if request.RepositoryRoot != "" {
		requested, err := filepath.Abs(request.RepositoryRoot)
		if err != nil || filepath.Clean(requested) != manager.repositoryRoot {
			return errors.New("workspace request repository root does not match manager")
		}
	}
	if scopeWithin(manager.repositoryRoot, manager.managedRoot) {
		relative, err := filepath.Rel(manager.repositoryRoot, manager.managedRoot)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("managed workspace root is ambiguous")
		}
		if _, err := runGit(ctx, manager.repositoryRoot, "check-ignore", "--quiet", "--", relative); err != nil {
			return errors.New("managed workspace root inside repository must be Git-ignored")
		}
	}
	if err := manager.validateRepositoryClean(ctx); err != nil {
		return err
	}
	rootResult, err := runGit(ctx, manager.repositoryRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("repository root is not a git repository: %w", err)
	}
	if filepath.Clean(strings.TrimSpace(rootResult.Stdout)) != manager.repositoryRoot {
		return errors.New("repository root does not match git top-level")
	}
	verify, err := runGit(ctx, manager.repositoryRoot, "rev-parse", "--verify", request.BaseCommit+"^{commit}")
	if err != nil || strings.TrimSpace(verify.Stdout) == "" {
		if err == nil {
			err = errors.New("base commit did not resolve")
		}
		return fmt.Errorf("base commit %q is invalid: %w", request.BaseCommit, err)
	}
	scopeRoot, err := absoluteExistingDir(request.ScopeRoot)
	if err != nil {
		return fmt.Errorf("task scope: %w", err)
	}
	if !scopeWithin(manager.repositoryRoot, scopeRoot) {
		return errors.New("task scope is outside repository root")
	}
	scopeResult, err := runGit(ctx, scopeRoot, "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(strings.TrimSpace(scopeResult.Stdout)) != manager.repositoryRoot {
		return errors.New("task scope is not part of the configured git repository")
	}
	if _, err := resolveArtifactPath(scopeRoot, request.ArtifactDir, request.TaskID); err != nil {
		return fmt.Errorf("task artifact path: %w", err)
	}
	return nil
}

func (manager *GitWorkspaceManager) validateRepositoryClean(ctx context.Context) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is unavailable: %w", err)
	}
	status, err := runGit(ctx, manager.repositoryRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect parent repository: %w", err)
	}
	if strings.TrimSpace(status.Stdout) != "" {
		return errors.New("parent repository is dirty; refusing to create or inspect mutable workspace")
	}
	return nil
}

func (manager *GitWorkspaceManager) validateLeaseMetadata() error {
	entries, err := os.ReadDir(manager.metadataRoot)
	if err != nil {
		return fmt.Errorf("read workspace lease metadata: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskEntries, err := os.ReadDir(filepath.Join(manager.metadataRoot, entry.Name()))
		if err != nil {
			return fmt.Errorf("read lease metadata %s: %w", entry.Name(), err)
		}
		for _, taskEntry := range taskEntries {
			if taskEntry.IsDir() || !strings.HasSuffix(taskEntry.Name(), ".json") {
				continue
			}
			var lease WorkspaceLease
			if err := readJSONPrivateFile(filepath.Join(manager.metadataRoot, entry.Name(), taskEntry.Name()), &lease); err != nil {
				return fmt.Errorf("invalid lease metadata %s: %w", taskEntry.Name(), err)
			}
			if lease.State == WorkspaceLeaseActive || lease.State == WorkspaceLeasePreserved {
				if !safeWorkspaceComponent(lease.TaskID) || !safeWorkspaceComponent(lease.AttemptID) || !scopeWithin(manager.worktreeRoot, lease.WorkspacePath) {
					return fmt.Errorf("unsafe workspace lease metadata for %s", lease.LeaseID)
				}
				if _, err := os.Stat(lease.WorkspacePath); err != nil {
					return fmt.Errorf("stale workspace lease metadata for %s: %w", lease.LeaseID, err)
				}
			}
		}
	}
	return nil
}

func (manager *GitWorkspaceManager) validateRegisteredWorktrees() error {
	result, err := runGit(context.Background(), manager.repositoryRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("inspect git worktrees: %w", err)
	}
	registered := make(map[string]struct{})
	for _, path := range parseWorktreePaths(result.Stdout) {
		registered[filepath.Clean(path)] = struct{}{}
		if scopeWithin(manager.worktreeRoot, path) {
			metadata, err := manager.metadataForWorkspace(path)
			if err != nil {
				return err
			}
			if metadata.State == WorkspaceLeaseRemoved {
				return fmt.Errorf("removed workspace remains registered with git: %s", path)
			}
		}
	}
	entries, err := os.ReadDir(manager.metadataRoot)
	if err != nil {
		return fmt.Errorf("read workspace lease metadata: %w", err)
	}
	for _, taskEntry := range entries {
		if !taskEntry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(manager.metadataRoot, taskEntry.Name()))
		if err != nil {
			return fmt.Errorf("read lease metadata %s: %w", taskEntry.Name(), err)
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			var lease WorkspaceLease
			if err := readJSONPrivateFile(filepath.Join(manager.metadataRoot, taskEntry.Name(), file.Name()), &lease); err != nil {
				return fmt.Errorf("invalid lease metadata %s: %w", file.Name(), err)
			}
			if (lease.State == WorkspaceLeaseActive || lease.State == WorkspaceLeasePreserved) && scopeWithin(manager.worktreeRoot, lease.WorkspacePath) {
				if _, ok := registered[filepath.Clean(lease.WorkspacePath)]; !ok {
					return fmt.Errorf("workspace lease is not registered with git: %s", lease.WorkspacePath)
				}
			}
		}
	}
	return nil
}

func (manager *GitWorkspaceManager) metadataForWorkspace(path string) (WorkspaceLease, error) {
	entries, err := os.ReadDir(manager.metadataRoot)
	if err != nil {
		return WorkspaceLease{}, err
	}
	for _, task := range entries {
		if !task.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(manager.metadataRoot, task.Name()))
		if err != nil {
			return WorkspaceLease{}, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			var lease WorkspaceLease
			if err := readJSONPrivateFile(filepath.Join(manager.metadataRoot, task.Name(), file.Name()), &lease); err != nil {
				return WorkspaceLease{}, err
			}
			if filepath.Clean(lease.WorkspacePath) == filepath.Clean(path) {
				return lease, nil
			}
		}
	}
	return WorkspaceLease{}, fmt.Errorf("orphaned git worktree under Bee root: %s", path)
}

func (manager *GitWorkspaceManager) validateLease(lease WorkspaceLease) error {
	if lease.RepositoryRoot != manager.repositoryRoot {
		return errors.New("workspace lease repository does not match manager")
	}
	if !safeWorkspaceComponent(lease.TaskID) || !safeWorkspaceComponent(lease.AttemptID) {
		return errors.New("workspace lease contains unsafe task or attempt id")
	}
	if !scopeWithin(manager.worktreeRoot, lease.WorkspacePath) || !scopeWithin(manager.artifactRoot, filepath.Dir(lease.ProposalArtifact)) {
		return errors.New("workspace lease path is outside Bee-managed roots")
	}
	metadataPath := manager.metadataPath(lease.TaskID, lease.AttemptID)
	var stored WorkspaceLease
	if err := readJSONPrivateFile(metadataPath, &stored); err != nil {
		return fmt.Errorf("read workspace lease metadata: %w", err)
	}
	if stored.LeaseID != lease.LeaseID || stored.WorkspacePath != lease.WorkspacePath {
		return errors.New("workspace lease metadata does not match supplied lease")
	}
	if stored.State == WorkspaceLeaseRemoved {
		return errors.New("workspace lease has already been removed")
	}
	return nil
}

func (manager *GitWorkspaceManager) writeMetadata(lease WorkspaceLease) error {
	if err := os.MkdirAll(filepath.Dir(manager.metadataPath(lease.TaskID, lease.AttemptID)), 0o700); err != nil {
		return fmt.Errorf("create lease metadata directory: %w", err)
	}
	return writeJSONPrivateFile(manager.metadataPath(lease.TaskID, lease.AttemptID), lease)
}

func (manager *GitWorkspaceManager) metadataPath(taskID, attemptID string) string {
	return filepath.Join(manager.metadataRoot, taskID, attemptID+".json")
}

func (manager *GitWorkspaceManager) removeGitWorktree(ctx context.Context, path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := runGitSimple(ctx, manager.repositoryRoot, "worktree", "remove", "--force", "--", path); err != nil {
		return fmt.Errorf("remove git worktree: %w", err)
	}
	return nil
}

func (manager *GitWorkspaceManager) recordTrace(ctx context.Context, lease WorkspaceLease, eventType, info string) error {
	if manager.trace == nil {
		return nil
	}
	return manager.trace.AppendTrace(ctx, TraceEvent{OrchestrationID: "workspace", TaskID: lease.TaskID, Attempt: 1, Type: eventType, Info: truncateText(info, 4096), Artifacts: []string{lease.WorkspacePath, lease.ProposalArtifact}})
}

func validateWorkspaceRequest(request WorkspaceRequest) error {
	if strings.TrimSpace(request.ScopeRoot) == "" || strings.TrimSpace(request.BaseCommit) == "" {
		return errors.New("workspace scope and base commit are required")
	}
	if !safeGitRevision(request.BaseCommit) {
		return errors.New("workspace base commit is not a safe git revision")
	}
	if !safeWorkspaceComponent(request.TaskID) || !safeWorkspaceComponent(request.AttemptID) {
		return errors.New("workspace task and attempt IDs must be safe path components")
	}
	return nil
}

func safeGitRevision(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, "\x00\r\n \t")
}

func safeWorkspaceComponent(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 96 || strings.HasPrefix(value, ".") {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func absoluteExistingDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path must be a directory")
	}
	return filepath.Clean(real), nil
}

func absoluteManagedRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("managed root must be a real directory")
		}
		return filepath.EvalSymlinks(abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, err := absoluteExistingDir(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("managed root parent: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func ensureManagedRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create managed workspace root: %w", err)
	}
	marker := filepath.Join(root, ".bee-managed-workspaces")
	data, err := os.ReadFile(marker)
	if err == nil {
		if string(data) != "bee-workspaces-v1\n" {
			return errors.New("managed workspace root marker is invalid")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read managed workspace marker: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("managed workspace root exists without Bee marker")
	}
	if err := writePrivateFile(marker, []byte("bee-workspaces-v1\n")); err != nil {
		return fmt.Errorf("write managed workspace marker: %w", err)
	}
	return nil
}

func resolveArtifactPath(scopeRoot, artifactDir, taskID string) (string, error) {
	if strings.TrimSpace(artifactDir) == "" {
		artifactDir = filepath.Join("generated", "children", taskID)
	}
	candidate := artifactDir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(scopeRoot, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if !scopeWithin(scopeRoot, abs) || filepath.Clean(scopeRoot) == abs {
		return "", errors.New("artifact directory must remain within task scope and not equal its root")
	}
	return abs, nil
}

func parseWorktreePaths(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, filepath.Clean(strings.TrimSpace(strings.TrimPrefix(line, "worktree "))))
		}
	}
	return paths
}

func splitLines(value string) []string {
	var result []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, filepath.ToSlash(line))
		}
	}
	sort.Strings(result)
	return result
}

// boundedBuffer deliberately names its bytes.Buffer instead of embedding it.
// Embedding promotes bytes.Buffer.ReadFrom, which lets io.Copy bypass Write
// and defeat the capture limit.
type boundedBuffer struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = buffer.data.Write(data[:remaining])
		buffer.truncated = true
		return len(data), nil
	}
	return buffer.data.Write(data)
}

func (buffer *boundedBuffer) String() string {
	return buffer.data.String()
}

func (buffer *boundedBuffer) Len() int {
	return buffer.data.Len()
}

type gitResult struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

func runGit(ctx context.Context, dir string, args ...string) (gitResult, error) {
	return runGitWithLimit(ctx, dir, workspaceCommandLimit, args...)
}

func runGitWithLimit(ctx context.Context, dir string, limit int, args ...string) (gitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := exec.LookPath("git"); err != nil {
		return gitResult{}, fmt.Errorf("git unavailable: %w", err)
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	stdout := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: limit}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := gitResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated}
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		if message == "" {
			message = err.Error()
		}
		return result, fmt.Errorf("git %s: %s", strings.Join(args, " "), truncateText(message, 4096))
	}
	return result, nil
}

func runGitSimple(ctx context.Context, dir string, args ...string) error {
	_, err := runGit(ctx, dir, args...)
	return err
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func writeJSONPrivateFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return writePrivateFile(path, data)
}

func readJSONPrivateFile(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("metadata path is a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	limited := io.LimitReader(file, workspaceCommandLimit)
	return json.NewDecoder(limited).Decode(value)
}
