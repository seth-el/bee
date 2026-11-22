package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ContextStatusToolName is the ordinary parent-only, read-only context
// introspection capability. It reports shape and counts, never prompt data.
const ContextStatusToolName = "context_status"

const (
	ContextRetrievalOff          = "off"
	ContextRetrievalNotAttempted = "not_attempted"
	ContextRetrievalSuccess      = "success"
	ContextRetrievalDegraded     = "degraded"
	ContextRetrievalFailure      = "failure"

	ContextStatusSnapshotCurrent = "last_provider_request"
	ContextStatusSnapshotNone    = "not_available"
	ContextBudgetScopeTurn       = "turn_context_selection"
	ContextRequestScopeMessages  = "messages_only"
	ContextEstimateUnit          = "approx_tokens"
	MaxContextStatusSources      = 16
	MaxContextStatusCount        = 1 << 30
)

// ContextBudgetStatus keeps two scopes explicit. Budget/used/remaining describe
// turn-context selection, while RequestEstimatedUsed describes messages in the
// last provider request attempt. Both use Bee's deterministic approximation,
// not provider tokenizer accounting; tool-definition overhead is excluded.
type ContextBudgetStatus struct {
	BudgetConfigured     bool   `json:"budget_configured"`
	Budget               int    `json:"budget"`
	BudgetScope          string `json:"budget_scope"`
	Unbounded            bool   `json:"unbounded"`
	EstimatedUsed        int    `json:"estimated_used"`
	EstimatedRemain      int    `json:"estimated_remaining"`
	OverBudget           bool   `json:"over_budget"`
	RequestEstimatedUsed int    `json:"request_estimated_used"`
	RequestEstimateScope string `json:"request_estimate_scope"`
	RequestSnapshot      string `json:"request_snapshot"`
	EstimateUnit         string `json:"estimate_unit"`
	Clamped              bool   `json:"clamped,omitempty"`
}

// ContextToolStepStatus reports the loop batch count. The current
// context_status call is counted because the agent increments the shared
// provider-tool batch counter before executing any calls in that batch.
type ContextToolStepStatus struct {
	Used              int    `json:"used"`
	Remaining         int    `json:"remaining"`
	Maximum           int    `json:"maximum"`
	Unlimited         bool   `json:"unlimited"`
	CurrentCallCounts bool   `json:"current_call_counts"`
	Unit              string `json:"unit"`
	Clamped           bool   `json:"clamped,omitempty"`
}

type ContextDropSourceStatus struct {
	Source        ContextSource `json:"source"`
	IncludedItems int           `json:"included_items"`
	DroppedItems  int           `json:"dropped_items"`
	EstimatedUsed int           `json:"estimated_used"`
}

type ContextDropStatus struct {
	IncludedItems    int                       `json:"included_items"`
	DroppedItems     int                       `json:"dropped_items"`
	EstimatedUsed    int                       `json:"estimated_used"`
	Sources          []ContextDropSourceStatus `json:"sources,omitempty"`
	SourcesTruncated bool                      `json:"sources_truncated,omitempty"`
	Clamped          bool                      `json:"clamped,omitempty"`
}

// ContextRetrievalStatus contains only retrieval mode/state and quantitative
// diagnostics. It deliberately excludes query text, excerpts, IDs, and
// provider diagnostics.
type ContextRetrievalStatus struct {
	Mode       string `json:"mode"`
	Attempted  bool   `json:"attempted"`
	State      string `json:"state"`
	Candidates int    `json:"candidates"`
	Hits       int    `json:"hits"`
	Returned   int    `json:"returned"`
	Sources    int    `json:"sources"`
	Truncated  bool   `json:"truncated"`
	Clamped    bool   `json:"clamped,omitempty"`
}

type ContextPlanStatus struct {
	Exists                  bool           `json:"exists"`
	PlanID                  string         `json:"plan_id,omitempty"`
	UnresolvedQuestions     bool           `json:"unresolved_questions"`
	UnresolvedQuestionCount int            `json:"unresolved_question_count"`
	CriteriaByState         map[string]int `json:"criteria_by_state"`
}

