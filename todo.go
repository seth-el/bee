package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	TodoStatusOpen          = "open"
	TodoStatusCompleted     = "completed"
	TodoStatusArchived      = "archived"
	TodoPriorityLow         = "low"
	TodoPriorityNormal      = "normal"
	TodoPriorityHigh        = "high"
	TodoMaxTitleBytes       = 256
	TodoMaxDescriptionBytes = 4096
	TodoMaxLabels           = 16
	TodoMaxLabelBytes       = 64
	TodoMaxRecords          = 10000
	TodoMaxListLimit        = 100
	TodoMaxOffset           = 10000
	TodoMaxHistory          = 100000
	TodoMaxSnapshotBytes    = 8 << 20
	TodoMaxToolOutputBytes  = 256 << 10
)

var (
	ErrTodoNotFound        = errors.New("todo not found")
	ErrTodoVersionConflict = errors.New("todo version conflict")
)

type TodoRecord struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Priority    string     `json:"priority"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	Labels      []string   `json:"labels,omitempty"`
	Status      string     `json:"status"`
	Version     int        `json:"version"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
}

type TodoHistoryEntry struct {
	TodoID  string    `json:"todo_id"`
	Version int       `json:"version"`
	Action  string    `json:"action"`
	At      time.Time `json:"at"`
}

type TodoCreateRequest struct {
	Title       string
	Description string
	Priority    string
	DueAt       *time.Time
	Labels      []string
}

type TodoUpdateRequest struct {
	ID              string
	ExpectedVersion int
	Title           *string
	Description     *string
	Priority        *string
	DueAt           *time.Time
	ClearDueAt      bool
	Labels          *[]string
}

type TodoListFilter struct {
	Status    string
	Priority  string
	Label     string
	DueBefore *time.Time
	DueAfter  *time.Time
	Limit     int
	Offset    int
}

type todoSnapshotPayload struct {
	Version int                `json:"version"`
	Todos   []TodoRecord       `json:"todos"`
	History []TodoHistoryEntry `json:"history,omitempty"`
}
type todoSnapshot struct {
	Version  int                `json:"version"`
	Todos    []TodoRecord       `json:"todos"`
	History  []TodoHistoryEntry `json:"history,omitempty"`
	Checksum string             `json:"checksum"`
}

type TodoStore struct {
	path  string
	mu    sync.Mutex
	clock func() time.Time
}

func NewTodoStore(path string) (*TodoStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, errors.New("todo store path is invalid")
	}
	abs := filepath.Clean(path)
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, errors.New("todo store path is unsafe")
	}
	if err := ensurePrivateDir(filepath.Dir(abs), 0o700); err != nil {
		return nil, errors.New("todo store directory is unavailable")
	}
	return &TodoStore{path: abs, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func NewDefaultTodoStore() (*TodoStore, error) {
	path, err := DefaultBeeTodoStorePath()
	if err != nil {
		return nil, err
	}
	return NewTodoStore(path)
}

func (s *TodoStore) SetClock(clock func() time.Time) {
	if s != nil && clock != nil {
		s.clock = clock
	}
}
func (s *TodoStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *TodoStore) withLock(ctx context.Context, fn func(*todoSnapshot) (bool, error)) error {
	if s == nil {
		return errors.New("todo store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.lockLocal(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("todo store is unsafe")
	}
	if info, err := os.Lstat(s.path + ".lock"); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("todo store is unsafe")
	}
	parentInfo, err := os.Lstat(filepath.Dir(s.path))
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("todo store directory is unsafe")
	}
	lock, err := acquireSessionFileLockContext(ctx, s.path+".lock")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("todo store is busy")
	}
	defer lock.Close()
	if err := rejectBeeSymlinkAncestors(s.path); err != nil {
		return errors.New("todo store is unsafe")
	}
	snap, err := s.load()
	if err != nil {
		return err
	}
	changed, err := fn(&snap)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.save(snap)
}

