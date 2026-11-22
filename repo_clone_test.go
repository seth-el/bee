package bee

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepoCloneUsesCanonicalProjectsRoot(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	beeHome := filepath.Join(root, ".bee")
	t.Setenv("BEE_HOME", beeHome)
	if _, err := EnsureBeeLayout(); err != nil {
		t.Fatal(err)
	}
	source, _ := newWorkspaceTestRepo(t)
	workspace := filepath.Join(beeHome, "workspace")
	scope := ExecutionScope{Root: workspace, Roots: []string{root}, WriteRoot: workspace, EnforceWriteRoots: true}
	output, err := (RepoCloneTool{Scope: scope}).Execute(context.Background(), mustJSON(map[string]any{"repository": source, "name": "cloned-repo"}))
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	var result repoCloneResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workspace, "projects", "cloned-repo")
	if result.Destination != want {
		t.Fatalf("destination=%q want=%q", result.Destination, want)
	}
	if info, err := os.Stat(filepath.Join(want, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("clone metadata info=%#v err=%v", info, err)
	}
}

func TestRepoCloneRejectsArbitraryNamesAndEmbeddedCredentials(t *testing.T) {
	tool := RepoCloneTool{Scope: ExecutionScope{Root: t.TempDir()}}
	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{"repository": "https://example.com/repo.git", "name": "../escape"})); err == nil {
		t.Fatal("unsafe clone name accepted")
	}
	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{"repository": "https://token@example.com/repo.git", "name": "repo"})); err == nil {
		t.Fatal("credential-bearing clone URL accepted")
	}
}
