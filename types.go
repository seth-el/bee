package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Role identifies the speaker or source of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is the provider-neutral representation of a requested tool call.
// Bee does not execute tool calls in Phase 1, but preserves them in responses
// so later phases do not need to depend on provider wire formats.
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

var ErrInvalidToolCallArguments = errors.New("invalid tool-call arguments")
var ErrInvalidToolSequence = errors.New("invalid tool-call/result sequence")

const MaxProviderToolArgumentsBytes = 16 << 20

// ToolCallArgumentsError identifies the assistant tool call that would make a
// provider request structurally invalid. It is intentionally returned before
// any provider I/O so malformed durable history cannot become a remote 500.
type ToolCallArgumentsError struct {
	MessageIndex int
	CallIndex    int
	ToolName     string
	CallID       string
	Cause        error
}

func (err *ToolCallArgumentsError) Error() string {
	if err == nil {
		return ErrInvalidToolCallArguments.Error()
	}
	return fmt.Sprintf("%v: assistant message %d tool call %d (%s, id %s) must be a valid JSON object: %v; repair durable history with `bee sessions repair --session SESSION_ID --apply` or start a new session", ErrInvalidToolCallArguments, err.MessageIndex+1, err.CallIndex+1, printableToolName(err.ToolName), printableCallID(err.CallID), err.Cause)
}

func (err *ToolCallArgumentsError) Unwrap() error { return ErrInvalidToolCallArguments }

// ToolSequenceError identifies an assistant/tool message pairing that would
// make an OpenAI-compatible provider request structurally invalid.
type ToolSequenceError struct {
	MessageIndex int
	CallID       string
	Reason       string
}

func (err *ToolSequenceError) Error() string {
	if err == nil {
		return ErrInvalidToolSequence.Error()
	}
	return fmt.Sprintf("%v: message %d%s: %s; repair durable history or start a new session", ErrInvalidToolSequence, err.MessageIndex+1, formatCallID(err.CallID), err.Reason)
}

func (err *ToolSequenceError) Unwrap() error { return ErrInvalidToolSequence }

func formatCallID(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return fmt.Sprintf(" for call %q", value)
}

func printableToolName(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unnamed"
	}
	return value
}

func printableCallID(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}

// Message is the provider-neutral conversation unit.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// Incomplete marks visible assistant content retained after a provider
	// stream failed. It is never sent as hidden reasoning or replayed as a
	// successful completion.
	Incomplete  bool            `json:"incomplete,omitempty"`
	Name        string          `json:"name,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	ToolCalls   []ToolCall      `json:"tool_calls,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// Usage contains token counts when the provider reports them.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// UnmarshalJSON normalizes cache telemetry used by OpenAI-compatible servers.
