package bee

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeGeneralAssistantMergedWebOutputsStayStructured(t *testing.T) {
	body := strings.Repeat(`<>&"界`, 40000)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer control.Close()
	config := DefaultConfig()
	config.Web.Enabled = true
	config.Web.SearchEndpoint = control.URL
	config.Web.Browser.Enabled = true
	config.Web.Browser.CamofoxURL = control.URL
	root := t.TempDir()
	store, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := NewJobStore(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: store, JobStore: jobs, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, app.agentOptions(nil, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := agent.executor.Execute(context.Background(), "web_fetch", json.RawMessage(`{"url":"`+control.URL+`/fetch","max_chars":131072}`))
	if err != nil {
		t.Fatal(err)
	}
	var fetchResult WebFetchResult
	if err := json.Unmarshal([]byte(fetched.Output), &fetchResult); err != nil {
		t.Fatalf("fetch invalid JSON: %v", err)
	}
	if fetched.Truncated || len(fetched.Output) > MaxWebToolOutputBytes {
		t.Fatalf("fetch truncated=%v bytes=%d", fetched.Truncated, len(fetched.Output))
	}
	if _, err := agent.executor.Execute(context.Background(), BrowserToolName, json.RawMessage(`{"action":"open","url":"http://example.test"}`)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := agent.executor.Execute(context.Background(), BrowserToolName, json.RawMessage(`{"action":"snapshot","offset":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var browserResult map[string]any
	if err := json.Unmarshal([]byte(snapshot.Output), &browserResult); err != nil {
		t.Fatalf("browser invalid JSON: %v", err)
	}
	if snapshot.Truncated || len(snapshot.Output) > MaxBrowserToolOutputBytes {
		t.Fatalf("browser truncated=%v bytes=%d", snapshot.Truncated, len(snapshot.Output))
	}
}

func TestComposeGeneralAssistantBrowserCompositionPlanBoundary(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"running":true,"tabs":[]}`))
	}))
	defer control.Close()
	config := DefaultConfig()
	config.Assistant.RequirePlan = true
	config.Web.Enabled = true
	config.Web.SearchEndpoint = control.URL
	config.Web.Browser.Enabled = true
	config.Web.Browser.CamofoxURL = control.URL
	root := t.TempDir()
	store, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := NewJobStore(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: store, JobStore: jobs, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	options := app.agentOptions(nil, nil, "")
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, options)
	if err != nil {
		t.Fatal(err)
	}
	read, ok := agent.executor.Registry.Lookup(BrowserToolName)
	if !ok || read.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("read browser=%v", ok)
	}
	act, ok := agent.executor.Registry.Lookup(BrowserActToolName)
	if !ok || act.Definition().Mutability != ToolMutabilityMutating {
		t.Fatalf("act browser=%v", ok)
	}
	if _, err := agent.executor.Execute(context.Background(), BrowserActToolName, json.RawMessage(`{"action":"click","ref":"e1"}`)); err == nil || !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("act was not plan gated: %v", err)
	}
	if err := agent.executor.SetPlan(TaskPlan{ID: "browser-plan", Goal: "inspect page", Inputs: []string{"page"}, Outputs: []string{"observation"}, Steps: []string{"open", "click"}, Acceptance: []string{"observation returned"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.executor.Execute(context.Background(), BrowserActToolName, json.RawMessage(`{"action":"click","ref":"e1"}`)); err == nil || errors.Is(err, ErrPlanRequired) {
		t.Fatalf("act remained plan blocked after plan: %v", err)
	}
}
