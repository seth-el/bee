package bee

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type scoutSequenceProvider struct {
	calls    int
	requests []Request
}

func (p *scoutSequenceProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

type fluentScoutProvider struct{}

func (fluentScoutProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

type emptyBriefAfterSearchProvider struct{ calls int }

func (provider *emptyBriefAfterSearchProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (provider *emptyBriefAfterSearchProvider) Stream(_ context.Context, _ Request, _ StreamFunc) (Response, error) {
	provider.calls++
	if provider.calls == 1 {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "search", Type: "function", Name: "web_search", Arguments: `{"query":"x"}`}}}}, nil
	}
	return Response{Message: Message{Role: RoleAssistant, Content: ""}}, nil
}
func (fluentScoutProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{Message: Message{Role: RoleAssistant, Content: "Answer: fluent but unsourced."}}, nil
}

type blockedSearchProvider struct {
	calls  int
	repeat bool
}

func (provider *blockedSearchProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (provider *blockedSearchProvider) Stream(_ context.Context, _ Request, _ StreamFunc) (Response, error) {
	provider.calls++
	switch provider.calls {
	case 1:
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "search-1", Type: "function", Name: "web_search", Arguments: `{"query":"x"}`}}}}, nil
	case 2:
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "search-2", Type: "function", Name: "web_search", Arguments: `{"query":"x"}`}}}}, nil
	case 3:
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "blocked", Type: "function", Name: "web_search", Arguments: `{"query":"x-again"}`}}}}, nil
	case 4:
		if provider.repeat {
			return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "blocked-again", Type: "function", Name: "web_search", Arguments: `{"query":"x-again"}`}}}}, nil
		}
		return Response{Message: Message{Role: RoleAssistant, Content: "Answer/Findings\nThe bounded search budget was reached; this is the final synthesis."}}, nil
	default:
		return Response{Message: Message{Role: RoleAssistant, Content: "Answer/Findings\nFinal synthesis."}}, nil
	}
}

type countingBlockingScoutProvider struct {
	mu    sync.Mutex
	calls int
}

func (provider *countingBlockingScoutProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (provider *countingBlockingScoutProvider) Stream(ctx context.Context, _ Request, _ StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.calls++
	provider.mu.Unlock()
	<-ctx.Done()
	return Response{}, ctx.Err()
}

func (provider *countingBlockingScoutProvider) Calls() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func newTestScoutService(t *testing.T, provider Provider) *WebResearchScoutService {
	t.Helper()
	config := DefaultConfig()
	config.Web.Enabled = true
	config.Web.SearchEndpoint = "http://127.0.0.1:9999/search"
	config.Web.Scout.MaxConcurrency = 1
	child := config
	child.ProviderRole = ProviderRoleChild
	child.ProviderProfile = "local-child-8082"
	child.Provider.BaseURL = "http://127.0.0.1:8082"
	child.Provider.Model = "muse-test"
	registry, err := NewRegistry(
		ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
			return `{"query":"bee","results":[],"retryable":true}`, nil
		}},
		ToolFunc{ToolDefinition: ToolDefinition{Name: "web_fetch", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
			return `{"final_url":"","title":"","content":""}`, nil
		}},
		ToolFunc{ToolDefinition: ToolDefinition{Name: BrowserToolName, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
			return `{"action":"close","ok":true}`, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Config: config, ChildConfig: child, ChildProvider: provider, ChildProviderProfile: "local-child-8082", ChildBroker: broker}
	service, err := NewWebResearchScoutService(app, &Executor{Registry: registry, Scope: ExecutionScope{Root: "."}}, config.Web.Scout)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func (p *scoutSequenceProvider) Stream(_ context.Context, request Request, _ StreamFunc) (Response, error) {
	p.calls++
	p.requests = append(p.requests, request)
	if p.calls == 1 {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "search", Type: "function", Name: "web_search", Arguments: `{"query":"bee"}`}}}}, nil
	}
	if p.calls == 2 {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "fetch", Type: "function", Name: "web_fetch", Arguments: `{"url":"https://example.com/one"}`}}}}, nil
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "Answer/Findings\nBee is a test.\n\nSources\n- One"}}, nil
}

