package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxJobToolPromptBytes = 16 << 10
	maxJobToolTextBytes   = 128
	maxJobToolListSize    = 32
	maxJobAttempts        = 8
)

type JobCreateTool struct{ Store *JobStore }
type JobListTool struct{ Store *JobStore }
type JobShowTool struct{ Store *JobStore }
type JobApproveTool struct{ Store *JobStore }
type JobCancelTool struct{ Store *JobStore }
type JobRecoverTool struct{ Store *JobStore }

// NewJobTools returns the parent-owned foreground job lifecycle tools. These
// tools persist and inspect jobs only; they never execute a scheduled prompt
// or grant job authority to a child executor.
func NewJobTools(store *JobStore) []Tool {
	return []Tool{
		JobCreateTool{Store: store},
		JobListTool{Store: store},
		JobShowTool{Store: store},
		JobApproveTool{Store: store},
		JobCancelTool{Store: store},
		JobRecoverTool{Store: store},
	}
}

func isParentOwnedJobTool(name string) bool {
	switch name {
	case "job_create", "job_list", "job_show", "job_approve", "job_cancel", "job_recover":
		return true
	default:
		return false
	}
}

type jobCreateArgs struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name"`
	Owner          string `json:"owner"`
	IdempotencyKey string `json:"idempotency_key"`
	Prompt         string `json:"prompt"`
	RunAt          string `json:"run_at"`
	MaxAttempts    int    `json:"max_attempts"`
}

type jobIDArgs struct {
	ID string `json:"id"`
}

type jobListArgs struct {
	Owner string   `json:"owner,omitempty"`
	State JobState `json:"state,omitempty"`
	Limit int      `json:"limit,omitempty"`
}

type jobRecoverArgs struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

type jobToolView struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Owner          string        `json:"owner"`
	IdempotencyKey string        `json:"idempotency_key"`
	RunAt          time.Time     `json:"run_at"`
	State          JobState      `json:"state"`
	Approval       ApprovalState `json:"approval"`
	Attempts       int           `json:"attempts"`
	MaxAttempts    int           `json:"max_attempts"`
	LastError      string        `json:"last_error,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	FinishedAt     *time.Time    `json:"finished_at,omitempty"`
}

type jobListView struct {
	Jobs      []jobToolView `json:"jobs"`
	Truncated bool          `json:"truncated,omitempty"`
}

func (tool JobCreateTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_create",
		Description: "Create one bounded, approval-required, one-shot Bee job. This records the prompt for explicit operator-driven execution later; it does not run the prompt now.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},"owner":{"type":"string"},"idempotency_key":{"type":"string"},"prompt":{"type":"string"},"run_at":{"type":"string","description":"RFC3339 timestamp"},"max_attempts":{"type":"integer","minimum":1,"maximum":8}},"required":["name","owner","idempotency_key","prompt","run_at","max_attempts"]}`),
	}
}

func (tool JobCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := requireJobToolContext(ctx); err != nil {
		return "", err
	}
	if tool.Store == nil {
		return "", errors.New("job store is not configured")
	}
	var input jobCreateArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("job_create arguments: %w", err)
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Owner = strings.TrimSpace(input.Owner)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if err := validateJobIdentifier("name", input.Name, false); err != nil {
		return "", err
	}
	if err := validateJobIdentifier("owner", input.Owner, false); err != nil {
		return "", err
	}
	if err := validateJobIdentifier("idempotency_key", input.IdempotencyKey, false); err != nil {
		return "", err
	}
	if input.ID != "" {
		if err := validateJobIdentifier("id", input.ID, true); err != nil {
			return "", err
		}
	} else {
		input.ID = newStableID()
	}
	if len(input.Prompt) == 0 || len(input.Prompt) > maxJobToolPromptBytes {
		return "", fmt.Errorf("prompt must be between 1 and %d bytes", maxJobToolPromptBytes)
	}
	if input.MaxAttempts < 1 || input.MaxAttempts > maxJobAttempts {
		return "", fmt.Errorf("max_attempts must be between 1 and %d", maxJobAttempts)
	}
	runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(input.RunAt))
	if err != nil {
		return "", errors.New("run_at must be RFC3339")
	}
	job := Job{
		ID:             input.ID,
		Name:           input.Name,
		Owner:          input.Owner,
		IdempotencyKey: input.IdempotencyKey,
		Schedule:       Schedule{RunAt: runAt.UTC()},
		Request:        JobRequest{Prompt: input.Prompt, Title: input.Name},
		MaxAttempts:    input.MaxAttempts,
	}
	if err := tool.Store.Create(job, time.Now().UTC()); err != nil {
		return "", err
	}
	created, err := tool.Store.Get(job.ID)
	if err != nil {
		return "", err
	}
	return marshalJobToolView(created)
}

