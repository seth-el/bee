package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryEditToolPreservesOmittedFieldsAndRejectsStaleVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "test-parent"}
	record, err := service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: "memory-1", Namespace: MemoryNamespaceDurableMemory, Key: "key", Content: "before", Summary: "keep", Sensitivity: MemorySensitivityPublic}, AuditID: "create-1", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	tool := MemoryEditTool{Service: service, Actor: actor}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"record_id":"memory-1","expected_version":1,"reason":"correct","fields":{"content":"after"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"version":2`) {
		t.Fatalf("out=%s", out)
	}
	shown, err := service.Show(context.Background(), ShowMemoryRequest{Actor: actor, RecordID: record.ID, Bounds: MemoryDisplayBounds{MaxContentBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if shown.Record.Content != "after" || shown.Record.Summary != "keep" {
		t.Fatalf("record=%#v", shown.Record)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"record_id":"memory-1","expected_version":1,"reason":"stale","fields":{"content":"bad"}}`))
	if !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestMemorySupersedeToolIsAtomicLocalBatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "test-parent"}
	_, err = service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: "old", Namespace: MemoryNamespaceDurableMemory, Key: "old-key", Content: "old", Sensitivity: MemorySensitivityPublic}, AuditID: "create-old", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	tool := MemorySupersedeTool{Service: service, Actor: actor}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"record_id":"old","expected_version":1,"reason":"replace","replacement":{"key":"old-key","content":"new","sensitivity":"public"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var result memoryLifecycleToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.ID == "old" {
		t.Fatalf("replacement id=%q", result.ID)
	}
	old, err := service.Show(context.Background(), ShowMemoryRequest{Actor: actor, RecordID: "old", Bounds: MemoryDisplayBounds{MaxContentBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if old.Record.Status != MemoryStatusSuperseded || old.Record.SupersededBy != result.ID {
		t.Fatalf("old=%#v", old.Record)
	}
}

func TestMemorySupersedeBatchFailureLeavesLogAndStateUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "parent"}
	_, err = service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: "old", Namespace: MemoryNamespaceDurableMemory, Key: "old", Content: "old", Sensitivity: MemorySensitivityPublic}, AuditID: "create-old", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	store := service.Memory.(*LocalMemoryStore)
	before, err := os.ReadFile(filepath.Join(root, memoryEventsFile))
	if err != nil {
		t.Fatal(err)
	}
	store.batchFailureInjector = func(stage string) error {
		if stage == "batch_write" {
			return errors.New("injected")
		}
		return nil
	}
	_, err = service.Supersede(context.Background(), SupersedeMemoryRequest{Actor: actor, RecordID: "old", ExpectedVersion: 1, Replacement: MemoryRecord{ID: "new", Key: "new", Content: "new", Sensitivity: MemorySensitivityPublic}, AuditID: "supersede", Reason: "test"})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	after, err := os.ReadFile(filepath.Join(root, memoryEventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed batch changed event log")
	}
	if _, err := service.Show(context.Background(), ShowMemoryRequest{Actor: actor, RecordID: "new", Bounds: MemoryDisplayBounds{MaxContentBytes: 1024}}); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("replacement visible after failed batch: %v", err)
	}
	store.batchFailureInjector = nil
	if _, err := service.Supersede(context.Background(), SupersedeMemoryRequest{Actor: actor, RecordID: "old", ExpectedVersion: 1, Replacement: MemoryRecord{ID: "new", Key: "new", Content: "new", Sensitivity: MemorySensitivityPublic}, AuditID: "supersede-2", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	store2, err := NewLocalMemoryStore(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store2.GetRecord(context.Background(), actor, "new", false); err != nil {
		t.Fatalf("replay after commit: %v", err)
	}
}

func TestMemorySupersedeRejectsLifecycleFieldsAndEditCanClearExpiry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	_ = os.MkdirAll(root, 0o700)
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "parent"}
	_, err = service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: "old", Namespace: MemoryNamespaceDurableMemory, Key: "old", Content: "old", Sensitivity: MemorySensitivityPublic, ExpiresAt: time.Now().UTC().Add(time.Hour)}, AuditID: "c", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	tool := MemorySupersedeTool{Service: service, Actor: actor}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"record_id":"old","expected_version":1,"reason":"r","replacement":{"key":"new","content":"new","created_by":{"kind":"parent","id":"evil"}}}`)); err == nil {
		t.Fatal("expected lifecycle field rejection")
	}
	edit := MemoryEditTool{Service: service, Actor: actor}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"record_id":"old","expected_version":1,"reason":"r","fields":{"expires_at":""}}`)); err != nil {
		t.Fatal(err)
	}
	shown, err := service.Show(context.Background(), ShowMemoryRequest{Actor: actor, RecordID: "old", Bounds: MemoryDisplayBounds{MaxContentBytes: 1024}})
	if err != nil || !shown.Record.ExpiresAt.IsZero() {
		t.Fatalf("shown=%#v err=%v", shown, err)
	}
}

