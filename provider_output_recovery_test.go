package bee

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type interruptedToolRecoveryProvider struct {
	calls int
}

type repeatedProviderRequestEOF struct {
	calls int
}

type providerRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip providerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func (provider *repeatedProviderRequestEOF) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}

func (provider *repeatedProviderRequestEOF) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}

func (provider *repeatedProviderRequestEOF) StreamWithEvents(context.Context, Request, StreamEventFunc) (Response, error) {
	provider.calls++
	return Response{}, &providerRequestError{cause: io.EOF}
}

func TestProviderRequestEOFRetriesOnceBeforeResponse(t *testing.T) {
	calls := 0
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	provider, err := NewOpenAICompatibleProvider(config.Provider, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.client = &http.Client{Transport: providerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, io.EOF
		}
		body := "data: {\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	var failure *ProviderOutputRejection
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "provider_output_rejected" && event.ProviderOutput != nil {
			copy := *event.ProviderOutput
			failure = &copy
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if err != nil || response.Message.Content != "recovered" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if calls != 2 {
		t.Fatalf("provider calls=%d, want 2", calls)
	}
	if failure == nil || failure.Stage != ProviderFailureStageRequest || !failure.Retrying || failure.RecoveryAttempt != 1 || failure.ErrorClass != "connection_eof" {
		t.Fatalf("failure=%#v", failure)
	}
}

func TestProviderRequestEOFRetryIsBounded(t *testing.T) {
	provider := &repeatedProviderRequestEOF{}
	var failures []ProviderOutputRejection
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "provider_output_rejected" && event.ProviderOutput != nil {
			failures = append(failures, *event.ProviderOutput)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want EOF", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls=%d, want initial request plus one retry", provider.calls)
	}
	if len(failures) != 2 || !failures[0].Retrying || failures[1].Retrying || failures[0].Stage != ProviderFailureStageRequest || failures[1].Stage != ProviderFailureStageRequest {
		t.Fatalf("failures=%#v", failures)
	}
}

func (provider *interruptedToolRecoveryProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unused")
}

func (provider *interruptedToolRecoveryProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, errors.New("unused")
}

func (provider *interruptedToolRecoveryProvider) StreamWithEvents(_ context.Context, _ Request, onEvent StreamEventFunc) (Response, error) {
	provider.calls++
	switch provider.calls {
	case 1:
		_ = onEvent(StreamEvent{Kind: StreamToolCallDelta, ToolCall: &ToolCallDelta{Index: 0, ID: "partial", Type: "function", Name: "echo", Arguments: `{"x":`}})
		return Response{Message: Message{Role: RoleAssistant}, FinishReason: ""}, io.ErrUnexpectedEOF
	case 2:
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "good", Name: "echo", Arguments: `{}`}}}, FinishReason: "tool_calls"}, nil
	default:
		return Response{Message: Message{Role: RoleAssistant, Content: "done"}, FinishReason: "stop"}, nil
	}
}