// ContextStatus is the complete model-facing context_status envelope. It is
// intentionally structural: no prompts, messages, criteria text, evidence
// summaries, paths, arguments, provider payloads, or secrets are included.
type ContextStatus struct {
	Context   ContextBudgetStatus    `json:"context"`
	ToolSteps ContextToolStepStatus  `json:"tool_steps"`
	Dropped   ContextDropStatus      `json:"dropped"`
	Retrieval ContextRetrievalStatus `json:"retrieval"`
	Plan      ContextPlanStatus      `json:"plan"`
	Request   RequestContextUsage    `json:"request"`
	Cache     ContextCacheStatus     `json:"cache"`
	// LastProviderCall is the timing of the most recent model call: duration,
	// time to first token, decode speed, context fill. Absent before any.
	LastProviderCall *ProviderCallTiming `json:"last_provider_call,omitempty"`
}

type ContextCacheStatus struct {
	ReadTokens         int    `json:"read_tokens"`
	WriteTokens        int    `json:"write_tokens"`
	StablePrefixTokens int    `json:"stable_prefix_tokens"`
	PrefixChanged      bool   `json:"prefix_changed"`
	Epoch              uint64 `json:"epoch,omitempty"`
	ChangeReason       string `json:"change_reason,omitempty"`
}

func contextStatusToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        ContextStatusToolName,
		Description: "Read-only total request pressure including system, history, tool results, attachments, tool schemas, output reserve, cache telemetry, dropped counts, retrieval, and plan counts. Never returns prompt or message contents.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		PlanExempt:  true,
	}
}

