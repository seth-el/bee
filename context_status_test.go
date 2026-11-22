package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type contextStatusProvider struct {
	requests  []Request
	calls     int
	arguments string
}

func (provider *contextStatusProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("complete not used")
}

func (provider *contextStatusProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	provider.calls++
	if provider.calls == 1 {
		arguments := provider.arguments
		if arguments == "" {
			arguments = `{}`
		}
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "status-call", Type: "function", Name: ContextStatusToolName, Arguments: arguments}}}}, nil
	}
	if onDelta != nil {
		if err := onDelta("done"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "done"}}, nil
}

func contextStatusAgent(t *testing.T, provider Provider, options AgentOptions) *Agent {
	t.Helper()
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	options.Executor = &Executor{Registry: registry, Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyAllow, nil
	})}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, options)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestContextStatusActiveLoopSemanticsAndRedaction(t *testing.T) {
	provider := &contextStatusProvider{}
	agent := contextStatusAgent(t, provider, AgentOptions{ExposeContextStatus: true, Context: ContextOptions{Budget: 1, SystemPrompt: "bounded turn context"}})
	if _, err := agent.Run(context.Background(), "prompt secret=do-not-return", nil); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests=%d", len(provider.requests))
	}
	var status ContextStatus
	if len(agent.Messages()) == 0 {
		t.Fatal("missing agent messages")
	}
	// The first tool result is deliberately inspected through the provider's
	// second request, where it is available as an ordinary tool message.
	for _, message := range provider.requests[1].Messages {
		if message.Role == RoleTool && message.Name == ContextStatusToolName {
			var result ToolResult
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
				t.Fatalf("tool result JSON=%q: %v", message.Content, err)
			}
			if result.Error != "" {
				t.Fatalf("context status tool error=%q", result.Error)
			}
			if err := json.Unmarshal([]byte(result.Output), &status); err != nil {
				t.Fatalf("status JSON=%q: %v", result.Output, err)
			}
		}
	}
	if err := status.Validate(); err != nil {
		t.Fatal(err)
	}
	if status.ToolSteps.Used != 1 || status.ToolSteps.Remaining != DefaultMaxToolSteps-1 || status.ToolSteps.Maximum != DefaultMaxToolSteps || !status.ToolSteps.CurrentCallCounts || status.ToolSteps.Unit != "provider_tool_batches" {
		t.Fatalf("tool status=%#v", status.ToolSteps)
	}
	if status.Context.RequestSnapshot != ContextStatusSnapshotCurrent || status.Context.RequestEstimatedUsed == 0 || status.Context.EstimatedUsed == 0 || !status.Context.OverBudget || status.Context.EstimatedRemain != 0 {
		t.Fatalf("context status=%#v", status.Context)
	}
	if status.Context.RequestEstimatedUsed != estimateRequestMessages(provider.requests[0].Messages) {
		t.Fatalf("request estimate=%d want=%d", status.Context.RequestEstimatedUsed, estimateRequestMessages(provider.requests[0].Messages))
	}
	if status.Context.BudgetScope != ContextBudgetScopeTurn || status.Context.RequestEstimateScope != ContextRequestScopeMessages || status.Context.EstimateUnit != ContextEstimateUnit || status.Dropped.EstimatedUsed != status.Context.EstimatedUsed {
		t.Fatalf("context/drop scope mismatch: context=%#v dropped=%#v", status.Context, status.Dropped)
	}
	if status.Retrieval.Mode != string(MemoryRecallHybrid) || status.Retrieval.State != ContextRetrievalNotAttempted || status.Retrieval.Attempted {
		t.Fatalf("retrieval status=%#v", status.Retrieval)
	}
	if strings.Contains(messageContent(provider.requests[1].Messages, ContextStatusToolName), "secret=do-not-return") {
		t.Fatal("context status leaked prompt text")
	}
}

func messageContent(messages []Message, name string) string {
	for _, message := range messages {
		if message.Role == RoleTool && message.Name == name {
			return message.Content
		}
	}
	return ""
}