func (tool JobListTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_list",
		Description: "List a bounded view of parent-owned Bee jobs. Prompt contents and attempt details are never returned.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"state":{"type":"string","enum":["pending","approved","running","succeeded","failed","paused","canceled","recoverable"]},"limit":{"type":"integer","minimum":1,"maximum":32}}}`),
	}
}

func (tool JobListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := requireJobToolContext(ctx); err != nil {
		return "", err
	}
	if tool.Store == nil {
		return "", errors.New("job store is not configured")
	}
	var input jobListArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("job_list arguments: %w", err)
	}
	if input.Limit == 0 {
		input.Limit = maxJobToolListSize
	}
	if input.Limit < 1 || input.Limit > maxJobToolListSize {
		return "", fmt.Errorf("limit must be between 1 and %d", maxJobToolListSize)
	}
	if input.Owner != "" {
		if err := validateJobIdentifier("owner", input.Owner, false); err != nil {
			return "", err
		}
	}
	if input.State != "" && !validJobState(input.State) {
		return "", fmt.Errorf("state %q is invalid", input.State)
	}
	jobs, err := tool.Store.List()
	if err != nil {
		return "", err
	}
	views := make([]jobToolView, 0, minJobInt(len(jobs), input.Limit))
	for _, job := range jobs {
		if input.Owner != "" && job.Owner != input.Owner {
			continue
		}
		if input.State != "" && job.State != input.State {
			continue
		}
		views = append(views, safeJobToolView(job))
		if len(views) == input.Limit {
			break
		}
	}
	return marshalJobListView(jobListView{Jobs: views, Truncated: countMatchingJobs(jobs, input.Owner, input.State) > len(views)})
}

func (tool JobShowTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_show",
		Description: "Show one bounded, redacted view of a parent-owned Bee job. Prompt contents and attempt details are never returned.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (tool JobShowTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return executeJobLookup(ctx, "job_show", tool.Store, args)
}

func (tool JobApproveTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_approve",
		Description: "Explicitly approve one pending Bee job for later operator-driven execution. Approval is never automatic.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (tool JobApproveTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return executeJobTransition(ctx, "job_approve", tool.Store, args, func(store *JobStore, id string, now time.Time) error { return store.Approve(id, now) })
}

func (tool JobCancelTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_cancel",
		Description: "Cancel one non-terminal Bee job. A running execution is still finalized as canceled if the scheduler observes the cancellation.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (tool JobCancelTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return executeJobTransition(ctx, "job_cancel", tool.Store, args, func(store *JobStore, id string, now time.Time) error { return store.Cancel(id, now) })
}

func (tool JobRecoverTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "job_recover",
		Description: "Explicitly recover a stale running Bee job as retry or fail after its lease expires.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"action":{"type":"string","enum":["retry","fail"]}},"required":["id","action"]}`),
	}
}

