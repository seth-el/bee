package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type toolLoopProvider struct {
	requests      []Request
	calls         int
	alwaysTool    bool
	toolResponses int
}

func (provider *toolLoopProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (provider *toolLoopProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	provider.calls++
	if provider.alwaysTool || provider.calls == 1 || (provider.toolResponses > 0 && provider.calls <= provider.toolResponses) {
		return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: fmt.Sprintf("call-%d", provider.calls), Type: "function", Name: "echo", Arguments: fmt.Sprintf(`{"call":%d}`, provider.calls)}}}}, nil
	}
	if onDelta != nil {
		if err := onDelta("done"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "done"}}, nil
}

func testEchoExecutor(t *testing.T) *Executor {
	t.Helper()
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "echo", Description: "echo"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "tool output", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}}
}

func TestAgentExecutesBoundedToolLoopAndAppendsToolMessage(t *testing.T) {
	config := DefaultConfig()
	provider := &toolLoopProvider{}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	messages := agent.Messages()
	if len(messages) != 5 || messages[3].Role != RoleTool || messages[3].ToolCallID != "call-1" {
		t.Fatalf("messages=%#v", messages)
	}
	var visibleResult ToolResult
	if err := json.Unmarshal([]byte(messages[3].Content), &visibleResult); err != nil {
		t.Fatalf("decode tool result metadata: %v", err)
	}
	if visibleResult.CallID != "call-1" || visibleResult.ToolName != "echo" {
		t.Fatalf("provider-visible tool result=%#v", visibleResult)
	}
	if len(provider.requests) != 2 || len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "echo" {
		t.Fatalf("requests=%#v", provider.requests)
	}
}

func TestAgentStopsAtToolStepLimit(t *testing.T) {
	provider := &toolLoopProvider{alwaysTool: true}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), MaxToolSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if err != nil || response.Message.Content == "" {
		t.Fatalf("response=%#v err=%v, want graceful landing", response, err)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls=%d, want work, rejected batch, and finalizer", provider.calls)
	}
	if len(provider.requests) != 3 || !reflect.DeepEqual(provider.requests[2].Tools, provider.requests[1].Tools) || provider.requests[2].ToolChoice == nil || provider.requests[2].ToolChoice.Mode != ToolChoiceNone {
		t.Fatalf("finalizer request=%#v", provider.requests[len(provider.requests)-1])
	}
}

func TestAgentDefaultToolLoopAllowsNonRepeatingBatches(t *testing.T) {
	provider := &toolLoopProvider{toolResponses: 9}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "hello", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if provider.calls != 10 {
		t.Fatalf("provider calls=%d, want 10", provider.calls)
	}
}

type scriptedToolProvider struct {
	responses []Response
	requests  []Request
}

func (provider *scriptedToolProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *scriptedToolProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	if len(provider.responses) == 0 {
		return Response{}, errors.New("scripted provider exhausted")
	}
	response := provider.responses[0]
	provider.responses = provider.responses[1:]
	if response.Message.Content != "" && onDelta != nil {
		if err := onDelta(response.Message.Content); err != nil {
			return Response{}, err
		}
	}
	return response, nil
}

func validPlanArguments() string {
	return `{"id":"plan-1","goal":"change a file","inputs":["repository state"],"outputs":["updated file"],"steps":["inspect","edit","test"],"variables":[{"name":"path","type":"string","scope":"local"}],"acceptance":["tests pass"]}`
}

func TestAgentExecutesExposedInternalPlanToolWithoutExternalExecutor(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "status-call", Type: "function", Name: PlanStatusToolName, Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{ExposePlanTracking: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "report plan status", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d, want 2", len(provider.requests))
	}
	if err := validateToolMessageSequence(provider.requests[1].Messages); err != nil {
		t.Fatalf("internal tool produced invalid replay: %v", err)
	}
}

func TestAgentRejectsRegistryCollisionWithInternalPlanTool(t *testing.T) {
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: PlanToolName, Description: "shadow"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: &Executor{Registry: registry}, ExposePlanTracking: true})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("collision error=%v", err)
	}
}

func TestAgentPlanningGateRequiresAndAcceptsPlan(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "plan-call", Type: "function", Name: PlanToolName, Arguments: validPlanArguments()}}}},
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "echo-call", Type: "function", Name: "echo", Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), RequirePlan: true, MaxToolSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	response, err := agent.Run(context.Background(), "change it", nil)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if plan := agent.Plan(); plan == nil || plan.Goal != "change a file" {
		t.Fatalf("plan=%#v", plan)
	}
	if len(provider.requests) != 3 || len(provider.requests[0].Tools) != 2 || provider.requests[0].Tools[0].Name != "bee_plan" {
		t.Fatalf("requests=%#v", provider.requests)
	}
}

func TestAgentPlanningGateAllowsFinalResponseWithoutPlan(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "i need more information"}}}}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: testEchoExecutor(t), RequirePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "what is missing?", nil); err != nil || response.Message.Content != "i need more information" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestAgentPlanningGateAllowsReadOnlyInspectionBeforePlan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello bee\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "read-call", Type: "function", Name: "read_file", Arguments: `{"path":"README.md"}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "I inspected the README."}},
	}}
	executor, err := NewCodingExecutor(ExecutionScope{Root: root}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor, RequirePlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := agent.Run(context.Background(), "inspect the README", nil); err != nil || response.Message.Content != "I inspected the README." {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}
