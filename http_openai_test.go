package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newOpenAITestHandler(t *testing.T, app *Application, routes RouteBindingStore, maxConcurrent int, heartbeat time.Duration) *OpenAIHTTPHandler {
	t.Helper()
	handler, err := NewOpenAIHTTPHandler(&HarnessHandlers{Application: app}, OpenAIHTTPOptions{
		BearerToken:       "test-token",
		ModelIDs:          []string{"bee-assistant"},
		RouteBindings:     routes,
		MaxConcurrent:     maxConcurrent,
		HeartbeatInterval: heartbeat,
		RequestTimeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func openAITestRequest(t *testing.T, body, conversationID string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	if conversationID != "" {
		request.Header.Set("X-Session-Id", conversationID)
	}
	return request
}

func TestOpenAIHTTPAuthModelsAndHealth(t *testing.T) {
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
	app.SetToolDisplayMode(ToolDisplayOff)
	app.SetThinkingVisible(false)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthorized status=%d headers=%v", unauthorized.Code, unauthorized.Header())
	}

	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer test-token")
	models := httptest.NewRecorder()
	handler.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"bee-assistant"`) {
		t.Fatalf("models status=%d body=%s", models.Code, models.Body.String())
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRequest.Header.Set("Authorization", "Bearer test-token")
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, healthRequest)
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}

	policyRequest := httptest.NewRequest(http.MethodGet, "/api/interaction-policy", nil)
	policyRequest.Header.Set("Authorization", "Bearer test-token")
	policy := httptest.NewRecorder()
	handler.ServeHTTP(policy, policyRequest)
	if policy.Code != http.StatusOK || policy.Header().Get("Cache-Control") != "no-store" || !strings.Contains(policy.Body.String(), `"tool_mode":"off"`) || !strings.Contains(policy.Body.String(), `"thinking":false`) {
		t.Fatalf("policy status=%d headers=%v body=%s", policy.Code, policy.Header(), policy.Body.String())
	}
}

func TestOpenAIHTTPDurableSessionMappingIgnoresReplayedHistory(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, Content: "first answer"}, Usage: Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}},
		{Message: Message{Role: RoleAssistant, Content: "second answer"}, Usage: Usage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11}},
		{Message: Message{Role: RoleAssistant, Content: "other answer"}},
	}}
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
	handler := newOpenAITestHandler(t, app, routes, 2, time.Second)

	firstBody := `{"model":"bee-assistant","messages":[{"role":"system","content":"client system"},{"role":"user","content":"stale user"},{"role":"assistant","content":"stale answer"},{"role":"user","content":"fresh user"}]}`
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, openAITestRequest(t, firstBody, "conversation-A"))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "first answer") {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	firstSession := first.Header().Get("X-Bee-Session-Id")
	if firstSession == "" {
		t.Fatal("first response omitted X-Bee-Session-Id")
	}

	secondBody := `{"model":"bee-assistant","messages":[{"role":"system","content":"client system"},{"role":"user","content":"fresh user"},{"role":"assistant","content":"first answer"},{"role":"user","content":"second user"}]}`
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, openAITestRequest(t, secondBody, "conversation-A"))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "second answer") {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("X-Bee-Session-Id") != firstSession {
		t.Fatalf("sessions differ: first=%q second=%q", firstSession, second.Header().Get("X-Bee-Session-Id"))
	}

	other := httptest.NewRecorder()
	handler.ServeHTTP(other, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"other user"}]}`, "conversation-B"))
	if other.Code != http.StatusOK || other.Header().Get("X-Bee-Session-Id") == firstSession {
		t.Fatalf("other status=%d session=%q first=%q", other.Code, other.Header().Get("X-Bee-Session-Id"), firstSession)
	}

	if len(provider.requests) != 3 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	if messageCount(provider.requests[0].Messages, RoleUser, "stale user") != 0 || messageCount(provider.requests[0].Messages, RoleAssistant, "stale answer") != 0 {
		t.Fatalf("client history was replayed: %#v", provider.requests[0].Messages)
	}
	if messageCount(provider.requests[0].Messages, RoleUser, "fresh user") != 1 {
		t.Fatalf("newest user message missing or duplicated: %#v", provider.requests[0].Messages)
	}
	if messageCount(provider.requests[1].Messages, RoleUser, "fresh user") != 1 || messageCount(provider.requests[1].Messages, RoleAssistant, "first answer") != 1 || messageCount(provider.requests[1].Messages, RoleUser, "second user") != 1 {
		t.Fatalf("durable history was not authoritative: %#v", provider.requests[1].Messages)
	}
	if messageCount(provider.requests[1].Messages, RoleSystem, "client system") != 1 {
		t.Fatalf("ephemeral system prompt missing or duplicated: %#v", provider.requests[1].Messages)
	}
	session, err := store.Load(SessionID(firstSession))
	if err != nil {
		t.Fatal(err)
	}
	durableMessages, err := session.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if messageCount(durableMessages, RoleSystem, "client system") != 0 {
		t.Fatalf("request system prompt leaked into durable history: %#v", durableMessages)
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(second.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage.TotalTokens != 11 || decoded.Model != "bee-assistant" {
		t.Fatalf("response=%#v", decoded)
	}
}

func messageCount(messages []Message, role Role, content string) int {
	count := 0
	for _, message := range messages {
		if message.Role == role && message.Content == content {
			count++
		}
	}
	return count
}

func TestOpenAIHTTPSSEHeartbeatsDuringToolExecutionAndRejectsOverlap(t *testing.T) {
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Name: "wait", Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "final answer"}},
	}}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "wait", Description: "wait in a test", Risk: RiskLow, Mutability: ToolMutabilityReadOnly}, Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(toolStarted)
		select {
		case <-releaseTool:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = &Executor{Registry: registry, Scope: testScope(t), Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyAllow, nil
	})}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newOpenAITestHandler(t, app, routes, 1, 5*time.Millisecond)
	body := `{"model":"bee-assistant","messages":[{"role":"user","content":"use the tool"}],"stream":true}`
	stream := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(stream, openAITestRequest(t, body, "busy-conversation"))
		close(done)
	}()
	select {
	case <-toolStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	time.Sleep(20 * time.Millisecond)

	overlap := httptest.NewRecorder()
	handler.ServeHTTP(overlap, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"overlap"}]}`, "busy-conversation"))
	if overlap.Code != http.StatusConflict {
		t.Fatalf("overlap status=%d body=%s", overlap.Code, overlap.Body.String())
	}
	atCapacity := httptest.NewRecorder()
	handler.ServeHTTP(atCapacity, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"other conversation"}]}`, "other-conversation"))
	if atCapacity.Code != http.StatusTooManyRequests {
		t.Fatalf("capacity status=%d body=%s", atCapacity.Code, atCapacity.Body.String())
	}
	close(releaseTool)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish")
	}
	if stream.Code != http.StatusOK || stream.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status=%d headers=%v", stream.Code, stream.Header())
	}
	output := stream.Body.String()
	for _, expected := range []string{": ping\n\n", `"role":"assistant"`, `"content":"final answer"`, `"finish_reason":"stop"`, "data: [DONE]\n\n"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("stream missing %q: %s", expected, output)
		}
	}
	if strings.Contains(output, "tool_call") || strings.Contains(output, "tool_result") {
		t.Fatalf("internal events leaked into SSE: %s", output)
	}
}

