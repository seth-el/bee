package bee

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	tuiMaxToolArgumentDisplayBytes = 6 << 10
	tuiMaxToolOutputDisplayBytes   = 10 << 10
	tuiMaxDiffDisplayBytes         = 24 << 10
	tuiMaxDetailDisplayLines       = 240
	tuiMaxReasoningDisplayBytes    = 24 << 10
	tuiCompactToolOutputLines      = 5
	tuiCompactToolOutputBytes      = 2 << 10
)

type tuiToolDisplay struct {
	Text string
	Diff string
}

type tuiToolActivityClass uint8

const (
	tuiToolActivityOther tuiToolActivityClass = iota
	tuiToolActivityExploration
	tuiToolActivityCommand
	tuiToolActivityChange
)

type tuiToolActivity struct {
	Call   ToolCall
	Result *ToolResultEvent
}

type tuiCompactToolAction struct {
	Class   tuiToolActivityClass
	Active  string
	Done    string
	Failure string
}

func tuiCompactToolCallAction(call ToolCall) tuiCompactToolAction {
	name := strings.TrimSpace(call.Name)
	args := tuiToolArgumentObject(call)
	path := tuiToolStringArgument(args, "path")
	if path == "" {
		path = "."
	}
	switch name {
	case "read_file":
		target := path
		if target == "." {
			target = "file"
		}
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Reading " + target, Done: "Read " + target, Failure: "Failed to read " + target}
	case "list_files":
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Listing " + path, Done: "Listed " + path, Failure: "Failed to list " + path}
	case "find_files":
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "pattern")) + tuiToolScopeSuffix(path)
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Finding " + target, Done: "Found files matching " + target, Failure: "Failed to find " + target}
	case "grep":
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "query")) + tuiToolScopeSuffix(path)
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Searching " + target, Done: "Searched " + target, Failure: "Search failed for " + target}
	case "memory_search":
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "query"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Searching memory for " + target, Done: "Searched memory for " + target, Failure: "Memory search failed for " + target}
	case "conversation_search":
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "query"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Searching conversations for " + target, Done: "Searched conversations for " + target, Failure: "Conversation search failed for " + target}
	case "web_search":
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "query"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Searching the web for " + target, Done: "Searched the web for " + target, Failure: "Web search failed for " + target}
	case "web_research":
		target := tuiQuotedToolTarget(firstNonemptyToolArgument(args, "query", "objective", "prompt"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Researching " + target, Done: "Researched " + target, Failure: "Research failed for " + target}
	case "web_fetch":
		target := tuiCompactURL(tuiToolStringArgument(args, "url"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Fetching " + target, Done: "Fetched " + target, Failure: "Failed to fetch " + target}
	case "media_search":
		kind := tuiToolStringArgument(args, "kind")
		if kind == "" {
			kind = "media"
		}
		target := tuiQuotedToolTarget(tuiToolStringArgument(args, "query"))
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Searching " + kind + " for " + target, Done: "Searched " + kind + " for " + target, Failure: "Media search failed for " + target}
	case "memory_show":
		target := tuiToolStringArgument(args, "id")
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Reading memory " + target, Done: "Read memory " + target, Failure: "Failed to read memory " + target}
	case "git_diff":
		target := firstNonemptyToolArgument(args, "path", "repo_path")
		if target == "" {
			target = "working tree"
		}
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Reviewing diff for " + target, Done: "Reviewed diff for " + target, Failure: "Failed to review diff for " + target}
	case "git_staged_review":
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Reviewing staged changes", Done: "Reviewed staged changes", Failure: "Failed to review staged changes"}
	case "process_list", "service_list", "scratch_list", "mutation_list", "artifact_list":
		target := strings.ReplaceAll(name, "_", " ")
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Inspecting " + target, Done: "Inspected " + target, Failure: "Failed to inspect " + target}
	case "process_info", "service_status", "service_logs", "scratch_get", "mutation_show":
		target := firstNonemptyToolArgument(args, "unit", "pid", "key", "id")
		label := strings.ReplaceAll(name, "_", " ")
		if target != "" {
			label += " " + target
		}
		return tuiCompactToolAction{Class: tuiToolActivityExploration, Active: "Inspecting " + label, Done: "Inspected " + label, Failure: "Failed to inspect " + label}
	case "write_file":
		if path == "." {
			path = "file"
		}
		operation := tuiToolStringArgument(args, "operation")
		if operation == writeOperationEdit {
			removed := tuiTextLineCount(tuiToolStringArgumentRaw(args, "old_text"))
			added := tuiTextLineCount(tuiToolStringArgumentRaw(args, "new_text"))
			detail := fmt.Sprintf("%s · +%d −%d", path, added, removed)
			return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Editing " + detail, Done: "Updated " + detail, Failure: "Failed to update " + path}
		}
		bytes := len(tuiToolStringArgumentRaw(args, "content"))
		detail := path
		if bytes > 0 {
			detail += fmt.Sprintf(" · %d bytes", bytes)
		}
		if operation == writeOperationAppend {
			return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Appending " + detail, Done: "Appended " + detail, Failure: "Failed to append " + path}
		}
		if operation == writeOperationReplace {
			return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Replacing " + detail, Done: "Replaced " + detail, Failure: "Failed to replace " + path}
		}
		return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Writing " + detail, Done: "Wrote " + detail, Failure: "Failed to write " + path}
	case "edit_file":
		if path == "." {
			path = "file"
		}
		removed := tuiTextLineCount(tuiToolStringArgumentRaw(args, "old"))
		added := tuiTextLineCount(tuiToolStringArgumentRaw(args, "new"))
		detail := fmt.Sprintf("%s · +%d −%d", path, added, removed)
		return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Editing " + detail, Done: "Updated " + detail, Failure: "Failed to update " + path}
	case "git_stage":
		count := len(tuiToolStringArrayArgument(args, "paths"))
		detail := "files"
		if count == 1 {
			detail = "1 file"
		} else if count > 1 {
			detail = fmt.Sprintf("%d files", count)
		}
		return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Staging " + detail, Done: "Staged " + detail, Failure: "Failed to stage " + detail}
	case "git_commit":
		message := tuiQuotedToolTarget(tuiToolStringArgument(args, "message"))
		return tuiCompactToolAction{Class: tuiToolActivityChange, Active: "Committing " + message, Done: "Committed " + message, Failure: "Commit failed for " + message}
	case ExecToolName:
		command := tuiToolStringArgument(args, "command")
		if command == "" {
			command = "shell command"
		}
		return tuiCompactToolAction{Class: tuiToolActivityCommand, Active: "Running " + command, Done: "Ran " + command, Failure: "Command failed: " + command}
	case "run_command":
		command := strings.Join(tuiToolStringArrayArgument(args, "argv"), " ")
		if command == "" {
			command = "command"
		}
		return tuiCompactToolAction{Class: tuiToolActivityCommand, Active: "Running " + tuiCompactText(command, 180), Done: "Ran " + tuiCompactText(command, 180), Failure: "Command failed: " + tuiCompactText(command, 160)}
	case TerminalToolName:
		action := tuiToolStringArgument(args, "action")
		command := firstNonemptyToolArgument(args, "command", "input")
		if command != "" && (action == "open" || action == "run" || action == "submit") {
			return tuiCompactToolAction{Class: tuiToolActivityCommand, Active: "Running " + command, Done: "Ran " + command, Failure: "Terminal command failed: " + command}
		}
		if action == "read" {
			return tuiCompactToolAction{Class: tuiToolActivityCommand, Active: "Waiting for terminal", Done: "Read terminal output", Failure: "Failed to read terminal output"}
		}
		label := strings.TrimSpace(action + " terminal")
		return tuiCompactToolAction{Class: tuiToolActivityCommand, Active: "Using " + label, Done: "Used " + label, Failure: "Failed to use " + label}
	default:
		label := strings.ReplaceAll(tuiCompactText(name, 80), "_", " ")
		if label == "" {
			label = "tool"
		}
		target := firstNonemptyToolArgument(args, "path", "query", "url", "unit", "id", "name")
		if target != "" {
			label += " · " + target
		}
		return tuiCompactToolAction{Class: tuiToolActivityOther, Active: "Using " + label, Done: "Used " + label, Failure: "Failed " + label}
	}
}

func tuiCompactToolActivitiesDisplay(activities []tuiToolActivity) (tuiToolDisplay, bool) {
	if len(activities) == 0 {
		return tuiToolDisplay{}, false
	}
	allExploration := true
	active := false
	failedCount := 0
	for _, activity := range activities {
		action := tuiCompactToolCallAction(activity.Call)
		allExploration = allExploration && action.Class == tuiToolActivityExploration
		if activity.Result == nil {
			active = true
		} else if failed, _, _ := tuiCompactToolResultState(activity); failed {
			failedCount++
		}
	}
	if allExploration && len(activities) > 1 {
		header := "Explored"
		if active {
			header = "Exploring"
		}
		if failedCount > 0 {
			header += fmt.Sprintf(" · %d failed", failedCount)
		}
		lines := []string{header}
		for index, activity := range activities {
			action := tuiCompactToolCallAction(activity.Call)
			line := action.Done
			if activity.Result == nil {
				line = action.Active
			} else if failed, suffix, output := tuiCompactToolResultState(activity); failed {
				line = action.Failure + suffix
				if detail := tuiCompactText(output, 160); detail != "" {
					line += " · " + detail
				}
			}
			branch := "├─ "
			if index == len(activities)-1 {
				branch = "└─ "
			}
			lines = append(lines, branch+line)
		}
		return tuiToolDisplay{Text: strings.Join(lines, "\n")}, failedCount > 0
	}

	activity := activities[0]
	action := tuiCompactToolCallAction(activity.Call)
	text := action.Active
	failed, suffix, output := tuiCompactToolResultState(activity)
	if activity.Result != nil {
		text = action.Done + suffix
		if failed {
			text = action.Failure + suffix
		}
	}
	if activity.Result != nil && output != "" && (failed || action.Class == tuiToolActivityCommand || action.Class == tuiToolActivityOther) {
		label := "Output"
		if failed {
			label = "Error"
		}
		text += "\n└─ " + label + "\n" + tuiCompactOutputPreview(output, tuiCompactToolOutputLines)
	}
	return tuiToolDisplay{Text: text}, failed
}

func tuiCompactToolResultState(activity tuiToolActivity) (failed bool, suffix, output string) {
	if activity.Result == nil {
		return false, "", ""
	}
	result := ProjectToolResultForDisplay(*activity.Result)
	if strings.TrimSpace(result.Error) != "" {
		return true, "", result.Error
	}
	output = result.Output
	if activity.Call.Name != ExecToolName && activity.Call.Name != TerminalToolName && activity.Call.Name != "run_command" {
		return false, "", output
	}
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(result.Output))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false, "", output
	}
	if nested, ok := value["read"].(map[string]any); ok {
		value = nested
	}
	if text, ok := value["output"].(string); ok {
		output = text
	}
	exitCode := ""
	if number, ok := value["exit_code"].(json.Number); ok {
		exitCode = number.String()
	} else if number, ok := value["exit_code"].(float64); ok {
		exitCode = fmt.Sprintf("%.0f", number)
	}
	status, _ := value["status"].(string)
	failed = status == "failed" || status == "canceled" || status == "exited_nonzero" || (exitCode != "" && exitCode != "0")
	if exitCode != "" && exitCode != "0" {
		suffix = " · exit " + exitCode
	} else if status == "running" {
		suffix = " · still running"
	} else if status == "canceled" {
		suffix = " · canceled"
	}
	return failed, suffix, output
}

