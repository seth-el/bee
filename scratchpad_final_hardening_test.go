package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScratchpadLegalNearBoundSnapshotAndEncodedOverhead(t *testing.T) {
	_, store, _, ctx := newScratchFixture(t)
	value := strings.Repeat("x", ScratchpadMaxValueBytes)
	for i := 0; i < ScratchpadMaxTotalBytes/ScratchpadMaxValueBytes; i++ {
		if _, err := store.Set(ctx, "near"+string(rune('a'+i)), value, time.Hour); err != nil {
			t.Fatalf("near-bound entry %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(store.Root(), "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > ScratchpadMaxSnapshotBytes {
		t.Fatalf("snapshot bytes=%d cap=%d", len(data), ScratchpadMaxSnapshotBytes)
	}
	var snap scratchpadSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != ScratchpadMaxTotalBytes/ScratchpadMaxValueBytes {
		t.Fatalf("entries=%d", len(snap.Entries))
	}
	if _, err := store.Set(ctx, "over", "x", time.Hour); err == nil {
		t.Fatal("over-bound entry accepted")
	}
}

func TestScratchpadUnexpiredReadsDoNotRewriteButExpiredReadsPrune(t *testing.T) {
	_, store, _, ctx := newScratchFixture(t)
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	if _, err := store.Set(ctx, "stable", "value", time.Hour); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), "fixture.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "stable"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("unexpired read rewrote snapshot")
	}
	now = now.Add(2 * time.Hour)
	if _, err := store.List(ctx); err != nil {
		t.Fatal(err)
	}
	pruned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(pruned) == string(after) {
		t.Fatal("expired access did not persist pruning")
	}
}

func TestScratchAndInspectorToolsReportMissingConfiguration(t *testing.T) {
	ctx := context.Background()
	if _, err := (ScratchSetTool{}).Execute(ctx, json.RawMessage(`{"key":"k","value":"v"}`)); err == nil {
		t.Fatal("nil scratch_set store accepted")
	}
	if _, err := (ScratchGetTool{}).Execute(ctx, json.RawMessage(`{"key":"k"}`)); err == nil {
		t.Fatal("nil scratch_get store accepted")
	}
	if _, err := (ScratchListTool{}).Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("nil scratch_list store accepted")
	}
	if _, err := (ScratchDeleteTool{}).Execute(ctx, json.RawMessage(`{"key":"k"}`)); err == nil {
		t.Fatal("nil scratch_delete store accepted")
	}
	if _, err := (ProcessListTool{}).Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("nil process inspector accepted")
	}
	if _, err := (ProcessInfoTool{}).Execute(ctx, json.RawMessage(`{"pid":1}`)); err == nil {
		t.Fatal("nil process_info inspector accepted")
	}
	if _, err := (ServiceListTool{}).Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("nil service inspector accepted")
	}
	if _, err := (ServiceStatusTool{}).Execute(ctx, json.RawMessage(`{"unit":"x.service"}`)); err == nil {
		t.Fatal("nil service_status inspector accepted")
	}
	if _, err := (ServiceLogsTool{}).Execute(ctx, json.RawMessage(`{"unit":"x.service"}`)); err == nil {
		t.Fatal("nil service_logs inspector accepted")
	}
}

type processOnlyInspector struct{ hugeInspector }

func (processOnlyInspector) ServiceInspectionAvailable() bool { return false }

func TestGeneralAssistantRegistersScratchAndAvailableInspectorTools(t *testing.T) {
	root := t.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := NewScratchpadStore(filepath.Join(root, "scratch"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(root, "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	compose := func(inspector SystemInspector) *Application {
		app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
			Store:           sessions,
			JobStore:        mustJobStore(t),
			ScopeRoot:       root,
			MemoryRoot:      filepath.Join(root, "memory"),
			ArtifactStore:   artifacts,
			ScratchpadStore: scratch,
			SystemInspector: inspector,
		})
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	app := compose(hugeInspector{})
	for _, name := range []string{"service_list", "service_status", "service_logs"} {
		if _, ok := app.Executor.Registry.Lookup(name); !ok {
			t.Fatalf("missing tool %s", name)
		}
	}
	// scratch_* and process_* are pruned from the foreground assistant.
	for _, name := range []string{"scratch_set", "scratch_get", "scratch_list", "scratch_delete", "process_list", "process_info"} {
		if _, ok := app.Executor.Registry.Lookup(name); ok {
			t.Fatalf("pruned tool %s is still model-facing", name)
		}
	}
	processOnly := compose(processOnlyInspector{})
	for _, name := range []string{"process_list", "process_info"} {
		if _, ok := processOnly.Executor.Registry.Lookup(name); ok {
			t.Fatalf("pruned process-only tool %s is still model-facing", name)
		}
	}
	for _, name := range []string{"service_list", "service_status", "service_logs"} {
		if _, ok := processOnly.Executor.Registry.Lookup(name); ok {
			t.Fatalf("unexpected unavailable service tool %s", name)
		}
	}
}
