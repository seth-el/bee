package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConversationHistorySearchLargeLateMatchAndRebuildsSidecar(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "large", Title: "Large"})
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("padding ", (5<<20)/len("padding ")) + "late-needle 😀"
	if err := session.AppendMessage(Message{Role: RoleUser, Content: content}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "late-needle"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Hits) != 1 || !strings.Contains(result.Hits[0].Snippet, "late-needle") {
		t.Fatalf("result=%+v", result)
	}
	indexPath := filepath.Join(store.Root(), "large.jsonl.search.idx")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "late-needle"})
	if err != nil || result.Total != 1 {
		t.Fatalf("corrupt sidecar rebuild result=%+v err=%v", result, err)
	}
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestEnd := strings.IndexByte(string(indexData), '\n')
	if manifestEnd < 0 {
		t.Fatal("search sidecar has no manifest line")
	}
	if err := os.WriteFile(indexPath, indexData[:manifestEnd+1], 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "late-needle"})
	if err != nil || result.Total != 1 {
		t.Fatalf("manifest-only sidecar rebuild result=%+v err=%v", result, err)
	}
	indexData, err = os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	modifiedIndex := strings.Replace(string(indexData), "late-needle", "late-needlf", 1)
	if modifiedIndex == string(indexData) {
		t.Fatal("failed to alter sidecar chunk fixture")
	}
	if err := os.WriteFile(indexPath, []byte(modifiedIndex), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "late-needle"})
	if err != nil || result.Total != 1 {
		t.Fatalf("checksum-mismatched sidecar rebuild result=%+v err=%v", result, err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "newly appended searchable phrase"}); err != nil {
		t.Fatal(err)
	}
	result, err = service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "newly appended searchable phrase"})
	if err != nil || result.Total != 1 {
		t.Fatalf("append rebuild result=%+v err=%v", result, err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "token=sidecar-secret redaction marker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "redaction marker"}); err != nil {
		t.Fatal(err)
	}
	indexData, err = os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(indexData), "sidecar-secret") {
		t.Fatal("conversation search sidecar retained raw secret value")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Search(cancelled, ConversationHistorySearchRequest{Actor: service.Actor, Query: "redaction marker"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search error=%v", err)
	}
	if err := store.Delete(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("search sidecar survived delete: %v", err)
	}
}

func TestConversationHistorySearchCancellationInterruptsLockWait(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "lock wait needle"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSessionFileLock(store.path(session.ID) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := service.Search(ctx, ConversationHistorySearchRequest{Actor: service.Actor, Query: "needle"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock-wait cancellation error=%v", err)
	}
}

func TestConversationHistorySearchConcurrentReaders(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "concurrent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "unicode boundary café needle"}); err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationHistorySearch(store, MemoryActor{Kind: MemoryActorParent, ID: "bee-assistant"})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, searchErr := service.Search(context.Background(), ConversationHistorySearchRequest{Actor: service.Actor, Query: "café needle"})
			if searchErr != nil || result.Total != 1 {
				t.Errorf("result=%+v err=%v", result, searchErr)
			}
		}()
	}
	group.Wait()
}