func TestContextStatusBeforeSnapshotAndPlanCounts(t *testing.T) {
	plan := trackedPlan(
		AcceptanceCriterion{ID: "pending", Text: "private criterion text", State: AcceptancePending},
		AcceptanceCriterion{ID: "progress", Text: "progress", State: AcceptanceInProgress},
		AcceptanceCriterion{ID: "passed", Text: "passed", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Reference: "call-passed", Provenance: EvidenceObserved}}},
		AcceptanceCriterion{ID: "failed", Text: "failed", State: AcceptanceFailed},
		AcceptanceCriterion{ID: "blocked", Text: "blocked", State: AcceptanceBlocked},
	)
	plan.Questions = []string{"unresolved question"}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "status-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-passed", ToolName: "test", Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	agent := contextStatusAgent(t, &fakeProvider{}, AgentOptions{Session: session, ExposeContextStatus: true})
	result := agent.executeContextStatus()
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	var status ContextStatus
	if err := json.Unmarshal([]byte(result.Output), &status); err != nil {
		t.Fatal(err)
	}
	if status.Context.RequestSnapshot != ContextStatusSnapshotNone || !status.Context.Unbounded || status.Context.RequestEstimatedUsed != 0 || status.Context.EstimatedUsed != 0 {
		t.Fatalf("before-request context=%#v", status.Context)
	}
	if !status.Plan.Exists || status.Plan.PlanID != plan.ID || !status.Plan.UnresolvedQuestions || status.Plan.UnresolvedQuestionCount != len(plan.Questions) {
		t.Fatalf("plan=%#v", status.Plan)
	}
	for _, state := range []AcceptanceState{AcceptancePending, AcceptanceInProgress, AcceptancePassed, AcceptanceFailed, AcceptanceBlocked} {
		if status.Plan.CriteriaByState[string(state)] != 1 {
			t.Fatalf("criteria=%#v", status.Plan.CriteriaByState)
		}
	}
	if strings.Contains(result.Output, "private criterion text") || strings.Contains(result.Output, plan.Goal) || strings.Contains(result.Output, "call-passed") {
		t.Fatalf("plan data leaked: %s", result.Output)
	}
}

