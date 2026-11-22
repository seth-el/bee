package bee

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeOrchestrationExecutor struct {
	mu       sync.Mutex
	packets  map[string]ChildCompletionPacket
	order    []string
	started  chan string
	block    string
	observed []ChildObservation
}

func (fake *fakeOrchestrationExecutor) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return fake.RunObserved(ctx, task, nil)
}

func (fake *fakeOrchestrationExecutor) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	fake.mu.Lock()
	fake.order = append(fake.order, task.TaskID)
	packet := fake.packets[task.TaskID]
	fake.mu.Unlock()
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "started"})
	if fake.started != nil {
		select {
		case fake.started <- task.TaskID:
		default:
		}
	}
	if task.TaskID == fake.block {
		<-ctx.Done()
		packet.Status = ChildCancelled
		packet.Summary = "cancelled"
	}
	if packet.Summary == "" {
		packet.Summary = string(packet.Status)
	}
	if packet.Status == "" {
		packet.Status = ChildSucceeded
	}
	call := ToolCall{ID: task.TaskID + "-call", Name: "read_file"}
	fake.mu.Lock()
	fake.observed = append(fake.observed, ChildObservation{TaskID: task.TaskID, Type: "tool_call", ToolCall: &call})
	fake.mu.Unlock()
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "tool_call", ToolCall: &call})
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "completed", Info: string(packet.Status), Completion: &packet})
	return packet, nil
}

func orchestrationTask(root, id string) ChildTask {
	return ChildTask{TaskID: id, ParentID: "parent", Objective: "objective", Prompt: "prompt", Scope: ExecutionScope{Root: root, TimeoutSeconds: 2, MaxOutputBytes: 1024}, Budget: ChildBudget{TimeoutSeconds: 1, MaxSteps: 4, MaxOutputBytes: 128}, AcceptanceCriteria: []string{"done"}}
}

func orchestrationPlan(root string, tasks ...PlannedChild) OrchestrationPlan {
	return OrchestrationPlan{OrchestrationID: "orch-1", ParentID: "parent", Tasks: tasks, Budgets: OrchestrationBudgets{TimeoutSeconds: 5, MaxSteps: 20, MaxOutputBytes: 1024}, FailurePolicy: FailureContinueIndependent}
}

func TestOrchestrationPlanRejectsDuplicateMissingCycleAndUnsafeArtifacts(t *testing.T) {
	root := t.TempDir()
	base := orchestrationTask(root, "a")
	for name, plan := range map[string]OrchestrationPlan{
		"duplicate": orchestrationPlan(root, PlannedChild{Task: base}, PlannedChild{Task: base}),
		"missing":   orchestrationPlan(root, PlannedChild{Task: base, DependsOn: []string{"missing"}}),
		"cycle":     orchestrationPlan(root, PlannedChild{Task: orchestrationTask(root, "a"), DependsOn: []string{"b"}}, PlannedChild{Task: orchestrationTask(root, "b"), DependsOn: []string{"a"}}),
		"artifact escape": func() OrchestrationPlan {
			task := base
			task.ArtifactDir = filepath.Dir(root)
			return orchestrationPlan(root, PlannedChild{Task: task})
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := plan.Validate(); err == nil {
				t.Fatalf("plan unexpectedly validated")
			}
		})
	}
}

func TestSequentialOrchestrationDeterministicDAGAndTracePersistence(t *testing.T) {
	root := t.TempDir()
	fake := &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{
		"a": {Status: ChildSucceeded, Summary: "a", Steps: 1, Artifacts: []string{"generated/a.txt"}},
		"b": {Status: ChildSucceeded, Summary: "b", Steps: 1},
		"c": {Status: ChildSucceeded, Summary: "c", Steps: 1},
	}}
	sink := &MemoryTraceSink{}
	orchestrator, err := NewSequentialOrchestrator(fake, sink)
	if err != nil {
		t.Fatal(err)
	}
	plan := orchestrationPlan(root,
		PlannedChild{Task: orchestrationTask(root, "c"), DependsOn: []string{"a", "b"}},
		PlannedChild{Task: orchestrationTask(root, "b")},
		PlannedChild{Task: orchestrationTask(root, "a")},
	)
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OrchestrationSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(fake.order, []string{"a", "b", "c"}) {
		t.Fatalf("order = %#v", fake.order)
	}
	if got := len(result.Artifacts); got != 1 || result.Artifacts[0] != "generated/a.txt" {
		t.Fatalf("artifacts = %#v", result.Artifacts)
	}
	events := sink.Events()
	if len(events) < 8 {
		t.Fatalf("trace events = %d", len(events))
	}
	var sawChild, sawCompletion bool
	for _, event := range events {
		sawChild = sawChild || event.Type == "child_event"
		sawCompletion = sawCompletion || event.Type == "orchestration_completed"
	}
	if !sawChild || !sawCompletion {
		t.Fatalf("trace lacks child/completion events: %#v", events)
	}
}

