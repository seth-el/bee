package bee

import (
	"strings"
	"testing"
)

func TestSessionStoreSearchSessionsMatchesPersistedMessageContent(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(Project{Name: "Search Project"})
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.Create(SessionMetadata{Title: "Older", ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := older.AppendMessage(Message{Role: RoleUser, Content: "Please inspect the Honeycomb deployment."}); err != nil {
		t.Fatal(err)
	}
	if err := older.AppendToolResult(ToolResultEvent{ToolName: "deploy", Output: "Honeycomb deployment completed"}); err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(SessionMetadata{Title: "Newer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.AppendMessage(Message{Role: RoleAssistant, Content: "The HONEYCOMB report is ready."}); err != nil {
		t.Fatal(err)
	}

	results, err := store.SearchSessions(SessionSearchOptions{Query: "hOnEyCoMb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	if results[0].ID != newer.ID {
		t.Fatalf("newest result first: %#v", results)
	}
	if len(results[0].Matches) != 1 || results[0].Matches[0].Role != RoleAssistant || !strings.Contains(strings.ToLower(results[0].Matches[0].Snippet), "honeycomb") {
		t.Fatalf("newer matches = %#v", results[0].Matches)
	}
	if len(results[1].Matches) != 2 || results[1].Matches[0].Role != RoleUser || results[1].Matches[1].Role != RoleTool {
		t.Fatalf("older matches = %#v", results[1].Matches)
	}
	if len(results[0].Matches[0].Snippet) > MaxSessionSearchSnippetBytes || len(results[1].Matches[0].Snippet) > MaxSessionSearchSnippetBytes {
		t.Fatal("search snippets exceeded their bound")
	}

	filtered, err := store.SearchSessions(SessionSearchOptions{Query: "honeycomb", ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != older.ID {
		t.Fatalf("project-filtered results = %#v", filtered)
	}
	unassigned, err := store.SearchSessions(SessionSearchOptions{Query: "honeycomb", Unassigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unassigned) != 1 || unassigned[0].ID != newer.ID {
		t.Fatalf("unassigned results = %#v", unassigned)
	}
}

func TestSessionStoreSearchSessionsValidatesAndCaps(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "   ", strings.Repeat("x", MaxSessionSearchQueryBytes+1)} {
		if _, err := store.SearchSessions(SessionSearchOptions{Query: query}); err == nil {
			t.Fatalf("query %q was accepted", query)
		}
	}
	if _, err := store.SearchSessions(SessionSearchOptions{Query: "ok", Limit: -1}); err != nil {
		t.Fatalf("negative limit should use the safe default: %v", err)
	}
}
