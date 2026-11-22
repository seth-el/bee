package bee

import "testing"

func TestNormalizeRequestCopiesMessagesAndToolCalls(t *testing.T) {
	calls := []ToolCall{{ID: "call-1", Type: "function", Name: "lookup", Arguments: `{"q":"bee"}`}}
	request, err := NormalizeRequest(Request{
		Model:       "test-model",
		Temperature: 0.2,
		MaxTokens:   100,
		Messages:    []Message{{Role: RoleAssistant, ToolCalls: calls}},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls[0].Name = "mutated"
	if request.Messages[0].ToolCalls[0].Name != "lookup" {
		t.Fatal("normalized request aliases tool call input")
	}
}

func TestNormalizeRequestRejectsUnknownRole(t *testing.T) {
	_, err := NormalizeRequest(Request{Model: "test-model", Messages: []Message{{Role: Role("narrator")}}})
	if err == nil {
		t.Fatal("expected unknown role error")
	}
}

func TestNormalizeRequestValidatesProviderNeutralToolChoiceModes(t *testing.T) {
	base := Request{Model: "test-model", Messages: []Message{{Role: RoleUser, Content: "hello"}}}
	for _, choice := range []*ToolChoice{{Name: "lookup"}, {Mode: ToolChoiceNamed, Name: "lookup"}, {Mode: ToolChoiceNone}} {
		request := base
		request.ToolChoice = choice
		if _, err := NormalizeRequest(request); err != nil {
			t.Fatalf("choice %#v rejected: %v", choice, err)
		}
	}
	for _, choice := range []*ToolChoice{{Mode: ToolChoiceNamed}, {Mode: ToolChoiceNone, Name: "lookup"}, {Mode: ToolChoiceMode("arbitrary")}} {
		request := base
		request.ToolChoice = choice
		if _, err := NormalizeRequest(request); err == nil {
			t.Fatalf("choice %#v was accepted", choice)
		}
	}
}

func TestRequestMessagesUseOpenAIFunctionShape(t *testing.T) {
	wire := requestMessages([]Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Name: "lookup", Arguments: `{"q":"bee"}`}}}})
	if len(wire) != 1 || len(wire[0].ToolCalls) != 1 {
		t.Fatalf("wire = %#v", wire)
	}
	call := wire[0].ToolCalls[0]
	if call.Function.Name != "lookup" || call.Function.Arguments != `{"q":"bee"}` {
		t.Fatalf("wire tool call = %#v", call)
	}
}

func TestRequestToolsUseProviderNeutralDefinitions(t *testing.T) {
	wire := requestTools([]ToolDefinition{{Name: "read_file", Description: "read", Parameters: []byte(`{"type":"object"}`)}})
	if len(wire) != 1 || wire[0].Type != "function" || wire[0].Function.Name != "read_file" {
		t.Fatalf("wire = %#v", wire)
	}
}
