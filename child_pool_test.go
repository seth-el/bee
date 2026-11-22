package bee

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type poolBarrierProvider struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	active  int
	peak    int
}

func (provider *poolBarrierProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.Stream(ctx, request, nil)
}

func (provider *poolBarrierProvider) Stream(ctx context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.active++
	if provider.active > provider.peak {
		provider.peak = provider.active
	}
	provider.mu.Unlock()
	defer func() {
		provider.mu.Lock()
		provider.active--
		provider.mu.Unlock()
	}()
	select {
	case provider.started <- struct{}{}:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	select {
	case <-provider.release:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	if onDelta != nil {
		if err := onDelta("child complete"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "child complete"}}, nil
}

func (provider *poolBarrierProvider) Peak() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.peak
}

func poolTask(root, id string) ChildTask {
	task := childTask(root)
	task.TaskID = id
	return task
}

func TestChildRunnerPoolRunsRealChildrenConcurrently(t *testing.T) {
	provider := &poolBarrierProvider{started: make(chan struct{}, 2), release: make(chan struct{})}
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewChildRunnerPool(app, 2)
	if err != nil {
		t.Fatal(err)
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	plan := OrchestrationPlan{
		OrchestrationID: "orch-pool",
		ParentID:        "parent-1",
		MaxConcurrent:   2,
		Tasks: []PlannedChild{
			{Task: poolTask(rootA, "a")},
			{Task: poolTask(rootB, "b")},
		},
	}
	resultCh := make(chan OrchestrationResult, 1)
	go func() {
		result, _ := mustRunOrchestration(pool, plan)
		resultCh <- result
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-provider.started:
		case <-time.After(2 * time.Second):
			t.Fatal("real child did not reach provider barrier")
		}
	}
	close(provider.release)
	select {
	case result := <-resultCh:
		if result.Status != OrchestrationSucceeded {
			t.Fatalf("result=%#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("orchestration did not complete")
	}
	if provider.Peak() != 2 {
		t.Fatalf("peak provider concurrency=%d, want 2", provider.Peak())
	}
}

func TestChildRunnerPoolCapacityIsBoundedAndReusable(t *testing.T) {
	provider := &poolBarrierProvider{started: make(chan struct{}, 3), release: make(chan struct{})}
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewChildRunnerPool(app, 2)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		root := t.TempDir()
		id := string(rune('a' + i))
		go func() {
			_, runErr := pool.Run(context.Background(), poolTask(root, id))
			results <- runErr
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-provider.started:
		case <-time.After(2 * time.Second):
			t.Fatal("pool did not start two children")
		}
	}
	select {
	case <-provider.started:
		t.Fatal("pool exceeded capacity")
	case <-time.After(50 * time.Millisecond):
	}
	close(provider.release)
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if provider.Peak() != 2 {
		t.Fatalf("peak provider concurrency=%d, want 2", provider.Peak())
	}
}

func TestChildRunnerPoolAcquireHonorsCancellation(t *testing.T) {
	provider := &poolBarrierProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewChildRunnerPool(app, 1)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := pool.Run(context.Background(), poolTask(t.TempDir(), "first"))
		firstDone <- runErr
	}()
	<-provider.started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := pool.Run(ctx, poolTask(t.TempDir(), "second")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire err=%v", err)
	}
	close(provider.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Run(context.Background(), poolTask(t.TempDir(), "third")); err != nil {
		t.Fatalf("reused slot err=%v", err)
	}
}

func mustRunOrchestration(executor ChildExecutor, plan OrchestrationPlan) (OrchestrationResult, error) {
	orchestrator, err := NewParallelOrchestrator(executor, nil)
	if err != nil {
		return OrchestrationResult{}, err
	}
	return orchestrator.Run(context.Background(), plan)
}