func TestContextStatusRetrievalStatesAndTurnReset(t *testing.T) {
	provider := &fakeProvider{}
	memory := &runtimeContextMemoryService{result: MemorySearchResult{Hits: []MemoryDisplayHit{{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "m", Content: "hidden", Namespace: MemoryNamespaceDurableMemory, Key: "k", Sensitivity: MemorySensitivityPublic}}}}, Diagnostics: MemoryRetrievalDiagnostics{Candidates: 4, Returned: 1}}}
	config := DefaultConfig()
	config.Memory.RecallMode = MemoryRecallAlways
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{MemoryService: memory, AutoRecallMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	status, err := agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Retrieval.State != ContextRetrievalSuccess || !status.Retrieval.Attempted || status.Retrieval.Hits != 1 || status.Retrieval.Candidates != 4 {
		t.Fatalf("success retrieval=%#v", status.Retrieval)
	}
	if data, marshalErr := json.Marshal(status); marshalErr != nil || strings.Contains(string(data), "hidden") || strings.Contains(string(data), `"m"`) {
		t.Fatalf("retrieval status leaked memory data: %s err=%v", data, marshalErr)
	}
	memory.err = errors.New("offline")
	if _, err := agent.Run(context.Background(), "second", nil); err == nil {
		t.Fatal("fail-closed retrieval unexpectedly succeeded")
	}
	status, err = agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Retrieval.State != ContextRetrievalFailure || !status.Retrieval.Attempted || status.Retrieval.Hits != 0 {
		t.Fatalf("failure retrieval=%#v", status.Retrieval)
	}
	hybridConfig := DefaultConfig()
	hybridConfig.Memory.RecallMode = MemoryRecallHybrid
	hybrid, err := NewAgentWithOptions(hybridConfig, &fakeProvider{}, AgentOptions{MemoryService: &runtimeContextMemoryService{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hybrid.Run(context.Background(), "ordinary request", nil); err != nil {
		t.Fatal(err)
	}
	status, err = hybrid.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Retrieval.State != ContextRetrievalSuccess || !status.Retrieval.Attempted {
		t.Fatalf("broad hybrid retrieval=%#v", status.Retrieval)
	}
	optionalMemory := &runtimeContextMemoryService{err: errors.New("offline")}
	optional, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{MemoryService: optionalMemory, AutoRecallMemory: true, MemoryFailure: MemoryFailureOptional})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := optional.Run(context.Background(), "optional recall", nil); err != nil {
		t.Fatal(err)
	}
	status, err = optional.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Retrieval.State != ContextRetrievalDegraded || !status.Retrieval.Attempted {
		t.Fatalf("degraded retrieval=%#v", status.Retrieval)
	}
}

func TestContextStatusDroppedDiagnosticsAreBounded(t *testing.T) {
	sources := make([]ContextSourceDiagnostics, MaxContextStatusSources+1)
	for index := range sources {
		sources[index] = ContextSourceDiagnostics{Source: ContextSourceMemory, Included: 1, Bytes: 1}
	}
	sources[0] = ContextSourceDiagnostics{Source: ContextSourceMemory, Included: int(^uint(0) >> 1), Dropped: int(^uint(0) >> 1), Bytes: int(^uint(0) >> 1)}
	agent := &Agent{
		contextBase:         ContextOptions{Budget: int(^uint(0) >> 1)},
		lastRequestEstimate: int(^uint(0) >> 1),
		lastRequestSnapshot: true,
		turnDiagnostics: ContextDiagnostics{
			IncludedItems: int(^uint(0) >> 1),
			DroppedItems:  int(^uint(0) >> 1),
			IncludedBytes: int(^uint(0) >> 1),
			Sources:       sources,
		},
		maxToolSteps:      int(^uint(0) >> 1),
		turnToolStepsUsed: int(^uint(0) >> 1),
	}
	status, err := agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Context.Budget != MaxContextStatusCount || status.Context.EstimatedUsed != MaxContextStatusCount || status.Context.RequestEstimatedUsed != MaxContextStatusCount || !status.Context.Clamped || status.Dropped.DroppedItems != MaxContextStatusCount || status.ToolSteps.Used != MaxContextStatusCount {
		t.Fatalf("unbounded values were not clamped: %#v", status)
	}
	if len(status.Dropped.Sources) != MaxContextStatusSources || !status.Dropped.SourcesTruncated || !status.Dropped.Clamped || status.Dropped.Sources[0].EstimatedUsed != MaxContextStatusCount {
		t.Fatalf("source diagnostics=%#v", status.Dropped.Sources)
	}
}

func TestContextStatusCompositionBoundaries(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPlanTool(app.Capabilities.ToolNames, ContextStatusToolName) {
		t.Fatalf("ordinary capabilities=%v", app.Capabilities.ToolNames)
	}
	provider := &fakeProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, app.agentOptions(nil, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	request, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	if !requestHasTool(request.Tools, ContextStatusToolName) {
		t.Fatal("ordinary parent request omitted context_status")
	}
	status, err := agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Context.RequestSnapshot != ContextStatusSnapshotNone {
		t.Fatalf("request construction alone created a provider snapshot: %#v", status.Context)
	}
	locked, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if containsPlanTool(locked.Capabilities.ToolNames, ContextStatusToolName) {
		t.Fatal("locked composition leaked context_status")
	}
	child, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{RequirePlan: true, ExposePlanTracking: false})
	if err != nil {
		t.Fatal(err)
	}
	request, err = child.request()
	if err != nil {
		t.Fatal(err)
	}
	if requestHasTool(request.Tools, ContextStatusToolName) {
		t.Fatal("child-style agent inherited context_status")
	}
	if definition := contextStatusToolDefinition(); !definition.PlanExempt || definition.Mutability != ToolMutabilityReadOnly {
		t.Fatalf("definition=%#v", definition)
	}
}

func TestContextStatusExplicitMemorySearchObservation(t *testing.T) {
	agent := contextStatusAgent(t, &fakeProvider{}, AgentOptions{ExposeContextStatus: true})
	payload := memorySearchToolResult{
		Hits:          []memorySearchToolHit{{Source: "memory"}, {Source: "memory"}, {Source: "session"}},
		MoreAvailable: true,
		Diagnostics:   memorySearchToolDiagnostics{Candidates: 7, Returned: 3, Degraded: true},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	agent.observeMemorySearchResult(ToolResult{ToolName: "memory_search", Output: string(data)})
	status, err := agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Retrieval.Attempted || status.Retrieval.State != ContextRetrievalDegraded || status.Retrieval.Candidates != 7 || status.Retrieval.Hits != 3 || status.Retrieval.Returned != 3 || status.Retrieval.Sources != 2 || !status.Retrieval.Truncated {
		t.Fatalf("retrieval=%#v", status.Retrieval)
	}
}

func TestContextStatusExposureRejectsMissingExecutorAndReservedRegistryName(t *testing.T) {
	if _, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{ExposeContextStatus: true}); err == nil || !strings.Contains(err.Error(), "requires an executor") {
		t.Fatalf("executorless exposure error=%v", err)
	}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: ContextStatusToolName, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "shadow", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{ExposeContextStatus: true, Executor: &Executor{Registry: registry}})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved-name exposure error=%v", err)
	}
	childConfig := DefaultConfig()
	childConfig.ProviderRole = ProviderRoleChild
	cleanRegistry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAgentWithOptions(childConfig, &fakeProvider{}, AgentOptions{ExposeContextStatus: true, Executor: &Executor{Registry: cleanRegistry}})
	if err == nil || !strings.Contains(err.Error(), "child providers") {
		t.Fatalf("child exposure error=%v", err)
	}
}

