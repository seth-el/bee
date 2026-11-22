package bee

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderTUIEventsUsesCompactActivityTimelineAndStableBoundaries(t *testing.T) {
	events := make(chan RunEvent, 8)
	events <- RunEvent{Type: RunStart, RunID: "run-secret-noise"}
	events <- RunEvent{Type: RunSession, SessionID: "session-1"}
	events <- RunEvent{Type: RunToolCall, SessionID: "session-1", ToolCall: &ToolCall{Name: "read_file"}}
	events <- RunEvent{Type: RunToolResult, SessionID: "session-1", ToolResult: &ToolResultEvent{ToolName: "read_file", Output: "private output"}}
	events <- RunEvent{Type: RunAssistantDelta, SessionID: "session-1", Delta: "answer without newline"}
	events <- RunEvent{Type: RunAssistant, SessionID: "session-1", Message: &Message{Role: RoleAssistant, Content: "answer without newline"}}
	events <- RunEvent{Type: RunUsage, SessionID: "session-1", Usage: &UsageEvent{Usage: Usage{TotalTokens: 12}}}
	events <- RunEvent{Type: RunEnd, SessionID: "session-1"}
	close(events)

	var output bytes.Buffer
	sessionID, err := renderTUIEvents(&output, events)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-1" {
		t.Fatalf("session id=%q", sessionID)
	}
	got := output.String()
	for _, want := range []string{"● Read file", "•\x1b[0m answer without newline\n", "session session-1", "12 tokens · 1 provider call"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output=%q missing %q", got, want)
		}
	}
	for _, hidden := range []string{"Using read_file", "completed · read_file", "private output"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("compact output=%q contains %q", got, hidden)
		}
	}
	for _, unwanted := range []string{"run-secret-noise", "tool:", "tool done:", "\x1b[90mdone\x1b[0m"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output=%q contains %q", got, unwanted)
		}
	}
}

