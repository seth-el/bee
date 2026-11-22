package bee

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const openAIWebCommandTestConversation = "web-command-test"

func bindOpenAIWebCommandSession(t *testing.T, store *SessionStore, routes RouteBindingStore) *Session {
	t.Helper()
	session, err := store.Create(SessionMetadata{Title: "command target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := routes.Bind(context.Background(), openAIRouteKey(openAIWebCommandTestConversation), session.ID); err != nil {
		t.Fatal(err)
	}
	return session
}

func authenticatedCommandRequest(body string) *http.Request {
	request := authenticatedSessionRequest(http.MethodPost, "/api/commands", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Id", openAIWebCommandTestConversation)
	return request
}

func TestOpenAIHTTPCommandRunsSharedInformationalCommand(t *testing.T) {
	handler, store, routes, provider := newOpenAISessionTestHandler(t)
	session := bindOpenAIWebCommandSession(t, store, routes)

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/commands", strings.NewReader(`{"name":"status"}`))
	request.Header.Set("X-Session-Id", openAIWebCommandTestConversation)
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	response := httptest.NewRecorder()
	request = authenticatedCommandRequest(`{"name":"status"}`)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var event LocalCommandEvent
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Name != CommandStatus || event.Kind != commandKindStatus || event.Status == nil || event.Status.SessionID != session.ID || event.Status.GatewayReady == nil || !*event.Status.GatewayReady {
		t.Fatalf("event=%#v", event)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("informational command reached provider %d time(s)", len(provider.requests))
	}
}

func TestOpenAIHTTPCommandUnknownAndDisabledAreStructuredErrors(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	bindOpenAIWebCommandSession(t, store, routes)
	for _, name := range []string{"unknown", "stop"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedCommandRequest(`{"name":"`+name+`"}`))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status=%d body=%s", name, response.Code, response.Body.String())
		}
		var event LocalCommandEvent
		if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Name != CommandName(name) || event.Kind != commandKindError || event.Text == "" {
			t.Fatalf("name=%q event=%#v", name, event)
		}
	}
}

