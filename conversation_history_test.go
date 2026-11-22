package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationHistorySearchRanksBoundsAndExcludesToolData(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(SessionMetadata{ID: "first", Title: "First chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(Message{Role: RoleUser, Content: "blue key was discussed in the old tower."}); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendToolCall(ToolCall{Name: "run_command", Arguments: `{"token":"secret-token"}`}); err != nil {
		t.Fatal(err)
	}
	if err := first.AppendToolResult(ToolResultEvent{ToolName: "run_command", Output: "hidden reasoning and secret-token"}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(SessionMetadata{ID: "second", Title: "Second chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.AppendMessage(Message{Role: RoleAssistant, Content: "The blue key is in the old tower."}); err != nil {
		t.Fatal(err)
	}
	if err := second.AppendMessage(Message{Role: RoleUser, Content: "A different topic."}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "blue key", MaxResults: 1, MaxSnippetBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Hits) != 1 || result.Hits[0].SessionID != "first" {
		t.Fatalf("unexpected ranked result: %+v", result)
	}
	if result.Hits[0].Role != RoleUser || len(result.Hits[0].Snippet) > 32 || strings.Contains(result.Hits[0].Snippet, "secret") {
		t.Fatalf("unsafe or unbounded hit: %+v", result.Hits[0])
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > MaxConversationHistoryBytes {
		t.Fatalf("result exceeded byte bound: %d", len(encoded))
	}
}

func TestConversationHistorySearchCanExcludeActiveSession(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.Create(SessionMetadata{ID: "prior-session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := prior.AppendMessage(Message{Role: RoleAssistant, Content: "shared rollout phrase from prior"}); err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(SessionMetadata{ID: "active-session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := active.AppendMessage(Message{Role: RoleAssistant, Content: "shared rollout phrase from active"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "shared rollout phrase", ExcludeSessionID: active.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].SessionID != prior.ID || strings.Contains(result.Hits[0].Snippet, "active") {
		t.Fatalf("excluded-session result = %#v", result)
	}
}

func TestConversationHistorySearchFailsClosedOnMalformedTranscript(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.jsonl"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "history"}); err == nil {
		t.Fatal("malformed transcript unexpectedly searched successfully")
	}
}

func TestConversationHistorySearchRejectsActorMismatchAndChild(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorChild, ID: "child"}); err == nil {
		t.Fatal("child actor unexpectedly received history access")
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Search(context.Background(), ConversationHistorySearchRequest{Actor: MemoryActor{Kind: MemoryActorParent, ID: "other"}, Query: "history"})
	if err == nil {
		t.Fatal("actor mismatch unexpectedly authorized")
	}
}

func TestConversationHistorySearchToolAndAssistantWiring(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := app.Executor.Registry.Lookup(ConversationHistorySearchToolName)
	if !ok {
		t.Fatal("conversation_search not registered")
	}
	if tool.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("history tool mutability=%q", tool.Definition().Mutability)
	}
	if !strings.Contains(DefaultSystemPrompt, "search conversation history first") {
		t.Fatal("default prompt lacks truthful history-search guidance")
	}
}

func TestConversationHistorySearchPreservesTimestamps(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "timed", Title: "Timed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "A remembered answer"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "remembered"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].At.IsZero() || result.Hits[0].At.After(time.Now().UTC()) {
		t.Fatalf("timestamp not preserved: %+v", result)
	}
}

func TestConversationHistorySearchMatchesNaturalRecallQueries(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "natural", Title: "Natural recall"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "We decided to keep the game progress ledger separate from generic memory."}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "what did we decide about game memory"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || !strings.Contains(strings.ToLower(result.Hits[0].Snippet), "decided") {
		t.Fatalf("natural recall query did not match: %+v", result)
	}
}

func TestConversationHistorySearchUsesNewestSessionsWhenBounded(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.Create(SessionMetadata{ID: "older", Title: "Older"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(SessionMetadata{ID: "newer", Title: "Newer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []*Session{older, newer} {
		if err := session.AppendMessage(Message{Role: RoleUser, Content: "shared history topic"}); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(store.Root(), "older.jsonl"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(store.Root(), "newer.jsonl"), newTime, newTime); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "shared history", MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].SessionID != "newer" || !result.MoreAvailable {
		t.Fatalf("bounded search did not prefer newest session: %+v", result)
	}
}

func TestConversationHistorySearchFiltersByMessageTimeWindow(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "windowed", Title: "Windowed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "heron sighting one"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	middle := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "heron sighting two"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	search := func(since, before time.Time) ConversationHistorySearchResult {
		t.Helper()
		result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "heron sighting", Since: since, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if all := search(time.Time{}, time.Time{}); all.Total != 2 {
		t.Fatalf("unfiltered total = %d", all.Total)
	}
	after := search(middle, time.Time{})
	if after.Total != 1 || len(after.Hits) != 1 || !strings.Contains(after.Hits[0].Snippet, "two") {
		t.Fatalf("since filter = %+v", after)
	}
	prior := search(time.Time{}, middle)
	if prior.Total != 1 || len(prior.Hits) != 1 || !strings.Contains(prior.Hits[0].Snippet, "one") {
		t.Fatalf("before filter = %+v", prior)
	}
	// since is inclusive, before is exclusive.
	exact := after.Hits[0].At
	if got := search(exact, exact.Add(time.Nanosecond)); got.Total != 1 {
		t.Fatalf("inclusive since = %+v", got)
	}
	if got := search(time.Time{}, exact); got.Total != 1 || strings.Contains(got.Hits[0].Snippet, "two") {
		t.Fatalf("exclusive before = %+v", got)
	}
	if _, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "heron", Since: middle, Before: middle}); err == nil {
		t.Fatal("empty window accepted")
	}

	// A session file untouched since before the window cannot hold messages
	// inside it and is skipped without consuming the session budget.
	stale, err := store.Create(SessionMetadata{ID: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.AppendMessage(Message{Role: RoleUser, Content: "heron sighting stale"}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(store.root, "stale.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "heron sighting", Since: time.Now().Add(-24 * time.Hour), MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.MoreAvailable {
		t.Fatalf("stale session consumed the bounded session budget: %+v", result)
	}
	for _, hit := range result.Hits {
		if hit.SessionID == "stale" {
			t.Fatalf("stale session searched: %+v", result)
		}
	}
}

func TestConversationHistorySearchToolParsesTimeWindow(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "tooled"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "kestrel note"}); err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"}
	service, err := NewConversationHistorySearch(store, actor)
	if err != nil {
		t.Fatal(err)
	}
	tool := ConversationHistorySearchTool{Service: service, Actor: actor}
	if schema := string(tool.Definition().Parameters); !strings.Contains(schema, `"since"`) || !strings.Contains(schema, `"before"`) {
		t.Fatalf("schema lacks time window: %s", schema)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"kestrel","since":"`+future+`"}`))
	if err != nil || !strings.Contains(output, `"total":0`) {
		t.Fatalf("future since output=%s err=%v", output, err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	output, err = tool.Execute(context.Background(), json.RawMessage(`{"query":"kestrel","since":"`+past+`","before":"`+future+`"}`))
	if err != nil || !strings.Contains(output, "kestrel") {
		t.Fatalf("bounded window output=%s err=%v", output, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"kestrel","since":"yesterday"}`)); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("non-RFC3339 since err=%v", err)
	}
}
