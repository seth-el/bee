package bee

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ParallelOrchestrator runs independent read-only children with a bounded
// concurrency limit. The parent remains authoritative for scheduling,
// cancellation, budgets, dependencies, and trace persistence.
type ParallelOrchestrator struct {
	Executor ChildExecutor
	Trace    TraceSink
}

func NewParallelOrchestrator(executor ChildExecutor, trace TraceSink) (*ParallelOrchestrator, error) {
	if executor == nil {
		return nil, errors.New("child executor is required")
	}
	return &ParallelOrchestrator{Executor: executor, Trace: trace}, nil
}

// ValidateParallel applies the additional fail-closed checks needed before a
// plan may run with more than one child in flight. SequentialOrchestrator
// intentionally does not call this method, preserving the Phase 6C contract.
func (plan OrchestrationPlan) ValidateParallel() error {
	if err := plan.Validate(); err != nil {
		return err
	}
	maxConcurrent := plan.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	artifacts := make([]string, 0, len(plan.Tasks))
	roots := make([]string, 0, len(plan.Tasks))
	for _, planned := range plan.Tasks {
		mutability, err := parallelTaskMutability(planned.Task)
		if err != nil {
			return fmt.Errorf("task %q mutability: %w", planned.Task.TaskID, err)
		}
		if mutability == TaskMutable && maxConcurrent > 1 {
			return fmt.Errorf("task %q is mutable; parallel mutation is unsupported (set max_concurrent to 1)", planned.Task.TaskID)
		}
		root, err := planned.Task.Scope.rootPath()
		if err != nil {
			return fmt.Errorf("task %q scope: %w", planned.Task.TaskID, err)
		}
		artifact, err := childArtifactDir(planned.Task)
		if err != nil {
			return fmt.Errorf("task %q artifacts: %w", planned.Task.TaskID, err)
		}
		roots = append(roots, root)
		artifacts = append(artifacts, artifact)
	}
	// An artifact directory is written by the child boundary itself. It must
	// not sit inside another task's scope, where a concurrent read could race
	// with completion.json creation or observe another task's private output.
	for i := range artifacts {
		for j := range roots {
			if i == j || filepath.Clean(roots[i]) == filepath.Clean(roots[j]) {
				continue
			}
			if pathsOverlap(artifacts[i], roots[j]) {
				return fmt.Errorf("task artifact directory %q conflicts with task %q scope %q", artifacts[i], plan.Tasks[j].Task.TaskID, roots[j])
			}
		}
	}
	return nil
}

func parallelTaskMutability(task ChildTask) (TaskMutability, error) {
	switch task.Mutability {
	case TaskMutable:
		return TaskMutable, nil
	case TaskReadOnly:
		if unsafe, unknown := taskToolRisk(task.AllowedTools); unsafe != "" {
			return "", fmt.Errorf("read-only task allows mutation-capable tool %q", unsafe)
		} else if unknown != "" {
			return "", fmt.Errorf("read-only task allows tool %q with unknown mutability", unknown)
		}
		return TaskReadOnly, nil
	case TaskMutabilityUnspecified:
		if len(task.AllowedTools) == 0 {
			return TaskReadOnly, nil
		}
		if unsafe, _ := taskToolRisk(task.AllowedTools); unsafe != "" {
			return "", fmt.Errorf("task mutability must be explicit for mutation-capable tool %q", unsafe)
		}
		if _, unknown := taskToolRisk(task.AllowedTools); unknown != "" {
			return "", fmt.Errorf("task mutability is ambiguous for tool %q", unknown)
		}
		return TaskReadOnly, nil
	default:
		return "", fmt.Errorf("invalid mutability %q", task.Mutability)
	}
}

// taskToolRisk deliberately recognizes only the built-in tools whose risk is
// part of this package's stable contract. Unknown/custom tools fail closed in
// an inferred or declared read-only task rather than guessing their effects.
func taskToolRisk(tools []string) (unsafe, unknown string) {
	for _, name := range tools {
		switch name {
		case "read_file", "list_files", "find_files", "grep":
			continue
		case "write_file", "edit_file", "run_command":
			if unsafe == "" {
				unsafe = name
			}
		default:
			if unknown == "" {
				unknown = name
			}
		}
	}
	return unsafe, unknown
}

type parallelRunResult struct {
	id     string
	packet ChildCompletionPacket
	err    error
}

type taskBudgetReservation struct {
	steps  int
	output int
}

