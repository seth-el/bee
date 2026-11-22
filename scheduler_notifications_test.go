package bee

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type testNotifier struct {
	err   error
	calls int
}

func (n *testNotifier) Notify(context.Context, JobResult) error { n.calls++; return n.err }

func TestJobNotificationFailureDoesNotRewriteExecutionState(t *testing.T) {
	store, _ := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "notify", Name: "notify", Owner: "alex", IdempotencyKey: "notify-1", Request: JobRequest{Prompt: "notify"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	notifier := &testNotifier{err: errors.New("notification unavailable")}
	results, err := store.RunDueWithNotifier(context.Background(), now, func(context.Context, Job) error { return nil }, notifier)
	if err != nil || len(results) != 1 || results[0].State != JobSucceeded || notifier.calls != 1 {
		t.Fatalf("results=%#v err=%v calls=%d", results, err, notifier.calls)
	}
	persisted, _ := store.Get(job.ID)
	if persisted.State != JobSucceeded || len(persisted.Notifications) != 1 || persisted.Notifications[0].State != "failed" {
		t.Fatalf("persisted=%#v", persisted)
	}
}

func TestJobNotificationSuccessIsRecordedSeparately(t *testing.T) {
	store, _ := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "notify-ok", Name: "notify", Owner: "alex", IdempotencyKey: "notify-ok-1", Request: JobRequest{Prompt: "notify"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunDueWithNotifier(context.Background(), now, func(context.Context, Job) error { return nil }, &testNotifier{}); err != nil {
		t.Fatal(err)
	}
	persisted, _ := store.Get(job.ID)
	if persisted.State != JobSucceeded || persisted.Notifications[0].State != "sent" {
		t.Fatalf("persisted=%#v", persisted)
	}
}
