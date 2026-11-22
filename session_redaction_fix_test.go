package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type requestCaptureProvider struct {
	requests []Request
}

func (provider *requestCaptureProvider) Complete(context.Context, Request) (Response, error) {
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
}

func (provider *requestCaptureProvider) Stream(_ context.Context, request Request, _ StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
}

func TestSessionDurableHistoryPreservesNestedTokenRedactionShape(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	original := `{"command":"kubectl exec -n plex plex -- sh -c 'grep -oE \"PlexOnlineToken=super-secret\" /config/Preferences.xml'"}`
	if err := session.AppendToolCall(ToolCall{ID: "call-1", Type: "function", Name: "exec", Arguments: original}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := DecodeToolCallEvent(loaded.Events[len(loaded.Events)-1])
	if err != nil {
		t.Fatal(err)
	}
	if payload.Call.Arguments != original {
		t.Fatalf("arguments changed: %q", payload.Call.Arguments)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(store.Root(), string(session.ID)+".jsonl"))), "super-secret") {
		t.Fatal("nested secret-like command text was redacted before persistence")
	}
}

func TestAppendReloadProviderRequestKeepsValidToolArguments(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	arguments := `{"command":"printf 'TOKEN=abc'"}`
	if err := session.AppendToolCall(ToolCall{ID: "call-1", Name: "exec", Arguments: arguments}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-1", ToolName: "exec", Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &requestCaptureProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: loaded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Messages) == 0 {
		t.Fatalf("requests=%#v", provider.requests)
	}
	found := false
	for _, message := range provider.requests[0].Messages {
		for _, call := range message.ToolCalls {
			found = true
			var object map[string]any
			if err := json.Unmarshal([]byte(call.Arguments), &object); err != nil {
				t.Fatalf("provider received invalid arguments: %v", err)
			}
			if !strings.Contains(call.Arguments, "TOKEN=abc") {
				t.Fatal("provider request lost faithful argument content")
			}
		}
	}
	if !found {
		t.Fatal("provider request did not contain historical tool call")
	}
}

func TestMalformedDurableHistoryIsOmittedBeforeProviderDispatch(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "bad", Name: "exec", Arguments: `{"command":"TOKEN=[REDACTED]"oops"}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "bad", ToolName: "exec", Output: "ok"}); err != nil {
		t.Fatal(err)
	}
	provider := &requestCaptureProvider{}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: loaded})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "continue", nil)
	if err != nil || response.Message.Content != "ok" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests=%d, want 1", len(provider.requests))
	}
	for _, message := range provider.requests[0].Messages {
		for _, call := range message.ToolCalls {
			if call.ID == "bad" {
				t.Fatalf("malformed historical call reached provider: %#v", provider.requests[0].Messages)
			}
		}
	}
}

func TestRepairSessionDryRunApplyBackupPermissionsAndIdempotence(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "preserve me"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "bad", Name: "exec", Arguments: `{"command":"TOKEN=[REDACTED]"oops"}`}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), string(session.ID)+".jsonl")
	before := string(mustRead(t, path))
	dry, err := store.RepairSession(session.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || !dry.Changed || dry.InvalidToolCalls != 1 || dry.BackupPath != "" || string(mustRead(t, path)) != before {
		t.Fatalf("dry report=%#v", dry)
	}
	applied, err := store.RepairSession(session.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.BackupPath == "" || applied.InvalidToolCalls != 1 {
		t.Fatalf("apply report=%#v", applied)
	}
	backupInfo, err := os.Stat(applied.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%o", backupInfo.Mode().Perm())
	}
	sessionInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if sessionInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session mode=%o", sessionInfo.Mode().Perm())
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("session root mode=%o", rootInfo.Mode().Perm())
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) != len(session.Events) || !strings.Contains(string(mustRead(t, path)), "preserve me") {
		t.Fatalf("events=%d", len(loaded.Events))
	}
	var repaired ToolCallEvent
	for _, event := range loaded.Events {
		if event.Type == EventToolCall {
			repaired, err = DecodeToolCallEvent(event)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(repaired.Call.Arguments), &object); err != nil || object["_bee_repaired"] != true {
		t.Fatalf("repaired args=%q object=%#v err=%v", repaired.Call.Arguments, object, err)
	}
	second, err := store.RepairSession(session.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || second.Applied || second.BackupPath != "" {
		t.Fatalf("repair was not idempotent: %#v", second)
	}
}

func TestOpenAIProviderRejectsMalformedToolArgumentsLocally(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:1", Model: "test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "exec", Arguments: `{"x":`}}}}})
	var argumentErr *ToolCallArgumentsError
	if !errors.As(err, &argumentErr) {
		t.Fatalf("error=%v, want local typed validation", err)
	}
}