func (orchestrator *ParallelOrchestrator) Run(ctx context.Context, plan OrchestrationPlan) (OrchestrationResult, error) {
	result := OrchestrationResult{OrchestrationID: plan.OrchestrationID, ParentID: plan.ParentID}
	if err := plan.ValidateParallel(); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A one-slot parallel plan is intentionally delegated to the proven 6C
	// implementation, including its exact ordering and result semantics.
	if plan.MaxConcurrent <= 1 {
		sequential, err := NewSequentialOrchestrator(orchestrator.Executor, orchestrator.Trace)
		if err != nil {
			return result, err
		}
		return sequential.Run(ctx, plan)
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

	var traceMu sync.Mutex
	var traceErr error
	record := func(event TraceEvent) error {
		if orchestrator.Trace == nil {
			return nil
		}
		traceMu.Lock()
		defer traceMu.Unlock()
		if traceErr != nil {
			return traceErr
		}
		event.OrchestrationID = plan.OrchestrationID
		event.ParentID = plan.ParentID
		if event.ID == "" {
			event.ID = newStableID()
		}
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		if err := orchestrator.Trace.AppendTrace(runCtx, event); err != nil {
			traceErr = err
			cancel()
			return err
		}
		return nil
	}
	observe := func(taskID string) ChildObserver {
		return childObserverFunc(func(observation ChildObservation) {
			if err := record(TraceEvent{TaskID: taskID, Type: "child_event", Observation: &observation}); err != nil {
				return
			}
		})
	}
	if err := record(TraceEvent{Type: "orchestration_started"}); err != nil {
		result.Status = OrchestrationFailed
		result.Failure = err.Error()
		return result, err
	}

	maxConcurrent := plan.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	results := make(chan parallelRunResult, len(plan.Tasks))
	running := make(map[string]bool, len(plan.Tasks))
	completed := 0
	usedSteps, usedOutput := 0, 0
	reservedSteps, reservedOutput := 0, 0
	reservations := make(map[string]taskBudgetReservation, len(plan.Tasks))
	stopReason := ""
	setStop := func(reason string) {
		if stopReason == "" {
			stopReason = reason
			cancel()
		}
	}
	markPending := func(status OrchestrationTaskStatus, failure string) {
		for _, id := range pendingTaskIDs(states) {
			state := states[id]
			state.Status = status
			state.Failure = failure
			states[id] = state
			completed++
			_ = record(TraceEvent{TaskID: id, Type: "task_" + string(status), Status: string(status), Failure: failure})
		}
	}
	launch := func(id string) {
		plannedTask := planned[id]
		boundedTask, reservation, budgetErr := reserveTaskBudget(plannedTask.Task, plan.Budgets, usedSteps, usedOutput, reservedSteps, reservedOutput)
		if budgetErr != nil {
			state := states[id]
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = budgetErr.Error()
			states[id] = state
			completed++
			_ = record(TraceEvent{TaskID: id, Type: "task_budget_exceeded", Status: string(state.Status), Failure: state.Failure})
			setStop("budget")
			return
		}
		plannedTask.Task = boundedTask
		reservations[id] = reservation
		reservedSteps += reservation.steps
		reservedOutput += reservation.output
		state := states[id]
		state.Status, state.Attempts = OrchestrationTaskRunning, 1
		states[id] = state
		running[id] = true
		if err := record(TraceEvent{TaskID: id, Type: "task_started", Status: string(state.Status), Attempt: 1}); err != nil {
			delete(running, id)
			setStop("trace")
			return
		}
		go func() {
			var packet ChildCompletionPacket
			var runErr error
			if observable, ok := orchestrator.Executor.(ChildObservableExecutor); ok {
				packet, runErr = observable.RunObserved(runCtx, plannedTask.Task, observe(id))
			} else {
				packet, runErr = orchestrator.Executor.Run(runCtx, plannedTask.Task)
			}
			results <- parallelRunResult{id: id, packet: packet, err: runErr}
		}()
	}

	for completed < len(plan.Tasks) {
		traceMu.Lock()
		currentTraceErr := traceErr
		traceMu.Unlock()
		if currentTraceErr != nil {
			setStop("trace")
		}
		if stopReason == "" && ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				setStop("timeout")
			} else {
				setStop("cancel")
			}
		}
		if stopReason == "" && runCtx.Err() != nil {
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				setStop("timeout")
			} else {
				setStop("cancel")
			}
		}
		if stopReason == "" {
			for len(running) < maxConcurrent {
				ready := readyTaskIDs(plan.Tasks, states)
				if len(ready) == 0 {
					break
				}
				launch(ready[0])
				if stopReason != "" {
					break
				}
			}
		} else if len(running) == 0 {
			status := OrchestrationTaskBlocked
			if stopReason == "cancel" || stopReason == "timeout" {
				status = OrchestrationTaskCancelled
			}
			failure := "orchestration stopped"
			if runCtx.Err() != nil {
				failure = runCtx.Err().Error()
			}
			markPending(status, failure)
		}

		if len(running) == 0 {
			if completed >= len(plan.Tasks) {
				break
			}
			if stopReason != "" {
				break
			}
			progress := false
			for _, id := range pendingTaskIDs(states) {
				if dependencyBlocked(planned[id], states) {
					state := states[id]
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
			setStop("no_progress")
			markPending(OrchestrationTaskBlocked, "orchestration made no dependency progress")
			break
		}

		run := <-results
		delete(running, run.id)
		reservation := reservations[run.id]
		delete(reservations, run.id)
		reservedSteps -= reservation.steps
		reservedOutput -= reservation.output
		if stopReason == "" && runCtx.Err() != nil {
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				setStop("timeout")
			} else {
				setStop("cancel")
			}
		}
		state := states[run.id]
		packet := run.packet
		state.Packet = &packet
		state.Artifacts = append([]string(nil), packet.Artifacts...)
		usedSteps += packet.Steps
		usedOutput += completionOutputBytes(packet)
		state.Status = mapChildStatus(packet.Status)
		if run.err != nil {
			state.Failure = run.err.Error()
		} else if state.Status != OrchestrationTaskSucceeded {
			state.Failure = strings.Join(packet.Blockers, "; ")
			if state.Failure == "" {
				state.Failure = packet.Summary
			}
		}
		if (stopReason == "cancel" || stopReason == "timeout") && run.err != nil && (errors.Is(run.err, context.Canceled) || errors.Is(run.err, context.DeadlineExceeded)) {
			if stopReason == "timeout" {
				state.Status = OrchestrationTaskTimedOut
			} else {
				state.Status = OrchestrationTaskCancelled
			}
		}
		if state.Status == OrchestrationTaskSucceeded && run.err != nil {
			state.Status = OrchestrationTaskFailed
		}
		if state.Status == OrchestrationTaskPending || state.Status == OrchestrationTaskRunning {
			state.Status = OrchestrationTaskFailed
		}
		if plan.Budgets.MaxSteps > 0 && usedSteps > plan.Budgets.MaxSteps {
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = "aggregate step budget exceeded"
			setStop("budget")
		}
		if plan.Budgets.MaxOutputBytes > 0 && usedOutput > plan.Budgets.MaxOutputBytes {
			state.Status = OrchestrationTaskBudgetExceeded
			state.Failure = "aggregate output budget exceeded"
			setStop("budget")
		}
		states[run.id] = state
		completed++
		_ = record(TraceEvent{TaskID: run.id, Type: "task_completed", Status: string(state.Status), Attempt: 1, Completion: &packet, Artifacts: packet.Artifacts, Failure: state.Failure})
		if stopReason == "" && state.Status != OrchestrationTaskSucceeded && (plan.FailurePolicy == "" || plan.FailurePolicy == FailureStopOnError || state.Status == OrchestrationTaskBudgetExceeded) {
			setStop("failure")
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
	switch stopReason {
	case "budget":
		result.Status = OrchestrationBudgetExceeded
		result.Failure = "aggregate orchestration budget exceeded"
	case "cancel", "timeout":
		if stopReason == "timeout" {
			result.Status = OrchestrationBudgetExceeded
		} else {
			result.Status = OrchestrationCancelled
		}
		result.Failure = runCtx.Err().Error()
	case "trace":
		result.Status = OrchestrationFailed
		result.Failure = traceErr.Error()
	case "failure":
		result.Status = OrchestrationFailed
		for _, task := range result.Tasks {
			if task.Status != OrchestrationTaskSucceeded {
				result.Failure = task.Failure
				break
			}
		}
	case "no_progress":
		result.Status = OrchestrationFailed
		result.Failure = "orchestration made no dependency progress"
	default:
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
	if traceErr != nil {
		return result, traceErr
	}
	return result, nil
}

func reserveTaskBudget(task ChildTask, budget OrchestrationBudgets, usedSteps, usedOutput, reservedSteps, reservedOutput int) (ChildTask, taskBudgetReservation, error) {
	reservation := taskBudgetReservation{}
	if budget.MaxSteps > 0 {
		available := budget.MaxSteps - usedSteps - reservedSteps
		if available <= 0 {
			return task, reservation, errors.New("aggregate step budget exhausted")
		}
		requested := task.Budget.MaxSteps
		if requested <= 0 {
			requested = DefaultMaxToolSteps
		}
		if requested > available {
			requested = available
		}
		task.Budget.MaxSteps = requested
		reservation.steps = requested
	}
	if budget.MaxOutputBytes > 0 {
		available := budget.MaxOutputBytes - usedOutput - reservedOutput
		if available <= 0 {
			return task, reservation, errors.New("aggregate output budget exhausted")
		}
		requested := task.Budget.MaxOutputBytes
		if requested <= 0 {
			requested = task.Scope.maxOutput()
		}
		if requested > available {
			requested = available
		}
		task.Budget.MaxOutputBytes = requested
		reservation.output = requested
	}
	return task, reservation, nil
}
