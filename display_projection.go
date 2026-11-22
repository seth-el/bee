package bee

import (
	"encoding/json"
	"io"
	"strings"
)

const invalidToolArgumentsDisplayMarker = `{"_bee_display":"invalid_tool_arguments"}`

// ProjectMessageForDisplay returns a detached, redacted projection for local
// UI/API responses. It must never be used for provider replay or durable
// persistence: those paths retain the faithful message.
func ProjectMessageForDisplay(message Message) Message {
	projected := message
	projected.Content = redactDisplayText(message.Content)
	if projected.Role == RoleAssistant && projected.Incomplete {
		projected.ToolCalls = nil
	}
	if message.ToolCalls != nil && !(projected.Role == RoleAssistant && projected.Incomplete) {
		projected.ToolCalls = make([]ToolCall, len(message.ToolCalls))
		for index, call := range message.ToolCalls {
			projected.ToolCalls[index] = ProjectToolCallForDisplay(call)
		}
	}
	if message.Attachments != nil {
		projected.Attachments = append([]AttachmentRef(nil), message.Attachments...)
	}
	return projected
}

// ProjectMessageForDisplayWithVisibility applies the current owner display
// policy to a detached message projection. It is intended for history/API
// adapters, never provider replay.
func ProjectMessageForDisplayWithVisibility(message Message, visibility DisplayVisibility) Message {
	projected := ProjectMessageForDisplay(message)
	switch visibility.toolDisplayMode() {
	case ToolDisplayOff:
		projected.ToolCalls = nil
	case ToolDisplayCompact:
		for index := range projected.ToolCalls {
			projected.ToolCalls[index] = ProjectToolCallForDisplayWithVisibility(message.ToolCalls[index], visibility)
		}
	}
	return projected
}

// ProjectToolCallForDisplay structurally redacts nested secret-shaped strings
// while guaranteeing that the displayed arguments remain a JSON object.
func ProjectToolCallForDisplay(call ToolCall) ToolCall {
	projected := call
	projected.Arguments = redactToolArgumentsForDisplay(call.Arguments)
	return projected
}

func ProjectToolCallForDisplayWithVisibility(call ToolCall, visibility DisplayVisibility) ToolCall {
	projected := ProjectToolCallForDisplay(call)
	if visibility.toolDisplayMode() == ToolDisplayCompact {
		projected.Arguments = string(boundedAuditArgs([]byte(projected.Arguments)))
	}
	return projected
}

// ProjectToolResultForDisplay redacts output/error text at the display
// boundary without changing the durable or provider-facing result.
func ProjectToolResultForDisplay(result ToolResultEvent) ToolResultEvent {
	projected := result
	projected.Output = redactDisplayText(result.Output)
	projected.Error = redactDisplayText(result.Error)
	return projected
}

func ProjectToolResultForDisplayWithVisibility(result ToolResultEvent, visibility DisplayVisibility) ToolResultEvent {
	projected := ProjectToolResultForDisplay(result)
	if visibility.toolDisplayMode() == ToolDisplayCompact {
		projected.Output = ""
	}
	return projected
}

// ProjectApprovalRequestForDisplay bounds and structurally redacts arguments
// before an owner-facing adapter renders or serializes an approval prompt.
func ProjectApprovalRequestForDisplay(request ApprovalRequest) ApprovalRequest {
	projected := request
	projected.Args = boundedAuditArgs(request.Args)
	return projected
}

func redactToolArgumentsForDisplay(arguments string) string {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return invalidToolArgumentsDisplayMarker
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalidToolArgumentsDisplayMarker
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return invalidToolArgumentsDisplayMarker
	}
	redactDisplayValue(object)
	encoded, err := json.Marshal(object)
	if err != nil {
		return invalidToolArgumentsDisplayMarker
	}
	return string(encoded)
}

func redactDisplayText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err == nil {
		var extra any
		if decoder.Decode(&extra) == io.EOF {
			switch value.(type) {
			case map[string]any, []any:
				redactDisplayValue(value)
				if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
					return string(encoded)
				}
			}
		}
	}
	return redactSecretAssignmentsForDisplay(text)
}

func redactSecretAssignmentsForDisplay(text string) string {
	markers := []string{"api_key=", "api_key:", "apikey=", "apikey:", "access_token=", "access_token:", "authorization=", "authorization:", "token=", "token:", "password=", "password:", "secret=", "secret:"}
	for _, marker := range markers {
		searchFrom := 0
		for searchFrom < len(text) {
			lower := strings.ToLower(text)
			relative := strings.Index(lower[searchFrom:], marker)
			if relative < 0 {
				break
			}
			valueStart := searchFrom + relative + len(marker)
			for valueStart < len(text) && (text[valueStart] == ' ' || text[valueStart] == '\t') {
				valueStart++
			}
			if strings.HasPrefix(strings.ToLower(text[valueStart:]), "[redacted]") {
				searchFrom = valueStart + len("[REDACTED]")
				continue
			}
			contentStart, contentEnd := displaySecretValueBounds(text, valueStart, strings.HasPrefix(marker, "authorization"))
			text = text[:contentStart] + "[REDACTED]" + text[contentEnd:]
			searchFrom = contentStart + len("[REDACTED]")
		}
	}
	return text
}

func displaySecretValueBounds(text string, start int, allowSpaces bool) (int, int) {
	if start >= len(text) {
		return start, start
	}
	if start+1 < len(text) && text[start] == '\\' && (text[start+1] == '"' || text[start+1] == '\'') {
		terminator := text[start : start+2]
		contentStart := start + 2
		if relative := strings.Index(text[contentStart:], terminator); relative >= 0 {
			return contentStart, contentStart + relative
		}
		return contentStart, len(text)
	}
	if text[start] == '"' || text[start] == '\'' {
		quote := text[start]
		for index := start + 1; index < len(text); index++ {
			if text[index] == quote && (index == start+1 || text[index-1] != '\\') {
				return start + 1, index
			}
		}
		return start + 1, len(text)
	}
	end := start
	delimiters := " \t\r\n,;}])"
	if allowSpaces {
		delimiters = "\r\n,;}])"
	}
	for end < len(text) && !strings.ContainsRune(delimiters, rune(text[end])) {
		end++
	}
	return start, end
}

func redactDisplayValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSecretKey(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			if text, ok := child.(string); ok {
				typed[key] = redactDisplayText(text)
				continue
			}
			redactDisplayValue(child)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = redactDisplayText(text)
				continue
			}
			redactDisplayValue(child)
		}
	}
}