func TestContextStatusTurnResetClearsRequestAndToolCounters(t *testing.T) {
	agent := contextStatusAgent(t, &fakeProvider{}, AgentOptions{ExposeContextStatus: true})
	agent.turnToolStepsUsed = 7
	agent.lastRequestEstimate = 123
	agent.lastRequestSnapshot = true
	if err := agent.prepareTurnContext(context.Background(), "new turn", ""); err != nil {
		t.Fatal(err)
	}
	status, err := agent.contextStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ToolSteps.Used != 0 || status.Context.RequestSnapshot != ContextStatusSnapshotNone || status.Context.RequestEstimatedUsed != 0 {
		t.Fatalf("turn state was not reset: %#v", status)
	}
}

func TestContextStatusErrorsDoNotEchoArgumentsOrPaths(t *testing.T) {
	provider := &contextStatusProvider{arguments: `{"password=super-secret":true}`}
	agent := contextStatusAgent(t, provider, AgentOptions{ExposeContextStatus: true})
	if _, err := agent.Run(context.Background(), "status", nil); err != nil {
		t.Fatal(err)
	}
	content := messageContent(provider.requests[1].Messages, ContextStatusToolName)
	if !strings.Contains(content, "invalid context_status arguments") || strings.Contains(content, "super-secret") || strings.Contains(content, "password") {
		t.Fatalf("argument error leaked input: %s", content)
	}

	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "context-status-corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	path := store.path(session.ID)
	pathParent := store.Root()
	pathAgent := contextStatusAgent(t, &fakeProvider{}, AgentOptions{ExposeContextStatus: true, Session: session})
	if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := pathAgent.executeContextStatus()
	if result.Error != "context status unavailable" || strings.Contains(result.Error, path) || strings.Contains(result.Error, pathParent) {
		t.Fatalf("path error leaked details: %#v", result)
	}
}

func TestContextEstimateArithmeticSaturates(t *testing.T) {
	items := []ContextItem{{Source: ContextSourceSystem, SizeEstimate: maxIntValue()}, {Source: ContextSourceSystem, SizeEstimate: 1}}
	if got := EstimateItems(items); got != maxIntValue() {
		t.Fatalf("EstimateItems=%d", got)
	}
	diagnostics := contextDiagnostics(items, items)
	if diagnostics.IncludedBytes != maxIntValue() {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}