func tuiCompactOutputPreview(value string, maxLines int) string {
	value = strings.TrimSpace(tuiBoundDisplayText(value, tuiCompactToolOutputBytes, 0))
	if value == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	if len(lines) > maxLines && maxLines >= 3 {
		omitted := len(lines) - (maxLines - 1)
		lines = append(append(append([]string(nil), lines[:2]...), fmt.Sprintf("… %d lines omitted", omitted)), lines[len(lines)-(maxLines-3):]...)
	}
	for index := range lines {
		lines[index] = "   " + lines[index]
	}
	return strings.Join(lines, "\n")
}

func tuiToolArgumentObject(call ToolCall) map[string]any {
	projected := ProjectToolCallForDisplay(call)
	decoder := json.NewDecoder(strings.NewReader(projected.Arguments))
	decoder.UseNumber()
	var args map[string]any
	if decoder.Decode(&args) != nil || args == nil {
		return map[string]any{}
	}
	return args
}

func tuiToolStringArgument(args map[string]any, key string) string {
	return tuiCompactText(tuiToolStringArgumentRaw(args, key), 180)
}

func tuiToolStringArgumentRaw(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return tuiSafeText(value)
}

func tuiToolStringArrayArgument(args map[string]any, key string) []string {
	values, _ := args[key].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, tuiCompactText(text, 120))
		}
	}
	return result
}

