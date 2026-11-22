package bee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractMemoryCandidatesPreservesMarkdownProvenance(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "MEMORY.md"), "# Bee\n\n## Routing\n8081 is main.\n\n### Rationale\nNo automatic fallback.\n")
	manifestPath := writeTestInventory(t, root, "memory")
	candidates, report, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath, SourceTypes: []string{"curated_long_term"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesExtracted != 1 || report.SectionsExtracted != 3 || len(candidates) != 3 {
		t.Fatalf("report=%#v candidates=%#v", report, candidates)
	}
	var routing, rationale MemoryCandidate
	for _, candidate := range candidates {
		switch candidate.Source.LineStart {
		case 3:
			routing = candidate
		case 6:
			rationale = candidate
		}
	}
	if routing.Source.LineEnd != 4 || len(routing.Source.HeadingPath) != 2 || routing.Source.HeadingPath[1] != "Routing" {
		t.Fatalf("routing=%#v", routing)
	}
	if rationale.Source.LineEnd != 7 || len(rationale.Source.HeadingPath) != 3 || rationale.Source.HeadingPath[2] != "Rationale" {
		t.Fatalf("rationale=%#v", rationale)
	}
	if routing.Content == "" || routing.Source.FileSHA256 == "" || routing.Source.ContentSHA256 == "" {
		t.Fatalf("missing provenance/content=%#v", routing)
	}
}

func TestExtractMemoryCandidatesRejectsStaleInventory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "MEMORY.md")
	mustWriteInventoryFile(t, path, "# Original\n")
	manifestPath := writeTestInventory(t, root, "memory")
	mustWriteInventoryFile(t, path, "# Changed\n")
	candidates, report, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath, SourceTypes: []string{"curated_long_term"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 || report.FilesStale != 1 || report.FilesExtracted != 0 {
		t.Fatalf("report=%#v candidates=%#v", report, candidates)
	}
}

func TestExtractMemoryCandidatesDeduplicatesAndDoesNotLeakRejectedSecret(t *testing.T) {
	root := t.TempDir()
	secret := "api_key = sk-abcdefghijklmnopqrstuvwxyz123456"
	mustWriteInventoryFile(t, filepath.Join(root, "a.md"), "# One\nSame section\n")
	mustWriteInventoryFile(t, filepath.Join(root, "b.md"), "# Two\nSame section\n")
	mustWriteInventoryFile(t, filepath.Join(root, "c.md"), "# Secret\n"+secret+"\n")
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: root}}})
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(root, "c.md")
	secretData, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(secretData)
	for index := range manifest.Files {
		if manifest.Files[index].RelativePath == "c.md" {
			manifest.Files[index].Status = "included"
			manifest.Files[index].SHA256 = hex.EncodeToString(hash[:])
		}
	}
	manifestPath := filepath.Join(t.TempDir(), "inventory.json")
	if err := WriteMemoryInventory(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	candidates, report, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath, SourceTypes: []string{"source_file"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesSelected != 3 {
		t.Fatalf("selected report=%#v", report)
	}
	if report.Duplicates != 1 || report.Rejected != 1 {
		t.Fatalf("report=%#v", report)
	}
	data, _ := json.Marshal(candidates)
	if strings.Contains(string(data), secret) {
		t.Fatal("rejected secret leaked into candidate artifact")
	}
	var duplicate, rejected bool
	for _, candidate := range candidates {
		duplicate = duplicate || candidate.Status == MemoryCandidateDuplicate
		rejected = rejected || candidate.Status == MemoryCandidateRejected
	}
	if !duplicate || !rejected {
		t.Fatalf("candidates=%#v", candidates)
	}
}

func TestExtractMemoryCandidatesFlagsVolatileTodoAndArchive(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "2026-08-19.md"), "# Current\nTODO: verify endpoint 203.0.113.10:8081\n")
	mustWriteInventoryFile(t, filepath.Join(root, "archive", "old.md"), "# Historical\nFormer plan was superseded.\n")
	manifestPath := writeTestInventory(t, root, "memory")
	withoutArchive, _, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutArchive) != 1 {
		t.Fatalf("without archive=%#v", withoutArchive)
	}
	withArchive, report, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath, IncludeArchives: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withArchive) != 2 || report.ReviewRequired < 2 {
		t.Fatalf("report=%#v candidates=%#v", report, withArchive)
	}
	for _, candidate := range withArchive {
		if candidate.Status != MemoryCandidateReviewRequired {
			t.Fatalf("candidate=%#v", candidate)
		}
	}
}

func TestWriteMemoryCandidatesRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	candidatePath := filepath.Join(root, "out", "candidates.jsonl")
	reportPath := filepath.Join(root, "out", "report.json")
	if err := WriteMemoryCandidates(candidatePath, reportPath, []MemoryCandidate{{CandidateID: "candidate-test"}}, MemoryCandidateReport{FormatVersion: MemoryCandidateFormatVersion}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{candidatePath, reportPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", path, info.Mode().Perm())
		}
	}
	if err := WriteMemoryCandidates(candidatePath, reportPath, nil, MemoryCandidateReport{}); err == nil {
		t.Fatal("overwrite accepted")
	}
}

func writeTestInventory(t *testing.T, root, label string) string {
	t.Helper()
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: label, Path: root}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := WriteMemoryInventory(path, manifest); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractMemoryCandidatesDefaultExcludesDerivedLayers(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "MEMORY.md"), "# Dashboard\n\n## Active\nDreamer conclusion.\n")
	mustWriteInventoryFile(t, filepath.Join(root, "topics", "infra.md"), "# Infra\n\n## Router\nDreamer topic summary.\n")
	mustWriteInventoryFile(t, filepath.Join(root, "2026-11-11.md"), "# Daily\n\n## Router\nWhat actually happened.\n")
	manifestPath := writeTestInventory(t, root, "memory")
	candidates, _, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("daily notes were not extracted by default")
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate.Content, "Dreamer") || candidate.Source.RelativePath != "2026-11-11.md" {
			t.Fatalf("default extraction absorbed a derived layer: %#v", candidate)
		}
	}
	// Derived layers remain importable only by explicit operator choice.
	explicit, _, err := ExtractMemoryCandidates(MemoryCandidateOptions{InventoryPath: manifestPath, SourceTypes: []string{"curated_long_term", "curated_topic"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit) == 0 {
		t.Fatal("explicit derived-layer extraction returned nothing")
	}
}
