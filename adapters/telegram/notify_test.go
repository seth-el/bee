package telegram

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type fakeMessageSender struct{ requests []SendMessageRequest }

func (f *fakeMessageSender) SendMessage(_ context.Context, request SendMessageRequest) (Message, error) {
	f.requests = append(f.requests, request)
	return Message{MessageID: len(f.requests)}, nil
}

func TestNotifyConfigValidation(t *testing.T) {
	config := testConfig(t)
	config.NotifyChatID = 42
	if err := config.Validate(); err != nil {
		t.Fatalf("allowed notify chat rejected: %v", err)
	}
	config.NotifyChatID = 77
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "allowed_chats") {
		t.Fatalf("notify chat outside allowlist accepted: %v", err)
	}
	config.NotifyChatID = 0
	config.NotifyStates = []string{"paused"}
	if err := config.Validate(); err == nil {
		t.Fatal("notify_states without notify_chat_id accepted")
	}
	config.NotifyChatID = 42
	config.NotifyStates = []string{"running"}
	if err := config.Validate(); err == nil {
		t.Fatal("unknown notify state accepted")
	}
}

func TestPushNotificationsOnceSendsToNotifyChat(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 10, 0, 0, time.UTC)
	inbox, err := bee.NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	inbox.SetClock(func() time.Time { return now })
	if _, err := inbox.EnqueueJobResult(context.Background(), bee.JobResult{JobID: "recurring-dreaming-1", AttemptID: "a1", State: bee.JobPaused, Summary: "dreaming", FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	sender := &fakeMessageSender{}
	// Push off: nothing is sent.
	if result, err := PushNotificationsOnce(context.Background(), sender, config, inbox, func() time.Time { return now }); err != nil || result.Delivered != 0 || len(sender.requests) != 0 {
		t.Fatalf("push with notify off = %+v, %v, %d", result, err, len(sender.requests))
	}
	config.NotifyChatID = 42
	for i := 0; i < 2; i++ {
		if _, err := PushNotificationsOnce(context.Background(), sender, config, inbox, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.requests) != 1 || sender.requests[0].ChatID != 42 || !strings.Contains(sender.requests[0].Text, "paused") {
		t.Fatalf("requests = %#v", sender.requests)
	}
}
