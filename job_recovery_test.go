package bee

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pausedRecurringOccurrence runs one recurring tick whose execution needs an
// approval, leaving a paused occurrence and its inbox notification.
func pausedRecurringOccurrence(t *testing.T) (RecurringJobsConfig, *JobStore, *NotificationInboxStore, *RecurringScheduler, Job, time.Time) {
	t.Helper()
	start := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].MaxCatchUp = 1
	inbox, err := NewNotificationInboxStore(NotificationStorePathForJobStore(store.Path()))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := scheduler.Tick(context.Background(), start, func(context.Context, Job) (JobExecution, error) {
		return JobExecution{SessionID: "first-session"}, ErrJobApprovalRequired
	}, NewJobNotificationNotifier(inbox))
	if err != nil || len(results) != 1 || results[0].State != JobPaused {
		t.Fatalf("paused tick = %#v, %v", results, err)
	}
	return config, store, inbox, scheduler, results[0], start
}

func TestRetryRecurringRerunsSameOccurrenceWithIdenticalContract(t *testing.T) {
	_, store, inbox, scheduler, paused, start := pausedRecurringOccurrence(t)
	before := *paused.Contract
	retried, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != JobApproved || retried.MaxAttempts != retried.Attempts+1 || retried.LastError != "" || retried.FinishedAt != nil {
		t.Fatalf("retried job = %#v", retried)
	}
	if !reflect.DeepEqual(*retried.Contract, before) || retried.Request.Prompt != paused.Request.Prompt {
		t.Fatalf("retry changed the contract or prompt: %#v", retried)
	}
	if len(retried.OwnerActions) != 1 || retried.OwnerActions[0].Action != JobOwnerActionRetry || retried.OwnerActions[0].Detail != jobRetryKeptPersisted {
		t.Fatalf("owner actions = %#v", retried.OwnerActions)
	}

	// The next tick runs the same occurrence once; nothing is materialized twice.
	ran := 0
	results, err := scheduler.Tick(context.Background(), start.Add(2*time.Second), func(_ context.Context, job Job) (JobExecution, error) {
		ran++
		if job.ID != paused.ID {
			t.Fatalf("ran %s, want %s", job.ID, paused.ID)
		}
		return JobExecution{SessionID: "second-session"}, nil
	}, NewJobNotificationNotifier(inbox))
	if err != nil || ran != 1 || len(results) != 1 || results[0].State != JobSucceeded || results[0].Attempts != 2 {
		t.Fatalf("retry tick ran=%d results=%#v err=%v", ran, results, err)
	}
	if got := results[0].AttemptsLog; len(got) != 2 || got[0].SessionID != "first-session" || got[1].SessionID != "second-session" {
		t.Fatalf("attempt history = %#v", got)
	}
	jobs, err := store.List()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs after retry = %d, %v", len(jobs), err)
	}
	list, err := inbox.List(context.Background(), 10)
	if err != nil || len(list.Items) != 2 || list.Items[0].State != NotificationStateSucceeded {
		t.Fatalf("retry produced its own notification: %#v, %v", list, err)
	}
}

func TestRetryRecurringFailureDoesNotRetryAgainOnItsOwn(t *testing.T) {
	_, store, inbox, scheduler, paused, start := pausedRecurringOccurrence(t)
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start); err != nil {
		t.Fatal(err)
	}
	calls := 0
	results, err := scheduler.Tick(context.Background(), start.Add(time.Second), func(context.Context, Job) (JobExecution, error) {
		calls++
		return JobExecution{}, errors.New("still broken")
	}, NewJobNotificationNotifier(inbox))
	if err != nil || calls != 1 || len(results) != 1 || results[0].State != JobFailed {
		t.Fatalf("calls=%d results=%#v err=%v", calls, results, err)
	}
	// The owner can retry a failed occurrence again.
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start.Add(2*time.Second)); err != nil {
		t.Fatalf("retry of failed occurrence: %v", err)
	}
}

