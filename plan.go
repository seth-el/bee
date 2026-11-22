package bee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const PlanToolName = "bee_plan"

const (
	PlanStatusToolName            = "plan_status"
	PlanUpdateToolName            = "plan_update"
	MaxPlanCriteria               = 64
	MaxPlanCriterionTextBytes     = 512
	MaxPlanEvidencePerCriterion   = 8
	MaxPlanEvidenceSourceBytes    = 64
	MaxPlanEvidenceReferenceBytes = 256
	MaxPlanEvidenceSummaryBytes   = 512
	MaxPlanUpdateNoteBytes        = 512
	MaxPlanStatusCriterionBytes   = 192
	MaxPlanStatusReferenceBytes   = 96
	MaxPlanStatusEvidencePerKind  = 1
	MaxPlanEvidenceCandidates     = 12
	MaxPlanEvidenceToolNameBytes  = 128
	MaxPlanQuestionUpdates        = 64
	MaxPlanQuestionTextBytes      = 1024
	MaxPlanQuestionReasonBytes    = 512
)

const (
	PlanEvidenceSourceToolResult = "tool_result"
	PlanEvidenceSourceArtifact   = "artifact"
	PlanEvidenceSourceAssistant  = "assistant"

	PlanEvidenceSelectorLatestSuccessfulToolResult = "latest_successful_tool_result"
)

// AcceptanceState is deliberately closed: a criterion is never represented
// as an arbitrary model-defined status.
type AcceptanceState string

const (
	AcceptancePending    AcceptanceState = "pending"
	AcceptanceInProgress AcceptanceState = "in_progress"
	AcceptancePassed     AcceptanceState = "passed"
	AcceptanceFailed     AcceptanceState = "failed"
	AcceptanceBlocked    AcceptanceState = "blocked"
)

// EvidenceProvenance distinguishes a bounded reference to an observed tool,
// test, artifact, or other runtime result from a model assertion. Claims are
// retained for transparency but never count as observed verification.
type EvidenceProvenance string

const (
	EvidenceObserved   EvidenceProvenance = "observed"
	EvidenceModelClaim EvidenceProvenance = "model_claim"
)

type AcceptanceEvidence struct {
	Source     string                `json:"source"`
	Reference  string                `json:"reference,omitempty"`
	Selector   *PlanEvidenceSelector `json:"selector,omitempty"`
	Provenance EvidenceProvenance    `json:"provenance"`
	Summary    string                `json:"summary,omitempty"`
}

// PlanEvidenceSelector lets the parent resolve a useful same-session result
// without requiring the model to retain an opaque provider-generated call ID.
// Resolution happens atomically before persistence, and the durable update
// stores only the resolved reference.
type PlanEvidenceSelector struct {
	Kind     string `json:"kind"`
	ToolName string `json:"tool_name"`
}

// PlanEvidenceCandidate is bounded metadata for evidence the current plan may
// reference. Tool output, arguments, artifact names, and host paths are never
// included.
type PlanEvidenceCandidate struct {
	Source    string `json:"source"`
	Reference string `json:"reference"`
	ToolName  string `json:"tool_name,omitempty"`
}

type AcceptanceCriterion struct {
	ID       string               `json:"id"`
	Text     string               `json:"text"`
	State    AcceptanceState      `json:"state"`
	Evidence []AcceptanceEvidence `json:"evidence,omitempty"`
}

type AcceptanceCriterionUpdate struct {
	ID       string               `json:"id"`
	State    AcceptanceState      `json:"state"`
	Evidence []AcceptanceEvidence `json:"evidence,omitempty"`
}

// PlanQuestionDisposition records why a formerly blocking question no longer
// prevents mutations. Deferred questions remain visible in the canonical plan
// so later human validation is not lost.
type PlanQuestionDisposition string

const (
	PlanQuestionResolved PlanQuestionDisposition = "resolved"
	PlanQuestionDeferred PlanQuestionDisposition = "deferred"
)

type PlanQuestionUpdate struct {
	Question    string                  `json:"question"`
	Disposition PlanQuestionDisposition `json:"disposition"`
	Reason      string                  `json:"reason"`
}

// PlanUpdate is a parent-session-scoped lifecycle update. Criteria are a
// bounded partial patch: omitted criteria remain unchanged.
type PlanUpdate struct {
	PlanID          string                      `json:"plan_id"`
	Criteria        []AcceptanceCriterionUpdate `json:"criteria,omitempty"`
	QuestionUpdates []PlanQuestionUpdate        `json:"question_updates,omitempty"`
	Note            string                      `json:"note,omitempty"`
}

type PlanUpdateEvent struct {
	Update PlanUpdate `json:"update"`
}

type AcceptanceCriterionStatus struct {
	ID                string               `json:"id"`
	Text              string               `json:"text,omitempty"`
	State             AcceptanceState      `json:"state"`
	ObservedEvidence  []AcceptanceEvidence `json:"observed_evidence,omitempty"`
	ModelClaims       []AcceptanceEvidence `json:"model_claims,omitempty"`
	EvidenceTruncated bool                 `json:"evidence_truncated,omitempty"`
}

