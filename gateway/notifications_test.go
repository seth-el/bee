package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type notificationCancelWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	cancel  context.CancelFunc
	flushes int
}

func (w *notificationCancelWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *notificationCancelWriter) WriteHeader(status int) { w.status = status }
func (w *notificationCancelWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}
func (w *notificationCancelWriter) Flush() {
	w.flushes++
	if w.flushes >= 1 && w.cancel != nil {
		w.cancel()
	}
}

func TestGatewayNotificationInboxListAndReadFlow(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	if service.notifications == nil {
		t.Fatal("notification store was not wired")
	}
	item, err := service.notifications.EnqueueJobResult(context.Background(), bee.JobResult{JobID: "job-web", AttemptID: "job-web-1", State: bee.JobSucceeded, Summary: "web job", FinishedAt: time.Unix(100, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	list := httptest.NewRecorder()
	service.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/notifications?limit=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var payload bee.NotificationListResult
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.UnreadCount != 1 || payload.Items[0].ID != item.ID {
		t.Fatalf("payload=%#v", payload)
	}
	read := httptest.NewRecorder()
	service.Handler().ServeHTTP(read, httptest.NewRequest(http.MethodPost, "/api/notifications/"+item.ID+"/read", strings.NewReader("{}")))
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	list = httptest.NewRecorder()
	service.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("second list status=%d", list.Code)
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.UnreadCount != 0 || payload.Items[0].ReadAt == nil {
		t.Fatalf("read state=%#v", payload)
	}
	if strings.Contains(list.Body.String(), "delivery_key") || strings.Contains(list.Body.String(), "attempt_id") {
		t.Fatalf("private notification identifiers leaked: %s", list.Body.String())
	}
}

func TestGatewayCustomJobStoreUsesMatchingNotificationStore(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	config.Provider.Model = "custom-path-test"
	config.Assistant.MemoryRoot = t.TempDir()
	if err := os.Chmod(config.Assistant.MemoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(t.TempDir(), "jobs", "jobs.json")
	config.Assistant.JobStorePath = jobPath
	service, err := New(Config{BeeConfig: config, SessionStore: store, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())
	want := bee.NotificationStorePathForJobStore(jobPath)
	if got := service.App().NotificationStore.Path(); got != want {
		t.Fatalf("notification path=%q want %q", got, want)
	}
	if _, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationSourceKeepsDOMOnlyAccessiblePagination(t *testing.T) {
	source := string(indexPage)
	for _, marker := range []string{"next_before", "Load older", "aria-expanded", "notificationButton.focus()", "min-height: 44px", "replaceChildren()", "notificationRefresh.addEventListener('click', () => refreshNotifications())"} {
		if !strings.Contains(source, marker) {
			t.Fatalf("embedded notification UI missing %q", marker)
		}
	}
	if strings.Contains(source, "notificationList.innerHTML") || strings.Contains(source, "notificationList.innerHTML =") {
		t.Fatal("notification UI must remain DOM-only")
	}
}

func TestNotificationSSEValidatesBeforeHeadersAndEmitsInitialSequence(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	if _, err := service.notifications.EnqueueJobResult(context.Background(), bee.JobResult{JobID: "sse", AttemptID: "sse-1", State: bee.JobSucceeded, FinishedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &notificationCancelWriter{cancel: cancel}
	service.Handler().ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/notifications/events", nil).WithContext(ctx))
	if writer.status != http.StatusOK || !strings.Contains(writer.body.String(), "id: 1") || !strings.Contains(writer.body.String(), `"sequence":1`) {
		t.Fatalf("SSE status=%d body=%q", writer.status, writer.body.String())
	}

	data, err := os.ReadFile(service.notifications.Path())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["checksum"] = "corrupt"
	corrupt, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.notifications.Path(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewRecorder()
	service.Handler().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/notifications/events", nil))
	if bad.Code != http.StatusInternalServerError || strings.HasPrefix(strings.ToLower(bad.Header().Get("Content-Type")), "text/event-stream") {
		t.Fatalf("corrupt SSE response=%d headers=%v body=%q", bad.Code, bad.Header(), bad.Body.String())
	}
}