func firstNonemptyToolArgument(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := tuiToolStringArgument(args, key); value != "" {
			return value
		}
		if number, ok := args[key].(json.Number); ok {
			return number.String()
		}
	}
	return ""
}

func tuiQuotedToolTarget(value string) string {
	value = tuiCompactText(value, 120)
	if value == "" {
		return "items"
	}
	return fmt.Sprintf("%q", value)
}

func tuiToolScopeSuffix(path string) string {
	path = tuiCompactText(path, 100)
	if path == "" || path == "." {
		return ""
	}
	return " in " + path
}

func tuiCompactURL(value string) string {
	value = tuiCompactText(value, 180)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return value
	}
	display := parsed.Host + parsed.EscapedPath()
	if display == "" {
		return value
	}
	return tuiCompactText(display, 160)
}

func tuiTextLineCount(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func tuiToolCallDisplay(call ToolCall) tuiToolDisplay {
	hasArguments := strings.TrimSpace(call.Arguments) != ""
	projected := ProjectToolCallForDisplay(call)
	name := tuiCompactText(projected.Name, 80)
	if name == "" {
		name = "unnamed tool"
	}
	display := tuiToolDisplay{Text: "Using " + name}
	if arguments := tuiPrettyJSON(projected.Arguments, tuiMaxToolArgumentDisplayBytes); hasArguments && arguments != "" && arguments != "{}" {
		display.Text += "\n" + arguments
	}
	if projected.Name == "edit_file" || (projected.Name == "write_file" && tuiToolStringArgument(tuiToolArgumentObject(projected), "operation") == writeOperationEdit) {
		display.Diff = tuiEditCallDiff(projected.Arguments)
	}
	return display
}

func tuiToolResultDisplay(result ToolResultEvent) tuiToolDisplay {
	projected := ProjectToolResultForDisplay(result)
	name := tuiCompactText(projected.ToolName, 80)
	if name == "" {
		name = "tool"
	}
	callID := tuiCompactText(redactDisplayText(projected.CallID), MaxPlanStatusReferenceBytes)
	state := "completed · " + name
	if callID != "" {
		if planEvidenceToolIsDiscoverable(projected.ToolName) {
			state += "\nevidence: tool_result/" + callID
		} else {
			state += "\ncall: " + callID
		}
	}
	if projected.Error != "" {
		state = "failed · " + name
		if callID != "" {
			state += "\ncall: " + callID
		}
		return tuiToolDisplay{Text: state + "\n" + tuiBoundDisplayText(projected.Error, tuiMaxToolOutputDisplayBytes, tuiMaxDetailDisplayLines)}
	}
	display := tuiToolDisplay{Text: state}
	if diff, ok := tuiResultDiff(projected.ToolName, projected.Output); ok {
		display.Diff = diff
		return display
	}
	if output := tuiPrettyJSON(projected.Output, tuiMaxToolOutputDisplayBytes); output != "" {
		display.Text += "\n" + output
	}
	return display
}

func tuiPrettyJSON(value string, maxBytes int) string {
	value = strings.TrimSpace(tuiSafeText(value))
	if value == "" {
		return ""
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return tuiBoundDisplayText(value, maxBytes, tuiMaxDetailDisplayLines)
		}
		decoded = tuiCompactDisplayValue(decoded)
		if encoded, err := json.MarshalIndent(decoded, "", "  "); err == nil {
			value = string(encoded)
		}
	}
	return tuiBoundDisplayText(value, maxBytes, tuiMaxDetailDisplayLines)
}