func TestMalformedProviderToolCallRetriesOnceWithoutPoisoningSession(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "provider recovery"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad-1", Type: "function", Name: "echo", Arguments: `{"content":"truncated`}}}, FinishReason: "tool_calls", Usage: Usage{TotalTokens: 10}},
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "good-1", Type: "function", Name: "echo", Arguments: `{}`}}}, FinishReason: "tool_calls", Usage: Usage{TotalTokens: 4}},
		{Message: Message{Role: RoleAssistant, Content: "done"}, FinishReason: "stop", Usage: Usage{TotalTokens: 2}},
	}}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "ok" })
	var rejected *ProviderOutputRejection
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, Executor: executor, EventHook: func(event AgentEvent) {
		if event.Type == "provider_output_rejected" && event.ProviderOutput != nil {
			copy := *event.ProviderOutput
			rejected = &copy
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "write it", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if *executions != 1 || len(provider.requests) != 3 {
		t.Fatalf("executions=%d requests=%d, want 1 and 3", *executions, len(provider.requests))
	}
	if rejected == nil || !rejected.Retrying || rejected.ToolName != "echo" || rejected.ArgumentBytes == 0 || rejected.ErrorClass != "invalid_tool_arguments" {
		t.Fatalf("rejection=%#v", rejected)
	}
	if !requestContainsRecoveryInstruction(provider.requests[1]) {
		t.Fatalf("retry request lacks recovery instruction: %#v", provider.requests[1].Messages)
	}
	assertNoDurableToolCall(t, session, "bad-1")
	if _, err := agent.request(); err != nil {
		t.Fatalf("completed recovery left invalid in-memory history: %v", err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Messages(); err != nil {
		t.Fatalf("completed recovery left invalid durable history: %v", err)
	}
}

func TestRepeatedMalformedProviderToolCallFailsCleanlyAndSessionResumes(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad-1", Name: "echo", Arguments: `{"x":`}}}, FinishReason: "tool_calls"},
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad-2", Name: "echo", Arguments: `{"x":`}}}, FinishReason: "tool_calls"},
		{Message: Message{Role: RoleAssistant, Content: "resumed"}, FinishReason: "stop"},
	}}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "unexpected" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "first", nil); !errors.Is(err, ErrInvalidProviderToolCall) {
		t.Fatalf("err=%v, want ErrInvalidProviderToolCall", err)
	}
	if *executions != 0 || len(provider.requests) != 2 {
		t.Fatalf("executions=%d requests=%d, want 0 and 2", *executions, len(provider.requests))
	}
	assertNoDurableToolCall(t, session, "bad-1")
	assertNoDurableToolCall(t, session, "bad-2")
	response, err := agent.Run(context.Background(), "continue", nil)
	if err != nil || response.Message.Content != "resumed" {
		t.Fatalf("resume response=%#v err=%v", response, err)
	}
}

func TestLengthFinishedToolCallIsDiscardedAndRetried(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "length-1", Name: "echo", Arguments: `{}`}}}, FinishReason: "length"},
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "good-1", Name: "echo", Arguments: `{}`}}}, FinishReason: "tool_calls"},
		{Message: Message{Role: RoleAssistant, Content: "done"}, FinishReason: "stop"},
	}}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "ok" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "run", nil)
	if err != nil || response.Message.Content != "done" || *executions != 1 {
		t.Fatalf("response=%#v executions=%d err=%v", response, *executions, err)
	}
}

func TestInterruptedToolStreamRetriesOnceWithoutExecutingPartialCall(t *testing.T) {
	provider := &interruptedToolRecoveryProvider{}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "ok" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "run", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if provider.calls != 3 || *executions != 1 {
		t.Fatalf("provider calls=%d executions=%d, want 3 and 1", provider.calls, *executions)
	}
}

func TestInvalidMultiToolBatchExecutesNothing(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "valid", Name: "echo", Arguments: `{}`},
			{ID: "invalid", Name: "echo", Arguments: `{"x":`},
		}}, FinishReason: "tool_calls"},
		{Message: Message{Role: RoleAssistant, Content: "recovered without tools"}, FinishReason: "stop"},
	}}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "unexpected" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "run", nil)
	if err != nil || response.Message.Content != "recovered without tools" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if *executions != 0 {
		t.Fatalf("executed %d calls from an invalid batch", *executions)
	}
}

func TestLengthFinishedWithoutToolRetriesOnceWithoutReplayingPartialOutput(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, Content: "partial"}, FinishReason: "length"},
		{Message: Message{Role: RoleAssistant, Content: "complete"}, FinishReason: "stop"},
	}}
	var rejected *ProviderOutputRejection
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, EventHook: func(event AgentEvent) {
		if event.Type == "provider_output_rejected" && event.ProviderOutput != nil {
			copy := *event.ProviderOutput
			rejected = &copy
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "answer", nil)
	if err != nil || response.Message.Content != "complete" || len(provider.requests) != 2 {
		t.Fatalf("response=%#v requests=%d err=%v", response, len(provider.requests), err)
	}
	if rejected == nil || !rejected.Retrying || rejected.FinishReason != "length" {
		t.Fatalf("rejection=%#v", rejected)
	}
	if !requestContainsLengthRecoveryInstruction(provider.requests[1]) {
		t.Fatalf("retry request lacks length recovery instruction: %#v", provider.requests[1].Messages)
	}
	for _, message := range provider.requests[1].Messages {
		if message.Content == "partial" {
			t.Fatalf("partial response replayed during retry: %#v", provider.requests[1].Messages)
		}
	}
	request, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range request.Messages {
		if message.Content == "partial" {
			t.Fatalf("partial response replayed: %#v", request.Messages)
		}
	}
}

func TestRepeatedLengthFinishedWithoutToolFailsAfterOneRetry(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant}, Reasoning: "first unfinished thought", FinishReason: "length"},
		{Message: Message{Role: RoleAssistant}, Reasoning: "second unfinished thought", FinishReason: "max_tokens"},
	}}
	var rejected []ProviderOutputRejection
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "provider_output_rejected" && event.ProviderOutput != nil {
			rejected = append(rejected, *event.ProviderOutput)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "answer", nil)
	if !errors.Is(err, ErrIncompleteProviderResponse) || !response.Message.Incomplete || len(provider.requests) != 2 {
		t.Fatalf("response=%#v requests=%d err=%v", response, len(provider.requests), err)
	}
	if len(rejected) != 2 || !rejected[0].Retrying || rejected[1].Retrying {
		t.Fatalf("rejections=%#v", rejected)
	}
}

func TestMalformedHistoricalToolGroupIsOmittedFromReplay(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad", Name: "write_file", Arguments: `{"path":"x`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleTool, Name: "write_file", ToolCallID: "bad", Content: "error"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "after"}); err != nil {
		t.Fatal(err)
	}
	messages, err := session.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != RoleUser || messages[0].Content != "after" {
		t.Fatalf("replayed malformed tool group: %#v", messages)
	}
}

func TestToolLoopLimitDoesNotPersistUnexecutedCall(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedBatchProvider{batches: []scriptedBatch{
		{calls: []ToolCall{{ID: "executed", Name: "echo", Arguments: `{}`}}},
		{calls: []ToolCall{{ID: "over-limit", Name: "echo", Arguments: `{}`}}},
	}}
	executor, executions := countedEchoExecutor(t, func(_ int, _ string) string { return "ok" })
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session, Executor: executor, MaxToolSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "run", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if *executions != 1 {
		t.Fatalf("executions=%d, want 1", *executions)
	}
	assertNoDurableToolCall(t, session, "over-limit")
	response, err := agent.Run(context.Background(), "continue", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func requestContainsRecoveryInstruction(request Request) bool {
	for _, message := range request.Messages {
		if message.Role == RoleSystem && strings.Contains(message.Content, "previous provider response was discarded") {
			return true
		}
	}
	return false
}

func requestContainsLengthRecoveryInstruction(request Request) bool {
	for _, message := range request.Messages {
		if message.Role == RoleSystem && strings.Contains(message.Content, "reached the generation limit") {
			return true
		}
	}
	return false
}

func assertNoDurableToolCall(t *testing.T, session *Session, callID string) {
	t.Helper()
	for _, event := range session.EventsSnapshot() {
		switch event.Type {
		case EventMessage:
			payload, err := DecodeMessageEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range payload.Message.ToolCalls {
				if call.ID == callID {
					t.Fatalf("malformed call %q persisted in message event", callID)
				}
			}
		case EventToolCall:
			payload, err := DecodeToolCallEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			if payload.Call.ID == callID {
				t.Fatalf("malformed call %q persisted in tool-call event", callID)
			}
		}
	}
}
