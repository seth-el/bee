package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	planModeReasonEnabled     = "plan mode enabled"
	planModeReasonDisabled    = "plan mode disabled"
	planModeReasonInitialized = "plan mode initialized from configuration"
)

const (
	planControlMessagePrefix = "<bee-control>"
	planStateMessagePrefix   = "<bee-plan-state>"
)

type planControlRecord struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func planModeControlMessage(enabled bool) Message {
	value := enabled
	data, _ := json.Marshal(planControlRecord{Version: 1, Kind: "plan_mode", Enabled: &value})
	return Message{Role: RoleSystem, Content: planControlMessagePrefix + string(data) + "</bee-control>"}
}

func planResetControlMessage() Message {
	data, _ := json.Marshal(planControlRecord{Version: 1, Kind: "plan_reset"})
	return Message{Role: RoleSystem, Content: planControlMessagePrefix + string(data) + "</bee-control>"}
}

func planStateControlMessage(plan TaskPlan, sequence uint64) Message {
	payload := struct {
		Version     int      `json:"version"`
		Plan        TaskPlan `json:"plan"`
		UpdatedSeq  uint64   `json:"updated_seq,omitempty"`
		Fingerprint string   `json:"fingerprint"`
	}{Version: 1, Plan: providerSafePlan(plan), UpdatedSeq: sequence, Fingerprint: taskPlanFingerprint(plan)}
	data, _ := json.Marshal(payload)
	return Message{Role: RoleSystem, Content: planStateMessagePrefix + string(data) + "</bee-plan-state>"}
}

func (agent *Agent) planToolsExposed() bool {
	return agent != nil && (agent.requirePlan || agent.exposePlanTracking)
}

func hasPlanModeControlMessage(messages []Message) bool {
	for _, message := range messages {
		if message.Role == RoleSystem && strings.HasPrefix(message.Content, planControlMessagePrefix) && strings.Contains(message.Content, `"kind":"plan_mode"`) {
			return true
		}
	}
	return false
}

func hasCanonicalPlanState(messages []Message, fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	for _, message := range messages {
		trustedPlanState := message.Role == RoleSystem && strings.HasPrefix(message.Content, planStateMessagePrefix)
		trustedPlanTool := message.Role == RoleTool && (message.Name == PlanToolName || message.Name == PlanStatusToolName || message.Name == PlanUpdateToolName)
		if (trustedPlanState || trustedPlanTool) && strings.Contains(message.Content, fingerprint) {
			return true
		}
	}
	return false
}

func (agent *Agent) appendPlanStateMessage(message Message) error {
	if agent.session != nil {
		if err := agent.session.AppendMessage(message); err != nil {
			return err
		}
	}
	agent.messages = append(agent.messages, message)
	return nil
}

func (agent *Agent) appendCurrentPlanCheckpoint() error {
	if !agent.planToolsExposed() {
		return nil
	}
	if err := agent.appendPlanStateMessage(planModeControlMessage(agent.requirePlan)); err != nil {
		return fmt.Errorf("append plan mode checkpoint: %w", err)
	}
	if agent.plan == nil {
		return nil
	}
	_, sequence, err := agent.currentPlanState()
	if err != nil {
		return err
	}
	if err := agent.appendPlanStateMessage(planStateControlMessage(*agent.plan, sequence)); err != nil {
		return fmt.Errorf("append active plan checkpoint: %w", err)
	}
	return nil
}

// ensureAppendOnlyPlanState migrates legacy or compacted transcripts by
// appending the minimum trusted state that is not already visible. It never
// rewrites or prepends provider-visible history.
func (agent *Agent) ensureAppendOnlyPlanState() error {
	if !agent.planToolsExposed() {
		return nil
	}
	if !hasPlanModeControlMessage(agent.messages) {
		return agent.appendCurrentPlanCheckpoint()
	}
	if agent.plan == nil {
		return nil
	}
	fingerprint := taskPlanFingerprint(*agent.plan)
	if hasCanonicalPlanState(agent.messages, fingerprint) {
		return nil
	}
	_, sequence, err := agent.currentPlanState()
	if err != nil {
		return err
	}
	if err := agent.appendPlanStateMessage(planStateControlMessage(*agent.plan, sequence)); err != nil {
		return fmt.Errorf("append active plan checkpoint: %w", err)
	}
	return nil
}

