package bee

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseSSEAccumulatesContentUsageAndToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"model":"test-model","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"bee\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var deltas []string
	response, err := parseSSE(strings.NewReader(stream), func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "Hello" || response.Message.Content != "Hello" {
		t.Fatalf("content = %q deltas = %#v", response.Message.Content, deltas)
	}
	if response.Usage.TotalTokens != 7 || response.FinishReason != "tool_calls" || response.Model != "test-model" {
		t.Fatalf("response metadata = %#v", response)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls = %#v", response.Message.ToolCalls)
	}
}

func TestParseSSERejectsMalformedData(t *testing.T) {
	_, err := parseSSE(strings.NewReader("data: not-json\n\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "malformed SSE data") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseSSESeparatesInterleavedReasoningContentAndToolFragments(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"model":"glm","choices":[{"delta":{"reasoning_content":"think-"}}]}`,
		`data: {"choices":[{"delta":{"content":"vis"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"more"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"ible"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var kinds []StreamEventKind
	var deltas []string
	response, err := parseSSEWithEvents(strings.NewReader(stream), func(event StreamEvent) error {
		kinds = append(kinds, event.Kind)
		deltas = append(deltas, event.Delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Reasoning != "think-more" || response.Message.Content != "visible" {
		t.Fatalf("response=%#v", response)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Fatalf("tool calls=%#v", response.Message.ToolCalls)
	}
	want := []StreamEventKind{StreamReasoningDelta, StreamContentDelta, StreamReasoningDelta, StreamToolCallDelta, StreamContentDelta, StreamToolCallDelta}
	if strings.Join(stringifyKinds(kinds), ",") != strings.Join(stringifyKinds(want), ",") {
		t.Fatalf("kinds=%v", kinds)
	}
}

func stringifyKinds(values []StreamEventKind) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func TestParseSSEReturnsPartialResponseOnUnexpectedEOF(t *testing.T) {
	response, err := parseSSE(strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial\",\"content\":\"answer\"}}]}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "read SSE response") {
		t.Fatalf("err=%v", err)
	}
	if response.Reasoning != "partial" || response.Message.Content != "answer" || !response.Incomplete {
		t.Fatalf("partial=%#v", response)
	}
}

func TestParseSSEAcceptsCleanEOFAfterFinishReason(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n"
	response, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil || response.Message.Content != "done" || response.FinishReason != "stop" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestParseSSERejectsCleanEOFWithoutFinishReason(t *testing.T) {
	response, err := parseSSE(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") || !response.Incomplete {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestParseSSEBoundsMultibyteReasoningDuringStreaming(t *testing.T) {
	reasoning := strings.Repeat("界", MaxReasoningBytes/len("界")+100)
	stream := "data: " + string(mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"reasoning_content": reasoning}}}})) + "\n\ndata: [DONE]\n"
	response, err := parseSSE(strings.NewReader(stream), nil)
	if err != nil || !response.ReasoningTruncated || response.ReasoningOriginalBytes != len(reasoning) || len(response.Reasoning) > MaxReasoningBytes {
		t.Fatalf("len=%d original=%d truncated=%t err=%v", len(response.Reasoning), response.ReasoningOriginalBytes, response.ReasoningTruncated, err)
	}
	if !utf8.ValidString(response.Reasoning) {
		t.Fatal("reasoning was truncated at an invalid UTF-8 boundary")
	}
}

func TestParseSSEBoundsOversizedReasoningBeforePartialError(t *testing.T) {
	reasoning := strings.Repeat("界", MaxReasoningBytes/len("界")+100)
	stream := "data: " + string(mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"reasoning_content": reasoning}}}})) + "\n"
	response, err := parseSSE(strings.NewReader(stream), nil)
	if err == nil || !response.Incomplete || !response.ReasoningTruncated || response.ReasoningOriginalBytes != len(reasoning) || !utf8.ValidString(response.Reasoning) {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestOpenAIProviderCompleteSeparatesReasoningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"glm","choices":[{"message":{"role":"assistant","reasoning_content":"private","content":"public"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "glm"}, "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Reasoning != "private" || response.Message.Content != "public" {
		t.Fatalf("response=%#v", response)
	}
}

func TestOpenAIProviderStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		var payload openAIRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "test-model" || len(payload.Messages) != 1 {
			t.Fatalf("request payload = %#v", payload)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("content = %q", response.Message.Content)
	}
}

func TestOpenAIProviderReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, `{"error":"bad request"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAIProviderCompleteNormalizesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"bee\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup" || response.Usage.TotalTokens != 3 {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAIProviderSendsNamedToolChoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			ToolChoice map[string]any `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.ToolChoice["type"] != "function" {
			t.Fatalf("tool choice type = %#v", payload.ToolChoice)
		}
		function, ok := payload.ToolChoice["function"].(map[string]any)
		if !ok || function["name"] != "memory_search" {
			t.Fatalf("tool choice function = %#v", payload.ToolChoice)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"memory_search","arguments":"{\"query\":\"inn\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "what do you remember?"}},
		ToolChoice: &ToolChoice{Name: "memory_search"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "memory_search" {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAIProviderSendsNoneToolChoiceWithSchemas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Tools      []openAITool `json:"tools"`
			ToolChoice any          `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Tools) != 1 || payload.Tools[0].Function.Name != "lookup" {
			t.Fatalf("tools = %#v", payload.Tools)
		}
		if payload.ToolChoice != "none" {
			t.Fatalf("tool choice = %#v, want none", payload.ToolChoice)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"landed"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{
		Messages:   []Message{{Role: RoleUser, Content: "finish"}},
		Tools:      []ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceNone},
	})
	if err != nil || response.Message.Content != "landed" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestOpenAIProviderSendsStoredImageAsLlamaContentPart(t *testing.T) {
	sessions, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "image test"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	imageBytes := []byte{137, 80, 78, 71, 13, 10, 26, 10, 1, 2, 3, 4}
	attachment, err := attachments.Create(context.Background(), session.ID, "bee.png", "image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		messages := payload["messages"].([]any)
		message := messages[0].(map[string]any)
		content := message["content"].([]any)
		if got := content[0].(map[string]any); got["type"] != "text" || got["text"] != "describe this" {
			t.Fatalf("text content part = %#v", got)
		} else if _, ok := got["image_url"]; ok {
			t.Fatalf("text part unexpectedly has image_url: %#v", got)
		}
		imagePart := content[1].(map[string]any)
		if imagePart["type"] != "image_url" {
			t.Fatalf("image part = %#v", imagePart)
		}
		imageURL := imagePart["image_url"].(map[string]any)["url"].(string)
		prefix := "data:image/png;base64,"
		if !strings.HasPrefix(imageURL, prefix) {
			t.Fatalf("image URL = %q", imageURL)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, prefix))
		if err != nil || string(decoded) != string(imageBytes) {
			t.Fatalf("decoded image = %v err=%v", decoded, err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1, ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetAttachmentResolver(artifacts)
	response, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "describe this", Attachments: []AttachmentRef{{ID: attachment.ID, ArtifactID: attachment.ArtifactID, Filename: attachment.Filename, MediaType: attachment.MediaType, Kind: attachment.Kind}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAIProviderOmitsImagesWhenCapabilityIsDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload openAIRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload.Messages[0].Content.(string); !ok {
			t.Fatalf("content should remain string: %#v", payload.Messages[0].Content)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "describe this", Attachments: []AttachmentRef{{Kind: "image", ArtifactID: "artifact-1", MediaType: "image/png"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAIProviderRejectsImageWithoutResolverOrUnsupportedMedia(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:1", Model: "test-model", TimeoutSeconds: 1, ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "describe", Attachments: []AttachmentRef{{Kind: "image", ArtifactID: "artifact-1", MediaType: "image/png"}}}}})
	if err == nil || !strings.Contains(err.Error(), "resolver") {
		t.Fatalf("missing resolver error = %v", err)
	}
}
