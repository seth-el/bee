package bee

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type timeSyncProvider struct {
	requests  []Request
	responses []Response
}

func (provider *timeSyncProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *timeSyncProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	response := provider.responses[0]
	provider.responses = provider.responses[1:]
	if onDelta != nil && response.Message.Content != "" {
		if err := onDelta(response.Message.Content); err != nil {
			return Response{}, err
		}
	}
	return response, nil
}

func newTimeSyncSession(t *testing.T) *Session {
	t.Helper()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func clockMessages(messages []Message) []Message {
	var result []Message
	for _, message := range messages {
		if message.Role == RoleSystem && strings.HasPrefix(message.Content, timeSyncMessagePrefix) {
			result = append(result, message)
		}
	}
	return result
}

func TestTimeSyncIncludesLocalWeekdayAndPreservesLegacyRecords(t *testing.T) {
	payload, err := NewTimeSyncPayload(time.Date(2026, time.July, 1, 3, 0, 0, 0, time.UTC), "America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	if payload.Local != "2026-06-30T20:00:00-07:00" || payload.Weekday != "Tuesday" {
		t.Fatalf("local clock = %#v, want Tuesday before the UTC date boundary", payload)
	}
	message, err := timeSyncControlMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `<bee-clock utc="2026-07-01T03:00:00Z" local="2026-06-30T20:00:00-07:00" weekday="Tuesday" tz="America/Los_Angeles"/>`
	if message.Content != want {
		t.Fatalf("clock message = %q, want %q", message.Content, want)
	}

	legacy := payload
	legacy.Weekday = ""
	legacyMessage, err := timeSyncControlMessage(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyWant := `<bee-clock utc="2026-07-01T03:00:00Z" local="2026-06-30T20:00:00-07:00" tz="America/Los_Angeles"/>`
	if legacyMessage.Content != legacyWant {
		t.Fatalf("legacy clock message = %q, want byte-stable %q", legacyMessage.Content, legacyWant)
	}

	payload.Weekday = "Wednesday"
	if err := payload.validate(); err == nil {
		t.Fatal("mismatched local weekday unexpectedly validated")
	}
}

func TestTimeSyncOncePerTurnReplayAndStablePrefix(t *testing.T) {
	session := newTimeSyncSession(t)
	provider := &timeSyncProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "clock-tool", Type: "function", Name: "echo", Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "first done"}},
		{Message: Message{Role: RoleAssistant, Content: "second done"}},
	}}
	times := []time.Time{
		time.Date(2026, time.July, 1, 12, 0, 0, 987, time.UTC),
		time.Date(2026, time.July, 1, 12, 5, 0, 123, time.UTC),
	}
	clockCalls := 0
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{
		Session:          session,
		Executor:         testEchoExecutor(t),
		TimeSyncEnabled:  true,
		TimeSyncTimeZone: "America/Los_Angeles",
		TimeSyncClock: func() time.Time {
			value := times[clockCalls]
			clockCalls++
			return value
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if clockCalls != 2 {
		t.Fatalf("clock calls = %d, want one per user turn", clockCalls)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("provider requests = %d, want two tool-loop calls plus next turn", len(provider.requests))
	}
	firstClock := `<bee-clock utc="2026-07-01T12:00:00Z" local="2026-07-01T05:00:00-07:00" weekday="Wednesday" tz="America/Los_Angeles"/>`
	for index, request := range provider.requests[:2] {
		clocks := clockMessages(request.Messages)
		if len(clocks) != 1 || clocks[0].Content != firstClock {
			t.Fatalf("request %d clocks = %#v", index, clocks)
		}
	}
	lastClocks := clockMessages(provider.requests[2].Messages)
	if len(lastClocks) != 2 || lastClocks[1].Content != `<bee-clock utc="2026-07-01T12:05:00Z" local="2026-07-01T05:05:00-07:00" weekday="Wednesday" tz="America/Los_Angeles"/>` {
		t.Fatalf("second-turn clocks = %#v", lastClocks)
	}
	if !reflect.DeepEqual(provider.requests[1].Messages[:len(provider.requests[0].Messages)], provider.requests[0].Messages) {
		t.Fatalf("same-turn provider retry changed existing prefix:\nfirst=%#v\nsecond=%#v", provider.requests[0].Messages, provider.requests[1].Messages)
	}
	if !reflect.DeepEqual(provider.requests[2].Messages[:len(provider.requests[1].Messages)], provider.requests[1].Messages) {
		t.Fatalf("next turn did not extend previous request prefix:\nprevious=%#v\nnext=%#v", provider.requests[1].Messages, provider.requests[2].Messages)
	}

	events := session.EventsSnapshot()
	timeEvents := 0
	for _, event := range events {
		if event.Type == EventTimeSync {
			timeEvents++
		}
	}
	if timeEvents != 2 {
		t.Fatalf("time_sync events = %d, want 2", timeEvents)
	}
	replayed, err := AssembleContext(events, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	originalMessages := agent.Messages()
	durableStart := len(originalMessages) - len(replayed.Messages)
	if durableStart < 0 || !reflect.DeepEqual(replayed.Messages, originalMessages[durableStart:]) {
		t.Fatalf("replayed durable transcript differs:\noriginal=%#v\nreplayed=%#v", originalMessages, replayed.Messages)
	}
	resumed, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Session: session, TimeSyncEnabled: true, TimeSyncTimeZone: "America/Los_Angeles"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.Messages(), originalMessages) {
		t.Fatalf("resumed provider transcript differs:\noriginal=%#v\nresumed=%#v", originalMessages, resumed.Messages())
	}
	visible, err := MessagesFromEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if clocks := clockMessages(visible); len(clocks) != 0 {
		t.Fatalf("visible history contains clock records: %#v", clocks)
	}
}

func TestTimeSyncCompactionDropsObsoleteClock(t *testing.T) {
	session := newTimeSyncSession(t)
	payload, err := NewTimeSyncPayload(time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC), "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessageWithTimeSync(Message{Role: RoleUser, Content: "old turn"}, payload); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "old answer"}); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	events := session.EventsSnapshot()
	if err := agent.compactContextThrough(context.Background(), events, events[len(events)-1].Seq, "test/"); err != nil {
		t.Fatal(err)
	}
	compactedEvents := session.EventsSnapshot()
	result, err := AssembleContext(compactedEvents, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if clocks := clockMessages(result.Messages); len(clocks) != 0 {
		t.Fatalf("compacted context contains obsolete clock: %#v", clocks)
	}
	if len(result.Messages) != 1 || !strings.Contains(result.Messages[0].Content, "old turn") || strings.Contains(result.Messages[0].Content, timeSyncMessagePrefix) {
		t.Fatalf("compacted messages = %#v", result.Messages)
	}
	for index := len(compactedEvents) - 1; index >= 0; index-- {
		if compactedEvents[index].Type != EventCompaction {
			continue
		}
		var compaction CompactionEvent
		if err := json.Unmarshal(compactedEvents[index].Data, &compaction); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(compaction.Summary, timeSyncMessagePrefix) {
			t.Fatalf("compaction summary retained stale clock: %q", compaction.Summary)
		}
		return
	}
	t.Fatal("compaction event was not recorded")
}

func TestTimeSyncDisabledLeavesSessionBehaviorUnchanged(t *testing.T) {
	session := newTimeSyncSession(t)
	provider := &fakeProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if clocks := clockMessages(provider.requests[0].Messages); len(clocks) != 0 {
		t.Fatalf("disabled request clocks = %#v", clocks)
	}
	for _, event := range session.EventsSnapshot() {
		if event.Type == EventTimeSync {
			t.Fatalf("disabled session recorded time_sync: %#v", event)
		}
	}
}

func TestGeneralAssistantTimeSyncUsesEffectiveWebTimeZone(t *testing.T) {
	config := DefaultConfig()
	config.Web.TimeZone = "America/Chicago"
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !app.TimeSyncEnabled || app.TimeSyncTimeZone != config.Web.EffectiveTimeZone() {
		t.Fatalf("application time sync = enabled:%v zone:%q", app.TimeSyncEnabled, app.TimeSyncTimeZone)
	}
	session, err := app.Store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	options := app.agentOptions(nil, session, "")
	if !options.TimeSyncEnabled || options.TimeSyncTimeZone != "America/Chicago" {
		t.Fatalf("agent options time sync = enabled:%v zone:%q", options.TimeSyncEnabled, options.TimeSyncTimeZone)
	}
}
