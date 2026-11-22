package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportNormalizedDryRunIsSideEffectFree(t *testing.T) {
	item := testNormalizedImportItem("candidate-a", "decision-a")
	normalizedPath, reportPath := writeNormalizedImportFixtures(t, []MemoryNormalizedCandidate{item})
	memoryRoot := filepath.Join(t.TempDir(), "new-memory-root")
	before, _ := os.Stat(memoryRoot)
	result, err := ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: memoryRoot, AuditID: "m4-dry-run", Reason: "test dry run"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.Applied || result.RecordsReady != 1 || result.RecordsImported != 0 {
		t.Fatalf("result=%#v", result)
	}
	if _, statErr := os.Stat(memoryRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dry-run created memory root: %v before=%v", statErr, before)
	}
}

func TestImportNormalizedApplyWritesParentOwnedProvenanceRecord(t *testing.T) {
	item := testNormalizedImportItem("candidate-a", "decision-a")
	normalizedPath, reportPath := writeNormalizedImportFixtures(t, []MemoryNormalizedCandidate{item})
	memoryRoot := filepath.Join(t.TempDir(), "memory")
	result, err := ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: memoryRoot, AuditID: "m4-apply", Reason: "explicit test import", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || !result.Applied || result.RecordsImported != 1 {
		t.Fatalf("result=%#v", result)
	}
	policy := DefaultMemoryPolicy()
	policy.Retention = 0
	store, err := NewLocalMemoryStore(memoryRoot, policy)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ListRecords(context.Background(), MemoryActor{Kind: MemoryActorParent, ID: "reader"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].CreatedBy.ID != NormalizedImportActorID || records[0].Provenance == nil {
		t.Fatalf("records=%#v", records)
	}
	if records[0].Provenance.CandidateID != item.CandidateID || records[0].Provenance.LineStart != item.Source.LineStart {
		t.Fatalf("provenance=%#v", records[0].Provenance)
	}
}

func TestImportNormalizedRefusesExistingIDOrKey(t *testing.T) {
	item := testNormalizedImportItem("candidate-a", "decision-a")
	normalizedPath, reportPath := writeNormalizedImportFixtures(t, []MemoryNormalizedCandidate{item})
	memoryRoot := filepath.Join(t.TempDir(), "memory")
	first, err := ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: memoryRoot, AuditID: "m4-first", Reason: "first import", Apply: true})
	if err != nil || !first.Applied {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: memoryRoot, AuditID: "m4-second", Reason: "second import", Apply: true})
	if err == nil || second.Valid || len(second.Conflicts) == 0 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestImportNormalizedRejectsUnsupportedTargetsAndBadAudit(t *testing.T) {
	item := testNormalizedImportItem("candidate-profile", "decision-profile")
	item.Target = MemoryReviewTargetUserProfile
	normalizedPath, reportPath := writeNormalizedImportFixtures(t, []MemoryNormalizedCandidate{item})
	_, err := ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: filepath.Join(t.TempDir(), "memory"), AuditID: "m4-profile", Reason: "test"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unsupported target err=%v", err)
	}
	item = testNormalizedImportItem("candidate-bad-audit", "decision-bad-audit")
	normalizedPath, reportPath = writeNormalizedImportFixtures(t, []MemoryNormalizedCandidate{item})
	_, err = ImportNormalizedMemory(context.Background(), NormalizedImportOptions{NormalizedPath: normalizedPath, ReviewReportPath: reportPath, MemoryRoot: filepath.Join(t.TempDir(), "memory"), AuditID: "", Reason: "test"})
	if err == nil {
		t.Fatal("missing audit accepted")
	}
}

func testNormalizedImportItem(candidateID, decisionID string) MemoryNormalizedCandidate {
	when := time.Date(2026, 8, 19, 16, 0, 0, 0, time.UTC)
	return MemoryNormalizedCandidate{CandidateID: candidateID, DecisionID: decisionID, Target: MemoryReviewTargetDurableMemory, Namespace: "durable_memory/projects/bee", Key: "provider-routing-" + candidateID, Summary: "Provider routing", Content: "8081 is main and the cloud profile is manual.", Sensitivity: MemorySensitivityPersonal, Confidence: 0.98, Status: "active", EffectiveAt: when.Add(-time.Hour), LastConfirmedAt: when, Source: MemoryCandidateSource{InventoryID: "inventory-test", RootLabel: "memory", RelativePath: "MEMORY.md", FileSHA256: strings.Repeat("a", 64), ContentSHA256: strings.Repeat("b", 64), LineStart: 10, LineEnd: 14}}
}

func writeNormalizedImportFixtures(t *testing.T, items []MemoryNormalizedCandidate) (string, string) {
	t.Helper()
	root := t.TempDir()
	normalizedPath := filepath.Join(root, "normalized.jsonl")
	file, err := os.OpenFile(normalizedPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, item := range items {
		if err := encoder.Encode(item); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(normalizedPath)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	// The M4 report only needs to authorize the normalized artifact's record
	// count and carry a review batch identity; the normalized loader verifies
	// the artifact hash independently for its report.
	report := MemoryReviewReport{FormatVersion: MemoryReviewFormatVersion, ReviewBatchID: "review-fixture", CandidateArtifactSHA: hex.EncodeToString(hash[:]), CandidateCount: len(items), DecisionCount: len(items), Accepted: len(items), ReadyForM4: len(items)}
	reportPath := filepath.Join(root, "review-report.json")
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return normalizedPath, reportPath
}