func TestSequentialOrchestrationFailureBlocksDependents(t *testing.T) {
	root := t.TempDir()
	fake := &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{
		"a": {Status: ChildFailed, Summary: "failed", Blockers: []string{"nope"}},
		"b": {Status: ChildSucceeded, Summary: "independent"},
		"c": {Status: ChildSucceeded, Summary: "should not run"},
	}}
	orchestrator, _ := NewSequentialOrchestrator(fake, nil)
	plan := orchestrationPlan(root, PlannedChild{Task: orchestrationTask(root, "a")}, PlannedChild{Task: orchestrationTask(root, "b")}, PlannedChild{Task: orchestrationTask(root, "c"), DependsOn: []string{"a"}})
	plan.FailurePolicy = FailureContinueIndependent
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.order, []string{"a", "b"}) {
		t.Fatalf("order = %#v", fake.order)
	}
	for _, task := range result.Tasks {
		if task.TaskID == "c" && task.Status != OrchestrationTaskBlocked {
			t.Fatalf("dependent task = %#v", task)
		}
	}
}

func TestSequentialOrchestrationCancellationAndAggregateBudget(t *testing.T) {
	root := t.TempDir()
	fake := &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{"a": {Status: ChildSucceeded, Summary: "12345", Steps: 2}}, block: "b", started: make(chan string, 1)}
	orchestrator, _ := NewSequentialOrchestrator(fake, nil)
	plan := orchestrationPlan(root, PlannedChild{Task: orchestrationTask(root, "a")}, PlannedChild{Task: orchestrationTask(root, "b")})
	plan.Budgets.MaxSteps = 1
	result, err := orchestrator.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OrchestrationBudgetExceeded {
		t.Fatalf("budget result = %#v", result)
	}
	if len(fake.order) != 1 {
		t.Fatalf("budget ran tasks = %#v", fake.order)
	}

	fake = &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{"a": {Status: ChildSucceeded, Summary: "a"}}, block: "a", started: make(chan string, 1)}
	orchestrator, _ = NewSequentialOrchestrator(fake, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-fake.started; cancel() }()
	result, err = orchestrator.Run(ctx, orchestrationPlan(root, PlannedChild{Task: orchestrationTask(root, "a")}, PlannedChild{Task: orchestrationTask(root, "b")}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OrchestrationCancelled {
		t.Fatalf("cancel result = %#v", result)
	}
}

func TestJSONLTraceSinkAndChildEventForwarding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "trace.jsonl")
	sink, err := NewJSONLTraceSink(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{"a": {Status: ChildSucceeded, Summary: strings.Repeat("x", 10000), Artifacts: []string{"a"}}}}
	orchestrator, _ := NewSequentialOrchestrator(fake, sink)
	_, err = orchestrator.Run(context.Background(), orchestrationPlan(root, PlannedChild{Task: orchestrationTask(root, "a")}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := ReplayTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("trace is empty")
	}
	for _, event := range events {
		if event.Observation != nil && len(event.Observation.Delta) > 4096 {
			t.Fatal("unbounded observation")
		}
	}
	if len(fake.observed) != 1 || fake.observed[0].ToolCall == nil {
		t.Fatalf("fake observations = %#v", fake.observed)
	}
}

func TestSequentialOrchestrationTraceFailureFailsClosed(t *testing.T) {
	badSink := traceSinkFunc(func(context.Context, TraceEvent) error { return errors.New("disk full") })
	fake := &fakeOrchestrationExecutor{packets: map[string]ChildCompletionPacket{"a": {Status: ChildSucceeded, Summary: "a"}}}
	orchestrator, _ := NewSequentialOrchestrator(fake, badSink)
	result, err := orchestrator.Run(context.Background(), orchestrationPlan(t.TempDir(), PlannedChild{Task: orchestrationTask(t.TempDir(), "a")}))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("trace error = %v", err)
	}
	if result.Status != "" && result.Status != OrchestrationFailed {
		t.Fatalf("result = %#v", result)
	}
}

type traceSinkFunc func(context.Context, TraceEvent) error

func (fn traceSinkFunc) AppendTrace(ctx context.Context, event TraceEvent) error {
	return fn(ctx, event)
}

var _ = time.Second