type PlanStatus struct {
	PlanID         string                      `json:"plan_id"`
	Plan           *TaskPlan                   `json:"plan,omitempty"`
	Fingerprint    string                      `json:"fingerprint,omitempty"`
	Criteria       []AcceptanceCriterionStatus `json:"criteria"`
	RecentEvidence []PlanEvidenceCandidate     `json:"recent_evidence,omitempty"`
	UpdatedSeq     uint64                      `json:"updated_seq,omitempty"`
}

func taskPlanFingerprint(plan TaskPlan) string {
	data, _ := json.Marshal(plan)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// taskPlanDefinitionFingerprint ignores mutable progress/evidence while
// retaining the task definition. It lets Bee reuse an already-active plan
// instead of appending an equivalent replacement on every continuation.
func taskPlanDefinitionFingerprint(plan TaskPlan) string {
	copy := cloneTaskPlan(plan)
	copy.ID = ""
	for index := range copy.Criteria {
		copy.Criteria[index].ID = ""
		copy.Criteria[index].State = AcceptancePending
		copy.Criteria[index].Evidence = nil
	}
	for taskIndex := range copy.Tasks {
		copy.Tasks[taskIndex].ID = ""
		for itemIndex := range copy.Tasks[taskIndex].Checklist {
			copy.Tasks[taskIndex].Checklist[itemIndex].ID = ""
			copy.Tasks[taskIndex].Checklist[itemIndex].Done = false
		}
	}
	return taskPlanFingerprint(copy)
}

func (state AcceptanceState) valid() bool {
	switch state {
	case AcceptancePending, AcceptanceInProgress, AcceptancePassed, AcceptanceFailed, AcceptanceBlocked:
		return true
	default:
		return false
	}
}

func (provenance EvidenceProvenance) valid() bool {
	return provenance == EvidenceObserved || provenance == EvidenceModelClaim
}

func (evidence AcceptanceEvidence) Validate() error {
	if err := validateSafeID("acceptance evidence source", strings.TrimSpace(evidence.Source), MaxPlanEvidenceSourceBytes); err != nil {
		return err
	}
	reference := strings.TrimSpace(evidence.Reference)
	if evidence.Selector != nil {
		if reference != "" {
			return errors.New("acceptance evidence must use exactly one of reference or selector")
		}
		if evidence.Provenance != EvidenceObserved || evidence.Source != PlanEvidenceSourceToolResult {
			return errors.New("acceptance evidence selector requires observed tool_result provenance")
		}
		if evidence.Selector.Kind != PlanEvidenceSelectorLatestSuccessfulToolResult {
			return fmt.Errorf("acceptance evidence selector kind %q is unsupported", evidence.Selector.Kind)
		}
		if err := validateSafeID("acceptance evidence selector tool_name", strings.TrimSpace(evidence.Selector.ToolName), MaxPlanEvidenceToolNameBytes); err != nil {
			return err
		}
	} else if err := validateText("acceptance evidence reference", reference, MaxPlanEvidenceReferenceBytes, true); err != nil {
		return err
	}
	lowerReference := strings.ToLower(reference)
	for _, marker := range []string{"token=", "secret=", "password=", "authorization:", "bearer "} {
		if strings.Contains(lowerReference, marker) {
			return errors.New("acceptance evidence reference looks like credential or secret material")
		}
	}
	if !evidence.Provenance.valid() {
		return fmt.Errorf("acceptance evidence provenance %q is invalid", evidence.Provenance)
	}
	if evidence.Provenance == EvidenceObserved && evidence.Source != PlanEvidenceSourceToolResult && evidence.Source != PlanEvidenceSourceArtifact {
		return fmt.Errorf("observed acceptance evidence source %q is unsupported", evidence.Source)
	}
	if err := validateText("acceptance evidence summary", evidence.Summary, MaxPlanEvidenceSummaryBytes, false); err != nil {
		return err
	}
	return nil
}

func (criterion AcceptanceCriterion) Validate() error {
	if err := validateSafeID("acceptance criterion id", strings.TrimSpace(criterion.ID), 128); err != nil {
		return err
	}
	if err := validateText("acceptance criterion text", criterion.Text, MaxPlanCriterionTextBytes, true); err != nil {
		return err
	}
	if !criterion.State.valid() {
		return fmt.Errorf("acceptance criterion %q has invalid state %q", criterion.ID, criterion.State)
	}
	if len(criterion.Evidence) > MaxPlanEvidencePerCriterion {
		return fmt.Errorf("acceptance criterion %q exceeds %d evidence references", criterion.ID, MaxPlanEvidencePerCriterion)
	}
	seen := make(map[string]struct{}, len(criterion.Evidence))
	for _, evidence := range criterion.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("acceptance criterion %q evidence: %w", criterion.ID, err)
		}
		key := acceptanceEvidenceKey(evidence)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("acceptance criterion %q contains duplicate evidence", criterion.ID)
		}
		seen[key] = struct{}{}
	}
	if criterion.State == AcceptancePassed && !criterion.hasObservedEvidence() {
		return fmt.Errorf("acceptance criterion %q cannot be passed without observed evidence", criterion.ID)
	}
	return nil
}

