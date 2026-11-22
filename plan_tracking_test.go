package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func trackedPlan(criteria ...AcceptanceCriterion) TaskPlan {
	return TaskPlan{ID: "tracked-plan", Goal: "bounded work", Inputs: []string{"repository"}, Outputs: []string{"result"}, Steps: []string{"inspect", "change", "verify"}, Criteria: criteria}
}

func observedCriterionUpdate(id string, state AcceptanceState) AcceptanceCriterionUpdate {
	return AcceptanceCriterionUpdate{ID: id, State: state, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Reference: "call-" + id, Provenance: EvidenceObserved, Summary: "bounded tool result"}}}
}

func appendSuccessfulPlanEvidence(t *testing.T, session *Session, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := session.AppendToolResult(ToolResultEvent{CallID: "call-" + id, ToolName: "test", Output: "passed"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcceptanceCriterionValidationRequiresObservedEvidenceForPassed(t *testing.T) {
	criterion := AcceptanceCriterion{ID: "check-1", Text: "tests pass", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: "tool_result", Reference: "call-1", Provenance: EvidenceModelClaim, Summary: "model says tests passed"}}}
	if err := criterion.Validate(); err == nil || !strings.Contains(err.Error(), "observed evidence") {
		t.Fatalf("validation error=%v", err)
	}
	criterion.Evidence[0].Provenance = EvidenceObserved
	if err := criterion.Validate(); err != nil {
		t.Fatal(err)
	}
	criterion.Evidence[0].Reference = "token=secret"
	if err := criterion.Validate(); err == nil {
		t.Fatal("secret-like evidence reference accepted")
	}
}

func TestPlanUpdateTransitionsAndClaimsRemainDistinct(t *testing.T) {
	plan := trackedPlan(
		AcceptanceCriterion{ID: "check-1", Text: "inspect", State: AcceptancePending},
		AcceptanceCriterion{ID: "check-2", Text: "verify", State: AcceptancePending},
	)
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	updated, err := applyPlanUpdate(plan, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptanceInProgress, Evidence: []AcceptanceEvidence{{Source: "assistant", Reference: "claim-1", Provenance: EvidenceModelClaim, Summary: "model inspected"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	status := planStatusFromPlan(updated, 4)
	if len(status.Criteria) != 2 || len(status.Criteria[0].ModelClaims) != 1 || len(status.Criteria[0].ObservedEvidence) != 0 {
		t.Fatalf("status=%#v", status)
	}
	_, err = applyPlanUpdate(updated, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: "assistant", Reference: "claim-1", Provenance: EvidenceModelClaim}}}}})
	if err == nil || !strings.Contains(err.Error(), "observed evidence") {
		t.Fatalf("pass without observed evidence error=%v", err)
	}
	passed, err := applyPlanUpdate(updated, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Reference: "call-1", Provenance: EvidenceObserved}}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyPlanUpdate(passed, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptanceFailed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Reference: "call-2", Provenance: EvidenceObserved}}}}})
	if err == nil || !strings.Contains(err.Error(), "cannot transition backward") {
		t.Fatalf("passed regression error=%v", err)
	}
}

func TestPlanUpdateDefersNonBlockingQuestion(t *testing.T) {
	question := "Final visual tuning needs the user's eyes after a viable browser render exists."
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "render works", State: AcceptancePending})
	plan.Questions = []string{question}
	if !plan.NeedsClarification() {
		t.Fatal("plan question did not initially block mutations")
	}

	update := PlanUpdate{PlanID: plan.ID, QuestionUpdates: []PlanQuestionUpdate{{
		Question:    question,
		Disposition: PlanQuestionDeferred,
		Reason:      "This is post-implementation validation, not information required before implementation.",
	}}}
	updated, err := applyPlanUpdate(plan, update)
	if err != nil {
		t.Fatal(err)
	}
	if updated.NeedsClarification() || len(updated.Questions) != 0 || !reflect.DeepEqual(updated.DeferredValidations, []string{question}) {
		t.Fatalf("updated plan=%#v", updated)
	}
	if _, err := applyPlanUpdate(updated, update); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("repeated question update error=%v", err)
	}
	answered := plan
	answered.Questions = []string{"Which target should be used?"}
	answered, err = applyPlanUpdate(answered, PlanUpdate{PlanID: plan.ID, QuestionUpdates: []PlanQuestionUpdate{{Question: answered.Questions[0], Disposition: PlanQuestionResolved, Reason: "The user selected the current workspace."}}})
	if err != nil || answered.NeedsClarification() || len(answered.DeferredValidations) != 0 {
		t.Fatalf("resolved plan=%#v err=%v", answered, err)
	}
}