func (s *TodoStore) lockLocal(ctx context.Context) error {
	for {
		if s.mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *TodoStore) read(ctx context.Context) (todoSnapshot, error) {
	var out todoSnapshot
	err := s.withLock(ctx, func(snap *todoSnapshot) (bool, error) { out = cloneTodoSnapshot(*snap); return false, nil })
	return out, err
}

func cloneTodoSnapshot(in todoSnapshot) todoSnapshot {
	out := in
	out.Todos = make([]TodoRecord, len(in.Todos))
	for i, item := range in.Todos {
		out.Todos[i] = cloneTodo(item)
	}
	out.History = append([]TodoHistoryEntry(nil), in.History...)
	return out
}
func cloneTodo(in TodoRecord) TodoRecord {
	out := in
	out.Labels = append([]string(nil), in.Labels...)
	if in.DueAt != nil {
		v := *in.DueAt
		out.DueAt = &v
	}
	if in.CompletedAt != nil {
		v := *in.CompletedAt
		out.CompletedAt = &v
	}
	if in.ArchivedAt != nil {
		v := *in.ArchivedAt
		out.ArchivedAt = &v
	}
	return out
}

func (s *TodoStore) load() (todoSnapshot, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return todoSnapshot{Version: 1, Todos: []TodoRecord{}}, nil
	}
	if err != nil {
		return todoSnapshot{}, errors.New("todo store cannot be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return todoSnapshot{}, errors.New("todo store permissions are unsafe")
	}
	if info.Size() > TodoMaxSnapshotBytes {
		return todoSnapshot{}, errors.New("todo store is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, TodoMaxSnapshotBytes+1))
	if err != nil {
		return todoSnapshot{}, errors.New("todo store cannot be read")
	}
	if len(data) > TodoMaxSnapshotBytes {
		return todoSnapshot{}, errors.New("todo store is too large")
	}
	var snap todoSnapshot
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return todoSnapshot{}, errors.New("todo store is corrupt")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return todoSnapshot{}, errors.New("todo store is corrupt")
	}
	if snap.Version != 1 || snap.Checksum == "" {
		return todoSnapshot{}, errors.New("todo store is corrupt")
	}
	payload := todoSnapshotPayload{Version: snap.Version, Todos: snap.Todos, History: snap.History}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return todoSnapshot{}, errors.New("todo store is corrupt")
	}
	sum := sha256.Sum256(encoded)
	if !strings.EqualFold(snap.Checksum, hex.EncodeToString(sum[:])) {
		return todoSnapshot{}, errors.New("todo store checksum mismatch")
	}
	if err := validateTodoSnapshot(&snap); err != nil {
		return todoSnapshot{}, errors.New("todo store is corrupt")
	}
	return snap, nil
}

func (s *TodoStore) save(snap todoSnapshot) error {
	if err := validateTodoSnapshot(&snap); err != nil {
		return err
	}
	payload := todoSnapshotPayload{Version: 1, Todos: snap.Todos, History: snap.History}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("todo store commit failed")
	}
	sum := sha256.Sum256(encoded)
	snap.Version = 1
	snap.Checksum = hex.EncodeToString(sum[:])
	data, err := json.Marshal(snap)
	if err != nil || len(data) > TodoMaxSnapshotBytes {
		return errors.New("todo store size limit exceeded")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".todo-")
	if err != nil {
		return errors.New("todo store commit failed")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("todo store commit failed")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("todo store commit failed")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("todo store commit failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("todo store commit failed")
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return errors.New("todo store commit failed")
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func validateTodoSnapshot(snap *todoSnapshot) error {
	if snap.Version != 1 || len(snap.Todos) > TodoMaxRecords || len(snap.History) > TodoMaxHistory {
		return errors.New("invalid todo snapshot")
	}
	seen := map[string]bool{}
	byID := make(map[string]TodoRecord, len(snap.Todos))
	for i := range snap.Todos {
		if err := validateTodo(&snap.Todos[i]); err != nil {
			return err
		}
		if seen[snap.Todos[i].ID] {
			return errors.New("duplicate todo id")
		}
		seen[snap.Todos[i].ID] = true
		byID[snap.Todos[i].ID] = snap.Todos[i]
	}
	type historyState struct {
		version     int
		at          time.Time
		status      string
		completedAt *time.Time
		archivedAt  *time.Time
	}
	history := make(map[string]historyState, len(snap.Todos))
	for _, entry := range snap.History {
		if err := validateSafeID("todo history id", entry.TodoID, 96); err != nil || entry.Version < 1 || entry.At.IsZero() || !timeIsUTC(entry.At) || !validTodoHistoryAction(entry.Action) {
			return errors.New("invalid todo history")
		}
		todo, ok := byID[entry.TodoID]
		if !ok {
			return errors.New("invalid todo history")
		}
		state := history[entry.TodoID]
		if entry.Version != state.version+1 || (!state.at.IsZero() && entry.At.Before(state.at)) {
			return errors.New("invalid todo history")
		}
		if entry.Version == 1 {
			if entry.Action != "create" || !entry.At.Equal(todo.CreatedAt) {
				return errors.New("invalid todo history")
			}
			state.status = TodoStatusOpen
		} else {
			switch entry.Action {
			case "update":
				if state.status == TodoStatusArchived {
					return errors.New("invalid todo history")
				}
			case TodoStatusCompleted:
				if state.status != TodoStatusOpen {
					return errors.New("invalid todo history")
				}
				at := entry.At
				state.status, state.completedAt = TodoStatusCompleted, &at
			case TodoStatusArchived:
				if state.status != TodoStatusOpen && state.status != TodoStatusCompleted {
					return errors.New("invalid todo history")
				}
				at := entry.At
				state.status, state.archivedAt = TodoStatusArchived, &at
			default:
				return errors.New("invalid todo history")
			}
		}
		state.version, state.at = entry.Version, entry.At
		history[entry.TodoID] = state
	}
	for _, todo := range snap.Todos {
		state, ok := history[todo.ID]
		if !ok || state.version != todo.Version || state.status != todo.Status || !state.at.Equal(todo.UpdatedAt) || !sameOptionalTime(state.completedAt, todo.CompletedAt) || !sameOptionalTime(state.archivedAt, todo.ArchivedAt) {
			return errors.New("invalid todo history")
		}
	}
	return nil
}

func appendTodoHistory(snap *todoSnapshot, todoID string, version int, action string, at time.Time) error {
	if len(snap.History) >= TodoMaxHistory {
		return errors.New("todo history limit exceeded")
	}
	snap.History = append(snap.History, TodoHistoryEntry{TodoID: todoID, Version: version, Action: action, At: at.UTC()})
	return nil
}

func validTodoHistoryAction(action string) bool {
	return action == "create" || action == "update" || action == TodoStatusCompleted || action == TodoStatusArchived
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validateTodo(todo *TodoRecord) error {
	if err := validateSafeID("todo id", todo.ID, 96); err != nil {
		return err
	}
	if err := validateTodoText("todo title", todo.Title, TodoMaxTitleBytes, true, false); err != nil {
		return err
	}
	if err := validateTodoText("todo description", todo.Description, TodoMaxDescriptionBytes, false, true); err != nil {
		return err
	}
	if !validTodoPriority(todo.Priority) || !validTodoStatus(todo.Status) || todo.Version < 1 || todo.Version > TodoMaxHistory {
		return errors.New("invalid todo fields")
	}
	if err := validateCanonicalTodoLabels(todo.Labels); err != nil {
		return err
	}
	if todo.CreatedAt.IsZero() || todo.UpdatedAt.IsZero() || !timeIsUTC(todo.CreatedAt) || !timeIsUTC(todo.UpdatedAt) || todo.UpdatedAt.Before(todo.CreatedAt) {
		return errors.New("invalid todo timestamps")
	}
	if todo.DueAt != nil && (todo.DueAt.IsZero() || !timeIsUTC(*todo.DueAt)) {
		return errors.New("invalid todo due date")
	}
	if todo.Status == TodoStatusCompleted && todo.CompletedAt == nil {
		return errors.New("completed todo missing timestamp")
	}
	if todo.Status == TodoStatusOpen && todo.CompletedAt != nil {
		return errors.New("todo completion timestamp is invalid")
	}
	if todo.Status == TodoStatusArchived && todo.ArchivedAt == nil {
		return errors.New("archived todo missing timestamp")
	}
	if todo.Status != TodoStatusArchived && todo.ArchivedAt != nil {
		return errors.New("todo archive timestamp is invalid")
	}
	if todo.CompletedAt != nil && (!timeIsUTC(*todo.CompletedAt) || todo.CompletedAt.Before(todo.CreatedAt) || todo.CompletedAt.After(todo.UpdatedAt)) {
		return errors.New("todo completion timestamp is invalid")
	}
	if todo.ArchivedAt != nil && (!timeIsUTC(*todo.ArchivedAt) || todo.ArchivedAt.Before(todo.CreatedAt) || todo.ArchivedAt.After(todo.UpdatedAt)) {
		return errors.New("todo archive timestamp is invalid")
	}
	return nil
}

func timeIsUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func validTodoStatus(v string) bool {
	return v == TodoStatusOpen || v == TodoStatusCompleted || v == TodoStatusArchived
}
func validTodoPriority(v string) bool {
	return v == TodoPriorityLow || v == TodoPriorityNormal || v == TodoPriorityHigh
}

func validateTodoText(label, value string, max int, required, multiline bool) error {
	if err := validateText(label, value, max, required); err != nil {
		return err
	}
	for _, char := range value {
		if unicode.IsControl(char) && !(multiline && (char == '\n' || char == '\t')) {
			return errors.New("todo text contains control characters")
		}
	}
	return nil
}
func normalizeTodoLabels(labels []string) ([]string, error) {
	if len(labels) > TodoMaxLabels {
		return nil, errors.New("todo has too many labels")
	}
	result := append([]string(nil), labels...)
	seen := map[string]bool{}
	for _, label := range result {
		if err := validateSafeID("todo label", label, TodoMaxLabelBytes); err != nil {
			return nil, err
		}
		if seen[label] {
			return nil, errors.New("todo labels must be unique")
		}
		seen[label] = true
	}
	sort.Strings(result)
	return result, nil
}

func validateCanonicalTodoLabels(labels []string) error {
	normalized, err := normalizeTodoLabels(labels)
	if err != nil {
		return err
	}
	for i := range labels {
		if labels[i] != normalized[i] {
			return errors.New("todo labels are not canonical")
		}
	}
	return nil
}
func normalizeTodoCreate(req TodoCreateRequest) (TodoCreateRequest, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	req.Priority = strings.TrimSpace(req.Priority)
	if req.Priority == "" {
		req.Priority = TodoPriorityNormal
	}
	if !validTodoPriority(req.Priority) {
		return req, errors.New("priority is invalid")
	}
	if err := validateTodoText("title", req.Title, TodoMaxTitleBytes, true, false); err != nil {
		return req, err
	}
	if err := validateTodoText("description", req.Description, TodoMaxDescriptionBytes, false, true); err != nil {
		return req, err
	}
	labels, err := normalizeTodoLabels(req.Labels)
	if err != nil {
		return req, err
	}
	req.Labels = labels
	if req.DueAt != nil && req.DueAt.IsZero() {
		return req, errors.New("due_at is invalid")
	}
	req.DueAt = cloneTime(req.DueAt)
	return req, nil
}
func cloneTime(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	v := in.UTC()
	return &v
}

func (s *TodoStore) Create(ctx context.Context, req TodoCreateRequest) (TodoRecord, error) {
	req, err := normalizeTodoCreate(req)
	if err != nil {
		return TodoRecord{}, err
	}
	now := s.clock().UTC()
	todo := TodoRecord{ID: "todo-" + newStableID(), Title: req.Title, Description: req.Description, Priority: req.Priority, DueAt: req.DueAt, Labels: append([]string(nil), req.Labels...), Status: TodoStatusOpen, Version: 1, CreatedAt: now, UpdatedAt: now}
	err = s.withLock(ctx, func(snap *todoSnapshot) (bool, error) {
		if len(snap.Todos) >= TodoMaxRecords {
			return false, errors.New("todo limit exceeded")
		}
		snap.Todos = append(snap.Todos, todo)
		if err := appendTodoHistory(snap, todo.ID, todo.Version, "create", now); err != nil {
			return false, err
		}
		return true, nil
	})
	return todo, err
}

func (s *TodoStore) Get(ctx context.Context, id string) (TodoRecord, error) {
	if err := validateSafeID("todo id", id, 96); err != nil {
		return TodoRecord{}, err
	}
	snap, err := s.read(ctx)
	if err != nil {
		return TodoRecord{}, err
	}
	for _, todo := range snap.Todos {
		if todo.ID == id {
			return todo, nil
		}
	}
	return TodoRecord{}, ErrTodoNotFound
}

func (s *TodoStore) Show(ctx context.Context, id string) (TodoRecord, error) {
	return s.Get(ctx, id)
}

func (s *TodoStore) List(ctx context.Context, filter TodoListFilter) ([]TodoRecord, bool, error) {
	if filter.Limit == 0 {
		filter.Limit = TodoMaxListLimit
	}
	if filter.Limit < 1 || filter.Limit > TodoMaxListLimit || filter.Offset < 0 || filter.Offset > TodoMaxOffset {
		return nil, false, errors.New("todo list bounds are invalid")
	}
	if filter.Status != "" && !validTodoStatus(filter.Status) {
		return nil, false, errors.New("status is invalid")
	}
	if filter.Priority != "" && !validTodoPriority(filter.Priority) {
		return nil, false, errors.New("priority is invalid")
	}
	if filter.Label != "" {
		if err := validateSafeID("label", filter.Label, TodoMaxLabelBytes); err != nil {
			return nil, false, err
		}
	}
	if filter.DueBefore != nil && filter.DueAfter != nil && !filter.DueAfter.Before(*filter.DueBefore) {
		return nil, false, errors.New("due_after must be before due_before")
	}
	snap, err := s.read(ctx)
	if err != nil {
		return nil, false, err
	}
	rows := make([]TodoRecord, 0, len(snap.Todos))
	for _, todo := range snap.Todos {
		if filter.Status == "" && todo.Status == TodoStatusArchived {
			continue
		}
		if filter.Status != "" && todo.Status != filter.Status {
			continue
		}
		if filter.Priority != "" && todo.Priority != filter.Priority {
			continue
		}
		if filter.Label != "" && !containsTodoLabel(todo.Labels, filter.Label) {
			continue
		}
		if filter.DueBefore != nil && (todo.DueAt == nil || !todo.DueAt.Before(*filter.DueBefore)) {
			continue
		}
		if filter.DueAfter != nil && (todo.DueAt == nil || !todo.DueAt.After(*filter.DueAfter)) {
			continue
		}
		rows = append(rows, todo)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
			return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := filter.Offset < len(rows) && len(rows)-filter.Offset > filter.Limit
	if filter.Offset >= len(rows) {
		return []TodoRecord{}, false, nil
	}
	end := filter.Offset + filter.Limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[filter.Offset:end], truncated, nil
}
func containsTodoLabel(labels []string, wanted string) bool {
	for _, label := range labels {
		if label == wanted {
			return true
		}
	}
	return false
}

func (s *TodoStore) Update(ctx context.Context, req TodoUpdateRequest) (TodoRecord, error) {
	if err := validateSafeID("todo id", req.ID, 96); err != nil {
		return TodoRecord{}, err
	}
	if req.ExpectedVersion < 1 {
		return TodoRecord{}, errors.New("expected_version is required")
	}
	if req.ClearDueAt && req.DueAt != nil {
		return TodoRecord{}, errors.New("due_at cannot be set and cleared together")
	}
	if req.Title == nil && req.Description == nil && req.Priority == nil && req.DueAt == nil && !req.ClearDueAt && req.Labels == nil {
		return TodoRecord{}, errors.New("todo update has no changes")
	}
	var out TodoRecord
	err := s.withLock(ctx, func(snap *todoSnapshot) (bool, error) {
		for i := range snap.Todos {
			todo := &snap.Todos[i]
			if todo.ID != req.ID {
				continue
			}
			if todo.Version != req.ExpectedVersion {
				return false, ErrTodoVersionConflict
			}
			if todo.Status == TodoStatusArchived {
				return false, errors.New("archived todos cannot be updated")
			}
			changed := false
			if req.Title != nil {
				value := strings.TrimSpace(*req.Title)
				if err := validateTodoText("title", value, TodoMaxTitleBytes, true, false); err != nil {
					return false, err
				}
				if value != todo.Title {
					todo.Title, changed = value, true
				}
			}
			if req.Description != nil {
				value := strings.TrimSpace(*req.Description)
				if err := validateTodoText("description", value, TodoMaxDescriptionBytes, false, true); err != nil {
					return false, err
				}
				if value != todo.Description {
					todo.Description, changed = value, true
				}
			}
			if req.Priority != nil {
				value := strings.TrimSpace(*req.Priority)
				if !validTodoPriority(value) {
					return false, errors.New("priority is invalid")
				}
				if value != todo.Priority {
					todo.Priority, changed = value, true
				}
			}
			if req.ClearDueAt {
				if todo.DueAt != nil {
					todo.DueAt, changed = nil, true
				}
			} else if req.DueAt != nil {
				if req.DueAt.IsZero() {
					return false, errors.New("due_at is invalid")
				}
				value := cloneTime(req.DueAt)
				if todo.DueAt == nil || !todo.DueAt.Equal(*value) {
					todo.DueAt, changed = value, true
				}
			}
			if req.Labels != nil {
				labels, err := normalizeTodoLabels(*req.Labels)
				if err != nil {
					return false, err
				}
				if !sameStringSlice(labels, todo.Labels) {
					todo.Labels, changed = labels, true
				}
			}
			if !changed {
				out = cloneTodo(*todo)
				return false, nil
			}
			now := s.clock().UTC()
			todo.Version++
			todo.UpdatedAt = now
			if err := appendTodoHistory(snap, todo.ID, todo.Version, "update", now); err != nil {
				return false, err
			}
			out = cloneTodo(*todo)
			return true, nil
		}
		return false, ErrTodoNotFound
	})
	return out, err
}

func (s *TodoStore) Complete(ctx context.Context, id string, expectedVersion int) (TodoRecord, error) {
	return s.transition(ctx, id, expectedVersion, TodoStatusCompleted)
}
func (s *TodoStore) Archive(ctx context.Context, id string, expectedVersion int) (TodoRecord, error) {
	return s.transition(ctx, id, expectedVersion, TodoStatusArchived)
}
func (s *TodoStore) transition(ctx context.Context, id string, expectedVersion int, target string) (TodoRecord, error) {
	if err := validateSafeID("todo id", id, 96); err != nil {
		return TodoRecord{}, err
	}
	if expectedVersion < 1 {
		return TodoRecord{}, errors.New("expected_version is required")
	}
	var out TodoRecord
	err := s.withLock(ctx, func(snap *todoSnapshot) (bool, error) {
		for i := range snap.Todos {
			todo := &snap.Todos[i]
			if todo.ID != id {
				continue
			}
			if todo.Version != expectedVersion {
				return false, ErrTodoVersionConflict
			}
			now := s.clock().UTC()
			if target == TodoStatusCompleted {
				if todo.Status == TodoStatusArchived {
					return false, errors.New("archived todos cannot be completed")
				}
				if todo.Status == TodoStatusCompleted {
					out = cloneTodo(*todo)
					return false, nil
				}
				todo.Status = target
				todo.CompletedAt = &now
			} else {
				if todo.Status == TodoStatusArchived {
					out = cloneTodo(*todo)
					return false, nil
				}
				todo.Status = target
				todo.ArchivedAt = &now
			}
			todo.Version++
			todo.UpdatedAt = now
			if err := appendTodoHistory(snap, todo.ID, todo.Version, target, now); err != nil {
				return false, err
			}
			out = cloneTodo(*todo)
			return true, nil
		}
		return false, ErrTodoNotFound
	})
	return out, err
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type TodoService struct{ Store *TodoStore }

func NewTodoService(store *TodoStore) *TodoService { return &TodoService{Store: store} }

func (service *TodoService) Create(ctx context.Context, request TodoCreateRequest) (TodoRecord, error) {
	if service == nil || service.Store == nil {
		return TodoRecord{}, errors.New("todo service is not configured")
	}
	return service.Store.Create(ctx, request)
}
func (service *TodoService) Get(ctx context.Context, id string) (TodoRecord, error) {
	if service == nil || service.Store == nil {
		return TodoRecord{}, errors.New("todo service is not configured")
	}
	return service.Store.Get(ctx, id)
}
func (service *TodoService) Show(ctx context.Context, id string) (TodoRecord, error) {
	return service.Get(ctx, id)
}
func (service *TodoService) List(ctx context.Context, filter TodoListFilter) ([]TodoRecord, bool, error) {
	if service == nil || service.Store == nil {
		return nil, false, errors.New("todo service is not configured")
	}
	return service.Store.List(ctx, filter)
}
func (service *TodoService) Update(ctx context.Context, request TodoUpdateRequest) (TodoRecord, error) {
	if service == nil || service.Store == nil {
		return TodoRecord{}, errors.New("todo service is not configured")
	}
	return service.Store.Update(ctx, request)
}
func (service *TodoService) Complete(ctx context.Context, id string, expectedVersion int) (TodoRecord, error) {
	if service == nil || service.Store == nil {
		return TodoRecord{}, errors.New("todo service is not configured")
	}
	return service.Store.Complete(ctx, id, expectedVersion)
}
func (service *TodoService) Archive(ctx context.Context, id string, expectedVersion int) (TodoRecord, error) {
	if service == nil || service.Store == nil {
		return TodoRecord{}, errors.New("todo service is not configured")
	}
	return service.Store.Archive(ctx, id, expectedVersion)
}
