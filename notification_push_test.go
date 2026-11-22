package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPushTestInbox(t *testing.T, now time.Time, results ...JobResult) *NotificationInboxStore {
	t.Helper()
	inbox, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	inbox.SetClock(func() time.Time { return now })
	for _, result := range results {
		if _, err := inbox.EnqueueJobResult(context.Background(), result); err != nil {
			t.Fatal(err)
		}
	}
	return inbox
}

type pushRecorder struct {
	texts []string
	fail  error
}

func (r *pushRecorder) send(_ context.Context, _ NotificationItemView, text string) error {
	if r.fail != nil {
		return r.fail
	}
	r.texts = append(r.texts, text)
	return nil
}

func TestNotificationPushDeliversOnceAndSkipsByDefaultState(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 10, 0, 0, time.UTC)
	inbox := newPushTestInbox(t, now,
		JobResult{JobID: "recurring-dreaming-1", AttemptID: "a1", State: JobPaused, Summary: "dreaming", Error: "private reason /home/x", FinishedAt: now.Add(-5 * time.Minute)},
		JobResult{JobID: "recurring-dreaming-2", AttemptID: "a2", State: JobSucceeded, Summary: "dreaming", FinishedAt: now.Add(-4 * time.Minute)},
		JobResult{JobID: "recurring-dreaming-3", AttemptID: "a3", State: JobFailed, Summary: "dreaming", FinishedAt: now.Add(-3 * time.Minute)},
	)
	options := NotificationPushOptions{Target: "telegram:42", Clock: func() time.Time { return now }}
	recorder := &pushRecorder{}
	result, err := DeliverNotificationPushes(context.Background(), inbox, options, recorder.send)
	if err != nil || result.Delivered != 2 || result.Skipped != 1 {
		t.Fatalf("first push = %+v, %v", result, err)
	}
	if len(recorder.texts) != 2 || !strings.Contains(recorder.texts[0], `"dreaming" paused`) || !strings.Contains(recorder.texts[1], `"dreaming" failed`) {
		t.Fatalf("texts (want oldest first, paused then failed) = %q", recorder.texts)
	}
	for _, text := range recorder.texts {
		if strings.Contains(text, "private reason") || strings.Contains(text, "/home/") {
			t.Fatalf("push leaked private error text: %q", text)
		}
	}
	// A later call, as after a restart, sends nothing again.
	result, err = DeliverNotificationPushes(context.Background(), inbox, options, recorder.send)
	if err != nil || result != (NotificationPushResult{}) || len(recorder.texts) != 2 {
		t.Fatalf("second push = %+v, %v, %d texts", result, err, len(recorder.texts))
	}
	ledger := NotificationPushLedgerPath(inbox, options.Target)
	if info, err := os.Stat(ledger); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, %v", info, err)
	}
	// Push never marks inbox items read.
	list, err := inbox.List(context.Background(), 10)
	if err != nil || list.UnreadCount != 3 {
		t.Fatalf("unread after push = %d, %v", list.UnreadCount, err)
	}
	// Opting into succeeded with a fresh target only sends recent items.
	recorder = &pushRecorder{}
	all := NotificationPushOptions{Target: "telegram:43", States: []string{"succeeded"}, Clock: func() time.Time { return now }}
	if result, err := DeliverNotificationPushes(context.Background(), inbox, all, recorder.send); err != nil || result.Delivered != 1 || !strings.Contains(recorder.texts[0], "succeeded") {
		t.Fatalf("succeeded push = %+v, %v, %q", result, err, recorder.texts)
	}
}

func TestNotificationPushSkipsReadAndBacklog(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 10, 0, 0, time.UTC)
	inbox := newPushTestInbox(t, now,
		JobResult{JobID: "old", AttemptID: "a1", State: JobPaused, FinishedAt: now.Add(-48 * time.Hour)},
		JobResult{JobID: "read", AttemptID: "a2", State: JobPaused, FinishedAt: now.Add(-time.Minute)},
	)
	if _, err := inbox.MarkJobRead(context.Background(), "read"); err != nil {
		t.Fatal(err)
	}
	recorder := &pushRecorder{}
	result, err := DeliverNotificationPushes(context.Background(), inbox, NotificationPushOptions{Target: "telegram:42", Clock: func() time.Time { return now }}, recorder.send)
	if err != nil || result.Delivered != 0 || result.Skipped != 2 || len(recorder.texts) != 0 {
		t.Fatalf("push = %+v, %v, %q", result, err, recorder.texts)
	}
}

func TestNotificationPushFailureRetriesThenStops(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 10, 0, 0, time.UTC)
	inbox := newPushTestInbox(t, now, JobResult{JobID: "job", AttemptID: "a1", State: JobFailed, FinishedAt: now.Add(-time.Minute)})
	options := NotificationPushOptions{Target: "telegram:42", Clock: func() time.Time { return now }}
	recorder := &pushRecorder{fail: errors.New("network down")}
	for i := 0; i < NotificationPushMaxAttempts; i++ {
		result, err := DeliverNotificationPushes(context.Background(), inbox, options, recorder.send)
		if err != nil || result.Failed != 1 {
			t.Fatalf("attempt %d = %+v, %v", i+1, result, err)
		}
	}
	result, err := DeliverNotificationPushes(context.Background(), inbox, options, recorder.send)
	if err != nil || result != (NotificationPushResult{}) {
		t.Fatalf("after max attempts = %+v, %v", result, err)
	}
	list, err := inbox.List(context.Background(), 10)
	if err != nil || list.UnreadCount != 1 {
		t.Fatalf("failed push must leave the item unread: %d, %v", list.UnreadCount, err)
	}
	// A recovered target delivers an item that failed fewer than max times.
	inbox2 := newPushTestInbox(t, now, JobResult{JobID: "job", AttemptID: "a1", State: JobFailed, FinishedAt: now.Add(-time.Minute)})
	recorder = &pushRecorder{fail: errors.New("down")}
	if _, err := DeliverNotificationPushes(context.Background(), inbox2, options, recorder.send); err != nil {
		t.Fatal(err)
	}
	recorder.fail = nil
	if result, err := DeliverNotificationPushes(context.Background(), inbox2, options, recorder.send); err != nil || result.Delivered != 1 {
		t.Fatalf("recovered push = %+v, %v", result, err)
	}
}

func TestNotificationPushRejectsBadOptions(t *testing.T) {
	inbox := newPushTestInbox(t, time.Now().UTC())
	send := (&pushRecorder{}).send
	for _, options := range []NotificationPushOptions{
		{Target: ""},
		{Target: "telegram:../x"},
		{Target: "telegram:42", States: []string{"running"}},
		{Target: "telegram:42", States: []string{"paused", "paused"}},
	} {
		if _, err := DeliverNotificationPushes(context.Background(), inbox, options, send); err == nil {
			t.Fatalf("options %+v accepted", options)
		}
	}
}
