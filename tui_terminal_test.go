package bee

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeTUITerminal struct {
	width  int
	height int
	writes strings.Builder
}

func (*fakeTUITerminal) enter() error                       { return nil }
func (*fakeTUITerminal) leave() error                       { return nil }
func (*fakeTUITerminal) read(time.Duration) ([]byte, error) { return nil, io.EOF }
func (terminal *fakeTUITerminal) size() (int, int, error) {
	return terminal.width, terminal.height, nil
}
func (terminal *fakeTUITerminal) write(value string) error {
	_, err := terminal.writes.WriteString(value)
	return err
}

func TestFullscreenTUIKeepsComposerPinnedWhileAssistantStreams(t *testing.T) {
	terminal := &fakeTUITerminal{width: 64, height: 12}
	config := DefaultConfig()
	config.Provider.Model = "test-model"
	ui := newFullscreenTUI(&Application{Config: config, ProviderProfile: "local"}, terminal, "session-a", "")
	ui.draft = []rune("next question")
	ui.cursor = len(ui.draft)
	ui.handleRunEvent(RunEvent{Type: RunStart})
	ui.handleRunEvent(RunEvent{Type: RunAssistantDelta, Delta: "streamed answer"})

	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	got := terminal.writes.String()
	for _, want := range []string{"streamed answer", "context — / unknown", "› next question", "/steer <correction>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered screen missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b[E") {
		t.Fatalf("screen used wrap-sensitive relative row movement: %q", got)
	}
	for _, row := range []string{"\x1b[10;2H", "\x1b[11;2H", "\x1b[12;2H"} {
		if !strings.Contains(got, row) {
			t.Fatalf("screen missing fixed composer row %q: %q", row, got)
		}
	}
}

func TestFullscreenTUIStreamingDeltaOnlyRepaintsChangedRow(t *testing.T) {
	terminal := &fakeTUITerminal{width: 64, height: 12}
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, terminal, "session-a", "")
	ui.handleRunEvent(RunEvent{Type: RunStart})
	ui.handleRunEvent(RunEvent{Type: RunAssistantDelta, Delta: "streamed"})
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}

	terminal.writes.Reset()
	ui.handleRunEvent(RunEvent{Type: RunAssistantDelta, Delta: " answer"})
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	rendered := terminal.writes.String()
	if !strings.Contains(rendered, "streamed answer") {
		t.Fatalf("streaming update omitted changed assistant row: %q", rendered)
	}
	if count := strings.Count(rendered, "\x1b[K"); count != 1 {
		t.Fatalf("streaming update repainted %d rows, want 1: %q", count, rendered)
	}
	for _, unchanged := range []string{"Bee", "/steer <correction>"} {
		if strings.Contains(rendered, unchanged) {
			t.Fatalf("streaming update repainted unchanged content %q: %q", unchanged, rendered)
		}
	}
	if strings.Contains(rendered, "\x1b[2K") {
		t.Fatalf("streaming update cleared a row before replacing it: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[?2026h") || !strings.Contains(rendered, "\x1b[?2026l") {
		t.Fatalf("streaming update was not emitted as a synchronized frame: %q", rendered)
	}
	if strings.Contains(rendered, "\x1b[?25l") || strings.Contains(rendered, "\x1b[?25h") {
		t.Fatalf("streaming update toggled cursor visibility: %q", rendered)
	}
}

func TestFullscreenTUIFormatsMarkdownAndKeepsMetadataLegible(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 72, height: 24}, "session-a", "")
	ui.addBlock(tuiBlockAssistant, "# Heading\n\n- item with **bold** and `code`\n> quoted\n\n```go\nfmt.Println(\"hi\")\n```")
	visual := ui.visualLines(72)
	var plain strings.Builder
	var rendered strings.Builder
	for _, line := range visual {
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		rendered.WriteString(line.rendered)
	}
	for _, want := range []string{"Heading", "• item with bold and code", "│ quoted", "┌─ go", "│ fmt.Println(\"hi\")", "└─"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("formatted Markdown missing %q: %q", want, plain.String())
		}
	}
	for _, marker := range []string{"# Heading", "**bold**", "`code`", "```"} {
		if strings.Contains(plain.String(), marker) {
			t.Fatalf("formatted Markdown retained marker %q: %q", marker, plain.String())
		}
	}
	for _, style := range []string{tuiBold, tuiInlineCode, tuiCodeText} {
		if !strings.Contains(rendered.String(), style) {
			t.Fatalf("formatted Markdown missing ANSI style %q: %q", style, rendered.String())
		}
	}
	for _, kind := range []tuiBlockKind{tuiBlockMeta, tuiBlockBranch} {
		if color := tuiBlockColor(kind); color != tuiSubtle {
			t.Fatalf("informational color=%q, want adaptive subtle style %q", color, tuiSubtle)
		}
	}
}