func TestProviderRejectsMalformedToolSequences(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:1", Model: "test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		messages []Message
	}{
		{
			name: "empty call id",
			messages: []Message{
				{Role: RoleAssistant, ToolCalls: []ToolCall{{Arguments: `{}`}}},
				{Role: RoleTool, ToolCallID: "", Content: "ok"},
			},
		},
		{
			name:     "duplicate call id",
			messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "same", Arguments: `{}`}, {ID: "same", Arguments: `{}`}}}},
		},
		{
			name:     "orphan result",
			messages: []Message{{Role: RoleTool, ToolCallID: "missing", Content: "ok"}},
		},
		{
			name: "mismatched result",
			messages: []Message{
				{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "expected", Arguments: `{}`}}},
				{Role: RoleTool, ToolCallID: "other", Content: "ok"},
			},
		},
		{
			name: "duplicate result",
			messages: []Message{
				{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call", Arguments: `{}`}}},
				{Role: RoleTool, ToolCallID: "call", Content: "one"},
				{Role: RoleTool, ToolCallID: "call", Content: "two"},
			},
		},
		{
			name:     "dangling call",
			messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "dangling", Arguments: `{}`}}}},
		},
		{
			name: "intervening message",
			messages: []Message{
				{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call", Arguments: `{}`}}},
				{Role: RoleUser, Content: "intervening"},
				{Role: RoleTool, ToolCallID: "call", Content: "late"},
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := provider.Complete(context.Background(), Request{Messages: test.messages})
			var sequenceErr *ToolSequenceError
			if !errors.As(err, &sequenceErr) {
				t.Fatalf("error=%v, want ToolSequenceError", err)
			}
		})
	}
	valid, err := NormalizeRequest(Request{Model: "test", Messages: []Message{
		{Role: RoleUser, Content: "run"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "one", Name: "a", Arguments: `{}`}, {ID: "two", Name: "b", Arguments: `{}`}}},
		{Role: RoleTool, ToolCallID: "one", Content: "a"},
		{Role: RoleTool, ToolCallID: "two", Content: "b"},
		{Role: RoleAssistant, Content: "done"},
	}})
	if err != nil || len(valid.Messages) != 5 || valid.Messages[1].ToolCalls[0].ID != "one" {
		t.Fatalf("valid sequence changed or failed: %#v err=%v", valid, err)
	}
}

func TestSessionReplayOmitsInvalidGroupsButPreservesValidGroups(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "valid", Name: "lookup", Arguments: `{}`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleTool, ToolCallID: "valid", Name: "lookup", Content: "valid result"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "dangling", Name: "exec", Arguments: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "orphan", ToolName: "exec", Output: "orphan result"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "after"}); err != nil {
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
	if len(messages) != 3 || messages[0].ToolCalls[0].ID != "valid" || messages[1].ToolCallID != "valid" || messages[2].Content != "after" {
		t.Fatalf("replayed messages = %#v", messages)
	}
}

func TestRepairedMalformedAndFinalDanglingHistoryIsSafeToReplay(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "corrupt", Name: "exec", Arguments: `{"command":"TOKEN=[REDACTED]"oops"}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "final-dangling", Name: "exec", Arguments: `{}`}); err != nil {
		t.Fatal(err)
	}
	report, err := store.RepairSession(session.ID, true)
	if err != nil || report.InvalidToolCalls != 1 || !strings.Contains(report.ReplayBehavior, "omitted") {
		t.Fatalf("repair report = %#v err=%v", report, err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &requestCaptureProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Session: loaded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	for _, message := range provider.requests[0].Messages {
		if len(message.ToolCalls) != 0 {
			t.Fatalf("invalid historical tool group reached provider: %#v", provider.requests[0].Messages)
		}
	}
}

func TestNewSessionStoreRejectsSymlinkAncestor(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "linked")
	if err := os.Symlink(target, linked); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewSessionStore(filepath.Join(linked, "sessions")); err == nil {
		t.Fatal("session store accepted a symlink ancestor")
	}
}
