package bee

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDiagnosticNormalizationRedactsAndBounds(t *testing.T) {
	d, err := NormalizeDiagnostic(Diagnostic{Component: "scheduler", Operation: "run_due", Fields: map[string]string{"job_id": "job-1", "token": "secret-value", "path": "/private/file", "note": strings.Repeat("x", MaxDiagnosticValueBytes+50)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Fields["token"]; ok {
		t.Fatal("secret field survived")
	}
	if _, ok := d.Fields["path"]; ok {
		t.Fatal("path survived")
	}
	if len(d.Fields["note"]) != MaxDiagnosticValueBytes {
		t.Fatalf("note len=%d", len(d.Fields["note"]))
	}
}
func TestJSONLDiagnosticSinkConcurrentBoundedOutput(t *testing.T) {
	var b bytes.Buffer
	s := NewWriterDiagnosticSink(&b)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Emit(context.Background(), Diagnostic{Component: "test", Operation: "emit", Status: DiagnosticSucceeded, Fields: map[string]string{"id": "x"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if lines := strings.Count(b.String(), "\n"); lines != 16 {
		t.Fatalf("lines=%d", lines)
	}
}
func TestJSONLDiagnosticSinkPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "diag.jsonl")
	s, err := NewJSONLDiagnosticSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Emit(context.Background(), Diagnostic{Component: "ops", Operation: "health"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("info=%v %#v", err, info)
	}
}
