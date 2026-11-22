package bee

import (
	"context"
	"strings"
	"sync"
	"time"
)

// RuntimeMetricsSnapshot is a bounded, content-free view of harness activity.
// It contains no prompts, tool arguments, provider output, or filesystem paths.
type RuntimeMetricsSnapshot struct {
	RunsStarted       uint64 `json:"runs_started"`
	RunsSucceeded     uint64 `json:"runs_succeeded"`
	RunsFailed        uint64 `json:"runs_failed"`
	RunsCanceled      uint64 `json:"runs_canceled"`
	ActiveRuns        int    `json:"active_runs"`
	SessionBusy       uint64 `json:"session_busy"`
	AdmissionRejected uint64 `json:"admission_rejected"`
}

// RuntimeMetrics owns the small amount of state needed to correlate run start
// and terminal events. Finished run IDs are removed immediately.
type RuntimeMetrics struct {
	mu                sync.Mutex
	startedAt         map[string]time.Time
	runsStarted       uint64
	runsSucceeded     uint64
	runsFailed        uint64
	runsCanceled      uint64
	activeRuns        int
	sessionBusy       uint64
	admissionRejected uint64
}

func NewRuntimeMetrics() *RuntimeMetrics {
	return &RuntimeMetrics{startedAt: make(map[string]time.Time)}
}

func (metrics *RuntimeMetrics) Snapshot() RuntimeMetricsSnapshot {
	if metrics == nil {
		return RuntimeMetricsSnapshot{}
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	return RuntimeMetricsSnapshot{RunsStarted: metrics.runsStarted, RunsSucceeded: metrics.runsSucceeded, RunsFailed: metrics.runsFailed, RunsCanceled: metrics.runsCanceled, ActiveRuns: metrics.activeRuns, SessionBusy: metrics.sessionBusy, AdmissionRejected: metrics.admissionRejected}
}

func (metrics *RuntimeMetrics) recordSessionBusy() {
	if metrics == nil {
		return
	}
	metrics.mu.Lock()
	metrics.sessionBusy++
	metrics.mu.Unlock()
}

func (metrics *RuntimeMetrics) RecordAdmissionRejected() {
	if metrics == nil {
		return
	}
	metrics.mu.Lock()
	metrics.admissionRejected++
	metrics.mu.Unlock()
}

func (metrics *RuntimeMetrics) observe(event RunEvent) (Diagnostic, bool) {
	if metrics == nil || event.RunID == "" {
		return Diagnostic{}, false
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	switch event.Type {
	case RunStart:
		if metrics.startedAt == nil {
			metrics.startedAt = make(map[string]time.Time)
		}
		if _, exists := metrics.startedAt[event.RunID]; exists {
			return Diagnostic{}, false
		}
		started := event.At
		if started.IsZero() {
			started = time.Now().UTC()
		}
		metrics.startedAt[event.RunID] = started
		metrics.runsStarted++
		metrics.activeRuns++
		return runtimeDiagnostic(event, DiagnosticStarted, "", 0), true
	case RunEnd:
		started, exists := metrics.startedAt[event.RunID]
		if !exists {
			return Diagnostic{}, false
		}
		delete(metrics.startedAt, event.RunID)
		if metrics.activeRuns > 0 {
			metrics.activeRuns--
		}
		status, code := runtimeTerminalStatus(event.Error)
		switch status {
		case DiagnosticSucceeded:
			metrics.runsSucceeded++
		case DiagnosticCanceled:
			metrics.runsCanceled++
		default:
			metrics.runsFailed++
		}
		finished := event.At
		if finished.IsZero() {
			finished = time.Now().UTC()
		}
		duration := finished.Sub(started)
		if duration < 0 {
			duration = 0
		}
		return runtimeDiagnostic(event, status, code, duration.Milliseconds()), true
	default:
		return Diagnostic{}, false
	}
}

func runtimeTerminalStatus(message string) (DiagnosticStatus, string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return DiagnosticSucceeded, ""
	}
	if trimmed == context.Canceled.Error() || strings.HasSuffix(trimmed, ": "+context.Canceled.Error()) {
		return DiagnosticCanceled, "context_canceled"
	}
	if trimmed == context.DeadlineExceeded.Error() || strings.HasSuffix(trimmed, ": "+context.DeadlineExceeded.Error()) {
		return DiagnosticFailed, "deadline_exceeded"
	}
	return DiagnosticFailed, "run_failed"
}

func runtimeDiagnostic(event RunEvent, status DiagnosticStatus, errorCode string, durationMS int64) Diagnostic {
	fields := map[string]string{"run_id": event.RunID}
	if event.SessionID != "" {
		fields["session_id"] = string(event.SessionID)
	}
	if event.Profile != "" {
		fields["profile"] = event.Profile
	}
	if event.Role != "" {
		fields["role"] = string(event.Role)
	}
	if event.Model != "" {
		fields["model"] = event.Model
	}
	level := DiagnosticInfo
	if status == DiagnosticCanceled {
		level = DiagnosticWarn
	} else if status == DiagnosticFailed {
		level = DiagnosticError
	}
	return Diagnostic{Time: event.At, Level: level, Component: "runtime", Operation: "run", Status: status, ErrorCode: errorCode, DurationMS: durationMS, Fields: fields}
}

func (app *Application) observeRuntimeEvent(event RunEvent) {
	if app == nil {
		return
	}
	diagnostic, emit := app.Metrics.observe(event)
	if !emit || app.Diagnostics == nil {
		return
	}
	_ = app.Diagnostics.Emit(context.Background(), diagnostic)
}

func (app *Application) recordSessionBusy(sessionID SessionID) {
	if app == nil {
		return
	}
	app.Metrics.recordSessionBusy()
	if app.Diagnostics != nil {
		fields := map[string]string{}
		if sessionID != "" {
			fields["session_id"] = string(sessionID)
		}
		_ = app.Diagnostics.Emit(context.Background(), Diagnostic{Level: DiagnosticWarn, Component: "runtime", Operation: "run_admission", Status: DiagnosticSkipped, ErrorCode: "session_busy", Fields: fields})
	}
}

// RecordAdmissionRejected records a content-free gateway capacity rejection.
// It is exported so transport packages can use the application's single
// observability stream without exposing prompts or request bodies.
func (app *Application) RecordAdmissionRejected() {
	if app == nil {
		return
	}
	app.Metrics.RecordAdmissionRejected()
	if app.Diagnostics != nil {
		_ = app.Diagnostics.Emit(context.Background(), Diagnostic{Level: DiagnosticWarn, Component: "runtime", Operation: "run_admission", Status: DiagnosticSkipped, ErrorCode: "gateway_capacity"})
	}
}
