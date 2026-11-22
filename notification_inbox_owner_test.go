package bee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotificationInboxBulkAcknowledgement(t *testing.T) {
	inbox, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	for _, result := range []JobResult{
		{JobID: "job-a", AttemptID: "job-a-1", State: JobPaused, FinishedAt: finished},
		{JobID: "job-a", AttemptID: "job-a-2", State: JobFailed, FinishedAt: finished},
		{JobID: "job-b", AttemptID: "job-b-1", State: JobSucceeded, FinishedAt: finished},
	} {
		if _, err := inbox.EnqueueJobResult(context.Background(), result); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := inbox.MarkJobRead(context.Background(), "job-a"); err != nil || count != 2 {
		t.Fatalf("mark job read = %d, %v", count, err)
	}
	if count, err := inbox.MarkJobRead(context.Background(), "job-a"); err != nil || count != 0 {
		t.Fatalf("second mark job read = %d, %v (want idempotent)", count, err)
	}
	list, err := inbox.List(context.Background(), 10)
	if err != nil || list.UnreadCount != 1 || list.Items[0].JobID != "job-b" || list.Items[0].ReadAt != nil {
		t.Fatalf("after job ack = %#v, %v", list, err)
	}
	if count, err := inbox.MarkAllRead(context.Background()); err != nil || count != 1 {
		t.Fatalf("mark all read = %d, %v", count, err)
	}
	if list, err = inbox.List(context.Background(), 10); err != nil || list.UnreadCount != 0 {
		t.Fatalf("after ack all = %#v, %v", list, err)
	}
	if _, err := inbox.MarkJobRead(context.Background(), "../escape"); err == nil {
		t.Fatal("unsafe job id accepted")
	}
}

func TestInboxCommandListsAndAcknowledges(t *testing.T) {
	inbox, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{NotificationStore: inbox}
	if result := DispatchCommand(context.Background(), "/inbox", CommandContext{Application: app}); result.Kind != commandKindInbox || result.Inbox == nil || result.Inbox.UnreadCount != 0 || !strings.Contains(result.Text, "0 unread") {
		t.Fatalf("empty inbox = %#v", result)
	}
	finished := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := inbox.EnqueueJobResult(context.Background(), JobResult{JobID: "recurring-dreaming-1", AttemptID: "recurring-dreaming-1-1", State: JobPaused, Summary: "dreaming", Error: "private reason", FinishedAt: finished}); err != nil {
		t.Fatal(err)
	}
	result := DispatchCommand(context.Background(), "/inbox", CommandContext{Application: app})
	if result.Inbox == nil || result.Inbox.UnreadCount != 1 || len(result.Inbox.Items) != 1 || !strings.Contains(result.Text, "paused") || !strings.Contains(result.Text, "recurring-dreaming-1") || !strings.Contains(result.Text, "2h ago") {
		t.Fatalf("inbox = %#v", result)
	}
	if strings.Contains(result.Text, "private reason") {
		t.Fatal("/inbox exposed the private failure reason")
	}
	event, _ := json.Marshal(localCommandEvent(result))
	if !strings.Contains(string(event), `"inbox"`) || strings.Contains(string(event), "private reason") || strings.Contains(string(event), "delivery_key") {
		t.Fatalf("event = %s", event)
	}
	acked := DispatchCommand(context.Background(), "/inbox ack", CommandContext{Application: app})
	if acked.Inbox == nil || acked.Inbox.Acknowledged != 1 || acked.Inbox.UnreadCount != 0 {
		t.Fatalf("ack = %#v", acked)
	}
	if bad := DispatchCommand(context.Background(), "/inbox purge", CommandContext{Application: app}); bad.Kind != commandKindError {
		t.Fatalf("bad arg = %#v", bad)
	}
	if none := DispatchCommand(context.Background(), "/inbox", CommandContext{Application: &Application{}}); none.Kind != commandKindError {
		t.Fatalf("no store = %#v", none)
	}
	if badge := tuiInboxBadge(3, false); badge != "inbox 3 unread · /inbox" {
		t.Fatalf("badge = %q", badge)
	}
	if badge := tuiInboxBadge(99, true); !strings.Contains(badge, "99+") || tuiInboxBadge(0, false) != "" {
		t.Fatalf("badge truncation = %q", badge)
	}
}

func TestOpenAIHTTPNotificationsAreAuthenticatedBoundedAndStrict(t *testing.T) {
	handler, _, _, _ := newOpenAISessionTestHandler(t)
	inbox, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler.handlers.Application.NotificationStore = inbox
	item, err := inbox.EnqueueJobResult(context.Background(), JobResult{JobID: "job-a", AttemptID: "job-a-1", State: JobFailed, Summary: "nightly", Error: "private failure detail", FinishedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized = %d", unauthorized.Code)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedSessionRequest(http.MethodGet, "/api/notifications?limit=5", ""))
	if list.Code != http.StatusOK || list.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list = %d %v %s", list.Code, list.Header(), list.Body.String())
	}
	body := list.Body.String()
	for _, private := range []string{"private failure detail", "delivery_key", "attempt_id", "job-a-1"} {
		if strings.Contains(body, private) {
			t.Fatalf("list exposed %q: %s", private, body)
		}
	}
	var decoded NotificationListResult
	if err := json.Unmarshal(list.Body.Bytes(), &decoded); err != nil || decoded.UnreadCount != 1 || len(decoded.Items) != 1 || decoded.Items[0].ID != item.ID {
		t.Fatalf("decoded = %#v, %v", decoded, err)
	}

	for _, target := range []string{"/api/notifications?limit=0", "/api/notifications?limit=51", "/api/notifications?before=0", "/api/notifications?state=unread", "/api/notifications?limit=1&limit=2"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedSessionRequest(http.MethodGet, target, ""))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d", target, recorder.Code)
		}
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, authenticatedSessionRequest(http.MethodPost, "/api/notifications", ""))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post list = %d", post.Code)
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, authenticatedSessionRequest(http.MethodPost, "/api/notifications/"+item.ID+"/read", ""))
	if read.Code != http.StatusOK || strings.Contains(read.Body.String(), "private failure detail") || !strings.Contains(read.Body.String(), `"read_at"`) {
		t.Fatalf("read = %d %s", read.Code, read.Body.String())
	}
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, authenticatedSessionRequest(http.MethodPost, "/api/notifications/"+item.ID+"/read", ""))
	if again.Code != http.StatusOK {
		t.Fatalf("idempotent read = %d", again.Code)
	}
	for target, want := range map[string]int{
		"/api/notifications/notification-missing/read": http.StatusNotFound,
		"/api/notifications/" + item.ID:                http.StatusNotFound,
		"/api/notifications/" + item.ID + "/delete":    http.StatusNotFound,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedSessionRequest(http.MethodPost, target, ""))
		if recorder.Code != want {
			t.Fatalf("%s = %d, want %d", target, recorder.Code, want)
		}
	}
	getRead := httptest.NewRecorder()
	handler.ServeHTTP(getRead, authenticatedSessionRequest(http.MethodGet, "/api/notifications/"+item.ID+"/read", ""))
	if getRead.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET read = %d", getRead.Code)
	}

	// /inbox (and /inbox ack) is allowed through the HTTP command endpoint.
	if reason := openAIWebCommandUnavailable(CommandInbox, ""); reason != "" {
		t.Fatalf("/inbox unavailable over HTTP: %s", reason)
	}
	if reason := openAIWebCommandUnavailable(CommandInbox, "ack"); reason != "" {
		t.Fatalf("/inbox ack unavailable over HTTP: %s", reason)
	}

	handler.handlers.Application.NotificationStore = nil
	unavailable := httptest.NewRecorder()
	handler.ServeHTTP(unavailable, authenticatedSessionRequest(http.MethodGet, "/api/notifications", ""))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable = %d", unavailable.Code)
	}
}
