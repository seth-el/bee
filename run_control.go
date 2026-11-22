package bee

import "context"

func (app *Application) registerActiveRun(runID string, cancel context.CancelFunc) {
	if app == nil || runID == "" || cancel == nil {
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.activeRuns == nil {
		app.activeRuns = make(map[string]context.CancelFunc)
	}
	app.activeRuns[runID] = cancel
}

func (app *Application) unregisterActiveRun(runID string) {
	if app == nil || runID == "" {
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	delete(app.activeRuns, runID)
}

// CancelActiveRuns cancels every currently active top-level run and returns
// the number of run contexts cancelled. It does not kill unrelated processes;
// child/process runners observe the same parent cancellation and perform their
// existing process-group cleanup.
func (app *Application) CancelActiveRuns() int {
	if app == nil {
		return 0
	}
	app.mu.Lock()
	active := app.activeRuns
	app.activeRuns = make(map[string]context.CancelFunc)
	app.mu.Unlock()
	count := 0
	for _, cancel := range active {
		if cancel != nil {
			cancel()
			count++
		}
	}
	return count
}
