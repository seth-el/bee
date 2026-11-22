package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeBrowserBackend struct {
	opened    []BrowserOpenRequest
	navigated []BrowserNavigateRequest
	snapshots []BrowserSnapshotRequest
	acted     []BrowserActRequest
	closed    []BrowserCloseRequest
}

func (fake *fakeBrowserBackend) Tabs(context.Context) (BrowserTabsResult, error) {
	return BrowserTabsResult{Running: true, Count: 1}, nil
}
func (fake *fakeBrowserBackend) Open(_ context.Context, request BrowserOpenRequest) (BrowserOpenResult, error) {
	fake.opened = append(fake.opened, request)
	return BrowserOpenResult{TargetID: "opaque-target-1", URL: request.URL, Title: "Shell"}, nil
}
func (fake *fakeBrowserBackend) Navigate(_ context.Context, request BrowserNavigateRequest) (BrowserNavigateResult, error) {
	fake.navigated = append(fake.navigated, request)
	return BrowserNavigateResult{TargetID: request.TargetID, URL: request.URL, Title: "Next"}, nil
}
func (fake *fakeBrowserBackend) Snapshot(_ context.Context, request BrowserSnapshotRequest) (BrowserSnapshotResult, error) {
	fake.snapshots = append(fake.snapshots, request)
	next := request.Offset + 2
	return BrowserSnapshotResult{TargetID: request.TargetID, URL: "http://127.0.0.1/next", Title: "Next", Format: request.Format, Text: "界面", Offset: request.Offset, TotalChars: 4, HasMore: request.Offset == 0, NextOffset: func() *int {
		if request.Offset == 0 {
			return &next
		}
		return nil
	}()}, nil
}
func (fake *fakeBrowserBackend) Act(_ context.Context, request BrowserActRequest) (BrowserActResult, error) {
	fake.acted = append(fake.acted, request)
	return BrowserActResult{}, nil
}
func (fake *fakeBrowserBackend) Close(_ context.Context, request BrowserCloseRequest) error {
	fake.closed = append(fake.closed, request)
	return nil
}

type staleBrowserBackend struct{ fakeBrowserBackend }

func (stale *staleBrowserBackend) Navigate(context.Context, BrowserNavigateRequest) (BrowserNavigateResult, error) {
	return BrowserNavigateResult{}, &CamofoxError{Status: 404, Class: "http status"}
}

type failingOpenBrowserBackend struct{ fakeBrowserBackend }

func (failing *failingOpenBrowserBackend) Open(context.Context, BrowserOpenRequest) (BrowserOpenResult, error) {
	return BrowserOpenResult{}, errors.New("dial failed with private target-id-should-not-leak")
}

func TestBrowserToolBindsTargetsAndNeverReturnsIDs(t *testing.T) {
	backend := &fakeBrowserBackend{}
	tool, err := NewBrowserTool(backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withArtifactContext(context.Background(), SessionID("session-a"), "run-a")
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/app"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "target") || strings.Contains(result, "native") {
		t.Fatalf("unsafe output=%s", result)
	}
	if _, err = tool.Execute(ctx, json.RawMessage(`{"action":"navigate","url":"http://127.0.0.1/next"}`)); err != nil {
		t.Fatal(err)
	}
	if len(backend.navigated) != 1 || backend.navigated[0].TargetID != "opaque-target-1" {
		t.Fatalf("binding=%#v", backend.navigated)
	}
	if _, err = tool.Execute(ctx, json.RawMessage(`{"action":"snapshot","offset":123}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = tool.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/other"}`)); err != nil {
		t.Fatal(err)
	}
	if len(backend.closed) != 1 {
		t.Fatalf("replacement close=%#v", backend.closed)
	}
}

