package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedGitStageReviewCommitExactPaths(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("updated\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageOutput, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stageOutput, "other.txt") {
		t.Fatalf("stage output leaked unrequested path: %s", stageOutput)
	}
	var staged gitStageResult
	if err := json.Unmarshal([]byte(stageOutput), &staged); err != nil {
		t.Fatal(err)
	}
	if staged.PathCount != 1 || len(staged.Paths) != 1 || staged.Paths[0] != "README.md" {
		t.Fatalf("stage=%#v", staged)
	}
	reviewOutput, err := (GitStagedReviewTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var review gitStagedReviewResult
	if err := json.Unmarshal([]byte(reviewOutput), &review); err != nil {
		t.Fatal(err)
	}
	if review.ReviewHash == "" || review.PathCount != 1 || review.Paths[0] != "README.md" || !strings.Contains(review.Diff, "updated") {
		t.Fatalf("review=%#v", review)
	}
	commitArgs, _ := json.Marshal(gitCommitArgs{Message: "bounded update", ReviewHash: review.ReviewHash})
	commitOutput, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), commitArgs)
	if err != nil {
		t.Fatal(err)
	}
	var committed gitCommitResult
	if err := json.Unmarshal([]byte(commitOutput), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.OldHead != review.Head || committed.NewHead == "" || committed.PathCount != 1 {
		t.Fatalf("commit=%#v", committed)
	}
	if got, err := runGit(context.Background(), root, "show", "--format=%s", "--no-patch", "HEAD"); err != nil || strings.TrimSpace(got.Stdout) != "bounded update" {
		t.Fatalf("show=%q err=%v", got.Stdout, err)
	}
	if got, err := runGit(context.Background(), root, "status", "--porcelain"); err != nil || !strings.Contains(got.Stdout, "?? other.txt") {
		t.Fatalf("status=%q err=%v", got.Stdout, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("successful commit left its index lock behind: %v", err)
	}
}

func TestBoundedGitCommitRefusesStaleReview(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("one staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err != nil {
		t.Fatal(err)
	}
	reviewOutput, err := (GitStagedReviewTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var review gitStagedReviewResult
	if err := json.Unmarshal([]byte(reviewOutput), &review); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("index changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["other.txt"]}`)); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(gitCommitArgs{Message: "must refuse", ReviewHash: review.ReviewHash})
	if _, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
		t.Fatal("stale staged review was accepted")
	}
}

func TestBoundedGitCompositionBoundaries(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	// The git tools are pruned from the foreground assistant (git goes
	// through exec); their bounded behavior is tested on the tools directly.
	for _, name := range []string{"git_status", "git_diff", "git_staged_review", "git_stage", "git_commit"} {
		if _, ok := trusted.Executor.Registry.Lookup(name); ok {
			t.Fatalf("trusted still exposes %s", name)
		}
	}
	locked, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, MemoryRoot: filepath.Join(root, "locked-memory"), Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Executor != nil {
		t.Fatal("locked composition has executor")
	}
	prompted, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, MemoryRoot: filepath.Join(root, "prompted-memory"), Policy: AssistantPolicyPrompted})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git_staged_review", "git_stage", "git_commit"} {
		if _, ok := prompted.Executor.Registry.Lookup(name); ok {
			t.Fatalf("prompted still exposes %s", name)
		}
	}
}

func TestBoundedGitPolicyAndPlanSemantics(t *testing.T) {
	stage := (GitStageTool{}).Definition()
	commit := (GitCommitTool{}).Definition()
	review := (GitStagedReviewTool{}).Definition()
	if decision, err := (GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}).Evaluate(context.Background(), stage, nil, ExecutionScope{TrustedForeground: true}); err != nil || decision != PolicyAllow {
		t.Fatalf("trusted stage decision=%v err=%v", decision, err)
	}
	if decision, err := (GeneralAssistantPolicy{Mode: AssistantPolicyPrompted}).Evaluate(context.Background(), commit, nil, ExecutionScope{}); err != nil || decision != PolicyRequireApproval {
		t.Fatalf("prompted commit decision=%v err=%v", decision, err)
	}
	if decision, err := (GeneralAssistantPolicy{Mode: AssistantPolicyLocked}).Evaluate(context.Background(), review, nil, ExecutionScope{}); err != nil || decision != PolicyDeny {
		t.Fatalf("locked review decision=%v err=%v", decision, err)
	}
}

func TestBoundedGitChildProjectionOmitsMutationTools(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git_staged_review", "git_stage", "git_commit"} {
		_, err := copyChildExecutor(app.Executor, ChildTask{TaskID: "git-child-" + name, ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{name}, Scope: app.Executor.Scope, Budget: ChildBudget{}})
		if err == nil {
			t.Fatalf("child projection unexpectedly accepted %s", name)
		}
	}
}
