package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func newTodoFixture(t *testing.T) (*TodoStore, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "todos.json")
	store, err := NewTodoStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	return store, &now
}

func TestTodoLifecycleAndPartialPatch(t *testing.T) {
	store, now := newTodoFixture(t)
	created, err := store.Create(context.Background(), TodoCreateRequest{Title: "Buy tea", Description: "earl grey", Priority: TodoPriorityHigh, Labels: []string{"home", "errand"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.Status != TodoStatusOpen {
		t.Fatalf("created=%+v", created)
	}
	newTitle := "Buy better tea"
	updated, err := store.Update(context.Background(), TodoUpdateRequest{ID: created.ID, ExpectedVersion: 1, Title: &newTitle})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != created.Description || updated.Priority != created.Priority || updated.Version != 2 {
		t.Fatalf("partial patch lost omitted fields: %+v", updated)
	}
	updated, err = store.Update(context.Background(), TodoUpdateRequest{ID: created.ID, ExpectedVersion: 2, ClearDueAt: true, Labels: &[]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DueAt != nil || len(updated.Labels) != 0 {
		t.Fatalf("explicit clears failed: %+v", updated)
	}
	*now = now.Add(time.Minute)
	completed, err := store.Complete(context.Background(), created.ID, updated.Version)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != TodoStatusCompleted || completed.CompletedAt == nil || completed.Version != 4 {
		t.Fatalf("completed=%+v", completed)
	}
	noOp, err := store.Update(context.Background(), TodoUpdateRequest{ID: created.ID, ExpectedVersion: completed.Version, Title: &newTitle})
	if err != nil || noOp.Version != completed.Version {
		t.Fatalf("no-op update=%+v err=%v", noOp, err)
	}
	newDescription := "tea acquired"
	postComplete, err := store.Update(context.Background(), TodoUpdateRequest{ID: created.ID, ExpectedVersion: completed.Version, Description: &newDescription})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.Archive(context.Background(), created.ID, postComplete.Version)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != TodoStatusArchived || archived.ArchivedAt == nil {
		t.Fatalf("archived=%+v", archived)
	}
	rows, _, err := store.List(context.Background(), TodoListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("default list returned archived rows: %+v", rows)
	}
	rows, _, err = store.List(context.Background(), TodoListFilter{Status: TodoStatusArchived})
	if err != nil || len(rows) != 1 {
		t.Fatalf("archived list rows=%d err=%v", len(rows), err)
	}
}

func TestTodoVersionConflictAndIdempotentTerminalTransitions(t *testing.T) {
	store, _ := newTodoFixture(t)
	created, err := store.Create(context.Background(), TodoCreateRequest{Title: "Conflict"})
	if err != nil {
		t.Fatal(err)
	}
	title := "Changed"
	if _, err := store.Update(context.Background(), TodoUpdateRequest{ID: created.ID, ExpectedVersion: 99, Title: &title}); !errors.Is(err, ErrTodoVersionConflict) {
		t.Fatalf("err=%v", err)
	}
	completed, err := store.Complete(context.Background(), created.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.Complete(context.Background(), created.ID, completed.Version)
	if err != nil || repeated.Version != completed.Version {
		t.Fatalf("idempotent complete=%+v err=%v", repeated, err)
	}
	if _, err := store.Archive(context.Background(), created.ID, 1); !errors.Is(err, ErrTodoVersionConflict) {
		t.Fatalf("stale archive err=%v", err)
	}
	open, err := store.Create(context.Background(), TodoCreateRequest{Title: "archive open"})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := store.Archive(context.Background(), open.ID, open.Version)
	if err != nil || archived.Status != TodoStatusArchived || archived.CompletedAt != nil {
		t.Fatalf("archived open=%+v err=%v", archived, err)
	}
	repeatedArchive, err := store.Archive(context.Background(), open.ID, archived.Version)
	if err != nil || repeatedArchive.Version != archived.Version {
		t.Fatalf("idempotent archive=%+v err=%v", repeatedArchive, err)
	}
	if _, err := store.Complete(context.Background(), open.ID, archived.Version); err == nil {
		t.Fatal("archived todo was completed")
	}
}

func TestTodoPersistenceChecksumAndCorruption(t *testing.T) {
	store, _ := newTodoFixture(t)
	created, err := store.Create(context.Background(), TodoCreateRequest{Title: "Persist"})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewTodoStore(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Get(context.Background(), created.ID)
	if err != nil || got.Title != created.Title {
		t.Fatalf("reload=%+v err=%v", got, err)
	}
	if err := os.WriteFile(store.Path(), []byte(`{"version":1,"todos":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reloaded.List(context.Background(), TodoListFilter{}); err == nil {
		t.Fatal("truncated snapshot was accepted")
	}
}

func TestTodoConcurrentWritersUseCrossProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	a, err := NewTodoStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTodoStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*TodoStore{a, b} {
		wg.Add(1)
		go func(s *TodoStore) {
			defer wg.Done()
			_, e := s.Create(context.Background(), TodoCreateRequest{Title: "concurrent"})
			errs <- e
		}(store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := a.List(context.Background(), TodoListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
}

func TestTodoValidationRejectsSecretsDuplicateLabelsAndUnsafePaths(t *testing.T) {
	store, _ := newTodoFixture(t)
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "api_key=secret"}); err == nil {
		t.Fatal("secret-like title accepted")
	}
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "labels", Labels: []string{"x", "x"}}); err == nil {
		t.Fatal("duplicate labels accepted")
	}
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "path label", Labels: []string{"../etc"}}); err == nil {
		t.Fatal("path-like label accepted")
	}
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "bad\ncontrol"}); err == nil {
		t.Fatal("title control character accepted")
	}
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "description", Description: "bad\x01control"}); err == nil {
		t.Fatal("description control character accepted")
	}
	if _, err := NewTodoStore(filepath.Join(t.TempDir(), "a", "..", "todos.json")); err != nil {
		t.Fatalf("normal path rejected: %v", err)
	}
}

func TestTodoToolsAndPlanGateBoundary(t *testing.T) {
	store, _ := newTodoFixture(t)
	tools := NewTodoTools(store)
	if len(tools) != 6 {
		t.Fatalf("tool count=%d", len(tools))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		seen[tool.Definition().Name] = true
		if tool.Definition().Mutability == ToolMutabilityMutating && !tool.Definition().PlanExempt {
			t.Fatalf("todo mutation is not explicitly plan-exempt: %#v", tool.Definition())
		}
	}
	for _, name := range []string{"todo_create", "todo_list", "todo_show", "todo_update", "todo_complete", "todo_archive"} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
	registry, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, planGate: &PlanGate{}}
	created, err := executor.Execute(context.Background(), "todo_create", json.RawMessage(`{"title":"before plan"}`))
	if err != nil {
		t.Fatalf("todo create unexpectedly gated: %v", err)
	}
	var row TodoRecord
	if err := json.Unmarshal([]byte(created.Output), &row); err != nil {
		t.Fatal(err)
	}
	if row.ID == "" {
		t.Fatal("tool returned no id")
	}
	if _, err := executor.Execute(context.Background(), "todo_update", json.RawMessage(`{"id":"`+row.ID+`","expected_version":1,"title":"changed"}`)); err != nil {
		t.Fatalf("todo update unexpectedly gated: %v", err)
	}
	other, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "ordinary_mutation", Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "{}", nil }})
	if err != nil {
		t.Fatal(err)
	}
	blocked := &Executor{Registry: other, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, planGate: &PlanGate{}}
	if _, err := blocked.Execute(context.Background(), "ordinary_mutation", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ordinary mutation bypassed coding plan gate")
	}
}

func TestTodoReadsDoNotRewriteSnapshotAndLockCancellationIsTyped(t *testing.T) {
	store, _ := newTodoFixture(t)
	created, err := store.Create(context.Background(), TodoCreateRequest{Title: "stable read"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.List(context.Background(), TodoListFilter{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("read-only todo operations rewrote the snapshot")
	}

	other, err := NewTodoStore(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.withLock(context.Background(), func(*todoSnapshot) (bool, error) {
			close(held)
			<-release
			return false, nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := other.Create(ctx, TodoCreateRequest{Title: "blocked"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation err=%v", err)
	}
	localCtx, localCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer localCancel()
	if _, err := store.Create(localCtx, TodoCreateRequest{Title: "locally blocked"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("local lock cancellation err=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTodoSnapshotIntegrityPermissionsAndSizeBound(t *testing.T) {
	store, _ := newTodoFixture(t)
	created, err := store.Create(context.Background(), TodoCreateRequest{Title: "history integrity"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	snap.History[0].TodoID = "todo-missing"
	writeTodoSnapshotForTest(t, store.Path(), snap)
	if _, err := store.Get(context.Background(), created.ID); err == nil || strings.Contains(err.Error(), store.Path()) {
		t.Fatalf("tampered history err=%v", err)
	}
	impossibleStore, _ := newTodoFixture(t)
	open, err := impossibleStore.Create(context.Background(), TodoCreateRequest{Title: "impossible lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := impossibleStore.Archive(context.Background(), open.ID, open.Version)
	if err != nil {
		t.Fatal(err)
	}
	impossible, err := impossibleStore.load()
	if err != nil {
		t.Fatal(err)
	}
	postArchive := archived.UpdatedAt.Add(time.Second)
	impossible.Todos[0].Version++
	impossible.Todos[0].UpdatedAt = postArchive
	impossible.History = append(impossible.History, TodoHistoryEntry{TodoID: open.ID, Version: impossible.Todos[0].Version, Action: "update", At: postArchive})
	writeTodoSnapshotForTest(t, impossibleStore.Path(), impossible)
	if _, err := impossibleStore.Get(context.Background(), open.ID); err == nil {
		t.Fatal("post-archive history was accepted")
	}

	permissionStore, _ := newTodoFixture(t)
	if _, err := permissionStore.Create(context.Background(), TodoCreateRequest{Title: "permissions"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(permissionStore.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := permissionStore.Get(context.Background(), "todo-any"); err == nil || strings.Contains(err.Error(), permissionStore.Path()) {
		t.Fatalf("unsafe permission err=%v", err)
	}
	broadStore, _ := newTodoFixture(t)
	if err := os.Chmod(filepath.Dir(broadStore.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := broadStore.List(context.Background(), TodoListFilter{}); err == nil || !strings.Contains(err.Error(), "directory is unsafe") {
		t.Fatalf("unsafe directory err=%v", err)
	}

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewTodoStore(filepath.Join(linkDir, "todos.json")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	lockStore, _ := newTodoFixture(t)
	lockTarget := filepath.Join(t.TempDir(), "lock-target")
	if err := os.WriteFile(lockTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lockTarget, lockStore.Path()+".lock"); err != nil {
		t.Skipf("lock symlink unavailable: %v", err)
	}
	if _, err := lockStore.Create(context.Background(), TodoCreateRequest{Title: "unsafe lock"}); err == nil {
		t.Fatal("symlink lock accepted")
	}

	sizeStore, _ := newTodoFixture(t)
	large := todoSnapshotFixture(2000, TodoMaxDescriptionBytes)
	if err := sizeStore.save(large); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized snapshot err=%v", err)
	}
}

func TestTodoListFiltersPaginationAndBoundedJSON(t *testing.T) {
	store, now := newTodoFixture(t)
	dueSoon := now.Add(2 * time.Hour)
	dueLater := now.Add(4 * time.Hour)
	first, err := store.Create(context.Background(), TodoCreateRequest{Title: "first", Priority: TodoPriorityHigh, DueAt: &dueSoon, Labels: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := store.Create(context.Background(), TodoCreateRequest{Title: "second", Priority: TodoPriorityLow, DueAt: &dueLater, Labels: []string{"home"}}); err != nil {
		t.Fatal(err)
	}
	rows, truncated, err := store.List(context.Background(), TodoListFilter{Priority: TodoPriorityHigh, Label: "work", DueBefore: &dueLater, Limit: 1})
	if err != nil || truncated || len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("filtered rows=%#v truncated=%t err=%v", rows, truncated, err)
	}
	if _, _, err := store.List(context.Background(), TodoListFilter{DueAfter: &dueLater, DueBefore: &dueSoon}); err == nil {
		t.Fatal("invalid due range accepted")
	}

	boundedStore, _ := newTodoFixture(t)
	if err := boundedStore.save(todoSnapshotFixture(100, TodoMaxDescriptionBytes)); err != nil {
		t.Fatal(err)
	}
	output, err := (TodoListTool{Store: boundedStore}).Execute(context.Background(), json.RawMessage(`{"limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(output) > TodoMaxToolOutputBytes {
		t.Fatalf("todo list output=%d", len(output))
	}
	var result struct {
		Todos      []TodoRecord `json:"todos"`
		Truncated  bool         `json:"truncated"`
		Offset     int          `json:"offset"`
		Limit      int          `json:"limit"`
		NextOffset int          `json:"next_offset"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Todos) == 0 || len(result.Todos) >= 100 || result.NextOffset != len(result.Todos) || result.Limit != 100 {
		t.Fatalf("bounded list=%#v", result)
	}
}

func TestTodoToolExplicitClearsGenericErrorsAndPromptedApproval(t *testing.T) {
	store, now := newTodoFixture(t)
	due := now.Add(time.Hour).Format(time.RFC3339)
	createdJSON, err := (TodoCreateTool{Store: store}).Execute(context.Background(), json.RawMessage(`{"title":"tool todo","description":"clear me","due_at":"`+due+`","labels":["work"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var created TodoRecord
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	updatedJSON, err := (TodoUpdateTool{Store: store}).Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q,"expected_version":1,"description":"","due_at":null,"labels":null}`, created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	var updated TodoRecord
	if err := json.Unmarshal([]byte(updatedJSON), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Description != "" || updated.DueAt != nil || len(updated.Labels) != 0 || updated.Version != 2 {
		t.Fatalf("updated=%#v", updated)
	}
	if _, err := (TodoUpdateTool{Store: store}).Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q,"expected_version":2}`, created.ID))); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("empty update err=%v", err)
	}
	if _, err := (TodoUpdateTool{Store: store}).Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q,"expected_version":2,"title":null}`, created.ID))); err == nil || err.Error() != "invalid todo_update arguments" {
		t.Fatalf("null title err=%v", err)
	}
	if _, err := (TodoCreateTool{Store: store}).Execute(context.Background(), json.RawMessage(`{"password=do-not-echo":true}`)); err == nil || err.Error() != "invalid todo_create arguments" {
		t.Fatalf("argument error=%v", err)
	}
	if _, err := (TodoShowTool{Store: store}).Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q,"expected_version":2}`, created.ID))); err == nil || err.Error() != "invalid todo_show arguments" {
		t.Fatalf("show argument error=%v", err)
	}

	registry, err := NewRegistry(NewTodoTools(store)...)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyPrompted}, Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		approvals++
		return ApprovalApproved, nil
	}), planGate: &PlanGate{}}
	if _, err := executor.Execute(context.Background(), "todo_create", json.RawMessage(`{"title":"approved"}`)); err != nil || approvals != 1 {
		t.Fatalf("prompted todo err=%v approvals=%d", err, approvals)
	}
	shadow, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "todo_create", Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "{}", nil }})
	if err != nil {
		t.Fatal(err)
	}
	blocked := &Executor{Registry: shadow, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, planGate: &PlanGate{}}
	if _, err := blocked.Execute(context.Background(), "todo_create", json.RawMessage(`{}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("unmarked todo-name tool bypassed plan: %v", err)
	}
}

func TestTodoCompositionChildBoundaryAndJobSeparation(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	sessions, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	todos, _ := newTodoFixture(t)
	root := t.TempDir()
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: sessions, JobStore: jobs, TodoStore: todos, ScopeRoot: root, MemoryRoot: filepath.Join(t.TempDir(), "memory")})
	if err != nil {
		t.Fatal(err)
	}
	if app.TodoStore != todos {
		t.Fatal("todo store was not retained by parent composition")
	}
	for _, name := range []string{"todo_create", "todo_list", "todo_update", "todo_complete"} {
		if _, ok := app.Executor.Registry.Lookup(name); !ok || !containsPlanTool(app.Capabilities.ToolNames, name) {
			t.Fatalf("parent composition missing %s", name)
		}
	}
	for _, name := range []string{"todo_show", "todo_archive"} {
		if _, ok := app.Executor.Registry.Lookup(name); ok || containsPlanTool(app.Capabilities.ToolNames, name) {
			t.Fatalf("pruned %s is still model-facing", name)
		}
	}
	beforeJobs, err := jobs.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (TodoCreateTool{Store: todos}).Execute(context.Background(), json.RawMessage(`{"title":"not a job"}`)); err != nil {
		t.Fatal(err)
	}
	afterJobs, err := jobs.List()
	if err != nil || len(beforeJobs) != len(afterJobs) {
		t.Fatalf("todo changed jobs before=%d after=%d err=%v", len(beforeJobs), len(afterJobs), err)
	}
	child, err := copyChildExecutor(app.Executor, ChildTask{TaskID: "todo-child", ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{"todo_list"}, Scope: app.Executor.Scope, Budget: ChildBudget{}})
	if err == nil || child != nil || !strings.Contains(err.Error(), "parent-owned") {
		t.Fatalf("child todo projection child=%#v err=%v", child, err)
	}

	lockedTodoPath := filepath.Join(t.TempDir(), "locked", "todos.json")
	locked, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: sessions, JobStore: jobs, TodoStorePath: lockedTodoPath, ScopeRoot: root, Policy: AssistantPolicyLocked, MemoryRoot: filepath.Join(t.TempDir(), "locked-memory")})
	if err != nil {
		t.Fatal(err)
	}
	if locked.TodoStore != nil || locked.Executor != nil {
		t.Fatalf("locked composition todo=%#v executor=%#v", locked.TodoStore, locked.Executor)
	}
	if _, err := os.Stat(lockedTodoPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locked composition created todo store: %v", err)
	}
}

func TestTodoStorePathLoadsFromConfig(t *testing.T) {
	wanted := filepath.Join(t.TempDir(), "todos.json")
	configPath := filepath.Join(t.TempDir(), "config.json")
	body := fmt.Sprintf(`{"assistant":{"todo_store_path":%q}}`, wanted)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.TodoStorePath != wanted {
		t.Fatalf("todo store path=%q want=%q", config.Assistant.TodoStorePath, wanted)
	}
}

func todoSnapshotFixture(count, descriptionBytes int) todoSnapshot {
	now := time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC)
	snap := todoSnapshot{Version: 1, Todos: make([]TodoRecord, 0, count), History: make([]TodoHistoryEntry, 0, count)}
	description := strings.Repeat("x", descriptionBytes)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("todo-fixture-%05d", i)
		at := now.Add(time.Duration(i) * time.Second)
		snap.Todos = append(snap.Todos, TodoRecord{ID: id, Title: fmt.Sprintf("todo %d", i), Description: description, Priority: TodoPriorityNormal, Status: TodoStatusOpen, Version: 1, CreatedAt: at, UpdatedAt: at})
		snap.History = append(snap.History, TodoHistoryEntry{TodoID: id, Version: 1, Action: "create", At: at})
	}
	return snap
}

func writeTodoSnapshotForTest(t *testing.T, path string, snap todoSnapshot) {
	t.Helper()
	payload := todoSnapshotPayload{Version: 1, Todos: snap.Todos, History: snap.History}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	snap.Version = 1
	snap.Checksum = hex.EncodeToString(sum[:])
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
