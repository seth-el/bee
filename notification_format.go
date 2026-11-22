package bee

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const maxInboxCommandItems = 10

// FormatNotificationLine renders one notification as a single owner-facing
// line: read marker, state, job, age, and the bounded summary. It never
// includes delivery keys, attempt IDs, or the private error.
func FormatNotificationLine(item NotificationItemView, now time.Time) string {
	marker := "*"
	if item.ReadAt != nil {
		marker = " "
	}
	line := fmt.Sprintf("%s %-9s %s  (%s)", marker, item.State, item.JobID, notificationAge(item.CreatedAt, now))
	if summary := strings.TrimSpace(item.Summary); summary != "" {
		line += "  " + summary
	}
	return line
}

func notificationAge(at, now time.Time) string {
	elapsed := now.Sub(at)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed/time.Minute))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed/(24*time.Hour)))
	}
}

// CommandInboxData is the bounded projection returned by /inbox. It reuses
// the public notification view, so it carries no private failure details.
type CommandInboxData struct {
	UnreadCount     int                    `json:"unread_count"`
	UnreadTruncated bool                   `json:"unread_truncated,omitempty"`
	Items           []NotificationItemView `json:"items,omitempty"`
	Acknowledged    int                    `json:"acknowledged,omitempty"`
}

// commandInbox lists unread notifications (`/inbox`) or acknowledges all of
// them (`/inbox ack`). Acknowledging only marks items read; it never changes
// job state, so it is safe for any owner surface.
func commandInbox(ctx context.Context, commandContext CommandContext, args string) CommandResult {
	if commandContext.Application == nil || commandContext.Application.NotificationStore == nil {
		return commandError("the notification inbox is not available in this interface")
	}
	store := commandContext.Application.NotificationStore
	action := strings.ToLower(strings.TrimSpace(args))
	data := CommandInboxData{}
	var lines []string
	switch action {
	case "":
	case "ack":
		count, err := store.MarkAllRead(ctx)
		if err != nil {
			return commandError("acknowledge notifications: " + boundedCommandText(err.Error()))
		}
		data.Acknowledged = count
		lines = append(lines, fmt.Sprintf("acknowledged %d notification(s)", count))
	default:
		return commandError("usage: /inbox [ack]")
	}
	result, err := store.List(ctx, NotificationMaxListLimit)
	if err != nil {
		return commandError("read notifications: " + boundedCommandText(err.Error()))
	}
	data.UnreadCount, data.UnreadTruncated = result.UnreadCount, result.UnreadTruncated
	for _, item := range result.Items {
		if item.ReadAt == nil && len(data.Items) < maxInboxCommandItems {
			data.Items = append(data.Items, item)
		}
	}
	unread := fmt.Sprintf("%d", data.UnreadCount)
	if data.UnreadTruncated {
		unread += "+"
	}
	lines = append(lines, "inbox: "+unread+" unread")
	now := time.Now()
	for _, item := range data.Items {
		lines = append(lines, FormatNotificationLine(item, now))
	}
	if len(data.Items) > 0 {
		lines = append(lines, "details and recovery: `bee inbox`; `/inbox ack` marks all read")
	}
	return CommandResult{Handled: true, Name: CommandInbox, Kind: commandKindInbox, Text: strings.Join(lines, "\n"), Inbox: &data}
}