func (status ContextStatus) Validate() error {
	if status.Context.RequestSnapshot != ContextStatusSnapshotCurrent && status.Context.RequestSnapshot != ContextStatusSnapshotNone {
		return fmt.Errorf("context status snapshot %q is invalid", status.Context.RequestSnapshot)
	}
	if status.Context.BudgetScope != ContextBudgetScopeTurn {
		return fmt.Errorf("context status budget scope %q is invalid", status.Context.BudgetScope)
	}
	if status.Context.RequestEstimateScope != ContextRequestScopeMessages {
		return fmt.Errorf("context status request estimate scope %q is invalid", status.Context.RequestEstimateScope)
	}
	if status.Context.EstimateUnit != ContextEstimateUnit {
		return fmt.Errorf("context status estimate unit %q is invalid", status.Context.EstimateUnit)
	}
	if status.Context.BudgetConfigured != (status.Context.Budget > 0) || status.Context.Unbounded == status.Context.BudgetConfigured {
		return errors.New("context status budget flags are inconsistent")
	}
	if status.Context.RequestSnapshot == ContextStatusSnapshotNone && status.Context.RequestEstimatedUsed != 0 {
		return errors.New("context status unavailable request snapshot has a non-zero estimate")
	}
	if status.Context.EstimatedUsed != status.Dropped.EstimatedUsed {
		return errors.New("context status budget and dropped diagnostics disagree")
	}
	if !status.Context.Clamped {
		if status.Context.BudgetConfigured {
			if status.Context.OverBudget {
				if status.Context.EstimatedUsed <= status.Context.Budget || status.Context.EstimatedRemain != 0 {
					return errors.New("context status over-budget arithmetic is inconsistent")
				}
			} else if status.Context.EstimatedUsed > status.Context.Budget || status.Context.EstimatedRemain != status.Context.Budget-status.Context.EstimatedUsed {
				return errors.New("context status budget arithmetic is inconsistent")
			}
		} else if status.Context.EstimatedRemain != 0 || status.Context.OverBudget {
			return errors.New("context status unbounded budget has bounded arithmetic")
		}
	}
	if status.ToolSteps.Unit != "provider_tool_batches" {
		return fmt.Errorf("context status tool-step unit %q is invalid", status.ToolSteps.Unit)
	}
	if status.ToolSteps.Unlimited != (status.ToolSteps.Maximum == 0) {
		return errors.New("context status tool-step limit flags are inconsistent")
	}
	if status.ToolSteps.Unlimited && status.ToolSteps.Remaining != 0 {
		return errors.New("context status unlimited tool steps must not report a remainder")
	}
	if !status.ToolSteps.Clamped && !status.ToolSteps.Unlimited {
		if status.ToolSteps.Used > status.ToolSteps.Maximum || status.ToolSteps.Remaining != status.ToolSteps.Maximum-status.ToolSteps.Used {
			return errors.New("context status tool-step arithmetic is inconsistent")
		}
	}
	if status.Retrieval.State != ContextRetrievalOff && status.Retrieval.State != ContextRetrievalNotAttempted && status.Retrieval.State != ContextRetrievalSuccess && status.Retrieval.State != ContextRetrievalDegraded && status.Retrieval.State != ContextRetrievalFailure {
		return fmt.Errorf("context status retrieval state %q is invalid", status.Retrieval.State)
	}
	if status.Retrieval.Mode == "" {
		return errors.New("context status retrieval mode is required")
	}
	if status.Retrieval.Mode != string(MemoryRecallOff) && status.Retrieval.Mode != string(MemoryRecallHybrid) && status.Retrieval.Mode != string(MemoryRecallAlways) {
		return fmt.Errorf("context status retrieval mode %q is invalid", status.Retrieval.Mode)
	}
	if status.Retrieval.State == ContextRetrievalOff && status.Retrieval.Mode != string(MemoryRecallOff) {
		return errors.New("context status off retrieval state has a non-off mode")
	}
	if status.Retrieval.State == ContextRetrievalNotAttempted && status.Retrieval.Mode == string(MemoryRecallOff) {
		return errors.New("context status off retrieval mode must use the off state")
	}
	if (status.Retrieval.State == ContextRetrievalOff || status.Retrieval.State == ContextRetrievalNotAttempted) && status.Retrieval.Attempted {
		return errors.New("context status unattempted retrieval state is inconsistent")
	}
	if status.Retrieval.State != ContextRetrievalOff && status.Retrieval.State != ContextRetrievalNotAttempted && !status.Retrieval.Attempted {
		return errors.New("context status attempted retrieval state is inconsistent")
	}
	if (status.Retrieval.State == ContextRetrievalOff || status.Retrieval.State == ContextRetrievalNotAttempted) && (status.Retrieval.Candidates != 0 || status.Retrieval.Hits != 0 || status.Retrieval.Returned != 0 || status.Retrieval.Sources != 0 || status.Retrieval.Truncated) {
		return errors.New("context status unattempted retrieval has result diagnostics")
	}
	if !status.Retrieval.Clamped {
		if status.Retrieval.Returned > status.Retrieval.Candidates || status.Retrieval.Hits > status.Retrieval.Returned || status.Retrieval.Sources > status.Retrieval.Hits {
			return errors.New("context status retrieval counts are inconsistent")
		}
		if status.Retrieval.State == ContextRetrievalFailure && (status.Retrieval.Candidates != 0 || status.Retrieval.Hits != 0 || status.Retrieval.Returned != 0 || status.Retrieval.Sources != 0 || status.Retrieval.Truncated) {
			return errors.New("context status failed retrieval has result diagnostics")
		}
	}
	for _, value := range []int{status.Context.Budget, status.Context.EstimatedUsed, status.Context.EstimatedRemain, status.Context.RequestEstimatedUsed, status.Dropped.IncludedItems, status.Dropped.DroppedItems, status.Dropped.EstimatedUsed, status.ToolSteps.Used, status.ToolSteps.Remaining, status.ToolSteps.Maximum, status.Retrieval.Candidates, status.Retrieval.Hits, status.Retrieval.Returned, status.Retrieval.Sources, status.Plan.UnresolvedQuestionCount} {
		if value < 0 || value > MaxContextStatusCount {
			return errors.New("context status count is outside bounded range")
		}
	}
	for _, state := range []AcceptanceState{AcceptancePending, AcceptanceInProgress, AcceptancePassed, AcceptanceFailed, AcceptanceBlocked} {
		if count := status.Plan.CriteriaByState[string(state)]; count < 0 || count > MaxContextStatusCount {
			return errors.New("context status criterion count is outside bounded range")
		}
	}
	if len(status.Dropped.Sources) > MaxContextStatusSources {
		return errors.New("context status source diagnostics exceed bound")
	}
	for _, source := range status.Dropped.Sources {
		if !validContextStatusSource(source.Source) || source.IncludedItems < 0 || source.DroppedItems < 0 || source.EstimatedUsed < 0 || source.IncludedItems > MaxContextStatusCount || source.DroppedItems > MaxContextStatusCount || source.EstimatedUsed > MaxContextStatusCount {
			return errors.New("context status source diagnostic is invalid")
		}
	}
	if !status.Dropped.SourcesTruncated && !status.Dropped.Clamped {
		included, dropped, estimated := 0, 0, 0
		for _, source := range status.Dropped.Sources {
			included = saturatingIntAdd(included, source.IncludedItems)
			dropped = saturatingIntAdd(dropped, source.DroppedItems)
			estimated = saturatingIntAdd(estimated, source.EstimatedUsed)
		}
		if included != status.Dropped.IncludedItems || dropped != status.Dropped.DroppedItems || estimated != status.Dropped.EstimatedUsed {
			return errors.New("context status source diagnostics disagree with totals")
		}
	}
	if len(status.Plan.CriteriaByState) != 5 {
		return errors.New("context status criterion state map is incomplete")
	}
	for _, state := range []AcceptanceState{AcceptancePending, AcceptanceInProgress, AcceptancePassed, AcceptanceFailed, AcceptanceBlocked} {
		if _, ok := status.Plan.CriteriaByState[string(state)]; !ok {
			return fmt.Errorf("context status criterion state %q is missing", state)
		}
	}
	if status.Plan.PlanID != "" && validateSafeID("context status plan id", status.Plan.PlanID, 128) != nil {
		return errors.New("context status plan id is invalid")
	}
	if !status.Plan.Exists {
		if status.Plan.PlanID != "" || status.Plan.UnresolvedQuestions || status.Plan.UnresolvedQuestionCount != 0 {
			return errors.New("context status absent plan has non-empty metadata")
		}
		for _, count := range status.Plan.CriteriaByState {
			if count != 0 {
				return errors.New("context status absent plan has criterion counts")
			}
		}
	}
	return nil
}

