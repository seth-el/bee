package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newGitDiffTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := runGitSimple(context.Background(), root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "user.email", "bee-tests@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "config", "user.name", "Bee Tests"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "commit", "-qm", "initial"); err != nil {
		t.Fatal(err)
	}
	return root
}

func executeGitDiff(t *testing.T, root string, args string) map[string]any {
	t.Helper()
	tool := GitDiffTool{Scope: ExecutionScope{Root: root}}
	output, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if len(output) > maxGitReviewBytes {
		t.Fatalf("output length=%d exceeds bound %d", len(output), maxGitReviewBytes)
	}
	if !json.Valid([]byte(output)) {
		t.Fatalf("invalid JSON: %q", output)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"repository", "ref", "path", "has_changes", "diff", "truncated", "note"}
	gotKeys := make([]string, 0, len(decoded))
	for key := range decoded {
		gotKeys = append(gotKeys, key)
	}
	if !reflect.DeepEqual(sortedStrings(gotKeys), sortedStrings(wantKeys)) {
		t.Fatalf("keys=%v, want=%v", gotKeys, wantKeys)
	}
	for _, key := range []string{"repository", "ref", "path", "diff", "note"} {
		if _, ok := decoded[key].(string); !ok {
			t.Fatalf("%s=%#v is not a string", key, decoded[key])
		}
	}
	for _, key := range []string{"has_changes", "truncated"} {
		if _, ok := decoded[key].(bool); !ok {
			t.Fatalf("%s=%#v is not a bool", key, decoded[key])
		}
	}
	return decoded
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j] < result[i] {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

func TestGitDiffCanonicalEnvelopeCleanAndEmptyPath(t *testing.T) {
	root := newGitDiffTestRepo(t)
	clean := executeGitDiff(t, root, `{}`)
	if clean["ref"] != "HEAD" || clean["path"] != "" || clean["has_changes"] != false || clean["diff"] != "" || clean["truncated"] != false || clean["note"] != "" {
		t.Fatalf("clean result=%#v", clean)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filtered := executeGitDiff(t, root, `{"path":"README.md"}`)
	if filtered["path"] != "README.md" || filtered["has_changes"] != false || filtered["diff"] != "" || filtered["note"] != "" {
		t.Fatalf("filtered result=%#v", filtered)
	}
}

func TestGitDiffCanonicalEnvelopeIdenticalRefAndChanges(t *testing.T) {
	root := newGitDiffTestRepo(t)
	identical := executeGitDiff(t, root, `{"ref":"HEAD"}`)
	if identical["has_changes"] != false || identical["diff"] != "" {
		t.Fatalf("identical result=%#v", identical)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unstaged := executeGitDiff(t, root, `{}`)
	if unstaged["has_changes"] != true || !strings.Contains(unstaged["diff"].(string), "unstaged") || unstaged["note"] != "" {
		t.Fatalf("unstaged result=%#v", unstaged)
	}
	if err := runGitSimple(context.Background(), root, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	stagedOnly := executeGitDiff(t, root, `{}`)
	if stagedOnly["has_changes"] != true || !strings.Contains(stagedOnly["diff"].(string), "unstaged") {
		t.Fatalf("staged-only result=%#v", stagedOnly)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("staged and worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stagedAndUnstaged := executeGitDiff(t, root, `{}`)
	if stagedAndUnstaged["has_changes"] != true || !strings.Contains(stagedAndUnstaged["diff"].(string), "staged and worktree") {
		t.Fatalf("staged+unstaged result=%#v", stagedAndUnstaged)
	}
}

func TestGitDiffCanonicalEnvelopeMultiFileCommittedRefAndTruncation(t *testing.T) {
	root := newGitDiffTestRepo(t)
	for name, contents := range map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runGitSimple(context.Background(), root, "add", "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	multi := executeGitDiff(t, root, `{}`)
	if multi["has_changes"] != true || !strings.Contains(multi["diff"].(string), "a.txt") || !strings.Contains(multi["diff"].(string), "b.txt") {
		t.Fatalf("multi-file result=%#v", multi)
	}
	if err := runGitSimple(context.Background(), root, "commit", "-qm", "second"); err != nil {
		t.Fatal(err)
	}
	committed := executeGitDiff(t, root, `{"ref":"HEAD~1"}`)
	if committed["has_changes"] != true || !strings.Contains(committed["diff"].(string), "a.txt") {
		t.Fatalf("committed result=%#v", committed)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(strings.Repeat("long change\n", 10000)), 0o600); err != nil {
		t.Fatal(err)
	}
	truncated := executeGitDiff(t, root, `{"max_bytes":64}`)
	if truncated["has_changes"] != true || truncated["truncated"] != true {
		t.Fatalf("truncated result=%#v", truncated)
	}
	note := truncated["note"].(string)
	if !strings.Contains(note, "capture limit") || !strings.Contains(note, "returned prefix may be shorter") || !strings.Contains(note, "do not repeat") || !strings.Contains(note, "increase max_bytes") || !strings.Contains(note, "narrow path") || !strings.Contains(note, "partial diff") || !strings.Contains(note, "32768") {
		t.Fatalf("below-max truncation note=%q", note)
	}
}

func TestGitDiffTruncationNoteAtMaximumDoesNotSuggestIncreasing(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(strings.Repeat("maximum change\n", 20000)), 0o600); err != nil {
		t.Fatal(err)
	}
	truncated := executeGitDiff(t, root, `{"max_bytes":32768}`)
	if truncated["truncated"] != true {
		t.Fatalf("truncated result=%#v", truncated)
	}
	note := truncated["note"].(string)
	if !strings.Contains(note, "maximum") || !strings.Contains(note, "capture limit") || !strings.Contains(note, "returned prefix may be shorter") || !strings.Contains(note, "do not repeat") || !strings.Contains(note, "narrow path") || !strings.Contains(note, "partial diff") || strings.Contains(note, "increase max_bytes") {
		t.Fatalf("maximum truncation note=%q", note)
	}
}
