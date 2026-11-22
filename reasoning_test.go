package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestReasoningEventsAreDurableButIgnoredByContext(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendReasoning(ReasoningEvent{RunID: "run-1", Provider: "provider", Model: "glm", Step: 1, Content: "private thought", Complete: true, FinishReason: "tool_calls"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "visible"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := loaded.Messages()
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "private thought") {
			t.Fatal("reasoning leaked into messages")
		}
	}
	result, err := AssembleContext(loaded.EventsSnapshot(), ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ContextText(result), "private thought") {
		t.Fatal("reasoning leaked into provider context")
	}
	var reasoningEvent Event
	for _, event := range loaded.EventsSnapshot() {
		if event.Type == EventReasoning {
			reasoningEvent = event
			break
		}
	}
	decoded, err := DecodeReasoningEvent(reasoningEvent)
	if err != nil || decoded.Content != "private thought" || decoded.Step != 1 {
		t.Fatalf("reasoning=%#v err=%v", decoded, err)
	}
}

type richPartialProvider struct{}

func (richPartialProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richPartialProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richPartialProvider) StreamWithEvents(_ context.Context, _ Request, onEvent StreamEventFunc) (Response, error) {
	_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: "hidden"})
	_ = onEvent(StreamEvent{Kind: StreamContentDelta, Delta: "partial"})
	return Response{Message: Message{Role: RoleAssistant, Content: "partial"}, Reasoning: "hidden", Incomplete: true}, context.DeadlineExceeded
}

func TestAgentRetainsPartialReasoningAndVisibleContentOnProviderFailure(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test"
	agent, err := NewAgent(config, richPartialProvider{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if response.Message.Content != "partial" || !response.Message.Incomplete || response.Reasoning != "hidden" || !response.Incomplete {
		t.Fatalf("response=%#v", response)
	}
	if len(response.Message.ToolCalls) != 0 {
		t.Fatalf("partial tool calls executed/retained: %#v", response.Message.ToolCalls)
	}
}

type richPartialToolProvider struct{}

func (richPartialToolProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richPartialToolProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richPartialToolProvider) StreamWithEvents(_ context.Context, _ Request, onEvent StreamEventFunc) (Response, error) {
	_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: "thinking"})
	_ = onEvent(StreamEvent{Kind: StreamToolCallDelta, ToolCall: &ToolCallDelta{Index: 0, ID: "partial", Type: "function", Name: "danger", Arguments: `{"x":`}})
	return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "partial", Name: "danger", Arguments: `{"x":`}}}, Reasoning: "thinking", Incomplete: true}, context.DeadlineExceeded
}