func (criterion AcceptanceCriterion) hasObservedEvidence() bool {
	for _, evidence := range criterion.Evidence {
		if evidence.Provenance == EvidenceObserved {
			return true
		}
	}
	return false
}

func (update AcceptanceCriterionUpdate) Validate() error {
	if err := validateSafeID("acceptance criterion id", strings.TrimSpace(update.ID), 128); err != nil {
		return err
	}
	if !update.State.valid() {
		return fmt.Errorf("acceptance criterion %q has invalid state %q", update.ID, update.State)
	}
	if len(update.Evidence) > MaxPlanEvidencePerCriterion {
		return fmt.Errorf("acceptance criterion %q exceeds %d evidence references", update.ID, MaxPlanEvidencePerCriterion)
	}
	seen := make(map[string]struct{}, len(update.Evidence))
	for _, evidence := range update.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("acceptance criterion %q evidence: %w", update.ID, err)
		}
		key := acceptanceEvidenceKey(evidence)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("acceptance criterion %q contains duplicate evidence", update.ID)
		}
		seen[key] = struct{}{}
	}
	if update.State == AcceptancePassed && !evidenceHasObserved(update.Evidence) {
		return fmt.Errorf("acceptance criterion %q cannot be passed without observed evidence", update.ID)
	}
	return nil
}

func (disposition PlanQuestionDisposition) valid() bool {
	return disposition == PlanQuestionResolved || disposition == PlanQuestionDeferred
}

func (update PlanQuestionUpdate) Validate() error {
	if err := validateText("plan question update question", update.Question, MaxPlanQuestionTextBytes, true); err != nil {
		return err
	}
	if !update.Disposition.valid() {
		return fmt.Errorf("plan question %q has invalid disposition %q", update.Question, update.Disposition)
	}
	if err := validateText("plan question update reason", update.Reason, MaxPlanQuestionReasonBytes, true); err != nil {
		return err
	}
	return nil
}

func evidenceHasObserved(evidence []AcceptanceEvidence) bool {
	for _, item := range evidence {
		if item.Provenance == EvidenceObserved {
			return true
		}
	}
	return false
}

func acceptanceEvidenceKey(evidence AcceptanceEvidence) string {
	selectorKind, selectorTool := "", ""
	if evidence.Selector != nil {
		selectorKind = strings.TrimSpace(evidence.Selector.Kind)
		selectorTool = strings.TrimSpace(evidence.Selector.ToolName)
	}
	return string(evidence.Provenance) + "\x00" + strings.TrimSpace(evidence.Source) + "\x00" + strings.TrimSpace(evidence.Reference) + "\x00" + selectorKind + "\x00" + selectorTool
}

func planCriteriaHaveObservedEvidence(criteria []AcceptanceCriterion) bool {
	for _, criterion := range criteria {
		if evidenceHasObserved(criterion.Evidence) {
			return true
		}
	}
	return false
}

type planEvidenceIndex struct {
	successfulToolResults map[string]struct{}
	artifacts             map[string]struct{}
	latestToolResults     map[string]string
	recent                []PlanEvidenceCandidate
}

func newPlanEvidenceIndex() *planEvidenceIndex {
	return &planEvidenceIndex{successfulToolResults: make(map[string]struct{}), artifacts: make(map[string]struct{}), latestToolResults: make(map[string]string)}
}

func planEvidenceIndexFromEvents(events []Event) *planEvidenceIndex {
	index := newPlanEvidenceIndex()
	for _, event := range events {
		index.observe(event)
	}
	return index
}

func (index *planEvidenceIndex) observe(event Event) {
	if index == nil {
		return
	}
	switch event.Type {
	case EventToolResult:
		payload, err := decodeEventPayload[ToolResultEvent](event, EventToolResult)
		if err == nil && strings.TrimSpace(payload.CallID) != "" && strings.TrimSpace(payload.Error) == "" {
			reference := strings.TrimSpace(payload.CallID)
			if _, exists := index.successfulToolResults[reference]; !exists {
				index.successfulToolResults[reference] = struct{}{}
				toolName := strings.TrimSpace(payload.ToolName)
				if planEvidenceToolIsDiscoverable(toolName) && planEvidenceReferenceIsDiscoverable(PlanEvidenceSourceToolResult, reference) {
					index.latestToolResults[toolName] = reference
					index.appendRecent(PlanEvidenceCandidate{Source: PlanEvidenceSourceToolResult, Reference: reference, ToolName: toolName})
				}
			}
		}
	case EventArtifact:
		payload, err := decodeEventPayload[ArtifactEvent](event, EventArtifact)
		if err == nil && strings.TrimSpace(payload.Artifact.ID) != "" {
			reference := strings.TrimSpace(payload.Artifact.ID)
			if _, exists := index.artifacts[reference]; !exists {
				index.artifacts[reference] = struct{}{}
				if planEvidenceReferenceIsDiscoverable(PlanEvidenceSourceArtifact, reference) {
					index.appendRecent(PlanEvidenceCandidate{Source: PlanEvidenceSourceArtifact, Reference: reference})
				}
			}
		}
	}
}

