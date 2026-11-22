package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type TodoCreateTool struct{ Store *TodoStore }
type TodoListTool struct{ Store *TodoStore }
type TodoShowTool struct{ Store *TodoStore }
type TodoUpdateTool struct{ Store *TodoStore }
type TodoCompleteTool struct{ Store *TodoStore }
type TodoArchiveTool struct{ Store *TodoStore }

func isParentOwnedTodoTool(name string) bool {
	switch name {
	case "todo_create", "todo_list", "todo_show", "todo_update", "todo_complete", "todo_archive":
		return true
	default:
		return false
	}
}

func NewTodoTools(store *TodoStore) []Tool {
	return []Tool{TodoCreateTool{store}, TodoListTool{store}, TodoShowTool{store}, TodoUpdateTool{store}, TodoCompleteTool{store}, TodoArchiveTool{store}}
}

type todoCreateArgs struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	DueAt       string   `json:"due_at,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}
type todoIDArgs struct {
	ID              string `json:"id"`
	ExpectedVersion int    `json:"expected_version,omitempty"`
}
type todoShowArgs struct {
	ID string `json:"id"`
}
type todoListArgs struct {
	Status    string `json:"status,omitempty"`
	Priority  string `json:"priority,omitempty"`
	Label     string `json:"label,omitempty"`
	DueBefore string `json:"due_before,omitempty"`
	DueAfter  string `json:"due_after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
}

func todoDefinition(name, description string, mutability ToolMutability, risk RiskLevel, params string) ToolDefinition {
	definition := ToolDefinition{Name: name, Description: description, Mutability: mutability, Risk: risk, Parameters: json.RawMessage(params)}
	if mutability == ToolMutabilityMutating {
		definition.PlanExempt = true
	}
	return definition
}
func (tool TodoCreateTool) Definition() ToolDefinition {
	return todoDefinition("todo_create", "Create a durable human/assistant todo. It records metadata only and never executes, schedules, or notifies.", ToolMutabilityMutating, RiskMedium, `{"type":"object","additionalProperties":false,"properties":{"title":{"type":"string","minLength":1,"maxLength":256},"description":{"type":"string","maxLength":4096},"priority":{"type":"string","enum":["low","normal","high"]},"due_at":{"type":"string","description":"RFC3339 timestamp"},"labels":{"type":"array","items":{"type":"string","minLength":1,"maxLength":64},"maxItems":16}},"required":["title"]}`)
}
func (tool TodoListTool) Definition() ToolDefinition {
	return todoDefinition("todo_list", "List bounded durable todos. Archived records are excluded unless status is explicitly archived.", ToolMutabilityReadOnly, RiskLow, `{"type":"object","additionalProperties":false,"properties":{"status":{"type":"string","enum":["open","completed","archived"]},"priority":{"type":"string","enum":["low","normal","high"]},"label":{"type":"string","minLength":1,"maxLength":64},"due_before":{"type":"string"},"due_after":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100},"offset":{"type":"integer","minimum":0,"maximum":10000}}}`)
}
func (tool TodoShowTool) Definition() ToolDefinition {
	return todoDefinition("todo_show", "Show exactly one bounded durable todo.", ToolMutabilityReadOnly, RiskLow, `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string","minLength":1,"maxLength":96}},"required":["id"]}`)
}
func (tool TodoUpdateTool) Definition() ToolDefinition {
	return todoDefinition("todo_update", "Partially update fields on an open or completed todo using optimistic concurrency. Lifecycle fields cannot be forged.", ToolMutabilityMutating, RiskMedium, `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string","minLength":1,"maxLength":96},"expected_version":{"type":"integer","minimum":1},"title":{"type":"string","minLength":1,"maxLength":256},"description":{"type":"string","maxLength":4096},"priority":{"type":"string","enum":["low","normal","high"]},"due_at":{"anyOf":[{"type":"string"},{"type":"null"}]},"labels":{"anyOf":[{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":64}},{"type":"null"}]}},"required":["id","expected_version"]}`)
}
func (tool TodoCompleteTool) Definition() ToolDefinition {
	return todoDefinition("todo_complete", "Explicitly complete one todo and record its completion timestamp.", ToolMutabilityMutating, RiskMedium, `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string","minLength":1,"maxLength":96},"expected_version":{"type":"integer","minimum":1}},"required":["id","expected_version"]}`)
}
func (tool TodoArchiveTool) Definition() ToolDefinition {
	return todoDefinition("todo_archive", "Explicitly archive one todo without destroying its audit history.", ToolMutabilityMutating, RiskMedium, `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string","minLength":1,"maxLength":96},"expected_version":{"type":"integer","minimum":1}},"required":["id","expected_version"]}`)
}

func (tool TodoListTool) SelfBoundedOutputLimit() int { return TodoMaxToolOutputBytes }