func TestOpenAIHTTPApprovalAPIApprovesAndDeniesPromptedTools(t *testing.T) {
	for _, test := range []struct {
		name         string
		decision     ApprovalDecision
		wantExecuted bool
	}{
		{name: "approve", decision: ApprovalApproved, wantExecuted: true},
		{name: "deny", decision: ApprovalDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			executed := false
			provider := &scriptedToolProvider{responses: []Response{
				{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Name: "mutate", Arguments: `{}`}}}},
				{Message: Message{Role: RoleAssistant, Content: "decision observed"}},
			}}
			registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "mutate", Description: "mutate in a test", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
				executed = true
				return "done", nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewSessionStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			app, err := NewApplication(DefaultConfig(), provider, store)
			if err != nil {
				t.Fatal(err)
			}
			app.Executor = &Executor{Registry: registry, Scope: testScope(t), Policy: GeneralAssistantPolicy{Mode: AssistantPolicyPrompted}}
			routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
			if err != nil {
				t.Fatal(err)
			}
			handler := newOpenAITestHandler(t, app, routes, 1, time.Second)

			unauthorized := httptest.NewRecorder()
			handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/approvals", nil))
			if unauthorized.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized status=%d", unauthorized.Code)
			}

			response := httptest.NewRecorder()
			completed := make(chan struct{})
			go func() {
				handler.ServeHTTP(response, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"mutate"}]}`, "prompted-"+test.name))
				close(completed)
			}()

			var pending PendingApproval
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				request := httptest.NewRequest(http.MethodGet, "/api/approvals", nil)
				request.Header.Set("Authorization", "Bearer test-token")
				listed := httptest.NewRecorder()
				handler.ServeHTTP(listed, request)
				var payload struct {
					Approvals []PendingApproval `json:"approvals"`
				}
				if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Approvals) == 1 {
					pending = payload.Approvals[0]
					break
				}
				time.Sleep(time.Millisecond)
			}
			if pending.ID == "" || pending.RunID == "" || pending.SessionID == "" {
				t.Fatalf("pending approval=%#v", pending)
			}
			decisionRequest := httptest.NewRequest(http.MethodPost, "/api/approvals/"+pending.ID, strings.NewReader(`{"decision":"`+string(test.decision)+`"}`))
			decisionRequest.Header.Set("Authorization", "Bearer test-token")
			decided := httptest.NewRecorder()
			handler.ServeHTTP(decided, decisionRequest)
			if decided.Code != http.StatusAccepted {
				t.Fatalf("decision status=%d body=%s", decided.Code, decided.Body.String())
			}
			select {
			case <-completed:
			case <-time.After(2 * time.Second):
				t.Fatal("chat completion did not resume after approval decision")
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "decision observed") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if executed != test.wantExecuted {
				t.Fatalf("executed=%t want=%t", executed, test.wantExecuted)
			}
		})
	}
}

func TestOpenAIHTTPValidatesModelConversationAndCapacity(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "unused"}}}}
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

	missingConversation := httptest.NewRecorder()
	handler.ServeHTTP(missingConversation, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"hi"}]}`, ""))
	if missingConversation.Code != http.StatusBadRequest {
		t.Fatalf("missing conversation status=%d body=%s", missingConversation.Code, missingConversation.Body.String())
	}
	wrongModel := httptest.NewRecorder()
	handler.ServeHTTP(wrongModel, openAITestRequest(t, `{"model":"other","messages":[{"role":"user","content":"hi"}]}`, "conversation"))
	if wrongModel.Code != http.StatusBadRequest {
		t.Fatalf("wrong model status=%d body=%s", wrongModel.Code, wrongModel.Body.String())
	}

	if _, err := NewOpenAIHTTPHandler(&HarnessHandlers{Application: app}, OpenAIHTTPOptions{BearerToken: "bad token", RouteBindings: routes}); err == nil {
		t.Fatal("whitespace bearer token was accepted")
	}
	if _, err := NewOpenAIHTTPHandler(&HarnessHandlers{Application: app}, OpenAIHTTPOptions{BearerToken: "ok", RouteBindings: routes, MaxConcurrent: MaxOpenAIServeConcurrent + 1}); err == nil {
		t.Fatal("excessive concurrency was accepted")
	}

	oversized := bytes.Repeat([]byte("x"), MaxOpenAIRequestBodyBytes+1)
	request := openAITestRequest(t, string(oversized), "conversation")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized status=%d", response.Code)
	}
}