// PlanModeState is the effective planning policy for a session or for the
// next session when SessionID is empty.
type PlanModeState struct {
	Enabled            bool      `json:"enabled"`
	Source             string    `json:"source"`
	SessionID          SessionID `json:"session_id,omitempty"`
	ActivePlan         bool      `json:"active_plan"`
	PlanID             string    `json:"plan_id,omitempty"`
	PlanGoal           string    `json:"plan_goal,omitempty"`
	PlanUpdatedSeq     uint64    `json:"plan_updated_seq,omitempty"`
	PlanFingerprint    string    `json:"plan_fingerprint,omitempty"`
	CriteriaPending    int       `json:"criteria_pending,omitempty"`
	CriteriaInProgress int       `json:"criteria_in_progress,omitempty"`
	CriteriaPassed     int       `json:"criteria_passed,omitempty"`
	CriteriaFailed     int       `json:"criteria_failed,omitempty"`
	CriteriaBlocked    int       `json:"criteria_blocked,omitempty"`
}

func latestSessionPlanMode(events []Event, defaultEnabled bool) (enabled bool, explicit bool, err error) {
	enabled = defaultEnabled
	for _, event := range events {
		if event.Type != EventPlanMode {
			continue
		}
		payload, decodeErr := decodeEventPayload[PlanModeEvent](event, EventPlanMode)
		if decodeErr != nil {
			return true, true, fmt.Errorf("load session plan mode: %w", decodeErr)
		}
		enabled = payload.Enabled
		explicit = true
	}
	return enabled, explicit, nil
}

func (app *Application) supportsPlanMode() bool {
	return app != nil && app.ProviderRole != ProviderRoleChild && app.Capabilities.Policy != AssistantPolicyLocked
}

// PlanMode reports the persisted session mode, a queued mode for the next
// durable session, or the configured process default in that order.
func (app *Application) PlanMode(id SessionID) (PlanModeState, error) {
	if app == nil {
		return PlanModeState{}, errors.New("application is required")
	}
	app.mu.Lock()
	var pending *bool
	if app.pendingPlanMode != nil {
		value := *app.pendingPlanMode
		pending = &value
	}
	var process *bool
	if app.processPlanMode != nil {
		value := *app.processPlanMode
		process = &value
	}
	app.mu.Unlock()
	if strings.TrimSpace(string(id)) == "" {
		if app.Store == nil && process != nil {
			return PlanModeState{Enabled: *process, Source: "process"}, nil
		}
		if pending != nil {
			return PlanModeState{Enabled: *pending, Source: "next session"}, nil
		}
		return PlanModeState{Enabled: app.RequirePlan, Source: "configuration"}, nil
	}
	if app.Store == nil {
		return PlanModeState{}, errors.New("session store is not configured")
	}
	if pending != nil {
		return PlanModeState{Enabled: *pending, Source: "pending override", SessionID: id}, nil
	}
	session, err := app.Store.Load(id)
	if err != nil {
		return PlanModeState{}, err
	}
	enabled, explicit, err := latestSessionPlanMode(session.EventsSnapshot(), app.RequirePlan)
	if err != nil {
		return PlanModeState{}, err
	}
	source := "configuration"
	if explicit {
		source = "session"
	}
	state := PlanModeState{Enabled: enabled, Source: source, SessionID: id}
	plan, sequence, planErr := latestSessionPlanState(session.EventsSnapshot())
	if planErr != nil {
		return PlanModeState{}, planErr
	}
	populatePlanModeState(&state, plan, sequence)
	return state, nil
}

func populatePlanModeState(state *PlanModeState, plan *TaskPlan, sequence uint64) {
	if state == nil || plan == nil {
		return
	}
	state.ActivePlan = true
	state.PlanID = plan.ID
	state.PlanGoal = truncateUTF8(strings.TrimSpace(plan.Goal), 512)
	state.PlanUpdatedSeq = sequence
	state.PlanFingerprint = taskPlanFingerprint(*plan)
	for _, criterion := range plan.Criteria {
		switch criterion.State {
		case AcceptancePending:
			state.CriteriaPending++
		case AcceptanceInProgress:
			state.CriteriaInProgress++
		case AcceptancePassed:
			state.CriteriaPassed++
		case AcceptanceFailed:
			state.CriteriaFailed++
		case AcceptanceBlocked:
			state.CriteriaBlocked++
		}
	}
}

