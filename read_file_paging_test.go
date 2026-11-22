package bee

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func readFileForTest(t *testing.T, scope ExecutionScope, args string) (string, error) {
	t.Helper()
	return ReadFileTool{Scope: scope}.Execute(context.Background(), []byte(args))
}

func numberedLines(count int) string {
	var builder strings.Builder
	for index := 1; index <= count; index++ {
		fmt.Fprintf(&builder, "line %03d\n", index)
	}
	return builder.String()
}

func TestReadFileSmallFileIsUnchanged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\nworld"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readFileForTest(t, ExecutionScope{Root: root}, `{"path":"a.txt"}`)
	if err != nil || got != "hello\nworld" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestReadFileOffsetAndLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "n.txt"), []byte(numberedLines(10)), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: root}
	got, err := readFileForTest(t, scope, `{"path":"n.txt","offset":3,"limit":2}`)
	if err != nil || got != "line 003\nline 004\n[read_file: lines 3-4 of 10; continue with offset=5]" {
		t.Fatalf("middle page = %q, %v", got, err)
	}
	got, err = readFileForTest(t, scope, `{"path":"n.txt","offset":9}`)
	if err != nil || got != "line 009\nline 010\n[read_file: lines 9-10 of 10]" {
		t.Fatalf("last page = %q, %v", got, err)
	}
	if _, err := readFileForTest(t, scope, `{"path":"n.txt","offset":11}`); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("offset past end err=%v", err)
	}
	if _, err := readFileForTest(t, scope, `{"path":"n.txt","lines":2}`); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

// Following the trailers from the first page must reproduce the file
// exactly, whole lines only, never more than the output cap per page.
func TestReadFileLargeFilePagesThroughToTheEnd(t *testing.T) {
	root := t.TempDir()
	content := numberedLines(400) + "last line without newline"
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: root, MaxOutputBytes: 1024}
	trailer := regexp.MustCompile(`\[read_file: lines (\d+)-(\d+) of (\d+)(?:; continue with offset=(\d+))?\]$`)
	var rebuilt strings.Builder
	args := `{"path":"big.txt"}`
	for page := 0; page < 100; page++ {
		got, err := readFileForTest(t, scope, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 1024 {
			t.Fatalf("page %d is %d bytes, over the cap", page, len(got))
		}
		match := trailer.FindStringSubmatch(got)
		if match == nil {
			t.Fatalf("page %d lacks a trailer: %q", page, got)
		}
		if match[3] != "401" {
			t.Fatalf("total lines = %s, want 401", match[3])
		}
		rebuilt.WriteString(got[:len(got)-len(match[0])])
		if match[4] == "" {
			if match[2] != "401" {
				t.Fatalf("final page ends at line %s", match[2])
			}
			break
		}
		next, _ := strconv.Atoi(match[4])
		args = fmt.Sprintf(`{"path":"big.txt","offset":%d}`, next)
	}
	// The file has no trailing newline; read_file adds one before the trailer.
	if got := rebuilt.String(); got != content && got != content+"\n" {
		t.Fatalf("pages do not reassemble the file: %d vs %d bytes", len(got), len(content))
	}
}

func TestReadFileCutsAnOversizedLineAndContinues(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("x", 5000) + "\nsecond\n"
	if err := os.WriteFile(filepath.Join(root, "min.js"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readFileForTest(t, ExecutionScope{Root: root, MaxOutputBytes: 1024}, `{"path":"min.js"}`)
	if err != nil || !strings.Contains(got, "…[line truncated]") || !strings.Contains(got, "continue with offset=2") || len(got) > 1024 {
		t.Fatalf("long line page = %q, %v", got, err)
	}
	got, err = readFileForTest(t, ExecutionScope{Root: root, MaxOutputBytes: 1024}, `{"path":"min.js","offset":2}`)
	if err != nil || !strings.HasPrefix(got, "second\n") {
		t.Fatalf("after long line = %q, %v", got, err)
	}
}