func TestMutationJournalWriteUndoAndHashGuard(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewMutationJournal(filepath.Join(root, "state"), scope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope, "new.txt")
	tool := WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"new.txt","content":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := journal.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%#v err=%v", entries, err)
	}
	if _, err := journal.Undo(context.Background(), entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path err=%v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"new.txt","content":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	entries, _ = journal.Entries()
	last := entries[len(entries)-1]
	if err := os.WriteFile(path, []byte("human"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Undo(context.Background(), last.ID); err == nil {
		t.Fatal("expected hash guard")
	}
}

func TestMutationJournalCoversUnifiedAppendAndEdit(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope, "notes.md")
	if err := os.WriteFile(path, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := NewMutationJournal(filepath.Join(root, "state"), scope)
	if err != nil {
		t.Fatal(err)
	}
	tool := WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","operation":"append","content":" tail"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","operation":"edit","old_text":"tail","new_text":"end"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := journal.Entries()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%#v err=%v", entries, err)
	}
	if entries[0].Tool != "write_file" || !strings.Contains(entries[0].Result, "appended") || entries[1].Tool != "write_file" || !strings.Contains(entries[1].Result, "edited") {
		t.Fatalf("unified journal entries=%#v", entries)
	}
	if _, err := journal.Undo(context.Background(), entries[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Undo(context.Background(), entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "base" {
		t.Fatalf("restored data=%q err=%v", data, err)
	}
}

func TestAtomicCreateFileNeverClobbersExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicCreateFile(path, []byte("replacement"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("atomic create error=%v, want already exists", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("atomic create clobbered file: data=%q err=%v", data, err)
	}
}

func TestMutationJournalForRootsPreservesPrimaryAndUndoesSecondary(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	workspace := filepath.Join(home, ".bee", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	journalRoot := filepath.Join(base, "journals")
	primary, err := NewMutationJournal(journalRoot, workspace)
	if err != nil {
		t.Fatal(err)
	}
	primaryPath := filepath.Join(workspace, "memory", "primary.md")
	primaryTool := WriteFileTool{Scope: ExecutionScope{Root: workspace}, Journal: primary}
	if _, err := primaryTool.Execute(context.Background(), mustJSON(map[string]any{"path": primaryPath, "content": "primary"})); err != nil {
		t.Fatal(err)
	}

	journal, err := NewMutationJournalForRoots(journalRoot, []string{workspace, home})
	if err != nil {
		t.Fatal(err)
	}
	secondaryPath := filepath.Join(home, "approved.txt")
	scope := ExecutionScope{Root: workspace, Roots: []string{home}, WriteRoot: workspace}
	secondaryTool := WriteFileTool{Scope: scope, Journal: journal}
	approved := withExplicitToolApproval(context.Background())
	if _, err := secondaryTool.Execute(approved, mustJSON(map[string]any{"path": secondaryPath, "content": "secondary"})); err != nil {
		t.Fatal(err)
	}
	entries, err := journal.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%#v", entries)
	}
	if entries[0].ScopeID != primary.scopeID {
		t.Fatalf("primary scope id=%q want %q", entries[0].ScopeID, primary.scopeID)
	}
	var secondary MutationJournalEntry
	for _, entry := range entries {
		if entry.ScopeID != primary.scopeID {
			secondary = entry
		}
	}
	if secondary.ID == "" {
		t.Fatalf("secondary entry missing: %#v", entries)
	}
	registry, err := NewRegistry(MutationUndoTool{Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	undoApprovals := 0
	executor := &Executor{
		Registry: registry,
		Scope:    scope,
		Policy:   GeneralAssistantPolicy{Mode: AssistantPolicyTrusted},
		Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
			undoApprovals++
			return ApprovalApproved, nil
		}),
	}
	if _, err := executor.Execute(context.Background(), "mutation_undo", mustJSON(map[string]any{"id": secondary.ID})); err != nil {
		t.Fatal(err)
	}
	if undoApprovals != 1 {
		t.Fatalf("undo approvals=%d", undoApprovals)
	}
	if _, err := os.Stat(secondaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secondary path still exists: %v", err)
	}
	if data, err := os.ReadFile(primaryPath); err != nil || string(data) != "primary" {
		t.Fatalf("primary data=%q err=%v", data, err)
	}
}

func TestGeneralRegistryRegistersLifecycleGitAndMutationTools(t *testing.T) {
	root := t.TempDir()
	service, err := NewLocalMemoryService(filepath.Join(root, "memory"), DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewMutationJournal(filepath.Join(root, "state"), root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newGeneralAssistantRegistry(ExecutionScope{Root: root}, service, MemoryActor{Kind: MemoryActorParent, ID: "parent"}, DefaultMemoryPolicy(), MemoryRetrievalOptions{}, nil, nil, nil, journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"memory_edit", "memory_forget", "memory_supersede", "git_status", "git_diff", "mutation_list", "mutation_show", "mutation_undo"} {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestMutationJournalScopesAndCompositionUseInjectedSessionRoot(t *testing.T) {
	root := t.TempDir()
	scopeA := filepath.Join(root, "a")
	scopeB := filepath.Join(root, "b")
	_ = os.MkdirAll(scopeA, 0700)
	_ = os.MkdirAll(scopeB, 0700)
	jA, err := NewMutationJournal(filepath.Join(root, "journals"), scopeA)
	if err != nil {
		t.Fatal(err)
	}
	jB, err := NewMutationJournal(filepath.Join(root, "journals"), scopeB)
	if err != nil {
		t.Fatal(err)
	}
	if jA.scopeID == jB.scopeID || jA.Root() == jB.Root() {
		t.Fatalf("scope isolation failed: ids=%s/%s roots=%s/%s", jA.scopeID, jB.scopeID, jA.Root(), jB.Root())
	}
	p := filepath.Join(scopeA, "same.txt")
	tool := WriteFileTool{Scope: ExecutionScope{Root: scopeA}, Journal: jA}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"same.txt","content":"a"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := jB.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("scope B saw scope A entry")
	}
	if _, err := jB.Undo(context.Background(), "mutation-nope"); err == nil {
		t.Fatal("scope B undid scope A entry")
	}
	_ = p
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store:         sessions,
		ScopeRoots:    []string{scopeA, scopeB},
		WriteRoot:     scopeA,
		MemoryRoot:    filepath.Join(root, "memory"),
		Policy:        AssistantPolicyTrusted,
		JobStorePath:  filepath.Join(root, "jobs.json"),
		TodoStorePath: filepath.Join(root, "todos.json"),
		Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
			approvals++
			return ApprovalApproved, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	value, ok := app.Executor.Registry.Lookup("mutation_list")
	if !ok {
		t.Fatal("composition missing mutation journal")
	}
	journalTool := value.(MutationListTool)
	if !strings.HasPrefix(journalTool.Journal.Root(), sessions.Root()) {
		t.Fatalf("journal root=%s sessions=%s", journalTool.Journal.Root(), sessions.Root())
	}
	if strings.Contains(journalTool.Journal.Root(), DefaultBeeStateRootOrEmptyForTest()) {
		t.Fatal("composition unexpectedly used default state root")
	}
	secondaryPath := filepath.Join(scopeB, "approved.txt")
	if _, err := app.Executor.Execute(context.Background(), "write_file", mustJSON(map[string]any{"path": secondaryPath, "content": "approved"})); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("approvals=%d", approvals)
	}
	if data, err := os.ReadFile(secondaryPath); err != nil || string(data) != "approved" {
		t.Fatalf("secondary data=%q err=%v", data, err)
	}
	entries, err = journalTool.Journal.Entries()
	if err != nil || len(entries) != 1 || entries[0].Path != "approved.txt" {
		t.Fatalf("entries=%#v err=%v", entries, err)
	}
}

func DefaultBeeStateRootOrEmptyForTest() string { root, _ := DefaultBeeStateRoot(); return root }

func TestGitDiffUsesContextAndIncludesStagedChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "--quiet", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runGitSimple(context.Background(), root, "add", "file.txt"); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: root}
	out, err := (GitDiffTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"path":"file.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "two") || !strings.Contains(out, "one") {
		t.Fatalf("diff=%s", out)
	}
	if _, err := (GitDiffTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"ref":"--output=/tmp/x"}`)); err == nil {
		t.Fatal("expected unsafe ref rejection")
	}
}

func TestGitStatusJSONIsBoundedAndValid(t *testing.T) {
	out := gitStatusResult{Repository: "/repo"}
	for i := 0; i < 1000; i++ {
		out.Entries = append(out.Entries, gitStatusEntry{Index: "?", Worktree: "?", Path: strings.Repeat("x", 100)})
	}
	data, err := boundedGitStatusJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxGitReviewBytes {
		t.Fatalf("len=%d", len(data))
	}
	var decoded gitStatusResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Truncated {
		t.Fatal("expected truncation")
	}
}

func TestMemoryForgetTombstoneAndNonParentRejection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	_ = os.MkdirAll(root, 0o700)
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	parent := MemoryActor{Kind: MemoryActorParent, ID: "p"}
	child := MemoryActor{Kind: MemoryActorChild, ID: "c"}
	_, err = service.Remember(context.Background(), RememberRequest{Actor: parent, Record: MemoryRecord{ID: "forget", Namespace: MemoryNamespaceDurableMemory, Key: "forget", Content: "x", Sensitivity: MemorySensitivityPublic}, AuditID: "create", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	childCases := []struct {
		tool Tool
		args json.RawMessage
	}{
		{MemoryEditTool{Service: service, Actor: child}, json.RawMessage(`{"record_id":"forget","expected_version":1,"reason":"r","fields":{"content":"x"}}`)},
		{MemoryForgetTool{Service: service, Actor: child}, json.RawMessage(`{"record_id":"forget","expected_version":1,"reason":"r"}`)},
		{MemorySupersedeTool{Service: service, Actor: child}, json.RawMessage(`{"record_id":"forget","expected_version":1,"reason":"r","replacement":{"key":"forget","content":"x"}}`)},
	}
	for _, test := range childCases {
		if _, err := test.tool.Execute(context.Background(), test.args); !errors.Is(err, ErrMemoryUnauthorized) {
			t.Fatalf("child mutation error=%v", err)
		}
	}
	forget := MemoryForgetTool{Service: service, Actor: parent}
	if _, err := forget.Execute(context.Background(), json.RawMessage(`{"record_id":"forget","expected_version":1,"reason":"r"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Show(context.Background(), ShowMemoryRequest{Actor: parent, RecordID: "forget", Bounds: MemoryDisplayBounds{MaxContentBytes: 1024}}); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("tombstone visible: %v", err)
	}
	if _, err := forget.Execute(context.Background(), json.RawMessage(`{"record_id":"forget","expected_version":1,"reason":"stale"}`)); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale=%v", err)
	}
}

func TestMemorySupersedeDuplicateReplacementConflictAndJournalModifiedRestore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	_ = os.MkdirAll(root, 0700)
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "p"}
	for _, id := range []string{"old", "existing"} {
		_, err = service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: id, Namespace: MemoryNamespaceDurableMemory, Key: id, Content: id, Sensitivity: MemorySensitivityPublic}, AuditID: "create-" + id, Reason: "r"})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = service.Supersede(context.Background(), SupersedeMemoryRequest{Actor: actor, RecordID: "old", ExpectedVersion: 1, Replacement: MemoryRecord{ID: "existing", Key: "replacement", Content: "x", Sensitivity: MemorySensitivityPublic}, AuditID: "sup", Reason: "r"})
	if !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	base := t.TempDir()
	scope := filepath.Join(base, "scope")
	_ = os.MkdirAll(scope, 0700)
	journal, err := NewMutationJournal(filepath.Join(base, "journals"), scope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope, "mode.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}).Execute(context.Background(), json.RawMessage(`{"path":"mode.txt","operation":"replace","content":"after"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := journal.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Undo(context.Background(), entries[0].ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "before" || info.Mode().Perm() != 0600 {
		t.Fatalf("restore data=%q mode=%o", data, info.Mode().Perm())
	}
}

