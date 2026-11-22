package bee

import (
	"context"
	"errors"
	"fmt"
)

// ChildRunnerPool is a parent-owned bounded set of independent runner slots.
// Each slot remains single-flight; concurrency comes from using separate
// runners, not from making one runner's mutable lifecycle state reentrant.
type ChildRunnerPool struct {
	slots chan ChildObservableExecutor
}

func NewChildRunnerPool(app *Application, capacity int) (*ChildRunnerPool, error) {
	if capacity <= 0 {
		return nil, errors.New("child runner pool capacity must be positive")
	}
	if app != nil && app.ChildMax > 0 && capacity > app.ChildMax {
		return nil, fmt.Errorf("child runner pool capacity %d exceeds configured maximum %d", capacity, app.ChildMax)
	}
	if app == nil || app.Provider == nil {
		return nil, errors.New("child runner pool application provider is required")
	}
	slots := make(chan ChildObservableExecutor, capacity)
	for i := 0; i < capacity; i++ {
		runner, err := NewChildRunner(app)
		if err != nil {
			return nil, err
		}
		slots <- runner
	}
	return &ChildRunnerPool{slots: slots}, nil
}

func NewProcessChildRunnerPool(app *Application, config ChildProcessConfig, capacity int) (*ChildRunnerPool, error) {
	if capacity <= 0 {
		return nil, errors.New("child runner pool capacity must be positive")
	}
	if app != nil && app.ChildMax > 0 && capacity > app.ChildMax {
		return nil, fmt.Errorf("child runner pool capacity %d exceeds configured maximum %d", capacity, app.ChildMax)
	}
	if app == nil || app.Provider == nil {
		return nil, errors.New("process child runner pool application provider is required")
	}
	slots := make(chan ChildObservableExecutor, capacity)
	for i := 0; i < capacity; i++ {
		runner, err := NewProcessChildRunner(app, config)
		if err != nil {
			return nil, err
		}
		slots <- runner
	}
	return &ChildRunnerPool{slots: slots}, nil
}

func (pool *ChildRunnerPool) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return pool.RunObserved(ctx, task, nil)
}

func (pool *ChildRunnerPool) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	packet := ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID}
	if pool == nil || pool.slots == nil {
		return packet, errors.New("child runner pool is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var slot ChildObservableExecutor
	select {
	case slot = <-pool.slots:
	case <-ctx.Done():
		return packet, ctx.Err()
	}
	defer func() { pool.slots <- slot }()
	return slot.RunObserved(ctx, task, observer)
}

var _ ChildObservableExecutor = (*ChildRunnerPool)(nil)
