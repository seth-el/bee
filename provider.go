package bee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	MaxProviderImageAttachments = 4
)

var supportedProviderImageMediaTypes = map[string]struct{}{
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// OpenAICompatibleProvider speaks the common /v1/chat/completions JSON and
// SSE protocol without importing a vendor SDK.
type OpenAICompatibleProvider struct {
	baseURL             string
	model               string
	client              *http.Client
	apiKey              string
	supportsImages      bool
	contextWindowTokens int
	attachmentMu        sync.RWMutex
	attachmentResolver  AttachmentArtifactResolver
	// stall tunes the silence/health-check watch (zero = defaults).
	stall stallPolicy
}

// providerRequestError marks failures that occur before an HTTP response is
// available. The agent uses this boundary to distinguish connection setup and
// stale-connection failures from malformed or interrupted provider output.
type providerRequestError struct {
	cause error
}

func (err *providerRequestError) Error() string {
	if err == nil || err.cause == nil {
		return "provider request failed"
	}
	return fmt.Sprintf("provider request: %v", err.cause)
}

func (err *providerRequestError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func NewProvider(config ProviderConfig) (Provider, error) {
	if err := (Config{Provider: config, Generation: DefaultConfig().Generation, Agent: DefaultConfig().Agent}).Validate(); err != nil {
		return nil, err
	}
	apiKey, err := config.APIKey()
	if err != nil {
		return nil, err
	}
	return NewOpenAICompatibleProvider(config, apiKey)
}

func NewOpenAICompatibleProvider(config ProviderConfig, apiKey string) (*OpenAICompatibleProvider, error) {
	if err := (Config{Provider: config, Generation: DefaultConfig().Generation, Agent: DefaultConfig().Agent}).Validate(); err != nil {
		return nil, err
	}
	return &OpenAICompatibleProvider{
		baseURL:             strings.TrimRight(config.BaseURL, "/"),
		model:               config.Model,
		apiKey:              apiKey,
		supportsImages:      config.ImageInput || localMultimodalProviderURL(config.BaseURL),
		contextWindowTokens: config.ContextWindowTokens,
		// net/http treats a zero Client.Timeout as unlimited. Parent request
		// contexts still provide authoritative cancellation for every request.
		// Connection setup is bounded separately (ProviderConnectTimeout).
		client: newProviderHTTPClient(optionalTimeout(config.TimeoutSeconds)),
	}, nil
}

func (provider *OpenAICompatibleProvider) ContextWindowTokens(_ string) (int, bool) {
	if provider == nil || provider.contextWindowTokens <= 0 {
		return 0, false
	}
	return provider.contextWindowTokens, true
}

func optionalTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// SupportsImageInput reports the explicitly configured capability of this
// provider. OpenAI-compatible endpoints are not assumed to support images.
func (provider *OpenAICompatibleProvider) SupportsImageInput() bool {
	return provider != nil && provider.supportsImages
}

// SetAttachmentResolver wires the composition-owned artifact store without
// exposing filesystem paths or changing capabilities of other providers.
func (provider *OpenAICompatibleProvider) SetAttachmentResolver(resolver AttachmentArtifactResolver) {
	if provider != nil {
		provider.attachmentMu.Lock()
		defer provider.attachmentMu.Unlock()
		provider.attachmentResolver = resolver
	}
}

func (provider *OpenAICompatibleProvider) attachmentResolverValue() AttachmentArtifactResolver {
	provider.attachmentMu.RLock()
	defer provider.attachmentMu.RUnlock()
	return provider.attachmentResolver
}

func (provider *OpenAICompatibleProvider) Complete(ctx context.Context, request Request) (Response, error) {
	normalized, err := provider.normalizeRequest(request)
	if err != nil {
		return Response{}, err
	}
	messages, err := provider.requestMessages(ctx, normalized.Messages)
	if err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(openAIRequest{Model: normalized.Model, Messages: messages, Tools: requestTools(normalized.Tools), ToolChoice: requestToolChoice(normalized.ToolChoice), Temperature: normalized.Temperature, MaxTokens: normalized.MaxTokens, ReasoningEffort: normalized.ReasoningEffort, ChatTemplateKwargs: chatTemplateKwargs(normalized)})
	if err != nil {
		return Response{}, fmt.Errorf("encode provider request: %w", err)
	}
	ctx, touch, stop := provider.watchStall(ctx)
	defer stop()
	response, err := provider.do(ctx, body, false)
	if err != nil {
		if unreachable := stallCause(ctx); unreachable != nil {
			return Response{}, &providerRequestError{cause: unreachable}
		}
		return Response{}, err
	}
	defer response.Body.Close()
	touch()
	response.Body = activityReader{ReadCloser: response.Body, touch: touch}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, readHTTPError(response)
	}
	var payload openAIResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		if unreachable := stallCause(ctx); unreachable != nil {
			return Response{}, unreachable
		}
		return Response{}, fmt.Errorf("decode provider response: %w", err)
	}
	return payload.response()
}