func TestRetryRecurringRefreshFromConfigReplacesPromptAndContract(t *testing.T) {
	config, store, _, _, paused, start := pausedRecurringOccurrence(t)
	definition := config.Jobs[0]
	definition.Prompt = "fixed prompt"
	definition.Contract.MaxToolCalls = 8
	retried, err := store.RetryRecurring(paused.ID, JobRetryOptions{Refresh: &definition}, start)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Request.Prompt != "fixed prompt" || retried.Contract.MaxToolCalls != 8 || retried.OwnerActions[0].Detail != jobRefreshedFromConfig {
		t.Fatalf("refreshed job = %#v", retried)
	}

	_, store, _, _, paused, start = pausedRecurringOccurrence(t)
	other := definition
	other.ID = "someone-else"
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{Refresh: &other}, start); err == nil || !strings.Contains(err.Error(), "different recurring job") {
		t.Fatalf("foreign definition err = %v", err)
	}
	invalid := definition
	invalid.Contract.WorkspacePaths = nil
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{Refresh: &invalid}, start); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid definition err = %v", err)
	}
	if job, _ := store.Get(paused.ID); job.State != JobPaused {
		t.Fatalf("rejected refresh changed the job: %#v", job)
	}
}

func TestRetryRecurringRefusals(t *testing.T) {
	_, store, inbox, scheduler, paused, start := pausedRecurringOccurrence(t)
	if _, err := store.RetryRecurring("missing", JobRetryOptions{}, start); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing job err = %v", err)
	}
	oneShot := Job{ID: "one-shot", Name: "one-shot", Owner: "owner", IdempotencyKey: "one-shot", Request: JobRequest{Prompt: "p"}, Schedule: Schedule{RunAt: start}}
	if err := store.Create(oneShot, start); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryRecurring("one-shot", JobRetryOptions{}, start); err == nil || !strings.Contains(err.Error(), "recurring") {
		t.Fatalf("one-shot retry err = %v", err)
	}
	if _, err := store.DismissRecurring("one-shot", "", start); err == nil || !strings.Contains(err.Error(), "recurring") {
		t.Fatalf("one-shot dismiss err = %v", err)
	}

	// Succeeded and dismissed occurrences cannot be retried.
	if _, err := store.DismissRecurring(paused.ID, "superseded", start); err != nil {
		t.Fatal(err)
	}
	results, err := scheduler.Tick(context.Background(), start.Add(time.Minute), func(context.Context, Job) (JobExecution, error) {
		return JobExecution{}, nil
	}, NewJobNotificationNotifier(inbox))
	if err != nil || len(results) != 1 || results[0].State != JobSucceeded {
		t.Fatalf("next occurrence = %#v, %v", results, err)
	}
	if _, err := store.RetryRecurring(results[0].ID, JobRetryOptions{}, start); err == nil || !strings.Contains(err.Error(), "only paused or failed") {
		t.Fatalf("succeeded retry err = %v", err)
	}
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("dismissed retry err = %v", err)
	}

	// A later occurrence exists: retrying the old one would run out of order.
	_, store, _, _, paused, start = pausedRecurringOccurrence(t)
	jobs, _ := store.List()
	later := jobs[0]
	later.ID, later.IdempotencyKey, later.Occurrence, later.State = "later", "later", later.Occurrence.Add(time.Minute), JobSucceeded
	if err := store.saveLocked(append(jobs, later)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start); err == nil || !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("out-of-order retry err = %v", err)
	}
}

func TestRetryRecurringOwnerActionLimit(t *testing.T) {
	_, store, _, _, paused, start := pausedRecurringOccurrence(t)
	jobs, _ := store.List()
	for i := 0; i < maxJobOwnerActions; i++ {
		jobs[0].OwnerActions = append(jobs[0].OwnerActions, JobOwnerAction{Action: JobOwnerActionRetry, At: start})
	}
	if err := store.saveLocked(jobs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryRecurring(paused.ID, JobRetryOptions{}, start); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit err = %v", err)
	}
}

func TestDismissRecurringIsTerminalAndNeverRematerialized(t *testing.T) {
	_, store, _, scheduler, paused, start := pausedRecurringOccurrence(t)
	if _, err := store.DismissRecurring(paused.ID, "bad\nreason", start); err == nil {
		t.Fatal("multi-line reason accepted")
	}
	dismissed, err := store.DismissRecurring(paused.ID, "prompt was wrong", start)
	if err != nil {
		t.Fatal(err)
	}
	if dismissed.State != JobCanceled || dismissed.LastError != "dismissed by owner: prompt was wrong" || dismissed.OwnerActions[0].Action != JobOwnerActionDismiss {
		t.Fatalf("dismissed = %#v", dismissed)
	}
	if _, err := store.DismissRecurring(paused.ID, "", start); err == nil {
		t.Fatal("dismissing twice was accepted")
	}
	// Same tick time: the dismissed occurrence is not recreated.
	created, err := scheduler.Materialize(start)
	if err != nil || len(created) != 0 {
		t.Fatalf("rematerialized after dismiss: %#v, %v", created, err)
	}
}
