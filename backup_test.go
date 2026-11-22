package bee

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func backupOps(t *testing.T) (OpsConfig, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		os.Mkdir(filepath.Join(root, d), 0700)
	}
	job := filepath.Join(root, "state", "jobs.json")
	os.WriteFile(job, []byte("jobs"), 0600)
	os.WriteFile(filepath.Join(root, "sessions", "session.jsonl"), []byte("session"), 0600)
	cfg := OpsConfig{Version: 1, StateRoot: filepath.Join(root, "state"), SessionRoot: filepath.Join(root, "sessions"), MemoryRoot: filepath.Join(root, "memory"), JobStore: job, ArtifactRoot: filepath.Join(root, "artifacts"), DiagnosticsRoot: filepath.Join(root, "diagnostics"), Health: OpsHealth{TimeoutMS: 1000}}
	ops := filepath.Join(root, "ops.json")
	return cfg, ops
}
func TestBackupDryRunRestoreAndConflict(t *testing.T) {
	cfg, _ := backupOps(t)
	dest := filepath.Join(t.TempDir(), "bee.tar.gz")
	manifest, err := Backup(context.Background(), cfg, BackupOptions{Destination: dest, MaxBytes: 1 << 20})
	if err != nil || manifest.TotalBytes == 0 {
		t.Fatalf("manifest=%#v err=%v", manifest, err)
	}
	target := t.TempDir()
	restored := cfg
	restored.StateRoot = filepath.Join(target, "state")
	restored.SessionRoot = filepath.Join(target, "sessions")
	restored.MemoryRoot = filepath.Join(target, "memory")
	restored.ArtifactRoot = filepath.Join(target, "artifacts")
	restored.DiagnosticsRoot = filepath.Join(target, "diagnostics")
	restored.JobStore = filepath.Join(restored.StateRoot, "jobs.json")
	for _, d := range []string{restored.StateRoot, restored.SessionRoot, restored.MemoryRoot, restored.ArtifactRoot, restored.DiagnosticsRoot} {
		os.Mkdir(d, 0700)
	}
	report, err := Restore(context.Background(), restored, RestoreOptions{Source: dest, MaxBytes: 1 << 20})
	if err != nil || report.WouldWrite == 0 || report.Applied {
		t.Fatalf("dry report=%#v err=%v", report, err)
	}
	report, err = Restore(context.Background(), restored, RestoreOptions{Source: dest, Apply: true, MaxBytes: 1 << 20})
	if err != nil || !report.Applied {
		t.Fatalf("apply report=%#v err=%v", report, err)
	}
	if _, err := os.Stat(restored.JobStore); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(restored.JobStore, []byte("conflict"), 0600)
	report, err = Restore(context.Background(), restored, RestoreOptions{Source: dest, Apply: true, MaxBytes: 1 << 20})
	if err != nil || len(report.Conflicts) == 0 {
		t.Fatalf("conflict report=%#v err=%v", report, err)
	}
	_ = time.Now()
}
