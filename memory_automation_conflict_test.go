package bee

import (
	"context"
	"testing"
)

func TestApplyMemoryExtractionConflictingKeyUsesProposal(t *testing.T) {
	service := newLifecycleTestService(t)
	if _, err := service.Remember(context.Background(), RememberRequest{Actor: testMemoryParent(), Record: lifecycleRecord("existing-memory", "preference.answer_style", "brief"), AuditID: "existing-audit", Reason: "existing preference"}); err != nil {
		t.Fatal(err)
	}
	input := TurnMemoryInput{RunID: "run-conflict", Prompt: "I prefer concise answers", Assistant: "Noted."}
	config := DefaultMemoryAutomationConfig()
	config.AutoSaveSafe = true
	report := ApplyMemoryExtraction(context.Background(), service, testMemoryParent(), localMemoryPolicy(), input, MemoryExtractionResult{Candidates: []MemoryExtractionCandidate{automationCandidate("preference", "concise technical answers")}}, config)
	if len(report.Saved) != 0 || len(report.Proposed) != 1 || report.Rejected != 0 {
		t.Fatalf("report = %#v", report)
	}
}