func TestRenderTUIEventsShowsReasoningArgumentsAndColoredDiffs(t *testing.T) {
	events := make(chan RunEvent, 8)
	events <- RunEvent{Type: RunStart}
	events <- RunEvent{Type: RunReasoningDelta, Reasoning: &ReasoningRunEvent{Delta: "inspect the change"}}
	events <- RunEvent{Type: RunReasoningFinal, Reasoning: &ReasoningRunEvent{Content: "inspect the change", Complete: true}}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{Name: "git_diff", Arguments: `{"repo_path":".","token":"secret-value"}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{ToolName: "git_diff", Output: `{"repository":"/work/demo","ref":"HEAD","has_changes":true,"diff":"@@ -1 +1 @@\n-old\n+new\n","truncated":false}`}}
	events <- RunEvent{Type: RunEnd}
	close(events)

	var output bytes.Buffer
	visibility := DisplayVisibility{ToolCalls: true, ToolMode: ToolDisplayFull, Thinking: true}
	if _, err := renderTUIEventsFromSessionWithVisibility(&output, events, "", visibility); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"◆ Thinking", "inspect the change", "● Using git_diff", `"repo_path": "."`, `"token": "[REDACTED]"`, "┌ Diff · demo ↔ HEAD", "-old", "+new"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output=%q missing %q", got, want)
		}
	}
	if strings.Contains(got, "secret-value") {
		t.Fatalf("tool argument secret reached TUI: %q", got)
	}
	if !strings.Contains(got, tuiRed+"  │ -old") || !strings.Contains(got, tuiGreen+"  │ +new") {
		t.Fatalf("diff did not use addition/removal colors: %q", got)
	}
}

func TestRenderTUIEventsCompactGroupsExplorationAndPreviewsCommands(t *testing.T) {
	events := make(chan RunEvent, 12)
	events <- RunEvent{Type: RunStart}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "read", ToolName: "read_file", Output: "private file body"}}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "grep", Name: "grep", Arguments: `{"path":".","query":"ToolCall"}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "grep", ToolName: "grep", Output: "private matches"}}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "exec", Name: "exec", Arguments: `{"command":"go test ./..."}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "exec", ToolName: "exec", Output: `{"status":"exited","exit_code":0,"output":"ok one\nok two\nok three\nok four\nok five\nok six"}`}}
	events <- RunEvent{Type: RunEnd}
	close(events)

	var output bytes.Buffer
	if _, err := renderTUIEvents(&output, events); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"● Explored", "  ├─ Read main.go", `  └─ Searched "ToolCall"`, "● Ran go test ./...", "  └─ Output", "ok one", "lines omitted", "ok six"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output=%q missing %q", got, want)
		}
	}
	for _, hidden := range []string{"private file body", "private matches", `"command"`} {
		if strings.Contains(got, hidden) {
			t.Fatalf("output=%q contains compact-hidden detail %q", got, hidden)
		}
	}
}

func TestCompactToolVisualLinesUseHierarchyAndScopedFailureColor(t *testing.T) {
	success := ToolResultEvent{CallID: "read", ToolName: "read_file", Output: "contents"}
	failure := ToolResultEvent{CallID: "grep", ToolName: "grep", Error: "permission denied"}
	activities := []tuiToolActivity{
		{Call: ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`}, Result: &success},
		{Call: ToolCall{ID: "grep", Name: "grep", Arguments: `{"query":"needle"}`}, Result: &failure},
	}

	lines := tuiCompactToolVisualLines(activities, 80)
	if len(lines) != 3 {
		t.Fatalf("visual lines=%#v", lines)
	}
	if lines[0].text != "● Explored · 1 failed" || !strings.HasPrefix(lines[0].rendered, tuiRed+tuiBold) {
		t.Fatalf("header=%#v", lines[0])
	}
	if lines[1].text != "  ├─ Read main.go" || !strings.HasPrefix(lines[1].rendered, tuiSubtle) {
		t.Fatalf("successful branch=%#v", lines[1])
	}
	if lines[2].text != "  └─ Search failed for \"needle\" · permission denied" || !strings.HasPrefix(lines[2].rendered, tuiRed) {
		t.Fatalf("failed branch=%#v", lines[2])
	}

	successLines := tuiCompactToolVisualLines(activities[:1], 80)
	if len(successLines) != 1 || !strings.HasPrefix(successLines[0].rendered, tuiBrightGreen+tuiBold) {
		t.Fatalf("successful summary=%#v", successLines)
	}

	groupedNarrow := tuiCompactToolVisualLines(activities, 24)
	if len(groupedNarrow) < 4 || !strings.HasPrefix(groupedNarrow[len(groupedNarrow)-1].text, "     ") {
		t.Fatalf("narrow branch wrapping=%#v", groupedNarrow)
	}
}

func TestRenderTUIEventsPadsCompactToolBlocks(t *testing.T) {
	events := make(chan RunEvent, 6)
	events <- RunEvent{Type: RunStart}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "read", ToolName: "read_file", Output: "contents"}}
	events <- RunEvent{Type: RunAssistantDelta, Delta: "done"}
	events <- RunEvent{Type: RunAssistant, Message: &Message{Role: RoleAssistant, Content: "done"}}
	events <- RunEvent{Type: RunEnd}
	close(events)

	var output bytes.Buffer
	if _, err := renderTUIEvents(&output, events); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "\r\x1b[2K\n"+tuiBrightGreen+tuiBold+"● Read main.go") {
		t.Fatalf("compact tool lacks leading padding or bright-green summary: %q", got)
	}
	if !strings.Contains(got, "Read main.go"+tuiReset+"\n\n"+tuiGreen+"•"+tuiReset+" done") {
		t.Fatalf("compact tool lacks trailing padding: %q", got)
	}
}

