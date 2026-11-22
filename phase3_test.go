package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionJSONLReplayAndTypedEvents(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "test", ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "call-1", Name: "echo", Arguments: `{"value":"x"}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-1", ToolName: "echo", EffectiveToolName: "echo_readonly", Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := session.RecordUsage("test", "model", Usage{PromptTokens: 2, TotalTokens: 3}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) != 6 {
		t.Fatalf("events=%d", len(loaded.Events))
	}
	messages, err := loaded.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Content != "hello" || messages[1].ToolCalls[0].Name != "echo" || messages[2].Content != "ok" {
		t.Fatalf("messages=%#v", messages)
	}
	for _, event := range loaded.EventsSnapshot() {
		if event.Type != EventToolResult {
			continue
		}
		result, err := DecodeToolResultEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result.ToolName != "echo" || result.EffectiveToolName != "echo_readonly" {
			t.Fatalf("tool result attribution=%#v", result)
		}
	}
	data, err := os.ReadFile(filepath.Join(store.Root(), string(session.ID)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `value`) {
		t.Fatal("durable tool payload was not persisted faithfully")
	}
}

func TestSessionStoreCrossInstanceAppendKeepsSequencesContiguous(t *testing.T) {
	root := t.TempDir()
	first, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := first.Create(SessionMetadata{Title: "concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 4
	const appends = 12
	errs := make(chan error, workers*appends)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		store := first
		if worker%2 == 1 {
			store = second
		}
		group.Add(1)
		go func(worker int, store *SessionStore) {
			defer group.Done()
			for index := 0; index < appends; index++ {
				if _, err := store.Append(session.ID, Event{Type: EventMessage, Data: mustJSON(MessageEvent{Message: Message{Role: RoleUser, Content: fmt.Sprintf("%d-%d", worker, index)}})}); err != nil {
					errs <- err
				}
			}
		}(worker, store)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	loaded, err := first.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(loaded.Events), 2+workers*appends; got != want {
		t.Fatalf("events=%d want=%d", got, want)
	}
	for index, event := range loaded.Events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("event %d sequence=%d", index, event.Seq)
		}
	}
}

func TestSessionStoreToleratesTruncatedFinalLineButDetectsCorruption(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), string(session.ID)+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"id":"truncated"`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := store.Load(session.ID); err != nil {
		t.Fatalf("truncated final line: %v", err)
	}
	if _, err := store.Append(session.ID, Event{Type: EventMessage, Data: mustJSON(MessageEvent{Message: Message{Role: RoleUser, Content: "api_key=secret"}})}); err != nil {
		t.Fatalf("append after truncated final line: %v", err)
	}
	if _, err := store.Load(session.ID); err != nil {
		t.Fatalf("load after repairing truncated final line: %v", err)
	}
	if data := mustRead(t, path); !strings.Contains(string(data), "api_key=secret") {
		t.Fatal("durable message was not persisted faithfully")
	}
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("\nnot-json\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(session.ID); err == nil {
		t.Fatal("expected corruption error")
	}
}

func TestAgentSessionResumePreservesSystemAndCompactionContext(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := session.Compact(3, "summary of old work"); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "ok"}}}}
	config := DefaultConfig()
	config.Agent.SystemPrompt = "system"
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) < 2 || messages[0].Content != "system" || messages[1].Content != "summary of old work" {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestSessionListForkAndCompaction(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Create(SessionMetadata{Title: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if err := original.AppendMessage(Message{Role: RoleUser, Content: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := original.AppendMessage(Message{Role: RoleAssistant, Content: "new"}); err != nil {
		t.Fatal(err)
	}
	fork, err := store.Fork(original.ID, uint64(len(original.Events)), SessionMetadata{Title: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	if fork.Metadata.ParentID != original.ID {
		t.Fatalf("metadata=%#v", fork.Metadata)
	}
	if err := store.Compact(original.ID, 3, "summary"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := AssembleContext(loaded.Events, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[0].Content != "summary" || result.Messages[1].Content != "new" {
		t.Fatalf("context=%#v", result.Messages)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list=%#v", list)
	}
}

func TestSessionStoreListsNewestUpdatedFirstAndDeletesSafely(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.Create(SessionMetadata{Title: "older"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(SessionMetadata{Title: "newer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(older.ID, Event{Type: EventMessage, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Data: mustJSON(MessageEvent{Message: Message{Role: RoleUser, Content: "old"}})}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(newer.ID, Event{Type: EventMessage, At: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Data: mustJSON(MessageEvent{Message: Message{Role: RoleUser, Content: "new"}})}); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("newest-first list = %#v", list)
	}
	if err := store.Delete(older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(older.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("deleted session load error = %v", err)
	}
	if err := store.Delete(older.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("second delete error = %v", err)
	}
	for _, id := range []SessionID{"../escape", `nested\\escape`, ""} {
		if err := store.Delete(id); err == nil {
			t.Fatalf("unsafe session id %q was accepted", id)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(store.Root(), "linked.jsonl")
	if err := os.Symlink(outside, linkedPath); err != nil {
		t.Logf("symlink safety check skipped: %v", err)
	} else {
		if err := store.Delete("linked"); err == nil {
			t.Fatal("symlinked session path was deleted")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("symlink target was affected: %v", err)
		}
	}
}

func TestContextOrderingAndBudget(t *testing.T) {
	id := SessionID("ctx")
	events := make([]Event, 0, 3)
	for _, message := range []Message{{Role: RoleUser, Content: "old old old"}, {Role: RoleAssistant, Content: "middle"}, {Role: RoleUser, Content: "recent"}} {
		event, err := NewMessageEvent(id, message)
		if err != nil {
			t.Fatal(err)
		}
		event.Seq = uint64(len(events) + 1)
		events = append(events, event)
	}
	result, err := AssembleContext(events, ContextOptions{SystemPrompt: "system", Budget: 12, RecentMessages: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) < 2 || result.Messages[0].Role != RoleSystem || result.Messages[len(result.Messages)-1].Content != "recent" {
		t.Fatalf("messages=%#v used=%d", result.Messages, result.Used)
	}
}

type phase3Summarizer struct{}

func (phase3Summarizer) Summarize(context.Context, []ContextItem, int) (ContextItem, error) {
	return ContextItem{Source: ContextSourceSummary, Required: true, Priority: 900, Message: Message{Role: RoleSystem, Content: "summarized"}}, nil
}

func TestSummarizerSeamAndAgentUsageTracking(t *testing.T) {
	id := SessionID("ctx")
	event, _ := NewMessageEvent(id, Message{Role: RoleUser, Content: strings.Repeat("x", 100)})
	event.Seq = 1
	result, err := AssembleContext([]Event{event}, ContextOptions{Budget: 1, Summarizer: phase3Summarizer{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected summarized context")
	}

	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "done"}, Usage: Usage{TotalTokens: 4}, Model: "m"}}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range loaded.Events {
		if event.Type == EventUsage {
			found = true
		}
	}
	if !found {
		t.Fatal("usage event missing")
	}
}

func TestProjectInstructionPrecedence(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Dir(root)
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("sub"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProjectInstructions(root, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Files) < 3 || loaded.Files[len(loaded.Files)-1].Content != "sub" {
		t.Fatalf("files=%#v", loaded.Files)
	}
	if strings.Index(loaded.Content, "parent") > strings.Index(loaded.Content, "root") || strings.Index(loaded.Content, "root") > strings.Index(loaded.Content, "sub") {
		t.Fatalf("content=%q", loaded.Content)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }
