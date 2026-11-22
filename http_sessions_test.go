package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func newOpenAISessionTestHandler(t *testing.T) (*OpenAIHTTPHandler, *SessionStore, *LocalRouteBindingStore, *scriptedToolProvider) {
	t.Helper()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, Content: "first session reply"}},
		{Message: Message{Role: RoleAssistant, Content: "second session reply"}},
	}}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newOpenAITestHandler(t, app, routes, 2, time.Second), store, routes, provider
}

func authenticatedSessionRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	return request
}

func TestOpenAIHTTPSessionListIsAuthenticatedAndRedacted(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	projectRoot := t.TempDir()
	session, err := store.Create(SessionMetadata{Title: "visible title", Pinned: true, ProjectID: "project-1", ProjectRoot: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	const privateContent = "private-session-content-must-not-cross-the-list-boundary"
	if err := session.AppendMessage(Message{Role: RoleUser, Content: privateContent}); err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if strings.Contains(response.Body.String(), privateContent) {
		t.Fatalf("session content leaked into summary list: %s", response.Body.String())
	}
	var summaries []SessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries=%#v", summaries)
	}
	summary := summaries[0]
	if summary.ID != session.ID || summary.Title != "visible title" || !summary.Pinned || summary.ProjectID != "project-1" || summary.ProjectRoot != projectRoot || summary.EventCount != 3 || summary.ParentID != "" || summary.CreatedAt.IsZero() || summary.UpdatedAt.IsZero() {
		t.Fatalf("summary=%#v", summary)
	}
}

func TestOpenAIHTTPSessionListIsBoundedAndStrict(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	for index := 0; index < MaxOpenAIWebSessionSummaries+1; index++ {
		if _, err := store.Create(SessionMetadata{Title: "bounded"}); err != nil {
			t.Fatal(err)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedSessionRequest(http.MethodGet, "/api/sessions", ""))
	if response.Code != http.StatusOK || response.Header().Get("X-Bee-Sessions-Truncated") != "true" {
		t.Fatalf("bounded list status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var summaries []SessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != MaxOpenAIWebSessionSummaries {
		t.Fatalf("summary count=%d want=%d", len(summaries), MaxOpenAIWebSessionSummaries)
	}

	for _, target := range []string{"/api/sessions?limit=1", "/api/sessions"} {
		body := ""
		if target == "/api/sessions" {
			body = `{}`
		}
		invalid := httptest.NewRecorder()
		handler.ServeHTTP(invalid, authenticatedSessionRequest(http.MethodGet, target, body))
		if invalid.Code != http.StatusBadRequest || invalid.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid list target=%q status=%d headers=%v body=%s", target, invalid.Code, invalid.Header(), invalid.Body.String())
		}
	}
}

func TestOpenAIHTTPSessionSearchIsBoundedStrictAndDisplayProjected(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	visible, err := store.Create(SessionMetadata{Title: "Visible search result", Pinned: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := visible.AppendMessage(Message{Role: RoleUser, Content: "token=raw-search-secret Find the Honeycomb rollout."}); err != nil {
		t.Fatal(err)
	}
	if err := visible.AppendMessage(Message{Role: RoleAssistant, Content: "The HONEYCOMB rollout is ready."}); err != nil {
		t.Fatal(err)
	}
	if err := visible.AppendToolResult(ToolResultEvent{ToolName: "deploy", Output: "private Honeycomb tool output"}); err != nil {
		t.Fatal(err)
	}
	toolOnly, err := store.Create(SessionMetadata{Title: "Tool-only match"})
	if err != nil {
		t.Fatal(err)
	}
	if err := toolOnly.AppendToolResult(ToolResultEvent{ToolName: "inspect", Output: "Honeycomb must stay private"}); err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/sessions?q=honeycomb", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedSessionRequest(http.MethodGet, "/api/sessions?q=hOnEyCoMb", ""))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("search status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, hidden := range []string{"raw-search-secret", "private Honeycomb tool output", "Honeycomb must stay private", string(toolOnly.ID)} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Fatalf("search exposed %q: %s", hidden, response.Body.String())
		}
	}
	var results []SessionSearchResult
	if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != visible.ID || len(results[0].Matches) != 2 {
		t.Fatalf("results=%#v", results)
	}
	if results[0].Matches[0].Role != RoleUser || !strings.Contains(results[0].Matches[0].Snippet, "[REDACTED]") || results[0].Matches[1].Role != RoleAssistant {
		t.Fatalf("matches=%#v", results[0].Matches)
	}
	titleOnly, err := store.Create(SessionMetadata{Title: "Codex title match"})
	if err != nil {
		t.Fatal(err)
	}
	titleResponse := httptest.NewRecorder()
	handler.ServeHTTP(titleResponse, authenticatedSessionRequest(http.MethodGet, "/api/sessions?q=codex", ""))
	results = nil
	if err := json.Unmarshal(titleResponse.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if titleResponse.Code != http.StatusOK || len(results) != 1 || results[0].ID != titleOnly.ID || len(results[0].Matches) != 0 {
		t.Fatalf("title search status=%d results=%#v body=%s", titleResponse.Code, results, titleResponse.Body.String())
	}

	noResults := httptest.NewRecorder()
	handler.ServeHTTP(noResults, authenticatedSessionRequest(http.MethodGet, "/api/sessions?q=absent", ""))
	if noResults.Code != http.StatusOK || strings.TrimSpace(noResults.Body.String()) != "[]" {
		t.Fatalf("no-results status=%d body=%s", noResults.Code, noResults.Body.String())
	}

	for index := 0; index <= MaxSessionSearchResults; index++ {
		session, createErr := store.Create(SessionMetadata{Title: "Capped search result"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if appendErr := session.AppendMessage(Message{Role: RoleUser, Content: "bounded-search-cap"}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	capped := httptest.NewRecorder()
	handler.ServeHTTP(capped, authenticatedSessionRequest(http.MethodGet, "/api/sessions?q=bounded-search-cap", ""))
	if capped.Code != http.StatusOK {
		t.Fatalf("capped search status=%d body=%s", capped.Code, capped.Body.String())
	}
	results = nil
	if err := json.Unmarshal(capped.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != MaxSessionSearchResults {
		t.Fatalf("capped result count=%d want=%d", len(results), MaxSessionSearchResults)
	}
}

func TestOpenAIHTTPSessionSearchRejectsInvalidQueries(t *testing.T) {
	handler, _, _, _ := newOpenAISessionTestHandler(t)
	tests := []struct {
		target string
		body   string
	}{
		{target: "/api/sessions?q="},
		{target: "/api/sessions?q=%20%20"},
		{target: "/api/sessions?limit=1"},
		{target: "/api/sessions?q=one&q=two"},
		{target: "/api/sessions?q=%zz"},
		{target: "/api/sessions?q=%ff"},
		{target: "/api/sessions?q=" + strings.Repeat("x", MaxSessionSearchQueryBytes+1)},
		{target: "/api/sessions?q=valid", body: `{}`},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedSessionRequest(http.MethodGet, test.target, test.body))
		if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("target=%q status=%d headers=%v body=%s", test.target, response.Code, response.Header(), response.Body.String())
		}
	}
}

func TestOpenAIHTTPWebSessionHistoryIsAuthenticatedBoundedAndDisplayProjected(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	session, err := store.Create(SessionMetadata{Title: "history target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleSystem, Content: "private-system-prompt"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "token=raw-user-token\nshow the history"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "visible assistant answer", ToolCalls: []ToolCall{{Name: "exec", Arguments: `{"password":"raw-tool-secret"}`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleTool, Content: "private-tool-result"}); err != nil {
		t.Fatal(err)
	}
	messageTimes := make([]time.Time, 0, 2)
	for _, event := range session.EventsSnapshot() {
		if event.Type != EventMessage {
			continue
		}
		payload, decodeErr := DecodeMessageEvent(event)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if payload.Message.Role == RoleUser || payload.Message.Role == RoleAssistant {
			messageTimes = append(messageTimes, event.At)
		}
	}

	target := "/api/sessions/" + string(session.ID) + "/history"
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, target, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedSessionRequest(http.MethodGet, target, ""))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("history status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, hidden := range []string{"private-system-prompt", "raw-user-token", "raw-tool-secret", "private-tool-result", "tool_calls", "attachments"} {
		if strings.Contains(response.Body.String(), hidden) {
			t.Fatalf("history exposed %q: %s", hidden, response.Body.String())
		}
	}
	var history openAIWebSessionHistory
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.SessionID != session.ID || history.HasMore || len(history.Messages) != 2 {
		t.Fatalf("history=%#v", history)
	}
	if history.Messages[0].Role != RoleUser || !strings.Contains(history.Messages[0].Content, "[REDACTED]") || history.Messages[1].Role != RoleAssistant || history.Messages[1].Content != "visible assistant answer" {
		t.Fatalf("messages=%#v", history.Messages)
	}
	if len(messageTimes) != len(history.Messages) {
		t.Fatalf("message times=%#v messages=%#v", messageTimes, history.Messages)
	}
	for index, message := range history.Messages {
		if !message.CreatedAt.Equal(messageTimes[index]) {
			t.Fatalf("message[%d].created_at=%s want=%s", index, message.CreatedAt, messageTimes[index])
		}
	}
}

func TestOpenAIHTTPWebSessionHistoryKeepsLatestMessagesWithinByteBounds(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	session, err := store.Create(SessionMetadata{Title: "bounded history"})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= MaxOpenAIWebHistoryMessages; index++ {
		if err := session.AppendMessage(Message{Role: RoleUser, Content: fmt.Sprintf("message-%03d", index)}); err != nil {
			t.Fatal(err)
		}
	}
	oversized := strings.Repeat("界", MaxOpenAIWebHistoryMessageBytes)
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: oversized, Incomplete: true}); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	target := "/api/sessions/" + string(session.ID) + "/history"
	handler.ServeHTTP(response, authenticatedSessionRequest(http.MethodGet, target, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", response.Code, response.Body.String())
	}
	var history openAIWebSessionHistory
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if !history.HasMore || len(history.Messages) != MaxOpenAIWebHistoryMessages {
		t.Fatalf("bounded history count=%d has_more=%v", len(history.Messages), history.HasMore)
	}
	last := history.Messages[len(history.Messages)-1]
	if last.Role != RoleAssistant || !last.Incomplete || !last.ContentTruncated || len(last.Content) > MaxOpenAIWebHistoryMessageBytes || !utf8.ValidString(last.Content) {
		t.Fatalf("last message=%#v bytes=%d valid=%v", last, len(last.Content), utf8.ValidString(last.Content))
	}
}

func TestOpenAIHTTPWebSessionHistoryIsStrict(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	session, err := store.Create(SessionMetadata{Title: "strict history"})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/sessions/" + string(session.ID) + "/history"
	for _, test := range []struct {
		name   string
		method string
		target string
		body   string
		status int
	}{
		{name: "method", method: http.MethodPost, target: base, status: http.StatusMethodNotAllowed},
		{name: "query", method: http.MethodGet, target: base + "?limit=1", status: http.StatusBadRequest},
		{name: "body", method: http.MethodGet, target: base, body: `{}`, status: http.StatusBadRequest},
		{name: "missing", method: http.MethodGet, target: "/api/sessions/0123456789abcdef0123456789abcdef/history", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedSessionRequest(test.method, test.target, test.body))
			if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestOpenAIHTTPWebSessionCreateBindsClientIdentityAndPreservesChatAutoCreate(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	const conversationID = "3d9d0fd8-5cb8-4ac4-916d-a5851c3f5eef"
	createRequest := authenticatedSessionRequest(http.MethodPost, "/api/sessions", `{}`)
	createRequest.Header.Set("X-Session-Id", conversationID)
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, createRequest)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status=%d headers=%v body=%s", created.Code, created.Header(), created.Body.String())
	}
	var binding openAIWebSessionBinding
	if err := json.Unmarshal(created.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.SessionID == "" || binding.ConversationID != conversationID {
		t.Fatalf("binding=%#v", binding)
	}
	mapped, err := routes.Lookup(context.Background(), openAIRouteKey(conversationID))
	if err != nil || mapped != binding.SessionID {
		t.Fatalf("mapped=%q err=%v want=%q", mapped, err, binding.SessionID)
	}
	session, err := store.Load(binding.SessionID)
	if err != nil || session.Metadata.Title != "openai:"+conversationID {
		t.Fatalf("session=%#v err=%v", session, err)
	}

	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"created conversation"}]}`, conversationID))
	if chat.Code != http.StatusOK || chat.Header().Get("X-Bee-Session-Id") != string(binding.SessionID) {
		t.Fatalf("chat status=%d session=%q body=%s", chat.Code, chat.Header().Get("X-Bee-Session-Id"), chat.Body.String())
	}

	duplicateRequest := authenticatedSessionRequest(http.MethodPost, "/api/sessions", `{}`)
	duplicateRequest.Header.Set("X-Session-Id", conversationID)
	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, duplicateRequest)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate create status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	const unmappedConversation = "5dde6299-0b23-48ff-bab3-a42bf9505ce5"
	autoCreated := httptest.NewRecorder()
	handler.ServeHTTP(autoCreated, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"auto create"}]}`, unmappedConversation))
	if autoCreated.Code != http.StatusOK || autoCreated.Header().Get("X-Bee-Session-Id") == "" || autoCreated.Header().Get("X-Bee-Session-Id") == string(binding.SessionID) {
		t.Fatalf("auto-create status=%d session=%q body=%s", autoCreated.Code, autoCreated.Header().Get("X-Bee-Session-Id"), autoCreated.Body.String())
	}
}

func TestOpenAIHTTPWebSessionCreateRequiresClientIdentityAndStrictBody(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	for _, test := range []struct {
		name   string
		target string
		body   string
		header string
	}{
		{name: "missing header", target: "/api/sessions", body: `{}`},
		{name: "unknown field", target: "/api/sessions", body: `{"title":"not accepted"}`, header: "client-id"},
		{name: "missing body", target: "/api/sessions", header: "client-id"},
		{name: "trailing JSON", target: "/api/sessions", body: `{} {}`, header: "client-id"},
		{name: "query", target: "/api/sessions?new=true", body: `{}`, header: "client-id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := authenticatedSessionRequest(http.MethodPost, test.target, test.body)
			if test.header != "" {
				request.Header.Set("X-Session-Id", test.header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
	summaries, err := store.List()
	if err != nil || len(summaries) != 0 {
		t.Fatalf("invalid creation mutated sessions: summaries=%#v err=%v", summaries, err)
	}

	missingHeaderChat := httptest.NewRecorder()
	handler.ServeHTTP(missingHeaderChat, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"no identity"}]}`, ""))
	if missingHeaderChat.Code != http.StatusBadRequest {
		t.Fatalf("missing-header chat status=%d body=%s", missingHeaderChat.Code, missingHeaderChat.Body.String())
	}
}

func TestOpenAIHTTPWebSessionSelectBindsCanonicalAliasAndResumesDurableSession(t *testing.T) {
	handler, store, routes, provider := newOpenAISessionTestHandler(t)
	target, err := store.Create(SessionMetadata{Title: "existing session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AppendMessage(Message{Role: RoleUser, Content: "prior durable message"}); err != nil {
		t.Fatal(err)
	}

	selectTarget := func() *httptest.ResponseRecorder {
		request := authenticatedSessionRequest(http.MethodPost, "/api/sessions/"+string(target.ID)+"/select", `{}`)
		request.Header.Set("X-Session-Id", string(target.ID))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	selected := selectTarget()
	if selected.Code != http.StatusOK || selected.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("select status=%d headers=%v body=%s", selected.Code, selected.Header(), selected.Body.String())
	}
	var binding openAIWebSessionBinding
	if err := json.Unmarshal(selected.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.SessionID != target.ID || binding.ConversationID != string(target.ID) {
		t.Fatalf("binding=%#v target=%q", binding, target.ID)
	}
	if mapped, err := routes.Lookup(context.Background(), openAIRouteKey(string(target.ID))); err != nil || mapped != target.ID {
		t.Fatalf("mapped=%q err=%v want=%q", mapped, err, target.ID)
	}
	if repeated := selectTarget(); repeated.Code != http.StatusOK {
		t.Fatalf("idempotent select status=%d body=%s", repeated.Code, repeated.Body.String())
	}

	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"continue here"}]}`, string(target.ID)))
	if chat.Code != http.StatusOK || chat.Header().Get("X-Bee-Session-Id") != string(target.ID) {
		t.Fatalf("chat status=%d session=%q body=%s", chat.Code, chat.Header().Get("X-Bee-Session-Id"), chat.Body.String())
	}
	if len(provider.requests) != 1 || messageCount(provider.requests[0].Messages, RoleUser, "prior durable message") != 1 || messageCount(provider.requests[0].Messages, RoleUser, "continue here") != 1 {
		t.Fatalf("provider requests=%#v", provider.requests)
	}
}

func TestOpenAIHTTPWebSessionSelectReportsBusyWithoutBinding(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	target, err := store.Create(SessionMetadata{Title: "busy target"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.acquireTurnLease(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	request := authenticatedSessionRequest(http.MethodPost, "/api/sessions/"+string(target.ID)+"/select", `{}`)
	request.Header.Set("X-Session-Id", string(target.ID))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), ErrSessionBusy.Error()) {
		t.Fatalf("busy select status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := routes.Lookup(context.Background(), openAIRouteKey(string(target.ID))); !errors.Is(err, ErrRouteBindingNotFound) {
		t.Fatalf("busy selection created route binding: %v", err)
	}
	if _, err := store.Load(target.ID); err != nil {
		t.Fatalf("busy selection changed target session: %v", err)
	}
}

func TestOpenAIHTTPWebSessionSelectNeverOverwritesConflictingAlias(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	target, err := store.Create(SessionMetadata{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(SessionMetadata{Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	key := openAIRouteKey(string(target.ID))
	if err := routes.Bind(context.Background(), key, other.ID); err != nil {
		t.Fatal(err)
	}

	request := authenticatedSessionRequest(http.MethodPost, "/api/sessions/"+string(target.ID)+"/select", `{}`)
	request.Header.Set("X-Session-Id", string(target.ID))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("conflicting select status=%d body=%s", response.Code, response.Body.String())
	}
	mapped, err := routes.Lookup(context.Background(), key)
	if err != nil || mapped != other.ID {
		t.Fatalf("conflicting select changed mapping to %q: err=%v want=%q", mapped, err, other.ID)
	}
}

func TestOpenAIHTTPWebSessionSelectRequiresCanonicalAlias(t *testing.T) {
	handler, store, _, _ := newOpenAISessionTestHandler(t)
	target, err := store.Create(SessionMetadata{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		target string
		body   string
		header string
		status int
	}{
		{name: "missing header", target: "/api/sessions/" + string(target.ID) + "/select", body: `{}`, status: http.StatusBadRequest},
		{name: "different alias", target: "/api/sessions/" + string(target.ID) + "/select", body: `{}`, header: "different", status: http.StatusBadRequest},
		{name: "unknown field", target: "/api/sessions/" + string(target.ID) + "/select", body: `{"force":true}`, header: string(target.ID), status: http.StatusBadRequest},
		{name: "query", target: "/api/sessions/" + string(target.ID) + "/select?force=true", body: `{}`, header: string(target.ID), status: http.StatusBadRequest},
		{name: "missing session", target: "/api/sessions/0123456789abcdef0123456789abcdef/select", body: `{}`, header: "0123456789abcdef0123456789abcdef", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := authenticatedSessionRequest(http.MethodPost, test.target, test.body)
			if test.header != "" {
				request.Header.Set("X-Session-Id", test.header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}
