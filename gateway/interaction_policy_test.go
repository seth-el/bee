package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func TestGatewayRunEventsApplyVisibilityAndDisplayRedaction(t *testing.T) {
	t.Run("hidden", func(t *testing.T) {
		app := &bee.Application{RequirePlan: true}
		app.SetToolDisplayMode(bee.ToolDisplayOff)
		app.SetThinkingVisible(false)
		record := newRunRecord("run-hidden", context.Background(), func() {}, app.NewRunEventDisplayProjector())
		policy := app.InteractionPolicy("")
		record.publish(bee.RunEvent{Type: bee.RunStart, RunID: record.id, InteractionPolicy: &policy})
		record.publish(bee.RunEvent{Type: bee.RunReasoningDelta, RunID: record.id, Reasoning: &bee.ReasoningRunEvent{Delta: "private reasoning"}})
		record.publish(bee.RunEvent{Type: bee.RunToolCall, RunID: record.id, ToolCall: &bee.ToolCall{ID: "call", Name: "exec", Arguments: `{"command":"secret command"}`}})
		record.publish(bee.RunEvent{Type: bee.RunToolResult, RunID: record.id, ToolResult: &bee.ToolResultEvent{CallID: "call", ToolName: "exec", Output: "secret output"}})
		record.publish(bee.RunEvent{Type: bee.RunAssistantDelta, RunID: record.id, Delta: "public answer"})
		record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id})

		body := gatewayRunEventBody(t, app, record)
		for _, hidden := range []string{`"type":"reasoning_delta"`, `"type":"tool_call"`, `"type":"tool_result"`, "private reasoning", "secret command", "secret output"} {
			if strings.Contains(body, hidden) {
				t.Fatalf("hidden run event stream contains %q: %s", hidden, body)
			}
		}
		if !strings.Contains(body, "public answer") || !strings.Contains(body, `"tool_mode":"off"`) || !strings.Contains(body, `"plan_mode":{"enabled":true`) {
			t.Fatalf("run event stream omitted public policy state: %s", body)
		}
	})

	t.Run("full redaction", func(t *testing.T) {
		app := &bee.Application{}
		app.SetToolDisplayMode(bee.ToolDisplayFull)
		record := newRunRecord("run-redacted", context.Background(), func() {}, app.NewRunEventDisplayProjector())
		record.publish(bee.RunEvent{Type: bee.RunStart, RunID: record.id})
		record.publish(bee.RunEvent{Type: bee.RunToolCall, RunID: record.id, ToolCall: &bee.ToolCall{ID: "call", Name: "exec", Arguments: `{"token":"must-not-leak","command":"go test ./..."}`}})
		record.publish(bee.RunEvent{Type: bee.RunToolResult, RunID: record.id, ToolResult: &bee.ToolResultEvent{CallID: "call", ToolName: "exec", Output: "token=must-not-leak"}})
		record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id})

		body := gatewayRunEventBody(t, app, record)
		if strings.Contains(body, "must-not-leak") || !strings.Contains(body, "[REDACTED]") {
			t.Fatalf("tool display projection did not redact the event stream: %s", body)
		}
	})

	t.Run("compact omits bodies", func(t *testing.T) {
		app := &bee.Application{}
		app.SetToolDisplayMode(bee.ToolDisplayCompact)
		record := newRunRecord("run-compact", context.Background(), func() {}, app.NewRunEventDisplayProjector())
		record.publish(bee.RunEvent{Type: bee.RunStart, RunID: record.id})
		record.publish(bee.RunEvent{Type: bee.RunToolCall, RunID: record.id, ToolCall: &bee.ToolCall{ID: "call", Name: "write_file", Arguments: `{"path":"main.go","content":"private file body"}`}})
		record.publish(bee.RunEvent{Type: bee.RunToolResult, RunID: record.id, ToolResult: &bee.ToolResultEvent{CallID: "call", ToolName: "write_file", Output: "private tool result"}})
		record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id})

		body := gatewayRunEventBody(t, app, record)
		if strings.Contains(body, "private file body") || strings.Contains(body, "private tool result") || !strings.Contains(body, "main.go") {
			t.Fatalf("compact projection exposed detail or lost safe metadata: %s", body)
		}
	})
}

