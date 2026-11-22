package bee

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestJobStoreApprovalDueExecutionRetryAndRestart(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "job-1", Name: "test", Owner: "alex", IdempotencyKey: "idem-1", Request: JobRequest{Prompt: "test prompt"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 2}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunDue(context.Background(), now, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("job-1")
	if got.State != JobPending {
		t.Fatalf("unapproved state=%s", got.State)
	}
	if err := store.Approve("job-1", now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	results, err := store.RunDue(context.Background(), now, func(context.Context, Job) error {
		calls++
		if calls == 1 {
			return errors.New("temporary")
		}
		return nil
	})
	if err != nil || len(results) != 1 || results[0].State != JobSucceeded || calls != 2 {
		t.Fatalf("results=%#v err=%v calls=%d", results, err, calls)
	}
	restarted, _ := NewJobStore(filepath.Join(filepath.Dir(store.path), "jobs.json"))
	persisted, err := restarted.Get("job-1")
	if err != nil || persisted.State != JobSucceeded || persisted.Attempts != 2 {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	if err := store.Create(job, now); err == nil {
		t.Fatal("duplicate idempotency key accepted")
	}
}

func TestJobStoreRecoveryRequiresStaleRunningLease(t *testing.T) {
	store, _ := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "job-recover", Name: "recover", Owner: "alex", IdempotencyKey: "idem-recover", Request: JobRequest{Prompt: "recover"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.transition(job.ID, JobRunning, now, func(*Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	loaded, _ := store.Get(job.ID)
	loaded.Attempts = 1
	loaded.AttemptsLog = []JobAttempt{{ID: "attempt", JobID: job.ID, Number: 1, State: JobRunning, StartedAt: now, LeaseUntil: now.Add(-time.Minute)}}
	if err := store.replace(loaded); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(job.ID, "retry", now); err != nil {
		t.Fatal(err)
	}
	recovered, _ := store.Get(job.ID)
	if recovered.State != JobApproved {
		t.Fatalf("state=%s", recovered.State)
	}
	if err := store.Recover(job.ID, "fail", now); err == nil {
		t.Fatal("non-running job recovered")
	}
}

func TestJobStoreCancelWhileRunningRemainsCanceled(t *testing.T) {
	store, _ := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	now := time.Now().UTC()
	job := Job{ID: "job-running-cancel", Name: "cancel", Owner: "alex", IdempotencyKey: "cancel-running", Request: JobRequest{Prompt: "cancel"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := store.RunDue(context.Background(), now, func(context.Context, Job) error {
			close(started)
			<-release
			return nil
		})
		finished <- err
	}()
	<-started
	if err := store.Cancel(job.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobCanceled {
		t.Fatalf("state=%s", got.State)
	}
}

func TestJobStoreCancellationAndFailureVisibility(t *testing.T) {
	store, _ := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	now := time.Unix(100, 0).UTC()
	if err := store.Create(Job{ID: "job-2", Name: "cancel", Owner: "alex", IdempotencyKey: "idem-2", Request: JobRequest{Prompt: "cancel prompt"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve("job-2", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.RunDue(ctx, now, func(context.Context, Job) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	if err := store.Cancel("job-2", now); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("job-2")
	if got.State != JobCanceled {
		t.Fatalf("state=%s", got.State)
	}
}
