package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func requestContainsSystemText(request Request, text string) bool {
	for _, message := range request.Messages {
		if message.Role == RoleSystem && strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}

type turnCaptureProvider struct {
	requests []Request
	response Response
}

func (provider *turnCaptureProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	return provider.response, nil
}

func (provider *turnCaptureProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	if onDelta != nil && provider.response.Message.Content != "" {
		_ = onDelta(provider.response.Message.Content)
	}
	return provider.response, nil
}

func TestAgentWarnsBeforeToolBudgetBoundary(t *testing.T) {
	provider := &toolLoopProvider{toolResponses: 1}
	config := DefaultConfig()
	config.Agent.ToolLandingSteps = 3
	var usages []RequestContextUsage
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 4, EventHook: func(event AgentEvent) {
		if event.Type == "context_usage" {
			usages = append(usages, event.ContextUsage)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if len(provider.requests) != 2 || !requestContainsSystemText(provider.requests[1], "Controlled landing warning") {
		t.Fatalf("second request did not receive landing warning: %#v", provider.requests)
	}
	if len(usages) != 2 {
		t.Fatalf("context usages=%#v", usages)
	}
	request := provider.requests[1]
	expected := classifyRequestUsage(request.Messages, request.Tools, agent.providerContextWindow(), request.MaxTokens, config.Context.ReserveTokens)
	expected.PrefixFingerprint, expected.PrefixStableTokens, _ = stableRequestPrefixFingerprint(request)
	_, _, expectedPrefix := stableRequestPrefixFingerprint(request)
	expected.PrefixStableBytes = len(expectedPrefix)
	components := stableRequestPrefixComponents(request)
	expected.PrefixModelHash = components.Model
	expected.PrefixTemplateHash = components.Template
	expected.PrefixToolsHash = components.Tools
	expected.PrefixHistoryHash = components.History
	expected.CacheEpoch = 1
	if !reflect.DeepEqual(usages[1], expected) {
		t.Fatalf("telemetry did not describe final provider request:\ngot=%#v\nwant=%#v", usages[1], expected)
	}
}

func TestAgentToolBudgetUsesCacheStableToolDisabledFinalizerAndCheckpoint(t *testing.T) {
	provider := &toolLoopProvider{alwaysTool: true}
	var checkpoint *TurnCheckpoint
	var usages []RequestContextUsage
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 1, EventHook: func(event AgentEvent) {
		if event.Type == "turn_checkpoint" {
			checkpoint = cloneTurnCheckpoint(event.Checkpoint)
		}
		if event.Type == "context_usage" {
			usages = append(usages, event.ContextUsage)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("requests=%#v", provider.requests)
	}
	work := provider.requests[1]
	finalizer := provider.requests[2]
	if len(finalizer.Tools) == 0 || !reflect.DeepEqual(finalizer.Tools, work.Tools) {
		t.Fatalf("finalizer tool schemas changed: work=%#v finalizer=%#v", work.Tools, finalizer.Tools)
	}
	if finalizer.ToolChoice == nil || finalizer.ToolChoice.Mode != ToolChoiceNone || finalizer.ToolChoice.Name != "" {
		t.Fatalf("finalizer tool choice=%#v", finalizer.ToolChoice)
	}
	if finalizer.ReasoningEffort != work.ReasoningEffort || finalizer.DisableThinking != work.DisableThinking || !reflect.DeepEqual(finalizer.ChatTemplateKwargs, work.ChatTemplateKwargs) {
		t.Fatalf("finalizer changed prompt settings: work=%#v finalizer=%#v", work, finalizer)
	}
	if len(usages) != 3 || usages[2].PrefixChanged || usages[2].ToolSchemaTokens != usages[1].ToolSchemaTokens {
		t.Fatalf("finalizer cache telemetry=%#v", usages)
	}
	for _, message := range agent.Messages() {
		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 && message.ToolCalls[0].ID != "call-1" {
			t.Fatalf("disabled finalizer tool call reached conversation state: %#v", message)
		}
	}
	if checkpoint == nil || checkpoint.WhyStopped != TurnStopToolBudget || checkpoint.ToolStepsUsed != 1 || checkpoint.NextAction == "" {
		t.Fatalf("checkpoint=%#v", checkpoint)
	}
}

type deadlineLandingProvider struct {
	mu       sync.Mutex
	requests []Request
	calls    int
	first    Response
	delay    time.Duration
	started  chan struct{}
}

func (provider *deadlineLandingProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *deadlineLandingProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if call == 1 {
		if provider.started != nil {
			close(provider.started)
		}
		timer := time.NewTimer(provider.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-timer.C:
		}
		if onDelta != nil && provider.first.Message.Content != "" {
			_ = onDelta(provider.first.Message.Content)
		}
		return provider.first, nil
	}
	if onDelta != nil {
		_ = onDelta("landed cleanly")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "landed cleanly"}}, nil
}

func TestAgentDeadlineGetsFreshFinalizationContext(t *testing.T) {
	provider := &deadlineLandingProvider{
		delay: 1100 * time.Millisecond,
		first: Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{
			ID: "call-after-deadline", Name: "echo", Arguments: `{"text":"late"}`,
		}}}},
	}
	config := DefaultConfig()
	config.Agent.TurnTimeoutSeconds = 1
	config.Agent.FinalizationTimeoutSeconds = 2
	var checkpoint *TurnCheckpoint
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Executor: testEchoExecutor(t), EventHook: func(event AgentEvent) {
		if event.Type == "turn_checkpoint" {
			checkpoint = cloneTurnCheckpoint(event.Checkpoint)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "landed cleanly" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	provider.mu.Lock()
	requests := append([]Request(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) != 2 || len(requests[1].Tools) == 0 || requests[1].ToolChoice == nil || requests[1].ToolChoice.Mode != ToolChoiceNone {
		t.Fatalf("requests=%#v", requests)
	}
	if checkpoint == nil || checkpoint.WhyStopped != TurnStopDeadline {
		t.Fatalf("checkpoint=%#v", checkpoint)
	}
}

func TestAgentLetsAdmittedFinalResponseFinishAfterTurnDeadline(t *testing.T) {
	provider := &deadlineLandingProvider{
		delay: 1100 * time.Millisecond,
		first: Response{Message: Message{Role: RoleAssistant, Content: "finished after the work deadline"}},
	}
	config := DefaultConfig()
	config.Agent.TurnTimeoutSeconds = 1
	var checkpoints int
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "turn_checkpoint" {
			checkpoints++
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "finished after the work deadline" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 1 || checkpoints != 0 {
		t.Fatalf("provider calls=%d checkpoints=%d, want completed response without checkpoint", calls, checkpoints)
	}
}

func TestAgentExplicitCancellationStillStopsAdmittedProviderResponse(t *testing.T) {
	provider := &deadlineLandingProvider{
		delay:   time.Hour,
		started: make(chan struct{}),
		first:   Response{Message: Message{Role: RoleAssistant, Content: "must not finish"}},
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := agent.Run(ctx, "work", nil)
		done <- runErr
	}()
	<-provider.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want cancellation", err)
	}
}

type slowFinalizationProvider struct {
	mu    sync.Mutex
	calls int
}

func (provider *slowFinalizationProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *slowFinalizationProvider) Stream(ctx context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.mu.Unlock()
	if call <= 2 {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: fmt.Sprintf("call-%d", call), Name: "echo", Arguments: `{"text":"work"}`}}}}, nil
	}
	timer := time.NewTimer(1100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-timer.C:
	}
	if onDelta != nil {
		_ = onDelta("finalization finished after its admission deadline")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "finalization finished after its admission deadline"}}, nil
}

