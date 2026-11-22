package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestTurnContextPreservesPriorMessagePrefix(t *testing.T) {
	base := []Message{
		{Role: RoleSystem, Content: "stable"},
		{Role: RoleUser, Content: "first"},
		{Role: RoleAssistant, Content: "answer"},
		{Role: RoleUser, Content: "current"},
	}
	got := requestMessagesWithTurnContext(base, []Message{{Role: RoleSystem, Content: "turn memory"}})
	if len(got) != 5 || got[3].Role != RoleUser || got[3].Content != "current" || got[4].Content != "turn memory" {
		t.Fatalf("turn context placement = %#v", got)
	}
	for index := range base {
		wantBytes, _ := json.Marshal(base[index])
		gotBytes, _ := json.Marshal(got[index])
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Fatalf("historical message %d changed: got %s want %s", index, gotBytes, wantBytes)
		}
	}
}

func TestGoldenStablePrefixGrowsWithoutChangingOldBytes(t *testing.T) {
	tools := []ToolDefinition{{Name: "alpha", Description: "stable", Parameters: json.RawMessage(`{"type":"object"}`)}}
	one := []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "one"}}
	_, _, prefixOne := stablePrefixFingerprint(one, tools)
	two := []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "one"}, {Role: RoleAssistant, Content: "a"}, {Role: RoleUser, Content: "two"}}
	_, _, prefixTwo := stablePrefixFingerprint(two, tools)
	if !bytes.HasPrefix(prefixTwo, prefixOne) {
		t.Fatalf("old serialized request prefix changed:\nold=%s\nnew=%s", prefixOne, prefixTwo)
	}
}

func TestAgentReportsStableGrowingPrefixAcrossTurns(t *testing.T) {
	config := DefaultConfig()
	config.Agent.SystemPrompt = "stable system"
	provider := &contextKernelCaptureProvider{response: Response{Message: Message{Role: RoleAssistant, Content: "answer"}}}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "two", nil); err != nil {
		t.Fatal(err)
	}
	if agent.lastContextUsage.PrefixChanged {
		t.Fatalf("stable append-only prefix reported changed: %#v", agent.lastContextUsage)
	}
	if agent.lastContextUsage.PrefixStableTokens == 0 {
		t.Fatal("stable prefix size was not reported")
	}
}

func TestRequestAccountingIncludesToolsReservationsAndUnknownWindow(t *testing.T) {
	messages := []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hello"}, {Role: RoleTool, Content: "result"}, {Role: RoleUser, Attachments: []AttachmentRef{{Kind: "image", MediaType: "image/png"}}}}
	tools := []ToolDefinition{{Name: "lookup", Description: "look", Parameters: json.RawMessage(`{"type":"object"}`)}}
	known := classifyRequestUsage(messages, tools, 1000, 100, 50)
	if !known.WindowKnown || known.PromptBudget != 850 || known.SystemTokens == 0 || known.HistoryTokens == 0 || known.ToolResultTokens == 0 || known.AttachmentTokens == 0 || known.ToolSchemaTokens == 0 {
		t.Fatalf("known accounting = %#v", known)
	}
	unknown := classifyRequestUsage(messages, tools, 0, 100, 50)
	if unknown.WindowKnown || unknown.PromptBudget != 0 || unknown.OverBudget {
		t.Fatalf("unknown window borrowed a budget: %#v", unknown)
	}
}

func TestToolResultPruningIsProjectionOnly(t *testing.T) {
	original := []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "x", Arguments: `{}`}}}, {Role: RoleTool, ToolCallID: "c", Content: strings.Repeat("large", 1000)}, {Role: RoleUser, Content: "next"}}
	projected, count := projectToolResults(original, 8, 0)
	if count != 1 || projected[1].Content != prunedToolResultMarker {
		t.Fatalf("projection=%#v count=%d", projected, count)
	}
	if original[1].Content == prunedToolResultMarker {
		t.Fatal("durable/in-memory source was rewritten")
	}
	if projected[0].ToolCalls[0].ID != projected[1].ToolCallID {
		t.Fatal("tool pairing was not preserved")
	}
}

func TestToolResultTargetPressureDoesNotRewriteStableHistory(t *testing.T) {
	content := strings.Repeat("stable", 100)
	messages := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "x", Arguments: `{}`}}},
		{Role: RoleTool, ToolCallID: "c", Content: content},
		{Role: RoleUser, Content: "next"},
	}
	projected, count := projectToolResults(messages, 0, 1)
	if count != 0 || projected[1].Content != content {
		t.Fatalf("target pressure rewrote stable history: count=%d projection=%#v", count, projected)
	}
}