func TestAgentDropsPartialToolCallsOnReasoningOnlyProviderFailure(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test"
	agent, err := NewAgent(config, richPartialToolProvider{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if response.Reasoning != "thinking" || len(response.Message.ToolCalls) != 0 {
		t.Fatalf("response=%#v", response)
	}
}

func TestProviderFailurePersistsIncompleteReasoningAndVisibleContent(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), richPartialProvider{}, AgentOptions{Session: session, RunID: "timeout-run"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !response.Message.Incomplete {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	var reasoning *ReasoningEvent
	for _, event := range session.EventsSnapshot() {
		if event.Type == EventReasoning {
			decoded, decodeErr := DecodeReasoningEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			reasoning = &decoded
		}
	}
	if reasoning == nil || !reasoning.Incomplete || reasoning.Content != "hidden" || reasoning.ErrorClass != "timeout" {
		t.Fatalf("reasoning=%#v", reasoning)
	}
	foundIncomplete := false
	for _, event := range session.EventsSnapshot() {
		if event.Type == EventMessage {
			message, decodeErr := DecodeMessageEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if message.Message.Incomplete && message.Message.Content == "partial" {
				foundIncomplete = true
			}
		}
	}
	if !foundIncomplete {
		t.Fatal("partial visible content was not durably retained")
	}
}

func TestProviderFailurePartialPersistenceIsContiguousAndAtomicOnStoreFailure(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), richPartialProvider{}, AgentOptions{Session: session, RunID: "atomic-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	events := session.EventsSnapshot()
	if len(events) < 3 || events[len(events)-2].Type != EventMessage || events[len(events)-1].Type != EventReasoning {
		t.Fatalf("partial events were not contiguous message+reasoning: %#v", events)
	}
	failureStore, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failureSession, err := failureStore.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	failurePath := filepath.Join(failureStore.Root(), string(failureSession.ID)+".jsonl")
	failingAgent, err := NewAgentWithOptions(DefaultConfig(), &deleteBeforePartialPersistProvider{path: failurePath}, AgentOptions{Session: failureSession, RunID: "failed-atomic-run"})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := failingAgent.Run(context.Background(), "again", nil)
	eventsAfterFailure := failureSession.EventsSnapshot()
	if runErr == nil {
		t.Fatalf("failed atomic append unexpectedly succeeded: events=%#v", eventsAfterFailure)
	}
	for _, event := range eventsAfterFailure {
		if event.Type == EventReasoning {
			t.Fatalf("failed atomic append retained reasoning: events=%#v err=%v", eventsAfterFailure, runErr)
		}
		if event.Type == EventMessage {
			message, decodeErr := DecodeMessageEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if message.Message.Content == "partial" || message.Message.Incomplete {
				t.Fatalf("failed atomic append retained partial assistant: events=%#v err=%v", eventsAfterFailure, runErr)
			}
		}
	}
}

type deleteBeforePartialPersistProvider struct{ path string }

func (provider *deleteBeforePartialPersistProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (provider *deleteBeforePartialPersistProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	_ = os.Remove(provider.path)
	if onDelta != nil {
		_ = onDelta("partial")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "partial"}, Reasoning: "hidden"}, context.DeadlineExceeded
}

type richReasoningSuccessProvider struct{}

func (richReasoningSuccessProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richReasoningSuccessProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (richReasoningSuccessProvider) StreamWithEvents(_ context.Context, _ Request, onEvent StreamEventFunc) (Response, error) {
	_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: strings.Repeat("界", MaxReasoningBytes/len("界")+100)})
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}, Reasoning: strings.Repeat("界", MaxReasoningBytes/len("界")+100), FinishReason: "stop"}, nil
}

func TestAgentBoundsReturnedReasoningWithoutSplittingUTF8(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test"
	var final AgentEvent
	agent, err := NewAgentWithOptions(config, richReasoningSuccessProvider{}, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "reasoning_final" {
			final = event
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !response.ReasoningTruncated || response.ReasoningOriginalBytes <= MaxReasoningBytes || !utf8.ValidString(response.Reasoning) || len(response.Reasoning) > MaxReasoningBytes {
		t.Fatalf("response=%#v", response)
	}
	if !final.ReasoningTruncated || final.ReasoningOriginalBytes <= MaxReasoningBytes || len(final.Reasoning) > MaxReasoningRunEventBytes {
		t.Fatalf("final event=%#v", final)
	}
}

func TestIncompleteAssistantIsPreservedButExcludedFromProviderReplay(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "failed prefix", Incomplete: true, ToolCalls: []ToolCall{{ID: "partial", Name: "danger", Arguments: `{"x":`}}}); err != nil {
		t.Fatal(err)
	}
	result, err := AssembleContext(session.EventsSnapshot(), ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ContextText(result), "failed prefix") {
		t.Fatal("incomplete assistant replayed into context")
	}
	messages, err := session.Messages()
	if err != nil || len(messages) != 1 || !messages[0].Incomplete || len(messages[0].ToolCalls) != 0 {
		t.Fatalf("history=%#v err=%v", messages, err)
	}
}

type incompleteThenSuccessProvider struct {
	calls    int
	requests []Request
}

