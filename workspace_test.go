package bee

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestGitWorkspaceManagerLeaseInspectProposalPreserveAndCleanup(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	managed := filepath.Join(filepath.Dir(repo), "bee-managed")
	manager, err := NewGitWorkspaceManager(repo, managed, &MemoryTraceSink{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "task-a", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !scopeWithin(filepath.Join(managed, "worktrees"), lease.WorkspacePath) {
		t.Fatalf("workspace escaped managed root: %s", lease.WorkspacePath)
	}
	if filepath.Clean(lease.WorkspacePath) == filepath.Clean(repo) {
		t.Fatal("workspace reused main repository")
	}
	if err := os.WriteFile(filepath.Join(lease.WorkspacePath, "changed.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.Inspect(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(inspection.UntrackedFiles, "changed.txt") {
		t.Fatalf("untracked files = %#v", inspection.UntrackedFiles)
	}
	upstream := fingerprintValue("test-upstream", "completion")
	reference := &CompletionManifestReference{
		Path: CompletionManifestFilename, Fingerprint: upstream,
		TaskFingerprint: fingerprintValue("test", "task"), HarnessFingerprint: fingerprintValue("test", "harness"),
		CapabilityFingerprint: fingerprintValue("test", "capabilities"), ProviderFingerprint: fingerprintValue("test", "provider"),
		Provider: CompletionProviderIdentity{Type: "test", Model: "test-model"},
	}
	proposal, err := manager.CollectProposal(context.Background(), lease, ChildCompletionPacket{TaskID: "task-a", ParentID: "parent", Status: ChildSucceeded, Summary: "done", Tests: []string{"go test ./..."}, Evidence: []string{"evidence"}, Manifest: reference})
	if err != nil {
		t.Fatal(err)
	}
	if !proposal.ReadyForReview || proposal.DiffArtifact == "" || proposal.ProposalArtifact == "" {
		t.Fatalf("proposal = %#v", proposal)
	}
	if _, err := os.Stat(proposal.DiffArtifact); err != nil {
		t.Fatal(err)
	}
	if proposal.CompletionManifest == nil {
		t.Fatal("integration completion manifest is missing")
	}
	manifest, err := ReadCompletionManifest(proposal.CompletionManifest.Path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != CompletionManifestIsolated || manifest.BaseCommit != base || manifest.AttemptID != lease.AttemptID || manifest.UpstreamFingerprint != upstream || manifest.Provider.Model != "test-model" {
		t.Fatalf("integration manifest = %#v", manifest)
	}
	if err := manager.Preserve(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.WorkspacePath); err != nil {
		t.Fatalf("preserved workspace missing: %v", err)
	}
	if err := manager.Cleanup(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.WorkspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace after cleanup: %v", err)
	}
	mainData, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mainData) != "base\n" {
		t.Fatalf("main worktree changed: %q", mainData)
	}
}

func TestGitWorkspaceManagerFailsClosedForDirtyRepoUnsafeIDsAndNonRepoScope(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	managed := filepath.Join(filepath.Dir(repo), "bee-managed")
	manager, err := NewGitWorkspaceManager(repo, managed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "task-a", AttemptID: "attempt-1"}); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty repo error = %v", err)
	}
	_ = os.Remove(filepath.Join(repo, "dirty.txt"))
	if _, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "../escape", AttemptID: "attempt-1"}); err == nil || !strings.Contains(err.Error(), "safe") {
		t.Fatalf("unsafe id error = %v", err)
	}
	nonRepo := t.TempDir()
	if _, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: nonRepo, BaseCommit: base, TaskID: "task-a", AttemptID: "attempt-1"}); err == nil || (!strings.Contains(err.Error(), "git") && !strings.Contains(err.Error(), "outside repository")) {
		t.Fatalf("non-repo scope error = %v", err)
	}
}

func TestGitWorkspaceManagerRejectsAmbiguousRootAndStaleLeaseMetadata(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	managed := filepath.Join(filepath.Dir(repo), "bee-managed")
	manager, err := NewGitWorkspaceManager(repo, managed, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "task-a", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(lease.WorkspacePath); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "task-b", AttemptID: "attempt-1"}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale metadata error = %v", err)
	}
}

