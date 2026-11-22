package bee

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenAIWebRunRegistryProjectsContentFreeLifecycle(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	app.SetToolDisplayMode(ToolDisplayFull)
	registry := newOpenAIWebRunRegistry(app)
	runID := "run-lifecycle"
	sessionID := SessionID("session-lifecycle")
	events := []RunEvent{
		{Type: RunStart, RunID: runID, SessionID: sessionID},
		{Type: RunAssistantDelta, RunID: runID, SessionID: sessionID, Delta: "private model text"},
		{Type: RunToolCall, RunID: runID, SessionID: sessionID, ToolCall: &ToolCall{Name: "exec", Arguments: `{"token":"must-not-leak"}`}},
		{Type: RunToolResult, RunID: runID, SessionID: sessionID, ToolResult: &ToolResultEvent{ToolName: "exec", Output: "private result"}},
		{Type: RunApproval, RunID: runID, SessionID: sessionID, Approval: &ApprovalEvent{ID: "approval-1", Status: ApprovalStatusPending, Request: ApprovalRequest{ID: "approval-1", ToolName: "exec", Summary: "private summary", Args: json.RawMessage(`{"secret":true}`)}}},
		{Type: RunApproval, RunID: runID, SessionID: sessionID, Approval: &ApprovalEvent{ID: "approval-1", Status: ApprovalStatusApproved, Decision: ApprovalApproved}},
		{Type: RunEnd, RunID: runID, SessionID: sessionID},
	}
	var projected []openAIWebRunEvent
	for _, event := range events {
		if item, ok := registry.publish("conversation-lifecycle", event); ok {
			projected = append(projected, item)
		}
	}
	if len(projected) != len(events) {
		t.Fatalf("projected events=%d, want %d: %#v", len(projected), len(events), projected)
	}
	if projected[2].State != openAIWebRunRunningTool || projected[2].ToolName != "exec" {
		t.Fatalf("tool projection=%#v", projected[2])
	}
	if projected[4].State != openAIWebRunWaitingForApproval || projected[4].ApprovalStatus != ApprovalStatusPending {
		t.Fatalf("approval projection=%#v", projected[4])
	}
	if projected[len(projected)-1].State != openAIWebRunDone || !projected[len(projected)-1].Terminal {
		t.Fatalf("terminal projection=%#v", projected[len(projected)-1])
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private model text", "must-not-leak", "private result", "private summary", "secret"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("lifecycle projection leaked %q: %s", private, encoded)
		}
	}

	app.SetToolDisplayMode(ToolDisplayOff)
	hiddenRun := "run-hidden"
	if _, ok := registry.publish("conversation-hidden", RunEvent{Type: RunStart, RunID: hiddenRun}); !ok {
		t.Fatal("run start was not retained")
	}
	if _, ok := registry.publish("conversation-hidden", RunEvent{Type: RunToolCall, RunID: hiddenRun, ToolCall: &ToolCall{Name: "exec"}}); ok {
		t.Fatal("hidden tool call crossed lifecycle projection")
	}
}

func TestOpenAIWebRunRegistryIsBoundedWhenAllRetainedRunsAreActive(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	registry := newOpenAIWebRunRegistry(app)
	for index := 0; index < maxOpenAIWebRunRecords; index++ {
		id := fmt.Sprintf("run-%d", index)
		if _, ok := registry.publish(id, RunEvent{Type: RunStart, RunID: id}); !ok {
			t.Fatalf("run %d was not retained", index)
		}
	}
	if _, ok := registry.publish("overflow", RunEvent{Type: RunStart, RunID: "overflow"}); ok {
		t.Fatal("registry retained an unbounded active run")
	}
	if len(registry.order) != maxOpenAIWebRunRecords {
		t.Fatalf("record count=%d, want %d", len(registry.order), maxOpenAIWebRunRecords)
	}
}

func TestOpenAIHTTPRunLifecycleStatusAndReplay(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newOpenAITestHandler(t, app, routes, 2, time.Second)
	handler.runLifecycle.publish("conversation-A", RunEvent{Type: RunStart, RunID: "run-A", SessionID: "session-A"})
	handler.runLifecycle.publish("conversation-A", RunEvent{Type: RunToolCall, RunID: "run-A", SessionID: "session-A", ToolCall: &ToolCall{Name: "read_file", Arguments: `{"path":"private"}`}})
	handler.runLifecycle.publish("conversation-A", RunEvent{Type: RunEnd, RunID: "run-A", SessionID: "session-A"})

	request := httptest.NewRequest(http.MethodGet, "/api/runs/current", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-Session-Id", "conversation-A")
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, request)
	if status.Code != http.StatusOK || status.Header().Get("Cache-Control") != "no-store" || !strings.Contains(status.Body.String(), `"state":"done"`) || strings.Contains(status.Body.String(), "private") {
		t.Fatalf("status=%d headers=%v body=%s", status.Code, status.Header(), status.Body.String())
	}

	eventsRequest := httptest.NewRequest(http.MethodGet, "/api/runs/run-A/events?after=0", nil)
	eventsRequest.Header.Set("Authorization", "Bearer test-token")
	eventsRequest.Header.Set("X-Session-Id", "conversation-A")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, eventsRequest)
	if stream.Code != http.StatusOK || stream.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status=%d headers=%v body=%s", stream.Code, stream.Header(), stream.Body.String())
	}
	for _, expected := range []string{`"state":"running"`, `"state":"running_tool"`, `"tool_name":"read_file"`, `"state":"done"`} {
		if !strings.Contains(stream.Body.String(), expected) {
			t.Fatalf("stream missing %q: %s", expected, stream.Body.String())
		}
	}
	if strings.Contains(stream.Body.String(), "private") {
		t.Fatalf("stream leaked private content: %s", stream.Body.String())
	}

	wrongConversation := httptest.NewRequest(http.MethodGet, "/api/runs/run-A/events", nil)
	wrongConversation.Header.Set("Authorization", "Bearer test-token")
	wrongConversation.Header.Set("X-Session-Id", "conversation-B")
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, wrongConversation)
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong-conversation status=%d body=%s", wrong.Code, wrong.Body.String())
	}

	invalidQuery := httptest.NewRequest(http.MethodGet, "/api/runs/run-A/events?after=", nil)
	invalidQuery.Header.Set("Authorization", "Bearer test-token")
	invalidQuery.Header.Set("X-Session-Id", "conversation-A")
	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, invalidQuery)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid-query status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	malformedQuery := httptest.NewRequest(http.MethodGet, "/api/runs/run-A/events", nil)
	malformedQuery.URL.RawQuery = "after=%zz"
	malformedQuery.Header.Set("Authorization", "Bearer test-token")
	malformedQuery.Header.Set("X-Session-Id", "conversation-A")
	malformed := httptest.NewRecorder()
	handler.ServeHTTP(malformed, malformedQuery)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed-query status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

