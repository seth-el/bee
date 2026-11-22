package bee

import (
	"os"
	"strings"
	"testing"
)

func TestSessionStoreIndexRebuildsLegacyAndRecoversCorruption(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "legacy", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "legacy searchable phrase"}); err != nil {
		t.Fatal(err)
	}
	indexPath := store.indexPath(session.ID)
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "legacy" || list[0].EventCount != 3 {
		t.Fatalf("rebuilt list = %#v", list)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("legacy index was not rebuilt: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := store.SearchSessions(SessionSearchOptions{Query: "searchable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Matches[0].Snippet, "searchable") {
		t.Fatalf("recovered search = %#v", results)
	}
}

func TestSessionStoreIndexTracksAppendMetadataAndDelete(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "append-only needle"}); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdatePinned(true); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdateProjectID("assigned"); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "before" || !list[0].Pinned || list[0].ProjectID != "assigned" {
		t.Fatalf("updated summary = %#v", list)
	}
	results, err := store.SearchSessions(SessionSearchOptions{Query: "needle", ProjectID: "assigned"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Matches[0].Role != RoleAssistant {
		t.Fatalf("indexed append search = %#v", results)
	}
	if err := store.Delete(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.indexPath(session.ID)); !os.IsNotExist(err) {
		t.Fatalf("index after delete: %v", err)
	}
	list, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("deleted list = %#v", list)
	}
}

func TestSessionStoreIndexCrossInstanceRefreshesAfterAppend(t *testing.T) {
	root := t.TempDir()
	first, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := first.Create(SessionMetadata{Title: "cross-process"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(session.ID, Event{Type: EventMessage, Data: mustJSON(MessageEvent{Message: Message{Role: RoleUser, Content: "cross instance needle"}})}); err != nil {
		t.Fatal(err)
	}
	results, err := second.SearchSessions(SessionSearchOptions{Query: "cross instance"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].EventCount != 3 {
		t.Fatalf("cross-instance indexed result = %#v", results)
	}
}

func BenchmarkSessionStoreListUsesDurableIndex(b *testing.B) {
	root := b.TempDir()
	store, err := NewSessionStore(root)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		session, err := store.Create(SessionMetadata{Title: "benchmark"})
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 20; j++ {
			if err := session.AppendMessage(Message{Role: RoleUser, Content: strings.Repeat("indexed content ", 20)}); err != nil {
				b.Fatal(err)
			}
		}
	}
	if _, err := store.List(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.List(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}