func TestPlanQuestionDispositionPersistsAppendOnly(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-question-disposition"})
	if err != nil {
		t.Fatal(err)
	}
	question := "Review the completed animation in a browser."
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "animation renders", State: AcceptancePending})
	plan.Questions = []string{question}
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	before := len(session.EventsSnapshot())
	update := PlanUpdate{PlanID: plan.ID, QuestionUpdates: []PlanQuestionUpdate{{Question: question, Disposition: PlanQuestionDeferred, Reason: "review follows implementation"}}}
	updated, sequence, err := store.UpdatePlan(session.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	if sequence == 0 || updated.NeedsClarification() || !reflect.DeepEqual(updated.DeferredValidations, []string{question}) {
		t.Fatalf("updated=%#v sequence=%d", updated, sequence)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.EventsSnapshot()) != before+1 {
		t.Fatalf("events=%d want=%d", len(loaded.EventsSnapshot()), before+1)
	}
	persisted, persistedSequence := latestPlanStateFromEvents(loaded.EventsSnapshot())
	if persisted == nil || persistedSequence != sequence || persisted.NeedsClarification() || !reflect.DeepEqual(persisted.DeferredValidations, []string{question}) {
		t.Fatalf("persisted=%#v sequence=%d", persisted, persistedSequence)
	}
}