func planEvidenceReferenceIsDiscoverable(source, reference string) bool {
	return (AcceptanceEvidence{Source: source, Reference: reference, Provenance: EvidenceObserved}).Validate() == nil
}

func (index *planEvidenceIndex) appendRecent(candidate PlanEvidenceCandidate) {
	if index == nil {
		return
	}
	index.recent = append(index.recent, candidate)
	if len(index.recent) > MaxPlanEvidenceCandidates {
		copy(index.recent, index.recent[len(index.recent)-MaxPlanEvidenceCandidates:])
		index.recent = index.recent[:MaxPlanEvidenceCandidates]
	}
}

func planEvidenceToolIsDiscoverable(name string) bool {
	switch strings.TrimSpace(name) {
	case "", PlanToolName, PlanStatusToolName, PlanUpdateToolName, ContextStatusToolName:
		return false
	default:
		return true
	}
}

func (index *planEvidenceIndex) recentCandidates(limit int) []PlanEvidenceCandidate {
	if index == nil || limit <= 0 || len(index.recent) == 0 {
		return nil
	}
	if limit > len(index.recent) {
		limit = len(index.recent)
	}
	result := make([]PlanEvidenceCandidate, 0, limit)
	for position := len(index.recent) - 1; position >= 0 && len(result) < limit; position-- {
		result = append(result, index.recent[position])
	}
	return result
}

func (index *planEvidenceIndex) latestSuccessfulToolResult(toolName string) (string, bool) {
	toolName = strings.TrimSpace(toolName)
	if index == nil || !planEvidenceToolIsDiscoverable(toolName) {
		return "", false
	}
	reference, ok := index.latestToolResults[toolName]
	return reference, ok
}

func resolvePlanEvidenceSelectors(index *planEvidenceIndex, update PlanUpdate) (PlanUpdate, error) {
	resolved := update
	resolved.Criteria = make([]AcceptanceCriterionUpdate, len(update.Criteria))
	resolved.QuestionUpdates = append([]PlanQuestionUpdate(nil), update.QuestionUpdates...)
	for criterionIndex, criterion := range update.Criteria {
		resolvedCriterion := criterion
		resolvedCriterion.Evidence = make([]AcceptanceEvidence, len(criterion.Evidence))
		for evidenceIndex, evidence := range criterion.Evidence {
			resolvedEvidence := cloneAcceptanceEvidence(evidence)
			if evidence.Selector != nil {
				reference, ok := index.latestSuccessfulToolResult(evidence.Selector.ToolName)
				if !ok {
					return PlanUpdate{}, fmt.Errorf("acceptance criterion %q evidence selector found no successful same-session result for tool %q", criterion.ID, evidence.Selector.ToolName)
				}
				resolvedEvidence.Reference = reference
				resolvedEvidence.Selector = nil
			}
			resolvedCriterion.Evidence[evidenceIndex] = resolvedEvidence
		}
		resolved.Criteria[criterionIndex] = resolvedCriterion
	}
	if err := resolved.Validate(); err != nil {
		return PlanUpdate{}, err
	}
	return resolved, nil
}

func validateObservedPlanCriteria(events []Event, criteria []AcceptanceCriterion) error {
	return validateObservedPlanCriteriaWithIndex(planEvidenceIndexFromEvents(events), criteria)
}

func validateObservedPlanCriteriaWithIndex(index *planEvidenceIndex, criteria []AcceptanceCriterion) error {
	for _, criterion := range criteria {
		if err := validateObservedPlanEvidenceItems(index, criterion.ID, criterion.Evidence); err != nil {
			return err
		}
	}
	return nil
}

func validateObservedPlanEvidence(events []Event, update PlanUpdate) error {
	return validateObservedPlanEvidenceWithIndex(planEvidenceIndexFromEvents(events), update)
}

func validateObservedPlanEvidenceWithIndex(index *planEvidenceIndex, update PlanUpdate) error {
	for _, criterion := range update.Criteria {
		if err := validateObservedPlanEvidenceItems(index, criterion.ID, criterion.Evidence); err != nil {
			return err
		}
	}
	return nil
}

func validateObservedPlanEvidenceItems(index *planEvidenceIndex, criterionID string, evidence []AcceptanceEvidence) error {
	if index == nil {
		index = newPlanEvidenceIndex()
	}
	for _, item := range evidence {
		if item.Provenance != EvidenceObserved {
			continue
		}
		reference := strings.TrimSpace(item.Reference)
		switch strings.TrimSpace(item.Source) {
		case PlanEvidenceSourceToolResult:
			if _, ok := index.successfulToolResults[reference]; !ok {
				return fmt.Errorf("acceptance criterion %q observed evidence %q does not reference a successful session tool result", criterionID, reference)
			}
		case PlanEvidenceSourceArtifact:
			if _, ok := index.artifacts[reference]; !ok {
				return fmt.Errorf("acceptance criterion %q observed evidence %q does not reference a session artifact", criterionID, reference)
			}
		default:
			return fmt.Errorf("acceptance criterion %q observed evidence source %q is unsupported", criterionID, item.Source)
		}
	}
	return nil
}

