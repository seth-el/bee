package bee

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	tuiReset       = "\x1b[0m"
	tuiBold        = "\x1b[1m"
	tuiSubtle      = "\x1b[2m"
	tuiDim         = "\x1b[90m"
	tuiCyan        = "\x1b[36m"
	tuiGreen       = "\x1b[32m"
	tuiBrightGreen = "\x1b[92m"
	tuiYellow      = "\x1b[33m"
	tuiMagenta     = "\x1b[35m"
	tuiRed         = "\x1b[31m"
)

func renderTUIHeader(output io.Writer, app *Application, sessionID SessionID, modelOverride string) error {
	profile := "default profile"
	model := strings.TrimSpace(modelOverride)
	scope := ""
	if app != nil {
		if strings.TrimSpace(app.ProviderProfile) != "" {
			profile = app.ProviderProfile
		}
		if model == "" {
			model = app.Config.Provider.Model
		}
		scope = app.Capabilities.ScopeRoot
		if scope == "" && app.Executor != nil {
			scope = app.Executor.Scope.Root
		}
	}
	if model == "" {
		model = "configured model"
	}
	if _, err := fmt.Fprintf(output, "%s╭─ %sBee%s\n%s│%s %s · %s\n", tuiCyan, tuiBold, tuiReset, tuiCyan, tuiReset, tuiCompactText(model, 80), tuiCompactText(profile, 80)); err != nil {
		return err
	}
	if strings.TrimSpace(scope) != "" {
		if _, err := fmt.Fprintf(output, "%s│%s %s\n", tuiCyan, tuiReset, tuiCompactText(scope, 120)); err != nil {
			return err
		}
	}
	if sessionID != "" {
		if _, err := fmt.Fprintf(output, "%s│%s session %s\n", tuiCyan, tuiReset, tuiCompactText(string(sessionID), 80)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "%s╰─%s %s/help for commands · /exit to quit%s\n\n", tuiCyan, tuiReset, tuiDim, tuiReset)
	return err
}

func renderTUIPrompt(output io.Writer) error {
	_, err := fmt.Fprintf(output, "%s%s›%s ", tuiBold, tuiCyan, tuiReset)
	return err
}

func renderTUICommand(output io.Writer, text string) error {
	text = strings.TrimRight(tuiSafeText(text), "\n")
	if text == "" {
		_, err := io.WriteString(output, "\n")
		return err
	}
	_, err := fmt.Fprintf(output, "%s\n\n", text)
	return err
}

type tuiEventRenderer struct {
	output              io.Writer
	visibility          DisplayVisibility
	sessionID           SessionID
	initialSessionID    SessionID
	newSessionAnnounced bool
	lineOpen            bool
	activityVisible     bool
	assistantSegment    bool
	deltaSeen           bool
	reasoningSegment    bool
	reasoningSeen       bool
	compactToolSegment  bool
	usageCalls          int
	totalTokens         int
	perf                runPerformance
	runStartedAt        time.Time
	pendingTools        []tuiToolActivity
	explorationTools    []tuiToolActivity
}

func renderTUIEventsFromSession(output io.Writer, events <-chan RunEvent, initialSessionID SessionID) (SessionID, error) {
	return renderTUIEventsFromSessionWithVisibility(output, events, initialSessionID, defaultDisplayVisibility())
}

func renderTUIEventsFromSessionWithVisibility(output io.Writer, events <-chan RunEvent, initialSessionID SessionID, visibility DisplayVisibility) (SessionID, error) {
	if output == nil {
		return initialSessionID, errors.New("tui output is required")
	}
	renderer := &tuiEventRenderer{output: output, visibility: visibility, sessionID: initialSessionID, initialSessionID: initialSessionID}
	for event := range events {
		if err := renderer.render(event); err != nil {
			return renderer.sessionID, err
		}
	}
	if err := renderer.finish(); err != nil {
		return renderer.sessionID, err
	}
	return renderer.sessionID, nil
}

func (renderer *tuiEventRenderer) render(event RunEvent) error {
	if event.SessionID != "" {
		renderer.sessionID = event.SessionID
	}
	if renderer.visibility.toolDisplayMode() == ToolDisplayCompact && tuiCompactToolBoundary(event.Type) {
		if err := renderer.flushCompactTools(); err != nil {
			return err
		}
	}
	switch event.Type {
	case RunStart:
		renderer.runStartedAt = time.Now()
		renderer.perf = runPerformance{}
		return renderer.startActivity()
	case RunSession:
		return nil
	case RunAssistantDelta:
		if event.Delta == "" {
			return nil
		}
		renderer.deltaSeen = true
		return renderer.writeAssistant(event.Delta)
	case RunReasoningDelta:
		if !renderer.visibility.Thinking {
			return nil
		}
		if event.Reasoning == nil || event.Reasoning.Delta == "" {
			return nil
		}
		renderer.reasoningSeen = true
		return renderer.writeReasoning(event.Reasoning.Delta)
	case RunReasoningFinal:
		if !renderer.visibility.Thinking {
			renderer.reasoningSeen = false
			return renderer.finishReasoning()
		}
		if event.Reasoning != nil && !renderer.reasoningSeen && event.Reasoning.Content != "" {
			if err := renderer.writeReasoning(event.Reasoning.Content); err != nil {
				return err
			}
		}
		if event.Reasoning != nil && event.Reasoning.Truncated {
			if err := renderer.writeReasoning("\n… provider reasoning truncated"); err != nil {
				return err
			}
		}
		renderer.reasoningSeen = false
		return renderer.finishReasoning()
	case RunAssistant:
		if err := renderer.finishReasoning(); err != nil {
			return err
		}
		if !renderer.deltaSeen && event.Message != nil && event.Message.Content != "" {
			if err := renderer.writeAssistant(event.Message.Content); err != nil {
				return err
			}
		}
		renderer.deltaSeen = false
		renderer.assistantSegment = false
	case RunPlan:
		if event.Plan != nil {
			return renderer.writeActivity(tuiMagenta, "Plan", event.Plan.Goal)
		}
	case RunToolCall:
		mode := renderer.visibility.toolDisplayMode()
		if mode == ToolDisplayOff {
			return nil
		}
		if event.ToolCall != nil {
			if mode == ToolDisplayCompact {
				return renderer.writeCompactToolCall(*event.ToolCall)
			}
			return renderer.writeToolCall(*event.ToolCall)
		}
	case RunToolResult:
		mode := renderer.visibility.toolDisplayMode()
		if mode == ToolDisplayOff {
			return nil
		}
		if event.ToolResult == nil {
			return nil
		}
		if mode == ToolDisplayCompact {
			return renderer.writeCompactToolResult(*event.ToolResult)
		}
		return renderer.writeToolResult(*event.ToolResult)
	case RunApproval:
		if event.Approval != nil {
			if event.Approval.EffectiveStatus() == ApprovalStatusPending {
				return nil
			}
			detail := string(event.Approval.Decision)
			if event.Approval.Request.ToolName != "" {
				detail += " · " + event.Approval.Request.ToolName
			}
			return renderer.writeActivity(tuiYellow, "Approval", detail)
		}
	case RunUsage:
		if event.Usage != nil {
			renderer.usageCalls++
			renderer.totalTokens += event.Usage.Usage.TotalTokens
			renderer.perf.add(event.Usage)
		}
	case RunProviderFallback:
		if event.ProviderFallback != nil {
			return renderer.writeActivity(tuiYellow, "Fallback", "main model unreachable · answering with "+event.ProviderFallback.To)
		}
		return nil
	case RunProviderOutputRejected:
		if event.ProviderOutput != nil {
			renderer.reasoningSeen = false
			if err := renderer.finishReasoning(); err != nil {
				return err
			}
			return renderer.writeActivity(tuiYellow, "Provider", tuiProviderFailureDetail(*event.ProviderOutput))
		}
	case RunCheckpoint:
		if event.Checkpoint != nil {
			detail := string(event.Checkpoint.WhyStopped) + " · continue " + event.Checkpoint.ID
			return renderer.writeActivity(tuiYellow, "Checkpoint", detail)
		}
	case RunFailure:
		if event.ErrorKind == ErrorKindModelUnreachable {
			return renderer.writeActivity(tuiRed, "Unreachable", RunFailureText(event))
		}
		return renderer.writeActivity(tuiRed, "Error", event.Error)
	case RunEnd:
		return renderer.finishRun()
	}
	return nil
}

func tuiCompactToolBoundary(eventType RunEventType) bool {
	switch eventType {
	case RunToolCall, RunToolResult, RunUsage, RunContextDiagnostics, RunContextUsage, RunSession:
		return false
	default:
		return true
	}
}

func tuiProviderFailureDetail(failure ProviderOutputRejection) string {
	detail := "output rejected; no tool executed"
	if failure.Stage == ProviderFailureStageRequest {
		detail = "request failed before response; no tool executed"
	}
	if failure.Retrying {
		detail += "; retrying once"
	}
	return detail
}

func (renderer *tuiEventRenderer) startActivity() error {
	return renderer.replaceActivity("Working…")
}

func (renderer *tuiEventRenderer) replaceActivity(detail string) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	detail = tuiCompactText(detail, 200)
	if detail == "" {
		detail = "Working…"
	}
	if _, err := fmt.Fprintf(renderer.output, "%s• %s%s", tuiDim, detail, tuiReset); err != nil {
		return err
	}
	renderer.lineOpen = true
	renderer.activityVisible = true
	return nil
}

