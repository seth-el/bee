package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// summaryProvider answers Complete with a fixed summary (or error) and
// records every request.
type summaryProvider struct {
	requests []Request
	summary  string
	err      error
}

func (provider *summaryProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	if provider.err != nil {
		return Response{}, provider.err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: provider.summary}}, nil
}

func (provider *summaryProvider) Stream(_ context.Context, request Request, _ StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	return Response{Message: Message{Role: RoleAssistant, Content: "done"}}, nil
}

func compactionTestSession(t *testing.T, turns int, size int) *Session {
	t.Helper()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "compaction"})
	if err != nil {
		t.Fatal(err)
	}
	for turn := 1; turn <= turns; turn++ {
		padding := strings.Repeat(" pad", size)
		for _, message := range []Message{{Role: RoleUser, Content: fmt.Sprintf("question %d%s", turn, padding)}, {Role: RoleAssistant, Content: fmt.Sprintf("answer %d", turn)}} {
			if err := session.AppendMessage(message); err != nil {
				t.Fatal(err)
			}
		}
	}
	return session
}

func latestCompaction(t *testing.T, events []Event) CompactionEvent {
	t.Helper()
	var found *CompactionEvent
	for _, event := range events {
		if event.Type != EventCompaction {
			continue
		}
		var payload CompactionEvent
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		found = &payload
		if err := VerifyCompaction(events, event); err != nil {
			t.Fatalf("verify compaction: %v", err)
		}
	}
	if found == nil {
		t.Fatal("no compaction event")
	}
	return *found
}

func joinedContents(messages []Message) string {
	var parts []string
	for _, message := range messages {
		parts = append(parts, message.Content)
	}
	return strings.Join(parts, "\n")
}

func TestModelCompactionSummaryKeepsRecentTurnsVerbatim(t *testing.T) {
	session := compactionTestSession(t, 5, 0)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "persona prompt"
	config.Context.ModelSummary = true
	config.Context.KeepRecentTurns = 2
	provider := &summaryProvider{summary: "1. Current state: testing compaction."}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.CompactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("summary requests = %d", len(provider.requests))
	}
	request := provider.requests[0]
	if request.Messages[0].Role != RoleSystem || !strings.Contains(request.Messages[0].Content, "persona prompt") {
		t.Fatalf("summary request lost the composed system prompt: %#v", request.Messages[0])
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != RoleUser || !strings.HasPrefix(last.Content, "<bee-compaction-request>") {
		t.Fatalf("last message = %#v", last)
	}
	sent := joinedContents(request.Messages)
	// Turn 5 is the current turn; turns 3 and 4 are kept; 1 and 2 are compacted.
	if !strings.Contains(sent, "question 1") || !strings.Contains(sent, "answer 2") || strings.Contains(sent, "question 3") {
		t.Fatalf("summary request carried the wrong history:\n%s", sent)
	}
	events := session.EventsSnapshot()
	payload := latestCompaction(t, events)
	if payload.Method != compactionMethodModel || !strings.HasPrefix(payload.Summary, modelSummaryHeader) || !strings.Contains(payload.Summary, "testing compaction") {
		t.Fatalf("checkpoint = %#v", payload)
	}
	replay, err := AssembleContext(events, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	text := joinedContents(replay.Messages)
	if strings.Contains(text, "question 1") || strings.Contains(text, "answer 2") {
		t.Fatalf("compacted turns still replay:\n%s", text)
	}
	for _, kept := range []string{"question 3", "answer 3", "question 4", "answer 4", "question 5", "answer 5"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("kept turn %q missing from replay:\n%s", kept, text)
		}
	}
}

func TestModelCompactionSummaryFailureFallsBackToDeterministic(t *testing.T) {
	session := compactionTestSession(t, 3, 0)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Context.ModelSummary = true
	provider := &summaryProvider{err: errors.New("backend down")}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.CompactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload := latestCompaction(t, session.EventsSnapshot())
	if payload.Method != compactionMethodModelFallback || !strings.Contains(payload.Summary, "question 1") || strings.Contains(payload.Summary, "backend down") {
		t.Fatalf("checkpoint = %#v", payload)
	}
}

func TestModelCompactionSummaryRejectsToolCalls(t *testing.T) {
	session := compactionTestSession(t, 3, 0)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Context.ModelSummary = true
	provider := &toolCallingSummaryProvider{}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.CompactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if payload := latestCompaction(t, session.EventsSnapshot()); payload.Method != compactionMethodModelFallback {
		t.Fatalf("method = %q", payload.Method)
	}
}

type toolCallingSummaryProvider struct{ summaryProvider }