func TestFullscreenTUILeavesOneRowBetweenConversationMessages(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "", "")
	ui.blocks = nil
	ui.addBlock(tuiBlockUser, "question")
	ui.addBlock(tuiBlockAssistant, "answer")

	visual := ui.visualLines(72)
	if len(visual) != 3 {
		t.Fatalf("visual lines=%#v, want user, spacer, and assistant rows", visual)
	}
	if visual[0].text != "› question" || visual[1].text != "" || visual[2].text != "• answer" {
		t.Fatalf("visual lines=%#v, want one blank row between messages", visual)
	}
}

func TestFullscreenTUIPadsCompactToolBlocks(t *testing.T) {
	ui := newFullscreenTUI(&Application{}, &fakeTUITerminal{}, "", "")
	ui.blocks = nil
	ui.addBlock(tuiBlockUser, "question")
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`}})
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "read", ToolName: "read_file", Output: "contents"}})
	ui.addBlock(tuiBlockAssistant, "answer")

	visual := ui.visualLines(72)
	if len(visual) != 5 {
		t.Fatalf("visual lines=%#v, want user, padding, tool, padding, and assistant rows", visual)
	}
	if visual[0].text != "› question" || visual[1].text != "" || visual[2].text != "● Read main.go" || visual[3].text != "" || visual[4].text != "• answer" {
		t.Fatalf("compact tool spacing=%#v", visual)
	}
	if !strings.HasPrefix(visual[2].rendered, tuiBrightGreen+tuiBold) {
		t.Fatalf("compact tool summary color=%q", visual[2].rendered)
	}
}

func TestFullscreenTUILeavesOneRowAboveComposer(t *testing.T) {
	terminal := &fakeTUITerminal{width: 64, height: 8}
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, terminal, "", "")
	ui.blocks = nil
	for index := 0; index < 12; index++ {
		ui.addBlock(tuiBlockActivity, fmt.Sprintf("activity %d", index))
	}

	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	composerStart := -1
	for index, row := range ui.renderedRows {
		if strings.Contains(row, "╭") {
			composerStart = index
			break
		}
	}
	if composerStart < 2 {
		t.Fatalf("composer start=%d in rows %#v", composerStart, ui.renderedRows)
	}
	if ui.renderedRows[composerStart-1] != "" {
		t.Fatalf("row above composer=%q, want blank padding", ui.renderedRows[composerStart-1])
	}
	if ui.renderedRows[composerStart-2] == "" {
		t.Fatalf("transcript did not fill the row before padding: %#v", ui.renderedRows)
	}
}

func TestFullscreenTUIComposerGrowsWithExplicitLines(t *testing.T) {
	terminal := &fakeTUITerminal{width: 64, height: 12}
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, terminal, "", "")
	ui.draft = []rune("one\ntwo\nthree")
	ui.cursor = len(ui.draft)

	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	got := terminal.writes.String()
	for _, want := range []string{"\x1b[8;2H", "\x1b[9;2H", "\x1b[10;2H", "\x1b[11;2H", "\x1b[12;2H", "› one", "   two", "   three"} {
		if !strings.Contains(got, want) {
			t.Fatalf("growing composer missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "\x1b[11;11H\x1b[?25h") {
		t.Fatalf("cursor was not placed on the final composer row: %q", got)
	}
}

func TestTUIComposerWrapsAndScrollsAtFiveLines(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "", "")
	ui.draft = []rune("one\ntwo\nthree\nfour\nfive\nsix\nseven")
	ui.cursor = len(ui.draft)
	layout := ui.composerLayout(40, tuiMaxComposerLines)

	if layout.totalLines != 7 || layout.firstLine != 2 || len(layout.lines) != 5 || layout.cursorRow != 4 {
		t.Fatalf("layout=%+v, want seven total lines with the last five visible", layout)
	}
	joined := strings.Join(layout.lines, "\n")
	for _, want := range []string{"↑", "three", "four", "five", "six", "seven"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("scrolled composer missing %q: %q", want, joined)
		}
	}
	for _, hidden := range []string{"one", "two"} {
		if strings.Contains(joined, hidden) {
			t.Fatalf("scrolled composer unexpectedly retained hidden line %q: %q", hidden, joined)
		}
	}

	ui.draft = []rune("abcdefghijklmnop")
	ui.cursor = len(ui.draft)
	wrapped := ui.composerLayout(12, tuiMaxComposerLines)
	if wrapped.totalLines != 3 {
		t.Fatalf("wrapped total lines=%d, want 3", wrapped.totalLines)
	}
}

func TestTUIComposerFitsTerminalCellsAndLeavesWrapMargin(t *testing.T) {
	const terminalWidth = 12
	renderWidth := tuiRenderWidth(terminalWidth)
	if renderWidth != terminalWidth-2*tuiEdgePadding {
		t.Fatalf("render width=%d, want balanced edge padding", renderWidth)
	}
	leftPadding := tuiContentColumn - 1
	rightPadding := terminalWidth - (tuiContentColumn + renderWidth - 1)
	if leftPadding != tuiEdgePadding || rightPadding != tuiEdgePadding {
		t.Fatalf("terminal padding left=%d right=%d, want %d", leftPadding, rightPadding, tuiEdgePadding)
	}

	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "", "")
	ui.draft = []rune("你好世界")
	ui.cursor = len(ui.draft)
	layout := ui.composerLayout(renderWidth, tuiMaxComposerLines)
	if len(layout.lines) != 2 {
		t.Fatalf("wide-character composer lines=%d, want 2: %#v", len(layout.lines), layout.lines)
	}
	for _, line := range layout.lines {
		if got, want := tuiDisplayWidth("│"+line+"│"), renderWidth; got != want {
			t.Fatalf("composer row width=%d, want %d: %q", got, want, line)
		}
	}
	if layout.cursorColumn >= terminalWidth {
		t.Fatalf("cursor column=%d reaches terminal wrap column %d", layout.cursorColumn, terminalWidth)
	}
	if got := tuiDisplayWidth(tuiBoxBorder('╭', '╮', " context — / unknown ", renderWidth)); got != renderWidth {
		t.Fatalf("composer border width=%d, want %d", got, renderWidth)
	}
}

func TestTUIComposerPreservesPastedLineBreaks(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "", "")
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyPaste, text: "one\r\ntwo\rthree\tfour"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := string(ui.draft), "one\ntwo\nthree four"; got != want {
		t.Fatalf("pasted draft=%q, want %q", got, want)
	}
}

func TestTUIComposerMovesVerticallyAcrossExplicitLinesWithStickyColumn(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 20}, "", "")
	ui.width = 20
	ui.draft = []rune("alpha\nxy\nomega")
	ui.cursor = 4

	for _, test := range []struct {
		key        tuiKeyKind
		wantCursor int
	}{
		{key: tuiKeyDown, wantCursor: 8},
		{key: tuiKeyDown, wantCursor: 13},
		{key: tuiKeyUp, wantCursor: 8},
		{key: tuiKeyUp, wantCursor: 4},
	} {
		if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: test.key}, nil); err != nil {
			t.Fatal(err)
		}
		if ui.cursor != test.wantCursor {
			t.Fatalf("after key %v cursor=%d, want %d", test.key, ui.cursor, test.wantCursor)
		}
	}
}

func TestTUIComposerMovesVerticallyAcrossWrappedAndWideRows(t *testing.T) {
	wrapped := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 12}, "", "")
	wrapped.width = 12
	wrapped.draft = []rune("abcdefghij")
	wrapped.cursor = 3
	_, _, _, available := tuiComposerDimensions(tuiRenderWidth(wrapped.width))
	wantWrappedCursor := available + 3
	if !wrapped.moveCursorVertical(1) || wrapped.cursor != wantWrappedCursor {
		t.Fatalf("wrapped cursor=%d, want %d", wrapped.cursor, wantWrappedCursor)
	}

	wide := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 20}, "", "")
	wide.width = 20
	wide.draft = []rune("你好a\nabcdef")
	wide.cursor = 2
	if !wide.moveCursorVertical(1) || wide.cursor != 8 {
		t.Fatalf("wide-character cursor=%d, want 8", wide.cursor)
	}
}

func TestTUIComposerArrowFallsBackToHistoryWithoutAdjacentRow(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 40}, "", "")
	ui.width = 40
	ui.history = []string{"older command"}
	ui.draft = []rune("current draft")
	ui.cursor = 3

	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyUp}, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(ui.draft); got != "older command" {
		t.Fatalf("draft=%q, want history entry", got)
	}
	if ui.cursor != len(ui.draft) {
		t.Fatalf("cursor=%d, want end of recalled history", ui.cursor)
	}
}

func TestTUIComposerMovesAndDeletesByWord(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "", "")
	ui.draft = []rune("one  two three")
	ui.cursor = len(ui.draft)
	ui.moveWordLeft()
	if ui.cursor != 9 {
		t.Fatalf("first word-left cursor=%d, want 9", ui.cursor)
	}
	ui.moveWordLeft()
	if ui.cursor != 5 {
		t.Fatalf("second word-left cursor=%d, want 5", ui.cursor)
	}
	ui.moveWordRight()
	if ui.cursor != 9 {
		t.Fatalf("word-right cursor=%d, want 9", ui.cursor)
	}
	ui.deletePreviousWord()
	if got, want := string(ui.draft), "one  three"; got != want || ui.cursor != 5 {
		t.Fatalf("backward word deletion draft=%q cursor=%d, want %q at 5", got, ui.cursor, want)
	}

	ui.draft = []rune("one  two three")
	ui.cursor = 3
	ui.deleteNextWord()
	if got, want := string(ui.draft), "one three"; got != want || ui.cursor != 3 {
		t.Fatalf("forward word deletion draft=%q cursor=%d, want %q at 3", got, ui.cursor, want)
	}
}

func TestFullscreenTUISteerInterruptsAndQueuesCorrection(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "session-a", "")
	ui.running = true
	ui.draft = []rune("/steer inspect the failing test first")
	ui.cursor = len(ui.draft)
	cancelled := false
	_, events, cancel, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyEnter}, func() { cancelled = true })
	if err != nil {
		t.Fatal(err)
	}
	if events != nil || cancel != nil {
		t.Fatal("steer unexpectedly started a concurrent run")
	}
	if !cancelled || ui.pendingSteer != "inspect the failing test first" || len(ui.draft) != 0 || ui.activity != "Steering…" {
		t.Fatalf("steer state: cancelled=%t pending=%q draft=%q activity=%q", cancelled, ui.pendingSteer, string(ui.draft), ui.activity)
	}
	before := len(ui.blocks)
	ui.handleRunEvent(RunEvent{Type: RunFailure, Error: context.Canceled.Error()})
	if len(ui.blocks) != before {
		t.Fatalf("steering cancellation rendered as an error: %#v", ui.blocks[before:])
	}
}

func TestFullscreenTUISteerIsRejectedWhileIdle(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{}, "session-a", "")
	ui.draft = []rune("/steer go another way")
	ui.cursor = len(ui.draft)
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyEnter}, nil); err != nil {
		t.Fatal(err)
	}
	if ui.pendingSteer != "" || len(ui.blocks) == 0 || !strings.Contains(ui.blocks[len(ui.blocks)-1].text, "response in progress") {
		t.Fatalf("idle steer state pending=%q blocks=%#v", ui.pendingSteer, ui.blocks)
	}
}

func TestFullscreenTUIRequiresExplicitApprovalKey(t *testing.T) {
	app := &Application{Config: DefaultConfig(), Executor: &Executor{}}
	if err := app.EnableApprovalBroker(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	decision := make(chan ApprovalDecision, 1)
	errCh := make(chan error, 1)
	request := ApprovalRequest{ID: "approval-tui", ToolName: SkillEnableToolName}
	go func() {
		value, err := app.Executor.Approval.Approve(ctx, request)
		decision <- value
		errCh <- err
	}()
	for len(app.PendingApprovals()) == 0 {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	ui := newFullscreenTUI(app, &fakeTUITerminal{}, "", "")
	ui.handleRunEvent(RunEvent{Type: RunApproval, Approval: &ApprovalEvent{ID: request.ID, Request: request}})
	before := len(ui.blocks)
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyRune, rune: 'x'}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-decision:
		t.Fatalf("unexpected decision %q", value)
	default:
	}
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyRune, rune: 'y'}, nil); err != nil {
		t.Fatal(err)
	}
	if value := <-decision; value != ApprovalApproved {
		t.Fatalf("decision=%q", value)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if len(ui.blocks) != before {
		t.Fatalf("keypress rendered approval before authoritative event: %#v", ui.blocks[before:])
	}
	ui.handleRunEvent(RunEvent{Type: RunApproval, Approval: &ApprovalEvent{ID: request.ID, Request: request, Decision: ApprovalApproved}})
	if len(ui.blocks) != before+1 || ui.blocks[before].text != "Approval approved · "+SkillEnableToolName {
		t.Fatalf("approval blocks=%#v", ui.blocks[before:])
	}
}

func TestFullscreenTUIResumePickerNavigatesAndRestoresTranscript(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &commandCountingProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(SessionMetadata{Title: "First session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessages(Message{Role: RoleUser, Content: "first question"}, Message{Role: RoleAssistant, Content: "first answer"}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(SessionMetadata{Title: "Second session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.AppendMessages(Message{Role: RoleUser, Content: "second question"}, Message{Role: RoleAssistant, Content: "second answer"}); err != nil {
		t.Fatal(err)
	}

	terminal := &fakeTUITerminal{width: 88, height: 18}
	ui := newFullscreenTUI(app, terminal, second.ID, "model-a")
	ui.draft = []rune("/resume")
	ui.cursor = len(ui.draft)
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyEnter}, nil); err != nil {
		t.Fatal(err)
	}
	if ui.resumePicker == nil || len(ui.resumePicker.sessions) != 2 {
		t.Fatalf("resume picker=%#v", ui.resumePicker)
	}
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	rendered := terminal.writes.String()
	if !strings.Contains(rendered, "\x1b[1;2H") {
		t.Fatalf("resume picker did not honor the shared left edge padding: %q", rendered)
	}
	for _, want := range []string{"Resume a session", "First session", "Second session", "first question", "second question", "Enter resume"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("resume picker missing %q: %q", want, rendered)
		}
	}

	initial := ui.resumePicker.selected
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyDown}, nil); err != nil {
		t.Fatal(err)
	}
	if ui.resumePicker.selected != initial+1 {
		t.Fatalf("selected=%d want=%d", ui.resumePicker.selected, initial+1)
	}
	target := ui.resumePicker.sessions[ui.resumePicker.selected]
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyEnter}, nil); err != nil {
		t.Fatal(err)
	}
	if ui.resumePicker != nil || ui.sessionID != target.ID {
		t.Fatalf("picker=%#v session=%s want=%s", ui.resumePicker, ui.sessionID, target.ID)
	}
	var transcript strings.Builder
	for _, block := range ui.blocks {
		transcript.WriteString(block.text)
		transcript.WriteByte('\n')
	}
	if !strings.Contains(transcript.String(), target.Title) || !strings.Contains(transcript.String(), target.Preview) || !strings.Contains(transcript.String(), "resumed") {
		t.Fatalf("restored transcript=%q target=%#v", transcript.String(), target)
	}
}

func TestTUIMarkdownFormatsLinksTasksAndInlineEmphasis(t *testing.T) {
	lines := tuiMarkdownVisualLines("- [x] **done** with *care* and [docs](https://example.test)\n1. ~~old~~ new", 120)
	var plain strings.Builder
	var rendered strings.Builder
	for _, line := range lines {
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		rendered.WriteString(line.rendered)
	}
	for _, want := range []string{"☑ done with care and docs (https://example.test)", "1. old new"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("formatted Markdown missing %q: %q", want, plain.String())
		}
	}
	for _, style := range []string{tuiBold, tuiItalic, tuiUnderline, tuiStrike} {
		if !strings.Contains(rendered.String(), style) {
			t.Fatalf("formatted Markdown missing style %q: %q", style, rendered.String())
		}
	}
}

func TestTUIMarkdownFormatsTablesAsAlignedGrid(t *testing.T) {
	markdown := "The breakdown of the 168GB used:\n\n" +
		"| Item | Size |\n" +
		"|---|---:|\n" +
		"| GLM-5.3-Flash-Uncensored Q3KM (4-part split) | 144GB |\n" +
		"| mmproj GLM vision encoder (F16) | 1.1GB |\n" +
		"| Everything else (OS, builds, misc) | ~21GB |"
	lines := tuiMarkdownVisualLines(markdown, 72)
	var plain strings.Builder
	var rendered strings.Builder
	for _, line := range lines {
		if width := tuiDisplayWidth(line.text); width > 72 {
			t.Fatalf("table line width=%d exceeds terminal width: %q", width, line.text)
		}
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		rendered.WriteString(line.rendered)
	}
	for _, want := range []string{"┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘", "GLM-5.3-Flash-Uncensored", "144GB"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("formatted table missing %q: %q", want, plain.String())
		}
	}
	for _, raw := range []string{"| Item | Size |", "|---|---:|"} {
		if strings.Contains(plain.String(), raw) {
			t.Fatalf("formatted table retained Markdown row %q: %q", raw, plain.String())
		}
	}
	if !strings.Contains(plain.String(), "│  Size │") {
		t.Fatalf("right-aligned size column missing: %q", plain.String())
	}
	if !strings.Contains(rendered.String(), tuiBold) || !strings.Contains(rendered.String(), tuiGreen) {
		t.Fatalf("table missing header/border styling: %q", rendered.String())
	}
}

func TestTUIMarkdownTableFallsBackToLabelsOnNarrowScreens(t *testing.T) {
	lines := tuiMarkdownVisualLines("| Item | Size |\n|---|---|\n| alpha | 1GB |\n| beta | 2GB |", 14)
	var plain strings.Builder
	for _, line := range lines {
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		if width := tuiDisplayWidth(line.text); width > 14 {
			t.Fatalf("narrow table line width=%d exceeds terminal width: %q", width, line.text)
		}
	}
	for _, want := range []string{"Item: alpha", "Size: 1GB", "Item: beta", "Size: 2GB"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("narrow table missing %q: %q", want, plain.String())
		}
	}
	if strings.ContainsAny(plain.String(), "┌┬┐") {
		t.Fatalf("narrow table unexpectedly used a grid: %q", plain.String())
	}
}

func TestTUIMarkdownTablePreservesEscapedAndCodePipes(t *testing.T) {
	lines := tuiMarkdownVisualLines("| Item | Note |\n|---|---|\n| A \\| B | `x|y` |", 60)
	var plain strings.Builder
	var rendered strings.Builder
	for _, line := range lines {
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		rendered.WriteString(line.rendered)
	}
	for _, want := range []string{"A | B", "x|y"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("table cell missing %q: %q", want, plain.String())
		}
	}
	if !strings.Contains(rendered.String(), tuiInlineCode) {
		t.Fatalf("table code cell missing inline-code style: %q", rendered.String())
	}
}

func TestTUIMarkdownUsesAssistantGreenForGeneratedText(t *testing.T) {
	lines := tuiMarkdownVisualLines("plain text\n# Heading\n- list item\n```go\nreturn value\n```", 80)
	for _, line := range lines {
		visible := strings.TrimSpace(line.text)
		if visible == "" {
			continue
		}
		if !strings.Contains(line.rendered, tuiGreen) {
			t.Fatalf("model line %q does not use assistant green: %q", line.text, line.rendered)
		}
	}
}

func TestFullscreenTUIScrollOffsetTracksStreamingOutput(t *testing.T) {
	terminal := &fakeTUITerminal{width: 30, height: 8}
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, terminal, "", "")
	for index := 0; index < 12; index++ {
		ui.addBlock(tuiBlockMeta, "older transcript line")
	}
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	ui.scrollOffset = 2
	previousRows := ui.lastVisualRows
	ui.appendAssistant("new line while scrolled")
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	if ui.scrollOffset <= 2 || ui.lastVisualRows <= previousRows {
		t.Fatalf("scroll offset=%d visual rows=%d, want anchored above new output", ui.scrollOffset, ui.lastVisualRows)
	}
}

func TestFullscreenTUIPageScrollUsesVisibleTranscriptRows(t *testing.T) {
	terminal := &fakeTUITerminal{width: 120, height: 30}
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, terminal, "session-a", "")
	for index := 0; index < 80; index++ {
		ui.addBlock(tuiBlockActivity, fmt.Sprintf("activity %02d", index))
	}
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}

	transcriptRows := terminal.height - 4 - tuiMastheadRows
	beforeFirst := ui.renderedRows[tuiMastheadRows]
	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyPageUp}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := ui.scrollOffset, transcriptRows-1; got != want {
		t.Fatalf("page-up offset=%d, want visible transcript page %d", got, want)
	}
	if err := ui.render(); err != nil {
		t.Fatal(err)
	}
	if got := ui.renderedRows[tuiMastheadRows+transcriptRows-1]; got != beforeFirst {
		t.Fatalf("page-up boundary=%q, want overlap with prior first row %q", got, beforeFirst)
	}

	if _, _, _, err := ui.handleKey(context.Background(), tuiKey{kind: tuiKeyPageDown}, nil); err != nil {
		t.Fatal(err)
	}
	if ui.scrollOffset != 0 {
		t.Fatalf("page-down offset=%d, want bottom", ui.scrollOffset)
	}
}

func TestTUIKeyParserHandlesEditingUnicodeMousePasteAndNewline(t *testing.T) {
	parser := &tuiKeyParser{}
	keys := parser.feed([]byte("aλ\x1b[D\x7f\x1b[<64;2;2M\x1b[200~two\nlines\x1b[201~\n\r"))
	wants := []tuiKeyKind{tuiKeyRune, tuiKeyRune, tuiKeyLeft, tuiKeyBackspace, tuiKeyScrollUp, tuiKeyPaste, tuiKeyNewline, tuiKeyEnter}
	if len(keys) != len(wants) {
		t.Fatalf("keys=%#v", keys)
	}
	for index, want := range wants {
		if keys[index].kind != want {
			t.Fatalf("key %d kind=%v want=%v", index, keys[index].kind, want)
		}
	}
	if keys[5].text != "two\nlines" {
		t.Fatalf("paste=%q", keys[5].text)
	}
	if keys := parser.feed([]byte{0x1b}); len(keys) != 0 {
		t.Fatalf("standalone escape emitted before timeout: %#v", keys)
	}
	if keys := parser.feed(nil); len(keys) != 1 || keys[0].kind != tuiKeyEscape {
		t.Fatalf("standalone escape after timeout=%#v", keys)
	}
}

func TestTUIKeyParserHandlesCommonModifiedEditingSequences(t *testing.T) {
	tests := []struct {
		name string
		data string
		want tuiKeyKind
	}{
		{name: "xterm ctrl-left", data: "\x1b[1;5D", want: tuiKeyWordLeft},
		{name: "xterm alt-right", data: "\x1b[1;3C", want: tuiKeyWordRight},
		{name: "readline alt-left", data: "\x1bb", want: tuiKeyWordLeft},
		{name: "readline alt-right", data: "\x1bf", want: tuiKeyWordRight},
		{name: "readline alt-backspace", data: "\x1b\x7f", want: tuiKeyDeleteWord},
		{name: "readline alt-delete", data: "\x1bd", want: tuiKeyDeleteNextWord},
		{name: "xterm ctrl-delete", data: "\x1b[3;5~", want: tuiKeyDeleteNextWord},
		{name: "modify-other-keys ctrl-backspace", data: "\x1b[27;5;127~", want: tuiKeyDeleteWord},
		{name: "kitty ctrl-backspace", data: "\x1b[127;5u", want: tuiKeyDeleteWord},
		{name: "kitty ctrl-left", data: "\x1b[57350;5u", want: tuiKeyWordLeft},
		{name: "application cursor up", data: "\x1bOA", want: tuiKeyUp},
		{name: "shift-left stays character movement", data: "\x1b[1;2D", want: tuiKeyLeft},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := (&tuiKeyParser{}).feed([]byte(test.data))
			if len(keys) != 1 || keys[0].kind != test.want {
				t.Fatalf("keys=%#v, want one %v", keys, test.want)
			}
		})
	}
}

func TestTUIKeyParserDistinguishesControlBackspaceFromConfiguredEraseKey(t *testing.T) {
	parser := &tuiKeyParser{eraseByte: 0x7f}
	keys := parser.feed([]byte{0x7f, 0x08})
	if len(keys) != 2 || keys[0].kind != tuiKeyBackspace || keys[1].kind != tuiKeyDeleteWord {
		t.Fatalf("keys=%#v, want backspace followed by delete-word", keys)
	}
}

func TestFullscreenTUIInitialSessionIsNotRepeatedAtRunEnd(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 80, height: 24}, "session-a", "")
	ui.handleRunEvent(RunEvent{Type: RunEnd, SessionID: "session-a"})
	count := 0
	for _, block := range ui.blocks {
		if block.text == "session session-a" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("session announcement count=%d", count)
	}
}

func TestFullscreenTUIProviderRecoveryStartsFreshAssistantSegment(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 80, height: 24}, "session-a", "")
	ui.deltaSeen = true
	ui.assistantOpen = true
	ui.handleRunEvent(RunEvent{
		Type: RunProviderOutputRejected,
		ProviderOutput: &ProviderOutputRejection{
			Retrying: true,
		},
	})

	if ui.deltaSeen || ui.assistantOpen {
		t.Fatalf("provider recovery retained streaming state: deltaSeen=%t assistantOpen=%t", ui.deltaSeen, ui.assistantOpen)
	}
	if got := ui.blocks[len(ui.blocks)-1].text; got != "Provider output rejected; no tool executed; retrying once" {
		t.Fatalf("recovery activity=%q", got)
	}
}

func TestFullscreenTUIProviderRequestRetryIsNotLabeledRejectedOutput(t *testing.T) {
	ui := newFullscreenTUI(&Application{Config: DefaultConfig()}, &fakeTUITerminal{width: 80, height: 24}, "session-a", "")
	ui.handleRunEvent(RunEvent{
		Type: RunProviderOutputRejected,
		ProviderOutput: &ProviderOutputRejection{
			Stage:    ProviderFailureStageRequest,
			Retrying: true,
		},
	})

	if got := ui.blocks[len(ui.blocks)-1].text; got != "Provider request failed before response; no tool executed; retrying once" {
		t.Fatalf("request retry activity=%q", got)
	}
}

func TestFullscreenTUIShowsReasoningToolDetailsDiffAndStatusCard(t *testing.T) {
	config := DefaultConfig()
	config.Generation.ReasoningEffort = "high"
	config.Provider.ContextWindowTokens = 131072
	app := &Application{Config: config, ProviderProfile: "main", Capabilities: AssistantCapabilities{ScopeRoot: "/work/demo"}}
	app.SetToolDisplayMode(ToolDisplayFull)
	ui := newFullscreenTUI(app, &fakeTUITerminal{width: 88, height: 30}, "session-a", "model-a")
	ui.handleRunEvent(RunEvent{Type: RunReasoningDelta, Reasoning: &ReasoningRunEvent{Delta: "check the patch"}})
	ui.handleRunEvent(RunEvent{Type: RunReasoningFinal, Reasoning: &ReasoningRunEvent{Content: "check the patch", Complete: true}})
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{Name: "write_file", Arguments: `{"path":"main.go","operation":"edit","old_text":"old","new_text":"new"}`}})
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "call-edit-1", ToolName: "write_file", Output: "updated main.go"}})
	status := commandStatus(context.Background(), CommandContext{Application: app, SessionID: "session-a", Model: "model-a"})
	ui.addStatusBlock(status)

	visual := ui.visualLines(88)
	var plain strings.Builder
	var rendered strings.Builder
	for _, line := range visual {
		plain.WriteString(line.text)
		plain.WriteByte('\n')
		rendered.WriteString(line.rendered)
	}
	for _, want := range []string{"◆ Thinking", "check the patch", "● Using write_file", `"path": "main.go"`, "┌ Proposed diff · main.go", "-old", "+new", "└ completed · write_file", "evidence: tool_result/call-edit-1", "Bee status", "Model", "model-a", "Reasoning", "high", "Context window", "131,072 tokens"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("visual transcript missing %q: %q", want, plain.String())
		}
	}
	if !strings.Contains(rendered.String(), tuiRed+"  │ -old") || !strings.Contains(rendered.String(), tuiGreen+"  │ +new") {
		t.Fatalf("rendered diff lacks line colors: %q", rendered.String())
	}
	if !strings.Contains(plain.String(), "\n\n◆ Thinking") || !strings.Contains(plain.String(), "\n\n╭─ Bee status") {
		t.Fatalf("major transcript sections lack compact spacing: %q", plain.String())
	}
}

func TestFullscreenTUICompactsPairedToolsAndGroupsExploration(t *testing.T) {
	ui := newFullscreenTUI(&Application{}, &fakeTUITerminal{width: 88, height: 30}, "session-a", "model-a")
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`}})
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "read", ToolName: "read_file", Output: "private file body"}})
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "grep", Name: "grep", Arguments: `{"path":".","query":"ToolCall"}`}})
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "grep", ToolName: "grep", Output: "private matches"}})
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{ID: "edit", Name: "edit_file", Arguments: `{"path":"main.go","old":"one\ntwo","new":"one\nthree\nfour"}`}})
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{CallID: "edit", ToolName: "edit_file", Output: "updated main.go"}})

	var toolBlocks []tuiBlock
	for _, block := range ui.blocks {
		if len(block.toolActivities) > 0 {
			toolBlocks = append(toolBlocks, block)
		}
	}
	if len(toolBlocks) != 2 {
		t.Fatalf("compact tool blocks=%#v, want one grouped exploration block and one edit block", toolBlocks)
	}
	if len(toolBlocks[0].toolActivities) != 2 || len(toolBlocks[1].toolActivities) != 1 {
		t.Fatalf("compact activities=%#v", toolBlocks)
	}
	var transcript strings.Builder
	for _, block := range toolBlocks {
		transcript.WriteString(block.text)
		transcript.WriteByte('\n')
	}
	got := transcript.String()
	for _, want := range []string{"Explored", "├─ Read main.go", `└─ Searched "ToolCall"`, "Updated main.go · +3 −2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compact transcript=%q missing %q", got, want)
		}
	}
	for _, hidden := range []string{"private file body", "private matches", "updated main.go", `"old"`, "Proposed diff"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("compact transcript=%q contains %q", got, hidden)
		}
	}
}

