package bee

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// OrchestrationPlan is a parent-owned, acyclic plan for sequential child work.
type OrchestrationPlan struct {
	OrchestrationID string                     `json:"orchestration_id"`
	ParentID        string                     `json:"parent_id"`
	Tasks           []PlannedChild             `json:"tasks"`
	Budgets         OrchestrationBudgets       `json:"budgets,omitempty"`
	FailurePolicy   OrchestrationFailurePolicy `json:"failure_policy,omitempty"`
	// MaxConcurrent bounds parallel read-only children. Zero means one; a
	// value greater than one is accepted only by ParallelOrchestrator.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

type PlannedChild struct {
	Task      ChildTask `json:"task"`
	DependsOn []string  `json:"depends_on,omitempty"`
}

type OrchestrationBudgets struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxSteps       int `json:"max_steps,omitempty"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

func (budget OrchestrationBudgets) Validate() error {
	if budget.TimeoutSeconds < 0 || budget.MaxSteps < 0 || budget.MaxOutputBytes < 0 {
		return errors.New("orchestration budgets must not be negative")
	}
	return nil
}

type OrchestrationFailurePolicy string

const (
	FailureStopOnError         OrchestrationFailurePolicy = "stop_on_error"
	FailureContinueIndependent OrchestrationFailurePolicy = "continue_independent"
)

func (policy OrchestrationFailurePolicy) valid() bool {
	return policy == "" || policy == FailureStopOnError || policy == FailureContinueIndependent
}

func (plan OrchestrationPlan) Validate() error {
	if err := validateChildID("orchestration_id", plan.OrchestrationID); err != nil {
		return err
	}
	if err := validateChildID("parent_id", plan.ParentID); err != nil {
		return err
	}
	if len(plan.Tasks) == 0 {
		return errors.New("orchestration plan requires at least one task")
	}
	if err := plan.Budgets.Validate(); err != nil {
		return err
	}
	if plan.MaxConcurrent < 0 {
		return errors.New("orchestration max_concurrent must not be negative")
	}
	if !plan.FailurePolicy.valid() {
		return fmt.Errorf("orchestration failure policy %q is invalid", plan.FailurePolicy)
	}
	ids := make(map[string]struct{}, len(plan.Tasks))
	artifactDirs := make([]string, 0, len(plan.Tasks))
	for _, planned := range plan.Tasks {
		if err := planned.Task.Validate(); err != nil {
			return fmt.Errorf("task %q: %w", planned.Task.TaskID, err)
		}
		if planned.Task.ParentID != plan.ParentID {
			return fmt.Errorf("task %q parent id does not match orchestration parent", planned.Task.TaskID)
		}
		if _, exists := ids[planned.Task.TaskID]; exists {
			return fmt.Errorf("orchestration task %q is duplicated", planned.Task.TaskID)
		}
		ids[planned.Task.TaskID] = struct{}{}
		if err := validateArtifactRelationship(planned.Task); err != nil {
			return fmt.Errorf("task %q artifacts: %w", planned.Task.TaskID, err)
		}
		artifact, err := childArtifactDir(planned.Task)
		if err != nil {
			return fmt.Errorf("task %q artifacts: %w", planned.Task.TaskID, err)
		}
		artifactDirs = append(artifactDirs, artifact)
	}
	for _, planned := range plan.Tasks {
		seen := make(map[string]struct{}, len(planned.DependsOn))
		for _, dependency := range planned.DependsOn {
			if dependency == planned.Task.TaskID {
				return fmt.Errorf("task %q cannot depend on itself", planned.Task.TaskID)
			}
			if _, exists := ids[dependency]; !exists {
				return fmt.Errorf("task %q depends on missing task %q", planned.Task.TaskID, dependency)
			}
			if _, exists := seen[dependency]; exists {
				return fmt.Errorf("task %q repeats dependency %q", planned.Task.TaskID, dependency)
			}
			seen[dependency] = struct{}{}
		}
	}
	for i := range artifactDirs {
		for j := i + 1; j < len(artifactDirs); j++ {
			if pathsOverlap(artifactDirs[i], artifactDirs[j]) {
				return fmt.Errorf("task artifact directories overlap: %q and %q", artifactDirs[i], artifactDirs[j])
			}
		}
	}
	if cycle := findPlanCycle(plan.Tasks); len(cycle) > 0 {
		return fmt.Errorf("orchestration dependency cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

func validateArtifactRelationship(task ChildTask) error {
	root, err := task.Scope.rootPath()
	if err != nil {
		return err
	}
	dir := task.ArtifactDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join("generated", "children", task.TaskID)
	}
	candidate, err := filepath.Abs(filepath.Join(root, dir))
	if filepath.IsAbs(dir) {
		candidate, err = filepath.Abs(dir)
	}
	if err != nil {
		return err
	}
	candidate = filepath.Clean(candidate)
	if !scopeWithin(root, candidate) {
		return errors.New("artifact directory must remain within task scope")
	}
	if candidate == root {
		return errors.New("artifact directory must not be the task scope root")
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	left, _ = filepath.Abs(left)
	right, _ = filepath.Abs(right)
	return scopeWithin(left, right) || scopeWithin(right, left) || filepath.Clean(left) == filepath.Clean(right)
}

func findPlanCycle(tasks []PlannedChild) []string {
	deps := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		deps[task.Task.TaskID] = append([]string(nil), task.DependsOn...)
		sort.Strings(deps[task.Task.TaskID])
	}
	state := make(map[string]uint8, len(tasks))
	var stack []string
	var visit func(string) []string
	visit = func(id string) []string {
		if state[id] == 1 {
			for i, value := range stack {
				if value == id {
					return append(append([]string(nil), stack[i:]...), id)
				}
			}
			return []string{id}
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		for _, dependency := range deps[id] {
			if cycle := visit(dependency); len(cycle) > 0 {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.Task.TaskID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if cycle := visit(id); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

type OrchestrationTaskStatus string

const (
	OrchestrationTaskPending        OrchestrationTaskStatus = "pending"
	OrchestrationTaskRunning        OrchestrationTaskStatus = "running"
	OrchestrationTaskSucceeded      OrchestrationTaskStatus = "succeeded"
	OrchestrationTaskFailed         OrchestrationTaskStatus = "failed"
	OrchestrationTaskBlocked        OrchestrationTaskStatus = "blocked"
	OrchestrationTaskCancelled      OrchestrationTaskStatus = "cancelled"
	OrchestrationTaskTimedOut       OrchestrationTaskStatus = "timed_out"
	OrchestrationTaskBudgetExceeded OrchestrationTaskStatus = "budget_exceeded"
)

type OrchestrationTaskResult struct {
	TaskID    string                  `json:"task_id"`
	Status    OrchestrationTaskStatus `json:"status"`
	Attempts  int                     `json:"attempts"`
	Packet    *ChildCompletionPacket  `json:"packet,omitempty"`
	Artifacts []string                `json:"artifacts,omitempty"`
	Failure   string                  `json:"failure,omitempty"`
}

type OrchestrationStatus string

const (
	OrchestrationSucceeded      OrchestrationStatus = "succeeded"
	OrchestrationFailed         OrchestrationStatus = "failed"
	OrchestrationCancelled      OrchestrationStatus = "cancelled"
	OrchestrationBudgetExceeded OrchestrationStatus = "budget_exceeded"
)

type OrchestrationResult struct {
	OrchestrationID string                    `json:"orchestration_id"`
	ParentID        string                    `json:"parent_id"`
	Status          OrchestrationStatus       `json:"status"`
	Tasks           []OrchestrationTaskResult `json:"tasks"`
	Artifacts       []string                  `json:"artifacts,omitempty"`
	Failure         string                    `json:"failure,omitempty"`
	Steps           int                       `json:"steps"`
	OutputBytes     int                       `json:"output_bytes"`
}

// TraceEvent is a bounded append-only record for parent-owned orchestration.
type TraceEvent struct {
	ID              string                 `json:"id"`
	At              time.Time              `json:"at"`
	OrchestrationID string                 `json:"orchestration_id"`
	ParentID        string                 `json:"parent_id,omitempty"`
	TaskID          string                 `json:"task_id,omitempty"`
	Type            string                 `json:"type"`
	Status          string                 `json:"status,omitempty"`
	Attempt         int                    `json:"attempt,omitempty"`
	Info            string                 `json:"info,omitempty"`
	Observation     *ChildObservation      `json:"observation,omitempty"`
	Completion      *ChildCompletionPacket `json:"completion,omitempty"`
	Artifacts       []string               `json:"artifacts,omitempty"`
	Failure         string                 `json:"failure,omitempty"`
}

type TraceSink interface {
	AppendTrace(context.Context, TraceEvent) error
}

type MemoryTraceSink struct {
	mu     sync.Mutex
	events []TraceEvent
}

func (sink *MemoryTraceSink) AppendTrace(_ context.Context, event TraceEvent) error {
	if sink == nil {
		return errors.New("trace sink is nil")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, boundTraceEvent(event))
	return nil
}

func (sink *MemoryTraceSink) Events() []TraceEvent {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]TraceEvent(nil), sink.events...)
}

type JSONLTraceSink struct {
	path string
	mu   sync.Mutex
}

func NewJSONLTraceSink(path string) (*JSONLTraceSink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("trace path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	return &JSONLTraceSink{path: filepath.Clean(abs)}, nil
}

func (sink *JSONLTraceSink) AppendTrace(_ context.Context, event TraceEvent) error {
	if sink == nil {
		return errors.New("trace sink is nil")
	}
	bounded := boundTraceEvent(event)
	if bounded.ID == "" {
		bounded.ID = newStableID()
	}
	if bounded.At.IsZero() {
		bounded.At = time.Now().UTC()
	}
	data, err := json.Marshal(bounded)
	if err != nil {
		return fmt.Errorf("encode trace event: %w", err)
	}
	if len(data) > 1<<20 {
		return errors.New("trace event exceeds 1 MiB")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	file, err := os.OpenFile(sink.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	data = append(data, '\n')
	_, err = file.Write(data)
	return err
}

func boundTraceEvent(event TraceEvent) TraceEvent {
	event.Info = truncateText(event.Info, 4096)
	event.Failure = truncateText(event.Failure, 4096)
	event.Artifacts = boundedStrings(event.Artifacts, 256, 512)
	if event.Observation != nil {
		copy := *event.Observation
		copy.Delta = truncateText(copy.Delta, 4096)
		copy.Info = truncateText(copy.Info, 4096)
		if copy.Message != nil {
			value := *copy.Message
			value.Content = truncateText(value.Content, 4096)
			value.Name = truncateText(value.Name, 256)
			value.ToolCallID = truncateText(value.ToolCallID, 256)
			value.ToolCalls = append([]ToolCall(nil), value.ToolCalls...)
			if len(value.ToolCalls) > 256 {
				value.ToolCalls = value.ToolCalls[:256]
			}
			for i := range value.ToolCalls {
				value.ToolCalls[i].ID = truncateText(value.ToolCalls[i].ID, 256)
				value.ToolCalls[i].Type = truncateText(value.ToolCalls[i].Type, 64)
				value.ToolCalls[i].Name = truncateText(value.ToolCalls[i].Name, 256)
				value.ToolCalls[i].Arguments = truncateText(value.ToolCalls[i].Arguments, 4096)
			}
			copy.Message = &value
		}
		if copy.ToolCall != nil {
			value := *copy.ToolCall
			value.ID = truncateText(value.ID, 256)
			value.Type = truncateText(value.Type, 64)
			value.Name = truncateText(value.Name, 256)
			value.Arguments = truncateText(value.Arguments, 4096)
			copy.ToolCall = &value
		}
		if copy.Approval != nil {
			value := *copy.Approval
			value.Request.Summary = truncateText(value.Request.Summary, 4096)
			value.Request.Args = nil
			value.Reason = truncateText(value.Reason, 4096)
			copy.Approval = &value
		}
		if copy.ToolResult != nil {
			value := *copy.ToolResult
			value.Output = truncateText(value.Output, 4096)
			value.Error = truncateText(value.Error, 4096)
			copy.ToolResult = &value
		}
		if copy.Completion != nil {
			value := boundCompletion(*copy.Completion)
			copy.Completion = &value
		}
		event.Observation = &copy
	}
	if event.Completion != nil {
		value := boundCompletion(*event.Completion)
		event.Completion = &value
	}
	return event
}

func boundCompletion(packet ChildCompletionPacket) ChildCompletionPacket {
	packet.Summary = truncateText(packet.Summary, 4096)
	packet.ChangedFiles = boundedStrings(packet.ChangedFiles, 256, 512)
	packet.Commands = boundedStrings(packet.Commands, 256, 512)
	packet.Tests = boundedStrings(packet.Tests, 256, 512)
	packet.Evidence = boundedStrings(packet.Evidence, 256, 4096)
	packet.Uncertainty = boundedStrings(packet.Uncertainty, 64, 1024)
	packet.Artifacts = boundedStrings(packet.Artifacts, 256, 512)
	packet.Questions = boundedStrings(packet.Questions, 64, 1024)
	packet.Blockers = boundedStrings(packet.Blockers, 64, 2048)
	packet.FallbackReason = truncateText(packet.FallbackReason, 1024)
	if len(packet.Attempts) > 8 {
		packet.Attempts = packet.Attempts[:8]
	}
	for index := range packet.Attempts {
		packet.Attempts[index].Backend = truncateText(packet.Attempts[index].Backend, 128)
		packet.Attempts[index].AttemptID = truncateText(packet.Attempts[index].AttemptID, 128)
		packet.Attempts[index].WorkspacePath = truncateText(packet.Attempts[index].WorkspacePath, 512)
		packet.Attempts[index].Summary = truncateText(packet.Attempts[index].Summary, 2048)
	}
	return packet
}

func boundedStrings(values []string, maxItems, maxEach int) []string {
	if len(values) > maxItems {
		values = values[:maxItems]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, truncateText(value, maxEach))
	}
	return result
}

// SequentialOrchestrator runs at most one child at a time and owns all
// dependency, cancellation, budget, and failure decisions.
type SequentialOrchestrator struct {
	Executor ChildExecutor
	Trace    TraceSink
}

func NewSequentialOrchestrator(executor ChildExecutor, trace TraceSink) (*SequentialOrchestrator, error) {
	if executor == nil {
		return nil, errors.New("child executor is required")
	}
	return &SequentialOrchestrator{Executor: executor, Trace: trace}, nil
}

func (orchestrator *SequentialOrchestrator) Run(ctx context.Context, plan OrchestrationPlan) (OrchestrationResult, error) {
	result := OrchestrationResult{OrchestrationID: plan.OrchestrationID, ParentID: plan.ParentID}
	if err := plan.Validate(); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if plan.Budgets.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, time.Duration(plan.Budgets.TimeoutSeconds)*time.Second)
		defer timeoutCancel()
	}
	states := make(map[string]OrchestrationTaskResult, len(plan.Tasks))
	planned := make(map[string]PlannedChild, len(plan.Tasks))
	for _, task := range plan.Tasks {
		planned[task.Task.TaskID] = task
		states[task.Task.TaskID] = OrchestrationTaskResult{TaskID: task.Task.TaskID, Status: OrchestrationTaskPending}
	}
	record := func(event TraceEvent) error {
		if orchestrator.Trace == nil {
			return nil
		}
		event.OrchestrationID = plan.OrchestrationID
		event.ParentID = plan.ParentID
		if event.ID == "" {
			event.ID = newStableID()
		}
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		return orchestrator.Trace.AppendTrace(runCtx, event)
	}
	if err := record(TraceEvent{Type: "orchestration_started"}); err != nil {
		result.Status = OrchestrationFailed
		result.Failure = err.Error()
		return result, err
	}
	var traceMu sync.Mutex
	var traceErr error
	observe := func(taskID string) ChildObserver {
		return childObserverFunc(func(observation ChildObservation) {
			traceMu.Lock()
			defer traceMu.Unlock()
			if traceErr != nil {
				return
			}
			if err := record(TraceEvent{TaskID: taskID, Type: "child_event", Observation: &observation}); err != nil {
				traceErr = err
				cancel()
			}
		})
	}
	completed := 0
	usedSteps, usedOutput := 0, 0
	for completed < len(plan.Tasks) {
		if err := traceError(&traceMu, &traceErr); err != nil {
			result.Status = OrchestrationFailed
			result.Failure = err.Error()
			break
		}
		if runCtx.Err() != nil {
			for _, id := range pendingTaskIDs(states) {
				state := states[id]
				state.Status = OrchestrationTaskCancelled
				state.Failure = runCtx.Err().Error()
				states[id] = state
				completed++
				_ = record(TraceEvent{TaskID: id, Type: "task_cancelled", Status: string(state.Status), Failure: state.Failure})
			}
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				result.Status = OrchestrationBudgetExceeded
			} else {
				result.Status = OrchestrationCancelled
			}
			result.Failure = runCtx.Err().Error()
			break
		}
		ready := readyTaskIDs(plan.Tasks, states)
		if len(ready) == 0 {
			progress := false
			for _, id := range pendingTaskIDs(states) {
				state := states[id]
				if dependencyBlocked(planned[id], states) {
					state.Status = OrchestrationTaskBlocked
					state.Failure = "a dependency did not succeed"
					states[id] = state
					completed++
					progress = true
					_ = record(TraceEvent{TaskID: id, Type: "task_blocked", Status: string(state.Status), Failure: state.Failure})
				}
			}
			if progress {
				continue
			}
			result.Status = OrchestrationFailed
			result.Failure = "orchestration made no dependency progress"
			break
		}
		id := ready[0]
		plannedTask := planned[id]
		boundedTask, budgetErr := limitTaskToRemainingBudget(plannedTask.Task, plan.Budgets, usedSteps, usedOutput)
		state := states[id]
		if budgetErr != nil {
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = budgetErr.Error()
			states[id] = state
			completed++
			_ = record(TraceEvent{TaskID: id, Type: "task_budget_exceeded", Status: string(state.Status), Failure: state.Failure})
			for _, pendingID := range pendingTaskIDs(states) {
				pending := states[pendingID]
				pending.Status = OrchestrationTaskBlocked
				pending.Failure = "orchestration aggregate budget exhausted"
				states[pendingID] = pending
				completed++
				_ = record(TraceEvent{TaskID: pendingID, Type: "task_blocked", Status: string(pending.Status), Failure: pending.Failure})
			}
			result.Status = OrchestrationBudgetExceeded
			result.Failure = state.Failure
			break
		}
		plannedTask.Task = boundedTask
		state.Status, state.Attempts = OrchestrationTaskRunning, 1
		states[id] = state
		if err := record(TraceEvent{TaskID: id, Type: "task_started", Status: string(state.Status), Attempt: 1}); err != nil {
			result.Status = OrchestrationFailed
			result.Failure = err.Error()
			break
		}
		var packet ChildCompletionPacket
		var runErr error
		if observable, ok := orchestrator.Executor.(ChildObservableExecutor); ok {
			packet, runErr = observable.RunObserved(runCtx, plannedTask.Task, observe(id))
		} else {
			packet, runErr = orchestrator.Executor.Run(runCtx, plannedTask.Task)
		}
		if err := traceError(&traceMu, &traceErr); err != nil {
			runErr = err
		}
		state.Packet = &packet
		state.Artifacts = append([]string(nil), packet.Artifacts...)
		usedSteps += packet.Steps
		usedOutput += completionOutputBytes(packet)
		state.Status = mapChildStatus(packet.Status)
		if runErr != nil {
			state.Failure = runErr.Error()
		} else if state.Status != OrchestrationTaskSucceeded {
			state.Failure = strings.Join(packet.Blockers, "; ")
			if state.Failure == "" {
				state.Failure = packet.Summary
			}
		}
		if state.Status == OrchestrationTaskSucceeded && runErr != nil {
			state.Status = OrchestrationTaskFailed
		}
		if state.Status == OrchestrationTaskPending || state.Status == OrchestrationTaskRunning {
			state.Status = OrchestrationTaskFailed
		}
		if plan.Budgets.MaxSteps > 0 && usedSteps > plan.Budgets.MaxSteps {
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = "aggregate step budget exceeded"
		}
		if plan.Budgets.MaxOutputBytes > 0 && usedOutput > plan.Budgets.MaxOutputBytes {
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = "aggregate output budget exceeded"
		}
		states[id] = state
		completed++
		if err := record(TraceEvent{TaskID: id, Type: "task_completed", Status: string(state.Status), Attempt: 1, Completion: &packet, Artifacts: packet.Artifacts, Failure: state.Failure}); err != nil {
			result.Status = OrchestrationFailed
			result.Failure = err.Error()
			break
		}
		if runCtx.Err() != nil {
			for _, pendingID := range pendingTaskIDs(states) {
				pending := states[pendingID]
				pending.Status = OrchestrationTaskCancelled
				pending.Failure = runCtx.Err().Error()
				states[pendingID] = pending
				completed++
				_ = record(TraceEvent{TaskID: pendingID, Type: "task_cancelled", Status: string(pending.Status), Failure: pending.Failure})
			}
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				result.Status = OrchestrationBudgetExceeded
			} else {
				result.Status = OrchestrationCancelled
			}
			result.Failure = runCtx.Err().Error()
			break
		}
		if state.Status != OrchestrationTaskSucceeded && (plan.FailurePolicy == "" || plan.FailurePolicy == FailureStopOnError || state.Status == OrchestrationTaskBudgetExceeded) {
			for _, pendingID := range pendingTaskIDs(states) {
				pending := states[pendingID]
				pending.Status = OrchestrationTaskBlocked
				pending.Failure = "orchestration stopped after task failure"
				states[pendingID] = pending
				completed++
				_ = record(TraceEvent{TaskID: pendingID, Type: "task_blocked", Status: string(pending.Status), Failure: pending.Failure})
			}
			result.Status = OrchestrationFailed
			if state.Status == OrchestrationTaskBudgetExceeded {
				result.Status = OrchestrationBudgetExceeded
			}
			result.Failure = state.Failure
			break
		}
	}
	for _, task := range plan.Tasks {
		state := states[task.Task.TaskID]
		if state.Status == OrchestrationTaskPending {
			state.Status = OrchestrationTaskBlocked
			state.Failure = "task was not run"
		}
		result.Tasks = append(result.Tasks, state)
		result.Artifacts = append(result.Artifacts, state.Artifacts...)
	}
	result.Artifacts = uniqueStrings(result.Artifacts)
	result.Steps, result.OutputBytes = usedSteps, usedOutput
	if result.Status == "" {
		result.Status = OrchestrationSucceeded
		for _, task := range result.Tasks {
			if task.Status != OrchestrationTaskSucceeded {
				result.Status = OrchestrationFailed
				result.Failure = task.Failure
				break
			}
		}
	}
	if err := record(TraceEvent{Type: "orchestration_completed", Status: string(result.Status), Artifacts: result.Artifacts, Failure: result.Failure}); err != nil {
		if result.Failure == "" {
			result.Failure = err.Error()
		}
		if result.Status == OrchestrationSucceeded {
			result.Status = OrchestrationFailed
		}
		return result, err
	}
	return result, nil
}

func traceError(mu *sync.Mutex, target *error) error { mu.Lock(); defer mu.Unlock(); return *target }

func pendingTaskIDs(states map[string]OrchestrationTaskResult) []string {
	ids := make([]string, 0, len(states))
	for id, state := range states {
		if state.Status == OrchestrationTaskPending {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func readyTaskIDs(tasks []PlannedChild, states map[string]OrchestrationTaskResult) []string {
	var ready []string
	for _, planned := range tasks {
		state := states[planned.Task.TaskID]
		if state.Status != OrchestrationTaskPending {
			continue
		}
		ok := true
		for _, dependency := range planned.DependsOn {
			if states[dependency].Status != OrchestrationTaskSucceeded {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, planned.Task.TaskID)
		}
	}
	sort.Strings(ready)
	return ready
}

func dependencyBlocked(task PlannedChild, states map[string]OrchestrationTaskResult) bool {
	for _, dependency := range task.DependsOn {
		status := states[dependency].Status
		if status != OrchestrationTaskPending && status != OrchestrationTaskRunning && status != OrchestrationTaskSucceeded {
			return true
		}
	}
	return false
}

func mapChildStatus(status ChildStatus) OrchestrationTaskStatus {
	switch status {
	case ChildSucceeded:
		return OrchestrationTaskSucceeded
	case ChildCancelled:
		return OrchestrationTaskCancelled
	case ChildTimedOut:
		return OrchestrationTaskTimedOut
	case ChildBudgetExceeded:
		return OrchestrationTaskBudgetExceeded
	default:
		return OrchestrationTaskFailed
	}
}

func limitTaskToRemainingBudget(task ChildTask, budget OrchestrationBudgets, usedSteps, usedOutput int) (ChildTask, error) {
	if budget.MaxSteps > 0 {
		remaining := budget.MaxSteps - usedSteps
		if remaining <= 0 {
			return task, errors.New("aggregate step budget exhausted")
		}
		if task.Budget.MaxSteps <= 0 || task.Budget.MaxSteps > remaining {
			task.Budget.MaxSteps = remaining
		}
	}
	if budget.MaxOutputBytes > 0 {
		remaining := budget.MaxOutputBytes - usedOutput
		if remaining <= 0 {
			return task, errors.New("aggregate output budget exhausted")
		}
		if task.Budget.MaxOutputBytes <= 0 || task.Budget.MaxOutputBytes > remaining {
			task.Budget.MaxOutputBytes = remaining
		}
	}
	return task, nil
}

func completionOutputBytes(packet ChildCompletionPacket) int {
	total := len(packet.Summary)
	for _, value := range packet.Evidence {
		total += len(value)
	}
	return total
}

// ReplayTrace reads a JSONL trace without loading arbitrary session context.
func ReplayTrace(path string) ([]TraceEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var events []TraceEvent
	for scanner.Scan() {
		var event TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}