func (provider *OpenAICompatibleProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	return provider.StreamWithEvents(ctx, request, func(event StreamEvent) error {
		if event.Kind == StreamContentDelta && onDelta != nil {
			return onDelta(event.Delta)
		}
		return nil
	})
}

func (provider *OpenAICompatibleProvider) StreamWithEvents(ctx context.Context, request Request, onEvent StreamEventFunc) (Response, error) {
	normalized, err := provider.normalizeRequest(request)
	if err != nil {
		return Response{}, err
	}
	messages, err := provider.requestMessages(ctx, normalized.Messages)
	if err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(openAIRequest{
		Model:              normalized.Model,
		Messages:           messages,
		Tools:              requestTools(normalized.Tools),
		ToolChoice:         requestToolChoice(normalized.ToolChoice),
		Temperature:        normalized.Temperature,
		MaxTokens:          normalized.MaxTokens,
		ReasoningEffort:    normalized.ReasoningEffort,
		ChatTemplateKwargs: chatTemplateKwargs(normalized),
		Stream:             true,
		StreamOptions:      &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return Response{}, fmt.Errorf("encode provider request: %w", err)
	}
	ctx, touch, stop := provider.watchStall(ctx)
	defer stop()
	response, err := provider.do(ctx, body, true)
	if err != nil {
		if unreachable := stallCause(ctx); unreachable != nil {
			return Response{}, &providerRequestError{cause: unreachable}
		}
		return Response{}, err
	}
	defer response.Body.Close()
	touch()
	response.Body = activityReader{ReadCloser: response.Body, touch: touch}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, readHTTPError(response)
	}
	result, err := parseSSEWithEvents(response.Body, onEvent)
	if err != nil {
		if unreachable := stallCause(ctx); unreachable != nil {
			return result, fmt.Errorf("provider stream: %w", unreachable)
		}
	}
	return result, err
}

func thinkingOptions(disable bool) map[string]any {
	if !disable {
		return nil
	}
	return map[string]any{"enable_thinking": false}
}

func chatTemplateKwargs(request Request) map[string]any {
	if len(request.ChatTemplateKwargs) == 0 && !request.DisableThinking && request.ReasoningEffort == "" {
		return nil
	}
	kwargs := make(map[string]any, len(request.ChatTemplateKwargs)+2)
	for key, value := range request.ChatTemplateKwargs {
		kwargs[key] = value
	}
	if request.DisableThinking {
		kwargs["enable_thinking"] = false
	}
	if request.ReasoningEffort != "" {
		kwargs["reasoning_effort"] = request.ReasoningEffort
	}
	return kwargs
}

func (provider *OpenAICompatibleProvider) normalizeRequest(request Request) (Request, error) {
	if request.Model == "" {
		request.Model = provider.model
	}
	return NormalizeRequest(request)
}

func (provider *OpenAICompatibleProvider) do(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, completionsURL(provider.baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build provider request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	if provider.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+provider.apiKey)
	}
	response, err := provider.client.Do(request)
	if err != nil {
		if unreachable := classifyDialError(provider.baseURL, err); unreachable != nil {
			return nil, &providerRequestError{cause: unreachable}
		}
		return nil, &providerRequestError{cause: err}
	}
	return response, nil
}

func completionsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if parsed, err := url.Parse(base); err == nil && strings.HasSuffix(parsed.Path, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func readHTTPError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("provider returned HTTP %s: %s", response.Status, message)
}