func (tool TodoCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("todo store is not configured")
	}
	var in todoCreateArgs
	if rejectTodoNullFields(args, "title", "description", "priority", "due_at", "labels") || parseArgs(args, &in) != nil {
		return "", errors.New("invalid todo_create arguments")
	}
	var due *time.Time
	if strings.TrimSpace(in.DueAt) != "" {
		t, err := time.Parse(time.RFC3339, in.DueAt)
		if err != nil {
			return "", errors.New("due_at must be RFC3339")
		}
		due = &t
	}
	todo, err := tool.Store.Create(ctx, TodoCreateRequest{Title: in.Title, Description: in.Description, Priority: in.Priority, DueAt: due, Labels: in.Labels})
	if err != nil {
		return "", err
	}
	return marshalTodo(todo)
}
func (tool TodoListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("todo store is not configured")
	}
	var in todoListArgs
	if rejectTodoNullFields(args, "status", "priority", "label", "due_before", "due_after", "limit", "offset") || parseArgs(args, &in) != nil {
		return "", errors.New("invalid todo_list arguments")
	}
	before, err := parseTodoTime(in.DueBefore)
	if err != nil {
		return "", errors.New("due_before must be RFC3339")
	}
	after, err := parseTodoTime(in.DueAfter)
	if err != nil {
		return "", errors.New("due_after must be RFC3339")
	}
	rows, truncated, err := tool.Store.List(ctx, TodoListFilter{Status: in.Status, Priority: in.Priority, Label: in.Label, DueBefore: before, DueAfter: after, Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit == 0 {
		limit = TodoMaxListLimit
	}
	return marshalTodoList(rows, truncated, in.Offset, limit)
}
func (tool TodoShowTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("todo store is not configured")
	}
	var in todoShowArgs
	if rejectTodoNullFields(args, "id") || parseArgs(args, &in) != nil {
		return "", errors.New("invalid todo_show arguments")
	}
	row, err := tool.Store.Get(ctx, in.ID)
	if err != nil {
		return "", err
	}
	return marshalTodo(row)
}

type todoUpdateArgs struct {
	ID              string    `json:"id"`
	ExpectedVersion int       `json:"expected_version"`
	Title           *string   `json:"title"`
	Description     *string   `json:"description"`
	Priority        *string   `json:"priority"`
	DueAt           *string   `json:"due_at"`
	Labels          *[]string `json:"labels"`
}

func (in *todoUpdateArgs) UnmarshalJSON(data []byte) error {
	type alias todoUpdateArgs
	var v alias
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("todo update must be an object")
	}
	for _, field := range []string{"id", "expected_version", "title", "description", "priority"} {
		if value, ok := raw[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s must not be null", field)
		}
	}
	*in = todoUpdateArgs(v)
	if value, ok := raw["due_at"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		in.DueAt = nil
	}
	if value, ok := raw["labels"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var clear []string
		in.Labels = &clear
	}
	return nil
}
func (tool TodoUpdateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("todo store is not configured")
	}
	var in todoUpdateArgs
	if err := parseArgs(args, &in); err != nil {
		return "", errors.New("invalid todo_update arguments")
	}
	req := TodoUpdateRequest{ID: in.ID, ExpectedVersion: in.ExpectedVersion, Title: in.Title, Description: in.Description, Priority: in.Priority, Labels: in.Labels}
	if in.DueAt != nil {
		if strings.TrimSpace(*in.DueAt) != "" {
			t, err := time.Parse(time.RFC3339, *in.DueAt)
			if err != nil {
				return "", errors.New("due_at must be RFC3339")
			}
			req.DueAt = &t
		} else {
			req.ClearDueAt = true
		}
	} else {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(args, &raw)
		if value, ok := raw["due_at"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			req.ClearDueAt = true
		}
	}
	row, err := tool.Store.Update(ctx, req)
	if err != nil {
		return "", err
	}
	return marshalTodo(row)
}
func (tool TodoCompleteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return executeTodoTransition(ctx, "todo_complete", tool.Store, args, false)
}
func (tool TodoArchiveTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return executeTodoTransition(ctx, "todo_archive", tool.Store, args, true)
}
func executeTodoTransition(ctx context.Context, name string, store *TodoStore, args json.RawMessage, archive bool) (string, error) {
	if store == nil {
		return "", errors.New("todo store is not configured")
	}
	var in todoIDArgs
	if rejectTodoNullFields(args, "id", "expected_version") || parseArgs(args, &in) != nil {
		return "", fmt.Errorf("invalid %s arguments", name)
	}
	var row TodoRecord
	var err error
	if archive {
		row, err = store.Archive(ctx, in.ID, in.ExpectedVersion)
	} else {
		row, err = store.Complete(ctx, in.ID, in.ExpectedVersion)
	}
	if err != nil {
		return "", err
	}
	return marshalTodo(row)
}

func rejectTodoNullFields(args json.RawMessage, fields ...string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return true
	}
	if raw == nil {
		return true
	}
	for _, field := range fields {
		if value, ok := raw[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return true
		}
	}
	return false
}

func parseTodoTime(value string) (*time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
func marshalTodo(todo TodoRecord) (string, error) {
	data, err := json.Marshal(todo)
	if err != nil {
		return "", errors.New("encode todo result failed")
	}
	return string(data), nil
}
func marshalTodoList(rows []TodoRecord, truncated bool, offset, limit int) (string, error) {
	v := struct {
		Todos      []TodoRecord `json:"todos"`
		Truncated  bool         `json:"truncated,omitempty"`
		Offset     int          `json:"offset"`
		Limit      int          `json:"limit"`
		NextOffset int          `json:"next_offset"`
	}{Todos: rows, Truncated: truncated, Offset: offset, Limit: limit, NextOffset: offset + len(rows)}
	for {
		data, err := json.Marshal(v)
		if err != nil {
			return "", errors.New("encode todo result failed")
		}
		if len(data) <= TodoMaxToolOutputBytes {
			return string(data), nil
		}
		if len(v.Todos) == 0 {
			return "", errors.New("todo list output exceeds bound")
		}
		v.Todos = v.Todos[:len(v.Todos)-1]
		v.Truncated = true
		v.NextOffset = offset + len(v.Todos)
	}
}
