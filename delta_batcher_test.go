package bee

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAssistantDeltaBatcherImmediateFirstRapidCoalescingAndFinalFlush(t *testing.T) {
	at := time.Unix(100, 0)
	var got []string
	batcher := newAssistantDeltaBatcher(context.Background(), func(delta string) error {
		got = append(got, delta)
		return nil
	})
	batcher.now = func() time.Time { return at }
	if err := batcher.Add("A"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("first delta = %#v, want immediate delivery", got)
	}
	at = at.Add(5 * time.Millisecond)
	if err := batcher.Add("B"); err != nil {
		t.Fatal(err)
	}
	at = at.Add(5 * time.Millisecond)
	if err := batcher.Add("C"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("rapid deltas flushed too early = %#v", got)
	}
	if err := batcher.Flush(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"A", "BC"}) {
		t.Fatalf("final coalesced deltas = %#v, want [A BC]", got)
	}
}

func TestAssistantDeltaBatcherTimeAndByteBounds(t *testing.T) {
	at := time.Unix(200, 0)
	var got []string
	batcher := newAssistantDeltaBatcher(context.Background(), func(delta string) error {
		got = append(got, delta)
		return nil
	})
	batcher.now = func() time.Time { return at }
	_ = batcher.Add("first")
	at = at.Add(assistantDeltaBatchWindow)
	if err := batcher.Add("second"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "second" {
		t.Fatalf("time bound output = %#v", got)
	}
	big := strings.Repeat("x", assistantDeltaBatchMaxBytes)
	if err := batcher.Add(big); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2] != big {
		t.Fatalf("byte bound output = count %d last %d bytes", len(got), len(got[len(got)-1]))
	}
}

func TestAssistantDeltaBatcherCancellationDropsPendingWithoutPostCancelEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	batcher := newAssistantDeltaBatcher(ctx, func(delta string) error {
		got = append(got, delta)
		return nil
	})
	batcher.now = func() time.Time { return time.Unix(300, 0) }
	if err := batcher.Add("first"); err != nil {
		t.Fatal(err)
	}
	if err := batcher.Add("pending"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := batcher.Flush(); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush after cancellation = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("post-cancel output = %#v", got)
	}
	if err := batcher.Add("late"); !errors.Is(err, context.Canceled) {
		t.Fatalf("late add = %v, want context.Canceled", err)
	}
}

func TestAgentFlushesDeltasBeforeAssistantMessage(t *testing.T) {
	provider := rapidDeltaProvider{}
	config := DefaultConfig()
	config.Provider.Model = "batch-test"
	var order []string
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{EventHook: func(event AgentEvent) {
		if event.Type == "assistant_message" {
			order = append(order, event.Type)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	response, err := agent.Run(context.Background(), "hello", func(delta string) error {
		deltas = append(deltas, delta)
		order = append(order, "delta:"+delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "abcdef" || !reflect.DeepEqual(deltas, []string{"a", "bcdef"}) {
		t.Fatalf("response=%q deltas=%#v", response.Message.Content, deltas)
	}
	if !reflect.DeepEqual(order, []string{"delta:a", "delta:bcdef", "assistant_message"}) {
		t.Fatalf("event order = %#v", order)
	}
}

func TestAgentFlushesFinalRemainderBeforeProviderFailure(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "batch-failure-test"
	agent, err := NewAgent(config, failingRapidProvider{})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	_, err = agent.Run(context.Background(), "hello", func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("provider failure = %v", err)
	}
	if !reflect.DeepEqual(deltas, []string{"x", "y"}) {
		t.Fatalf("failure remainder = %#v, want [x y]", deltas)
	}
}

func TestAgentOrdersToolEventsAfterFlushedAssistantDelta(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "batch-tool-test"
	provider := &toolThenAnswerProvider{}
	var order []string
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{
		Executor:  testEchoExecutor(t),
		EventHook: func(event AgentEvent) { order = append(order, event.Type) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	if _, err := agent.Run(context.Background(), "hello", func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deltas, []string{"draft", "answer"}) {
		t.Fatalf("tool-loop deltas = %#v", deltas)
	}
	want := []string{"context_usage", "assistant_message", "usage", "tool_call", "tool_result", "context_usage", "assistant_message", "usage"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("tool-loop order = %#v, want %#v", order, want)
	}
}

type rapidDeltaProvider struct{}

func (rapidDeltaProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (rapidDeltaProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	for _, delta := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := onDelta(delta); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "abcdef"}}, nil
}

type failingRapidProvider struct{}

func (failingRapidProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (failingRapidProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	if err := onDelta("x"); err != nil {
		return Response{}, err
	}
	if err := onDelta("y"); err != nil {
		return Response{}, err
	}
	return Response{}, errors.New("provider failed")
}

type toolThenAnswerProvider struct {
	turn int
}

func (provider *toolThenAnswerProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *toolThenAnswerProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.turn++
	if provider.turn == 1 {
		if err := onDelta("draft"); err != nil {
			return Response{}, err
		}
		return Response{Message: Message{Role: RoleAssistant, Content: "draft", ToolCalls: []ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}}}}, nil
	}
	if err := onDelta("answer"); err != nil {
		return Response{}, err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "answer"}}, nil
}
