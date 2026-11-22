package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPhase10GRecoveryDrills(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	job := filepath.Join(root, "state", "jobs.json")
	if err := os.WriteFile(job, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", "session.jsonl"), []byte("session evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := OpsConfig{Version: 1, StateRoot: filepath.Join(root, "state"), SessionRoot: filepath.Join(root, "sessions"), MemoryRoot: filepath.Join(root, "memory"), JobStore: job, ArtifactRoot: filepath.Join(root, "artifacts"), DiagnosticsRoot: filepath.Join(root, "diagnostics"), Health: OpsHealth{TimeoutMS: 1000}}
	health := CheckHealth(context.Background(), cfg)
	if health.Status != CheckPass {
		t.Fatalf("health=%#v", health)
	}
	backupPath := filepath.Join(root, "backup.tar.gz")
	if _, err := Backup(context.Background(), cfg, BackupOptions{Destination: backupPath, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "clean-target")
	restored := cfg
	restored.StateRoot = filepath.Join(target, "state")
	restored.SessionRoot = filepath.Join(target, "sessions")
	restored.MemoryRoot = filepath.Join(target, "memory")
	restored.ArtifactRoot = filepath.Join(target, "artifacts")
	restored.DiagnosticsRoot = filepath.Join(target, "diagnostics")
	restored.JobStore = filepath.Join(restored.StateRoot, "jobs.json")
	for _, dir := range []string{restored.StateRoot, restored.SessionRoot, restored.MemoryRoot, restored.ArtifactRoot, restored.DiagnosticsRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := Restore(context.Background(), restored, RestoreOptions{Source: backupPath}); err != nil || report.Applied || report.WouldWrite == 0 {
		t.Fatalf("restore dry-run=%#v err=%v", report, err)
	}
	if report, err := Restore(context.Background(), restored, RestoreOptions{Source: backupPath, Apply: true}); err != nil || !report.Applied {
		t.Fatalf("restore apply=%#v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(restored.SessionRoot, "session.jsonl")); err != nil {
		t.Fatalf("restored session: %v", err)
	}
	if err := os.WriteFile(restored.JobStore, []byte("conflicting state"), 0600); err != nil {
		t.Fatal(err)
	}
	if report, err := Restore(context.Background(), restored, RestoreOptions{Source: backupPath, Apply: true}); err != nil || len(report.Conflicts) == 0 {
		t.Fatalf("restore conflict=%#v err=%v", report, err)
	}

	diagnostic := filepath.Join(cfg.DiagnosticsRoot, "diagnostics-old.jsonl")
	if err := os.WriteFile(diagnostic, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := os.Chtimes(diagnostic, old, old); err != nil {
		t.Fatal(err)
	}
	cfg.Retention.DiagnosticsDays = 1
	if report, err := PlanCleanup(context.Background(), cfg, CleanupOptions{Now: time.Now().UTC()}); err != nil || len(report.Candidates) != 1 {
		t.Fatalf("cleanup dry-run=%#v err=%v", report, err)
	}
	if _, err := os.Stat(diagnostic); err != nil {
		t.Fatal(err)
	}
	if report, err := ApplyCleanup(context.Background(), cfg, CleanupOptions{Now: time.Now().UTC()}); err != nil || len(report.Removed) != 1 {
		t.Fatalf("cleanup apply=%#v err=%v", report, err)
	}

	jobStore, err := NewJobStore(cfg.JobStore)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	drillJob := Job{ID: "drill-job", Name: "drill", Owner: "operator", IdempotencyKey: "drill-idem", Request: JobRequest{Prompt: "drill"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := jobStore.Create(drillJob, now); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Approve(drillJob.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := jobStore.RunDue(context.Background(), now, func(context.Context, Job) error { return errors.New("simulated crash") }); err != nil {
		t.Fatal(err)
	}
	stale, err := jobStore.Get(drillJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale.State = JobRunning
	stale.AttemptsLog = []JobAttempt{{ID: "drill-attempt", JobID: drillJob.ID, Number: 1, State: JobRunning, StartedAt: now.Add(-time.Hour), LeaseUntil: now.Add(-time.Minute)}}
	if err := jobStore.replace(stale); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Recover(drillJob.ID, "retry", now); err != nil {
		t.Fatal(err)
	}
	recovered, err := jobStore.Get(drillJob.ID)
	if err != nil || recovered.State != JobApproved {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
}
