package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editFixture(t *testing.T, content string) (EditFileTool, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return EditFileTool{Scope: ExecutionScope{Root: root, WriteRoot: root}}, path
}

func editCall(tool EditFileTool, args map[string]any) (string, error) {
	data, _ := json.Marshal(args)
	return tool.Execute(context.Background(), data)
}

func TestEditFileReplacesExactlyOnceOrAll(t *testing.T) {
	tool, path := editFixture(t, "a := 1\nb := 1\nc := 2\n")
	if _, err := editCall(tool, map[string]any{"path": "main.go", "old_text": "c := 2", "new_text": "c := 3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := editCall(tool, map[string]any{"path": "main.go", "old_text": ":= 1", "new_text": ":= 9", "replace_all": true}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "a := 9\nb := 9\nc := 3\n" {
		t.Fatalf("content=%q", data)
	}
	if _, err := editCall(tool, map[string]any{"path": "main.go", "old_text": "x"}); err == nil {
		t.Fatal("missing new_text accepted")
	}
	if _, err := editCall(tool, map[string]any{"path": "missing.go", "old_text": "x", "new_text": "y"}); err == nil {
		t.Fatal("edit of a missing file accepted")
	}
}

func TestEditFileMatchErrorsSayWhereAndWhy(t *testing.T) {
	tool, path := editFixture(t, "func main() {\n\tfmt.Println(\"hi\")\n\tfmt.Println(\"hi\")\n\treturn\n}\n")
	before, _ := os.ReadFile(path)
	for name, test := range map[string]struct {
		old  string
		want []string
	}{
		"several matches":  {"fmt.Println(\"hi\")", []string{"matches 2 places", "lines 2, 3", "replace_all"}},
		"whitespace only":  {"func main() {\n    fmt.Println(\"hi\")\n    fmt.Println(\"hi\")\n    return", []string{"whitespace is ignored", "near line 1"}},
		"first line found": {"\treturn\n\tos.Exit(1)", []string{"first line appears at line 4", "offset=2"}},
		"not found":        {"nothing like this", []string{"not found in the file", "read_file"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := editCall(tool, map[string]any{"path": "main.go", "old_text": test.old, "new_text": "x"})
			if err == nil {
				t.Fatal("edit unexpectedly succeeded")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
		})
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a failed edit changed the file")
	}
}

func TestWriteFileStillAcceptsLegacyEditAtRuntime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "n.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := WriteFileTool{Scope: ExecutionScope{Root: root, WriteRoot: root}}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"n.txt","operation":"edit","old_text":"one","new_text":"two"}`)); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "two" {
		t.Fatalf("content=%q", data)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"n.txt","content":"x"}`)); err == nil || !strings.Contains(err.Error(), "edit_file") {
		t.Fatalf("create over an existing file should point at replace/edit_file: %v", err)
	}
}

// edit_file goes through the same approval and session-grant path as
// write_file for writes outside the write roots.
func TestEditFileOutsideWriteRootsUsesApprovalAndSessionGrant(t *testing.T) {
	fixture := newGrantFixture(t, ApprovalApprovedForSession)
	target := filepath.Join(fixture.project, "src", "x.go")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionWriteGrants()
	ctx := grantContext(store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.app.Executor.Execute(ctx, "edit_file", json.RawMessage(`{"path":`+strconvQuote(target)+`,"old_text":"old","new_text":"new"}`)); err != nil {
		t.Fatal(err)
	}
	if fixture.approval.count() != 1 || fixture.approval.requests[0].SessionGrant != fixture.project {
		t.Fatalf("approval=%#v", fixture.approval.requests)
	}
	if data, _ := os.ReadFile(target); string(data) != "new" {
		t.Fatalf("content=%q", data)
	}
	listed, err := fixture.app.Executor.Execute(ctx, "mutation_list", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(listed.Output, "edit_file") {
		t.Fatalf("journal does not record edit_file: %q %v", listed.Output, err)
	}
}