func (renderer *tuiEventRenderer) writeCompactToolCall(call ToolCall) error {
	action := tuiCompactToolCallAction(call)
	if action.Class != tuiToolActivityExploration {
		if err := renderer.flushCompactTools(); err != nil {
			return err
		}
	}
	renderer.pendingTools = append(renderer.pendingTools, tuiToolActivity{Call: call})
	return renderer.replaceActivity(action.Active + "…")
}

func (renderer *tuiEventRenderer) writeCompactToolResult(result ToolResultEvent) error {
	index := -1
	for candidate := len(renderer.pendingTools) - 1; candidate >= 0; candidate-- {
		pending := renderer.pendingTools[candidate]
		if result.CallID != "" && pending.Call.ID == result.CallID {
			index = candidate
			break
		}
	}
	if index < 0 {
		for candidate := len(renderer.pendingTools) - 1; candidate >= 0; candidate-- {
			pending := renderer.pendingTools[candidate]
			if pending.Call.Name == result.ToolName {
				index = candidate
				break
			}
		}
	}
	activity := tuiToolActivity{Call: ToolCall{ID: result.CallID, Name: result.ToolName}}
	if index >= 0 {
		activity = renderer.pendingTools[index]
		renderer.pendingTools = append(renderer.pendingTools[:index], renderer.pendingTools[index+1:]...)
	}
	copy := result
	activity.Result = &copy
	action := tuiCompactToolCallAction(activity.Call)
	if action.Class == tuiToolActivityExploration {
		renderer.explorationTools = append(renderer.explorationTools, activity)
		return renderer.replaceActivity("Exploring…")
	}
	return renderer.writeCompactToolActivities([]tuiToolActivity{activity})
}