type openAIRequest struct {
	Model              string          `json:"model"`
	Messages           []openAIMessage `json:"messages"`
	Tools              []openAITool    `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	Temperature        float64         `json:"temperature,omitempty"`
	MaxTokens          int             `json:"max_tokens,omitempty"`
	ReasoningEffort    string          `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any  `json:"chat_template_kwargs,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	StreamOptions      *streamOptions  `json:"stream_options,omitempty"`
}

func requestToolChoice(choice *ToolChoice) any {
	if choice == nil {
		return nil
	}
	if choice.Mode == ToolChoiceNone {
		return string(ToolChoiceNone)
	}
	return map[string]any{
		"type": "function",
		"function": map[string]string{
			"name": choice.Name,
		},
	}
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIFunctionSpec `json:"function"`
}

type openAIFunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func requestTools(definitions []ToolDefinition) []openAITool {
	if len(definitions) == 0 {
		return nil
	}
	tools := make([]openAITool, len(definitions))
	for i, definition := range definitions {
		tools[i] = openAITool{Type: "function", Function: openAIFunctionSpec{Name: definition.Name, Description: definition.Description, Parameters: definition.Parameters}}
	}
	return tools
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   Usage          `json:"usage"`
}

type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	Delta        Message       `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

func (payload openAIResponse) response() (Response, error) {
	if len(payload.Choices) == 0 {
		return Response{}, errors.New("provider response contained no choices")
	}
	choice := payload.Choices[0]
	message := choice.Message.message()
	if message.Role == "" {
		message.Role = RoleAssistant
	}
	reasoning := newBoundedText(MaxReasoningBytes)
	reasoning.Append(choice.Message.ReasoningContent)
	result := Response{Message: message, Usage: payload.Usage, Model: payload.Model, Reasoning: reasoning.String(), ReasoningComplete: true, ReasoningTruncated: reasoning.Truncated(), ReasoningOriginalBytes: reasoning.OriginalBytes()}
	if choice.FinishReason != nil {
		result.FinishReason = *choice.FinishReason
	}
	return result, nil
}

type openAIMessage struct {
	Role             Role                 `json:"role"`
	Content          any                  `json:"content,omitempty"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	Name             string               `json:"name,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	ToolCalls        []openAIToolCallWire `json:"tool_calls,omitempty"`
}

type openAIContentPart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *openAIImageURLPart `json:"image_url,omitempty"`
}

type openAIImageURLPart struct {
	URL string `json:"url"`
}

type openAIToolCallWire struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIFunctionWire `json:"function,omitempty"`
}