func TestBrowserToolSessionIsolationAndStrictSchema(t *testing.T) {
	backend := &fakeBrowserBackend{}
	tool, _ := NewBrowserTool(backend)
	one := withArtifactContext(context.Background(), SessionID("one"), "run-one")
	two := withArtifactContext(context.Background(), SessionID("two"), "run-two")
	_, _ = tool.Execute(one, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/one"}`))
	_, _ = tool.Execute(two, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/two"}`))
	if len(backend.opened) != 2 || backend.opened[0].UserID == backend.opened[1].UserID {
		t.Fatalf("identities=%#v", backend.opened)
	}
	other, _ := NewBrowserTool(backend)
	_, _ = other.Execute(context.Background(), json.RawMessage(`{"action":"open","url":"http://127.0.0.1/fallback"}`))
	if len(backend.opened) != 3 || backend.opened[1].UserID == backend.opened[2].UserID {
		t.Fatalf("fallback identities=%#v", backend.opened)
	}
	if _, err := tool.Execute(one, json.RawMessage(`{"action":"snapshot","offset":123}`)); err != nil {
		t.Fatal(err)
	}
	if len(backend.snapshots) == 0 || backend.snapshots[len(backend.snapshots)-1].Offset != 123 {
		t.Fatalf("offset was not passed through: %#v", backend.snapshots)
	}
	if _, err := tool.Execute(one, json.RawMessage(`{"action":"snapshot","target_id":"bad"}`)); err == nil {
		t.Fatal("raw target accepted")
	}
	if _, err := tool.Execute(one, json.RawMessage(`{"action":"snapshot"}{"action":"close"}`)); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}

func TestBrowserOpenFailureIsNotMisreportedAsMissingPage(t *testing.T) {
	tool, _ := NewBrowserTool(&failingOpenBrowserBackend{})
	ctx := withArtifactContext(context.Background(), SessionID("open-failure"), "run-open-failure")
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/app"}`))
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(result)
	if !strings.Contains(lower, "backend unavailable") || strings.Contains(lower, "no browser page") || strings.Contains(lower, "target-id-should-not-leak") {
		t.Fatalf("open failure result=%s", result)
	}
}

func TestBrowserToolClearsStaleBindingAndSanitizesErrors(t *testing.T) {
	backend := &staleBrowserBackend{}
	tool, _ := NewBrowserTool(backend)
	ctx := withArtifactContext(context.Background(), SessionID("stale"), "run-stale")
	_, _ = tool.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/app"}`))
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"navigate","url":"http://127.0.0.1/next"}`))
	if err != nil || strings.Contains(result, "target") || !strings.Contains(result, "stale") {
		t.Fatalf("stale result=%s err=%v", result, err)
	}
	result, err = tool.Execute(ctx, json.RawMessage(`{"action":"snapshot"}`))
	if err != nil || !strings.Contains(strings.ToLower(result), "open") {
		t.Fatalf("stale binding not cleared: %s err=%v", result, err)
	}
}

func TestBrowserActRequiresExactFieldsAndRoutesTypeSafely(t *testing.T) {
	backend := &fakeBrowserBackend{}
	read, err := NewBrowserTool(backend)
	if err != nil {
		t.Fatal(err)
	}
	act := newBrowserActTool(backend, read.state)
	ctx := withArtifactContext(context.Background(), SessionID("act"), "run-act")
	_, _ = read.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/app"}`))
	if _, err := act.Execute(ctx, json.RawMessage(`{"action":"type","text":"hello"}`)); err == nil {
		t.Fatal("type without ref accepted")
	}
	if _, err := act.Execute(ctx, json.RawMessage(`{"action":"type","ref":"e1","text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if len(backend.acted) != 1 || backend.acted[0].Ref != "e1" || backend.acted[0].Text != "hello" {
		t.Fatalf("action=%#v", backend.acted)
	}
	if _, err := act.Execute(ctx, json.RawMessage(`{"action":"click","ref":"e1","extra":1}`)); err == nil {
		t.Fatal("extra field accepted")
	}
}

func TestWebResearchExecutorPlansOnlyMutatingBrowserTool(t *testing.T) {
	def := BrowserActTool{Backend: &fakeBrowserBackend{}}.Definition()
	gate := &PlanGate{}
	if err := gate.Check(def); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("act gate err=%v", err)
	}
	if err := gate.Check(BrowserTool{Backend: &fakeBrowserBackend{}}.Definition()); err != nil {
		t.Fatalf("read gate err=%v", err)
	}
	executor, err := NewWebResearchExecutor(WebConfig{Enabled: true, Browser: BrowserWebConfig{Enabled: true, CamofoxURL: "http://127.0.0.1:9377"}})
	if err != nil {
		t.Fatal(err)
	}
	act, ok := executor.Registry.Lookup(BrowserActToolName)
	if !ok || act.Definition().Mutability != ToolMutabilityMutating {
		t.Fatalf("act registration=%v", ok)
	}
	read, ok := executor.Registry.Lookup(BrowserToolName)
	if !ok || read.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("read registration=%v", ok)
	}
	if _, err := executor.Execute(context.Background(), BrowserActToolName, json.RawMessage(`{"action":"click","ref":"e1"}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("executor act gate err=%v", err)
	}
}
