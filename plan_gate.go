package bee

import (
	"errors"
	"fmt"
	"sync"
)

var ErrPlanHasClarification = errors.New("the coding plan has unresolved questions")

// PlanGate is the executor-level mutation invariant for a coding session.
// Read-only tools remain available while the model gathers facts; mutating or
// unknown tools require a valid, unambiguous plan.
type PlanGate struct {
	mu                          sync.RWMutex
	plan                        *TaskPlan
	AllowUnclassifiedBeforePlan bool
}

func (gate *PlanGate) SetPlan(plan TaskPlan) error {
	if gate == nil {
		return errors.New("plan gate is nil")
	}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate coding plan: %w", err)
	}
	copy := cloneTaskPlan(plan)
	gate.mu.Lock()
	gate.plan = &copy
	gate.mu.Unlock()
	return nil
}

func (gate *PlanGate) Clear() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	gate.plan = nil
	gate.mu.Unlock()
}

func (gate *PlanGate) Plan() *TaskPlan {
	if gate == nil {
		return nil
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	if gate.plan == nil {
		return nil
	}
	copy := cloneTaskPlan(*gate.plan)
	return &copy
}

func (gate *PlanGate) Check(definition ToolDefinition) error {
	if gate == nil || definition.Mutability == ToolMutabilityReadOnly || (definition.PlanExempt && allowedPlanExemptToolName(definition.Name)) {
		return nil
	}
	// These are narrowly bounded exceptions: media_retrieve creates one
	// immutable artifact, while scratch writes stay inside the current durable
	// session's private TTL store. Keep them explicit rather than weakening the
	// generic mutating-tool gate; policy/approval still runs afterward.
	if definition.Name == MediaRetrieveToolName || definition.Name == "scratch_set" || definition.Name == "scratch_delete" {
		return nil
	}
	if definition.Mutability == ToolMutabilityUnknown && gate.AllowUnclassifiedBeforePlan {
		return nil
	}
	// Unknown capabilities are treated as mutating at this boundary. A caller
	// must explicitly classify a custom tool read-only before it can run before
	// a plan exists.

	gate.mu.RLock()
	defer gate.mu.RUnlock()
	if gate.plan == nil {
		return ErrPlanRequired
	}
	if gate.plan.NeedsClarification() {
		return ErrPlanHasClarification
	}
	return nil
}

func cloneTaskPlan(plan TaskPlan) TaskPlan {
	copy := plan
	copy.Inputs = append([]string(nil), plan.Inputs...)
	copy.Outputs = append([]string(nil), plan.Outputs...)
	copy.Assumptions = append([]string(nil), plan.Assumptions...)
	copy.Questions = append([]string(nil), plan.Questions...)
	copy.DeferredValidations = append([]string(nil), plan.DeferredValidations...)
	copy.Steps = append([]string(nil), plan.Steps...)
	copy.Variables = append([]VariableSpec(nil), plan.Variables...)
	copy.Acceptance = append([]string(nil), plan.Acceptance...)
	copy.Criteria = make([]AcceptanceCriterion, 0, len(plan.Criteria))
	for _, criterion := range plan.Criteria {
		copy.Criteria = append(copy.Criteria, cloneAcceptanceCriterion(criterion))
	}
	copy.Tasks = make([]ScopedTask, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		taskCopy := task
		taskCopy.ToolNames = append([]string(nil), task.ToolNames...)
		taskCopy.Checklist = append([]ChecklistItem(nil), task.Checklist...)
		copy.Tasks = append(copy.Tasks, taskCopy)
	}
	return copy
}

func latestPlanFromEvents(events []Event) *TaskPlan {
	latest, _ := latestPlanStateFromEvents(events)
	return latest
}

func latestPlanStateFromEvents(events []Event) (*TaskPlan, uint64) {
	var latest *TaskPlan
	var sequence uint64
	evidence := newPlanEvidenceIndex()
	for _, event := range events {
		switch event.Type {
		case EventPlanReset:
			latest = nil
		case EventPlan:
			payload, err := decodeEventPayload[PlanEvent](event, EventPlan)
			if err == nil && payload.Plan.Validate() == nil && validateObservedPlanCriteriaWithIndex(evidence, payload.Plan.Criteria) == nil {
				copy := cloneTaskPlan(payload.Plan)
				normalizePlanCriteria(&copy)
				latest = &copy
				sequence = event.Seq
			}
		case EventPlanUpdate:
			if latest != nil {
				payload, err := decodeEventPayload[PlanUpdateEvent](event, EventPlanUpdate)
				if err == nil && validateObservedPlanEvidenceWithIndex(evidence, payload.Update) == nil {
					updated, updateErr := applyPlanUpdate(*latest, payload.Update)
					if updateErr == nil {
						latest = &updated
						sequence = event.Seq
					}
				}
			}
		}
		evidence.observe(event)
	}
	return latest, sequence
}

func latestSessionPlan(events []Event) (*TaskPlan, error) {
	plan, _, err := latestSessionPlanState(events)
	return plan, err
}

func latestSessionPlanState(events []Event) (*TaskPlan, uint64, error) {
	var latest *TaskPlan
	var sequence uint64
	evidence := newPlanEvidenceIndex()
	for _, event := range events {
		switch event.Type {
		case EventPlan:
			payload, err := decodeEventPayload[PlanEvent](event, EventPlan)
			if err != nil {
				return nil, 0, fmt.Errorf("load session plan: %w", err)
			}
			if err := payload.Plan.Validate(); err != nil {
				return nil, 0, fmt.Errorf("validate session plan: %w", err)
			}
			if err := validateObservedPlanCriteriaWithIndex(evidence, payload.Plan.Criteria); err != nil {
				return nil, 0, fmt.Errorf("validate session plan evidence: %w", err)
			}
			copy := cloneTaskPlan(payload.Plan)
			normalizePlanCriteria(&copy)
			latest = &copy
			sequence = event.Seq
		case EventPlanUpdate:
			payload, err := decodeEventPayload[PlanUpdateEvent](event, EventPlanUpdate)
			if err != nil {
				return nil, 0, fmt.Errorf("load plan update: %w", err)
			}
			if latest == nil {
				return nil, 0, errors.New("plan update has no current plan")
			}
			if err := validateObservedPlanEvidenceWithIndex(evidence, payload.Update); err != nil {
				return nil, 0, fmt.Errorf("validate plan update evidence: %w", err)
			}
			updated, err := applyPlanUpdate(*latest, payload.Update)
			if err != nil {
				return nil, 0, fmt.Errorf("apply session plan update: %w", err)
			}
			latest = &updated
			sequence = event.Seq
		case EventPlanReset:
			if _, err := decodeEventPayload[PlanResetEvent](event, EventPlanReset); err != nil {
				return nil, 0, fmt.Errorf("load plan reset: %w", err)
			}
			latest = nil
			sequence = event.Seq
		}
		evidence.observe(event)
	}
	return latest, sequence, nil
}