type openAIFunctionWire struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func requestMessages(messages []Message) []openAIMessage {
	result := make([]openAIMessage, len(messages))
	for i, message := range messages {
		result[i] = openAIMessage{Role: message.Role, Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			result[i].ToolCalls = append(result[i].ToolCalls, openAIToolCallWire{
				ID:   call.ID,
				Type: call.Type,
				Function: openAIFunctionWire{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
	}
	return result
}

func (provider *OpenAICompatibleProvider) requestMessages(ctx context.Context, messages []Message) ([]openAIMessage, error) {
	if err := validateToolMessageSequence(messages); err != nil {
		return nil, err
	}
	result := make([]openAIMessage, len(messages))
	for i, message := range messages {
		result[i] = openAIMessage{Role: message.Role, Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			result[i].ToolCalls = append(result[i].ToolCalls, openAIToolCallWire{
				ID: call.ID, Type: call.Type,
				Function: openAIFunctionWire{Name: call.Name, Arguments: call.Arguments},
			})
		}
		if len(message.Attachments) == 0 || !provider.supportsImages {
			continue
		}
		parts, err := provider.imageContentParts(ctx, message.Content, message.Attachments)
		if err != nil {
			return nil, fmt.Errorf("message %d image attachments: %w", i, err)
		}
		if len(parts) > 0 {
			result[i].Content = parts
		}
	}
	return result, nil
}

func (provider *OpenAICompatibleProvider) imageContentParts(ctx context.Context, text string, attachments []AttachmentRef) ([]openAIContentPart, error) {
	parts := make([]openAIContentPart, 0, len(attachments)+1)
	imageCount := 0
	resolver := provider.attachmentResolverValue()
	for _, attachment := range attachments {
		if attachment.Kind != "image" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(attachment.MediaType)), "image/") {
			continue
		}
		if resolver == nil {
			return nil, errors.New("image input is enabled but attachment resolver is not configured")
		}
		imageCount++
		if imageCount > MaxProviderImageAttachments {
			return nil, fmt.Errorf("at most %d image attachments may be sent", MaxProviderImageAttachments)
		}
		artifact, reader, err := resolver.OpenAttachmentPayload(ctx, attachment)
		if err != nil {
			return nil, err
		}
		data, readErr := readProviderImage(ctx, reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("image attachment %q is empty", attachment.Filename)
		}
		mediaType, err := providerImageMediaType(artifact.MediaType, attachment.MediaType)
		if err != nil {
			return nil, fmt.Errorf("image attachment %q: %w", attachment.Filename, err)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		dataURI := "data:" + mediaType + ";base64," + encoded
		parts = append(parts, openAIContentPart{Type: "image_url", ImageURL: &openAIImageURLPart{URL: dataURI}})
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(text) != "" {
		// Keep ordinary text alongside images in the llama.cpp/OpenAI
		// multimodal content-parts shape.
		return append([]openAIContentPart{{Type: "text", Text: text}}, parts...), nil
	}
	return parts, nil
}

func readProviderImage(ctx context.Context, reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("image attachment payload reader is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(p)
}

func providerImageMediaType(primary, fallback string) (string, error) {
	for _, candidate := range []string{primary, fallback} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		parsed, _, err := mime.ParseMediaType(candidate)
		if err != nil {
			continue
		}
		if _, ok := supportedProviderImageMediaTypes[parsed]; ok {
			return parsed, nil
		}
	}
	return "", errors.New("unsupported image media type")
}

func (message openAIMessage) message() Message {
	content, _ := message.Content.(string)
	result := Message{Role: message.Role, Content: content, Name: message.Name, ToolCallID: message.ToolCallID}
	for _, call := range message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: call.ID, Type: call.Type, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return result
}

// parseSSE parses OpenAI-compatible data events and accumulates assistant
// content, reasoning, tool-call fragments, usage, model, and finish reason.
func parseSSE(reader io.Reader, onDelta StreamFunc) (Response, error) {
	return parseSSEWithEvents(reader, func(event StreamEvent) error {
		if event.Kind == StreamContentDelta && onDelta != nil {
			return onDelta(event.Delta)
		}
		return nil
	})
}

func parseSSEWithEvents(reader io.Reader, onEvent StreamEventFunc) (Response, error) {
	if onEvent == nil {
		onEvent = func(StreamEvent) error { return nil }
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var response Response
	response.Message.Role = RoleAssistant
	var toolCalls []ToolCall
	lineNumber := 0
	sawDone := false
	sawFinish := false
	reasoning := newBoundedText(MaxReasoningBytes)
	syncReasoning := func() {
		response.Reasoning = reasoning.String()
		response.ReasoningTruncated = reasoning.Truncated()
		response.ReasoningOriginalBytes = reasoning.OriginalBytes()
	}
	partial := func() {
		syncReasoning()
		response.Message.ToolCalls = nil
		response.Incomplete = response.Message.Content != "" || response.Reasoning != ""
	}
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			partial()
			return response, fmt.Errorf("malformed SSE line %d: expected data field", lineNumber)
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			partial()
			return response, fmt.Errorf("malformed SSE data at line %d: %w", lineNumber, err)
		}
		if chunk.Model != "" {
			response.Model = chunk.Model
		}
		if chunk.Usage != nil {
			response.Usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != nil {
			response.FinishReason = *choice.FinishReason
			if response.FinishReason != "" {
				sawFinish = true
			}
		}
		if choice.Delta.Content != "" {
			response.Message.Content += choice.Delta.Content
			if err := onEvent(StreamEvent{Kind: StreamContentDelta, Delta: choice.Delta.Content}); err != nil {
				partial()
				return response, fmt.Errorf("stream callback: %w", err)
			}
		}
		if choice.Delta.ReasoningContent != "" {
			reasoning.Append(choice.Delta.ReasoningContent)
			syncReasoning()
			if err := onEvent(StreamEvent{Kind: StreamReasoningDelta, Delta: choice.Delta.ReasoningContent}); err != nil {
				partial()
				return response, fmt.Errorf("stream callback: %w", err)
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if call.Index < 0 {
				partial()
				return response, fmt.Errorf("malformed SSE tool call at line %d: negative index %d", lineNumber, call.Index)
			}
			for len(toolCalls) <= call.Index {
				toolCalls = append(toolCalls, ToolCall{Type: "function"})
			}
			tool := &toolCalls[call.Index]
			if call.ID != "" {
				tool.ID = call.ID
			}
			if call.Type != "" {
				tool.Type = call.Type
			}
			tool.Name += call.Function.Name
			tool.Arguments += call.Function.Arguments
			delta := &ToolCallDelta{Index: call.Index, ID: call.ID, Type: call.Type, Name: call.Function.Name, Arguments: call.Function.Arguments}
			if err := onEvent(StreamEvent{Kind: StreamToolCallDelta, ToolCall: delta}); err != nil {
				partial()
				return response, fmt.Errorf("stream callback: %w", err)
			}
		}
	}
	response.Message.ToolCalls = toolCalls
	if err := scanner.Err(); err != nil {
		partial()
		return response, fmt.Errorf("read SSE response: %w", err)
	}
	if !sawDone && !sawFinish {
		partial()
		return response, fmt.Errorf("read SSE response: %w", io.ErrUnexpectedEOF)
	}
	syncReasoning()
	response.ReasoningComplete = true
	return response, nil
}

type openAIStreamChunk struct {
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *Usage               `json:"usage"`
}

type openAIStreamChoice struct {
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openAIDelta struct {
	Role             Role             `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function openAIFunctionDelta `json:"function"`
}

type openAIFunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