func TestPlanUpdatePersistsAndIsSessionScoped(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(SessionMetadata{ID: "plan-session-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(SessionMetadata{ID: "plan-session-two"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := first.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := second.AppendPlan(trackedPlan(AcceptanceCriterion{ID: "other", Text: "other", State: AcceptancePending})); err != nil {
		t.Fatal(err)
	}
	appendSuccessfulPlanEvidence(t, first, "check-1")
	updated, sequence, err := store.UpdatePlan(first.ID, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{observedCriterionUpdate("check-1", AcceptancePassed)}})
	if err != nil {
		t.Fatal(err)
	}
	if sequence == 0 || updated.Criteria[0].State != AcceptancePassed {
		t.Fatalf("updated=%#v sequence=%d", updated, sequence)
	}
	loaded, err := store.Load(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, persistedSeq := latestPlanStateFromEvents(loaded.EventsSnapshot())
	if persisted == nil || persisted.Criteria[0].State != AcceptancePassed || persistedSeq != sequence {
		t.Fatalf("persisted=%#v sequence=%d", persisted, persistedSeq)
	}
	other, err := store.Load(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherPlan, _ := latestPlanStateFromEvents(other.EventsSnapshot())
	if otherPlan == nil || otherPlan.Criteria[0].State != AcceptancePending {
		t.Fatalf("session isolation failed: %#v", otherPlan)
	}
}

func TestPlanUpdateAtomicAcrossConcurrentSessions(t *testing.T) {
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-race"})
	if err != nil {
		t.Fatal(err)
	}
	criteria := make([]AcceptanceCriterion, 8)
	for index := range criteria {
		criteria[index] = AcceptanceCriterion{ID: "check-" + string(rune('a'+index)), Text: "criterion", State: AcceptancePending}
	}
	plan := trackedPlan(criteria...)
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, criterion := range criteria {
		appendSuccessfulPlanEvidence(t, session, criterion.ID)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(criteria))
	for _, criterion := range criteria {
		criterion := criterion
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, updateErr := store.UpdatePlan(session.ID, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{observedCriterionUpdate(criterion.ID, AcceptancePassed)}})
			if updateErr != nil {
				errs <- updateErr
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := latestPlanStateFromEvents(loaded.EventsSnapshot())
	for _, criterion := range current.Criteria {
		if criterion.State != AcceptancePassed || len(criterion.Evidence) != 1 || criterion.Evidence[0].Provenance != EvidenceObserved {
			t.Fatalf("criterion lost during concurrent updates: %#v", criterion)
		}
	}
}

func TestPlanTrackingCompositionBoundaries(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Assistant.RequirePlan = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: store, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPlanTool(app.Capabilities.ToolNames, PlanStatusToolName) || !containsPlanTool(app.Capabilities.ToolNames, PlanUpdateToolName) {
		t.Fatalf("trusted composition capabilities=%v", app.Capabilities.ToolNames)
	}
	provider := &fakeProvider{}
	app.Provider = provider
	runEvents, err := app.Run(context.Background(), RunRequest{Prompt: "show the current plan"})
	if err != nil {
		t.Fatal(err)
	}
	for range runEvents {
	}
	if len(provider.requests) != 1 || !requestHasTool(provider.requests[0].Tools, PlanStatusToolName) || !requestHasTool(provider.requests[0].Tools, PlanUpdateToolName) {
		t.Fatalf("trusted provider tools=%v", provider.requests)
	}
	locked, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: root, Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if containsPlanTool(locked.Capabilities.ToolNames, PlanStatusToolName) || containsPlanTool(locked.Capabilities.ToolNames, PlanUpdateToolName) {
		t.Fatalf("locked composition leaked plan tracking: %v", locked.Capabilities.ToolNames)
	}
	provider = &fakeProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{RequirePlan: true, ExposePlanTracking: false})
	if err != nil {
		t.Fatal(err)
	}
	request, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range request.Tools {
		if definition.Name == PlanStatusToolName || definition.Name == PlanUpdateToolName {
			t.Fatal("child/specialist-style agent exposed plan tracking")
		}
	}
	trackedAgent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{RequirePlan: true, ExposePlanTracking: true})
	if err != nil {
		t.Fatal(err)
	}
	trackedRequest, err := trackedAgent.request()
	if err != nil {
		t.Fatal(err)
	}
	if !requestHasTool(trackedRequest.Tools, PlanStatusToolName) || !requestHasTool(trackedRequest.Tools, PlanUpdateToolName) {
		t.Fatalf("tracked agent tools=%v", trackedRequest.Tools)
	}
}

func TestPlanTrackingToolStatusDoesNotLeakRawPlanText(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-status"})
	if err != nil {
		t.Fatal(err)
	}
	secretText := "prompt with token=should-not-appear"
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: secretText, State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{session: session, exposePlanTracking: true, plan: &plan}
	result := agent.executePlanTool(context.Background(), PlanStatusToolName, json.RawMessage(`{}`))
	if result.Error != "" || strings.Contains(result.Output, secretText) || strings.Contains(result.Output, "token=should-not-appear") {
		t.Fatalf("status leaked plan text: %#v", result)
	}
	var status PlanStatus
	if err := json.Unmarshal([]byte(result.Output), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Criteria) != 1 || status.Criteria[0].State != AcceptancePending {
		t.Fatalf("status=%#v", status)
	}
}

func TestPlanStatusRequiresDurableSessionForUpdate(t *testing.T) {
	agent := &Agent{exposePlanTracking: true, plan: func() *TaskPlan {
		plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "x", State: AcceptancePending})
		return &plan
	}()}
	result := agent.executePlanTool(context.Background(), PlanUpdateToolName, json.RawMessage(`{"plan_id":"tracked-plan","criteria":[{"id":"check-1","state":"in_progress"}]}`))
	if result.Error == "" || !strings.Contains(result.Error, "durable session") {
		t.Fatalf("error=%q", result.Error)
	}
	if errors.Is(errors.New(result.Error), ErrPlanRequired) {
		t.Fatal("plan update unexpectedly returned a mutation gate error")
	}
}