func (update PlanUpdate) Validate() error {
	if err := validateSafeID("plan update plan_id", strings.TrimSpace(update.PlanID), 128); err != nil {
		return err
	}
	if len(update.Criteria) == 0 && len(update.QuestionUpdates) == 0 {
		return errors.New("plan update must contain criteria or question_updates")
	}
	if len(update.Criteria) > MaxPlanCriteria {
		return fmt.Errorf("plan update criteria exceed %d entries", MaxPlanCriteria)
	}
	if len(update.QuestionUpdates) > MaxPlanQuestionUpdates {
		return fmt.Errorf("plan question updates exceed %d entries", MaxPlanQuestionUpdates)
	}
	if err := validateText("plan update note", update.Note, MaxPlanUpdateNoteBytes, false); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(update.Criteria))
	for _, criterion := range update.Criteria {
		if err := criterion.Validate(); err != nil {
			return err
		}
		if _, ok := seen[criterion.ID]; ok {
			return fmt.Errorf("plan update contains duplicate criterion %q", criterion.ID)
		}
		seen[criterion.ID] = struct{}{}
	}
	seenQuestions := make(map[string]struct{}, len(update.QuestionUpdates))
	for _, question := range update.QuestionUpdates {
		if err := question.Validate(); err != nil {
			return err
		}
		key := strings.TrimSpace(question.Question)
		if _, ok := seenQuestions[key]; ok {
			return fmt.Errorf("plan update contains duplicate question %q", question.Question)
		}
		seenQuestions[key] = struct{}{}
	}
	return nil
}

func normalizePlanCriteria(plan *TaskPlan) {
	if plan == nil || len(plan.Criteria) > 0 {
		return
	}
	plan.Criteria = make([]AcceptanceCriterion, 0, len(plan.Acceptance))
	for index, text := range plan.Acceptance {
		plan.Criteria = append(plan.Criteria, AcceptanceCriterion{ID: fmt.Sprintf("criterion-%d", index+1), Text: text, State: AcceptancePending})
	}
}

func planCriteria(plan TaskPlan) []AcceptanceCriterion {
	copy := cloneTaskPlan(plan)
	normalizePlanCriteria(&copy)
	return append([]AcceptanceCriterion(nil), copy.Criteria...)
}

func applyPlanUpdate(plan TaskPlan, update PlanUpdate) (TaskPlan, error) {
	if err := update.Validate(); err != nil {
		return TaskPlan{}, err
	}
	if plan.ID != update.PlanID {
		return TaskPlan{}, fmt.Errorf("plan update targets %q, current plan is %q", update.PlanID, plan.ID)
	}
	plan = cloneTaskPlan(plan)
	normalizePlanCriteria(&plan)
	byID := make(map[string]int, len(plan.Criteria))
	for index, criterion := range plan.Criteria {
		byID[criterion.ID] = index
	}
	for _, patch := range update.Criteria {
		index, ok := byID[patch.ID]
		if !ok {
			return TaskPlan{}, fmt.Errorf("plan update criterion %q is unknown", patch.ID)
		}
		current := plan.Criteria[index]
		if current.State == AcceptancePassed && patch.State != AcceptancePassed {
			return TaskPlan{}, fmt.Errorf("passed criterion %q cannot transition backward", patch.ID)
		}
		if patch.State == AcceptancePassed && !evidenceHasObserved(patch.Evidence) {
			return TaskPlan{}, fmt.Errorf("criterion %q cannot be passed without observed evidence", patch.ID)
		}
		current.State = patch.State
		current.Evidence = make([]AcceptanceEvidence, len(patch.Evidence))
		for evidenceIndex, evidence := range patch.Evidence {
			current.Evidence[evidenceIndex] = cloneAcceptanceEvidence(evidence)
		}
		plan.Criteria[index] = current
	}
	for _, patch := range update.QuestionUpdates {
		question := strings.TrimSpace(patch.Question)
		found := false
		original := ""
		remaining := make([]string, 0, len(plan.Questions))
		for _, candidate := range plan.Questions {
			if strings.TrimSpace(candidate) == question {
				if !found {
					original = candidate
				}
				found = true
				continue
			}
			remaining = append(remaining, candidate)
		}
		if !found {
			return TaskPlan{}, fmt.Errorf("plan question %q is unknown", patch.Question)
		}
		plan.Questions = remaining
		if patch.Disposition == PlanQuestionDeferred && !containsTrimmed(plan.DeferredValidations, original) {
			plan.DeferredValidations = append(plan.DeferredValidations, original)
		}
	}
	return plan, nil
}

