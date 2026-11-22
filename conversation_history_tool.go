package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConversationHistorySearchTool exposes transient, bounded local conversation
// history. It is read-only and only projects user/assistant message excerpts.
type ConversationHistorySearchTool struct {
	Service ConversationHistoryService
	Actor   MemoryActor
}

type conversationHistorySearchToolArgs struct {
	Query           string    `json:"query"`
	SessionID       SessionID `json:"session_id,omitempty"`
	MaxSessions     int       `json:"max_sessions,omitempty"`
	MaxMessages     int       `json:"max_messages,omitempty"`
	Limit           int       `json:"limit,omitempty"`
	MaxSnippetBytes int       `json:"max_snippet_bytes,omitempty"`
	Since           string    `json:"since,omitempty"`
	Before          string    `json:"before,omitempty"`
	MaxBytes        int       `json:"max_bytes,omitempty"`
}

func (tool ConversationHistorySearchTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        ConversationHistorySearchToolName,
		Description: "Search bounded local conversation history for prior discussions. Results are transient reference excerpts from user and assistant messages only; tool arguments, tool output, hidden reasoning, credentials, and durable memory are excluded.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"session_id":{"type":"string"},"max_sessions":{"type":"integer"},"max_messages":{"type":"integer"},"limit":{"type":"integer"},"max_snippet_bytes":{"type":"integer"},"max_bytes":{"type":"integer"},"since":{"type":"string","description":"RFC3339; only messages at or after this time"},"before":{"type":"string","description":"RFC3339; only messages before this time"}},"required":["query"]}`),
	}
}

func (tool ConversationHistorySearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Service == nil {
		return "", errors.New("conversation history search service is not configured")
	}
	var input conversationHistorySearchToolArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("conversation_search arguments: %w", err)
	}
	if strings.TrimSpace(input.Query) == "" {
		return "", errors.New("conversation_search query is required")
	}
	parseTime := func(value, name string) (time.Time, error) {
		if strings.TrimSpace(value) == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
		if err != nil {
			return time.Time{}, fmt.Errorf("conversation_search %s must be RFC3339", name)
		}
		return parsed.UTC(), nil
	}
	since, err := parseTime(input.Since, "since")
	if err != nil {
		return "", err
	}
	before, err := parseTime(input.Before, "before")
	if err != nil {
		return "", err
	}
	result, err := tool.Service.Search(ctx, ConversationHistorySearchRequest{
		Since:           since,
		Before:          before,
		Actor:           tool.Actor,
		Query:           input.Query,
		SessionID:       input.SessionID,
		MaxSessions:     input.MaxSessions,
		MaxMessages:     input.MaxMessages,
		MaxResults:      input.Limit,
		MaxSnippetBytes: input.MaxSnippetBytes,
		MaxBytes:        input.MaxBytes,
	})
	if err != nil {
		return "", err
	}
	return marshalConversationHistoryToolResult(result)
}

func marshalConversationHistoryToolResult(result ConversationHistorySearchResult) (string, error) {
	// Re-project defensively so custom service implementations cannot smuggle
	// unbounded or non-history fields into the model-facing contract.
	out := ConversationHistorySearchResult{Total: result.Total, MoreAvailable: result.MoreAvailable}
	for _, hit := range result.Hits {
		if len(out.Hits) >= MaxConversationHistoryResults {
			out.MoreAvailable = true
			break
		}
		candidate := ConversationHistoryHit{
			SessionID:        hit.SessionID,
			SessionTitle:     truncateUTF8(redactString(hit.SessionTitle), 256),
			MessageID:        truncateUTF8(redactString(hit.MessageID), 128),
			Sequence:         hit.Sequence,
			Role:             hit.Role,
			At:               hit.At.UTC(),
			SessionCreatedAt: hit.SessionCreatedAt.UTC(),
			SessionUpdatedAt: hit.SessionUpdatedAt.UTC(),
			Snippet:          truncateUTF8(redactString(hit.Snippet), MaxConversationHistorySnippet),
		}
		if candidate.Role != RoleUser && candidate.Role != RoleAssistant {
			continue
		}
		out.Hits = append(out.Hits, candidate)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if len(data) > MaxConversationHistoryBytes {
		out.Hits = nil
		out.MoreAvailable = result.Total > 0 || result.MoreAvailable
		data, err = json.Marshal(out)
		if err != nil {
			return "", err
		}
	}
	return string(data), nil
}

var _ Tool = ConversationHistorySearchTool{}