func TestStablePrefixFingerprintUsesProviderVisibleToolSchema(t *testing.T) {
	messages := []Message{{Role: RoleSystem, Content: "system"}, {Role: RoleUser, Content: "turn"}}
	first := []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Risk: RiskLow, Mutability: ToolMutabilityReadOnly}}
	second := []ToolDefinition{{Name: "lookup", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Risk: RiskCritical, Mutability: ToolMutabilityMutating}}
	firstFingerprint, _, _ := stablePrefixFingerprint(messages, first)
	secondFingerprint, _, _ := stablePrefixFingerprint(messages, second)
	if firstFingerprint != secondFingerprint {
		t.Fatalf("local-only tool policy changed provider prefix: %q != %q", firstFingerprint, secondFingerprint)
	}
}

func TestHistoricalImagesAreRemovedFromReplayProjection(t *testing.T) {
	historical := AttachmentRef{Reference: "old", Kind: "image", MediaType: "image/png"}
	current := AttachmentRef{Reference: "new", Kind: "image", MediaType: "image/png"}
	messages := []Message{
		{Role: RoleUser, Content: "old", Attachments: []AttachmentRef{historical}},
		{Role: RoleAssistant, Content: "described"},
		{Role: RoleUser, Content: "new", Attachments: []AttachmentRef{current}},
	}
	projected := stripHistoricalImageAttachments(messages)
	if len(projected[0].Attachments) != 0 {
		t.Fatalf("historical image remained in replay: %#v", projected[0].Attachments)
	}
	if len(projected[2].Attachments) != 1 || projected[2].Attachments[0].Reference != "new" {
		t.Fatalf("current image was removed: %#v", projected[2].Attachments)
	}
}

func TestCacheStateSurvivesAgentReconstructionAndClassifiesBreaks(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := Request{Model: "model-a", Messages: []Message{{Role: RoleSystem, Content: "core"}, {Role: RoleUser, Content: "one"}}}
	first := &Agent{config: DefaultConfig(), session: session}
	first.observeProviderRequest(firstRequest)
	reloaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := latestSessionCacheState(reloaded.EventsSnapshot())
	if err != nil || !found {
		t.Fatalf("cache state found=%v err=%v", found, err)
	}
	second := &Agent{
		config: DefaultConfig(), priorPrefixFingerprint: state.Fingerprint, priorPrefixBytes: state.Bytes,
		priorCacheEpoch: state.Epoch, priorPrefixModelHash: state.ModelHash, priorPrefixTemplateHash: state.TemplateHash,
		priorPrefixToolsHash: state.ToolsHash, priorPrefixHistoryHash: state.HistoryHash,
	}
	second.observeProviderRequest(Request{Model: "model-a", Messages: []Message{
		{Role: RoleSystem, Content: "core"}, {Role: RoleUser, Content: "one"}, {Role: RoleAssistant, Content: "answer"}, {Role: RoleUser, Content: "two"},
	}})
	if second.lastContextUsage.PrefixChanged || second.lastContextUsage.CacheEpoch != state.Epoch {
		t.Fatalf("append-only reconstruction reported a break: %#v", second.lastContextUsage)
	}
	broken := &Agent{
		config: DefaultConfig(), priorPrefixFingerprint: state.Fingerprint, priorPrefixBytes: state.Bytes,
		priorCacheEpoch: state.Epoch, priorPrefixModelHash: state.ModelHash, priorPrefixTemplateHash: state.TemplateHash,
		priorPrefixToolsHash: state.ToolsHash, priorPrefixHistoryHash: state.HistoryHash,
	}
	broken.observeProviderRequest(Request{Model: "model-b", Messages: firstRequest.Messages})
	if !broken.lastContextUsage.PrefixChanged || broken.lastContextUsage.CacheEpoch != state.Epoch+1 || !strings.Contains(broken.lastContextUsage.PrefixChangeReason, "model") {
		t.Fatalf("model break was not classified: %#v", broken.lastContextUsage)
	}
}

func TestToolResultPruningPreservesLatestCanonicalPlanState(t *testing.T) {
	plan := validPlanModeTestPlan("protected-plan")
	content := canonicalPlanToolOutput("accepted", plan, 1, "accepted")
	messages := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "plan-call", Name: PlanToolName, Arguments: `{}`}}},
		{Role: RoleTool, Name: PlanToolName, ToolCallID: "plan-call", Content: content},
		{Role: RoleUser, Content: "continue"},
	}
	projected, count := projectToolResults(messages, 1, 1)
	if count != 0 || projected[1].Content != content {
		t.Fatalf("canonical plan state was pruned: count=%d projection=%#v", count, projected)
	}
}

