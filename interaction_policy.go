package bee

import (
	"sync"
)

// ApprovalStatus is the owner-facing lifecycle state of one policy-required
// decision. Pending is transient; terminal states are emitted once and may be
// persisted with the durable session.
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "pending"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusDenied   ApprovalStatus = "denied"
	ApprovalStatusError    ApprovalStatus = "error"
)

func approvalStatus(decision ApprovalDecision, reason string) ApprovalStatus {
	if reason != "" {
		return ApprovalStatusError
	}
	if decision.Approves() {
		return ApprovalStatusApproved
	}
	if decision == ApprovalDenied {
		return ApprovalStatusDenied
	}
	return ApprovalStatusPending
}

func (approval ApprovalEvent) EffectiveStatus() ApprovalStatus {
	switch approval.Status {
	case ApprovalStatusPending, ApprovalStatusApproved, ApprovalStatusDenied, ApprovalStatusError:
		return approval.Status
	default:
		return approvalStatus(approval.Decision, approval.Reason)
	}
}

func terminalApprovalStatus(status ApprovalStatus) bool {
	return status == ApprovalStatusApproved || status == ApprovalStatusDenied || status == ApprovalStatusError
}

// InteractionPolicyState is the bounded owner-facing policy snapshot supplied
// to adapters at run start and through the HTTP policy endpoint.
type InteractionPolicyState struct {
	Display           DisplayVisibility `json:"display"`
	PlanMode          PlanModeState     `json:"plan_mode"`
	PlanModeAvailable bool              `json:"plan_mode_available"`
}

func (app *Application) InteractionPolicy(id SessionID) InteractionPolicyState {
	state := InteractionPolicyState{Display: app.DisplayVisibility()}
	if app == nil {
		return state
	}
	state.PlanModeAvailable = app.supportsPlanMode()
	plan, err := app.PlanMode(id)
	if err != nil {
		plan = PlanModeState{Enabled: true, Source: "invalid session state; fail closed", SessionID: id}
	}
	state.PlanMode = plan
	return state
}

// RunEventDisplayProjector is a per-run, stateful display boundary. Raw run
// events and durable session data stay faithful; owner adapters retain or send
// only the detached projection returned here.
type RunEventDisplayProjector struct {
	app *Application

	mu               sync.Mutex
	approvalStatuses map[string]ApprovalStatus
}

func (app *Application) NewRunEventDisplayProjector() *RunEventDisplayProjector {
	return &RunEventDisplayProjector{app: app, approvalStatuses: make(map[string]ApprovalStatus)}
}

func (projector *RunEventDisplayProjector) Project(event RunEvent) (RunEvent, bool) {
	if projector == nil {
		return event, true
	}
	projector.mu.Lock()
	defer projector.mu.Unlock()

	visibility := defaultDisplayVisibility()
	if projector.app != nil {
		visibility = projector.app.DisplayVisibility()
	}
	projected := event
	switch event.Type {
	case RunReasoningDelta, RunReasoningFinal:
		if !visibility.Thinking {
			return RunEvent{}, false
		}
	case RunToolCall:
		mode := visibility.toolDisplayMode()
		if mode == ToolDisplayOff {
			return RunEvent{}, false
		}
		if event.ToolCall != nil {
			call := ProjectToolCallForDisplayWithVisibility(*event.ToolCall, visibility)
			projected.ToolCall = &call
		}
	case RunToolResult:
		mode := visibility.toolDisplayMode()
		if mode == ToolDisplayOff {
			return RunEvent{}, false
		}
		if event.ToolResult != nil {
			result := ProjectToolResultForDisplayWithVisibility(*event.ToolResult, visibility)
			projected.ToolResult = &result
		}
	case RunApproval:
		if event.Approval == nil {
			return projected, true
		}
		approval := *event.Approval
		approval.Request = ProjectApprovalRequestForDisplay(event.Approval.Request)
		if approval.ID == "" {
			approval.ID = approval.Request.ID
		}
		approval.Status = approval.EffectiveStatus()
		if approval.ID != "" {
			if previous, exists := projector.approvalStatuses[approval.ID]; exists {
				if previous == approval.Status || terminalApprovalStatus(previous) || (previous == ApprovalStatusPending && approval.Status == ApprovalStatusPending) {
					return RunEvent{}, false
				}
			}
			projector.approvalStatuses[approval.ID] = approval.Status
		}
		projected.Approval = &approval
	}
	return projected, true
}
