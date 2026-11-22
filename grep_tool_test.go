package bee

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func searchFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	other := filepath.Join(base, "project")
	files := map[string]string{
		filepath.Join(root, "main.go"):                   "package main\n\nfunc fooTool() {}\nfunc barTool() {}\ncall(foo(\n",
		filepath.Join(root, "notes.md"):                  "Foo in prose\n",
		filepath.Join(root, ".git", "objects", "x"):      "func fooTool() {}\n",
		filepath.Join(root, "node_modules", "m", "a.js"): "fooTool()\n",
		filepath.Join(other, "lib.go"):                   "func fooTool() {}\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte("fooTool\x00\x01"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, other
}

func grepCall(t *testing.T, scope ExecutionScope, args map[string]any) (string, error) {
	t.Helper()
	data, _ := json.Marshal(args)
	return GrepTool{Scope: scope}.Execute(context.Background(), data)
}

func TestGrepLiteralRegexCaseAndGlob(t *testing.T) {
	root, _ := searchFixture(t)
	scope := ExecutionScope{Root: root}
	got, err := grepCall(t, scope, map[string]any{"query": "foo("})
	if err != nil || got != "main.go:5:call(foo(" {
		t.Fatalf("literal default = %q, %v", got, err)
	}
	got, err = grepCall(t, scope, map[string]any{"query": `func \w+Tool`, "regex": true})
	if err != nil || got != "main.go:3:func fooTool() {}\nmain.go:4:func barTool() {}" {
		t.Fatalf("regex = %q, %v", got, err)
	}
	if _, err := grepCall(t, scope, map[string]any{"query": "foo(", "regex": true}); err == nil || !strings.Contains(err.Error(), "literal") {
		t.Fatalf("invalid regex err=%v", err)
	}
	got, err = grepCall(t, scope, map[string]any{"query": "foo", "ignore_case": true, "glob": "*.md"})
	if err != nil || got != "notes.md:1:Foo in prose" {
		t.Fatalf("ignore_case+glob = %q, %v", got, err)
	}
	got, err = grepCall(t, scope, map[string]any{"query": "FOO", "ignore_case": true, "regex": true, "glob": "*.md"})
	if err != nil || got != "notes.md:1:Foo in prose" {
		t.Fatalf("regex ignore_case = %q, %v", got, err)
	}
}

func TestGrepSkipsNoiseBinaryAndUnreadable(t *testing.T) {
	root, _ := searchFixture(t)
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.go"), []byte("fooTool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	got, err := grepCall(t, ExecutionScope{Root: root}, map[string]any{"query": "fooTool"})
	if err != nil {
		t.Fatalf("an unreadable directory aborted the search: %v", err)
	}
	for _, unwanted := range []string{".git", "node_modules", "blob.bin"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("searched %s: %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "main.go:3:") {
		t.Fatalf("missing real match: %q", got)
	}
	// An explicit path inside .git is still searchable.
	if got, err := grepCall(t, ExecutionScope{Root: root}, map[string]any{"query": "fooTool", "path": ".git"}); err != nil || !strings.Contains(got, "objects/x") {
		t.Fatalf("explicit .git search = %q, %v", got, err)
	}
}

func TestGrepContextAndCap(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for index := 1; index <= 30; index++ {
		text := fmt.Sprintf("line %d", index)
		if index == 5 || index == 6 || index == 20 {
			text += " hit"
		}
		lines = append(lines, text)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: root}
	got, err := grepCall(t, scope, map[string]any{"query": "hit", "context": 1})
	want := "f.txt-4-line 4\nf.txt:5:line 5 hit\nf.txt:6:line 6 hit\nf.txt-7-line 7\n--\nf.txt-19-line 19\nf.txt:20:line 20 hit\nf.txt-21-line 21"
	if err != nil || got != want {
		t.Fatalf("context =\n%s\nwant\n%s\n(%v)", got, want, err)
	}
	got, err = grepCall(t, scope, map[string]any{"query": "hit", "max_results": 2})
	if err != nil || !strings.HasPrefix(got, "f.txt:5:line 5 hit\nf.txt:6:line 6 hit\n[grep: showing the first 2 matches") {
		t.Fatalf("capped = %q, %v", got, err)
	}
	got, err = grepCall(t, scope, map[string]any{"query": "hit", "max_results": 3})
	if err != nil || strings.Contains(got, "[grep:") {
		t.Fatalf("exactly-at-cap search reported more: %q, %v", got, err)
	}
}

func TestSearchPathsAreRelativeInsideTheRootAndAbsoluteOutside(t *testing.T) {
	root, other := searchFixture(t)
	scope := ExecutionScope{Root: root, Roots: []string{other}}
	got, err := grepCall(t, scope, map[string]any{"query": "fooTool", "path": other})
	if err != nil || got != filepath.ToSlash(filepath.Join(other, "lib.go"))+":1:func fooTool() {}" {
		t.Fatalf("outside-root path = %q, %v", got, err)
	}
	found, err := FindFilesTool{Scope: scope}.Execute(context.Background(), json.RawMessage(`{"path":`+strconvQuote(other)+`,"pattern":"*.go"}`))
	if err != nil || found != filepath.ToSlash(filepath.Join(other, "lib.go")) {
		t.Fatalf("find_files outside root = %q, %v", found, err)
	}
	if strings.Contains(got+found, "../") {
		t.Fatal("search output used ../ paths")
	}
}

func TestFindFilesSkipsNoiseAndRejectsBadGlob(t *testing.T) {
	root, _ := searchFixture(t)
	tool := FindFilesTool{Scope: ExecutionScope{Root: root}}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*"}`))
	if err != nil || strings.Contains(got, ".git") || strings.Contains(got, "node_modules") || !strings.Contains(got, "main.go") {
		t.Fatalf("find_files = %q, %v", got, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"[bad"}`)); err == nil || !strings.Contains(err.Error(), "not a valid glob") {
		t.Fatalf("bad glob err=%v", err)
	}
}
