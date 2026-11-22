package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type scriptedBatch struct {
	calls []ToolCall
}

type scriptedBatchProvider struct {
	batches []scriptedBatch
	calls   int
}

func (provider *scriptedBatchProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *scriptedBatchProvider) Stream(_ context.Context, _ Request, _ StreamFunc) (Response, error) {
	provider.calls++
	if len(provider.batches) == 0 {
		return Response{Message: Message{Role: RoleAssistant, Content: "done"}}, nil
	}
	batch := provider.batches[0]
	provider.batches = provider.batches[1:]
	return Response{Message: Message{Role: RoleAssistant, ToolCalls: batch.calls}}, nil
}

func unlimitedLoopConfig() Config {
	config := DefaultConfig()
	config.Agent.MaxToolSteps = 0
	config.Agent.maxToolStepsExplicit = true
	return config
}

func countedEchoExecutor(t *testing.T, output func(int, string) string) (*Executor, *int) {
	t.Helper()
	count := 0
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "echo", Description: "echo"}, Fn: func(_ context.Context, args json.RawMessage) (string, error) {
		count++
		return output(count, string(args)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}, &count
}

func TestAgentStallsAfterThreeIdenticalCompletedBatches(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "id-1", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "id-2", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "id-3", Type: "function", Name: "echo", Arguments: `{}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string { return "same" })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "repeat", nil)
	if err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 3 || provider.calls != 4 {
		t.Fatalf("executor=%d provider=%d, want 3 executions plus finalizer", *count, provider.calls)
	}
}

func TestAgentStallsBeforeFiniteToolStepLimit(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "id-1", Type: "function", Name: PlanToolName, Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "id-2", Type: "function", Name: PlanToolName, Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "id-3", Type: "function", Name: PlanToolName, Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "id-4", Type: "function", Name: PlanToolName, Arguments: `{}`}}},
	}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), RequirePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "make a change", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if provider.calls != 4 {
		t.Fatalf("provider calls=%d, want 3 work calls plus finalizer", provider.calls)
	}
}

func TestAgentDoesNotExecuteUnexposedPlanToolInternally(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{{calls: []ToolCall{{
		ID: "hidden-plan", Type: "function", Name: PlanToolName, Arguments: validPlanArguments(),
	}}}}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "do not plan", nil); err != nil {
		t.Fatal(err)
	}
	if agent.plan != nil {
		t.Fatalf("unexposed %s mutated plan state: %#v", PlanToolName, agent.plan)
	}
	if len(agent.messages) < 2 || !strings.Contains(agent.messages[len(agent.messages)-2].Content, "not registered") {
		t.Fatalf("hidden plan tool result = %#v, want executor rejection", agent.messages)
	}
}

func TestAgentStallSignatureResetsForChangedArguments(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "2", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "3", Type: "function", Name: "echo", Arguments: `{"x":1}`}}},
		{calls: []ToolCall{{ID: "4", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "5", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "6", Type: "function", Name: "echo", Arguments: `{}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string { return "same" })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "repeat", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 6 {
		t.Fatalf("executor=%d, want 6 after reset", *count)
	}
}

func TestAgentStopsRewordedPlanEvidenceFailureLoop(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-loop"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "c1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	invalidUpdate := func(reference string) ToolCall {
		return ToolCall{ID: "update-" + reference, Type: "function", Name: PlanUpdateToolName, Arguments: fmt.Sprintf(`{"plan_id":"tracked-plan","criteria":[{"id":"c1","state":"passed","evidence":[{"source":"exec","reference":%q,"provenance":"observed"}]}]}`, reference)}
	}
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{invalidUpdate("first-description")}},
		{calls: []ToolCall{{ID: "status-1", Type: "function", Name: PlanStatusToolName, Arguments: `{}`}}},
		{calls: []ToolCall{invalidUpdate("second-description")}},
		{calls: []ToolCall{{ID: "status-2", Type: "function", Name: PlanStatusToolName, Arguments: `{}`}}},
		{calls: []ToolCall{invalidUpdate("third-description")}},
		{calls: []ToolCall{{ID: "should-not-run", Type: "function", Name: PlanStatusToolName, Arguments: `{}`}}},
	}}
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), RequirePlan: true, ExposePlanTracking: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "finish the plan", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if provider.calls != 6 {
		t.Fatalf("provider calls=%d, want 5 work calls plus finalizer", provider.calls)
	}
	messages := agent.Messages()
	foundRecovery := false
	for _, message := range messages {
		foundRecovery = foundRecovery || strings.Contains(message.Content, "recovery: call plan_status")
	}
	if !foundRecovery {
		t.Fatalf("tool results lack actionable recovery: %#v", messages)
	}
}

func TestPlanEvidenceFailureGuardResetsOnlyOnMeaningfulSuccess(t *testing.T) {
	failure := ToolResult{ToolName: PlanUpdateToolName, Error: `invalid plan update: acceptance criterion "c1" evidence: observed acceptance evidence source "exec" is unsupported`}
	count := nextPlanEvidenceFailureCount(1, []ToolCall{{Name: PlanStatusToolName}, {Name: PlanUpdateToolName}}, []ToolResult{{ToolName: PlanStatusToolName, Output: `{}`}, failure})
	if count != 2 {
		t.Fatalf("bookkeeping reset failure count=%d, want 2", count)
	}
	count = nextPlanEvidenceFailureCount(count, []ToolCall{{Name: "exec"}}, []ToolResult{{ToolName: "exec", Output: "verified"}})
	if count != 0 {
		t.Fatalf("meaningful success left failure count=%d", count)
	}
}