func TestGitWorkspaceManagerAllowsIgnoredManagedRootInsideRepository(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".bee-workspaces/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "--", ".gitignore")
	gitTestCommand(t, repo, "commit", "--quiet", "-m", "ignore Bee workspaces")
	manager, err := NewGitWorkspaceManager(repo, filepath.Join(repo, ".bee-workspaces"), nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), WorkspaceRequest{RepositoryRoot: repo, ScopeRoot: repo, BaseCommit: base, TaskID: "task-a", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !scopeWithin(repo, lease.WorkspacePath) {
		t.Fatalf("workspace not under repository: %s", lease.WorkspacePath)
	}
	if status := strings.TrimSpace(gitTestOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all")); status != "" {
		t.Fatalf("ignored managed root dirtied parent: %q", status)
	}
	if err := manager.Cleanup(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeOrchestratorRoutesMutableTaskAndPreservesFailedWorkspace(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	managed := filepath.Join(filepath.Dir(repo), "bee-managed")
	manager, err := NewGitWorkspaceManager(repo, managed, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := &workspaceFakeExecutor{fail: true}
	orchestrator, err := NewWorktreeOrchestrator(fake, manager, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := ChildTask{TaskID: "mutable", ParentID: "parent", Objective: "change", Prompt: "change", Mutability: TaskMutable, Scope: ExecutionScope{Root: repo}, ArtifactDir: "generated/children/mutable", AcceptanceCriteria: []string{"tests"}}
	plan := OrchestrationPlan{OrchestrationID: "orch", ParentID: "parent", Tasks: []PlannedChild{{Task: task}}, MaxConcurrent: 2}
	result, runErr := orchestrator.Run(context.Background(), plan)
	if result.Status != OrchestrationFailed {
		t.Fatalf("result=%#v err=%v", result, runErr)
	}
	if len(fake.scopes) != 1 || filepath.Clean(fake.scopes[0]) == filepath.Clean(repo) || !scopeWithin(filepath.Join(managed, "worktrees"), fake.scopes[0]) {
		t.Fatalf("mutable task scopes = %#v", fake.scopes)
	}
	matches, err := filepath.Glob(filepath.Join(managed, "worktrees", "mutable", "mutable-attempt-1"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("preserved worktree matches=%#v err=%v", matches, err)
	}
	mainData, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mainData) != "base\n" {
		t.Fatalf("main worktree mutated: %q", mainData)
	}
}

func TestWorktreeOrchestratorIsolatesParallelMutableTasks(t *testing.T) {
	repo, base := newWorkspaceTestRepo(t)
	managed := filepath.Join(filepath.Dir(repo), "bee-managed")
	manager, err := NewGitWorkspaceManager(repo, managed, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := &workspaceFakeExecutor{}
	orchestrator, err := NewWorktreeOrchestrator(fake, manager, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := ChildTask{TaskID: "mutable-a", ParentID: "parent", Objective: "change", Prompt: "change", Mutability: TaskMutable, Scope: ExecutionScope{Root: repo}, ArtifactDir: "generated/children/mutable-a", AcceptanceCriteria: []string{"tests"}}
	second := first
	second.TaskID = "mutable-b"
	second.ArtifactDir = "generated/children/mutable-b"
	plan := OrchestrationPlan{OrchestrationID: "orch", ParentID: "parent", Tasks: []PlannedChild{{Task: first}, {Task: second}}, MaxConcurrent: 2}
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil || result.Status != OrchestrationSucceeded {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fake.scopes) != 2 || fake.scopes[0] == fake.scopes[1] {
		t.Fatalf("mutable workspace scopes = %#v", fake.scopes)
	}
	for _, scope := range fake.scopes {
		if !scopeWithin(filepath.Join(managed, "worktrees"), scope) {
			t.Fatalf("scope escaped managed worktree root: %s", scope)
		}
	}
}

type workspaceFakeExecutor struct {
	mu     sync.Mutex
	scopes []string
	fail   bool
}

func (fake *workspaceFakeExecutor) Run(_ context.Context, task ChildTask) (ChildCompletionPacket, error) {
	fake.mu.Lock()
	fake.scopes = append(fake.scopes, task.Scope.Root)
	fake.mu.Unlock()
	if err := os.WriteFile(filepath.Join(task.Scope.Root, "child-change.txt"), []byte("child\n"), 0o600); err != nil {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildFailed, Summary: "write failed", Blockers: []string{err.Error()}}, err
	}
	if fake.fail {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildFailed, Summary: "failed", Blockers: []string{"fake failure"}}, errors.New("fake failure")
	}
	return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildSucceeded, Summary: "ok"}, nil
}

func newWorkspaceTestRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "init", "--quiet")
	gitTestCommand(t, repo, "config", "user.email", "bee@example.test")
	gitTestCommand(t, repo, "config", "user.name", "Bee Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "--", "README.md")
	gitTestCommand(t, repo, "commit", "--quiet", "-m", "base")
	return repo, strings.TrimSpace(gitTestOutput(t, repo, "rev-parse", "HEAD"))
}

func gitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
