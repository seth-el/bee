package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedGitStageSupportsDeletionSymlinkAndExecutable(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\necho safe\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(root, "run-link")); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(gitStageArgs{Paths: []string{"README.md", "run.sh", "run-link"}})
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	result, err := runGit(context.Background(), root, "diff", "--cached", "--summary", "--")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Stdout, "delete mode 100644 README.md") || !strings.Contains(result.Stdout, "create mode 100755 run.sh") || !strings.Contains(result.Stdout, "create mode 120000 run-link") {
		t.Fatalf("summary=%q", result.Stdout)
	}
}

func TestBoundedGitStageIsAllOrNothing(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "good.txt"), []byte("good\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := runGit(context.Background(), root, "diff", "--cached", "--binary", "--")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(gitStageArgs{Paths: []string{"good.txt", "missing-directory"}})
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
		t.Fatal("invalid exact path list was accepted")
	}
	after, err := runGit(context.Background(), root, "diff", "--cached", "--binary", "--")
	if err != nil {
		t.Fatal(err)
	}
	if before.Stdout != after.Stdout {
		t.Fatalf("partial stage occurred: before=%q after=%q", before.Stdout, after.Stdout)
	}
}

func TestBoundedGitStageRejectsIgnoredAndNonCanonicalPaths(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"ignored.txt", "./README.md", "README.md/../README.md", ".git/config"} {
		args, _ := json.Marshal(gitStageArgs{Paths: []string{raw}})
		if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
			t.Fatalf("unsafe path accepted: %q", raw)
		}
	}
}

func TestBoundedGitRejectsDetachedAndMergeState(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := runGitSimple(context.Background(), root, "checkout", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err == nil {
		t.Fatal("detached HEAD accepted")
	}
	if err := runGitSimple(context.Background(), root, "switch", "-c", "safe-branch"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "MERGE_HEAD"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(root, ".git", "MERGE_HEAD"))
	if _, err := (GitStagedReviewTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("merge state accepted")
	}
}

func TestBoundedGitCommitRefusesMovedHead(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("reviewed\n"), 0o600); err != nil {
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
	if err := runGitSimple(context.Background(), root, "commit", "-qm", "external head move"); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(gitCommitArgs{Message: "must refuse moved head", ReviewHash: review.ReviewHash})
	if _, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
		t.Fatal("moved HEAD accepted")
	}
}

func TestBoundedGitDoesNotRunHooksFiltersOrDiffHelpers(t *testing.T) {
	root := newGitDiffTestRepo(t)
	marker := filepath.Join(root, "marker")
	script := filepath.Join(root, "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > "+marker+"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "core.hooksPath", filepath.Join(root, "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hooks", "pre-commit"), []byte("#!/bin/sh\nprintf hook > "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("README.md filter=evil diff=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "filter.evil.clean", script); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "filter.evil.process", script); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "diff.evil.textconv", script); err != nil {
		t.Fatal(err)
	}
	for _, item := range [][2]string{{"commit.gpgSign", "true"}, {"gpg.program", script}, {"core.editor", script}, {"sequence.editor", script}, {"credential.helper", "!" + script}, {"index.version", "4"}, {"core.splitIndex", "true"}} {
		if err := runGitSimple(context.Background(), root, "config", item[0], item[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("safe\n"), 0o600); err != nil {
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
	args, _ := json.Marshal(gitCommitArgs{Message: "safe commit", ReviewHash: review.ReviewHash})
	if _, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malicious helper/hook ran: %v", err)
	}
}

func TestBoundedGitRealIndexLockBlocksStageAndCommit(t *testing.T) {
	root := newGitDiffTestRepo(t)
	indexPath := filepath.Join(root, ".git", "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("locked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := indexPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lockFile.Close()
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err == nil {
		t.Fatal("stage accepted an existing real Git index lock")
	}
	if got, err := os.ReadFile(indexPath); err != nil || string(got) != string(beforeIndex) {
		t.Fatalf("index mutated while lock held: err=%v", err)
	}
	if err := os.Remove(lockPath); err != nil {
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
	lockFile, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lockFile.Close()
	args, _ := json.Marshal(gitCommitArgs{Message: "must block", ReviewHash: review.ReviewHash})
	if _, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
		t.Fatal("commit accepted an existing real Git index lock")
	}
	if got, err := runGit(context.Background(), root, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(got.Stdout) != review.Head {
		t.Fatalf("HEAD moved while lock held: %q err=%v", got.Stdout, err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("foreign lock was removed: %v", err)
	}
}

func TestBoundedGitRejectsExternalGitMetadata(t *testing.T) {
	root := newGitDiffTestRepo(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n\tworktree = "+outside+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err == nil {
		t.Fatal("external core.worktree was accepted")
	}
}

func TestBoundedGitRequiresRepositoryLocalIdentity(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := runGitSimple(context.Background(), root, "config", "--local", "--unset", "user.name"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("identity\n"), 0o600); err != nil {
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
	args, _ := json.Marshal(gitCommitArgs{Message: "must require local identity", ReviewHash: review.ReviewHash})
	if _, err := (GitCommitTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), args); err == nil {
		t.Fatal("commit silently consumed non-local identity")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("failed commit left its owned index lock behind: %v", err)
	}
}

func TestBoundedGitOwnedIndexLockBlocksOrdinaryGitAndCleansUp(t *testing.T) {
	root := newGitDiffTestRepo(t)
	indexPath := filepath.Join(root, ".git", "index")
	lock, err := acquireGitIndexLock(context.Background(), indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("external writer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "add", "README.md"); err == nil {
		t.Fatal("ordinary git add ignored Bee's real index lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("owned index lock was not cleaned up: %v", err)
	}
}

func TestBoundedGitRejectsTrackedGitlinkDeletion(t *testing.T) {
	root := newGitDiffTestRepo(t)
	head, err := runGit(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "update-index", "--add", "--cacheinfo", "160000", strings.TrimSpace(head.Stdout), "nested"); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["nested"]}`)); err == nil {
		t.Fatal("tracked gitlink deletion was accepted")
	}
}

func TestBoundedGitRejectsDotGitFileOutsideRepository(t *testing.T) {
	root := newGitDiffTestRepo(t)
	gitDir := filepath.Join(root, ".git")
	outside := filepath.Join(t.TempDir(), "metadata")
	if err := os.Rename(gitDir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+outside+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err == nil {
		t.Fatal("external .git file metadata was accepted")
	}
}

func TestBoundedGitRejectsSymlinkedRefMetadata(t *testing.T) {
	root := newGitDiffTestRepo(t)
	branch, err := runGit(context.Background(), root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	refPath := filepath.Join(root, ".git", "refs", "heads", strings.TrimSpace(branch.Stdout))
	target := filepath.Join(root, ".git", "refs", "heads", "target")
	data, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(refPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", refPath); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStageTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"paths":["README.md"]}`)); err == nil {
		t.Fatal("symlinked branch metadata was accepted")
	}
}
