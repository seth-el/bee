package bee

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDisplayProjectionRedactsStructuredAndInvalidToolArguments(t *testing.T) {
	projected := ProjectMessageForDisplay(Message{
		Role:    RoleAssistant,
		Content: "password=raw-password token=raw-token\nAuthorization: Bearer raw-bearer-token",
		ToolCalls: []ToolCall{{
			Name:      "exec",
			Arguments: `{"command":"printf 'token=raw-token' && printf 'password=\\\"quoted-password\\\"' && printf 'PlexOnlineToken=\\\"quoted-token\\\"'","password":"raw-password"}`,
		}, {
			Name:      "exec",
			Arguments: `{"command":"broken"oops"}`,
		}},
	})
	if strings.Contains(projected.Content, "raw-password") || strings.Contains(projected.Content, "raw-token") || strings.Contains(projected.Content, "raw-bearer-token") {
		t.Fatalf("content was not redacted: %q", projected.Content)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(projected.ToolCalls[0].Arguments), &object); err != nil {
		t.Fatal(err)
	}
	if object["password"] != "[REDACTED]" || strings.Contains(projected.ToolCalls[0].Arguments, "raw-token") || strings.Contains(projected.ToolCalls[0].Arguments, "quoted-password") || strings.Contains(projected.ToolCalls[0].Arguments, "quoted-token") {
		t.Fatalf("structured arguments were not redacted: %s", projected.ToolCalls[0].Arguments)
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(projected.ToolCalls[1].Arguments), &marker); err != nil {
		t.Fatal(err)
	}
	if marker["_bee_display"] != "invalid_tool_arguments" {
		t.Fatalf("invalid marker = %#v", marker)
	}
}
