package bee

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingMemoryExtractor struct{}

func (failingMemoryExtractor) Extract(context.Context, TurnMemoryInput) (MemoryExtractionResult, error) {
	return MemoryExtractionResult{}, errors.New("extractor unavailable")
}

type fixedMemoryExtractor struct {
	result MemoryExtractionResult
}

func (extractor fixedMemoryExtractor) Extract(context.Context, TurnMemoryInput) (MemoryExtractionResult, error) {
	return extractor.result, nil
}

func automationCandidate(category, content string) MemoryExtractionCandidate {
	return MemoryExtractionCandidate{
		Namespace:   MemoryNamespaceDurableMemory,
		Key:         "preference.answer_style",
		Content:     content,
		Summary:     "Answer style preference",
		Category:    category,
		Sensitivity: MemorySensitivityPersonal,
		Confidence:  0.98,
		Evidence:    MemoryEvidence{"I prefer concise answers"},
		Reason:      "explicit user preference",
	}
}

func TestParseMemoryExtractionRequiresStrictJSON(t *testing.T) {
	result, err := parseMemoryExtractionJSON([]byte(`{"candidates":[]}`), 4)
	if err != nil || len(result.Candidates) != 0 {
		t.Fatalf("valid extraction = %#v, err=%v", result, err)
	}
	if _, err := parseMemoryExtractionJSON([]byte(`{"candidates":[],"extra":true}`), 4); err == nil {
		t.Fatal("expected unknown extraction field rejection")
	}
	if _, err := parseMemoryExtractionJSON([]byte(`{"candidates":[]} trailing`), 4); err == nil {
		t.Fatal("expected trailing extraction data rejection")
	}
	if result, err := parseMemoryExtractionJSON([]byte(`{"candidates":[{"namespace":"durable_memory","key":"preference","content":"concise answers","category":"preference","sensitivity":"personal","confidence":0.95,"evidence":"I prefer concise answers"}]}`), 4); err != nil || len(result.Candidates) != 1 || len(result.Candidates[0].Evidence) != 1 {
		t.Fatalf("scalar evidence was not normalized: result=%#v err=%v", result, err)
	}
}

func TestMemoryExtractionTokenBudgetIsBoundedSeparatelyFromBytes(t *testing.T) {
	for _, test := range []struct {
		bytes int
		want  int
	}{
		{bytes: 0, want: 256},
		{bytes: 512, want: 256},
		{bytes: 4096, want: 1024},
		{bytes: 16 << 10, want: 2048},
		{bytes: 64 << 10, want: 2048},
	} {
		if got := memoryExtractionMaxTokens(test.bytes); got != test.want {
			t.Fatalf("maxOutputBytes=%d tokens=%d want %d", test.bytes, got, test.want)
		}
	}
}

func TestApplyMemoryExtractionSafeAutoSaveUsesParentRemember(t *testing.T) {
	service := newLifecycleTestService(t)
	input := TurnMemoryInput{RunID: "run-safe", SessionID: "session-safe", Prompt: "I prefer concise answers", Assistant: "Understood."}
	config := DefaultMemoryAutomationConfig()
	config.AutoSaveSafe = true
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), input, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{automationCandidate("preference", "concise technical answers")}}, config)
	if len(report.Saved) != 1 || len(report.Proposed) != 0 || report.Rejected != 0 {
		t.Fatalf("report = %#v", report)
	}
	list, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent()})
	if err != nil || len(list.Records) != 1 {
		t.Fatalf("saved records = %#v, err=%v", list, err)
	}
	if list.Records[0].Record.CreatedBy.Kind != MemoryActorParent {
		t.Fatalf("created_by = %#v", list.Records[0].Record.CreatedBy)
	}
}

func TestApplyMemoryExtractionIsIdempotentAcrossTurns(t *testing.T) {
	service := newLifecycleTestService(t)
	config := DefaultMemoryAutomationConfig()
	config.AutoSaveSafe = true
	candidate := automationCandidate("preference", "concise technical answers")
	first := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), TurnMemoryInput{RunID: "run-one", Prompt: "I prefer concise answers"}, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, config)
	second := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), TurnMemoryInput{RunID: "run-two", Prompt: "I prefer concise answers"}, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, config)
	if len(first.Saved) != 1 || len(second.Saved) != 0 || len(second.Unchanged) != 1 || len(second.Proposed) != 0 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	list, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent()})
	if err != nil || list.Total != 1 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