func containsTrimmed(values []string, wanted string) bool {
	wanted = strings.TrimSpace(wanted)
	for _, value := range values {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

func cloneAcceptanceCriterion(criterion AcceptanceCriterion) AcceptanceCriterion {
	copy := criterion
	copy.Evidence = make([]AcceptanceEvidence, len(criterion.Evidence))
	for index, evidence := range criterion.Evidence {
		copy.Evidence[index] = cloneAcceptanceEvidence(evidence)
	}
	return copy
}

func planStatusFromPlan(plan TaskPlan, seq uint64) PlanStatus {
	return planStatusFromPlanWithEvidence(plan, seq, nil)
}

func planStatusFromPlanWithEvidence(plan TaskPlan, seq uint64, evidence *planEvidenceIndex) PlanStatus {
	copy := providerSafePlan(plan)
	status := PlanStatus{PlanID: plan.ID, Plan: &copy, Fingerprint: taskPlanFingerprint(plan), UpdatedSeq: seq, Criteria: make([]AcceptanceCriterionStatus, 0, len(planCriteria(plan)))}
	for _, criterion := range planCriteria(plan) {
		entry := AcceptanceCriterionStatus{ID: criterion.ID, Text: safePlanStatusCriterionText(criterion.Text), State: criterion.State}
		observed, claims := 0, 0
		for _, evidence := range criterion.Evidence {
			if evidence.Provenance == EvidenceObserved {
				if observed >= MaxPlanStatusEvidencePerKind {
					entry.EvidenceTruncated = true
					continue
				}
				copy := cloneAcceptanceEvidence(evidence)
				copy.Source = truncateUTF8(copy.Source, 32)
				copy.Reference = truncateUTF8(copy.Reference, MaxPlanStatusReferenceBytes)
				copy.Summary = ""
				entry.ObservedEvidence = append(entry.ObservedEvidence, copy)
				observed++
			} else {
				if claims >= MaxPlanStatusEvidencePerKind {
					entry.EvidenceTruncated = true
					continue
				}
				copy := cloneAcceptanceEvidence(evidence)
				copy.Source = truncateUTF8(copy.Source, 32)
				copy.Reference = truncateUTF8(copy.Reference, MaxPlanStatusReferenceBytes)
				copy.Summary = ""
				entry.ModelClaims = append(entry.ModelClaims, copy)
				claims++
			}
		}
		status.Criteria = append(status.Criteria, entry)
	}
	status.RecentEvidence = evidence.recentCandidates(MaxPlanEvidenceCandidates)
	return status
}

func canonicalPlanToolOutput(status string, plan TaskPlan, sequence uint64, message string) string {
	payload := struct {
		Status      string   `json:"status"`
		Message     string   `json:"message,omitempty"`
		Plan        TaskPlan `json:"plan"`
		UpdatedSeq  uint64   `json:"updated_seq,omitempty"`
		Fingerprint string   `json:"fingerprint"`
	}{Status: status, Message: message, Plan: providerSafePlan(plan), UpdatedSeq: sequence, Fingerprint: taskPlanFingerprint(plan)}
	data, _ := json.Marshal(payload)
	return string(data)
}

func providerSafePlan(plan TaskPlan) TaskPlan {
	copy := cloneTaskPlan(plan)
	safe := func(value string) string { return redactString(value) }
	safeList := func(values []string) {
		for index := range values {
			values[index] = safe(values[index])
		}
	}
	copy.ID = safe(copy.ID)
	copy.Goal = safe(copy.Goal)
	copy.Scope = safe(copy.Scope)
	safeList(copy.Inputs)
	safeList(copy.Outputs)
	safeList(copy.Assumptions)
	safeList(copy.Questions)
	safeList(copy.DeferredValidations)
	safeList(copy.Steps)
	safeList(copy.Acceptance)
	for index := range copy.Variables {
		copy.Variables[index].Name = safe(copy.Variables[index].Name)
		copy.Variables[index].Type = safe(copy.Variables[index].Type)
		copy.Variables[index].Scope = safe(copy.Variables[index].Scope)
	}
	for index := range copy.Criteria {
		criterion := &copy.Criteria[index]
		criterion.ID = safe(criterion.ID)
		criterion.Text = safe(criterion.Text)
		for evidenceIndex := range criterion.Evidence {
			evidence := &criterion.Evidence[evidenceIndex]
			evidence.Source = safe(evidence.Source)
			evidence.Reference = safe(evidence.Reference)
			evidence.Summary = safe(evidence.Summary)
			if evidence.Selector != nil {
				evidence.Selector.ToolName = safe(evidence.Selector.ToolName)
			}
		}
	}
	for index := range copy.Tasks {
		task := &copy.Tasks[index]
		task.ID = safe(task.ID)
		task.Title = safe(task.Title)
		task.Prompt = safe(task.Prompt)
		safeList(task.ToolNames)
		task.Scope.Root = safe(task.Scope.Root)
		safeList(task.Scope.Roots)
		task.Scope.WriteRoot = safe(task.Scope.WriteRoot)
		safeList(task.Scope.WriteRoots)
		safeList(task.Scope.EnvAllowlist)
		task.Scope.RemoteGrant = nil
		for itemIndex := range task.Checklist {
			task.Checklist[itemIndex].ID = safe(task.Checklist[itemIndex].ID)
			task.Checklist[itemIndex].Text = safe(task.Checklist[itemIndex].Text)
		}
	}
	return copy
}

func safePlanStatusCriterionText(value string) string {
	value = strings.TrimSpace(redactString(value))
	if looksLikeSecret(value) {
		return "[REDACTED]"
	}
	return truncateUTF8(value, MaxPlanStatusCriterionBytes)
}

func cloneAcceptanceEvidence(evidence AcceptanceEvidence) AcceptanceEvidence {
	copy := evidence
	if evidence.Selector != nil {
		selector := *evidence.Selector
		copy.Selector = &selector
	}
	return copy
}

// VariableSpec records an important value the plan expects to introduce or
// carry through the implementation.
type VariableSpec struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Scope string `json:"scope"`
}

// ChecklistItem is a serializable unit of lead-model work tracking.
type ChecklistItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// ScopedTask describes a bounded task without starting a subagent. Phase 2
// exposes this seam so a future delegation phase can reuse the same contract.
type ScopedTask struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Prompt    string          `json:"prompt,omitempty"`
	ToolNames []string        `json:"tool_names,omitempty"`
	Scope     ExecutionScope  `json:"scope"`
	Checklist []ChecklistItem `json:"checklist,omitempty"`
}