func (tool JobRecoverTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := requireJobToolContext(ctx); err != nil {
		return "", err
	}
	if tool.Store == nil {
		return "", errors.New("job store is not configured")
	}
	var input jobRecoverArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("job_recover arguments: %w", err)
	}
	if err := validateJobID(input.ID); err != nil {
		return "", err
	}
	if input.Action != "retry" && input.Action != "fail" {
		return "", errors.New("action must be retry or fail")
	}
	if err := tool.Store.Recover(input.ID, input.Action, time.Now().UTC()); err != nil {
		return "", err
	}
	job, err := tool.Store.Get(input.ID)
	if err != nil {
		return "", err
	}
	return marshalJobToolView(job)
}

func executeJobLookup(ctx context.Context, name string, store *JobStore, args json.RawMessage) (string, error) {
	if err := requireJobToolContext(ctx); err != nil {
		return "", err
	}
	if store == nil {
		return "", errors.New("job store is not configured")
	}
	var input jobIDArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("%s arguments: %w", name, err)
	}
	if err := validateJobID(input.ID); err != nil {
		return "", err
	}
	job, err := store.Get(input.ID)
	if err != nil {
		return "", err
	}
	return marshalJobToolView(job)
}

func executeJobTransition(ctx context.Context, name string, store *JobStore, args json.RawMessage, transition func(*JobStore, string, time.Time) error) (string, error) {
	if err := requireJobToolContext(ctx); err != nil {
		return "", err
	}
	if store == nil {
		return "", errors.New("job store is not configured")
	}
	var input jobIDArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("%s arguments: %w", name, err)
	}
	if err := validateJobID(input.ID); err != nil {
		return "", err
	}
	if err := transition(store, input.ID, time.Now().UTC()); err != nil {
		return "", err
	}
	job, err := store.Get(input.ID)
	if err != nil {
		return "", err
	}
	return marshalJobToolView(job)
}

func requireJobToolContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("job tool context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validateJobIdentifier(label, value string, generatedID bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > maxJobToolTextBytes {
		return fmt.Errorf("%s must not exceed %d bytes", label, maxJobToolTextBytes)
	}
	if strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("%s contains an unsafe character", label)
	}
	if generatedID || label == "id" {
		if strings.ContainsAny(value, "\\/ ") {
			return fmt.Errorf("%s contains an unsafe character", label)
		}
	}
	return nil
}

func validateJobID(id string) error {
	return validateJobIdentifier("id", id, true)
}

func validJobState(state JobState) bool {
	switch state {
	case JobPending, JobApproved, JobRunning, JobSucceeded, JobFailed, JobPaused, JobCanceled, JobRecoverable:
		return true
	default:
		return false
	}
}

func safeJobToolView(job Job) jobToolView {
	return jobToolView{
		ID:             boundedJobText(job.ID, maxJobToolTextBytes),
		Name:           boundedJobText(job.Name, maxJobToolTextBytes),
		Owner:          boundedJobText(job.Owner, maxJobToolTextBytes),
		IdempotencyKey: boundedJobText(job.IdempotencyKey, maxJobToolTextBytes),
		RunAt:          job.Schedule.RunAt,
		State:          job.State,
		Approval:       job.Approval,
		Attempts:       job.Attempts,
		MaxAttempts:    job.MaxAttempts,
		LastError:      boundedJobText(redactString(job.LastError), 1024),
		CreatedAt:      job.CreatedAt,
		UpdatedAt:      job.UpdatedAt,
		FinishedAt:     job.FinishedAt,
	}
}

func boundedJobText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func marshalJobToolView(job Job) (string, error) {
	return marshalBoundedJobValue(safeJobToolView(job))
}

func marshalJobListView(view jobListView) (string, error) {
	return marshalBoundedJobValue(view)
}

func marshalBoundedJobValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func countMatchingJobs(jobs []Job, owner string, state JobState) int {
	count := 0
	for _, job := range jobs {
		if owner != "" && job.Owner != owner {
			continue
		}
		if state != "" && job.State != state {
			continue
		}
		count++
	}
	return count
}

func minJobInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