// Providers variously report it at top level or under prompt token details.
func (usage *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	var wire struct {
		usageAlias
		CachedTokens             int `json:"cached_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		PromptTokensDetails      struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*usage = Usage(wire.usageAlias)
	usage.CacheReadTokens = maxInt(usage.CacheReadTokens, wire.CachedTokens, wire.CacheReadInputTokens, wire.PromptTokensDetails.CachedTokens)
	usage.CacheWriteTokens = maxInt(usage.CacheWriteTokens, wire.CacheCreationInputTokens)
	return nil
}

func maxInt(values ...int) int {
	result := 0
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

// ToolChoiceMode is the provider-neutral subset of tool selection behavior
// that Bee permits callers to request.
type ToolChoiceMode string

const (
	ToolChoiceNamed ToolChoiceMode = "named"
	ToolChoiceNone  ToolChoiceMode = "none"
)

// ToolChoice either names one tool that the provider must request for the next
// completion or explicitly forbids tool calls while retaining the schemas in
// the prompt. An omitted mode with a non-empty name preserves the original
// named-choice API.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode,omitempty"`
	Name string         `json:"name"`
}

func (choice ToolChoice) Validate() error {
	mode := choice.Mode
	if mode == "" {
		mode = ToolChoiceNamed
	}
	switch mode {
	case ToolChoiceNamed:
		if strings.TrimSpace(choice.Name) == "" {
			return errors.New("named tool choice requires a tool name")
		}
		if strings.ContainsAny(choice.Name, " \t\r\n") {
			return fmt.Errorf("tool choice name %q contains whitespace", choice.Name)
		}
	case ToolChoiceNone:
		if strings.TrimSpace(choice.Name) != "" {
			return errors.New("none tool choice must not name a tool")
		}
	default:
		return fmt.Errorf("tool choice mode %q must be named or none", choice.Mode)
	}
	return nil
}

// Request is the provider-neutral completion request.
type Request struct {
	Model              string           `json:"model"`
	Messages           []Message        `json:"messages"`
	Tools              []ToolDefinition `json:"tools,omitempty"`
	ToolChoice         *ToolChoice      `json:"tool_choice,omitempty"`
	Temperature        float64          `json:"temperature,omitempty"`
	MaxTokens          int              `json:"max_tokens,omitempty"`
	ReasoningEffort    string           `json:"reasoning_effort,omitempty"`
	DisableThinking    bool             `json:"disable_thinking,omitempty"`
	ChatTemplateKwargs map[string]any   `json:"chat_template_kwargs,omitempty"`
}

// Response is the provider-neutral completion response.
type Response struct {
	Message      Message `json:"message"`
	Usage        Usage   `json:"usage"`
	Model        string  `json:"model,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
	// Reasoning is private provider telemetry. It is intentionally omitted
	// from ordinary response JSON and never participates in request context.
	Reasoning              string `json:"-"`
	ReasoningComplete      bool   `json:"-"`
	ReasoningTruncated     bool   `json:"-"`
	ReasoningOriginalBytes int    `json:"-"`
	Incomplete             bool   `json:"incomplete,omitempty"`
}

// StreamFunc receives assistant text deltas as they arrive.
type StreamFunc func(delta string) error

type StreamEventKind string

const (
	StreamContentDelta   StreamEventKind = "content_delta"
	StreamReasoningDelta StreamEventKind = "reasoning_delta"
	StreamToolCallDelta  StreamEventKind = "tool_call_delta"
)

// StreamEvent is the richer, backward-compatible provider stream contract.
// Content, reasoning, and tool fragments remain distinct even when the wire
// stream interleaves them.
type StreamEvent struct {
	Kind         StreamEventKind `json:"kind"`
	Delta        string          `json:"delta,omitempty"`
	ToolCall     *ToolCallDelta  `json:"tool_call,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

type ToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type StreamEventFunc func(event StreamEvent) error

// RichStreamingProvider is optional so existing provider implementations and
// tests keep the original Provider interface unchanged.
type RichStreamingProvider interface {
	StreamWithEvents(ctx context.Context, request Request, onEvent StreamEventFunc) (Response, error)
}

// Provider is the narrow boundary between the agent loop and a model API.
type Provider interface {
	Complete(ctx context.Context, request Request) (Response, error)
	Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error)
}

// ProviderContextWindow is an optional discovery seam for providers that can
// resolve model metadata at construction time. Explicit profile configuration
// remains authoritative.
type ProviderContextWindow interface {
	ContextWindowTokens(model string) (int, bool)
}

// NormalizeRequest validates and copies a request so providers receive a
// stable provider-neutral shape without mutating conversation state.
func NormalizeRequest(request Request) (Request, error) {
	if request.Model == "" {
		return Request{}, errors.New("request model is required")
	}
	if len(request.Messages) == 0 {
		return Request{}, errors.New("request must contain at least one message")
	}
	if request.Temperature < 0 || request.Temperature > 2 {
		return Request{}, fmt.Errorf("request temperature must be between 0 and 2, got %v", request.Temperature)
	}
	if request.MaxTokens < 0 {
		return Request{}, fmt.Errorf("request max tokens must not be negative, got %d", request.MaxTokens)
	}
	if err := validateReasoningEffort(request.ReasoningEffort); err != nil {
		return Request{}, err
	}

	normalized := request
	normalized.ReasoningEffort = strings.ToLower(strings.TrimSpace(request.ReasoningEffort))
	normalized.Messages = make([]Message, len(request.Messages))
	for i, message := range request.Messages {
		if err := validateRole(message.Role); err != nil {
			return Request{}, fmt.Errorf("message %d: %w", i, err)
		}
		normalized.Messages[i] = message
		if message.ToolCalls != nil {
			normalized.Messages[i].ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
			if message.Role == RoleAssistant {
				for callIndex, call := range message.ToolCalls {
					if err := validateToolCallArguments(call); err != nil {
						return Request{}, &ToolCallArgumentsError{MessageIndex: i, CallIndex: callIndex, ToolName: call.Name, CallID: call.ID, Cause: err}
					}
				}
			}
		}
		if message.Attachments != nil {
			normalized.Messages[i].Attachments = append([]AttachmentRef(nil), message.Attachments...)
		}
	}
	if request.Tools != nil {
		normalized.Tools = make([]ToolDefinition, len(request.Tools))
		for i, definition := range request.Tools {
			if err := definition.Validate(); err != nil {
				return Request{}, fmt.Errorf("tool %d: %w", i, err)
			}
			normalized.Tools[i] = definition
			normalized.Tools[i].Parameters = append(json.RawMessage(nil), definition.Parameters...)
		}
	}
	if request.ToolChoice != nil {
		if err := request.ToolChoice.Validate(); err != nil {
			return Request{}, err
		}
		choice := *request.ToolChoice
		normalized.ToolChoice = &choice
	}
	if request.ChatTemplateKwargs != nil {
		normalized.ChatTemplateKwargs = make(map[string]any, len(request.ChatTemplateKwargs))
		for key, value := range request.ChatTemplateKwargs {
			normalized.ChatTemplateKwargs[key] = value
		}
	}
	return normalized, nil
}

func validateReasoningEffort(effort string) error {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return fmt.Errorf("generation reasoning_effort %q must be one of none, minimal, low, medium, high, xhigh, or max", effort)
	}
}

