package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNotificationInboxEnqueueIsDurableAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	store, err := NewNotificationInboxStore(path)
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Unix(100, 0).UTC()
	result := JobResult{JobID: "job-1", AttemptID: "job-1-1", State: JobSucceeded, Summary: "backup", FinishedAt: finished}
	first, err := store.EnqueueJobResult(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnqueueJobResult(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Sequence != second.Sequence {
		t.Fatalf("duplicate enqueue changed item: %#v %#v", first, second)
	}
	listed, err := store.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.UnreadCount != 1 || listed.Sequence != 1 {
		t.Fatalf("listed=%#v", listed)
	}
	read, err := store.MarkRead(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ReadAt == nil {
		t.Fatal("notification was not marked read")
	}
	restarted, err := NewNotificationInboxStore(path)
	if err != nil {
		t.Fatal(err)
	}
	listed, err = restarted.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.UnreadCount != 0 || listed.Items[0].ReadAt == nil {
		t.Fatalf("restart lost read state: %#v", listed)
	}
}

func TestNotificationInboxCorruptionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	store, err := NewNotificationInboxStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EnqueueJobResult(context.Background(), JobResult{JobID: "job-1", AttemptID: "job-1-1", State: JobFailed, FinishedAt: time.Unix(100, 0).UTC(), Error: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["items"] = []any{}
	corrupt, _ := json.Marshal(raw)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background(), 50); err == nil {
		t.Fatal("corrupt notification state was silently accepted")
	}
}

func TestJobNotificationInboxFailureDoesNotLoseTerminalState(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "notify-fail", Name: "notify", Owner: "testuser", IdempotencyKey: "notify-fail-1", Request: JobRequest{Prompt: "notify"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	notifications, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "missing", "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(notifications.Path())); err != nil {
		t.Fatal(err)
	}
	results, err := store.RunDueWithNotifier(context.Background(), now, func(context.Context, Job) error { return nil }, NewJobNotificationNotifier(notifications))
	if err != nil || len(results) != 1 || results[0].State != JobSucceeded {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	persisted, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != JobSucceeded || len(persisted.Notifications) != 1 || persisted.Notifications[0].State != "failed" {
		t.Fatalf("persisted=%#v", persisted)
	}
}

func TestNotificationInboxConcurrentDuplicateEnqueue(t *testing.T) {
	store, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := JobResult{JobID: "job-race", AttemptID: "job-race-1", State: JobSucceeded, FinishedAt: time.Unix(100, 0).UTC()}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.EnqueueJobResult(context.Background(), result)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 {
		t.Fatalf("duplicate rows=%d", len(listed.Items))
	}
}

func TestNotificationMarkReadAdvancesChangeSequence(t *testing.T) {
	store, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.EnqueueJobResult(context.Background(), JobResult{JobID: "sequence", AttemptID: "sequence-1", State: JobSucceeded, FinishedAt: time.Unix(100, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if before.Sequence != item.Sequence {
		t.Fatalf("before sequence=%d item=%d", before.Sequence, item.Sequence)
	}
	if _, err := store.MarkRead(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if after.Sequence <= before.Sequence {
		t.Fatalf("read did not advance sequence: before=%d after=%d", before.Sequence, after.Sequence)
	}
}

func TestNotificationListHasStableOlderCursor(t *testing.T) {
	store, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 55; i++ {
		if _, err := store.EnqueueJobResult(context.Background(), JobResult{JobID: fmt.Sprintf("page-%d", i), AttemptID: fmt.Sprintf("page-%d-1", i), State: JobSucceeded, FinishedAt: time.Unix(int64(100+i), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.List(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 50 || first.NextBefore == nil || !first.Truncated {
		t.Fatalf("first page=%#v", first)
	}
	second, err := store.ListBefore(context.Background(), 50, *first.NextBefore)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 5 || second.Items[0].Sequence >= first.Items[len(first.Items)-1].Sequence {
		t.Fatalf("second page=%#v", second)
	}
}

func TestDurableNotificationIntentRecoversOnLaterDeliveryPass(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "recover", Name: "recover", Owner: "testuser", IdempotencyKey: "recover-1", Request: JobRequest{Prompt: "recover"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunDue(context.Background(), now, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NotificationIntent == nil || persisted.NotificationIntent.State != NotificationIntentPending {
		t.Fatalf("intent=%#v", persisted.NotificationIntent)
	}
	notifier := &testNotifier{}
	if _, err := store.RunDueWithNotifier(context.Background(), now, func(context.Context, Job) error { return nil }, notifier); err != nil {
		t.Fatal(err)
	}
	persisted, err = store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || persisted.NotificationIntent.State != NotificationIntentDelivered || len(persisted.Notifications) != 1 {
		t.Fatalf("calls=%d job=%#v", notifier.calls, persisted)
	}
}

func TestNotificationDeliveryRetriesAreBoundedAndHistoryPreserved(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "bounded", Name: "bounded", Owner: "testuser", IdempotencyKey: "bounded-1", Request: JobRequest{Prompt: "bounded"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	notifier := &testNotifier{err: errors.New("inbox full")}
	for i := 0; i < NotificationMaxDeliveryAttempts+2; i++ {
		if _, err := store.RunDueWithNotifier(context.Background(), now, func(context.Context, Job) error { return nil }, notifier); err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notifier.calls != NotificationMaxDeliveryAttempts || len(persisted.Notifications) != NotificationMaxDeliveryAttempts || persisted.NotificationIntent.State != NotificationIntentFailed {
		t.Fatalf("calls=%d job=%#v", notifier.calls, persisted)
	}
}

func TestNotificationDeliveryRecoversExpiredSendingLease(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "lease-recovery", Name: "lease-recovery", Owner: "testuser", IdempotencyKey: "lease-recovery-1", Request: JobRequest{Prompt: "recover"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunDue(context.Background(), now, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	firstDelivery := time.Unix(200, 0).UTC()
	_, firstAttempt, _, ok, err := store.beginNotificationDelivery(job.ID, firstDelivery)
	if err != nil || !ok {
		t.Fatalf("begin delivery ok=%v err=%v", ok, err)
	}
	notifier := &testNotifier{}
	if err := store.deliverPendingNotificationIntents(context.Background(), firstDelivery.Add(NotificationDeliveryLease+time.Second), notifier); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || persisted.NotificationIntent.State != NotificationIntentDelivered || persisted.NotificationIntent.Attempts != 2 || len(persisted.Notifications) != 2 {
		t.Fatalf("calls=%d job=%#v", notifier.calls, persisted)
	}
	if persisted.Notifications[0].ID != firstAttempt.ID || persisted.Notifications[0].State != "failed" || persisted.Notifications[0].FinishedAt == nil {
		t.Fatalf("interrupted attempt was not recovered: %#v", persisted.Notifications)
	}
}

type blockingNotificationNotifier struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (notifier *blockingNotificationNotifier) Notify(ctx context.Context, _ JobResult) error {
	notifier.once.Do(func() { close(notifier.started) })
	select {
	case <-notifier.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNotificationDeliveryLeasePreventsConcurrentClaim(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	job := Job{ID: "lease-exclusive", Name: "lease-exclusive", Owner: "testuser", IdempotencyKey: "lease-exclusive-1", Request: JobRequest{Prompt: "exclusive"}, Schedule: Schedule{RunAt: now}, MaxAttempts: 1}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunDue(context.Background(), now, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	notifier := &blockingNotificationNotifier{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- store.deliverPendingNotificationIntents(context.Background(), time.Unix(200, 0).UTC(), notifier)
	}()
	<-notifier.started
	second := &testNotifier{}
	if err := store.deliverPendingNotificationIntents(context.Background(), time.Unix(201, 0).UTC(), second); err != nil {
		t.Fatal(err)
	}
	if second.calls != 0 {
		t.Fatalf("concurrent delivery claimed leased intent: calls=%d", second.calls)
	}
	close(notifier.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.NotificationIntent.State != NotificationIntentDelivered || persisted.NotificationIntent.Attempts != 1 || len(persisted.Notifications) != 1 {
		t.Fatalf("job=%#v", persisted)
	}
}