func TestFullscreenTUICanHideThinkingAndToolDetails(t *testing.T) {
	app := &Application{}
	app.SetThinkingVisible(false)
	app.SetToolCallsVisible(false)
	ui := newFullscreenTUI(app, &fakeTUITerminal{width: 88, height: 30}, "session-a", "model-a")
	ui.handleRunEvent(RunEvent{Type: RunReasoningDelta, Reasoning: &ReasoningRunEvent{Delta: "private reasoning"}})
	ui.handleRunEvent(RunEvent{Type: RunReasoningFinal, Reasoning: &ReasoningRunEvent{Content: "private reasoning", Complete: true}})
	ui.handleRunEvent(RunEvent{Type: RunToolCall, ToolCall: &ToolCall{Name: "exec", Arguments: `{"command":"secret command"}`}})
	if ui.activity != "Working…" {
		t.Fatalf("hidden tool call exposed activity=%q", ui.activity)
	}
	ui.handleRunEvent(RunEvent{Type: RunToolResult, ToolResult: &ToolResultEvent{ToolName: "exec", Output: "secret output"}})
	ui.handleRunEvent(RunEvent{Type: RunAssistantDelta, Delta: "public answer"})

	var transcript strings.Builder
	for _, block := range ui.blocks {
		transcript.WriteString(block.text)
		transcript.WriteByte('\n')
	}
	got := transcript.String()
	if !strings.Contains(got, "public answer") {
		t.Fatalf("transcript omitted assistant answer: %q", got)
	}
	for _, hidden := range []string{"private reasoning", "secret command", "secret output"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("transcript=%q contains hidden detail %q", got, hidden)
		}
	}
}