func TestPlanUpdateRejectsUnverifiedObservedEvidence(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-unverified"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.UpdatePlan(session.ID, PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{observedCriterionUpdate("check-1", AcceptancePassed)}})
	if err == nil || !strings.Contains(err.Error(), "does not reference a successful session tool result") {
		t.Fatalf("unverified observed evidence error=%v", err)
	}
}

func TestPlanUpdateRejectsFailedToolResultAndAcceptsArtifact(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-kinds"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-failed", ToolName: "test", Error: "failed"}); err != nil {
		t.Fatal(err)
	}
	failed := PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Reference: "call-failed", Provenance: EvidenceObserved}}}}}
	if _, _, err := store.UpdatePlan(session.ID, failed); err == nil || !strings.Contains(err.Error(), "successful session tool result") {
		t.Fatalf("failed tool result evidence error=%v", err)
	}
	artifact := Artifact{ID: "artifact-1", Filename: "result.txt", MediaType: "text/plain", Kind: "text", Size: 6, SHA256: strings.Repeat("a", 64), SessionID: session.ID}
	artifactEvent, err := NewArtifactEvent(session.ID, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Append(artifactEvent); err != nil {
		t.Fatal(err)
	}
	passed := PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceArtifact, Reference: artifact.ID, Provenance: EvidenceObserved}}}}}
	updated, _, err := store.UpdatePlan(session.ID, passed)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Criteria[0].State != AcceptancePassed {
		t.Fatalf("updated=%#v", updated)
	}
}

func TestPlanStatusListsRecentEligibleEvidenceWithoutPayloads(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-status"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, result := range []ToolResultEvent{
		{CallID: "call-exec-old", ToolName: "exec", Output: "private old output"},
		{CallID: "call-failed", ToolName: "exec", Error: "private failure"},
		{CallID: "call-status", ToolName: PlanStatusToolName, Output: "bookkeeping"},
		{CallID: "token=private-call-id", ToolName: "exec", Output: "unsafe reference"},
	} {
		if err := session.AppendToolResult(result); err != nil {
			t.Fatal(err)
		}
	}
	artifactEvent, err := NewArtifactEvent(session.ID, Artifact{ID: "artifact-evidence", SessionID: session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Append(artifactEvent); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-exec-new", ToolName: "exec", Output: "private new output"}); err != nil {
		t.Fatal(err)
	}

	agent := &Agent{session: session, exposePlanTracking: true, plan: &plan}
	result := agent.executePlanTool(context.Background(), PlanStatusToolName, json.RawMessage(`{}`))
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	var status PlanStatus
	if err := json.Unmarshal([]byte(result.Output), &status); err != nil {
		t.Fatal(err)
	}
	want := []PlanEvidenceCandidate{
		{Source: PlanEvidenceSourceToolResult, Reference: "call-exec-new", ToolName: "exec"},
		{Source: PlanEvidenceSourceArtifact, Reference: "artifact-evidence"},
		{Source: PlanEvidenceSourceToolResult, Reference: "call-exec-old", ToolName: "exec"},
	}
	if !reflect.DeepEqual(status.RecentEvidence, want) {
		t.Fatalf("recent evidence=%#v, want %#v", status.RecentEvidence, want)
	}
	if strings.Contains(result.Output, "private") || strings.Contains(result.Output, "call-failed") || strings.Contains(result.Output, "call-status") || strings.Contains(result.Output, "token=") {
		t.Fatalf("plan status leaked or advertised ineligible data: %s", result.Output)
	}
}