// TaskPlan is a provider-neutral plan/checklist document. When an Agent is
// configured with RequirePlan, the model records one through the internal
// bee_plan tool before coding tools may mutate the workspace.
type TaskPlan struct {
	ID                  string                `json:"id"`
	Goal                string                `json:"goal"`
	Inputs              []string              `json:"inputs"`
	Outputs             []string              `json:"outputs"`
	Assumptions         []string              `json:"assumptions,omitempty"`
	Questions           []string              `json:"questions,omitempty"`
	DeferredValidations []string              `json:"deferred_validations,omitempty"`
	Steps               []string              `json:"steps"`
	Variables           []VariableSpec        `json:"variables,omitempty"`
	Scope               string                `json:"scope,omitempty"`
	Acceptance          []string              `json:"acceptance"`
	Criteria            []AcceptanceCriterion `json:"criteria,omitempty"`
	Tasks               []ScopedTask          `json:"tasks,omitempty"`
}

func (plan TaskPlan) Validate() error {
	if strings.TrimSpace(plan.ID) == "" {
		return errors.New("plan id is required")
	}
	if strings.TrimSpace(plan.Goal) == "" {
		return errors.New("plan goal is required")
	}
	if len(plan.Inputs) == 0 {
		return errors.New("plan must identify at least one input")
	}
	if len(plan.Outputs) == 0 {
		return errors.New("plan must identify at least one output")
	}
	if len(plan.Steps) == 0 {
		return errors.New("plan must contain at least one ordered step")
	}
	if len(plan.Acceptance) == 0 && len(plan.Criteria) == 0 {
		return errors.New("plan must identify at least one acceptance criterion")
	}
	if len(plan.Acceptance) > 0 && len(plan.Criteria) > 0 && !planAcceptanceMatchesCriteria(plan.Acceptance, plan.Criteria) {
		return errors.New("plan acceptance and criteria disagree")
	}
	if err := validateTextList("plan acceptance", plan.Acceptance, MaxPlanCriteria, MaxPlanCriterionTextBytes); err != nil {
		return err
	}
	if len(plan.Criteria) > MaxPlanCriteria {
		return fmt.Errorf("plan criteria exceed %d entries", MaxPlanCriteria)
	}
	if len(plan.Criteria) > 0 {
		seenCriteria := make(map[string]struct{}, len(plan.Criteria))
		for _, criterion := range plan.Criteria {
			if err := criterion.Validate(); err != nil {
				return err
			}
			if _, ok := seenCriteria[criterion.ID]; ok {
				return fmt.Errorf("duplicate acceptance criterion %q", criterion.ID)
			}
			seenCriteria[criterion.ID] = struct{}{}
		}
	}
	seenVariables := make(map[string]bool, len(plan.Variables))
	for index, variable := range plan.Variables {
		if strings.TrimSpace(variable.Name) == "" {
			return fmt.Errorf("variable %d name is required", index)
		}
		if strings.TrimSpace(variable.Type) == "" {
			return fmt.Errorf("variable %q type is required", variable.Name)
		}
		if strings.TrimSpace(variable.Scope) == "" {
			return fmt.Errorf("variable %q scope is required", variable.Name)
		}
		if seenVariables[variable.Name] {
			return fmt.Errorf("duplicate variable %q", variable.Name)
		}
		seenVariables[variable.Name] = true
	}
	seenTasks := make(map[string]bool, len(plan.Tasks))
	for index, task := range plan.Tasks {
		if strings.TrimSpace(task.ID) == "" {
			return fmt.Errorf("task %d id is required", index)
		}
		if seenTasks[task.ID] {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		seenTasks[task.ID] = true
		if strings.TrimSpace(task.Title) == "" {
			return fmt.Errorf("task %q title is required", task.ID)
		}
		if err := task.Scope.ValidateShape(); err != nil {
			return fmt.Errorf("task %q scope: %w", task.ID, err)
		}
		seenChecklist := make(map[string]bool, len(task.Checklist))
		for itemIndex, item := range task.Checklist {
			if strings.TrimSpace(item.ID) == "" {
				return fmt.Errorf("task %q checklist item %d id is required", task.ID, itemIndex)
			}
			if seenChecklist[item.ID] {
				return fmt.Errorf("task %q duplicate checklist id %q", task.ID, item.ID)
			}
			seenChecklist[item.ID] = true
			if strings.TrimSpace(item.Text) == "" {
				return fmt.Errorf("task %q checklist item %q text is required", task.ID, item.ID)
			}
		}
	}
	return nil
}

func planAcceptanceMatchesCriteria(acceptance []string, criteria []AcceptanceCriterion) bool {
	if len(acceptance) != len(criteria) {
		return false
	}
	for index := range acceptance {
		if acceptance[index] != criteria[index].Text {
			return false
		}
	}
	return true
}

// NeedsClarification reports whether the lead model surfaced a consequential
// ambiguity instead of silently choosing an interpretation.
func (plan TaskPlan) NeedsClarification() bool { return len(plan.Questions) > 0 }

func planToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        PlanToolName,
		Description: "Record the structured coding plan before gated mutations when the latest Bee control record enables plan mode. This tool remains exposed when plan mode is off so the harness prefix stays cache-stable; its presence alone does not require planning. Always supply id, goal, inputs, outputs, steps, and acceptance; for example: {\"id\":\"plan-1\",\"goal\":\"make the requested change\",\"inputs\":[\"current workspace\"],\"outputs\":[\"updated files\"],\"steps\":[\"inspect\",\"change\",\"verify\"],\"acceptance\":[\"the requested change is verified\"]}. Put only consequential unknowns that require a user answer before the next mutation in questions. Put later human review in acceptance, not questions.",
		Risk:        RiskLow,
		Parameters:  []byte(`{"type":"object","properties":{"id":{"type":"string","minLength":1},"goal":{"type":"string","minLength":1},"inputs":{"type":"array","minItems":1,"items":{"type":"string"}},"outputs":{"type":"array","minItems":1,"items":{"type":"string"}},"assumptions":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"string"}},"steps":{"type":"array","minItems":1,"items":{"type":"string"}},"variables":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"type":{"type":"string"},"scope":{"type":"string"}},"required":["name","type","scope"]}},"scope":{"type":"string"},"acceptance":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"string"}},"criteria":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"state":{"type":"string","enum":["pending"]}},"required":["id","text","state"],"additionalProperties":false}},"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"prompt":{"type":"string"},"tool_names":{"type":"array","items":{"type":"string"}},"scope":{"type":"object","properties":{"root":{"type":"string"},"timeout_seconds":{"type":"integer"},"max_output_bytes":{"type":"integer"},"env_allowlist":{"type":"array","items":{"type":"string"}}},"required":["root"]},"checklist":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"done":{"type":"boolean"}},"required":["id","text"]}}},"required":["id","title","scope"]}}},"required":["id","goal","inputs","outputs","steps","acceptance"],"additionalProperties":false}`),
	}
}

