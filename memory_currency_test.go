package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecalledMemoryShowsDateAndStatus(t *testing.T) {
	policy := DefaultMemoryPolicy()
	record := MemoryRecord{UpdatedAt: time.Date(2026, 2, 3, 23, 0, 0, 0, time.FixedZone("x", -5*3600)), Status: MemoryStatusHistorical}
	updated, status := memoryContextProvenance(record)
	if updated != "2026-02-04" || status != MemoryStatusHistorical {
		t.Fatalf("provenance = %q %q", updated, status)
	}
	if _, status := memoryContextProvenance(MemoryRecord{}); status != "" {
		t.Fatalf("active record status = %q", status)
	}
	packet := MemoryContextPacket{Items: []MemoryContextItem{
		{RecordID: "old", Namespace: MemoryNamespaceDurableMemory, Key: "k1", Content: "The assistant runs on host-a", Sensitivity: MemorySensitivityPublic, Updated: updated, Status: status},
		{RecordID: "new", Namespace: MemoryNamespaceDurableMemory, Key: "k2", Content: "the agent runs on testhost", Sensitivity: MemorySensitivityPublic, Updated: "2026-11-01"},
	}}
	rendered, err := renderMemoryContext(packet, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"updated=2026-02-04 status=historical]", "updated=2026-11-01]", "may no longer be true"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered packet lacks %q:\n%s", want, rendered)
		}
	}
	packet.Items[0].Updated = "last tuesday"
	if _, err := renderMemoryContext(packet, policy); err == nil {
		t.Fatal("invalid updated date rendered")
	}
}

func TestMemoryEditStatusPatch(t *testing.T) {
	record := MemoryRecord{}
	if err := applyMemoryFieldPatch(&record, map[string]any{"status": "historical"}); err != nil || record.Status != MemoryStatusHistorical {
		t.Fatalf("historical patch: %v %q", err, record.Status)
	}
	if err := applyMemoryFieldPatch(&record, map[string]any{"status": "active"}); err != nil || record.Status != MemoryStatusActive {
		t.Fatalf("active patch: %v %q", err, record.Status)
	}
	if err := applyMemoryFieldPatch(&record, map[string]any{"status": "superseded"}); err == nil {
		t.Fatal("edit marked a record superseded without a replacement")
	}
	superseded := MemoryRecord{Status: MemoryStatusSuperseded, SupersededBy: "x"}
	if err := applyMemoryFieldPatch(&superseded, map[string]any{"status": "active"}); err == nil {
		t.Fatal("edit revived a superseded record")
	}
}

func TestExplicitRecallIncludesHistoricalButNeverSuperseded(t *testing.T) {
	hit := func(id string, status MemoryStatus) MemoryDisplayHit {
		record := MemoryRecord{ID: id, Namespace: MemoryNamespaceDurableMemory, Key: id, Content: "fact " + id, Sensitivity: MemorySensitivityPublic, Status: status, UpdatedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
		if status == MemoryStatusSuperseded {
			record.SupersededBy = "current"
		}
		return MemoryDisplayHit{Record: MemoryDisplayRecord{Record: record}, Score: MemoryScore{Total: 0.9, Lexical: 0.9}}
	}
	provider := &fakeProvider{}
	memory := &runtimeContextMemoryService{result: MemorySearchResult{Hits: []MemoryDisplayHit{hit("current", ""), hit("past", MemoryStatusHistorical), hit("replaced", MemoryStatusSuperseded)}}}
	config := DefaultConfig()
	config.Memory.RecallMode = MemoryRecallHybrid
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{MemoryService: memory, MemoryFailure: MemoryFailureOptional})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "Do you remember where the server lived?", nil); err != nil {
		t.Fatal(err)
	}
	if len(memory.queries) != 1 || memory.queries[0].CurrentOnly {
		t.Fatalf("explicit recall query = %#v", memory.queries)
	}
	text := ContextText(ContextResult{Messages: provider.requests[0].Messages})
	if !strings.Contains(text, "fact current") || !strings.Contains(text, "fact past") || !strings.Contains(text, "status=historical") {
		t.Fatalf("explicit recall lacks current or labelled historical record:\n%s", text)
	}
	if strings.Contains(text, "fact replaced") {
		t.Fatalf("explicit recall injected a superseded record:\n%s", text)
	}
}

// The dreaming job retires stale records with memory_edit. Under the job's
// composition that must run without an approval (which would pause the job),
// and must leave the daily notes' read-only contract untouched.
func TestDreamingJobRetiresRecordWithoutApproval(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	memory := t.TempDir()
	if err := os.Chmod(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	topics := filepath.Join(memory, "topics")
	if err := os.MkdirAll(topics, 0o700); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store: mustAssistantStore(t), ScopeRoots: []string{topics, memory}, WriteRoots: []string{topics},
		MemoryRoot: memory, Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := MemoryActor{Kind: MemoryActorParent, ID: "seed"}
	now := time.Now().UTC()
	seeded, err := app.MemoryService.Remember(context.Background(), RememberRequest{Actor: actor, Mode: MemoryWriteExplicit, AuditID: "seed-1", Reason: "seed",
		Record: MemoryRecord{ID: "memory-old-home", Namespace: MemoryNamespaceDurableMemory, Key: "old-home", Content: "The assistant runs on host-a", Sensitivity: MemorySensitivityPublic, CreatedAt: now, UpdatedAt: now, Confidence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RestrictTools([]string{"conversation_search", "list_files", "read_file", "grep", "write_file", "edit_file", "memory_search", "memory_show", "memory_edit", "memory_supersede"}); err != nil {
		t.Fatal(err)
	}
	args := `{"record_id":"` + seeded.ID + `","expected_version":1,"reason":"moved to testhost","fields":{"status":"historical"}}`
	app.Provider = &goldenScriptedProvider{responses: []GoldenProviderResponse{
		{ToolCalls: []ToolCall{{ID: "m1", Type: "function", Name: "memory_edit", Arguments: args}}},
		{Content: "done"},
	}}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "consolidate", DisableCommands: true})
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if event.Type == RunApproval && event.Approval != nil && event.Approval.EffectiveStatus() != ApprovalStatusApproved {
			t.Fatalf("memory_edit needed an approval: %#v", event.Approval)
		}
	}
	shown, err := app.MemoryService.Show(context.Background(), ShowMemoryRequest{Actor: actor, RecordID: seeded.ID})
	if err != nil {
		t.Fatal(err)
	}
	if shown.Record.Status != MemoryStatusHistorical || !shown.Record.UpdatedAt.Equal(seeded.UpdatedAt) || shown.Record.CreatedBy != seeded.CreatedBy {
		t.Fatalf("record after retire: status %q updated %v creator %#v", shown.Record.Status, shown.Record.UpdatedAt, shown.Record.CreatedBy)
	}
	current, err := app.MemoryService.Search(context.Background(), SearchMemoryRequest{Query: MemoryQuery{Actor: actor, Text: "host-a", CurrentOnly: true}})
	if err != nil || len(current.Hits) != 0 {
		t.Fatalf("historical record still in current-only search: %d hits, %v", len(current.Hits), err)
	}
}