func validateToolMessageSequence(messages []Message) error {
	seenCalls := make(map[string]struct{})
	pending := make(map[string]struct{})
	for messageIndex, message := range messages {
		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 {
			if len(pending) > 0 {
				return &ToolSequenceError{MessageIndex: messageIndex, Reason: "a non-tool assistant message began before the previous tool-call group completed"}
			}
			for callIndex, call := range message.ToolCalls {
				callID := strings.TrimSpace(call.ID)
				if callID == "" {
					return &ToolSequenceError{MessageIndex: messageIndex, Reason: fmt.Sprintf("assistant tool call %d has an empty call ID", callIndex+1)}
				}
				if _, exists := seenCalls[callID]; exists {
					return &ToolSequenceError{MessageIndex: messageIndex, CallID: callID, Reason: "assistant tool-call ID is duplicated"}
				}
				seenCalls[callID] = struct{}{}
				pending[callID] = struct{}{}
			}
			continue
		}
		if message.Role == RoleTool {
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				return &ToolSequenceError{MessageIndex: messageIndex, Reason: "tool result has an empty tool_call_id"}
			}
			if _, exists := pending[callID]; !exists {
				return &ToolSequenceError{MessageIndex: messageIndex, CallID: callID, Reason: "tool result is orphaned, mismatched, or duplicated"}
			}
			delete(pending, callID)
			continue
		}
		if len(pending) > 0 {
			return &ToolSequenceError{MessageIndex: messageIndex, Reason: "a non-tool message intervened while tool results were pending"}
		}
	}
	if len(pending) > 0 {
		for callID := range pending {
			return &ToolSequenceError{MessageIndex: len(messages) - 1, CallID: callID, Reason: "assistant tool call has no matching tool result"}
		}
	}
	return nil
}

func validateToolCallArguments(call ToolCall) error {
	if len(call.Arguments) > MaxProviderToolArgumentsBytes {
		return fmt.Errorf("arguments exceed %d bytes", MaxProviderToolArgumentsBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(call.Arguments))
	decoder.UseNumber()
	var value map[string]json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON values")
		}
		return err
	}
	if value == nil {
		return errors.New("arguments must be a JSON object")
	}
	return nil
}

func validateRole(role Role) error {
	switch role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return nil
	default:
		return fmt.Errorf("unsupported message role %q", role)
	}
}