// SetPlanMode changes a durable session immediately. With an empty session ID,
// it queues the mode for the next durable session or changes a store-less
// process-local conversation.
func (app *Application) SetPlanMode(id SessionID, enabled bool) (bool, error) {
	if app == nil {
		return false, errors.New("application is required")
	}
	if enabled && !app.supportsPlanMode() {
		return false, errors.New("plan mode is unavailable for this assistant composition")
	}
	if strings.TrimSpace(string(id)) == "" {
		if app.Store != nil {
			app.mu.Lock()
			current := app.RequirePlan
			if app.pendingPlanMode != nil {
				current = *app.pendingPlanMode
			}
			value := enabled
			app.pendingPlanMode = &value
			app.mu.Unlock()
			return current != enabled, nil
		}
		app.mu.Lock()
		current := app.RequirePlan
		if app.processPlanMode != nil {
			current = *app.processPlanMode
		}
		value := enabled
		app.processPlanMode = &value
		agent := app.processAgent
		app.mu.Unlock()
		if agent != nil {
			agent.SetRequirePlan(enabled)
		}
		return current != enabled, nil
	}
	if app.Store == nil {
		return false, errors.New("session store is not configured")
	}
	session, err := app.Store.Load(id)
	if err != nil {
		return false, err
	}
	current, explicit, err := latestSessionPlanMode(session.EventsSnapshot(), app.RequirePlan)
	if err != nil {
		return false, err
	}
	changed := current != enabled
	if changed && enabled {
		err = session.EnablePlanMode(planModeReasonEnabled)
	} else if changed || !explicit {
		reason := planModeReasonDisabled
		if enabled {
			reason = planModeReasonInitialized
		}
		err = session.AppendPlanMode(enabled, reason)
	}
	if err != nil {
		return false, err
	}
	app.mu.Lock()
	app.pendingPlanMode = nil
	app.mu.Unlock()
	return changed, nil
}

// prepareSessionPlanMode freezes the config default into legacy/new sessions
// and applies a queued command-line or pre-session override exactly once.
func (app *Application) prepareSessionPlanMode(session *Session) (bool, error) {
	if app == nil {
		return false, errors.New("application is required")
	}
	if session == nil {
		app.mu.Lock()
		enabled := app.RequirePlan
		if app.processPlanMode != nil {
			enabled = *app.processPlanMode
		}
		app.mu.Unlock()
		return enabled, nil
	}
	app.mu.Lock()
	var pending *bool
	if app.pendingPlanMode != nil {
		value := *app.pendingPlanMode
		pending = &value
	}
	app.mu.Unlock()
	current, explicit, err := latestSessionPlanMode(session.EventsSnapshot(), app.RequirePlan)
	if err != nil {
		return true, err
	}
	desired := current
	if pending != nil {
		desired = *pending
	}
	if desired && !app.supportsPlanMode() {
		return false, errors.New("plan mode is unavailable for this assistant composition")
	}
	if current != desired && desired {
		err = session.EnablePlanMode(planModeReasonEnabled)
	} else if current != desired {
		err = session.AppendPlanMode(false, planModeReasonDisabled)
	} else if !explicit {
		err = session.AppendPlanMode(desired, planModeReasonInitialized)
	}
	if err != nil {
		return false, err
	}
	if pending != nil {
		app.mu.Lock()
		if app.pendingPlanMode != nil && *app.pendingPlanMode == *pending {
			app.pendingPlanMode = nil
		}
		app.mu.Unlock()
	}
	return desired, nil
}

func (app *Application) effectivePlanMode(session *Session) bool {
	if session == nil {
		app.mu.Lock()
		defer app.mu.Unlock()
		if app.processPlanMode != nil {
			return *app.processPlanMode
		}
		return app.RequirePlan
	}
	enabled, _, err := latestSessionPlanMode(session.EventsSnapshot(), app.RequirePlan)
	if err != nil {
		// A malformed policy event must never silently disable a safety gate.
		return true
	}
	return enabled
}