func (renderer *tuiEventRenderer) flushCompactTools() error {
	if len(renderer.pendingTools) > 0 {
		remaining := renderer.pendingTools[:0]
		for _, activity := range renderer.pendingTools {
			if tuiCompactToolCallAction(activity.Call).Class == tuiToolActivityExploration {
				renderer.explorationTools = append(renderer.explorationTools, activity)
				continue
			}
			if err := renderer.writeCompactToolActivities([]tuiToolActivity{activity}); err != nil {
				return err
			}
		}
		renderer.pendingTools = remaining
	}
	if len(renderer.explorationTools) == 0 {
		return nil
	}
	activities := append([]tuiToolActivity(nil), renderer.explorationTools...)
	renderer.explorationTools = renderer.explorationTools[:0]
	return renderer.writeCompactToolActivities(activities)
}

func (renderer *tuiEventRenderer) writeCompactToolActivities(activities []tuiToolActivity) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	if _, err := io.WriteString(renderer.output, "\n"); err != nil {
		return err
	}
	renderer.assistantSegment = false
	for _, line := range tuiCompactToolVisualLines(activities, 120) {
		if _, err := fmt.Fprintln(renderer.output, line.rendered); err != nil {
			return err
		}
	}
	renderer.compactToolSegment = true
	return nil
}