func TestPlanStatusBoundsRecentEvidence(t *testing.T) {
	index := newPlanEvidenceIndex()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-bounds"})
	if err != nil {
		t.Fatal(err)
	}
	for number := 0; number < MaxPlanEvidenceCandidates+5; number++ {
		event, eventErr := NewToolResultEvent(session.ID, ToolResultEvent{CallID: fmt.Sprintf("call-%02d", number), ToolName: "exec", Output: "ok"})
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		index.observe(event)
	}
	recent := index.recentCandidates(MaxPlanEvidenceCandidates)
	if len(recent) != MaxPlanEvidenceCandidates || recent[0].Reference != "call-16" || recent[len(recent)-1].Reference != "call-05" {
		t.Fatalf("bounded recent evidence=%#v", recent)
	}
}

func TestPlanUpdateResolvesLatestSuccessfulToolResultSelectorBeforePersistence(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-selector"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-old", ToolName: "exec", Output: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "call-new", ToolName: "exec", Output: "new"}); err != nil {
		t.Fatal(err)
	}
	update := PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{
		ID:    "check-1",
		State: AcceptancePassed,
		Evidence: []AcceptanceEvidence{{
			Source:     PlanEvidenceSourceToolResult,
			Selector:   &PlanEvidenceSelector{Kind: PlanEvidenceSelectorLatestSuccessfulToolResult, ToolName: "exec"},
			Provenance: EvidenceObserved,
		}},
	}}}
	updated, _, err := store.UpdatePlan(session.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	evidence := updated.Criteria[0].Evidence
	if len(evidence) != 1 || evidence[0].Reference != "call-new" || evidence[0].Selector != nil {
		t.Fatalf("resolved evidence=%#v", evidence)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range loaded.EventsSnapshot() {
		if event.Type != EventPlanUpdate {
			continue
		}
		payload, decodeErr := decodeEventPayload[PlanUpdateEvent](event, EventPlanUpdate)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		persisted := payload.Update.Criteria[0].Evidence[0]
		if persisted.Reference != "call-new" || persisted.Selector != nil {
			t.Fatalf("persisted unresolved selector: %#v", persisted)
		}
		return
	}
	t.Fatal("plan update event not persisted")
}

func TestPlanUpdateSelectorFailsClosedWithoutMatchingResult(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ID: "plan-evidence-selector-missing"})
	if err != nil {
		t.Fatal(err)
	}
	plan := trackedPlan(AcceptanceCriterion{ID: "check-1", Text: "verify", State: AcceptancePending})
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	update := PlanUpdate{PlanID: plan.ID, Criteria: []AcceptanceCriterionUpdate{{ID: "check-1", State: AcceptancePassed, Evidence: []AcceptanceEvidence{{Source: PlanEvidenceSourceToolResult, Selector: &PlanEvidenceSelector{Kind: PlanEvidenceSelectorLatestSuccessfulToolResult, ToolName: "exec"}, Provenance: EvidenceObserved}}}}}
	if _, _, err := store.UpdatePlan(session.ID, update); err == nil || !strings.Contains(err.Error(), "no successful same-session result") {
		t.Fatalf("missing selector result error=%v", err)
	}
}

func TestPlanUpdateSchemaAdvertisesEvidenceSelectorsAndQuestionDisposition(t *testing.T) {
	schema := string(planUpdateToolDefinition().Parameters)
	for _, want := range []string{`"source":{"type":"string","enum":["tool_result","artifact","assistant"]}`, `"latest_successful_tool_result"`, `"selector"`, `"question_updates"`, `"disposition":{"type":"string","enum":["resolved","deferred"]}`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("plan_update schema missing %s: %s", want, schema)
		}
	}
}

func TestPlanStatusRejectsUnexpectedArguments(t *testing.T) {
	agent := &Agent{exposePlanTracking: true}
	result := agent.executePlanTool(context.Background(), PlanStatusToolName, json.RawMessage(`{"unexpected":true}`))
	if result.Error == "" || !strings.Contains(result.Error, "unknown field") {
		t.Fatalf("result=%#v", result)
	}
}

func containsPlanTool(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func requestHasTool(definitions []ToolDefinition, wanted string) bool {
	for _, definition := range definitions {
		if definition.Name == wanted {
			return true
		}
	}
	return false
}
