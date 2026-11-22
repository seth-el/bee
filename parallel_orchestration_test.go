package bee

import (
	"context"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type parallelFakeExecutor struct {
	mu          sync.Mutex
	packets     map[string]ChildCompletionPacket
	started     chan string
	gate        <-chan struct{}
	waitForGate bool
	current     int
	maxCurrent  int
	order       []string
}

func (fake *parallelFakeExecutor) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return fake.RunObserved(ctx, task, nil)
}

func (fake *parallelFakeExecutor) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	fake.mu.Lock()
	fake.current++
	if fake.current > fake.maxCurrent {
		fake.maxCurrent = fake.current
	}
	fake.order = append(fake.order, task.TaskID)
	packet := fake.packets[task.TaskID]
	fake.mu.Unlock()
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "started"})
	if fake.started != nil {
		fake.started <- task.TaskID
	}
	if fake.waitForGate {
		select {
		case <-fake.gate:
		case <-ctx.Done():
			fake.mu.Lock()
			fake.current--
			fake.mu.Unlock()
			packet.Status = ChildCancelled
			packet.Summary = "cancelled"
			return packet, ctx.Err()
		}
	}
	fake.mu.Lock()
	fake.current--
	fake.mu.Unlock()
	if packet.Status == "" {
		packet.Status = ChildSucceeded
	}
	if packet.Summary == "" {
		packet.Summary = string(packet.Status)
	}
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "completed", Completion: &packet})
	return packet, nil
}

func (fake *parallelFakeExecutor) stats() (int, []string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.maxCurrent, append([]string(nil), fake.order...)
}

func readOnlyOrchestrationTask(root, id string) ChildTask {
	task := orchestrationTask(root, id)
	task.Mutability = TaskReadOnly
	task.AllowedTools = []string{"read_file", "grep"}
	return task
}

func TestParallelOrchestrationMaxConcurrencyAndDeterministicReadiness(t *testing.T) {
	root := t.TempDir()
	gate := make(chan struct{})
	fake := &parallelFakeExecutor{
		packets: map[string]ChildCompletionPacket{
			"a": {Status: ChildSucceeded, Summary: "a"},
			"b": {Status: ChildSucceeded, Summary: "b"},
			"c": {Status: ChildSucceeded, Summary: "c"},
		},
		started:     make(chan string, 3),
		gate:        gate,
		waitForGate: true,
	}
	sink := &MemoryTraceSink{}
	orchestrator, err := NewParallelOrchestrator(fake, sink)
	if err != nil {
		t.Fatal(err)
	}
	plan := orchestrationPlan(root,
		PlannedChild{Task: readOnlyOrchestrationTask(root, "c"), DependsOn: []string{"a", "b"}},
		PlannedChild{Task: readOnlyOrchestrationTask(root, "b")},
		PlannedChild{Task: readOnlyOrchestrationTask(root, "a")},
	)
	plan.MaxConcurrent = 2
	resultCh := make(chan OrchestrationResult, 1)
	go func() {
		result, _ := orchestrator.Run(context.Background(), plan)
		resultCh <- result
	}()
	started := map[string]bool{<-fake.started: true, <-fake.started: true}
	if !started["a"] || !started["b"] {
		t.Fatalf("started tasks = %#v", started)
	}
	max, order := fake.stats()
	if max != 2 {
		t.Fatalf("max concurrency = %d, order = %#v", max, order)
	}
	close(gate)
	result := <-resultCh
	if result.Status != OrchestrationSucceeded {
		t.Fatalf("result = %#v", result)
	}
	_, order = fake.stats()
	if len(order) != 3 || order[2] != "c" {
		t.Fatalf("start order = %#v", order)
	}
	events := sink.Events()
	var starts []string
	for _, event := range events {
		if event.Type == "task_started" {
			starts = append(starts, event.TaskID)
		}
	}
	if !reflect.DeepEqual(starts, []string{"a", "b", "c"}) {
		t.Fatalf("deterministic task-start trace = %#v", starts)
	}
}

