package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestApplicationRunEventOrderingAndSessionIdentity(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "hello"}, Usage: Usage{TotalTokens: 3}, Model: "test"}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hi", Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var got []RunEvent
	for event := range events {
		got = append(got, event)
	}
	if len(got) < 7 || got[0].Type != RunStart || got[1].Type != RunSession || got[2].Type != RunContextUsage || got[3].Type != RunAssistantDelta || got[4].Type != RunAssistant || got[5].Type != RunUsage || got[len(got)-1].Type != RunEnd {
		t.Fatalf("events=%#v", got)
	}
	if got[2].ContextUsage == nil || got[2].ContextUsage.PromptUsed <= 0 {
		t.Fatalf("context usage event=%#v", got[2])
	}
	if got[1].SessionID == "" || got[0].SessionID != got[1].SessionID || got[len(got)-1].SessionID != got[1].SessionID {
		t.Fatalf("session ids=%q/%q/%q", got[0].SessionID, got[1].SessionID, got[len(got)-1].SessionID)
	}
	if _, err := store.Load(got[1].SessionID); err != nil {
		t.Fatal(err)
	}
	status := DispatchCommand(context.Background(), "/status", CommandContext{Application: app, SessionID: got[1].SessionID})
	if status.Status == nil || status.Status.RequestContext == nil || status.Status.RequestContext.PromptUsed != got[2].ContextUsage.PromptUsed || status.Status.RecentUsage == nil || status.Status.RecentUsage.TotalTokens != 3 {
		t.Fatalf("status did not retain run telemetry: %#v", status.Status)
	}
	data, err := json.Marshal(got[3])
	if err != nil || !strings.Contains(string(data), `"type":"assistant_delta"`) {
		t.Fatalf("json=%s err=%v", data, err)
	}
}

func TestApplicationToolAndApprovalEvents(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "write", Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "write", Risk: RiskHigh, Description: "write"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: testScope(t), Policy: DefaultCodingPolicy{}, Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalApproved, nil })}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = executor
	events, err := app.Run(context.Background(), RunRequest{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	var types []RunEventType
	for event := range events {
		types = append(types, event.Type)
	}
	joined := strings.Trim(strings.ReplaceAll(strings.TrimSpace(string(mustJSON(types))), `"`, ""), "[]")
	if !strings.Contains(joined, "tool_call") || !strings.Contains(joined, "approval") || !strings.Contains(joined, "tool_result") {
		t.Fatalf("types=%v", types)
	}
}

func TestPrintJSONTUIAndRPCUseSameFacade(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "hello"}}}}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var print bytes.Buffer
	if err := RunPrint(context.Background(), app, RunRequest{Prompt: "hi"}, &print); err != nil || print.String() != "hello" {
		t.Fatalf("print=%q err=%v", print.String(), err)
	}
	provider.responses = []Response{{Message: Message{Role: RoleAssistant, Content: "json"}}}
	var ndjson bytes.Buffer
	if err := RunJSON(context.Background(), app, RunRequest{Prompt: "hi"}, &ndjson); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ndjson.String(), `"type":"assistant_delta"`) || !strings.Contains(ndjson.String(), "\n") {
		t.Fatalf("ndjson=%q", ndjson.String())
	}
	provider.responses = []Response{{Message: Message{Role: RoleAssistant, Content: "tui"}}}
	var tui bytes.Buffer
	if err := RunTUI(context.Background(), app, strings.NewReader("hello\n/exit\n"), &tui); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tui.String(), "tui") || !strings.Contains(tui.String(), "╭─") || !strings.Contains(tui.String(), "›") {
		t.Fatalf("tui=%q", tui.String())
	}
	for _, noisy := range []string{"bee>", "\x1b[90mdone\x1b[0m"} {
		if strings.Contains(tui.String(), noisy) {
			t.Fatalf("tui=%q contains retired transcript marker %q", tui.String(), noisy)
		}
	}
}

type noDeltaProvider struct{}

func (noDeltaProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (noDeltaProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{Message: Message{Role: RoleAssistant, Content: "fallback"}}, nil
}

func TestPrintFallsBackToAssistantMessageWhenProviderEmitsNoDeltas(t *testing.T) {
	app, err := NewApplication(DefaultConfig(), noDeltaProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := RunPrint(context.Background(), app, RunRequest{Prompt: "hi"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "fallback" {
		t.Fatalf("output=%q", output.String())
	}
}
