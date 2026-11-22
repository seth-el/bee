package bee

import (
	"context"
	"strings"
	"testing"
)

type captureMemoryProvider struct {
	request Request
	content string
}

func (provider *captureMemoryProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.request = request
	return Response{Message: Message{Role: RoleAssistant, Content: provider.content}, Model: request.Model}, nil
}

func (provider *captureMemoryProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, nil
}

func TestProviderMemoryExtractorBoundsInputAndRejectsMarkdown(t *testing.T) {
	provider := &captureMemoryProvider{content: "```json\n{\"candidates\":[]}\n```"}
	extractor := ProviderPostTurnMemoryExtractor{Provider: provider, Model: "local", MaxOutputBytes: 1024, MaxCandidates: 4, MaxPromptBytes: 8, MaxAssistantBytes: 8, MaxToolObservationBytes: 16}
	_, err := extractor.Extract(context.Background(), TurnMemoryInput{Prompt: strings.Repeat("p", 32), Assistant: strings.Repeat("a", 32), ToolObservations: []TurnMemoryObservation{{ToolName: "tool", Output: strings.Repeat("o", 64)}}})
	if err == nil {
		t.Fatal("expected strict JSON rejection")
	}
	if len(provider.request.Messages) != 2 || len(provider.request.Messages[1].Content) > 256 {
		t.Fatalf("bounded request = %#v", provider.request)
	}
	if !provider.request.DisableThinking {
		t.Fatal("memory extraction did not disable provider thinking")
	}
	if strings.Contains(provider.request.Messages[1].Content, strings.Repeat("p", 9)) || strings.Contains(provider.request.Messages[1].Content, strings.Repeat("a", 9)) {
		t.Fatalf("input was not bounded: %q", provider.request.Messages[1].Content)
	}
}