func TestParallelOrchestrationCancellationFansOutToRunningTasks(t *testing.T) {
	root := t.TempDir()
	gate := make(chan struct{})
	fake := &parallelFakeExecutor{
		packets: map[string]ChildCompletionPacket{
			"a": {Status: ChildSucceeded, Summary: "a"},
			"b": {Status: ChildSucceeded, Summary: "b"},
		},
		started:     make(chan string, 2),
		gate:        gate,
		waitForGate: true,
	}
	orchestrator, _ := NewParallelOrchestrator(fake, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plan := orchestrationPlan(root, PlannedChild{Task: readOnlyOrchestrationTask(root, "a")}, PlannedChild{Task: readOnlyOrchestrationTask(root, "b")})
	plan.MaxConcurrent = 2
	resultCh := make(chan OrchestrationResult, 1)
	go func() {
		result, _ := orchestrator.Run(ctx, plan)
		resultCh <- result
	}()
	<-fake.started
	<-fake.started
	cancel()
	result := <-resultCh
	if result.Status != OrchestrationCancelled {
		t.Fatalf("result = %#v", result)
	}
	for _, task := range result.Tasks {
		if task.Status != OrchestrationTaskCancelled {
			t.Fatalf("task %q was not cancelled: %#v", task.TaskID, task)
		}
	}
}

func TestParallelOrchestrationAggregateTimeout(t *testing.T) {
	root := t.TempDir()
	gate := make(chan struct{})
	fake := &parallelFakeExecutor{
		packets:     map[string]ChildCompletionPacket{"a": {Status: ChildSucceeded, Summary: "a"}},
		started:     make(chan string, 1),
		gate:        gate,
		waitForGate: true,
	}
	orchestrator, _ := NewParallelOrchestrator(fake, nil)
	plan := orchestrationPlan(root, PlannedChild{Task: readOnlyOrchestrationTask(root, "a")})
	plan.MaxConcurrent = 2
	plan.Budgets.TimeoutSeconds = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := orchestrator.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OrchestrationBudgetExceeded {
		t.Fatalf("timeout result = %#v", result)
	}
}

func TestParallelOrchestrationAggregateBudgetsAndTrace(t *testing.T) {
	root := t.TempDir()
	fake := &parallelFakeExecutor{packets: map[string]ChildCompletionPacket{
		"a": {Status: ChildSucceeded, Summary: "a", Steps: 2},
		"b": {Status: ChildSucceeded, Summary: strings.Repeat("b", 8), Steps: 2},
	}}
	sink := &MemoryTraceSink{}
	orchestrator, _ := NewParallelOrchestrator(fake, sink)
	plan := orchestrationPlan(root, PlannedChild{Task: readOnlyOrchestrationTask(root, "a")}, PlannedChild{Task: readOnlyOrchestrationTask(root, "b")})
	plan.MaxConcurrent = 2
	plan.Budgets.MaxSteps = 3
	plan.Budgets.MaxOutputBytes = 10
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OrchestrationBudgetExceeded || result.Steps > plan.Budgets.MaxSteps {
		t.Fatalf("budget result = %#v", result)
	}
	_, order := fake.stats()
	if len(order) != 1 || order[0] != "a" {
		t.Fatalf("budget started tasks = %#v", order)
	}
	events := sink.Events()
	var started, completed bool
	for _, event := range events {
		started = started || event.Type == "task_started"
		completed = completed || event.Type == "orchestration_completed"
	}
	if !started || !completed {
		t.Fatalf("trace events = %#v", events)
	}
}

func TestParallelPlanRejectsUnsafeMutabilityAndConflictingScopes(t *testing.T) {
	root := t.TempDir()
	mutation := orchestrationTask(root, "mutate")
	mutation.AllowedTools = []string{"write_file"}
	plan := orchestrationPlan(root, PlannedChild{Task: mutation})
	plan.MaxConcurrent = 2
	if err := plan.ValidateParallel(); err == nil || !strings.Contains(err.Error(), "mutability") {
		t.Fatalf("unsafe inferred mutability error = %v", err)
	}
	plan.Tasks[0].Task.Mutability = TaskReadOnly
	if err := plan.ValidateParallel(); err == nil || !strings.Contains(err.Error(), "mutation-capable") {
		t.Fatalf("read-only mutation error = %v", err)
	}
	plan.Tasks[0].Task.Mutability = TaskMutable
	if err := plan.ValidateParallel(); err == nil || !strings.Contains(err.Error(), "parallel mutation") {
		t.Fatalf("parallel mutation error = %v", err)
	}

	parent := t.TempDir()
	childRoot := parent + "/nested"
	if err := os.Mkdir(childRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	first := readOnlyOrchestrationTask(parent, "first")
	second := readOnlyOrchestrationTask(childRoot, "second")
	first.ArtifactDir = "nested/artifacts"
	conflict := orchestrationPlan(parent, PlannedChild{Task: first}, PlannedChild{Task: second})
	conflict.MaxConcurrent = 2
	if err := conflict.ValidateParallel(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("scope/artifact conflict error = %v", err)
	}
}

func TestParallelOrchestratorMaxOnePreservesSequentialCompatibility(t *testing.T) {
	root := t.TempDir()
	fake := &parallelFakeExecutor{packets: map[string]ChildCompletionPacket{
		"a": {Status: ChildSucceeded, Summary: "a"},
		"b": {Status: ChildSucceeded, Summary: "b"},
	}}
	orchestrator, _ := NewParallelOrchestrator(fake, nil)
	plan := orchestrationPlan(root, PlannedChild{Task: readOnlyOrchestrationTask(root, "b")}, PlannedChild{Task: readOnlyOrchestrationTask(root, "a")})
	plan.MaxConcurrent = 1
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil || result.Status != OrchestrationSucceeded {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	_, order := fake.stats()
	if !reflect.DeepEqual(order, []string{"a", "b"}) {
		t.Fatalf("sequential order = %#v", order)
	}
}

func TestParallelPlanAllowsExplicitMutableTaskOnlyWhenSerialized(t *testing.T) {
	root := t.TempDir()
	task := orchestrationTask(root, "mutable")
	task.Mutability = TaskMutable
	plan := orchestrationPlan(root, PlannedChild{Task: task})
	plan.MaxConcurrent = 1
	if err := plan.ValidateParallel(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewParallelOrchestrator(&parallelFakeExecutor{packets: map[string]ChildCompletionPacket{"mutable": {Status: ChildSucceeded, Summary: "ok"}}}, nil); err != nil {
		t.Fatal(err)
	}
}