func tuiCompactDisplayValue(value any) any {
	switch typed := value.(type) {
	case string:
		const retained = 1024
		if len(typed) > retained {
			return truncateUTF8(typed, retained) + fmt.Sprintf("… [%d bytes]", len(typed))
		}
		return typed
	case []any:
		limit := len(typed)
		if limit > 32 {
			limit = 32
		}
		result := make([]any, 0, limit+1)
		for _, item := range typed[:limit] {
			result = append(result, tuiCompactDisplayValue(item))
		}
		if limit < len(typed) {
			result = append(result, fmt.Sprintf("… [%d more items]", len(typed)-limit))
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = tuiCompactDisplayValue(item)
		}
		return result
	default:
		return value
	}
}

func tuiEditCallDiff(arguments string) string {
	var input struct {
		Path    string `json:"path"`
		Old     string `json:"old"`
		New     string `json:"new"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if json.Unmarshal([]byte(arguments), &input) != nil {
		return ""
	}
	if input.OldText != "" || input.NewText != "" {
		input.Old, input.New = input.OldText, input.NewText
	}
	if input.Old == "" && input.New == "" {
		return ""
	}
	path := tuiCompactText(input.Path, 120)
	if path == "" {
		path = "file"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Proposed diff · %s\n--- %s\n+++ %s\n@@ exact replacement @@\n", path, path, path)
	for _, line := range strings.Split(input.Old, "\n") {
		builder.WriteString("-" + line + "\n")
	}
	for _, line := range strings.Split(input.New, "\n") {
		builder.WriteString("+" + line + "\n")
	}
	return tuiBoundDisplayText(strings.TrimSuffix(builder.String(), "\n"), tuiMaxDiffDisplayBytes, tuiMaxDetailDisplayLines)
}

func tuiResultDiff(toolName, output string) (string, bool) {
	if toolName != "git_diff" && toolName != "git_staged_review" {
		return "", false
	}
	var envelope struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Path       string `json:"path"`
		Diff       string `json:"diff"`
		Truncated  bool   `json:"truncated"`
		Note       string `json:"note"`
	}
	if json.Unmarshal([]byte(output), &envelope) != nil {
		return "", false
	}
	title := "Diff"
	if toolName == "git_staged_review" {
		title = "Staged diff"
	}
	if envelope.Path != "" {
		title += " · " + tuiCompactText(envelope.Path, 100)
	} else if envelope.Repository != "" {
		title += " · " + tuiCompactText(filepath.Base(envelope.Repository), 80)
	}
	if envelope.Ref != "" {
		title += " ↔ " + tuiCompactText(envelope.Ref, 40)
	}
	body := strings.TrimRight(envelope.Diff, "\n")
	if body == "" {
		body = "(no changes)"
	}
	if envelope.Truncated {
		note := strings.TrimSpace(envelope.Note)
		if note == "" {
			note = "diff output truncated"
		}
		body += "\n… " + note
	}
	return title + "\n" + tuiBoundDisplayText(body, tuiMaxDiffDisplayBytes, tuiMaxDetailDisplayLines), true
}

func tuiBoundDisplayText(value string, maxBytes, maxLines int) string {
	value = tuiSafeText(value)
	truncated := false
	if maxBytes > 0 && len(value) > maxBytes {
		value = truncateUTF8(value, maxBytes)
		truncated = true
	}
	lines := strings.Split(value, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	value = strings.Join(lines, "\n")
	if truncated {
		value = strings.TrimRight(value, "\n") + "\n… display truncated"
	}
	return value
}

func tuiDetailVisualLines(kind tuiBlockKind, text string, width int) []tuiVisualLine {
	prefix := "● "
	continuation := "  "
	style := tuiCyan
	switch kind {
	case tuiBlockReasoning:
		prefix, continuation, style = "◆ ", "  ", tuiYellow
		text = "Thinking\n" + text
	case tuiBlockToolResult:
		prefix, continuation, style = "  └ ", "    ", tuiSubtle
	case tuiBlockToolError:
		prefix, continuation, style = "  └ ", "    ", tuiRed
	}
	return tuiWrappedStyledLines(kind, text, width, prefix, continuation, style)
}

// tuiCompactToolVisualLines gives compact activity a small visual hierarchy:
// a strong summary, tree branches for grouped work, and quiet output lines.
// Failures keep a red summary and failed branch without washing successful
// siblings in the error color.
func tuiCompactToolVisualLines(activities []tuiToolActivity, width int) []tuiVisualLine {
	display, failed := tuiCompactToolActivitiesDisplay(activities)
	if display.Text == "" {
		return nil
	}
	kind := tuiBlockTool
	if failed {
		kind = tuiBlockToolError
	}
	parts := strings.Split(tuiSafeText(display.Text), "\n")
	lines := make([]tuiVisualLine, 0, len(parts))
	for index, part := range parts {
		if index == 0 {
			style := tuiBrightGreen + tuiBold
			if failed {
				style = tuiRed + tuiBold
			}
			lines = append(lines, tuiWrappedStyledLines(kind, part, width, "● ", "  ", style)...)
			continue
		}

		style := tuiSubtle
		lower := strings.ToLower(strings.TrimSpace(part))
		if failed && (strings.Contains(lower, "failed") || strings.HasPrefix(lower, "error") || strings.HasPrefix(part, "└─ Error")) {
			style = tuiRed
		} else if strings.HasPrefix(part, "└─ Output") || strings.HasPrefix(part, "└─ Error") {
			style += tuiBold
		}
		continuation := "  " + tuiCompactToolLineIndent(part)
		lines = append(lines, tuiWrappedStyledLines(kind, part, width, "  ", continuation, style)...)
	}
	return lines
}

func tuiCompactToolLineIndent(line string) string {
	if strings.HasPrefix(line, "├─ ") || strings.HasPrefix(line, "└─ ") {
		return "   "
	}
	return strings.Repeat(" ", len(line)-len(strings.TrimLeft(line, " ")))
}

func tuiWrappedStyledLines(kind tuiBlockKind, text string, width int, firstPrefix, continuation, style string) []tuiVisualLine {
	if width < 1 {
		width = 1
	}
	parts := strings.Split(tuiSafeText(text), "\n")
	lines := make([]tuiVisualLine, 0, len(parts))
	for partIndex, part := range parts {
		prefix := continuation
		if partIndex == 0 {
			prefix = firstPrefix
		}
		available := width - utf8.RuneCountInString(prefix)
		if available < 1 {
			available = 1
		}
		runes := []rune(part)
		if len(runes) == 0 {
			plain := prefix
			lines = append(lines, tuiVisualLine{kind: kind, text: plain, rendered: style + plain + tuiReset})
			continue
		}
		for len(runes) > 0 {
			count := available
			if count > len(runes) {
				count = len(runes)
			}
			plain := prefix + string(runes[:count])
			lines = append(lines, tuiVisualLine{kind: kind, text: plain, rendered: style + plain + tuiReset})
			runes = runes[count:]
			prefix = continuation
			available = width - utf8.RuneCountInString(prefix)
			if available < 1 {
				available = 1
			}
		}
	}
	return lines
}

func tuiDiffVisualLines(text string, width int) []tuiVisualLine {
	parts := strings.Split(tuiSafeText(text), "\n")
	if len(parts) == 0 {
		return nil
	}
	lines := tuiWrappedStyledLines(tuiBlockDiff, parts[0], width, "  ┌ ", "    ", tuiMagenta+tuiBold)
	for _, part := range parts[1:] {
		style := tuiSubtle
		switch {
		case strings.HasPrefix(part, "diff --git "), strings.HasPrefix(part, "index "), strings.HasPrefix(part, "--- "), strings.HasPrefix(part, "+++ "):
			style = tuiCyan
		case strings.HasPrefix(part, "@@"):
			style = tuiMagenta
		case strings.HasPrefix(part, "+"):
			style = tuiGreen
		case strings.HasPrefix(part, "-"):
			style = tuiRed
		case strings.HasPrefix(part, "…"):
			style = tuiYellow
		}
		lines = append(lines, tuiWrappedStyledLines(tuiBlockDiff, part, width, "  │ ", "  │ ", style)...)
	}
	lines = append(lines, tuiVisualLine{kind: tuiBlockDiff, text: "  └", rendered: tuiMagenta + "  └" + tuiReset})
	return lines
}

func tuiStatusVisualLines(status CommandStatusData, width int) []tuiVisualLine {
	if width < 32 {
		contextWindow := "unknown"
		if status.ContextWindowTokens > 0 {
			contextWindow = formatStatusInteger(status.ContextWindowTokens) + " tokens"
		}
		compact := fmt.Sprintf("Status\nModel: %s\nReasoning: %s\nDirectory: %s\nContext: %s\nPlan mode: %s%s", commandStatusValue(status.Model, "<configured>"), commandStatusValue(status.ReasoningEffort, "provider default"), commandStatusValue(status.Directory, "<unknown>"), contextWindow, onOffLabel(status.PlanModeEnabled), planModeSourceSuffix(status.PlanModeSource))
		if status.ToolCallLimit != nil {
			compact += "\nTool-call limit: " + onOffLabel(status.ToolCallLimit.Enabled)
		}
		return tuiWrappedStyledLines(tuiBlockStatus, compact, width, "", "", tuiCyan)
	}
	text := FormatCommandStatus(status, width)
	parts := strings.Split(text, "\n")
	lines := make([]tuiVisualLine, 0, len(parts))
	for index, part := range parts {
		style := tuiCyan
		if index == 0 {
			style += tuiBold
		}
		lines = append(lines, tuiVisualLine{kind: tuiBlockStatus, text: part, rendered: style + part + tuiReset})
	}
	return lines
}
