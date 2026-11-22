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

func TestReconToolsBoundObservationsAndDropBinaryLines(t *testing.T) {
	root := t.TempDir()
	var content strings.Builder
	for i := 0; i < maxReconGrepMatches+40; i++ {
		fmt.Fprintf(&content, "needle line %03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte("needle\x00binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := ReconGrepTool{Scope: ExecutionScope{Root: root}}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(output, '\x00') || len(output) > maxReconToolBytes {
		t.Fatalf("unbounded or binary recon output: bytes=%d", len(output))
	}
	if lines := strings.Count(output, "\n") + 1; lines > maxReconGrepMatches+1 {
		t.Fatalf("recon grep lines=%d", lines)
	}
	if !strings.Contains(output, "recon output bounded") {
		t.Fatalf("missing bounded marker: %q", output)
	}
}