func TestKnownWindowRejectsBeforeProviderIO(t *testing.T) {
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Provider.ContextWindowTokens = 64
	config.Generation.MaxTokens = 32
	config.Context.ReserveTokens = 16
	provider := &contextKernelCaptureProvider{}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), strings.Repeat("x", 1024), nil)
	if !errors.Is(err, ErrContextWindowExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(provider.requests) != 0 {
		t.Fatal("over-budget request reached provider")
	}
}

func TestAutomaticOverflowCompactionRetainsTranscriptAndProvenance(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: strings.Repeat("old question ", 80)}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: strings.Repeat("old answer ", 80)}); err != nil {
		t.Fatal(err)
	}
	beforeEvents := session.EventsSnapshot()
	before := len(beforeEvents)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Provider.ContextWindowTokens = 220
	config.Generation.MaxTokens = 32
	config.Context = ContextKernelConfig{ReserveTokens: 16, OutputReserveTokens: 32, ToolResultTokens: 32, SummaryTokens: 24, AutoCompact: true}
	provider := &contextKernelCaptureProvider{response: Response{Message: Message{Role: RoleAssistant, Content: "done"}}}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "current", nil); err != nil {
		t.Fatal(err)
	}
	events := session.EventsSnapshot()
	if len(events) <= before {
		t.Fatal("compaction did not append events")
	}
	found := false
	for _, event := range events {
		if event.Type != EventCompaction {
			continue
		}
		var payload CompactionEvent
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.SourceSHA256 == "" || payload.SummarySHA256 == "" || !strings.HasPrefix(payload.Method, "overflow-recovery/") || payload.Through == 0 {
			t.Fatalf("provenance=%#v", payload)
		}
		if err := VerifyCompaction(events, event); err != nil {
			t.Fatalf("verify compaction: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("no compaction event")
	}
	// Append-only means the original events are byte-for-byte present at the front.
	for index, event := range events[:before] {
		beforeJSON, _ := json.Marshal(beforeEvents[index])
		afterJSON, _ := json.Marshal(event)
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatal("source transcript changed")
		}
	}
}

func TestManualCompactCommandAppendsCheckpoint(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{{Role: RoleUser, Content: "one"}, {Role: RoleAssistant, Content: "answer one"}, {Role: RoleUser, Content: "two"}, {Role: RoleAssistant, Content: "answer two"}} {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	app, err := NewApplication(config, &contextKernelCaptureProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	result := DispatchCommand(context.Background(), "/compact", CommandContext{Application: app, SessionID: session.ID})
	if !result.Handled || result.Kind != commandKindCompact || !strings.Contains(result.Text, "source transcript retained") {
		t.Fatalf("result=%#v", result)
	}
	fresh, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := fresh.EventsSnapshot()
	foundCompaction := false
	for _, event := range events {
		if event.Type == EventCompaction {
			foundCompaction = true
			break
		}
	}
	if !foundCompaction {
		t.Fatal("manual compaction did not append checkpoint")
	}
	messages, err := AssembleContext(events, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasPlanModeControlMessage(messages.Messages) {
		t.Fatalf("manual compaction did not append plan-mode checkpoint: %#v", messages.Messages)
	}
}

func TestUsageNormalizesCacheTelemetry(t *testing.T) {
	var usage Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":60},"cache_creation_input_tokens":12}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokens != 100 || usage.CacheReadTokens != 60 || usage.CacheWriteTokens != 12 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestContextKernelConfigAndProviderWindowLoad(t *testing.T) {
	path := t.TempDir() + "/config.json"
	body := []byte(`{"provider":{"context_window_tokens":32768},"context":{"reserve_tokens":512,"output_reserve_tokens":384,"tool_result_tokens":256,"summary_tokens":128,"auto_compact":true}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider.ContextWindowTokens != 32768 || config.Context.ReserveTokens != 512 || config.Context.OutputReserveTokens != 384 || config.Context.ToolResultTokens != 256 || config.Context.SummaryTokens != 128 || !config.Context.AutoCompact {
		t.Fatalf("config=%#v", config)
	}
}

type contextKernelCaptureProvider struct {
	requests []Request
	response Response
}

func (provider *contextKernelCaptureProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	return provider.response, nil
}

func (provider *contextKernelCaptureProvider) Stream(_ context.Context, request Request, _ StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	response := provider.response
	if response.Message.Role == "" {
		response.Message.Role = RoleAssistant
	}
	return response, nil
}