func (provider *toolCallingSummaryProvider) Complete(_ context.Context, request Request) (Response, error) {
	return Response{Message: Message{Role: RoleAssistant, Content: "summary", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{}`}}}}, nil
}

func TestModelCompactionSummaryLeavesOutOldestTurnsThatDoNotFit(t *testing.T) {
	// Turn 1 is far too large to fit alongside the summary request; the
	// request drops it rather than giving up on a model summary.
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "big"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{Role: RoleUser, Content: "question 1 " + strings.Repeat("huge ", 4000)},
		{Role: RoleAssistant, Content: "answer 1"},
		{Role: RoleUser, Content: "question 2"},
		{Role: RoleAssistant, Content: "answer 2"},
		{Role: RoleUser, Content: "question 3"},
		{Role: RoleAssistant, Content: "answer 3"},
	} {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Provider.ContextWindowTokens = 4000
	config.Generation.MaxTokens = 32
	config.Context = ContextKernelConfig{ReserveTokens: 16, OutputReserveTokens: 32, ToolResultTokens: 256, SummaryTokens: 200, ModelSummary: true}
	provider := &summaryProvider{summary: "short summary"}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.CompactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("summary requests = %d", len(provider.requests))
	}
	sent := joinedContents(provider.requests[0].Messages)
	if strings.Contains(sent, "question 1") || !strings.Contains(sent, "question 2") {
		t.Fatalf("summary request history:\n%s", sent)
	}
	if payload := latestCompaction(t, session.EventsSnapshot()); payload.Method != compactionMethodModel {
		t.Fatalf("method = %q", payload.Method)
	}
}

func TestCompactionKeepBoundaryNeedsTurnsNewerThanTheLastCheckpoint(t *testing.T) {
	session := compactionTestSession(t, 5, 0)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Context.KeepRecentTurns = 2
	agent, err := NewAgentWithOptions(config, &summaryProvider{}, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.CompactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for turn := 6; turn <= 8; turn++ {
		for _, message := range []Message{{Role: RoleUser, Content: fmt.Sprintf("question %d", turn)}, {Role: RoleAssistant, Content: fmt.Sprintf("answer %d", turn)}} {
			if err := session.AppendMessage(message); err != nil {
				t.Fatal(err)
			}
		}
	}
	events := session.EventsSnapshot()
	var compactionSeq, question7 uint64
	for _, event := range events {
		if event.Type == EventCompaction {
			compactionSeq = event.Seq
		}
		if event.Type == EventMessage {
			if payload, err := DecodeMessageEvent(event); err == nil && strings.HasPrefix(payload.Message.Content, "question 7") {
				question7 = event.Seq
			}
		}
	}
	base := compactionBoundaryBeforeCurrentTurn(events)
	// Keeping turns 6 and 7 would leave nothing new to compact.
	if got := compactionKeepBoundary(events, base, 2); got != 0 {
		t.Fatalf("keep 2 boundary = %d, want 0", got)
	}
	got := compactionBoundaryKeeping(events, base, 2)
	if got <= compactionSeq || got >= question7 {
		t.Fatalf("boundary = %d, want between checkpoint %d and question 7 at %d", got, compactionSeq, question7)
	}
	// With fewer turns than requested, everything before the current turn
	// still compacts.
	short := compactionTestSession(t, 2, 0).EventsSnapshot()
	if got := compactionBoundaryKeeping(short, compactionBoundaryBeforeCurrentTurn(short), 2); got == 0 {
		t.Fatal("short session did not compact")
	}
}

func TestDeterministicSummaryKeepsStartAndEnd(t *testing.T) {
	var items []ContextItem
	for index := 0; index < 200; index++ {
		role := RoleUser
		if index%2 == 1 {
			role = RoleAssistant
		}
		items = append(items, ContextItem{Message: Message{Role: role, Content: fmt.Sprintf("msg-%03d %s", index, strings.Repeat("x", 60))}})
	}
	summary := deterministicContextSummary(items, 400)
	if len(summary) > 400*4 {
		t.Fatalf("summary is %d bytes", len(summary))
	}
	for _, want := range []string{"msg-000", "msg-199", "msg-198", "messages from the middle of the conversation omitted"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "msg-100") {
		t.Fatalf("summary kept the middle:\n%s", summary)
	}
	if strings.Index(summary, "msg-198") > strings.Index(summary, "msg-199") {
		t.Fatal("tail is out of order")
	}
	// Small conversations are unchanged.
	short := deterministicContextSummary(items[:2], 400)
	if !strings.Contains(short, "msg-000") || !strings.Contains(short, "msg-001") || strings.Contains(short, "omitted") {
		t.Fatalf("short summary = %q", short)
	}
}

func TestCompactionSummaryConfigLoads(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(`{"context":{"model_summary":true,"keep_recent_turns":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Context.ModelSummary || config.Context.KeepRecentTurns != 3 {
		t.Fatalf("context = %#v", config.Context)
	}
	if err := os.WriteFile(path, []byte(`{"context":{"keep_recent_turns":17}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "keep_recent_turns") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverflowCompactionDropsKeptTurnsWhenTheyStillDoNotFit(t *testing.T) {
	session := compactionTestSession(t, 3, 200)
	config := DefaultConfig()
	config.Agent.SystemPrompt = "small"
	config.Provider.ContextWindowTokens = 300
	config.Generation.MaxTokens = 32
	config.Context = ContextKernelConfig{ReserveTokens: 16, OutputReserveTokens: 32, ToolResultTokens: 32, SummaryTokens: 24, AutoCompact: true, KeepRecentTurns: 2}
	provider := &contextKernelCaptureProvider{response: Response{Message: Message{Role: RoleAssistant, Content: "done"}}}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "current", nil); err != nil {
		t.Fatal(err)
	}
	events := session.EventsSnapshot()
	compactions := 0
	for _, event := range events {
		if event.Type == EventCompaction {
			compactions++
		}
	}
	if compactions != 2 {
		t.Fatalf("compactions = %d, want a keep-2 pass and then a keep-0 pass", compactions)
	}
	replay, err := AssembleContext(events, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if text := joinedContents(replay.Messages); strings.Contains(text, " pad pad pad") {
		t.Fatalf("an oversized kept turn still replays:\n%s", text)
	}
}