func (renderer *tuiEventRenderer) clearActivity() error {
	if !renderer.activityVisible {
		return nil
	}
	if _, err := io.WriteString(renderer.output, "\r\x1b[2K"); err != nil {
		return err
	}
	renderer.activityVisible = false
	renderer.lineOpen = false
	return nil
}

func (renderer *tuiEventRenderer) writeAssistant(text string) error {
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if !renderer.assistantSegment {
		if err := renderer.endLine(); err != nil {
			return err
		}
		if err := renderer.finishCompactToolSegment(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(renderer.output, "%s•%s ", tuiGreen, tuiReset); err != nil {
			return err
		}
		renderer.lineOpen = true
		renderer.assistantSegment = true
	}
	safe := tuiSafeText(text)
	if safe == "" {
		return nil
	}
	if _, err := io.WriteString(renderer.output, safe); err != nil {
		return err
	}
	renderer.lineOpen = !strings.HasSuffix(safe, "\n")
	return nil
}

func (renderer *tuiEventRenderer) writeReasoning(text string) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if !renderer.reasoningSegment {
		if err := renderer.endLine(); err != nil {
			return err
		}
		if err := renderer.finishCompactToolSegment(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(renderer.output, "%s◆ Thinking%s\n%s  ", tuiYellow+tuiBold, tuiReset, tuiYellow+tuiItalic); err != nil {
			return err
		}
		renderer.lineOpen = true
		renderer.reasoningSegment = true
		renderer.assistantSegment = false
	}
	safe := tuiSafeText(text)
	if safe == "" {
		return nil
	}
	safe = strings.ReplaceAll(safe, "\n", "\n  ")
	if _, err := fmt.Fprintf(renderer.output, "%s%s", safe, tuiReset); err != nil {
		return err
	}
	if _, err := io.WriteString(renderer.output, tuiYellow+tuiItalic); err != nil {
		return err
	}
	renderer.lineOpen = true
	return nil
}

func (renderer *tuiEventRenderer) finishReasoning() error {
	if !renderer.reasoningSegment {
		return nil
	}
	if _, err := io.WriteString(renderer.output, tuiReset); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	renderer.reasoningSegment = false
	return nil
}

func (renderer *tuiEventRenderer) writeToolCall(call ToolCall) error {
	display := tuiToolCallDisplay(call)
	if err := renderer.writeDetailBlock(tuiDetailVisualLines(tuiBlockTool, display.Text, 120)); err != nil {
		return err
	}
	if display.Diff != "" {
		return renderer.writeDetailBlock(tuiDiffVisualLines(display.Diff, 120))
	}
	return nil
}

func (renderer *tuiEventRenderer) writeToolResult(result ToolResultEvent) error {
	display := tuiToolResultDisplay(result)
	kind := tuiBlockToolResult
	if result.Error != "" {
		kind = tuiBlockToolError
	}
	if err := renderer.writeDetailBlock(tuiDetailVisualLines(kind, display.Text, 120)); err != nil {
		return err
	}
	if display.Diff != "" {
		return renderer.writeDetailBlock(tuiDiffVisualLines(display.Diff, 120))
	}
	return nil
}

func (renderer *tuiEventRenderer) writeDetailBlock(lines []tuiVisualLine) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	if err := renderer.finishCompactToolSegment(); err != nil {
		return err
	}
	renderer.assistantSegment = false
	for _, line := range lines {
		if _, err := fmt.Fprintln(renderer.output, line.rendered); err != nil {
			return err
		}
	}
	return nil
}

func (renderer *tuiEventRenderer) writeActivity(color, label, detail string) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	if err := renderer.finishCompactToolSegment(); err != nil {
		return err
	}
	renderer.assistantSegment = false
	label = tuiCompactText(label, 32)
	detail = tuiCompactText(detail, 160)
	if detail == "" {
		_, err := fmt.Fprintf(renderer.output, "%s• %s%s\n", color, label, tuiReset)
		return err
	}
	_, err := fmt.Fprintf(renderer.output, "%s• %s%s %s\n", color, label, tuiReset, detail)
	return err
}

