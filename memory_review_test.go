package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMemoryReviewRequiresExplicitDecisions(t *testing.T) {
	candidatePath := writeTestCandidates(t, []MemoryCandidate{testReviewCandidate("candidate-a", "first")})
	normalized, report, err := NormalizeMemoryReview(candidatePath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 0 || report.Unreviewed != 1 || report.ReadyForM4 != 0 {
		t.Fatalf("normalized=%#v report=%#v", normalized, report)
	}
}

func TestNormalizeMemoryReviewAcceptsExplicitReviewedCandidate(t *testing.T) {
	candidate := testReviewCandidate("candidate-a", "current provider routing")
	candidatePath := writeTestCandidates(t, []MemoryCandidate{candidate})
	decisionPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	decision := MemoryReviewDecision{DecisionID: "decision-a", CandidateID: candidate.CandidateID, Action: MemoryReviewAccept, Reviewer: "alex", Reason: "confirmed current", ReviewedAt: time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC), Target: MemoryReviewTargetDurableMemory, Namespace: "durable_memory/projects/bee", Key: "provider-routing", Summary: "Current provider routing", EffectiveAt: time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC), LastConfirmedAt: time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC), Sensitivity: MemorySensitivityPersonal, Confidence: 0.98}
	if err := AppendMemoryReviewDecision(decisionPath, decision); err != nil {
		t.Fatal(err)
	}
	normalized, report, err := NormalizeMemoryReview(candidatePath, decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || report.ReadyForM4 != 1 || report.InvalidDecisions != 0 {
		t.Fatalf("normalized=%#v report=%#v", normalized, report)
	}
	if normalized[0].Namespace != decision.Namespace || normalized[0].Key != decision.Key || normalized[0].Status != "active" {
		t.Fatalf("normalized=%#v", normalized[0])
	}
}

func TestNormalizeMemoryReviewRejectsMissingTargetAndSecret(t *testing.T) {
	candidate := testReviewCandidate("candidate-a", "safe content")
	candidatePath := writeTestCandidates(t, []MemoryCandidate{candidate})
	decisionPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	decision := MemoryReviewDecision{DecisionID: "decision-a", CandidateID: candidate.CandidateID, Action: MemoryReviewAccept, Reviewer: "alex", Reason: "reviewed", ReviewedAt: time.Now().UTC()}
	if err := AppendMemoryReviewDecision(decisionPath, decision); err != nil {
		t.Fatal(err)
	}
	_, report, err := NormalizeMemoryReview(candidatePath, decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.InvalidDecisions != 1 || report.ReadyForM4 != 0 {
		t.Fatalf("report=%#v", report)
	}
	bad := candidate
	bad.CandidateID = "candidate-secret"
	bad.Content = "api_key = sk-abcdefghijklmnopqrstuvwxyz123456"
	badPath := writeTestCandidates(t, []MemoryCandidate{bad})
	_, badReport, err := NormalizeMemoryReview(badPath, "")
	if err != nil || badReport.CandidateCount != 1 || badReport.Unreviewed != 1 {
		t.Fatalf("secret candidate report=%#v error=%v", badReport, err)
	}
	loaded, _, err := LoadMemoryCandidates(badPath)
	if err != nil || len(loaded) != 1 || loaded[0].Content != "" || loaded[0].Status != MemoryCandidateRejected {
		t.Fatalf("sanitized candidate=%#v error=%v", loaded, err)
	}
}

func TestNormalizeMemoryReviewDetectsDuplicateFinalKeysAndSupersession(t *testing.T) {
	first := testReviewCandidate("candidate-a", "old routing")
	second := testReviewCandidate("candidate-b", "new routing")
	candidatePath := writeTestCandidates(t, []MemoryCandidate{first, second})
	decisionPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	when := time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC)
	decisions := []MemoryReviewDecision{
		{DecisionID: "decision-a", CandidateID: first.CandidateID, Action: MemoryReviewSuperseded, Reviewer: "alex", Reason: "newer decision supersedes old", ReviewedAt: when, Target: MemoryReviewTargetDurableMemory, Namespace: "durable_memory/projects/bee", Key: "provider-routing", EffectiveAt: when.Add(-time.Hour), LastConfirmedAt: when, Sensitivity: MemorySensitivityPersonal, Confidence: 0.8, ReplacementCandidateID: second.CandidateID},
		{DecisionID: "decision-b", CandidateID: second.CandidateID, Action: MemoryReviewAccept, Reviewer: "alex", Reason: "confirmed newer decision", ReviewedAt: when, Target: MemoryReviewTargetDurableMemory, Namespace: "durable_memory/projects/bee", Key: "provider-routing", EffectiveAt: when, LastConfirmedAt: when, Sensitivity: MemorySensitivityPersonal, Confidence: 0.99},
	}
	for _, decision := range decisions {
		if err := AppendMemoryReviewDecision(decisionPath, decision); err != nil {
			t.Fatal(err)
		}
	}
	normalized, report, err := NormalizeMemoryReview(candidatePath, decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpenConflicts != 0 || report.DuplicateFinalKeys != 0 || len(normalized) != 2 {
		t.Fatalf("normalized=%#v report=%#v", normalized, report)
	}
	if normalized[0].SupersededBy == "" && normalized[1].SupersededBy == "" {
		t.Fatalf("missing supersession=%#v", normalized)
	}
	invalid := decisions[0]
	invalid.ReplacementCandidateID = "candidate-missing"
	badPath := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := AppendMemoryReviewDecision(badPath, invalid); err != nil {
		t.Fatal(err)
	}
}

func TestWriteReviewArtifactsAndAppendDecisionRefuseOverwrite(t *testing.T) {
	root := t.TempDir()
	decisionPath := filepath.Join(root, "decisions.jsonl")
	decision := MemoryReviewDecision{DecisionID: "decision-a", CandidateID: "candidate-a", Action: MemoryReviewReject, Reviewer: "alex", Reason: "not useful", ReviewedAt: time.Now().UTC()}
	if err := AppendMemoryReviewDecision(decisionPath, decision); err != nil {
		t.Fatal(err)
	}
	if err := AppendMemoryReviewDecision(decisionPath, decision); err == nil {
		t.Fatal("duplicate decision accepted")
	}
	output, reportPath := filepath.Join(root, "normalized.jsonl"), filepath.Join(root, "report.json")
	if err := WriteMemoryReviewArtifacts(output, reportPath, nil, MemoryReviewReport{FormatVersion: MemoryReviewFormatVersion}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{decisionPath, output, reportPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", path, info.Mode().Perm())
		}
	}
	if err := WriteMemoryReviewArtifacts(output, reportPath, nil, MemoryReviewReport{}); err == nil {
		t.Fatal("review artifact overwrite accepted")
	}
}

func testReviewCandidate(id, content string) MemoryCandidate {
	return MemoryCandidate{CandidateID: id, Status: MemoryCandidateCandidate, Category: CandidateCategoryProjectDecision, NamespaceSuggestion: "durable_memory/projects/bee", KeySuggestion: "provider-routing/decision", SummarySuggestion: "Provider routing", Content: content, SensitivitySuggestion: MemorySensitivityPersonal, ConfidenceSuggestion: 0.8, Source: MemoryCandidateSource{InventoryID: "inventory-test", RootLabel: "memory", RelativePath: "MEMORY.md", FileSHA256: strings.Repeat("a", 64), ContentSHA256: strings.Repeat("b", 64), LineStart: 1, LineEnd: 2}}
}

func writeTestCandidates(t *testing.T, candidates []MemoryCandidate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidates.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, candidate := range candidates {
		if err := encoder.Encode(candidate); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