func TestOpenAIHTTPCommandMutatesPlanModeThroughSharedGateState(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	session := bindOpenAIWebCommandSession(t, store, routes)
	app := handler.handlers.Application
	app.Executor = testEchoExecutor(t)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedCommandRequest(`{"name":"plan","args":"on"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var event LocalCommandEvent
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Name != CommandPlan || event.Kind != commandKindPlan || event.PlanMode == nil || !event.PlanMode.Enabled || event.PlanMode.ActivePlan {
		t.Fatalf("event=%#v", event)
	}
	state, err := app.PlanMode(session.ID)
	if err != nil || !state.Enabled || state.ActivePlan {
		t.Fatalf("plan state=%#v err=%v", state, err)
	}
	loaded, err := app.Resume(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	options := app.agentOptions(nil, loaded, "")
	if options.Executor == nil || options.Executor.planGate == nil {
		t.Fatalf("plan-enabled session did not install PlanGate: %#v", options.Executor)
	}
	if err := options.Executor.planGate.Check(ToolDefinition{Name: "mutate", Mutability: ToolMutabilityMutating}); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("mutation gate err=%v, want %v", err, ErrPlanRequired)
	}
}

func TestOpenAIHTTPCommandMutatesOwnerDisplaySettings(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	bindOpenAIWebCommandSession(t, store, routes)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedCommandRequest(`{"name":"tools","args":"off"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var event LocalCommandEvent
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	visibility := handler.handlers.Application.DisplayVisibility()
	if event.Name != CommandTools || event.Kind != commandKindDisplay || event.Display == nil || event.Display.ToolCalls || visibility.ToolCalls {
		t.Fatalf("event=%#v visibility=%#v", event, visibility)
	}
}

func TestOpenAIHTTPCommandContinueStartsCheckpointBackedChatTurn(t *testing.T) {
	handler, store, routes, provider := newOpenAISessionTestHandler(t)
	session := bindOpenAIWebCommandSession(t, store, routes)
	checkpoint, err := session.AppendTurnCheckpoint(TurnCheckpoint{
		WhyStopped: TurnStopToolBudget,
		Summary:    "completed bounded work",
		NextAction: "finish the remaining work",
	})
	if err != nil {
		t.Fatal(err)
	}

	commandResponse := httptest.NewRecorder()
	handler.ServeHTTP(commandResponse, authenticatedCommandRequest(`{"name":"continue"}`))
	if commandResponse.Code != http.StatusOK {
		t.Fatalf("command status=%d body=%s", commandResponse.Code, commandResponse.Body.String())
	}
	var event LocalCommandEvent
	if err := json.Unmarshal(commandResponse.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Name != CommandContinue || event.Kind != commandKindContinue || event.Continue == nil || event.Continue.CheckpointID != checkpoint.ID || event.Continue.Prompt == "" {
		t.Fatalf("event=%#v", event)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("command dispatch reached provider %d time(s)", len(provider.requests))
	}

	body, err := json.Marshal(map[string]any{
		"model":         "bee-assistant",
		"messages":      []map[string]string{{"role": "user", "content": event.Continue.Prompt}},
		"checkpoint_id": event.Continue.CheckpointID,
	})
	if err != nil {
		t.Fatal(err)
	}
	chatResponse := httptest.NewRecorder()
	handler.ServeHTTP(chatResponse, openAITestRequest(t, string(body), openAIWebCommandTestConversation))
	if chatResponse.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chatResponse.Code, chatResponse.Body.String())
	}
	if len(provider.requests) != 1 || !requestContainsSystemText(provider.requests[0], "<turn_checkpoint>") || !requestContainsSystemText(provider.requests[0], checkpoint.ID) {
		t.Fatalf("provider requests=%#v", provider.requests)
	}
	reloaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.LatestPendingTurnCheckpoint(); err == nil || !strings.Contains(err.Error(), "no pending") {
		t.Fatalf("checkpoint was not marked resumed: %v", err)
	}
}

func TestOpenAIHTTPCheckpointContinuationRejectsInvalidSelection(t *testing.T) {
	handler, store, routes, provider := newOpenAISessionTestHandler(t)
	bindOpenAIWebCommandSession(t, store, routes)

	for _, checkpointID := range []string{"missing-checkpoint", "bad checkpoint", "bad\x00checkpoint", strings.Repeat("x", 257)} {
		body, err := json.Marshal(map[string]any{
			"model":         "bee-assistant",
			"messages":      []map[string]string{{"role": "user", "content": "continue"}},
			"checkpoint_id": checkpointID,
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, openAITestRequest(t, string(body), openAIWebCommandTestConversation))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("checkpoint=%q status=%d body=%s", checkpointID, response.Code, response.Body.String())
		}
	}
	if len(provider.requests) != 0 {
		t.Fatalf("invalid checkpoint reached provider %d time(s)", len(provider.requests))
	}
}

func TestOpenAIHTTPCommandParsingIsStrictAndBounded(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	bindOpenAIWebCommandSession(t, store, routes)
	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{name: "method", method: http.MethodGet, target: "/api/commands"},
		{name: "query", method: http.MethodPost, target: "/api/commands?x=1", body: `{"name":"status"}`},
		{name: "unknown field", method: http.MethodPost, target: "/api/commands", body: `{"name":"status","extra":true}`},
		{name: "trailing json", method: http.MethodPost, target: "/api/commands", body: `{"name":"status"}{}`},
		{name: "leading slash", method: http.MethodPost, target: "/api/commands", body: `{"name":"/status"}`},
		{name: "oversized", method: http.MethodPost, target: "/api/commands", body: `{"name":"status","args":"` + strings.Repeat("x", MaxOpenAIWebCommandBodyBytes) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := authenticatedSessionRequest(test.method, test.target, test.body)
			request.Header.Set("X-Session-Id", openAIWebCommandTestConversation)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest && response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers=%v", response.Header())
			}
		})
	}
}

func TestOpenAIHTTPCommandRequiresMappedIdleSession(t *testing.T) {
	handler, store, routes, _ := newOpenAISessionTestHandler(t)
	session := bindOpenAIWebCommandSession(t, store, routes)
	lease, err := store.acquireTurnLease(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, authenticatedCommandRequest(`{"name":"status"}`))
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), ErrSessionBusy.Error()) {
		t.Fatalf("busy status=%d body=%s", busy.Code, busy.Body.String())
	}

	unmapped := authenticatedCommandRequest(`{"name":"status"}`)
	unmapped.Header.Set("X-Session-Id", "unmapped-command-session")
	notFound := httptest.NewRecorder()
	handler.ServeHTTP(notFound, unmapped)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unmapped status=%d body=%s", notFound.Code, notFound.Body.String())
	}
}