func validContextStatusSource(source ContextSource) bool {
	switch source {
	case ContextSourceSystem, ContextSourceIdentity, ContextSourceInstructions, ContextSourceUserProfile, ContextSourceMemory, ContextSourceConversation, ContextSourceSummary, ContextSourceSession, ContextSourceTool, ContextSourcePlan, ContextSourceAttachment:
		return true
	default:
		return false
	}
}

func (agent *Agent) executeContextStatus() ToolResult {
	result := ToolResult{ToolName: ContextStatusToolName}
	status, err := agent.contextStatus()
	if err != nil {
		result.Error = "context status unavailable"
		return result
	}
	data, err := json.Marshal(status)
	if err != nil {
		result.Error = "context status unavailable"
		return result
	}
	result.Output = string(data)
	return result
}

func (agent *Agent) contextStatus() (ContextStatus, error) {
	if agent == nil {
		return ContextStatus{}, errors.New("agent is nil")
	}
	retrieval := agent.turnRetrieval
	if retrieval.Mode == "" {
		mode := agent.memoryRecallMode.effective()
		retrieval.Mode = string(mode)
		if mode == MemoryRecallOff {
			retrieval.State = ContextRetrievalOff
		} else {
			retrieval.State = ContextRetrievalNotAttempted
		}
	}
	status := ContextStatus{
		Context:   agent.contextBudgetStatus(),
		ToolSteps: agent.contextToolStepStatus(),
		Dropped:   contextDropStatus(agent.turnDiagnostics),
		Retrieval: retrieval,
		Plan:      ContextPlanStatus{CriteriaByState: make(map[string]int, 5)},
		Request:   agent.lastContextUsage,
		Cache:     ContextCacheStatus{ReadTokens: saturatingStatusCount(agent.lastProviderUsage.CacheReadTokens), WriteTokens: saturatingStatusCount(agent.lastProviderUsage.CacheWriteTokens), StablePrefixTokens: saturatingStatusCount(agent.lastContextUsage.PrefixStableTokens), PrefixChanged: agent.lastContextUsage.PrefixChanged, Epoch: agent.lastContextUsage.CacheEpoch, ChangeReason: agent.lastContextUsage.PrefixChangeReason},
	}
	if agent.lastProviderTiming != nil {
		timing := *agent.lastProviderTiming
		status.LastProviderCall = &timing
	}
	for _, state := range []AcceptanceState{AcceptancePending, AcceptanceInProgress, AcceptancePassed, AcceptanceFailed, AcceptanceBlocked} {
		status.Plan.CriteriaByState[string(state)] = 0
	}
	plan, _, err := agent.currentPlanState()
	if err != nil {
		return ContextStatus{}, err
	}
	if plan != nil {
		status.Plan.Exists = true
		if validateSafeID("context status plan id", strings.TrimSpace(plan.ID), 128) == nil {
			status.Plan.PlanID = strings.TrimSpace(plan.ID)
		}
		for _, question := range plan.Questions {
			if strings.TrimSpace(question) != "" {
				status.Plan.UnresolvedQuestionCount = saturatingStatusCount(status.Plan.UnresolvedQuestionCount + 1)
			}
		}
		status.Plan.UnresolvedQuestions = status.Plan.UnresolvedQuestionCount > 0
		for _, criterion := range planCriteria(*plan) {
			if _, ok := status.Plan.CriteriaByState[string(criterion.State)]; ok {
				status.Plan.CriteriaByState[string(criterion.State)] = saturatingStatusCount(status.Plan.CriteriaByState[string(criterion.State)] + 1)
			}
		}
	}
	if err := status.Validate(); err != nil {
		return ContextStatus{}, fmt.Errorf("validate context status: %w", err)
	}
	return status, nil
}

