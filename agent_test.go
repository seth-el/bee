package bee

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeProvider struct {
	requests []Request
}

func (provider *fakeProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (provider *fakeProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	if onDelta != nil {
		if err := onDelta("reply"); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "reply"}}, nil
}

func TestAgentMaintainsProcessLocalConversation(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	config.Agent.SystemPrompt = "be concise"
	provider := &fakeProvider{}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Messages) != 2 {
		t.Fatalf("requests = %#v", provider.requests)
	}
	if _, err := agent.Run(context.Background(), "second", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests[1].Messages) != 4 {
		t.Fatalf("second request messages = %#v", provider.requests[1].Messages)
	}
}

func TestAgentRunMessagesPreservesAdapterMessagesAndRequestKwargs(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	config := DefaultConfig()
	config.Provider.Model = "game-model"
	config.Agent.SystemPrompt = "short game policy"
	config.Generation.ReasoningEffort = "high"
	provider := &fakeProvider{}
	agent, err := NewAgentWithOptions(config, provider, AgentOptions{ChatTemplateKwargs: map[string]any{"enable_thinking": false}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RunMessages(context.Background(), []Message{{Role: RoleSystem, Content: "rpg state"}, {Role: RoleUser, Content: "hello"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Messages) != 3 {
		t.Fatalf("request = %#v", provider.requests)
	}
	if provider.requests[0].Messages[0].Content != "short game policy" || provider.requests[0].Messages[1].Content != "rpg state" {
		t.Fatalf("messages = %#v", provider.requests[0].Messages)
	}
	if enabled, ok := provider.requests[0].ChatTemplateKwargs["enable_thinking"].(bool); !ok || enabled {
		t.Fatalf("chat_template_kwargs = %#v", provider.requests[0].ChatTemplateKwargs)
	}
	if provider.requests[0].ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q", provider.requests[0].ReasoningEffort)
	}
}

func TestAgentRunMessagesPreservesRepeatedConversationMessages(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "repeat-model"
	provider := &fakeProvider{}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{
		{Role: RoleUser, Content: "yes"},
		{Role: RoleAssistant, Content: "continue?"},
		{Role: RoleUser, Content: "yes"},
	}
	if _, err := agent.RunMessages(context.Background(), messages, nil); err != nil {
		t.Fatal(err)
	}
	request := provider.requests[0]
	seen := 0
	for _, message := range request.Messages {
		if message.Role == RoleUser && message.Content == "yes" {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("repeated user messages=%d, request=%#v", seen, request.Messages)
	}
}

func TestAgentRunMessagesPreservesDistinctEmptyToolCallGroups(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "tool-history-model"
	provider := &fakeProvider{}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Name: "lookup", Arguments: `{}`}}},
		{Role: RoleTool, Name: "lookup", ToolCallID: "call-1", Content: "one"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-2", Type: "function", Name: "lookup", Arguments: `{}`}}},
		{Role: RoleTool, Name: "lookup", ToolCallID: "call-2", Content: "two"},
		{Role: RoleUser, Content: "continue"},
	}
	if _, err := agent.RunMessages(context.Background(), messages, nil); err != nil {
		t.Fatal(err)
	}
	request := provider.requests[0]
	if err := validateToolMessageSequence(request.Messages); err != nil {
		t.Fatalf("adapter tool history became invalid: %v", err)
	}
	groups := 0
	for _, message := range request.Messages {
		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 {
			groups++
		}
	}
	if groups != 2 {
		t.Fatalf("tool-call groups=%d, request=%#v", groups, request.Messages)
	}
}

func TestTurnLocalSystemPromptChangeDoesNotRewriteStablePrefix(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "cache-model"
	base := []Message{
		{Role: RoleSystem, Content: "stable core"},
		{Role: RoleUser, Content: "prior turn"},
		{Role: RoleAssistant, Content: "prior answer"},
		{Role: RoleUser, Content: "current turn"},
	}
	makeRequest := func(control string) Request {
		agent, err := NewAgentWithOptions(config, &fakeProvider{}, AgentOptions{AdditionalSystemPrompt: control})
		if err != nil {
			t.Fatal(err)
		}
		agent.messages = append([]Message(nil), base...)
		if err := agent.prepareTurnContext(context.Background(), "current turn", ""); err != nil {
			t.Fatal(err)
		}
		request, err := agent.request()
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	first := makeRequest("temporary control one")
	second := makeRequest("temporary control two")
	firstFingerprint, _, _ := stableRequestPrefixFingerprint(first)
	secondFingerprint, _, _ := stableRequestPrefixFingerprint(second)
	if firstFingerprint != secondFingerprint {
		t.Fatalf("turn-local control rewrote stable prefix: %q != %q", firstFingerprint, secondFingerprint)
	}
	if first.Messages[len(first.Messages)-1].Content != "temporary control one" {
		t.Fatalf("turn-local control was not inserted after current user: %#v", first.Messages)
	}
}

type failingIdentityStore struct{}

func (failingIdentityStore) Load(context.Context) (IdentityProfile, error) {
	return IdentityProfile{}, errors.New("identity unavailable")
}
func (failingIdentityStore) Save(context.Context, IdentityProfile) error { return nil }

func TestAgentDoesNotPersistUserMessageWhenTurnPreparationFails(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Session: session, IdentityStore: failingIdentityStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "do not orphan me", nil); err == nil {
		t.Fatal("expected turn preparation failure")
	}
	for _, event := range session.EventsSnapshot() {
		if event.Type != EventMessage {
			continue
		}
		message, decodeErr := DecodeMessageEvent(event)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if message.Message.Role == RoleUser {
			t.Fatalf("failed turn persisted user message: %#v", message.Message)
		}
	}
}

func TestAgentPreservesExplicitPositiveToolLoopLimit(t *testing.T) {
	provider := &fakeProvider{}
	const optionMaxSteps = 3

	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{MaxToolSteps: optionMaxSteps})
	if err != nil {
		t.Fatalf("max_tool_steps option should allow positive values: %v", err)
	}
	if agent.maxToolSteps != optionMaxSteps {
		t.Fatalf("agent max_tool_steps from options = %d, want %d", agent.maxToolSteps, optionMaxSteps)
	}

	config := DefaultConfig()
	const configMaxSteps = 4
	config.Agent.MaxToolSteps = configMaxSteps
	if err := config.Validate(); err != nil {
		t.Fatalf("config max_tool_steps should allow positive values: %v", err)
	}
	configAgent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatalf("config max_tool_steps should be applied: %v", err)
	}
	if configAgent.maxToolSteps != configMaxSteps {
		t.Fatalf("agent max_tool_steps from config = %d, want %d", configAgent.maxToolSteps, configMaxSteps)
	}
	if err := agent.SetMaxToolSteps(optionMaxSteps); err != nil {
		t.Fatalf("SetMaxToolSteps should allow positive values: %v", err)
	}
	if agent.maxToolSteps != optionMaxSteps {
		t.Fatalf("agent max_tool_steps after setter = %d, want %d", agent.maxToolSteps, optionMaxSteps)
	}
}

func TestAgentOptionsCanExplicitlyDisableToolCallLimit(t *testing.T) {
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{DisableToolCallLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	if agent.maxToolSteps != 0 {
		t.Fatalf("max_tool_steps=%d, want unlimited zero", agent.maxToolSteps)
	}
}

func TestAgentRequiredToolChoiceRejectsNoToolFallback(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(ToolFunc{ToolDefinition: ToolDefinition{Name: "memory_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
		return `{"hits":[]}`, nil
	}}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{
		Executor: &Executor{Registry: registry, Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
			return PolicyAllow, nil
		})},
		ToolChoice: &ToolChoice{Name: "memory_search"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "what do you remember?", nil); !errors.Is(err, ErrRequiredToolCall) {
		t.Fatalf("error = %v, want ErrRequiredToolCall", err)
	}
	if len(provider.requests) != 1 || provider.requests[0].ToolChoice == nil || provider.requests[0].ToolChoice.Name != "memory_search" {
		t.Fatalf("provider request = %#v", provider.requests)
	}
}
