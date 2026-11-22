package bee

const maxCommandStatusTelemetrySessions = 64

// commandStatusTelemetry is a bounded, content-free snapshot used by /status.
// It deliberately retains only counts and estimates already safe for run
// events; prompts, messages, tool arguments, and tool output never enter it.
type commandStatusTelemetry struct {
	sequence             uint64
	runID                string
	runUsage             Usage
	runProviderCalls     int
	recentUsage          Usage
	recentProviderCalls  int
	lastTiming           *ProviderCallTiming
	contextUsage         RequestContextUsage
	contextUsageSeen     bool
	contextDiagnostics   ContextDiagnostics
	contextSeen          bool
	memoryExtraction     MemoryExtractionStatus
	memoryExtractionSeen bool
}

func (app *Application) observeCommandStatusEvent(event RunEvent) {
	if app == nil {
		return
	}
	switch event.Type {
	case RunStart, RunUsage, RunContextUsage, RunContextDiagnostics, RunMemoryExtraction:
	default:
		return
	}

	app.mu.Lock()
	defer app.mu.Unlock()
	if app.statusTelemetry == nil {
		app.statusTelemetry = make(map[string]*commandStatusTelemetry)
	}
	key := string(event.SessionID)
	snapshot := app.statusTelemetry[key]
	if snapshot == nil {
		if len(app.statusTelemetry) >= maxCommandStatusTelemetrySessions {
			app.evictOldestCommandStatusTelemetryLocked()
		}
		snapshot = &commandStatusTelemetry{}
		app.statusTelemetry[key] = snapshot
	}
	app.statusTelemetrySeq++
	snapshot.sequence = app.statusTelemetrySeq
	if event.Type == RunStart {
		snapshot.runID = event.RunID
		snapshot.runUsage = Usage{}
		snapshot.runProviderCalls = 0
	} else if event.RunID != "" && snapshot.runID != "" && snapshot.runID != event.RunID {
		// A newer run for this session already owns the displayed snapshot.
		// Ignore late events from an older concurrent run instead of merging
		// unrelated usage and context figures.
		return
	} else if snapshot.runID == "" {
		snapshot.runID = event.RunID
	}

	switch event.Type {
	case RunUsage:
		if event.Usage == nil {
			return
		}
		addCommandStatusUsage(&snapshot.runUsage, event.Usage.Usage)
		snapshot.runProviderCalls++
		snapshot.recentUsage = snapshot.runUsage
		snapshot.recentProviderCalls = snapshot.runProviderCalls
		if event.Usage.Timing != nil {
			timing := *event.Usage.Timing
			snapshot.lastTiming = &timing
		}
	case RunContextUsage:
		if event.ContextUsage == nil {
			return
		}
		snapshot.contextUsage = *event.ContextUsage
		snapshot.contextUsageSeen = true
	case RunContextDiagnostics:
		if event.ContextDiagnostics == nil {
			return
		}
		snapshot.contextDiagnostics = boundedCommandContextDiagnostics(*event.ContextDiagnostics)
		snapshot.contextSeen = true
	case RunMemoryExtraction:
		if event.MemoryExtraction == nil {
			return
		}
		snapshot.memoryExtraction = MemoryExtractionStatus{
			Attempted: event.MemoryExtraction.Attempted,
			Saved:     len(event.MemoryExtraction.Saved),
			Unchanged: len(event.MemoryExtraction.Unchanged),
			Proposed:  len(event.MemoryExtraction.Proposed),
			Rejected:  event.MemoryExtraction.Rejected,
			Error:     truncateText(event.MemoryExtraction.Error, 256),
		}
		snapshot.memoryExtractionSeen = true
	}
}

func (app *Application) commandStatusTelemetry(sessionID SessionID) commandStatusTelemetry {
	if app == nil {
		return commandStatusTelemetry{}
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	snapshot := app.statusTelemetry[string(sessionID)]
	if snapshot == nil {
		return commandStatusTelemetry{}
	}
	copy := *snapshot
	copy.contextDiagnostics.Sources = append([]ContextSourceDiagnostics(nil), snapshot.contextDiagnostics.Sources...)
	return copy
}

func (app *Application) evictOldestCommandStatusTelemetryLocked() {
	oldestKey := ""
	var oldestSequence uint64
	first := true
	for key, snapshot := range app.statusTelemetry {
		if snapshot == nil || first || snapshot.sequence < oldestSequence {
			oldestKey = key
			if snapshot != nil {
				oldestSequence = snapshot.sequence
			}
			first = false
		}
	}
	if !first {
		delete(app.statusTelemetry, oldestKey)
	}
}

func addCommandStatusUsage(total *Usage, addition Usage) {
	total.PromptTokens += addition.PromptTokens
	total.CompletionTokens += addition.CompletionTokens
	total.TotalTokens += addition.TotalTokens
	total.CacheReadTokens += addition.CacheReadTokens
	total.CacheWriteTokens += addition.CacheWriteTokens
}

func boundedCommandContextDiagnostics(diagnostics ContextDiagnostics) ContextDiagnostics {
	if len(diagnostics.Sources) > MaxContextStatusSources {
		diagnostics.Sources = append([]ContextSourceDiagnostics(nil), diagnostics.Sources[:MaxContextStatusSources]...)
	} else {
		diagnostics.Sources = append([]ContextSourceDiagnostics(nil), diagnostics.Sources...)
	}
	return diagnostics
}