func (provider *incompleteThenSuccessProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (provider *incompleteThenSuccessProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	provider.calls++
	if provider.calls == 1 {
		if onDelta != nil {
			_ = onDelta("failed prefix")
		}
		return Response{Message: Message{Role: RoleAssistant, Content: "failed prefix"}}, context.DeadlineExceeded
	}
	if onDelta != nil {
		_ = onDelta("ok")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}, FinishReason: "stop"}, nil
}

func TestIncompleteInMemoryContentIsAbsentFromSubsequentProviderRequests(t *testing.T) {
	provider := &incompleteThenSuccessProvider{}
	agent, err := NewAgent(DefaultConfig(), provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "first", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first err=%v", err)
	}
	if _, err := agent.Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	for _, message := range provider.requests[1].Messages {
		if strings.Contains(message.Content, "failed prefix") {
			t.Fatalf("incomplete content replayed: %#v", provider.requests[1])
		}
	}
}

type noReasoningDiagnosticProvider struct{}

func (noReasoningDiagnosticProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (noReasoningDiagnosticProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	if onDelta != nil {
		_ = onDelta("ok")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}, FinishReason: "stop"}, nil
}

func TestSuccessfulNoReasoningInvocationPersistsDiagnostics(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), noReasoningDiagnosticProvider{}, AgentOptions{Session: session, RunID: "run-diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	var found *ReasoningEvent
	for _, event := range session.EventsSnapshot() {
		if event.Type == EventReasoning {
			payload, decodeErr := DecodeReasoningEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			found = &payload
		}
	}
	if found == nil || !found.Complete || found.Content != "" || found.FinishReason != "stop" || found.RunID != "run-diagnostic" {
		t.Fatalf("diagnostic=%#v", found)
	}
}

func TestProviderReasoningPersistenceFailureIsReturned(t *testing.T) {
	agent := &Agent{session: &Session{ID: SessionID("persistence-failure")}}
	err := agent.recordProviderReasoning(Response{Reasoning: "private", Model: "model"}, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("err=%v", err)
	}
}

type richToolLoopReasoningProvider struct {
	calls    int
	requests []Request
}

func (provider *richToolLoopReasoningProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (provider *richToolLoopReasoningProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (provider *richToolLoopReasoningProvider) StreamWithEvents(_ context.Context, request Request, onEvent StreamEventFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	provider.calls++
	if provider.calls == 1 {
		_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: "step-one"})
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Name: "echo", Arguments: `{}`}}}, FinishReason: "tool_calls"}, nil
	}
	_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: "step-two"})
	return Response{Message: Message{Role: RoleAssistant, Content: "done"}, FinishReason: "stop"}, nil
}

func TestToolLoopPersistsOrderedDistinctReasoningStepsAndDoesNotReplayThem(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &richToolLoopReasoningProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, RunID: "loop-run", Executor: testEchoExecutor(t), MaxToolSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	var records []ReasoningEvent
	for _, event := range session.EventsSnapshot() {
		if event.Type == EventReasoning {
			record, decodeErr := DecodeReasoningEvent(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			records = append(records, record)
		}
	}
	if len(records) != 2 || records[0].Step != 1 || records[1].Step != 2 || records[0].Content != "step-one" || records[1].Content != "step-two" || records[0].FinishReason != "tool_calls" || records[1].FinishReason != "stop" {
		t.Fatalf("records=%#v", records)
	}
	for _, request := range provider.requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "step-one") || strings.Contains(message.Content, "step-two") {
				t.Fatalf("reasoning leaked into request=%#v", request)
			}
		}
	}
}

