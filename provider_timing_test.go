package bee

import (
	"context"
	"strings"
	"testing"
	"time"
)

type steppedClock struct{ now time.Time }

func (clock *steppedClock) Now() time.Time          { return clock.now }
func (clock *steppedClock) advance(d time.Duration) { clock.now = clock.now.Add(d) }

// timedProvider streams one reply on a fake clock: prefill before the first
// token, then decode.
type timedProvider struct {
	clock              *steppedClock
	prefill, decode    time.Duration
	prompt, completion int
}

func (provider *timedProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.Stream(ctx, request, nil)
}

func (provider *timedProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.clock.advance(provider.prefill)
	if onDelta != nil {
		if err := onDelta("reply"); err != nil {
			return Response{}, err
		}
	}
	provider.clock.advance(provider.decode)
	return Response{Message: Message{Role: RoleAssistant, Content: "reply"}, Usage: Usage{PromptTokens: provider.prompt, CompletionTokens: provider.completion, TotalTokens: provider.prompt + provider.completion, CacheReadTokens: provider.prompt / 2}}, nil
}

func timedAgent(t *testing.T, provider *timedProvider, window int) (*Agent, *[]AgentEvent) {
	t.Helper()
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	config.Provider.ContextWindowTokens = window
	var events []AgentEvent
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{EventHook: func(event AgentEvent) { events = append(events, event) }})
	if err != nil {
		t.Fatal(err)
	}
	agent.clock = provider.clock.Now
	return agent, &events
}

func usageTiming(t *testing.T, events []AgentEvent) *ProviderCallTiming {
	t.Helper()
	for _, event := range events {
		if event.Type == "usage" {
			return event.Timing
		}
	}
	t.Fatal("no usage event")
	return nil
}

func TestProviderCallTimingFromStream(t *testing.T) {
	clock := &steppedClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	provider := &timedProvider{clock: clock, prefill: 1200 * time.Millisecond, decode: 2 * time.Second, prompt: 5000, completion: 80}
	agent, events := timedAgent(t, provider, 32768)
	if _, err := agent.Run(context.Background(), "hello", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	timing := usageTiming(t, *events)
	want := ProviderCallTiming{DurationMs: 3200, TimeToFirstTokenMs: 1200, DecodeTokensPerSecond: 40, ContextWindowTokens: 32768, ContextUsedTokens: 5000}
	if timing == nil || *timing != want {
		t.Fatalf("timing = %#v, want %#v", timing, want)
	}
	status, err := agent.contextStatus()
	if err != nil || status.LastProviderCall == nil || *status.LastProviderCall != want {
		t.Fatalf("context_status = %#v, %v", status.LastProviderCall, err)
	}
	if got := formatProviderCallTiming(want); got != "3.2s · first token 1.2s · 40.0 tok/s · context 5,000 / 32,768 (15%)" {
		t.Fatalf("formatted = %q", got)
	}
}

func TestProviderCallTimingWithoutUsageOrWindow(t *testing.T) {
	clock := &steppedClock{now: time.Unix(0, 0)}
	// No usage block: no decode rate; context falls back to Bee's estimate.
	provider := &timedProvider{clock: clock, prefill: 300 * time.Millisecond, decode: 10 * time.Millisecond}
	agent, events := timedAgent(t, provider, 0)
	if _, err := agent.Run(context.Background(), "hello", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	timing := usageTiming(t, *events)
	if timing == nil || timing.DurationMs != 310 || timing.TimeToFirstTokenMs != 300 || timing.DecodeTokensPerSecond != 0 {
		t.Fatalf("timing = %#v", timing)
	}
	if timing.ContextUsedTokens > 0 && !timing.ContextUsedEstimated {
		t.Fatalf("estimate not marked: %#v", timing)
	}
}

func TestRunPerformanceSummary(t *testing.T) {
	var perf runPerformance
	perf.add(&UsageEvent{Usage: Usage{CompletionTokens: 40}, Timing: &ProviderCallTiming{DurationMs: 2000, TimeToFirstTokenMs: 1000, DecodeTokensPerSecond: 40}})
	perf.add(&UsageEvent{Usage: Usage{CompletionTokens: 120}, Timing: &ProviderCallTiming{DurationMs: 4000, TimeToFirstTokenMs: 1000, DecodeTokensPerSecond: 40}})
	perf.add(&UsageEvent{Usage: Usage{CompletionTokens: 5}}) // untimed: ignored
	if got := perf.summary(9 * time.Second); got != "model 6.0s (first token avg 1.0s, 40 tok/s) · other 3.0s" {
		t.Fatalf("summary = %q", got)
	}
	if got := (runPerformance{}).summary(time.Second); got != "" {
		t.Fatalf("empty summary = %q", got)
	}
	if !strings.HasPrefix(formatPerfSeconds(12_400), "12s") {
		t.Fatalf("long duration = %q", formatPerfSeconds(12_400))
	}
}

func TestProviderCallTimingIsPersistedWithSessionUsage(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Specialist.Enabled = false
	provider := &timedProvider{clock: &steppedClock{now: time.Unix(0, 0)}, prompt: 10, completion: 3}
	app, err := NewApplication(config, provider, store)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var id SessionID
	var runTiming *ProviderCallTiming
	for event := range events {
		if event.SessionID != "" {
			id = event.SessionID
		}
		if event.Type == RunUsage && event.Usage != nil {
			runTiming = event.Usage.Timing
		}
	}
	if runTiming == nil {
		t.Fatal("run usage event has no timing")
	}
	session, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range session.Events {
		if event.Type == EventUsage {
			payload, err := decodeEventPayload[UsageEvent](event, EventUsage)
			found = err == nil && payload.Timing != nil && payload.Usage.CompletionTokens == 3
		}
	}
	if !found {
		t.Fatal("session usage event lacks timing")
	}
}
