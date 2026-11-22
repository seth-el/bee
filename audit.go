package bee

import (
	"context"
	"errors"
)

// sessionAuditor persists denied authorization decisions and the bounded
// outcome of every auto-approved execution in the owning session. Ordinary
// successful execution records remain the responsibility of the
// caller-provided Auditor, preserving manual-mode behavior.
type sessionAuditor struct {
	session *Session
}

func (auditor sessionAuditor) Record(_ context.Context, event AuditEvent) error {
	if auditor.session == nil || (event.Outcome == AuditExecuted && !event.AutoApproved) {
		return nil
	}
	if event.AutoApproved {
		// The durable auto-approval journal is metadata-only. The target has
		// already been allowlisted and bounded by Executor; omit arguments and
		// tool-provided error text so content cannot be copied into the log.
		event.Args = nil
		if event.Error != "" {
			event.Error = "tool execution failed"
		}
	}
	return auditor.session.AppendAudit(event)
}

// compositeAuditor preserves existing live audit behavior while adding the
// session-backed denial record. Every configured sink is attempted; a failure
// is returned so the executor remains fail-closed.
type compositeAuditor []Auditor

func (auditor compositeAuditor) Record(ctx context.Context, event AuditEvent) error {
	var errs []error
	for _, sink := range auditor {
		if sink == nil {
			continue
		}
		if err := sink.Record(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func withSessionAuditor(executor *Executor, session *Session) {
	if executor == nil || session == nil {
		return
	}
	sinks := make(compositeAuditor, 0, 2)
	if executor.Auditor != nil {
		sinks = append(sinks, executor.Auditor)
	}
	sinks = append(sinks, sessionAuditor{session: session})
	executor.Auditor = sinks
}