func TestCompactToolDisplayHighlightsFailuresAndRedactsDetails(t *testing.T) {
	readResult := ToolResultEvent{CallID: "read", ToolName: "read_file", Error: "permission denied password=private-value"}
	display, failed := tuiCompactToolActivitiesDisplay([]tuiToolActivity{{
		Call:   ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"private.txt"}`},
		Result: &readResult,
	}})
	if !failed || !strings.Contains(display.Text, "Failed to read private.txt") || !strings.Contains(display.Text, "[REDACTED]") || strings.Contains(display.Text, "private-value") {
		t.Fatalf("failed read display=%q failed=%t", display.Text, failed)
	}

	execResult := ToolResultEvent{CallID: "exec", ToolName: "exec", Output: `{"status":"exited","exit_code":2,"output":"build failed"}`}
	display, failed = tuiCompactToolActivitiesDisplay([]tuiToolActivity{{
		Call:   ToolCall{ID: "exec", Name: "exec", Arguments: `{"command":"go build ./..."}`},
		Result: &execResult,
	}})
	if !failed || !strings.Contains(display.Text, "Command failed: go build ./... · exit 2") || !strings.Contains(display.Text, "build failed") {
		t.Fatalf("failed exec display=%q failed=%t", display.Text, failed)
	}
}

func TestRenderTUIEventsCanHideThinkingAndToolDetails(t *testing.T) {
	events := make(chan RunEvent, 8)
	events <- RunEvent{Type: RunStart}
	events <- RunEvent{Type: RunReasoningDelta, Reasoning: &ReasoningRunEvent{Delta: "private reasoning"}}
	events <- RunEvent{Type: RunReasoningFinal, Reasoning: &ReasoningRunEvent{Content: "private reasoning", Complete: true}}
	events <- RunEvent{Type: RunToolCall, ToolCall: &ToolCall{Name: "exec", Arguments: `{"command":"secret command"}`}}
	events <- RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{ToolName: "exec", Output: "secret output"}}
	events <- RunEvent{Type: RunAssistantDelta, Delta: "public answer"}
	events <- RunEvent{Type: RunAssistant, Message: &Message{Role: RoleAssistant, Content: "public answer"}}
	events <- RunEvent{Type: RunEnd}
	close(events)

	var output bytes.Buffer
	visibility := DisplayVisibility{ToolCalls: false, Thinking: false}
	if _, err := renderTUIEventsFromSessionWithVisibility(&output, events, "", visibility); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "public answer") {
		t.Fatalf("output omitted assistant answer: %q", got)
	}
	for _, hidden := range []string{"private reasoning", "secret command", "secret output", "Thinking", "Using exec"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("output=%q contains hidden detail %q", got, hidden)
		}
	}
}

func TestTUISafeTextNormalizesFormattingAndStripsTerminalControls(t *testing.T) {
	got := tuiSafeText("\x1b[31mred\x1b[0m\r\nnext\tcolumn\x00\x7f\x1b]0;title\x07")
	if got != "red\nnext    column" {
		t.Fatalf("safe text=%q", got)
	}
	if compact := tuiCompactText("  a\n\tb   c  ", 7); compact != "a b c" {
		t.Fatalf("compact text=%q", compact)
	}
	if compact := tuiCompactText("abcdefgh", 5); compact != "abcd…" {
		t.Fatalf("truncated compact text=%q", compact)
	}
}

func TestTUIHeaderShowsContextWithoutRoleLabels(t *testing.T) {
	config := DefaultConfig()
	config.ProviderProfile = "local-main"
	app := &Application{Config: config, ProviderProfile: "local-main", Capabilities: AssistantCapabilities{ScopeRoot: "/work/project"}}
	var output bytes.Buffer
	if err := renderTUIHeader(&output, app, "session-a", "override-model"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"╭─", "Bee", "override-model · local-main", "/work/project", "session session-a", "/help for commands"} {
		if !strings.Contains(got, want) {
			t.Fatalf("header=%q missing %q", got, want)
		}
	}
	if strings.Contains(got, "user turn") || strings.Contains(got, "agent turn") || strings.Contains(got, "bee>") {
		t.Fatalf("header retained role-block UI: %q", got)
	}
}
