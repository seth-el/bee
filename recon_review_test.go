package bee

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type reconReviewProvider struct {
	requests []Request
	response string
}

func (provider *reconReviewProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	return Response{Message: Message{Role: RoleAssistant, Content: provider.response}}, nil
}

func (provider *reconReviewProvider) Stream(_ context.Context, _ Request, _ StreamFunc) (Response, error) {
	return Response{}, nil
}

func TestReconReviewPropagatesBoundedEvidenceWithNoToolsAndOneCall(t *testing.T) {
	child := &reconReviewProvider{response: "unsupported claim: verify the listener before relying on port 8082"}
	app := &Application{ChildProvider: child, ChildConfig: Config{Provider: ProviderConfig{Model: "gemma-child"}}}
	service, err := NewReconReviewService(app)
	if err != nil {
		t.Fatal(err)
	}
	tool := ReconReviewTool{Service: service}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"check the local service","draft":"The service is healthy on port 8082.","evidence":["provider.go uses port 8082","the health probe was not run"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(child.requests) != 1 {
		t.Fatalf("child request count=%d, want exactly one", len(child.requests))
	}
	request := child.requests[0]
	if len(request.Tools) != 0 {
		t.Fatalf("child tools=%#v, want none", request.Tools)
	}
	if request.Model != "gemma-child" || len(request.Messages) != 2 {
		t.Fatalf("child request routing/messages=%#v", request)
	}
	prompt := request.Messages[1].Content
	for _, expected := range []string{"check the local service", "The service is healthy on port 8082.", "provider.go uses port 8082", "the health probe was not run"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("child prompt missing %q: %s", expected, prompt)
		}
	}
	if !strings.Contains(output, "unsupported claim") {
		t.Fatalf("tool output=%q", output)
	}
}

func TestReconReviewRejectsUnknownAndOversizedArguments(t *testing.T) {
	child := &reconReviewProvider{response: "ok"}
	app := &Application{ChildProvider: child, ChildConfig: Config{Provider: ProviderConfig{Model: "gemma-child"}}}
	service, err := NewReconReviewService(app)
	if err != nil {
		t.Fatal(err)
	}
	tool := ReconReviewTool{Service: service}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"task","draft":"draft","unexpected":true}`)); err == nil {
		t.Fatal("unknown argument was accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"task","draft":"`+strings.Repeat("x", MaxReconReviewDraftBytes+1)+`"}`)); err == nil {
		t.Fatal("oversized draft was accepted")
	}
	if len(child.requests) != 0 {
		t.Fatalf("invalid requests reached child: %d", len(child.requests))
	}
}

func TestReconReviewBoundsChildOutputAndTimeout(t *testing.T) {
	child := &reconReviewProvider{response: strings.Repeat("x", MaxReconReviewOutputBytes+128)}
	app := &Application{ChildProvider: child, ChildConfig: Config{Provider: ProviderConfig{Model: "gemma-child"}}}
	service, err := NewReconReviewService(app)
	if err != nil {
		t.Fatal(err)
	}
	service.Timeout = time.Hour
	if got := service.ExecutionTimeout(); got != MaxReconReviewTimeout {
		t.Fatalf("execution timeout=%s, want %s", got, MaxReconReviewTimeout)
	}
	output, err := service.Review(context.Background(), ReconReviewRequest{Task: "task", Draft: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Critique  string `json:"critique"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if len(output) > MaxReconReviewOutputBytes || len(result.Critique) >= MaxReconReviewOutputBytes || !result.Truncated {
		t.Fatalf("bounded result=%#v", result)
	}
}

func TestComposeGeneralAssistantRegistersReconReviewWhenChildAvailable(t *testing.T) {
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: mustJobStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := app.Executor.Registry.Lookup(ReconReviewToolName)
	if !ok {
		t.Fatal("recon_review was not registered")
	}
	definition := tool.Definition()
	if definition.Mutability != ToolMutabilityReadOnly || definition.Risk != RiskLow {
		t.Fatalf("definition=%#v", definition)
	}
	if app.ReconReview == nil {
		t.Fatal("application did not retain recon review service")
	}
}

func TestDefaultSystemPromptGuidesExplicitReconReview(t *testing.T) {
	for _, expected := range []string{"multi-step read-only recon", ReconReviewToolName, "stale ports or paths"} {
		if !strings.Contains(DefaultSystemPrompt, expected) {
			t.Fatalf("default prompt missing %q", expected)
		}
	}
}
