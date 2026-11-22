package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestMemoryCLIImportDryRunApplySearchAndConflict(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(t.TempDir(), "memory-export.json")
	policy := bee.DefaultMemoryPolicy()
	source, err := bee.NewLocalMemoryService(filepath.Join(root, "source"), policy)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := source.Remember(context.Background(), bee.RememberRequest{
		Actor:   bee.MemoryActor{Kind: bee.MemoryActorParent, ID: "cli-test"},
		Record:  bee.MemoryRecord{ID: "memory-1", Namespace: bee.MemoryNamespaceDurableMemory, Key: "preference", Content: "The owner prefers careful migrations", Sensitivity: bee.MemorySensitivityPublic, CreatedAt: now, UpdatedAt: now, Confidence: 0.9},
		AuditID: "remember-1",
		Reason:  "CLI acceptance fixture",
	}); err != nil {
		t.Fatal(err)
	}
	export, err := source.Export(context.Background(), bee.MemoryExportRequest{Actor: bee.MemoryActor{Kind: bee.MemoryActorParent, ID: "cli-test"}, AuditID: "export-1", Reason: "CLI acceptance export"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := bee.MemoryExportJSON(export)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(root, "destination")
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"import", "--memory-root", destination, "--source", exportPath, "--audit-id", "import-preview", "--reason", "preview"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var preview bee.MemoryImportResult
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil || !preview.DryRun || preview.Applied || preview.Memory != 1 {
		t.Fatalf("preview=%#v err=%v output=%q", preview, err, output.String())
	}
	if _, err := os.Stat(filepath.Join(destination, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dry-run mutated destination: %v", err)
	}

	output.Reset()
	if err := runMemory([]string{"import", "--memory-root", destination, "--source", exportPath, "--apply", "--audit-id", "import-apply", "--reason", "apply"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var applied bee.MemoryImportResult
	if err := json.Unmarshal(output.Bytes(), &applied); err != nil || !applied.Applied || applied.DryRun {
		t.Fatalf("applied=%#v err=%v output=%q", applied, err, output.String())
	}

	output.Reset()
	if err := runMemory([]string{"search", "--memory-root", destination, "--query", "careful migrations"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "The owner prefers careful migrations") {
		t.Fatalf("search output=%q", output.String())
	}
	if err := runMemory([]string{"import", "--memory-root", destination, "--source", exportPath, "--apply", "--audit-id", "import-conflict", "--reason", "conflict"}, &output, &errorOutput); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected import conflict, err=%v", err)
	}
}

func TestMemoryCLIRejectsSecretPayload(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "secret.json")
	payload := `{"format_version":1,"memory":[{"id":"secret","namespace":"durable_memory","key":"credential","content":"api_key=do-not-store","sensitivity":"public","created_at":"2026-08-18T00:00:00Z","updated_at":"2026-08-18T00:00:00Z","confidence":1,"version":1,"created_by":{"kind":"parent","id":"source"}}]}`
	if err := os.WriteFile(source, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"import", "--memory-root", root, "--source", source, "--audit-id", "secret-import", "--reason", "test"}, &output, &errorOutput); err == nil {
		t.Fatal("secret payload accepted")
	}
}

func TestMemoryCLIDoctorFindsUnimportedSourceCorpus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "legacy.md"), []byte("# Old memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"doctor", "--memory-root", root, "--semantic-index", filepath.Join(t.TempDir(), "missing-index")}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var report memoryDoctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SourceFiles != 1 || report.Records != 0 || len(report.Warnings) == 0 || report.Semantic.Code != bee.SemanticHealthMissing {
		t.Fatalf("report=%#v", report)
	}
}

func TestMemoryCLIProposalReviewLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	service, err := bee.NewLocalMemoryService(root, bee.DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	child := bee.MemoryActor{Kind: bee.MemoryActorChild, ID: "extractor"}
	proposal := bee.MemoryProposal{
		ID: "proposal-cli", ProposedBy: child, Mode: bee.MemoryWriteSuggested,
		AuditID: "proposal-cli-audit", Reason: "review fixture", CreatedAt: now,
		Record: bee.MemoryRecord{ID: "proposal-memory", Namespace: bee.MemoryNamespaceDurableMemory, Key: "preference.cli", Content: "Prefer CLI review", Sensitivity: bee.MemorySensitivityPublic, CreatedAt: now, UpdatedAt: now, Confidence: 0.9, Version: 1, CreatedBy: child},
	}
	if err := service.SubmitProposal(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"proposals", "list", "--memory-root", root}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var pending bee.MemoryProposalListResult
	if err := json.Unmarshal(output.Bytes(), &pending); err != nil || pending.Total != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	output.Reset()
	if err := runMemory([]string{"proposals", "approve", "--memory-root", root, "--id", proposal.ID, "--audit-id", "approve-cli", "--reason", "confirmed"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runMemory([]string{"list", "--memory-root", root}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var list bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &list); err != nil || list.Total != 1 || list.Records[0].Record.ID != "proposal-memory" {
		t.Fatalf("list=%#v err=%v", list, err)
	}
}

func TestMemoryCLIReadPolicyOverridesAreReadOnlyAndCompositional(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	policy := bee.DefaultMemoryPolicy()
	policy.AllowSensitiveMemory = true
	service, err := bee.NewLocalMemoryService(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Add(-90 * 24 * time.Hour)
	for _, record := range []bee.MemoryRecord{
		{ID: "old-public", Namespace: bee.MemoryNamespaceDurableMemory, Key: "old-note", Content: "old public note", Sensitivity: bee.MemorySensitivityPublic, CreatedAt: createdAt, UpdatedAt: createdAt, Confidence: 1},
		{ID: "old-personal", Namespace: bee.MemoryNamespaceDurableMemory, Key: "old-note", Content: "old personal note", Sensitivity: bee.MemorySensitivityPersonal, CreatedAt: createdAt, UpdatedAt: createdAt, Confidence: 1},
		{ID: "old-sensitive", Namespace: bee.MemoryNamespaceDurableMemory, Key: "old-note", Content: "old sensitive note", Sensitivity: bee.MemorySensitivitySensitive, CreatedAt: createdAt, UpdatedAt: createdAt, Confidence: 1},
	} {
		if _, err := service.Remember(context.Background(), bee.RememberRequest{Actor: bee.MemoryActor{Kind: bee.MemoryActorParent, ID: "fixture"}, Record: record, AuditID: "remember-" + record.ID, Reason: "CLI read policy fixture"}); err != nil {
			t.Fatal(err)
		}
	}

	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"list", "--memory-root", root}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var defaultList bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &defaultList); err != nil {
		t.Fatal(err)
	}
	if defaultList.Total != 2 {
		t.Fatalf("default list should retain old non-sensitive records: %#v", defaultList)
	}
	output.Reset()
	if err := runMemory([]string{"list", "--memory-root", root, "--include-sensitive"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var sensitiveOnlyList bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &sensitiveOnlyList); err != nil {
		t.Fatal(err)
	}
	if sensitiveOnlyList.Total != 3 {
		t.Fatalf("include-sensitive list should retain old records: %#v", sensitiveOnlyList)
	}

	output.Reset()
	if err := runMemory([]string{"list", "--memory-root", root, "--retention-days", "30", "--include-sensitive"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var explicitlyExpired bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &explicitlyExpired); err != nil {
		t.Fatal(err)
	}
	if explicitlyExpired.Total != 0 {
		t.Fatalf("explicit age retention was not enforced: %#v", explicitlyExpired)
	}

	output.Reset()
	if err := runMemory([]string{"list", "--memory-root", root, "--retention-days", "0"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var retainedList bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &retainedList); err != nil {
		t.Fatal(err)
	}
	if retainedList.Total != 2 {
		t.Fatalf("retention-only list = %#v; sensitive record should remain excluded", retainedList)
	}

	output.Reset()
	if err := runMemory([]string{"list", "--memory-root", root, "--retention-days", "0", "--include-sensitive"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var unrestrictedList bee.MemoryListResult
	if err := json.Unmarshal(output.Bytes(), &unrestrictedList); err != nil {
		t.Fatal(err)
	}
	if unrestrictedList.Total != 3 {
		t.Fatalf("combined read policy list = %#v", unrestrictedList)
	}

	output.Reset()
	if err := runMemory([]string{"search", "--memory-root", root, "--query", "old sensitive note", "--retention-days", "0"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var retentionSearch bee.MemorySearchResult
	if err := json.Unmarshal(output.Bytes(), &retentionSearch); err != nil {
		t.Fatal(err)
	}
	for _, hit := range retentionSearch.Hits {
		if hit.Record.Record.ID == "old-sensitive" {
			t.Fatalf("retention-only search exposed sensitive record: %#v", retentionSearch)
		}
	}

	output.Reset()
	if err := runMemory([]string{"search", "--memory-root", root, "--query", "old sensitive note", "--retention-days", "0", "--include-sensitive"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var unrestrictedSearch bee.MemorySearchResult
	if err := json.Unmarshal(output.Bytes(), &unrestrictedSearch); err != nil {
		t.Fatal(err)
	}
	foundSensitive := false
	for _, hit := range unrestrictedSearch.Hits {
		if hit.Record.Record.ID == "old-sensitive" && hit.Record.Record.Content == "old sensitive note" {
			foundSensitive = true
		}
	}
	if !foundSensitive {
		t.Fatalf("combined read policy search = %#v", unrestrictedSearch)
	}

	output.Reset()
	if err := runMemory([]string{"show", "--memory-root", root, "--id", "old-sensitive", "--retention-days", "0"}, &output, &errorOutput); err == nil {
		t.Fatal("retention-only show exposed sensitive record")
	}
	output.Reset()
	if err := runMemory([]string{"show", "--memory-root", root, "--id", "old-sensitive", "--retention-days", "0", "--include-sensitive"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var unrestrictedShow bee.MemoryDisplayRecord
	if err := json.Unmarshal(output.Bytes(), &unrestrictedShow); err != nil || unrestrictedShow.Record.Content != "old sensitive note" || unrestrictedShow.Redacted {
		t.Fatalf("combined read policy show = %#v err=%v", unrestrictedShow, err)
	}

	if err := runMemory([]string{"import", "--memory-root", root, "--retention-days", "0", "--source", "missing", "--audit-id", "x", "--reason", "test"}, &output, &errorOutput); err == nil {
		t.Fatal("read-only flag accepted on mutating import path")
	}
	if err := runMemory([]string{"list", "--memory-root", root, "--retention-days", "-1"}, &output, &errorOutput); err == nil {
		t.Fatal("negative retention-days accepted")
	}
}

func TestMemoryCLISourceArchiveDryRunAndApply(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "MEMORY.md"), []byte("# Bee memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"source-archive", "--root", "memory=" + source, "--name", "cli-test", "--dry-run"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var preview bee.MemorySourceArchiveReport
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil || !preview.DryRun || preview.Applied {
		t.Fatalf("preview=%#v err=%v output=%q", preview, err, output.String())
	}
	if _, err := os.Stat(preview.Destination); !os.IsNotExist(err) {
		t.Fatalf("dry-run created destination: %v", err)
	}

	output.Reset()
	if err := runMemory([]string{"source-archive", "--root", "memory=" + source, "--name", "cli-test"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var applied bee.MemorySourceArchiveReport
	if err := json.Unmarshal(output.Bytes(), &applied); err != nil || !applied.Applied || applied.DryRun || len(applied.Manifest.Files) != 1 {
		t.Fatalf("applied=%#v err=%v output=%q", applied, err, output.String())
	}
}

func TestMemoryCLISourceArchiveExclusions(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	source := t.TempDir()
	for relative, data := range map[string][]byte{
		"MEMORY.md":                      []byte("keep\n"),
		".dreams/short-term-recall.json": []byte(strings.Repeat("dream\n", 1024)),
		"topics/skip.md":                 []byte("skip\n"),
	} {
		path := filepath.Join(source, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"source-archive", "--root", "memory=" + source, "--name", "cli-exclusions", "--exclude", "topics/*.md", "--max-file-bytes", "64", "--dry-run"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var preview bee.MemorySourceArchiveReport
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Manifest.Included != 1 || preview.Manifest.Excluded != 2 {
		t.Fatalf("preview=%#v output=%q", preview, output.String())
	}
	if len(preview.Manifest.ExcludedFiles) != 2 {
		t.Fatalf("excluded=%#v", preview.Manifest.ExcludedFiles)
	}
}

func TestMemoryCLIMoodSweepReportsCountsWithoutProse(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const prose = "quietly relieved after repairing the misunderstanding"
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	delta := `<!-- bee:mood-delta at="` + at + `" decay="short" -->` + "\n" + prose + "\n<!-- /bee:mood-delta -->\n"
	if err := os.WriteFile(filepath.Join(root, time.Now().UTC().Format("2006-01-02")+".md"), []byte(delta), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"mood-sweep", "--memory-root", root}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var report bee.MoodConsolidationReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Merged != 1 || !report.Updated || report.ActiveEntries != 1 {
		t.Fatalf("report=%+v output=%q", report, output.String())
	}
	if strings.Contains(output.String(), prose) || strings.Contains(errorOutput.String(), prose) {
		t.Fatalf("mood prose leaked in CLI output: stdout=%q stderr=%q", output.String(), errorOutput.String())
	}
}
