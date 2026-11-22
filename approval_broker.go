package bee

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const MaxPendingApprovals = 128

// PendingApproval is the bounded, interface-neutral prompt exposed to owner
// adapters. RunID and SessionID are associations, not authorization scopes;
// decisions remain addressed by the opaque approval ID.
type PendingApproval struct {
	ID        string          `json:"id"`
	Request   ApprovalRequest `json:"request"`
	Status    ApprovalStatus  `json:"status"`
	RunID     string          `json:"run_id,omitempty"`
	SessionID SessionID       `json:"session_id,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	decision  chan ApprovalDecision
}

// ApprovalBroker coordinates policy-required owner decisions without owning
// any terminal or HTTP presentation.
type ApprovalBroker struct {
	mu      sync.Mutex
	pending map[string]*PendingApproval
}

func NewApprovalBroker() *ApprovalBroker {
	return &ApprovalBroker{pending: make(map[string]*PendingApproval)}
}

func (broker *ApprovalBroker) Approve(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	if broker == nil {
		return ApprovalDenied, errors.New("approval broker is unavailable")
	}
	if ctx == nil {
		return ApprovalDenied, errors.New("approval context is required")
	}
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		request.ID = "approval-" + newStableID()
	}
	runID, _ := RunIDFromContext(ctx)
	sessionID, _ := SessionIDFromContext(ctx)
	pending := &PendingApproval{
		ID:        request.ID,
		Request:   cloneApprovalRequest(request),
		Status:    ApprovalStatusPending,
		RunID:     runID,
		SessionID: sessionID,
		CreatedAt: time.Now().UTC(),
		decision:  make(chan ApprovalDecision, 1),
	}
	broker.mu.Lock()
	if len(broker.pending) >= MaxPendingApprovals {
		broker.mu.Unlock()
		return ApprovalDenied, errors.New("approval broker capacity is full")
	}
	if _, exists := broker.pending[pending.ID]; exists {
		broker.mu.Unlock()
		return ApprovalDenied, errors.New("approval request ID is already pending")
	}
	broker.pending[pending.ID] = pending
	broker.mu.Unlock()

	emitPendingApproval(ctx, *pending)
	defer func() {
		broker.mu.Lock()
		delete(broker.pending, pending.ID)
		broker.mu.Unlock()
	}()
	select {
	case decision := <-pending.decision:
		return decision, nil
	case <-ctx.Done():
		return ApprovalDenied, ctx.Err()
	}
}

func (broker *ApprovalBroker) Pending() []PendingApproval {
	if broker == nil {
		return nil
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	items := make([]PendingApproval, 0, len(broker.pending))
	for _, item := range broker.pending {
		items = append(items, PendingApproval{
			ID:        item.ID,
			Request:   ProjectApprovalRequestForDisplay(item.Request),
			Status:    ApprovalStatusPending,
			RunID:     item.RunID,
			SessionID: item.SessionID,
			CreatedAt: item.CreatedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items
}

func (broker *ApprovalBroker) Decide(id string, decision ApprovalDecision) error {
	if broker == nil {
		return errors.New("approval broker is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		return errors.New("approval request not found")
	}
	if !decision.ValidOwnerDecision() {
		return errors.New("decision must be approved, approved_session, or denied")
	}
	broker.mu.Lock()
	pending := broker.pending[id]
	broker.mu.Unlock()
	if pending == nil {
		return errors.New("approval request not found")
	}
	select {
	case pending.decision <- decision:
		return nil
	default:
		return errors.New("approval request was already decided")
	}
}

func cloneApprovalRequest(request ApprovalRequest) ApprovalRequest {
	request.Args = append([]byte(nil), request.Args...)
	return request
}

type pendingApprovalEmitterKey struct{}

func withPendingApprovalEmitter(ctx context.Context, emit func(PendingApproval)) context.Context {
	return context.WithValue(ctx, pendingApprovalEmitterKey{}, emit)
}

func emitPendingApproval(ctx context.Context, pending PendingApproval) {
	emit, _ := ctx.Value(pendingApprovalEmitterKey{}).(func(PendingApproval))
	if emit != nil {
		emit(pending)
	}
}

// EnableApprovalBroker installs Application's shared owner-decision
// coordinator on the foreground executor. Locked/model-only compositions have
// no executor and therefore no approval surface.
func (app *Application) EnableApprovalBroker() error {
	if app == nil {
		return errors.New("application is required")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.Executor == nil {
		return errors.New("application executor is not configured")
	}
	if app.approvalBroker == nil {
		app.approvalBroker = NewApprovalBroker()
	}
	app.Executor.Approval = app.approvalBroker
	return nil
}

func (app *Application) PendingApprovals() []PendingApproval {
	if app == nil {
		return nil
	}
	app.mu.Lock()
	broker := app.approvalBroker
	app.mu.Unlock()
	return broker.Pending()
}

func (app *Application) DecideApproval(id string, decision ApprovalDecision) error {
	if app == nil {
		return errors.New("application is required")
	}
	app.mu.Lock()
	broker := app.approvalBroker
	app.mu.Unlock()
	return broker.Decide(id, decision)
}

var _ ApprovalHook = (*ApprovalBroker)(nil)