func TestMutationListExactlyFiftyIsNotTruncated(t *testing.T) {
	base := t.TempDir()
	scope := filepath.Join(base, "scope")
	_ = os.MkdirAll(scope, 0700)
	journal, err := NewMutationJournal(filepath.Join(base, "journals"), scope)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := journal.Record(context.Background(), "test", filepath.Join(scope, fmt.Sprintf("f-%d", i)), MutationFileState{}, nil, MutationFileState{}, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	out, err := (MutationListTool{Journal: journal}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Entries   []MutationJournalEntry `json:"entries"`
		Truncated bool                   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Entries) != 50 || decoded.Truncated {
		t.Fatalf("entries=%d truncated=%v", len(decoded.Entries), decoded.Truncated)
	}
}

func TestJournalFailureRevertsMutationAndLargeWriteFailsBeforeChange(t *testing.T) {
	base := t.TempDir()
	scope := filepath.Join(base, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := NewMutationJournal(filepath.Join(base, "journals"), scope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope, "existing.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal.Root(), 0o500); err != nil {
		t.Fatal(err)
	}
	_, writeErr := (WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}).Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","operation":"replace","content":"after"}`))
	if err := os.Chmod(journal.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	if writeErr == nil {
		t.Fatal("expected journal commit failure")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "before" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	largePath := filepath.Join(scope, "large.txt")
	largeArgs, err := json.Marshal(map[string]any{"path": "large.txt", "content": strings.Repeat("x", mutationMaxFileBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}).Execute(context.Background(), largeArgs); err == nil {
		t.Fatal("expected journaled file size rejection")
	}
	if _, err := os.Stat(largePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("large file unexpectedly created: %v", err)
	}
	appendArgs, err := json.Marshal(map[string]any{"path": "existing.txt", "operation": "append", "content": strings.Repeat("x", mutationMaxFileBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (WriteFileTool{Scope: ExecutionScope{Root: scope}, Journal: journal}).Execute(context.Background(), appendArgs); err == nil {
		t.Fatal("expected oversized append result rejection")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "before" {
		t.Fatalf("oversized append changed data=%q err=%v", data, err)
	}
}

func TestMutationJournalRejectsSymlinkAncestorAndSerializesInstances(t *testing.T) {
	base := t.TempDir()
	scope := filepath.Join(base, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "linked")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMutationJournal(filepath.Join(linkRoot, "journal"), scope); err == nil {
		t.Fatal("expected symlinked journal ancestor rejection")
	}
	journalBase := filepath.Join(base, "journals")
	j1, err := NewMutationJournal(journalBase, scope)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := NewMutationJournal(journalBase, scope)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			journal := j1
			if i%2 == 1 {
				journal = j2
			}
			_, err := journal.Record(context.Background(), "test", filepath.Join(scope, fmt.Sprintf("f-%d", i)), MutationFileState{}, nil, MutationFileState{}, "ok")
			errCh <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	entries, err := j1.Entries()
	if err != nil || len(entries) != 20 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	info, err := os.Stat(j1.Root())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("journal root mode=%v", info.Mode().Perm())
	}
}

func TestGitToolsRejectNonRepositoryAndScopeEscape(t *testing.T) {
	root := t.TempDir()
	scope := ExecutionScope{Root: root}
	if _, err := (GitStatusTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected non-repository rejection")
	}
	outside := t.TempDir()
	args, err := json.Marshal(gitReviewArgs{RepoPath: outside})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (GitStatusTool{Scope: scope}).Execute(context.Background(), args); err == nil {
		t.Fatal("expected repository scope escape rejection")
	}
}