func TestAgentStallSignatureResetsForChangedOutputs(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "2", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "3", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "4", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "5", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "6", Type: "function", Name: "echo", Arguments: `{}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(call int, _ string) string {
		if call == 3 {
			return "changed"
		}
		return "same"
	})
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "repeat", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 6 {
		t.Fatalf("executor=%d, want 6 after output reset", *count)
	}
}

func TestAgentStallCanonicalizesArgumentObjectKeyOrder(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1", Type: "function", Name: "echo", Arguments: `{"a":1,"b":2}`}}},
		{calls: []ToolCall{{ID: "2", Type: "function", Name: "echo", Arguments: `{"b":2,"a":1}`}}},
		{calls: []ToolCall{{ID: "3", Type: "function", Name: "echo", Arguments: `{"a":1,"b":2}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string { return "same" })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "repeat", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 3 {
		t.Fatalf("executor=%d, want 3", *count)
	}
}

func TestAgentStallSignaturePreservesMultiCallOrder(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1a", Type: "function", Name: "echo", Arguments: `{"n":1}`}, {ID: "1b", Type: "function", Name: "echo", Arguments: `{"n":2}`}}},
		{calls: []ToolCall{{ID: "2a", Type: "function", Name: "echo", Arguments: `{"n":2}`}, {ID: "2b", Type: "function", Name: "echo", Arguments: `{"n":1}`}}},
		{calls: []ToolCall{{ID: "3a", Type: "function", Name: "echo", Arguments: `{"n":1}`}, {ID: "3b", Type: "function", Name: "echo", Arguments: `{"n":2}`}}},
		{calls: []ToolCall{{ID: "4a", Type: "function", Name: "echo", Arguments: `{"n":1}`}, {ID: "4b", Type: "function", Name: "echo", Arguments: `{"n":2}`}}},
		{calls: []ToolCall{{ID: "5a", Type: "function", Name: "echo", Arguments: `{"n":1}`}, {ID: "5b", Type: "function", Name: "echo", Arguments: `{"n":2}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(_ int, args string) string { return args })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "repeat", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 10 {
		t.Fatalf("executor=%d, want 10", *count)
	}
}

func TestAgentContextCancellationWinsOverStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &scriptedBatchProvider{batches: []scriptedBatch{{calls: []ToolCall{{ID: "1", Type: "function", Name: "echo", Arguments: `{}`}}}}}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string {
		cancel()
		return "same"
	})
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(ctx, "cancel", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if *count != 1 {
		t.Fatalf("executor=%d, want 1", *count)
	}
}

func TestAgentUnlimitedNonRepeatingLoopExceedsThirtyTwoSteps(t *testing.T) {
	batches := make([]scriptedBatch, 40)
	for index := range batches {
		batches[index] = scriptedBatch{calls: []ToolCall{{ID: fmt.Sprintf("%d", index), Type: "function", Name: "echo", Arguments: fmt.Sprintf(`{"step":%d}`, index)}}}
	}
	provider := &scriptedBatchProvider{batches: batches}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string { return "same" })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "progress", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if *count != 40 || provider.calls != 41 {
		t.Fatalf("executor=%d provider=%d, want 40 and 41", *count, provider.calls)
	}
}

func TestAgentPositiveToolLoopLimitRemainsBoundedBeforeStallGuard(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "2", Type: "function", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "3", Type: "function", Name: "echo", Arguments: `{}`}}},
	}}
	executor, count := countedEchoExecutor(t, func(_ int, _ string) string { return "same" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor, MaxToolSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "bounded", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *count != 2 || provider.calls != 4 {
		t.Fatalf("executor=%d provider=%d, want 2 executions and 4 provider calls", *count, provider.calls)
	}
}

func TestAgentStallPersistsMatchedToolResultsInDurableSession(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "stall regression"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "call-1", Type: "function", Name: "echo", Arguments: `{"secret":"do-not-leak"}`}}},
		{calls: []ToolCall{{ID: "call-2", Type: "function", Name: "echo", Arguments: `{"secret":"do-not-leak"}`}}},
		{calls: []ToolCall{{ID: "call-3", Type: "function", Name: "echo", Arguments: `{"secret":"do-not-leak"}`}}},
	}}
	executor, _ := countedEchoExecutor(t, func(_ int, _ string) string { return "private-result" })
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: executor, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "repeat", nil)
	if err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if strings.Contains(response.Message.Content, "do-not-leak") || strings.Contains(response.Message.Content, "private-result") {
		t.Fatalf("landing response exposed tool data: %q", response.Message.Content)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	callIDs := make(map[string]struct{})
	resultIDs := make(map[string]struct{})
	for _, event := range loaded.Events {
		switch event.Type {
		case EventToolCall:
			payload, decodeErr := DecodeToolCallEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			callIDs[payload.Call.ID] = struct{}{}
		case EventToolResult:
			payload, decodeErr := DecodeToolResultEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			resultIDs[payload.CallID] = struct{}{}
		}
	}
	if len(callIDs) != 3 || len(resultIDs) != 3 {
		t.Fatalf("durable calls=%v results=%v, want three each", callIDs, resultIDs)
	}
	for callID := range callIDs {
		if _, ok := resultIDs[callID]; !ok {
			t.Fatalf("tool call %q has no durable result", callID)
		}
	}
	if len(callIDs) != len(resultIDs) {
		t.Fatalf("unmatched durable tool events: calls=%v results=%v", callIDs, resultIDs)
	}
	if last := loaded.Events[len(loaded.Events)-1].Type; last != EventCheckpoint {
		t.Fatalf("last durable event=%s, want turn checkpoint", last)
	}
}
