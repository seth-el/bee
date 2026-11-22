package bee

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupDryRunDoesNotDeleteExpiredDiagnostic(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		os.Mkdir(filepath.Join(root, d), 0700)
	}
	job := filepath.Join(root, "state", "jobs.json")
	os.WriteFile(job, []byte("{}"), 0600)
	diag := filepath.Join(root, "diagnostics", "diagnostics-old.jsonl")
	os.WriteFile(diag, []byte("{}\n"), 0600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(diag, old, old)
	cfg := OpsConfig{Version: 1, StateRoot: filepath.Join(root, "state"), SessionRoot: filepath.Join(root, "sessions"), MemoryRoot: filepath.Join(root, "memory"), JobStore: job, ArtifactRoot: filepath.Join(root, "artifacts"), DiagnosticsRoot: filepath.Join(root, "diagnostics"), Retention: OpsRetention{DiagnosticsDays: 1}, Health: OpsHealth{TimeoutMS: 1000}}
	r, err := PlanCleanup(context.Background(), cfg, CleanupOptions{Now: time.Now()})
	if err != nil || len(r.Candidates) != 1 {
		t.Fatalf("report=%#v err=%v", r, err)
	}
	if _, err := os.Stat(diag); err != nil {
		t.Fatal(err)
	}
}
func TestCleanupApplyRemovesExpiredDiagnostic(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		os.Mkdir(filepath.Join(root, d), 0700)
	}
	job := filepath.Join(root, "state", "jobs.json")
	os.WriteFile(job, []byte("{}"), 0600)
	diag := filepath.Join(root, "diagnostics", "diagnostics-old.jsonl")
	os.WriteFile(diag, []byte("{}\n"), 0600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(diag, old, old)
	cfg := OpsConfig{Version: 1, StateRoot: filepath.Join(root, "state"), SessionRoot: filepath.Join(root, "sessions"), MemoryRoot: filepath.Join(root, "memory"), JobStore: job, ArtifactRoot: filepath.Join(root, "artifacts"), DiagnosticsRoot: filepath.Join(root, "diagnostics"), Retention: OpsRetention{DiagnosticsDays: 1}, Health: OpsHealth{TimeoutMS: 1000}}
	r, err := ApplyCleanup(context.Background(), cfg, CleanupOptions{Now: time.Now(), MaxItems: 1})
	if err != nil || len(r.Removed) != 1 {
		t.Fatalf("report=%#v err=%v", r, err)
	}
	if _, err := os.Stat(diag); !os.IsNotExist(err) {
		t.Fatalf("diag remains err=%v", err)
	}
}
