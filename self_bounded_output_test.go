package bee

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type maliciousSelfBoundedTool struct{}

func (maliciousSelfBoundedTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "malicious_self_bounded", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (maliciousSelfBoundedTool) Execute(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("x", 32), nil
}
func (maliciousSelfBoundedTool) SelfBoundedOutputLimit() int { return 16 }

type ordinaryOutputTool struct{}

func (ordinaryOutputTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "ordinary_output", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (ordinaryOutputTool) Execute(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("x", 128), nil
}

func TestSelfBoundedOutputFailsClosedAndOrdinaryToolsStillUseScopeLimit(t *testing.T) {
	registry, err := NewRegistry(maliciousSelfBoundedTool{}, ordinaryOutputTool{})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir(), MaxOutputBytes: 64}}
	bad, err := executor.Execute(context.Background(), "malicious_self_bounded", json.RawMessage(`{}`))
	if err == nil || bad.Output != "" || !strings.Contains(bad.Error, "self-bounded") {
		t.Fatalf("malicious result=%#v err=%v", bad, err)
	}
	ordinary, err := executor.Execute(context.Background(), "ordinary_output", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ordinary.Truncated || len(ordinary.Output) > 64 {
		t.Fatalf("ordinary result=%#v", ordinary)
	}
	if errors.Is(err, ErrPlanRequired) {
		t.Fatal("ordinary tool unexpectedly plan gated")
	}
}

func TestMergedWebToolsBypassGeneric64KiBOnlyWithinSelfBound(t *testing.T) {
	body := strings.Repeat(`<>&"界`, 40000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/tabs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tabId":"tab-1","url":"http://example.test","title":"Example"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/snapshot" {
			w.Header().Set("Content-Type", "application/json")
			payload, _ := json.Marshal(map[string]any{"targetId": "tab-1", "snapshot": body, "offset": 0, "totalChars": len([]rune(body)), "hasMore": false})
			_, _ = w.Write(payload)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	web, err := NewWebResearchExecutor(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5, Browser: BrowserWebConfig{Enabled: true, CamofoxURL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := web.Registry.Clone()
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir(), MaxOutputBytes: 64 << 10}, Policy: web.Policy, planGate: &PlanGate{}}
	fetched, err := executor.Execute(context.Background(), "web_fetch", json.RawMessage(`{"url":"`+server.URL+`/fetch","max_chars":131072}`))
	if err != nil {
		t.Fatal(err)
	}
	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(fetched.Output), &fetchResult); err != nil {
		t.Fatalf("fetch invalid JSON: %v output=%d", err, len(fetched.Output))
	}
	if fetched.Truncated || len(fetched.Output) > MaxWebToolOutputBytes || len([]rune(fetchResult.Content)) == 0 {
		t.Fatalf("fetch result=%#v truncated=%v", fetchResult, fetched.Truncated)
	}
	opened, err := executor.Execute(context.Background(), BrowserToolName, json.RawMessage(`{"action":"open","url":"http://example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if opened.Truncated {
		t.Fatalf("open generically truncated: %#v", opened)
	}
	snapshot, err := executor.Execute(context.Background(), BrowserToolName, json.RawMessage(`{"action":"snapshot","offset":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var browserResult map[string]any
	if err := json.Unmarshal([]byte(snapshot.Output), &browserResult); err != nil {
		t.Fatalf("browser invalid JSON: %v output=%d", err, len(snapshot.Output))
	}
	if snapshot.Truncated || len(snapshot.Output) > MaxBrowserToolOutputBytes || browserResult["content"] == nil {
		t.Fatalf("browser result=%#v truncated=%v", browserResult, snapshot.Truncated)
	}
}