func planStatusToolDefinition() ToolDefinition {
	return ToolDefinition{Name: PlanStatusToolName, Description: "Read the current canonical session plan, acceptance status, bounded evidence provenance, and recent_evidence entries containing exact eligible source/reference IDs. This tool remains exposed when plan mode is off for cache stability. Copy evidence IDs into plan_update; do not treat model claims as observed verification.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: []byte(`{"type":"object","properties":{},"additionalProperties":false}`)}
}

func planUpdateToolDefinition() ToolDefinition {
	return ToolDefinition{Name: PlanUpdateToolName, Description: "Update bounded acceptance criteria or disposition blocking questions in the current session plan and return the complete canonical plan state. This tool remains exposed when plan mode is off for cache stability. A question may be resolved after its answer is known, or deferred when it is actually later validation that does not block implementation; give the exact current question and a concrete reason. Deferred items remain visible in deferred_validations. Criteria use only pending, in_progress, passed, failed, or blocked. Passed requires observed evidence. Copy an exact source/reference from plan_status recent_evidence, or for a tool result use selector {kind: latest_successful_tool_result, tool_name: TOOL}; selectors are resolved to exact call IDs before persistence. Model claims remain explicitly non-observed.", Risk: RiskLow, Mutability: ToolMutabilityMutating, Parameters: []byte(`{"type":"object","properties":{"plan_id":{"type":"string"},"criteria":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string"},"state":{"type":"string","enum":["pending","in_progress","passed","failed","blocked"]},"evidence":{"type":"array","maxItems":8,"items":{"type":"object","properties":{"source":{"type":"string","enum":["tool_result","artifact","assistant"]},"reference":{"type":"string"},"selector":{"type":"object","properties":{"kind":{"type":"string","enum":["latest_successful_tool_result"]},"tool_name":{"type":"string"}},"required":["kind","tool_name"],"additionalProperties":false},"provenance":{"type":"string","enum":["observed","model_claim"]},"summary":{"type":"string"}},"required":["source","provenance"],"additionalProperties":false}}},"required":["id","state"],"additionalProperties":false}},"question_updates":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"question":{"type":"string","minLength":1,"maxLength":1024},"disposition":{"type":"string","enum":["resolved","deferred"]},"reason":{"type":"string","minLength":1,"maxLength":512}},"required":["question","disposition","reason"],"additionalProperties":false}},"note":{"type":"string"}},"required":["plan_id"],"anyOf":[{"required":["criteria"]},{"required":["question_updates"]}],"additionalProperties":false}`)}
}
