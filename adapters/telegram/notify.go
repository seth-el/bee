package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/seth-el/bee"
)

// DefaultNotifyInterval is how often the adapter checks the inbox for new
// notifications to push.
const DefaultNotifyInterval = 30 * time.Second

// MessageSender is the part of the client the push loop needs.
type MessageSender interface {
	SendMessage(ctx context.Context, request SendMessageRequest) (Message, error)
}

// NotificationPushTarget is the ledger target for one chat.
func NotificationPushTarget(chatID int64) string {
	return "telegram:" + strconv.FormatInt(chatID, 10)
}

// PushNotificationsOnce delivers pending inbox notifications to the
// configured chat. It is a no-op when push is not configured.
func PushNotificationsOnce(ctx context.Context, sender MessageSender, config Config, store *bee.NotificationInboxStore, clock func() time.Time) (bee.NotificationPushResult, error) {
	if config.NotifyChatID == 0 {
		return bee.NotificationPushResult{}, nil
	}
	if sender == nil || store == nil {
		return bee.NotificationPushResult{}, errors.New("telegram notification push is not configured")
	}
	chatID := config.NotifyChatID
	return bee.DeliverNotificationPushes(ctx, store, bee.NotificationPushOptions{
		Target: NotificationPushTarget(chatID),
		States: config.NotifyStates,
		Clock:  clock,
	}, func(ctx context.Context, _ bee.NotificationItemView, text string) error {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err := sender.SendMessage(sendCtx, SendMessageRequest{ChatID: chatID, Text: text})
		return err
	})
}

// RunNotificationPush checks the inbox every interval until ctx ends. Push
// problems are reported through logf as bounded lines without message
// content, and never stop the adapter: a dead push target leaves the items
// unread in the inbox, where every other surface still shows them.
func RunNotificationPush(ctx context.Context, sender MessageSender, config Config, store *bee.NotificationInboxStore, interval time.Duration, logf func(format string, args ...any)) {
	if config.NotifyChatID == 0 {
		return
	}
	if interval <= 0 {
		interval = DefaultNotifyInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	lastProblem := ""
	tick := func() {
		result, err := PushNotificationsOnce(ctx, sender, config, store, nil)
		problem := ""
		switch {
		case err != nil && ctx.Err() == nil:
			problem = "notification push: " + err.Error()
		case result.Failed > 0:
			problem = fmt.Sprintf("notification push: %d send(s) failed; will retry", result.Failed)
		}
		if problem != "" && problem != lastProblem {
			logf("%s", problem)
		}
		if problem == "" && lastProblem != "" {
			logf("notification push: recovered")
		}
		lastProblem = problem
		if result.Delivered > 0 {
			logf("notification push: delivered %d", result.Delivered)
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