type detachedLifecycleProvider struct {
	started chan struct{}
	release chan struct{}
}

func (provider *detachedLifecycleProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *detachedLifecycleProvider) Stream(ctx context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	close(provider.started)
	select {
	case <-provider.release:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	if onDelta != nil {
		if err := onDelta("finished after disconnect"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "finished after disconnect"}}, nil
}

func TestOpenAIHTTPLifecycleOptInSurvivesTransportDisconnect(t *testing.T) {
	provider := &detachedLifecycleProvider{started: make(chan struct{}), release: make(chan struct{})}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newOpenAITestHandler(t, app, routes, 1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	request := openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"continue in the background"}],"stream":true}`, "recoverable-conversation").WithContext(ctx)
	request.Header.Set(openAIWebRunLifecycleHeader, "1")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("detached lifecycle request ended when its transport disconnected")
	case <-time.After(25 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("detached lifecycle request did not finish")
	}
	status, ok := handler.runLifecycle.current("recoverable-conversation")
	if !ok || status.State != openAIWebRunDone || !status.Terminal {
		t.Fatalf("recovered status=%#v ok=%v", status, ok)
	}
	if !strings.Contains(response.Body.String(), `: bee-run {"cursor":1`) {
		t.Fatalf("opt-in stream omitted lifecycle comment: %s", response.Body.String())
	}
}

func TestOpenAIHTTPLifecycleTimeoutPublishesTerminalFailure(t *testing.T) {
	provider := &detachedLifecycleProvider{started: make(chan struct{}), release: make(chan struct{})}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newOpenAITestHandler(t, app, routes, 1, time.Second)
	handler.requestTimeout = 250 * time.Millisecond
	request := openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"time out"}],"stream":true}`, "timeout-conversation")
	request.Header.Set(openAIWebRunLifecycleHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	status, ok := handler.runLifecycle.current("timeout-conversation")
	if !ok || status.State != openAIWebRunFailed || !status.Terminal {
		t.Fatalf("timeout status=%#v ok=%v", status, ok)
	}
}

func TestOpenAIWebRunLifecycleCarriesModelCallPerformance(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	registry := newOpenAIWebRunRegistry(app)
	runID, sessionID := "run-perf", SessionID("session-perf")
	if _, ok := registry.publish("conversation-perf", RunEvent{Type: RunStart, RunID: runID, SessionID: sessionID}); !ok {
		t.Fatal("start not published")
	}
	usage := &UsageEvent{Provider: "local", Model: "private-model-name", Usage: Usage{PromptTokens: 5000, CompletionTokens: 80, CacheReadTokens: 4000},
		Timing: &ProviderCallTiming{DurationMs: 3200, TimeToFirstTokenMs: 1200, DecodeTokensPerSecond: 40, ContextUsedTokens: 5000, ContextWindowTokens: 32768}}
	item, ok := registry.publish("conversation-perf", RunEvent{Type: RunUsage, RunID: runID, SessionID: sessionID, Usage: usage})
	if !ok || item.State != openAIWebRunRunning || item.Performance == nil {
		t.Fatalf("usage record = %#v, %v", item, ok)
	}
	want := openAIWebRunPerformance{PromptTokens: 5000, CompletionTokens: 80, CachedTokens: 4000, DurationMs: 3200, TimeToFirstTokenMs: 1200, DecodeTokensPerSecond: 40, ContextUsedTokens: 5000, ContextWindowTokens: 32768}
	if *item.Performance != want {
		t.Fatalf("performance = %#v", item.Performance)
	}
	encoded, _ := json.Marshal(item)
	if strings.Contains(string(encoded), "private-model-name") {
		t.Fatalf("lifecycle leaked model name: %s", encoded)
	}
	if _, ok := registry.publish("conversation-perf", RunEvent{Type: RunUsage, RunID: runID, SessionID: sessionID}); ok {
		t.Fatal("usage without numbers was published")
	}
}