func TestReasoningSurvivesForkRepairCompactionAndIsExcludedFromSearch(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "reasoning source"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "visible history"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendReasoning(ReasoningEvent{RunID: "r", Profile: "main", Model: "m", Step: 1, Content: "private needle /root/secret", Complete: true, FinishReason: "stop"}); err != nil {
		t.Fatal(err)
	}
	through := uint64(len(session.Events))
	fork, err := store.Fork(session.ID, through, SessionMetadata{Title: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	foundForkReasoning := false
	for _, event := range fork.EventsSnapshot() {
		if event.Type == EventReasoning {
			foundForkReasoning = true
		}
	}
	if !foundForkReasoning {
		t.Fatal("fork dropped reasoning event")
	}
	if _, err := store.RepairSession(session.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := session.Compact(through, "compact summary"); err != nil {
		t.Fatal(err)
	}
	contextResult, err := AssembleContext(session.EventsSnapshot(), ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ContextText(contextResult), "private needle") {
		t.Fatal("compaction/context replay included reasoning")
	}
	history, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	search, err := history.Search(context.Background(), ConversationHistorySearchRequest{Actor: history.Actor, Query: "private needle"})
	if err != nil {
		t.Fatal(err)
	}
	if search.Total != 0 || len(search.Hits) != 0 {
		t.Fatalf("reasoning searchable: %#v", search)
	}
}

type chunkedReasoningProvider struct{}

func (chunkedReasoningProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (chunkedReasoningProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (chunkedReasoningProvider) StreamWithEvents(_ context.Context, _ Request, onEvent StreamEventFunc) (Response, error) {
	for index := 0; index < 1000; index++ {
		_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: strings.Repeat("x", 1024)})
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}, FinishReason: "stop"}, nil
}

func TestReasoningDeltasAreBatchedAndBoundedBeforeRunRetention(t *testing.T) {
	var deltas int
	var retainedBytes int
	agent, err := NewAgentWithOptions(DefaultConfig(), chunkedReasoningProvider{}, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "reasoning_delta" {
			deltas++
			retainedBytes += len(event.Reasoning)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if deltas >= 1000 || deltas < 1 {
		t.Fatalf("reasoning delta count=%d", deltas)
	}
	if retainedBytes > MaxReasoningRunEventBytes {
		t.Fatalf("reasoning delta bytes=%d, max=%d", retainedBytes, MaxReasoningRunEventBytes)
	}
}

type storelessConcurrencyProvider struct{}

func (storelessConcurrencyProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}
func (storelessConcurrencyProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}
func (storelessConcurrencyProvider) StreamWithEvents(_ context.Context, request Request, onEvent StreamEventFunc) (Response, error) {
	prompt := request.Messages[len(request.Messages)-1].Content
	_ = onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: "reason-" + prompt})
	return Response{Message: Message{Role: RoleAssistant, Content: "answer-" + prompt}, FinishReason: "stop"}, nil
}

func TestStorelessConcurrentRunsSerializeConversationAndReasoningCorrelation(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, storelessConcurrencyProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		prompt string
		events []RunEvent
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, prompt := range []string{"alpha", "beta"} {
		prompt := prompt
		wait.Add(1)
		go func() {
			defer wait.Done()
			events, runErr := app.Run(context.Background(), RunRequest{Prompt: prompt})
			if runErr != nil {
				t.Errorf("run %s: %v", prompt, runErr)
				return
			}
			var collected []RunEvent
			for event := range events {
				collected = append(collected, event)
			}
			results <- result{prompt: prompt, events: collected}
		}()
	}
	wait.Wait()
	close(results)
	if len(results) != 2 {
		t.Fatalf("results=%d", len(results))
	}
	for item := range results {
		var runID string
		steps := 0
		for _, event := range item.events {
			if runID == "" {
				runID = event.RunID
			}
			if event.RunID != runID {
				t.Fatalf("prompt %s crossed run IDs: %#v", item.prompt, item.events)
			}
			if event.Type == RunReasoningFinal {
				steps++
				if event.Reasoning == nil || event.Reasoning.Step != 1 || !strings.Contains(event.Reasoning.Content, "reason-"+item.prompt) {
					t.Fatalf("prompt %s reasoning=%#v", item.prompt, event.Reasoning)
				}
			}
		}
		if steps != 1 {
			t.Fatalf("prompt %s reasoning steps=%d events=%#v", item.prompt, steps, item.events)
		}
	}
}