func TestGatewayRunEventsSuppressDuplicateApprovalStatuses(t *testing.T) {
	app := &bee.Application{}
	record := newRunRecord("run-approval", context.Background(), func() {}, app.NewRunEventDisplayProjector())
	request := bee.ApprovalRequest{ID: "approval-one", ToolName: "exec", Args: json.RawMessage(`{"token":"must-not-leak"}`)}
	pending := bee.RunEvent{Type: bee.RunApproval, RunID: record.id, Approval: &bee.ApprovalEvent{ID: request.ID, Request: request, Status: bee.ApprovalStatusPending}}
	approved := bee.RunEvent{Type: bee.RunApproval, RunID: record.id, Approval: &bee.ApprovalEvent{ID: request.ID, Request: request, Decision: bee.ApprovalApproved, Status: bee.ApprovalStatusApproved}}
	lateDenied := bee.RunEvent{Type: bee.RunApproval, RunID: record.id, Approval: &bee.ApprovalEvent{ID: request.ID, Request: request, Decision: bee.ApprovalDenied, Status: bee.ApprovalStatusDenied}}
	record.publish(pending)
	record.publish(pending)
	record.publish(approved)
	record.publish(approved)
	record.publish(lateDenied)
	record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id})

	body := gatewayRunEventBody(t, app, record)
	if got := strings.Count(body, `"type":"approval"`); got != 2 {
		t.Fatalf("approval event count=%d, want pending plus one final status: %s", got, body)
	}
	if strings.Count(body, `"status":"pending"`) != 1 || strings.Count(body, `"status":"approved"`) != 1 || strings.Contains(body, `"status":"denied"`) {
		t.Fatalf("approval statuses were not de-duplicated: %s", body)
	}
	if strings.Contains(body, "must-not-leak") {
		t.Fatalf("approval arguments reached SSE: %s", body)
	}
}

func TestGatewayInteractionPolicyEndpoint(t *testing.T) {
	app := &bee.Application{RequirePlan: true}
	app.SetToolDisplayMode(bee.ToolDisplayOff)
	app.SetThinkingVisible(false)
	service := &Service{app: app}
	response := httptest.NewRecorder()
	service.handleInteractionPolicy(response, httptest.NewRequest(http.MethodGet, "/api/interaction-policy", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var state bee.InteractionPolicyState
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Display.ToolCalls || state.Display.Thinking || state.Display.ToolMode != bee.ToolDisplayOff || !state.PlanMode.Enabled || !state.PlanModeAvailable {
		t.Fatalf("interaction policy=%#v", state)
	}
}

func TestGatewayHistoryAppliesCurrentVisibility(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	service.app.SetToolDisplayMode(bee.ToolDisplayOff)
	service.app.SetThinkingVisible(false)
	session, err := service.app.Store.Create(bee.SessionMetadata{Title: "hidden history"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "visible answer", ToolCalls: []bee.ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"history-secret-command"}`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleTool, ToolCallID: "call", Name: "exec", Content: "history-secret-result"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendReasoning(bee.ReasoningEvent{RunID: "run", Step: 1, Content: "history-secret-reasoning", Complete: true}); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"/history", "/history?view=timeline&limit=16"} {
		response := httptest.NewRecorder()
		service.handleSessionHistory(response, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+suffix, nil), session.ID)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", suffix, response.Code, response.Body.String())
		}
		body := response.Body.String()
		for _, hidden := range []string{"history-secret-command", "history-secret-result", "history-secret-reasoning", `"tool_calls"`, `"type":"reasoning"`} {
			if strings.Contains(body, hidden) {
				t.Fatalf("%s history contains %q: %s", suffix, hidden, body)
			}
		}
		if !strings.Contains(body, "visible answer") {
			t.Fatalf("%s history omitted assistant text: %s", suffix, body)
		}
	}
}

func gatewayRunEventBody(t *testing.T, app *bee.Application, record *runRecord) string {
	t.Helper()
	service := &Service{app: app, ctx: context.Background(), runs: map[string]*runRecord{record.id: record}}
	response := httptest.NewRecorder()
	service.handleRunEvents(response, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events", nil), record.id)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	return response.Body.String()
}