func TestTUIDetailCardsBoundLargePayloadsAndNarrowStatus(t *testing.T) {
	call := ToolCall{Name: "write_file", Arguments: `{"path":"important.txt","content":"` + strings.Repeat("x", 20000) + `","password":"do-not-show"}`}
	display := tuiToolCallDisplay(call)
	if len(display.Text) > tuiMaxToolArgumentDisplayBytes+512 || !strings.Contains(display.Text, "important.txt") || !strings.Contains(display.Text, "[20000 bytes]") {
		t.Fatalf("bounded call display=%q len=%d", display.Text, len(display.Text))
	}
	if strings.Contains(display.Text, "do-not-show") || !strings.Contains(display.Text, "[REDACTED]") {
		t.Fatalf("call display did not redact secret: %q", display.Text)
	}
	result := tuiToolResultDisplay(ToolResultEvent{ToolName: "exec", Output: strings.Repeat("line\n", 10000)})
	if len(result.Text) > tuiMaxToolOutputDisplayBytes+512 || !strings.Contains(result.Text, "display truncated") {
		t.Fatalf("bounded result len=%d text=%q", len(result.Text), result.Text)
	}

	card := FormatCommandStatus(CommandStatusData{Model: strings.Repeat("model", 30), Directory: "/very/long/directory", ReasoningEffort: "high"}, 40)
	for _, line := range strings.Split(card, "\n") {
		if got := len([]rune(line)); got != 40 {
			t.Fatalf("status line width=%d, want 40: %q", got, line)
		}
	}
}