func (agent *Agent) contextBudgetStatus() ContextBudgetStatus {
	rawBudget := agent.contextBase.Budget
	rawUsed := agent.turnDiagnostics.IncludedBytes
	status := ContextBudgetStatus{BudgetConfigured: rawBudget > 0, BudgetScope: ContextBudgetScopeTurn, RequestEstimateScope: ContextRequestScopeMessages, RequestSnapshot: ContextStatusSnapshotNone, EstimateUnit: ContextEstimateUnit}
	if contextStatusCountNeedsClamp(rawBudget) || contextStatusCountNeedsClamp(rawUsed) || contextStatusCountNeedsClamp(agent.lastRequestEstimate) {
		status.Clamped = true
	}
	budget := saturatingStatusCount(rawBudget)
	used := saturatingStatusCount(rawUsed)
	status.Budget = budget
	status.EstimatedUsed = used
	if budget <= 0 {
		status.Unbounded = true
	}
	if agent.lastRequestSnapshot {
		status.RequestSnapshot = ContextStatusSnapshotCurrent
		status.RequestEstimatedUsed = saturatingStatusCount(agent.lastRequestEstimate)
	}
	if rawBudget > 0 {
		if rawUsed >= rawBudget {
			status.OverBudget = rawUsed > rawBudget
			status.EstimatedRemain = 0
		} else {
			status.EstimatedRemain = saturatingStatusCount(rawBudget - rawUsed)
		}
	}
	return status
}

func (agent *Agent) contextToolStepStatus() ContextToolStepStatus {
	used := agent.turnToolStepsUsed
	status := ContextToolStepStatus{Used: saturatingStatusCount(used), Maximum: saturatingStatusCount(agent.maxToolSteps), CurrentCallCounts: true, Unit: "provider_tool_batches"}
	if contextStatusCountNeedsClamp(used) || contextStatusCountNeedsClamp(agent.maxToolSteps) {
		status.Clamped = true
	}
	if agent.maxToolSteps <= 0 {
		status.Unlimited = true
		return status
	}
	if used < agent.maxToolSteps {
		status.Remaining = saturatingStatusCount(agent.maxToolSteps - used)
	}
	return status
}

func contextDropStatus(diagnostics ContextDiagnostics) ContextDropStatus {
	status := ContextDropStatus{IncludedItems: saturatingStatusCount(diagnostics.IncludedItems), DroppedItems: saturatingStatusCount(diagnostics.DroppedItems), EstimatedUsed: saturatingStatusCount(diagnostics.IncludedBytes)}
	if contextStatusCountNeedsClamp(diagnostics.IncludedItems) || contextStatusCountNeedsClamp(diagnostics.DroppedItems) || contextStatusCountNeedsClamp(diagnostics.IncludedBytes) {
		status.Clamped = true
	}
	for _, source := range diagnostics.Sources {
		if len(status.Sources) >= MaxContextStatusSources {
			status.SourcesTruncated = true
			break
		}
		if contextStatusCountNeedsClamp(source.Included) || contextStatusCountNeedsClamp(source.Dropped) || contextStatusCountNeedsClamp(source.Bytes) {
			status.Clamped = true
		}
		status.Sources = append(status.Sources, ContextDropSourceStatus{Source: source.Source, IncludedItems: saturatingStatusCount(source.Included), DroppedItems: saturatingStatusCount(source.Dropped), EstimatedUsed: saturatingStatusCount(source.Bytes)})
	}
	return status
}

func saturatingStatusCount(value int) int {
	if value <= 0 {
		return 0
	}
	if value > MaxContextStatusCount {
		return MaxContextStatusCount
	}
	return value
}

func contextStatusCountNeedsClamp(value int) bool {
	return value < 0 || value > MaxContextStatusCount
}