func TestWebResearchScoutRunsBoundedChildAndGroundsSources(t *testing.T) {
	config := DefaultConfig()
	config.Web.Enabled = true
	config.Web.SearchEndpoint = "http://127.0.0.1:9999/search"
	config.Web.Scout.Enabled = true
	child := config
	child.ProviderRole = ProviderRoleChild
	child.ProviderProfile = "local-child-8082"
	child.Provider.BaseURL = "http://127.0.0.1:8082"
	child.Provider.Model = "muse-test"
	provider := &scoutSequenceProvider{}
	registry, err := NewRegistry(
		ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
			return `{"query":"bee","results":[{"title":"One","url":"https://example.com/one"}],"retryable":true}`, nil
		}},
		ToolFunc{ToolDefinition: ToolDefinition{Name: "web_fetch", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
			return `{"final_url":"https://example.com/one","title":"One","content":"ok"}`, nil
		}},
		ToolFunc{ToolDefinition: ToolDefinition{Name: BrowserToolName, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			return `{"action":"close","ok":true}`, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Config: config, ChildConfig: child, ChildProvider: provider, ChildProviderProfile: "local-child-8082", ChildBroker: broker}
	scout, err := NewWebResearchScoutService(app, &Executor{Registry: registry, Scope: ExecutionScope{Root: "."}}, config.Web.Scout)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := scout.Tool().Execute(context.Background(), json.RawMessage(`{"question":"What is Bee?"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebResearchResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || result.ToolCalls.Search != 1 || result.ToolCalls.Fetch != 1 || len(result.Sources) != 1 || result.Sources[0].URL != "https://example.com/one" || result.Brief == "" || result.ModelPhases != 2 {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.requests) < 4 || len(provider.requests[len(provider.requests)-1].Tools) != 0 {
		t.Fatalf("synthesis request unexpectedly had tools: calls=%d request=%#v", len(provider.requests), provider.requests[len(provider.requests)-1])
	}
	if !strings.Contains(provider.requests[len(provider.requests)-1].Messages[len(provider.requests[len(provider.requests)-1].Messages)-1].Content, "https://example.com/one") {
		t.Fatal("synthesis prompt omitted grounded source evidence")
	}
}

func TestWebResearchScoutRequiresGroundedSourceForCompleteStatus(t *testing.T) {
	service := newTestScoutService(t, fluentScoutProvider{})
	encoded, err := service.Tool().Execute(context.Background(), json.RawMessage(`{"question":"unsourced"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebResearchResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || len(result.Sources) != 0 || !containsString(result.Limitations, "no grounded sources were observed") {
		t.Fatalf("result=%#v", result)
	}
}

func TestWebResearchScoutRequiresBriefEvenWithGroundedSource(t *testing.T) {
	service := newTestScoutService(t, &emptyBriefAfterSearchProvider{})
	if err := service.raw.Registry.Unregister("web_search"); err != nil {
		t.Fatal(err)
	}
	if err := service.raw.Registry.Register(ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
		return `{"query":"x","results":[{"title":"One","url":"https://example.com/one"}],"retryable":true}`, nil
	}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := service.Tool().Execute(context.Background(), json.RawMessage(`{"question":"empty brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebResearchResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || len(result.Sources) != 1 || !containsString(result.Limitations, "scout did not produce a research brief") {
		t.Fatalf("result=%#v", result)
	}
}

func TestWebResearchScoutFirstBlockedSearchAllowsFinalSynthesis(t *testing.T) {
	provider := &blockedSearchProvider{}
	service := newTestScoutService(t, provider)
	searchCalls := 0
	if err := service.raw.Registry.Unregister("web_search"); err != nil {
		t.Fatal(err)
	}
	if err := service.raw.Registry.Register(ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
		searchCalls++
		return `{"query":"x","results":[{"title":"One","url":"https://example.com/one"}],"retryable":true}`, nil
	}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := service.Tool().Execute(context.Background(), json.RawMessage(`{"question":"blocked once"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebResearchResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if searchCalls != 2 || provider.calls != 4 || result.Brief == "" || !containsString(result.Limitations, "search request budget expired") {
		t.Fatalf("calls=%d provider=%d result=%#v", searchCalls, provider.calls, result)
	}
}

func TestWebResearchScoutRepeatedBlockedSearchFailsClosed(t *testing.T) {
	provider := &blockedSearchProvider{repeat: true}
	service := newTestScoutService(t, provider)
	searchCalls := 0
	if err := service.raw.Registry.Unregister("web_search"); err != nil {
		t.Fatal(err)
	}
	if err := service.raw.Registry.Register(ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
		searchCalls++
		return `{"query":"x","results":[],"retryable":true}`, nil
	}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := service.Tool().Execute(context.Background(), json.RawMessage(`{"question":"blocked twice"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebResearchResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if searchCalls != 2 || provider.calls != 4 || result.Status != "partial" || !containsString(result.Limitations, "search request budget expired") {
		t.Fatalf("calls=%d provider=%d result=%#v", searchCalls, provider.calls, result)
	}
}

func TestWebResearchScoutQueueWaitUsesOverallTimeout(t *testing.T) {
	provider := &countingBlockingScoutProvider{}
	service := newTestScoutService(t, provider)
	service.config.TimeoutSeconds = 1
	service.slots <- struct{}{}
	started := time.Now()
	done := make(chan string, 1)
	go func() {
		encoded, err := service.Tool().Execute(context.Background(), json.RawMessage(`{"question":"queued"}`))
		if err != nil {
			done <- err.Error()
			return
		}
		done <- encoded
	}()
	select {
	case encoded := <-done:
		var result WebResearchResult
		if err := json.Unmarshal([]byte(encoded), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "partial" || !containsString(result.Limitations, "scout time budget expired") {
			t.Fatalf("queued result=%#v", result)
		}
		if provider.Calls() != 0 {
			t.Fatalf("queued request reached provider: %d calls", provider.Calls())
		}
		if time.Since(started) > 2*time.Second {
			t.Fatalf("queue timeout took too long: %s", time.Since(started))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued scout did not time out")
	}
	<-service.slots
}