func TestApplyMemoryExtractionDoesNotDuplicateEquivalentProposal(t *testing.T) {
	service := newLifecycleTestService(t)
	candidate := automationCandidate("ambiguous", "concise technical answers")
	first := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), TurnMemoryInput{RunID: "run-one", Prompt: "I prefer concise answers"}, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, DefaultMemoryAutomationConfig())
	second := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), TurnMemoryInput{RunID: "run-two", Prompt: "I prefer concise answers"}, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, DefaultMemoryAutomationConfig())
	if len(first.Proposed) != 1 || len(second.Proposed) != 0 || len(second.Unchanged) != 1 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	pending, err := service.ListProposals(context.Background(), ListProposalRequest{Actor: testMemoryParent()})
	if err != nil || pending.Total != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func TestApplyMemoryExtractionNormalizesOmittedNamespaceAndPrefixedKey(t *testing.T) {
	service := newLifecycleTestService(t)
	input := TurnMemoryInput{RunID: "run-normalize", SessionID: "session-normalize", Prompt: "I prefer direct technical explanations", Assistant: "Understood."}
	config := DefaultMemoryAutomationConfig()
	config.AutoSaveSafe = true
	candidate := automationCandidate("preference", "direct technical explanations")
	candidate.Namespace = ""
	candidate.Key = "durable_memory/communication_style"
	candidate.Evidence = MemoryEvidence{"I prefer direct technical explanations"}
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), input, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, config)
	if len(report.Saved) != 1 || report.Rejected != 0 {
		t.Fatalf("report=%#v", report)
	}
}

func TestApplyMemoryExtractionProposalFallbackForAmbiguousCandidate(t *testing.T) {
	service := newLifecycleTestService(t)
	input := TurnMemoryInput{RunID: "run-proposal", Prompt: "I prefer concise answers", Assistant: "Noted."}
	candidate := automationCandidate("ambiguous", "concise technical answers")
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), input, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, DefaultMemoryAutomationConfig())
	if len(report.Saved) != 0 || len(report.Proposed) != 1 || report.Rejected != 0 {
		t.Fatalf("report = %#v", report)
	}
	pending, err := service.ListProposals(context.Background(), ListProposalRequest{Actor: testMemoryParent()})
	if err != nil || len(pending.Proposals) != 1 {
		t.Fatalf("pending = %#v, err=%v", pending, err)
	}
	if pending.Proposals[0].Proposal.ProposedBy.Kind != MemoryActorChild || pending.Proposals[0].Record.Record.CreatedBy.Kind != MemoryActorChild {
		t.Fatalf("proposal actors = %#v", pending.Proposals[0])
	}
}

func TestApplyMemoryExtractionRejectsSecretCandidate(t *testing.T) {
	service := newLifecycleTestService(t)
	input := TurnMemoryInput{RunID: "run-secret", Prompt: "I prefer concise answers", Assistant: "Okay."}
	candidate := automationCandidate("preference", "api_key=sk-secret-value")
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), input, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, DefaultMemoryAutomationConfig())
	if report.Rejected != 1 || len(report.Saved) != 0 || len(report.Proposed) != 0 {
		t.Fatalf("report = %#v", report)
	}
	list, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent()})
	if err != nil || len(list.Records) != 0 {
		t.Fatalf("secret was persisted: %#v, err=%v", list, err)
	}
}

func TestApplyMemoryExtractionDoesNotAutoSaveNegatedMemoryRequest(t *testing.T) {
	service := newLifecycleTestService(t)
	config := DefaultMemoryAutomationConfig()
	config.AutoSaveSafe = true
	candidate := automationCandidate("preference", "concise technical answers")
	candidate.Evidence = MemoryEvidence{"I prefer concise answers"}
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), TurnMemoryInput{RunID: "run-negated", Prompt: "Don't remember that I prefer concise answers"}, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{candidate}}, config)
	if len(report.Saved) != 0 || len(report.Proposed) != 0 || report.Rejected != 1 {
		t.Fatalf("report=%#v", report)
	}
}

func TestPostTurnMemoryProviderFailureDoesNotFailCompletedTurn(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	config.Memory.Automation.Enabled = true
	app, err := NewApplication(config, &fakeProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.MemoryExtractor = failingMemoryExtractor{}
	report := app.extractPostTurnMemory(context.Background(), TurnMemoryInput{RunID: "run-failure", Prompt: "I prefer concise answers", Assistant: "Done."})
	if !report.Attempted || !strings.Contains(report.Error, "unavailable") {
		t.Fatalf("report = %#v", report)
	}
}

func TestMemoryAutomationConfigDefaultsDisabledAndLocalChildRole(t *testing.T) {
	config := DefaultConfig()
	if config.Memory.Automation.Enabled || config.Memory.Automation.AutoSaveSafe {
		t.Fatalf("unsafe defaults = %#v", config.Memory.Automation)
	}
	if config.Memory.Automation.ProviderRole != ProviderRoleChild || config.Memory.Automation.TimeoutSeconds <= 0 {
		t.Fatalf("automation defaults = %#v", config.Memory.Automation)
	}
	if err := (MemoryAutomationConfig{ProviderRole: ProviderRoleManual}).Validate(); err != nil {
		t.Fatalf("manual role should be shape-valid and rejected at runtime: %v", err)
	}
}