func (renderer *tuiEventRenderer) writeBranch(color, state, detail string) error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	if err := renderer.finishCompactToolSegment(); err != nil {
		return err
	}
	renderer.assistantSegment = false
	state = tuiCompactText(state, 32)
	detail = tuiCompactText(detail, 160)
	if detail == "" {
		_, err := fmt.Fprintf(renderer.output, "  %s└ %s%s\n", color, state, tuiReset)
		return err
	}
	_, err := fmt.Fprintf(renderer.output, "  %s└ %s%s · %s\n", color, state, tuiReset, detail)
	return err
}

func (renderer *tuiEventRenderer) finishRun() error {
	if err := renderer.clearActivity(); err != nil {
		return err
	}
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	if err := renderer.endLine(); err != nil {
		return err
	}
	if err := renderer.finishCompactToolSegment(); err != nil {
		return err
	}
	if renderer.initialSessionID == "" && renderer.sessionID != "" && !renderer.newSessionAnnounced {
		if _, err := fmt.Fprintf(renderer.output, "%s  session %s%s\n", tuiDim, tuiCompactText(string(renderer.sessionID), 80), tuiReset); err != nil {
			return err
		}
		renderer.newSessionAnnounced = true
	}
	if renderer.usageCalls > 0 {
		if _, err := fmt.Fprintf(renderer.output, "%s  %d tokens · %d provider call", tuiDim, renderer.totalTokens, renderer.usageCalls); err != nil {
			return err
		}
		if renderer.usageCalls != 1 {
			if _, err := io.WriteString(renderer.output, "s"); err != nil {
				return err
			}
		}
		var wall time.Duration
		if !renderer.runStartedAt.IsZero() {
			wall = time.Since(renderer.runStartedAt)
		}
		if perf := renderer.perf.summary(wall); perf != "" {
			if _, err := io.WriteString(renderer.output, " · "+perf); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(renderer.output, "%s\n", tuiReset); err != nil {
			return err
		}
	}
	_, err := io.WriteString(renderer.output, "\n")
	return err
}

func (renderer *tuiEventRenderer) finish() error {
	if renderer.visibility.toolDisplayMode() == ToolDisplayCompact {
		if err := renderer.flushCompactTools(); err != nil {
			return err
		}
	}
	if renderer.activityVisible {
		if err := renderer.clearActivity(); err != nil {
			return err
		}
	}
	if err := renderer.finishReasoning(); err != nil {
		return err
	}
	return renderer.endLine()
}

func (renderer *tuiEventRenderer) endLine() error {
	if !renderer.lineOpen {
		return nil
	}
	if _, err := io.WriteString(renderer.output, "\n"); err != nil {
		return err
	}
	renderer.lineOpen = false
	return nil
}

func (renderer *tuiEventRenderer) finishCompactToolSegment() error {
	if !renderer.compactToolSegment {
		return nil
	}
	if _, err := io.WriteString(renderer.output, "\n"); err != nil {
		return err
	}
	renderer.compactToolSegment = false
	return nil
}

func tuiCompactText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(tuiSafeText(value)), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// tuiSafeText prevents provider and command text from controlling the local
// terminal. Newlines remain meaningful, tabs become stable spaces, CRLF is
// normalized, and ANSI/OSC escape sequences and other controls are removed.
func tuiSafeText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); {
		current := value[index]
		if current == 0x1b {
			index = skipTUIEscape(value, index+1)
			continue
		}
		if current == '\r' {
			builder.WriteByte('\n')
			index++
			if index < len(value) && value[index] == '\n' {
				index++
			}
			continue
		}
		if current == '\t' {
			builder.WriteString("    ")
			index++
			continue
		}
		if current < 0x20 && current != '\n' || current == 0x7f {
			index++
			continue
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		if size == 0 {
			break
		}
		builder.WriteString(value[index : index+size])
		index += size
	}
	return builder.String()
}

func skipTUIEscape(value string, index int) int {
	if index >= len(value) {
		return index
	}
	switch value[index] {
	case '[': // Control Sequence Introducer (CSI).
		index++
		for index < len(value) {
			current := value[index]
			index++
			if current >= 0x40 && current <= 0x7e {
				break
			}
		}
		return index
	case ']': // Operating System Command (OSC), terminated by BEL or ST.
		index++
		for index < len(value) {
			if value[index] == 0x07 {
				return index + 1
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
			index++
		}
		return index
	default:
		return index + 1
	}
}