func TestAgentLetsAdmittedFinalizationFinishAfterFinalizationDeadline(t *testing.T) {
	provider := &slowFinalizationProvider{}
	config := DefaultConfig()
	config.Agent.FinalizationTimeoutSeconds = 1
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "finalization finished after its admission deadline" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestAgentAutoContinuationAdmitsBoundedNextSegment(t *testing.T) {
	provider := &toolLoopProvider{toolResponses: 2}
	config := DefaultConfig()
	config.Agent.MaxAutoContinuations = 1
	config.Agent.NoProgressLimit = 0
	var checkpoints int
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 1, EventHook: func(event AgentEvent) {
		if event.Type == "turn_checkpoint" {
			checkpoints++
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if provider.calls != 3 || checkpoints != 0 {
		t.Fatalf("provider calls=%d checkpoints=%d", provider.calls, checkpoints)
	}
}

func TestAgentCanCompactCompletedToolChatterBetweenSegments(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &toolLoopProvider{toolResponses: 2}
	config := DefaultConfig()
	config.Agent.MaxAutoContinuations = 1
	config.Agent.AutoCompactBetweenSegments = true
	config.Agent.NoProgressLimit = 0
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session, Executor: testEchoExecutor(t), MaxToolSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "work", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range loaded.EventsSnapshot() {
		if event.Type != EventCompaction {
			continue
		}
		var compaction CompactionEvent
		if err := json.Unmarshal(event.Data, &compaction); err != nil {
			t.Fatal(err)
		}
		found = found || strings.HasPrefix(compaction.Method, "segment-boundary/")
	}
	if !found {
		t.Fatal("missing durable segment-boundary compaction")
	}
}

func TestAgentDetectsEquivalentExecPollingWithoutLeakingOutput(t *testing.T) {
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "1", Name: "exec", Arguments: `{"command":"kubectl get deployment nitter -n prod -o json"}`}}},
		{calls: []ToolCall{{ID: "2", Name: "exec", Arguments: `{"command":"kubectl -n prod get deploy nitter -o wide"}`}}},
		{calls: []ToolCall{{ID: "3", Name: "exec", Arguments: `{"command":"kubectl get deployment nitter --namespace prod"}`}}},
	}}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "exec", Description: "test"}, Fn: func(context.Context, json.RawMessage) (string, error) {
		return "private unchanged observation", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint *TurnCheckpoint
	agent, err := NewAgentWithOptions(unlimitedLoopConfig(), provider, AgentOptions{Executor: &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}, EventHook: func(event AgentEvent) {
		if event.Type == "turn_checkpoint" {
			checkpoint = cloneTurnCheckpoint(event.Checkpoint)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "poll", nil)
	if err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if checkpoint == nil || checkpoint.WhyStopped != TurnStopNoProgress {
		t.Fatalf("checkpoint=%#v", checkpoint)
	}
	if strings.Contains(checkpoint.Summary, "private unchanged observation") {
		t.Fatalf("checkpoint duplicated private output: %#v", checkpoint)
	}
}

func TestDurableCheckpointCanBeExplicitlyResumed(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := session.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopToolBudget, Summary: "prior summary", NextAction: "continue", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	provider := &turnCaptureProvider{response: Response{Message: Message{Role: RoleAssistant, Content: "continued"}}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, ResumeCheckpoint: &checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "continue", nil)
	if err != nil || response.Message.Content != "continued" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if len(provider.requests) != 1 || !requestContainsSystemText(provider.requests[0], "<turn_checkpoint>") || !requestContainsSystemText(provider.requests[0], checkpoint.ID) {
		t.Fatalf("checkpoint was not injected: %#v", provider.requests)
	}
}

func TestCheckpointEvidenceFingerprintsSuppressRepeatedReadOnResume(t *testing.T) {
	call := ToolCall{ID: "prior-read", Name: "read", Arguments: `{"path":"config.json"}`}
	result := ToolResult{ToolName: "read", Output: "prior contents"}
	checkpoint := TurnCheckpoint{
		ID:         "checkpoint-read",
		WhyStopped: TurnStopDeadline,
		NextAction: "continue",
		Evidence:   appendCheckpointEvidence(nil, []ToolCall{call}, []ToolResult{result}),
	}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "read", Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("checkpointed read executed again")
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}, ResumeCheckpoint: &checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	reused, ok := agent.reuseCheckpointObservation(ToolCall{ID: "new-read", Name: call.Name, Arguments: call.Arguments})
	if !ok || reused.Error != "" || !strings.Contains(reused.Output, "already_observed") || !strings.Contains(reused.Output, checkpoint.ID) {
		t.Fatalf("reused=%#v ok=%t", reused, ok)
	}
	if _, ok := agent.reuseCheckpointObservation(ToolCall{ID: "different", Name: call.Name, Arguments: `{"path":"other.json"}`}); ok {
		t.Fatal("different read was suppressed")
	}
}

func TestLatestPendingCheckpointTracksResumeEvents(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopDeadline, NextAction: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopToolBudget, NextAction: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if latest, err := session.LatestPendingTurnCheckpoint(); err != nil || latest.ID != second.ID {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	if err := session.AppendTurnCheckpointResume(second.ID); err != nil {
		t.Fatal(err)
	}
	if latest, err := session.LatestPendingTurnCheckpoint(); err != nil || latest.ID != first.ID {
		t.Fatalf("fallback=%#v err=%v", latest, err)
	}
}

func TestApplicationScopesCheckpointContinuationToItsSession(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopToolBudget, NextAction: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &turnCaptureProvider{response: Response{Message: Message{Role: RoleAssistant, Content: "continued"}}}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Run(context.Background(), RunRequest{SessionID: other.ID, Prompt: "continue", CheckpointID: checkpoint.ID}); err == nil || !strings.Contains(err.Error(), "checkpoint not found") {
		t.Fatalf("cross-session checkpoint error=%v", err)
	}
	events, err := app.Run(context.Background(), RunRequest{SessionID: source.ID, Prompt: "continue", CheckpointID: checkpoint.ID})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(provider.requests) != 1 || !requestContainsSystemText(provider.requests[0], checkpoint.ID) {
		t.Fatalf("valid checkpoint was not resumed: %#v", provider.requests)
	}
}

func TestExplicitCancellationStillStopsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &deadlineLandingProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(ctx, "stop", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want cancellation", err)
	}
}
