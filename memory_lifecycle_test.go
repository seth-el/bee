package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newLifecycleTestService(t *testing.T) *LocalMemoryService {
	t.Helper()
	policy := localMemoryPolicy()
	service, err := NewLocalMemoryService(privateTempDir(t), policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func lifecycleRecord(id, key, content string) MemoryRecord {
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	record.ID, record.Key, record.Content, record.Summary = id, key, content, content
	record.CreatedAt = time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)
	record.UpdatedAt = record.CreatedAt
	return record
}

func TestLocalMemoryServiceLifecycleAndChildWriteRejection(t *testing.T) {
	service := newLifecycleTestService(t)
	ctx := context.Background()
	if _, err := service.Remember(ctx, RememberRequest{Actor: testMemoryChild(), Record: lifecycleRecord("memory-child", "pref.child", "no"), AuditID: "audit-child", Reason: "child write"}); err == nil || !errors.Is(err, ErrMemoryUnauthorized) {
		t.Fatalf("child remember = %v", err)
	}
	record, err := service.Remember(ctx, RememberRequest{Actor: testMemoryParent(), Record: lifecycleRecord("memory-one", "preference.theme", "concise"), AuditID: "audit-create", Reason: "user explicitly requested storage"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != 1 || record.CreatedBy.Kind != MemoryActorParent {
		t.Fatalf("record = %#v", record)
	}
	record.Content = "very concise"
	updated, err := service.Edit(ctx, EditMemoryRequest{Actor: testMemoryParent(), Record: record, ExpectedVersion: 1, AuditID: "audit-edit", Reason: "user corrected preference"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated = %#v", updated)
	}
	if err := service.EditConflictCheck(ctx, updated); err == nil {
		t.Fatal("expected stale conflict")
	}
	if err := service.Forget(ctx, ForgetRequest{Actor: testMemoryParent(), RecordID: updated.ID, ExpectedVersion: 2, AuditID: "audit-forget", Reason: "user asked to forget"}); err != nil {
		t.Fatal(err)
	}
	list, err := service.List(ctx, ListMemoryRequest{Actor: testMemoryParent()})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Records) != 0 {
		t.Fatalf("tombstone visible: %#v", list)
	}
}

func (service *LocalMemoryService) EditConflictCheck(ctx context.Context, record MemoryRecord) error {
	_, err := service.Edit(ctx, EditMemoryRequest{Actor: testMemoryParent(), Record: record, ExpectedVersion: 1, AuditID: "audit-stale", Reason: "stale edit"})
	return err
}

func TestLocalMemoryServiceProposalReviewAndParentTransformation(t *testing.T) {
	service := newLifecycleTestService(t)
	proposal := MemoryProposal{ID: "proposal-one", Record: lifecycleRecord("memory-proposed", "preference.answer", "concise"), ProposedBy: testMemoryChild(), Mode: MemoryWriteSuggested, Reason: "observed stable preference", AuditID: "child-audit", CreatedAt: time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)}
	proposal.Record.CreatedBy = testMemoryChild()
	if err := service.SubmitProposal(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	pending, err := service.ListProposals(context.Background(), ListProposalRequest{Actor: testMemoryParent()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Proposals) != 1 || pending.Proposals[0].Status != MemoryProposalStatus(MaxMemoryProposalStatus) {
		t.Fatalf("pending = %#v", pending)
	}
	record, err := service.ApproveProposal(context.Background(), ApproveProposalRequest{Actor: testMemoryParent(), ProposalID: proposal.ID, AuditID: "approve-one", Reason: "parent approved after review"})
	if err != nil {
		t.Fatal(err)
	}
	if record.CreatedBy.Kind != MemoryActorParent || record.ID != proposal.Record.ID {
		t.Fatalf("approved record = %#v", record)
	}
	pending, err = service.ListProposals(context.Background(), ListProposalRequest{Actor: testMemoryParent()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Proposals) != 0 {
		t.Fatalf("approved proposal remains pending: %#v", pending)
	}
	show, err := service.ShowProposal(context.Background(), ShowProposalRequest{Actor: testMemoryParent(), ProposalID: proposal.ID})
	if err != nil || show.Status != MemoryProposalStatus(MaxMemoryProposalApproved) {
		t.Fatalf("show = %#v, err=%v", show, err)
	}
}

func TestLocalMemoryServiceSensitiveRedactionAndDeterministicExportImport(t *testing.T) {
	policy := localMemoryPolicy()
	policy.AllowSensitiveMemory = true
	service, err := NewLocalMemoryService(privateTempDir(t), policy)
	if err != nil {
		t.Fatal(err)
	}
	record := lifecycleRecord("memory-sensitive", "profile.note", "private note")
	record.Sensitivity = MemorySensitivitySensitive
	if _, err := service.Remember(context.Background(), RememberRequest{Actor: testMemoryParent(), Record: record, AuditID: "audit-sensitive", Reason: "explicit sensitive storage"}); err != nil {
		t.Fatal(err)
	}
	record.Version = 1
	record.Content = "private note updated"
	if _, err := service.Edit(context.Background(), EditMemoryRequest{Actor: testMemoryParent(), Record: record, ExpectedVersion: 1, AuditID: "audit-sensitive-edit", Reason: "explicit correction"}); err != nil {
		t.Fatal(err)
	}
	child, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryChild()})
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Records) != 0 && (!child.Records[0].Redacted || child.Records[0].Record.Content != "") {
		t.Fatalf("child redaction = %#v", child)
	}
	parent, err := service.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent(), IncludeSensitive: true})
	if err != nil || len(parent.Records) != 1 || parent.Records[0].Record.Content != "private note updated" {
		t.Fatalf("parent sensitive = %#v, err=%v", parent, err)
	}
	export, err := service.Export(context.Background(), MemoryExportRequest{Actor: testMemoryParent(), IncludeSensitive: true, AuditID: "export-one", Reason: "backup"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := MemoryExportJSON(export)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MemoryExportJSON(export)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("export is not deterministic")
	}
	var decoded MemoryExport
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	other, err := NewLocalMemoryService(privateTempDir(t), service.policy)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := other.Import(context.Background(), MemoryImportRequest{Actor: testMemoryParent(), Payload: first, DryRun: true, AuditID: "import-preview", Reason: "preview"})
	if err != nil || !preview.Valid || preview.Applied {
		t.Fatalf("preview = %#v, err=%v", preview, err)
	}
	if result, err := other.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent(), IncludeSensitive: true}); err != nil || len(result.Records) != 0 {
		t.Fatalf("dry run mutated = %#v, err=%v", result, err)
	}
	applied, err := other.Import(context.Background(), MemoryImportRequest{Actor: testMemoryParent(), Payload: first, AuditID: "import-apply", Reason: "restore"})
	if err != nil || !applied.Applied {
		t.Fatalf("apply = %#v, err=%v", applied, err)
	}
	malformed := []byte(`{"format_version":1,"memory":[{"id":"bad"}]}`)
	before, _ := other.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent(), IncludeSensitive: true})
	if _, err := other.Import(context.Background(), MemoryImportRequest{Actor: testMemoryParent(), Payload: malformed, AuditID: "import-bad", Reason: "bad"}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("malformed import = %v", err)
	}
	after, _ := other.List(context.Background(), ListMemoryRequest{Actor: testMemoryParent(), IncludeSensitive: true})
	if len(before.Records) != len(after.Records) {
		t.Fatalf("malformed import mutated state: before=%#v after=%#v", before, after)
	}
}
