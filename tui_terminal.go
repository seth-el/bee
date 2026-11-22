package bee

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	tuiMaxTranscriptBlocks = 4000
	tuiMaxDraftRunes       = 1 << 20
	tuiMaxComposerLines    = 5
	tuiComposerTopPadding  = 1
	tuiEdgePadding         = 1
	tuiContentColumn       = tuiEdgePadding + 1
	tuiMaxResumeMessages   = 200
	tuiMaxResumeBlockBytes = 64 << 10
)

type tuiTerminalDevice interface {
	enter() error
	leave() error
	size() (int, int, error)
	read(time.Duration) ([]byte, error)
	write(string) error
}

type tuiBlockKind uint8

const (
	tuiBlockHeader tuiBlockKind = iota
	tuiBlockUser
	tuiBlockAssistant
	tuiBlockActivity
	tuiBlockBranch
	tuiBlockError
	tuiBlockMeta
	tuiBlockReasoning
	tuiBlockTool
	tuiBlockToolResult
	tuiBlockToolError
	tuiBlockDiff
	tuiBlockStatus
)

type tuiBlock struct {
	kind           tuiBlockKind
	text           string
	status         *CommandStatusData
	toolActivities []tuiToolActivity
	masthead       bool
}

type tuiResumePicker struct {
	sessions []CommandResumeSession
	selected int
}

type fullscreenTUI struct {
	app       *Application
	terminal  tuiTerminalDevice
	sessionID SessionID
	model     string
	profile   string
	scope     string

	blocks          []tuiBlock
	draft           []rune
	cursor          int
	preferredColumn int
	history         []string
	historyIndex    int
	historyDraft    string
	scrollOffset    int
	lastVisualRows  int
	transcriptRows  int
	running         bool
	activity        string
	notice          string
	assistantOpen   bool
	deltaSeen       bool
	reasoningOpen   bool
	reasoningSeen   bool
	newSessionShown bool
	pendingApproval *ApprovalEvent
	resumePicker    *tuiResumePicker
	pendingSteer    string
	usageCalls      int
	totalTokens     int
	perf            runPerformance
	runStartedAt    time.Time
	contextUsed     int
	contextWindow   int
	contextKnown    bool
	inboxUnread     int
	networkOff      bool
	inboxTruncated  bool
	inboxCheckedAt  time.Time
	runState        tuiActivity

	width        int
	height       int
	dirty        bool
	renderedRows []string
}

func newFullscreenTUI(app *Application, terminal tuiTerminalDevice, sessionID SessionID, model string) *fullscreenTUI {
	ui := &fullscreenTUI{
		app:             app,
		terminal:        terminal,
		sessionID:       sessionID,
		model:           strings.TrimSpace(model),
		preferredColumn: -1,
		historyIndex:    -1,
		activity:        "Message",
		dirty:           true,
		newSessionShown: sessionID != "",
		runState:        newTUIActivity(),
	}
	if app != nil {
		ui.profile = strings.TrimSpace(app.ProviderProfile)
		if ui.profile == "" {
			ui.profile = "default profile"
		}
		if ui.model == "" {
			ui.model = strings.TrimSpace(app.Config.Provider.Model)
		}
		ui.scope = strings.TrimSpace(app.Capabilities.ScopeRoot)
		if ui.scope == "" && app.Executor != nil {
			ui.scope = strings.TrimSpace(app.Executor.Scope.Root)
		}
	}
	if ui.model == "" {
		ui.model = "configured model"
	}
	if ui.profile == "" {
		ui.profile = "default profile"
	}
	ui.refreshContextUsage()
	ui.refreshInbox(time.Now())
	ui.addMastheadBlock(tuiBlockHeader, "Bee")
	ui.addMastheadBlock(tuiBlockMeta, ui.model+" · "+ui.profile)
	if ui.scope != "" {
		ui.addMastheadBlock(tuiBlockMeta, ui.scope)
	}
	if ui.sessionID != "" {
		ui.addMastheadBlock(tuiBlockMeta, "session "+string(ui.sessionID))
	}
	ui.addMastheadBlock(tuiBlockMeta, "/help for commands · /exit to quit")
	return ui
}

func (ui *fullscreenTUI) refreshContextUsage() {
	ui.contextUsed = 0
	ui.contextWindow = 0
	ui.contextKnown = false
	if ui.app == nil {
		return
	}
	ui.contextWindow = ui.app.Config.Provider.ContextWindowTokens
	if ui.contextWindow <= 0 {
		if discoverer, ok := ui.app.Provider.(ProviderContextWindow); ok {
			ui.contextWindow, _ = discoverer.ContextWindowTokens(ui.model)
		}
	}
	telemetry := ui.app.commandStatusTelemetry(ui.sessionID)
	if !telemetry.contextUsageSeen {
		return
	}
	ui.contextUsed = telemetry.contextUsage.PromptUsed
	ui.contextKnown = true
	if telemetry.contextUsage.WindowKnown && telemetry.contextUsage.WindowTokens > 0 {
		ui.contextWindow = telemetry.contextUsage.WindowTokens
	}
}

// tuiInboxRefreshInterval bounds how often the TUI rereads the notification
// inbox for its unread badge; the inbox is a locked file, so it is never read
// per frame.
const tuiInboxRefreshInterval = 30 * time.Second

// refreshInbox updates the unread badge. Failures leave the previous count:
// the badge is a hint, never a reason to interrupt the session.
func (ui *fullscreenTUI) refreshInbox(now time.Time) {
	ui.inboxCheckedAt = now
	ui.refreshNetwork()
	if ui.app == nil || ui.app.NotificationStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	result, err := ui.app.NotificationStore.List(ctx, 1)
	if err != nil {
		return
	}
	if result.UnreadCount != ui.inboxUnread || result.UnreadTruncated != ui.inboxTruncated {
		ui.inboxUnread, ui.inboxTruncated = result.UnreadCount, result.UnreadTruncated
		ui.dirty = true
	}
}

// refreshNetwork shows "network off" in the status line while this
// session's network is off (G11).
func (ui *fullscreenTUI) refreshNetwork() {
	if ui.app == nil {
		return
	}
	if enabled, err := ui.app.SessionNetworkEnabled(ui.sessionID); err == nil && ui.networkOff != !enabled {
		ui.networkOff = !enabled
		ui.dirty = true
	}
}

func tuiInboxBadge(unread int, truncated bool) string {
	if unread <= 0 {
		return ""
	}
	count := fmt.Sprintf("%d", unread)
	if truncated {
		count += "+"
	}
	return "inbox " + count + " unread · /inbox"
}

func runFullscreenTUI(ctx context.Context, app *Application, terminal tuiTerminalDevice, initialSessionID SessionID, model string) (returnErr error) {
	if err := terminal.enter(); err != nil {
		return err
	}
	defer func() {
		if err := terminal.leave(); returnErr == nil {
			returnErr = err
		}
	}()

	ui := newFullscreenTUI(app, terminal, initialSessionID, model)
	if app.Executor != nil {
		if err := app.EnableApprovalBroker(); err != nil {
			return err
		}
	}
	keys := make(chan tuiKey, 32)
	readerDone := make(chan struct{})
	readerStopped := make(chan struct{})
	defer func() {
		close(readerDone)
		<-readerStopped
	}()
	go func() {
		defer close(readerStopped)
		readTUIKeys(terminal, keys, readerDone)
	}()

	ticker := time.NewTicker(16 * time.Millisecond)
	defer ticker.Stop()

	var events <-chan RunEvent
	var cancelRun context.CancelFunc
	if err := ui.render(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			if cancelRun != nil {
				cancelRun()
			}
			return ctx.Err()
		case key := <-keys:
			exit, nextEvents, nextCancel, err := ui.handleKey(ctx, key, cancelRun)
			if err != nil {
				return err
			}
			if nextEvents != nil {
				events = nextEvents
				cancelRun = nextCancel
			}
			if exit {
				if cancelRun != nil {
					cancelRun()
				}
				return nil
			}
			if err := ui.render(); err != nil {
				return err
			}
		case event, ok := <-events:
			if !ok {
				events = nil
				if cancelRun != nil {
					cancelRun()
					cancelRun = nil
				}
				ui.running = false
				if ui.pendingSteer != "" {
					prompt := ui.pendingSteer
					ui.pendingSteer = ""
					nextEvents, nextCancel, err := ui.startRun(ctx, prompt)
					if err != nil {
						ui.addBlock(tuiBlockError, "Error steering response: "+err.Error())
						ui.activity = "Message"
					} else {
						events = nextEvents
						cancelRun = nextCancel
					}
				} else {
					ui.activity = "Message"
				}
				ui.dirty = true
				continue
			}
			ui.handleRunEvent(event)
		case <-ticker.C:
			width, height, err := terminal.size()
			if err != nil {
				return err
			}
			if width != ui.width || height != ui.height {
				ui.dirty = true
			}
			if time.Since(ui.inboxCheckedAt) >= tuiInboxRefreshInterval {
				ui.refreshInbox(time.Now())
			}
			if ui.runState.advance(time.Now()) && tuiMastheadVisible(tuiRenderWidth(width), height-3-tuiComposerTopPadding) {
				ui.dirty = true
			}
			if ui.dirty {
				if err := ui.render(); err != nil {
					return err
				}
			}
		}
	}
}

func (ui *fullscreenTUI) handleKey(ctx context.Context, key tuiKey, currentCancel context.CancelFunc) (bool, <-chan RunEvent, context.CancelFunc, error) {
	if ui.pendingApproval != nil {
		decision := ApprovalDecision("")
		switch {
		case key.kind == tuiKeyRune && (key.rune == 'y' || key.rune == 'Y'):
			decision = ApprovalApproved
		case key.kind == tuiKeyRune && (key.rune == 's' || key.rune == 'S') && ui.pendingApproval.Request.SessionGrant != "":
			decision = ApprovalApprovedForSession
		case key.kind == tuiKeyRune && (key.rune == 'n' || key.rune == 'N'):
			decision = ApprovalDenied
		case key.kind == tuiKeyEnter || key.kind == tuiKeyInterrupt || key.kind == tuiKeyEOF:
			decision = ApprovalDenied
		default:
			ui.notice = tuiCompactText(ui.pendingApproval.Request.ToolName, 28) + " · y approve" + tuiSessionGrantHint(ui.pendingApproval.Request) + " · n deny"
			ui.dirty = true
			return false, nil, nil, nil
		}
		approval := ui.pendingApproval
		if ui.app == nil {
			return false, nil, nil, errors.New("approval application is unavailable")
		}
		if err := ui.app.DecideApproval(approval.ID, decision); err != nil {
			return false, nil, nil, err
		}
		ui.pendingApproval = nil
		ui.notice = ""
		ui.activity = "Working…"
		return false, nil, nil, nil
	}
	if ui.resumePicker != nil {
		return ui.handleResumePickerKey(key)
	}
	ui.notice = ""
	switch key.kind {
	case tuiKeyEOF:
		if key.err != nil && !errors.Is(key.err, io.EOF) {
			return false, nil, nil, key.err
		}
		return true, nil, nil, nil
	case tuiKeyRune:
		ui.insertRunes([]rune{key.rune})
	case tuiKeyNewline:
		ui.insertRunes([]rune{'\n'})
	case tuiKeyPaste:
		text := strings.ReplaceAll(key.text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")
		value := strings.Map(func(r rune) rune {
			if r == '\t' {
				return ' '
			}
			if r == '\n' {
				return r
			}
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, text)
		ui.insertRunes([]rune(value))
	case tuiKeyLeft:
		if ui.cursor > 0 {
			ui.cursor--
		}
		ui.preferredColumn = -1
	case tuiKeyRight:
		if ui.cursor < len(ui.draft) {
			ui.cursor++
		}
		ui.preferredColumn = -1
	case tuiKeyWordLeft:
		ui.moveWordLeft()
	case tuiKeyWordRight:
		ui.moveWordRight()
	case tuiKeyHome:
		ui.cursor = 0
		ui.preferredColumn = -1
	case tuiKeyEnd:
		ui.cursor = len(ui.draft)
		ui.preferredColumn = -1
	case tuiKeyBackspace:
		if ui.cursor > 0 {
			ui.draft = append(ui.draft[:ui.cursor-1], ui.draft[ui.cursor:]...)
			ui.cursor--
		}
		ui.preferredColumn = -1
	case tuiKeyDelete:
		if ui.cursor < len(ui.draft) {
			ui.draft = append(ui.draft[:ui.cursor], ui.draft[ui.cursor+1:]...)
		}
		ui.preferredColumn = -1
	case tuiKeyDeleteWord:
		ui.deletePreviousWord()
	case tuiKeyDeleteNextWord:
		ui.deleteNextWord()
	case tuiKeyClear:
		ui.draft = nil
		ui.cursor = 0
		ui.preferredColumn = -1
		ui.historyIndex = -1
	case tuiKeyUp:
		if !ui.moveCursorVertical(-1) {
			ui.moveHistory(-1)
		}
	case tuiKeyDown:
		if !ui.moveCursorVertical(1) {
			ui.moveHistory(1)
		}
	case tuiKeyPageUp:
		ui.scrollOffset += ui.transcriptPageSize()
	case tuiKeyPageDown:
		ui.scrollOffset -= ui.transcriptPageSize()
		if ui.scrollOffset < 0 {
			ui.scrollOffset = 0
		}
	case tuiKeyScrollUp:
		ui.scrollOffset += 3
	case tuiKeyScrollDown:
		ui.scrollOffset -= 3
		if ui.scrollOffset < 0 {
			ui.scrollOffset = 0
		}
	case tuiKeyInterrupt:
		if ui.running && currentCancel != nil {
			currentCancel()
			ui.activity = "Cancelling…"
		} else {
			ui.draft = nil
			ui.cursor = 0
			ui.preferredColumn = -1
		}
	case tuiKeyEOFOrDelete:
		if len(ui.draft) == 0 {
			return true, nil, nil, nil
		}
		if ui.cursor < len(ui.draft) {
			ui.draft = append(ui.draft[:ui.cursor], ui.draft[ui.cursor+1:]...)
		}
		ui.preferredColumn = -1
	case tuiKeyEnter:
		prompt := strings.TrimSpace(string(ui.draft))
		if ui.running {
			fields := strings.Fields(prompt)
			if len(fields) == 0 || fields[0] != "/steer" {
				ui.notice = "response in progress · use /steer <correction> or Ctrl-C"
				break
			}
			command := DispatchCommand(ctx, prompt, CommandContext{Application: ui.app, SessionID: ui.sessionID, Model: ui.model, RunActive: true})
			if command.Steer == nil {
				ui.notice = "response in progress · use /steer <correction> or Ctrl-C"
				break
			}
			if ui.pendingSteer != "" {
				ui.notice = "a steering correction is already queued"
				break
			}
			ui.draft = nil
			ui.cursor = 0
			ui.history = append(ui.history, prompt)
			ui.historyIndex = -1
			ui.historyDraft = ""
			ui.scrollOffset = 0
			ui.addBlock(tuiBlockUser, prompt)
			ui.pendingSteer = command.Steer.Prompt
			ui.activity = "Steering…"
			ui.notice = "interrupting the current response"
			if currentCancel != nil {
				currentCancel()
			}
			break
		}
		if prompt == "" {
			break
		}
		ui.draft = nil
		ui.cursor = 0
		ui.history = append(ui.history, prompt)
		ui.historyIndex = -1
		ui.historyDraft = ""
		ui.scrollOffset = 0
		ui.addBlock(tuiBlockUser, prompt)
		if prompt == "/exit" {
			return true, nil, nil, nil
		}
		command := DispatchCommand(ctx, prompt, CommandContext{Application: ui.app, SessionID: ui.sessionID, Model: ui.model})
		if command.Inbox != nil {
			ui.inboxUnread, ui.inboxTruncated = command.Inbox.UnreadCount, command.Inbox.UnreadTruncated
		}
		if command.Name == CommandNetwork {
			ui.refreshNetwork()
		}
		if command.Handled {
			if command.Continue != nil {
				events, cancel, err := ui.startRunFromCheckpoint(ctx, command.Continue.Prompt, command.Continue.CheckpointID)
				if err != nil {
					ui.addBlock(tuiBlockError, "Error "+err.Error())
					break
				}
				return false, events, cancel, nil
			} else if command.NewSession != nil {
				if err := ui.activateNewSession(command.NewSession.Session.ID); err != nil {
					ui.addBlock(tuiBlockError, "Error starting session: "+err.Error())
				}
			} else if command.Resume != nil && command.Resume.Selected != nil {
				if err := ui.activateSession(command.Resume.Selected.ID); err != nil {
					ui.addBlock(tuiBlockError, "Error resuming session: "+err.Error())
				}
			} else if command.Resume != nil && len(command.Resume.Sessions) > 0 {
				ui.resumePicker = &tuiResumePicker{sessions: append([]CommandResumeSession(nil), command.Resume.Sessions...)}
				ui.activity = "Resume a session"
			} else if command.Status != nil {
				ui.addStatusBlock(*command.Status)
			} else if command.Text != "" {
				ui.addBlock(tuiBlockMeta, strings.TrimRight(tuiSafeText(command.Text), "\n"))
			}
			break
		}
		events, cancel, err := ui.startRun(ctx, prompt)
		if err != nil {
			ui.addBlock(tuiBlockError, "Error "+err.Error())
			break
		}
		return false, events, cancel, nil
	}
	ui.dirty = true
	return false, nil, nil, nil
}

func (ui *fullscreenTUI) transcriptPageSize() int {
	page := ui.transcriptRows - 1
	if page < 1 {
		return 1
	}
	return page
}

func (ui *fullscreenTUI) startRun(ctx context.Context, prompt string) (<-chan RunEvent, context.CancelFunc, error) {
	return ui.startRunFromCheckpoint(ctx, prompt, "")
}

func (ui *fullscreenTUI) startRunFromCheckpoint(ctx context.Context, prompt, checkpointID string) (<-chan RunEvent, context.CancelFunc, error) {
	if ui.sessionID == "" && ui.app != nil && ui.app.Store != nil {
		session, err := ui.app.Store.Create(SessionMetadata{})
		if err != nil {
			return nil, nil, err
		}
		ui.sessionID = session.ID
		ui.refreshNetwork()
		ui.refreshNetwork()
	}
	runCtx, cancel := context.WithCancel(ctx)
	events, err := ui.app.Run(runCtx, RunRequest{Prompt: prompt, SessionID: ui.sessionID, Model: ui.model, CheckpointID: checkpointID})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	ui.running = true
	ui.activity = "Working…"
	ui.runState.beginRun(time.Now())
	ui.notice = ""
	ui.usageCalls = 0
	ui.totalTokens = 0
	ui.perf = runPerformance{}
	return events, cancel, nil
}

func (ui *fullscreenTUI) handleResumePickerKey(key tuiKey) (bool, <-chan RunEvent, context.CancelFunc, error) {
	picker := ui.resumePicker
	if picker == nil || len(picker.sessions) == 0 {
		ui.resumePicker = nil
		ui.activity = "Message"
		ui.dirty = true
		return false, nil, nil, nil
	}
	move := 0
	switch key.kind {
	case tuiKeyEOF:
		if key.err != nil && !errors.Is(key.err, io.EOF) {
			return false, nil, nil, key.err
		}
		return true, nil, nil, nil
	case tuiKeyInterrupt, tuiKeyEscape:
		ui.resumePicker = nil
		ui.activity = "Message"
		ui.notice = "resume cancelled"
	case tuiKeyRune:
		switch key.rune {
		case 'j', 'J':
			move = 1
		case 'k', 'K':
			move = -1
		case 'q', 'Q':
			ui.resumePicker = nil
			ui.activity = "Message"
			ui.notice = "resume cancelled"
		}
	case tuiKeyUp, tuiKeyScrollUp:
		move = -1
	case tuiKeyDown, tuiKeyScrollDown:
		move = 1
	case tuiKeyPageUp:
		move = -5
	case tuiKeyPageDown:
		move = 5
	case tuiKeyHome:
		picker.selected = 0
	case tuiKeyEnd:
		picker.selected = len(picker.sessions) - 1
	case tuiKeyEnter, tuiKeyNewline:
		selected := picker.sessions[picker.selected]
		ui.resumePicker = nil
		if err := ui.activateSession(selected.ID); err != nil {
			ui.activity = "Message"
			ui.addBlock(tuiBlockError, "Error resuming session: "+err.Error())
		}
	case tuiKeyEOFOrDelete:
		ui.resumePicker = nil
		ui.activity = "Message"
		ui.notice = "resume cancelled"
	}
	if ui.resumePicker != nil && move != 0 {
		picker.selected += move
		if picker.selected < 0 {
			picker.selected = 0
		}
		if picker.selected >= len(picker.sessions) {
			picker.selected = len(picker.sessions) - 1
		}
	}
	ui.dirty = true
	return false, nil, nil, nil
}

func (ui *fullscreenTUI) activateSession(id SessionID) error {
	return ui.activateDurableSession(id, "resumed")
}

func (ui *fullscreenTUI) activateNewSession(id SessionID) error {
	return ui.activateDurableSession(id, "new session")
}

func (ui *fullscreenTUI) activateDurableSession(id SessionID, notice string) error {
	if ui.app == nil {
		return errors.New("application is not initialized")
	}
	session, err := ui.app.Resume(id)
	if err != nil {
		return err
	}
	messages, err := session.Messages()
	if err != nil {
		return err
	}
	visible := make([]Message, 0, len(messages))
	for _, message := range messages {
		if (message.Role == RoleUser || message.Role == RoleAssistant) && strings.TrimSpace(message.Content) != "" {
			visible = append(visible, message)
		}
	}
	omitted := 0
	if len(visible) > tuiMaxResumeMessages {
		omitted = len(visible) - tuiMaxResumeMessages
		visible = visible[omitted:]
	}

	ui.sessionID = session.ID
	ui.blocks = nil
	ui.scrollOffset = 0
	ui.lastVisualRows = 0
	ui.assistantOpen = false
	ui.deltaSeen = false
	ui.reasoningOpen = false
	ui.reasoningSeen = false
	ui.newSessionShown = true
	ui.usageCalls = 0
	ui.totalTokens = 0
	ui.perf = runPerformance{}
	ui.refreshContextUsage()
	ui.notice = ""
	ui.activity = "Message"
	ui.addMastheadBlock(tuiBlockHeader, "Bee")
	ui.addMastheadBlock(tuiBlockMeta, ui.model+" · "+ui.profile)
	if ui.scope != "" {
		ui.addMastheadBlock(tuiBlockMeta, ui.scope)
	}
	if session.Metadata.Title != "" {
		ui.addBlock(tuiBlockMeta, session.Metadata.Title)
	}
	ui.addMastheadBlock(tuiBlockMeta, "session "+string(session.ID))
	if omitted > 0 {
		ui.addBlock(tuiBlockMeta, fmt.Sprintf("%d earlier conversation messages omitted from the display", omitted))
	}
	for _, message := range visible {
		text := message.Content
		if len(text) > tuiMaxResumeBlockBytes {
			text = truncateUTF8(text, tuiMaxResumeBlockBytes-len("\n… display truncated")) + "\n… display truncated"
		}
		kind := tuiBlockUser
		if message.Role == RoleAssistant {
			kind = tuiBlockAssistant
		}
		ui.addBlock(kind, text)
	}
	ui.addBlock(tuiBlockMeta, notice+" · /help for commands · /exit to quit")
	return nil
}

func (ui *fullscreenTUI) handleRunEvent(event RunEvent) {
	now := time.Now()
	if event.SessionID != "" {
		ui.sessionID = event.SessionID
	}
	visibility := defaultDisplayVisibility()
	if ui.app != nil {
		visibility = ui.app.DisplayVisibility()
	}
	switch event.Type {
	case RunStart:
		ui.running = true
		ui.activity = "Working…"
		ui.runState.beginRun(now)
		ui.runStartedAt = now
		ui.perf = runPerformance{}
	case RunAssistantDelta:
		if event.Delta != "" {
			ui.deltaSeen = true
			ui.appendAssistant(event.Delta)
			ui.activity = "Responding…"
			ui.runState.setState(tuiActivityResponding, now, 0)
		}
	case RunReasoningDelta:
		if event.Reasoning != nil && event.Reasoning.Delta != "" {
			if visibility.Thinking {
				ui.reasoningSeen = true
				ui.appendReasoning(event.Reasoning.Delta)
			}
			ui.activity = "Thinking…"
			ui.runState.setState(tuiActivityThinking, now, 0)
		}
	case RunReasoningFinal:
		if visibility.Thinking && event.Reasoning != nil {
			if !ui.reasoningSeen && event.Reasoning.Content != "" {
				ui.appendReasoning(event.Reasoning.Content)
			}
			if event.Reasoning.Truncated {
				ui.appendReasoning("\n… provider reasoning truncated")
			}
		}
		ui.reasoningSeen = false
		ui.reasoningOpen = false
	case RunAssistant:
		if !ui.deltaSeen && event.Message != nil && event.Message.Content != "" {
			ui.appendAssistant(event.Message.Content)
		}
		ui.deltaSeen = false
		ui.assistantOpen = false
	case RunPlan:
		if event.Plan != nil {
			ui.addBlock(tuiBlockActivity, "Plan "+event.Plan.Goal)
			ui.activity = "Planning…"
			ui.runState.setState(tuiActivityThinking, now, 0)
		}
	case RunToolCall:
		if event.ToolCall != nil {
			ui.reasoningOpen = false
			ui.assistantOpen = false
			switch visibility.toolDisplayMode() {
			case ToolDisplayFull:
				display := tuiToolCallDisplay(*event.ToolCall)
				ui.addBlock(tuiBlockTool, display.Text)
				if display.Diff != "" {
					ui.addBlock(tuiBlockDiff, display.Diff)
				}
				ui.activity = "Using " + tuiCompactText(event.ToolCall.Name, 36) + "…"
			case ToolDisplayCompact:
				ui.addCompactToolCall(*event.ToolCall)
			default:
				ui.activity = "Working…"
			}
			ui.runState.beginTool(now)
		}
	case RunToolResult:
		if event.ToolResult != nil {
			switch visibility.toolDisplayMode() {
			case ToolDisplayFull:
				display := tuiToolResultDisplay(*event.ToolResult)
				kind := tuiBlockToolResult
				if event.ToolResult.Error != "" {
					kind = tuiBlockToolError
				}
				ui.addBlock(kind, display.Text)
				if display.Diff != "" {
					ui.addBlock(tuiBlockDiff, display.Diff)
				}
			case ToolDisplayCompact:
				ui.completeCompactTool(*event.ToolResult)
			}
			ui.activity = "Working…"
			if event.ToolResult.Error != "" {
				ui.runState.showError(now, 0)
			} else {
				ui.runState.setState(tuiActivityWorking, now, 0)
			}
		}
	case RunApproval:
		if event.Approval != nil {
			if event.Approval.EffectiveStatus() == ApprovalStatusPending {
				approval := *event.Approval
				approval.Request = cloneApprovalRequest(event.Approval.Request)
				ui.pendingApproval = &approval
				ui.activity = "Approval required"
				ui.runState.setState(tuiActivityApproval, now, 0)
				ui.notice = tuiCompactText(approval.Request.ToolName, 28) + " · y approve · n deny"
				detail := "Approval requested · " + approval.Request.ToolName
				if approval.Request.Summary != "" {
					detail += "\n" + approval.Request.Summary
				}
				if len(approval.Request.Args) > 0 {
					detail += "\nargs: " + string(boundedAuditArgs(approval.Request.Args))
				}
				ui.addBlock(tuiBlockActivity, detail)
				break
			}
			detail := string(event.Approval.Decision)
			if event.Approval.Request.ToolName != "" {
				detail += " · " + event.Approval.Request.ToolName
			}
			ui.addBlock(tuiBlockActivity, "Approval "+detail)
			ui.runState.setState(tuiActivityWorking, now, 0)
		}
	case RunUsage:
		if event.Usage != nil {
			ui.usageCalls++
			ui.totalTokens += event.Usage.Usage.TotalTokens
			ui.perf.add(event.Usage)
		}
	case RunContextUsage:
		if event.ContextUsage != nil {
			ui.contextUsed = event.ContextUsage.PromptUsed
			ui.contextKnown = true
			if event.ContextUsage.WindowKnown && event.ContextUsage.WindowTokens > 0 {
				ui.contextWindow = event.ContextUsage.WindowTokens
			}
		}
	case RunProviderFallback:
		if event.ProviderFallback != nil {
			ui.addBlock(tuiBlockActivity, "Main model unreachable · answering with "+event.ProviderFallback.To)
		}
	case RunProviderOutputRejected:
		if event.ProviderOutput != nil {
			ui.deltaSeen = false
			ui.assistantOpen = false
			ui.addBlock(tuiBlockActivity, "Provider "+tuiProviderFailureDetail(*event.ProviderOutput))
			ui.activity = "Recovering…"
			ui.runState.showError(now, 0)
		}
	case RunCheckpoint:
		if event.Checkpoint != nil {
			ui.addBlock(tuiBlockActivity, "Checkpoint "+string(event.Checkpoint.WhyStopped)+" · continue "+event.Checkpoint.ID)
			ui.activity = "Checkpoint saved"
		}
	case RunFailure:
		if ui.pendingSteer == "" || !strings.Contains(strings.ToLower(event.Error), context.Canceled.Error()) {
			if event.ErrorKind == ErrorKindModelUnreachable {
				ui.addBlock(tuiBlockError, RunFailureText(event))
				ui.notice = "model server unreachable · check it, then send again"
			} else {
				ui.addBlock(tuiBlockError, "Error "+event.Error)
			}
			ui.runState.failRun(now)
		} else {
			ui.runState.cancelRun(now)
		}
	case RunEnd:
		ui.runState.finishRun(now)
		ui.finishRun()
	}
	ui.dirty = true
}

func (ui *fullscreenTUI) addCompactToolCall(call ToolCall) {
	activity := tuiToolActivity{Call: call}
	action := tuiCompactToolCallAction(call)
	if action.Class == tuiToolActivityExploration && len(ui.blocks) > 0 {
		last := &ui.blocks[len(ui.blocks)-1]
		if last.kind == tuiBlockTool && len(last.toolActivities) > 0 && tuiToolActivitiesAreExploration(last.toolActivities) {
			last.toolActivities = append(last.toolActivities, activity)
			ui.refreshCompactToolBlock(last)
			ui.activity = action.Active + "…"
			return
		}
	}
	display, _ := tuiCompactToolActivitiesDisplay([]tuiToolActivity{activity})
	ui.addBlock(tuiBlockTool, display.Text)
	ui.blocks[len(ui.blocks)-1].toolActivities = []tuiToolActivity{activity}
	ui.activity = action.Active + "…"
}

func (ui *fullscreenTUI) completeCompactTool(result ToolResultEvent) {
	blockIndex, activityIndex := ui.findCompactToolActivity(result, true)
	if blockIndex < 0 {
		blockIndex, activityIndex = ui.findCompactToolActivity(result, false)
	}
	if blockIndex < 0 {
		ui.addCompactToolCall(ToolCall{ID: result.CallID, Name: result.ToolName})
		blockIndex, activityIndex = ui.findCompactToolActivity(result, false)
	}
	if blockIndex < 0 || activityIndex < 0 {
		return
	}
	copy := result
	block := &ui.blocks[blockIndex]
	block.toolActivities[activityIndex].Result = &copy
	ui.refreshCompactToolBlock(block)
}

func (ui *fullscreenTUI) findCompactToolActivity(result ToolResultEvent, exactID bool) (int, int) {
	for blockIndex := len(ui.blocks) - 1; blockIndex >= 0; blockIndex-- {
		block := &ui.blocks[blockIndex]
		for activityIndex := len(block.toolActivities) - 1; activityIndex >= 0; activityIndex-- {
			activity := block.toolActivities[activityIndex]
			if activity.Result != nil {
				continue
			}
			if exactID {
				if result.CallID != "" && activity.Call.ID == result.CallID {
					return blockIndex, activityIndex
				}
				continue
			}
			if activity.Call.Name == result.ToolName {
				return blockIndex, activityIndex
			}
		}
	}
	return -1, -1
}

func (ui *fullscreenTUI) refreshCompactToolBlock(block *tuiBlock) {
	if block == nil {
		return
	}
	display, failed := tuiCompactToolActivitiesDisplay(block.toolActivities)
	block.text = display.Text
	block.kind = tuiBlockTool
	if failed {
		block.kind = tuiBlockToolError
	}
	ui.dirty = true
}

func tuiToolActivitiesAreExploration(activities []tuiToolActivity) bool {
	if len(activities) == 0 {
		return false
	}
	for _, activity := range activities {
		if tuiCompactToolCallAction(activity.Call).Class != tuiToolActivityExploration {
			return false
		}
	}
	return true
}

func (ui *fullscreenTUI) finishRun() {
	ui.assistantOpen = false
	ui.deltaSeen = false
	ui.reasoningOpen = false
	ui.reasoningSeen = false
	ui.running = false
	ui.activity = "Message"
	if ui.pendingSteer != "" {
		ui.activity = "Steering…"
	}
	ui.refreshInbox(time.Now())
	if !ui.newSessionShown && ui.sessionID != "" {
		ui.addBlock(tuiBlockMeta, "session "+string(ui.sessionID))
		ui.newSessionShown = true
	}
	if ui.usageCalls > 0 {
		calls := "calls"
		if ui.usageCalls == 1 {
			calls = "call"
		}
		line := fmt.Sprintf("%d tokens · %d provider %s", ui.totalTokens, ui.usageCalls, calls)
		var wall time.Duration
		if !ui.runStartedAt.IsZero() {
			wall = time.Since(ui.runStartedAt)
		}
		if perf := ui.perf.summary(wall); perf != "" {
			line += " · " + perf
		}
		ui.addBlock(tuiBlockMeta, line)
	}
}

func (ui *fullscreenTUI) appendAssistant(text string) {
	text = tuiSafeText(text)
	if text == "" {
		return
	}
	ui.reasoningOpen = false
	if ui.assistantOpen && len(ui.blocks) > 0 && ui.blocks[len(ui.blocks)-1].kind == tuiBlockAssistant {
		ui.blocks[len(ui.blocks)-1].text += text
		return
	}
	ui.addBlock(tuiBlockAssistant, text)
	ui.assistantOpen = true
}

func (ui *fullscreenTUI) appendReasoning(text string) {
	text = tuiSafeText(text)
	if text == "" {
		return
	}
	ui.assistantOpen = false
	if ui.reasoningOpen && len(ui.blocks) > 0 && ui.blocks[len(ui.blocks)-1].kind == tuiBlockReasoning {
		block := &ui.blocks[len(ui.blocks)-1]
		remaining := tuiMaxReasoningDisplayBytes - len(block.text)
		if remaining <= 0 {
			return
		}
		if len(text) > remaining {
			text = truncateUTF8(text, remaining)
			text = strings.TrimRight(text, "\n") + "\n… reasoning display truncated"
		}
		block.text += text
		ui.dirty = true
		return
	}
	text = tuiBoundDisplayText(text, tuiMaxReasoningDisplayBytes, tuiMaxDetailDisplayLines)
	ui.addBlock(tuiBlockReasoning, text)
	ui.reasoningOpen = true
}

func (ui *fullscreenTUI) addBlock(kind tuiBlockKind, text string) {
	text = tuiSafeText(text)
	if text == "" {
		return
	}
	ui.blocks = append(ui.blocks, tuiBlock{kind: kind, text: text})
	if len(ui.blocks) > tuiMaxTranscriptBlocks {
		overflow := len(ui.blocks) - tuiMaxTranscriptBlocks
		copy(ui.blocks, ui.blocks[overflow:])
		ui.blocks = ui.blocks[:tuiMaxTranscriptBlocks]
	}
	ui.dirty = true
}

func (ui *fullscreenTUI) addMastheadBlock(kind tuiBlockKind, text string) {
	before := len(ui.blocks)
	ui.addBlock(kind, text)
	if len(ui.blocks) > before {
		ui.blocks[len(ui.blocks)-1].masthead = true
	}
}

func (ui *fullscreenTUI) addStatusBlock(status CommandStatusData) {
	statusCopy := status
	ui.blocks = append(ui.blocks, tuiBlock{kind: tuiBlockStatus, status: &statusCopy})
	if len(ui.blocks) > tuiMaxTranscriptBlocks {
		overflow := len(ui.blocks) - tuiMaxTranscriptBlocks
		copy(ui.blocks, ui.blocks[overflow:])
		ui.blocks = ui.blocks[:tuiMaxTranscriptBlocks]
	}
	ui.dirty = true
}

func (ui *fullscreenTUI) insertRunes(value []rune) {
	if len(value) == 0 {
		return
	}
	remaining := tuiMaxDraftRunes - len(ui.draft)
	if remaining <= 0 {
		ui.notice = "message is at the input limit"
		return
	}
	if len(value) > remaining {
		value = value[:remaining]
		ui.notice = "message was trimmed to the input limit"
	}
	ui.draft = append(ui.draft, make([]rune, len(value))...)
	copy(ui.draft[ui.cursor+len(value):], ui.draft[ui.cursor:len(ui.draft)-len(value)])
	copy(ui.draft[ui.cursor:], value)
	ui.cursor += len(value)
	ui.preferredColumn = -1
	ui.historyIndex = -1
}

func (ui *fullscreenTUI) moveWordLeft() {
	ui.cursor = tuiPreviousWordStart(ui.draft, ui.cursor)
	ui.preferredColumn = -1
}

func (ui *fullscreenTUI) moveWordRight() {
	ui.cursor = tuiNextWordStart(ui.draft, ui.cursor)
	ui.preferredColumn = -1
}

func (ui *fullscreenTUI) deletePreviousWord() {
	end := ui.cursor
	start := tuiPreviousWordStart(ui.draft, end)
	if start < end {
		ui.draft = append(ui.draft[:start], ui.draft[end:]...)
		ui.cursor = start
	}
	ui.preferredColumn = -1
}

func (ui *fullscreenTUI) deleteNextWord() {
	start := ui.cursor
	end := tuiNextWordEnd(ui.draft, start)
	if start < end {
		ui.draft = append(ui.draft[:start], ui.draft[end:]...)
	}
	ui.preferredColumn = -1
}

func tuiPreviousWordStart(value []rune, cursor int) int {
	if cursor > len(value) {
		cursor = len(value)
	}
	for cursor > 0 && unicode.IsSpace(value[cursor-1]) {
		cursor--
	}
	for cursor > 0 && !unicode.IsSpace(value[cursor-1]) {
		cursor--
	}
	return cursor
}

func tuiNextWordStart(value []rune, cursor int) int {
	if cursor < 0 {
		cursor = 0
	}
	for cursor < len(value) && !unicode.IsSpace(value[cursor]) {
		cursor++
	}
	for cursor < len(value) && unicode.IsSpace(value[cursor]) {
		cursor++
	}
	return cursor
}

func tuiNextWordEnd(value []rune, cursor int) int {
	if cursor < 0 {
		cursor = 0
	}
	for cursor < len(value) && unicode.IsSpace(value[cursor]) {
		cursor++
	}
	for cursor < len(value) && !unicode.IsSpace(value[cursor]) {
		cursor++
	}
	return cursor
}

func (ui *fullscreenTUI) moveHistory(direction int) {
	if len(ui.history) == 0 {
		return
	}
	if direction < 0 {
		if ui.historyIndex < 0 {
			ui.historyDraft = string(ui.draft)
			ui.historyIndex = len(ui.history) - 1
		} else if ui.historyIndex > 0 {
			ui.historyIndex--
		}
	} else {
		if ui.historyIndex < 0 {
			return
		}
		ui.historyIndex++
		if ui.historyIndex >= len(ui.history) {
			ui.historyIndex = -1
			ui.draft = []rune(ui.historyDraft)
			ui.cursor = len(ui.draft)
			ui.preferredColumn = -1
			return
		}
	}
	ui.draft = []rune(ui.history[ui.historyIndex])
	ui.cursor = len(ui.draft)
	ui.preferredColumn = -1
}

type tuiVisualLine struct {
	kind     tuiBlockKind
	text     string
	rendered string
}

func (ui *fullscreenTUI) visualLines(width int) []tuiVisualLine {
	return tuiVisualLines(ui.blocks, width)
}

func (ui *fullscreenTUI) visualLinesWithoutMasthead(width int) []tuiVisualLine {
	blocks := make([]tuiBlock, 0, len(ui.blocks))
	for _, block := range ui.blocks {
		if !block.masthead {
			blocks = append(blocks, block)
		}
	}
	return tuiVisualLines(blocks, width)
}

func tuiVisualLines(blocks []tuiBlock, width int) []tuiVisualLine {
	if width < 1 {
		width = 1
	}
	var lines []tuiVisualLine
	for blockIndex, block := range blocks {
		if blockIndex > 0 {
			previous := blocks[blockIndex-1]
			compactToolBoundary := len(previous.toolActivities) > 0 || len(block.toolActivities) > 0
			if compactToolBoundary || tuiBlocksNeedGap(previous.kind, block.kind) {
				lines = append(lines, tuiVisualLine{text: "", rendered: ""})
			}
		}
		switch block.kind {
		case tuiBlockReasoning, tuiBlockTool, tuiBlockToolResult, tuiBlockToolError:
			if len(block.toolActivities) > 0 {
				lines = append(lines, tuiCompactToolVisualLines(block.toolActivities, width)...)
				continue
			}
			lines = append(lines, tuiDetailVisualLines(block.kind, block.text, width)...)
			continue
		case tuiBlockDiff:
			lines = append(lines, tuiDiffVisualLines(block.text, width)...)
			continue
		case tuiBlockStatus:
			if block.status != nil {
				lines = append(lines, tuiStatusVisualLines(*block.status, width)...)
			}
			continue
		}
		if block.kind == tuiBlockAssistant {
			lines = append(lines, tuiMarkdownVisualLines(block.text, width)...)
			continue
		}
		prefix := tuiBlockPrefix(block.kind)
		continuation := strings.Repeat(" ", utf8.RuneCountInString(prefix))
		parts := strings.Split(block.text, "\n")
		for partIndex, part := range parts {
			linePrefix := continuation
			if partIndex == 0 {
				linePrefix = prefix
			}
			available := width - utf8.RuneCountInString(linePrefix)
			if available < 1 {
				available = 1
			}
			runes := []rune(part)
			if len(runes) == 0 {
				lines = append(lines, newTUIVisualLine(block.kind, linePrefix))
				continue
			}
			for len(runes) > 0 {
				count := available
				if count > len(runes) {
					count = len(runes)
				}
				lines = append(lines, newTUIVisualLine(block.kind, linePrefix+string(runes[:count])))
				runes = runes[count:]
				linePrefix = continuation
				available = width - utf8.RuneCountInString(linePrefix)
				if available < 1 {
					available = 1
				}
			}
		}
	}
	return lines
}

func tuiBlocksNeedGap(previous, current tuiBlockKind) bool {
	switch current {
	case tuiBlockUser, tuiBlockStatus:
		return true
	case tuiBlockReasoning:
		return previous != tuiBlockReasoning && previous != tuiBlockTool && previous != tuiBlockToolResult && previous != tuiBlockToolError && previous != tuiBlockDiff
	case tuiBlockAssistant:
		switch previous {
		case tuiBlockUser, tuiBlockReasoning, tuiBlockTool, tuiBlockToolResult, tuiBlockToolError, tuiBlockDiff:
			return true
		}
	case tuiBlockTool:
		return previous == tuiBlockAssistant
	}
	return false
}

func newTUIVisualLine(kind tuiBlockKind, text string) tuiVisualLine {
	return tuiVisualLine{kind: kind, text: text, rendered: tuiBlockColor(kind) + text + tuiReset}
}

func tuiBlockPrefix(kind tuiBlockKind) string {
	switch kind {
	case tuiBlockUser:
		return "› "
	case tuiBlockAssistant:
		return "• "
	case tuiBlockActivity, tuiBlockTool:
		return "• "
	case tuiBlockBranch, tuiBlockError:
		return "  "
	case tuiBlockMeta:
		return "  "
	default:
		return ""
	}
}

func tuiBlockColor(kind tuiBlockKind) string {
	switch kind {
	case tuiBlockHeader, tuiBlockUser:
		return tuiCyan
	case tuiBlockAssistant:
		return tuiGreen
	case tuiBlockActivity:
		return tuiMagenta
	case tuiBlockReasoning:
		return tuiYellow
	case tuiBlockTool:
		return tuiCyan
	case tuiBlockToolResult:
		return tuiSubtle
	case tuiBlockToolError:
		return tuiRed
	case tuiBlockDiff, tuiBlockStatus:
		return tuiCyan
	case tuiBlockError:
		return tuiRed
	case tuiBlockBranch, tuiBlockMeta:
		return tuiSubtle
	default:
		return ""
	}
}

func (ui *fullscreenTUI) render() error {
	width, height, err := ui.terminal.size()
	if err != nil {
		return err
	}
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	if width < 4 {
		width = 4
	}
	if height < 4 {
		height = 4
	}
	forceRedraw := width != ui.width || height != ui.height || len(ui.renderedRows) != height
	ui.width = width
	ui.height = height
	renderWidth := tuiRenderWidth(width)
	if ui.resumePicker != nil {
		ui.renderedRows = nil
		return ui.renderResumePicker(renderWidth, height)
	}
	maxComposerLines := height - 3 - tuiComposerTopPadding
	if maxComposerLines > tuiMaxComposerLines {
		maxComposerLines = tuiMaxComposerLines
	}
	composer := ui.composerLayout(renderWidth, maxComposerLines)
	composerTopPadding := tuiComposerTopPadding
	composerHeight := len(composer.lines) + 2 + composerTopPadding
	if composerHeight >= height {
		composerTopPadding = 0
		composerHeight = len(composer.lines) + 2
	}
	paneHeight := height - composerHeight
	transcriptTop := 0
	transcriptHeight := paneHeight
	var masthead []string
	var visual []tuiVisualLine
	if tuiMastheadVisible(renderWidth, paneHeight) {
		session := string(ui.sessionID)
		if session == "" {
			session = "new session"
		}
		masthead = ui.runState.renderedMasthead(renderWidth, tuiMastheadInfo{
			model:   ui.model,
			profile: ui.profile,
			session: session,
			scope:   ui.scope,
		})
	}
	if len(masthead) > 0 {
		transcriptTop = len(masthead)
		transcriptHeight -= transcriptTop
		visual = ui.visualLinesWithoutMasthead(renderWidth)
	} else {
		visual = ui.visualLines(renderWidth)
	}
	ui.transcriptRows = transcriptHeight
	if ui.scrollOffset > 0 && len(visual) > ui.lastVisualRows {
		ui.scrollOffset += len(visual) - ui.lastVisualRows
	}
	ui.lastVisualRows = len(visual)
	maxOffset := len(visual) - transcriptHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	if ui.scrollOffset > maxOffset {
		ui.scrollOffset = maxOffset
	}
	end := len(visual) - ui.scrollOffset
	if end < 0 {
		end = 0
	}
	start := end - transcriptHeight
	if start < 0 {
		start = 0
	}

	rows := make([]string, height)
	copy(rows, masthead)
	for row := 0; row < transcriptHeight; row++ {
		lineIndex := start + row
		if lineIndex < end {
			rows[transcriptTop+row] = visual[lineIndex].rendered
		}
	}

	title := formatTUIContextUsage(ui.contextUsed, ui.contextWindow, ui.contextKnown)
	if badge := tuiInboxBadge(ui.inboxUnread, ui.inboxTruncated); badge != "" {
		title += " · " + badge
	}
	if ui.networkOff {
		title += " · network off"
	}
	if ui.notice != "" {
		title += " · " + ui.notice
	}
	if ui.scrollOffset > 0 {
		title += fmt.Sprintf(" · ↑ %d", ui.scrollOffset)
	}
	composerStart := paneHeight + composerTopPadding
	top := tuiBoxBorder('╭', '╮', " "+title+" ", renderWidth)
	rows[composerStart] = tuiCyan + top + tuiReset
	for index, line := range composer.lines {
		rows[composerStart+1+index] = tuiCyan + "│" + tuiReset + line + tuiCyan + "│" + tuiReset
	}
	help := " Enter send · Ctrl+J newline · PgUp/PgDn scroll · Ctrl+D exit "
	if ui.running {
		help = " /steer <correction> + Enter · Ctrl-C cancel · PgUp/PgDn scroll "
	}
	bottom := tuiBoxBorder('╰', '╯', help, renderWidth)
	rows[composerStart+len(composer.lines)+1] = tuiCyan + bottom + tuiReset
	if err := ui.renderChangedRows(rows, composerStart+2+composer.cursorRow, composer.cursorColumn, forceRedraw); err != nil {
		return err
	}
	ui.dirty = false
	return nil
}

func (ui *fullscreenTUI) renderChangedRows(rows []string, cursorRow, cursorColumn int, force bool) error {
	var output strings.Builder
	output.Grow(ui.width + 128)
	rowsChanged := force
	if !rowsChanged {
		for row, content := range rows {
			if row >= len(ui.renderedRows) || ui.renderedRows[row] != content {
				rowsChanged = true
				break
			}
		}
	}
	screenChanged := rowsChanged
	if screenChanged {
		output.WriteString("\x1b[?2026h")
		if force {
			output.WriteString("\x1b[?25l")
		}
	}
	for row, content := range rows {
		if !force && row < len(ui.renderedRows) && ui.renderedRows[row] == content {
			continue
		}
		// Replace the row before erasing any stale suffix. Clearing first makes
		// high-frequency streaming updates visibly flash through a blank row.
		fmt.Fprintf(&output, "\x1b[%d;%dH%s\x1b[K", row+1, tuiContentColumn, content)
	}
	if screenChanged {
		fmt.Fprintf(&output, "\x1b[%d;%dH", cursorRow, cursorColumn+tuiEdgePadding)
		if force {
			output.WriteString("\x1b[?25h")
		}
		output.WriteString("\x1b[?2026l")
	} else {
		fmt.Fprintf(&output, "\x1b[%d;%dH", cursorRow, cursorColumn+tuiEdgePadding)
	}
	if err := ui.terminal.write(output.String()); err != nil {
		return err
	}
	ui.renderedRows = append(ui.renderedRows[:0], rows...)
	return nil
}

func (ui *fullscreenTUI) renderResumePicker(width, height int) error {
	picker := ui.resumePicker
	if picker == nil {
		return nil
	}
	if picker.selected < 0 {
		picker.selected = 0
	}
	if picker.selected >= len(picker.sessions) && len(picker.sessions) > 0 {
		picker.selected = len(picker.sessions) - 1
	}

	availableRows := height - 3
	if availableRows < 1 {
		availableRows = 1
	}
	rowsPerSession := 2
	visibleCount := availableRows / rowsPerSession
	if visibleCount < 1 {
		visibleCount = 1
		rowsPerSession = 1
	}
	if visibleCount > len(picker.sessions) {
		visibleCount = len(picker.sessions)
	}
	start := picker.selected - visibleCount/2
	if start < 0 {
		start = 0
	}
	if endLimit := len(picker.sessions) - visibleCount; start > endLimit {
		start = endLimit
	}
	if start < 0 {
		start = 0
	}
	end := start + visibleCount

	var output strings.Builder
	output.Grow(width*height + 256)
	output.WriteString("\x1b[?25l\x1b[H")
	for row := 1; row <= height; row++ {
		fmt.Fprintf(&output, "\x1b[%d;1H\x1b[2K", row)
	}
	fmt.Fprintf(&output, "\x1b[1;%dH%s%s%s", tuiContentColumn, tuiCyan, tuiBoxBorder('╭', '╮', "─ Resume a session ", width), tuiReset)
	subtitle := fmt.Sprintf(" %d recent durable session(s)", len(picker.sessions))
	if start > 0 || end < len(picker.sessions) {
		subtitle += fmt.Sprintf(" · showing %d–%d", start+1, end)
	}
	fmt.Fprintf(&output, "\x1b[2;%dH%s│%s%s%s│%s", tuiContentColumn, tuiCyan, tuiReset, tuiPaddedLine(subtitle, width-2), tuiCyan, tuiReset)

	row := 3
	for index := start; index < end && row < height; index++ {
		session := picker.sessions[index]
		marker := "  "
		if index == picker.selected {
			marker = "› "
		}
		pin := ""
		if session.Pinned {
			pin = "★ "
		}
		updated := "unknown time"
		if !session.UpdatedAt.IsZero() {
			updated = session.UpdatedAt.Local().Format("Jan 02 15:04")
		}
		title := session.Title
		if title == "" {
			title = "Untitled session"
		}
		current := ""
		if session.Current {
			current = " · current"
		}
		primary := fmt.Sprintf(" %s%s%-8s  %s  %s%s", marker, pin, shortSessionID(session.ID), updated, title, current)
		style := tuiSubtle
		if index == picker.selected {
			style = tuiBold + tuiGreen
		}
		fmt.Fprintf(&output, "\x1b[%d;%dH%s│%s%s%s%s│%s", row, tuiContentColumn, tuiCyan, tuiReset, style, tuiPaddedLine(primary, width-2), tuiCyan, tuiReset)
		row++
		if rowsPerSession == 2 && row < height {
			preview := session.Preview
			if preview == "" {
				preview = "<no message preview>"
			}
			secondary := "             " + preview
			secondaryStyle := tuiSubtle
			if index == picker.selected {
				secondaryStyle = tuiGreen
			}
			fmt.Fprintf(&output, "\x1b[%d;%dH%s│%s%s%s%s│%s", row, tuiContentColumn, tuiCyan, tuiReset, secondaryStyle, tuiPaddedLine(secondary, width-2), tuiCyan, tuiReset)
			row++
		}
	}
	for row < height {
		fmt.Fprintf(&output, "\x1b[%d;%dH%s│%s%s%s│%s", row, tuiContentColumn, tuiCyan, tuiReset, tuiPaddedLine("", width-2), tuiCyan, tuiReset)
		row++
	}
	help := " ↑/↓ select · Enter resume · Esc/q cancel "
	fmt.Fprintf(&output, "\x1b[%d;%dH%s%s%s", height, tuiContentColumn, tuiCyan, tuiBoxBorder('╰', '╯', help, width), tuiReset)
	if err := ui.terminal.write(output.String()); err != nil {
		return err
	}
	ui.dirty = false
	return nil
}

func tuiPaddedLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = clipRunes(value, width)
	return value + strings.Repeat(" ", width-tuiDisplayWidth(value))
}

func shortSessionID(id SessionID) string {
	value := []rune(tuiSafeText(string(id)))
	if len(value) <= 8 {
		return string(value)
	}
	return string(value[:8])
}

type tuiComposerLayout struct {
	lines        []string
	cursorRow    int
	cursorColumn int
	totalLines   int
	firstLine    int
}

type tuiComposerPoint struct {
	row    int
	column int
}

type tuiComposerGeometry struct {
	rows   [][]rune
	points []tuiComposerPoint
}

func tuiComposerDimensions(width int) (int, string, int, int) {
	innerWidth := width - 2
	if innerWidth < 1 {
		innerWidth = 1
	}
	prefix := " › "
	prefixWidth := tuiDisplayWidth(prefix)
	available := innerWidth - prefixWidth - 1
	if available < 1 {
		prefix = ""
		prefixWidth = 0
		available = innerWidth
	}
	return innerWidth, prefix, prefixWidth, available
}

func (ui *fullscreenTUI) composerGeometry(width int) tuiComposerGeometry {
	_, _, _, available := tuiComposerDimensions(width)
	rows := make([][]rune, 0, len(ui.draft)/available+1)
	points := make([]tuiComposerPoint, len(ui.draft)+1)
	current := make([]rune, 0, available)
	currentWidth := 0
	for index, value := range ui.draft {
		if value != '\n' {
			valueWidth := tuiRuneWidth(value)
			if len(current) > 0 && currentWidth+valueWidth > available {
				rows = append(rows, current)
				current = make([]rune, 0, available)
				currentWidth = 0
			}
		}
		points[index] = tuiComposerPoint{row: len(rows), column: currentWidth}
		if value == '\n' {
			rows = append(rows, current)
			current = make([]rune, 0, available)
			currentWidth = 0
			continue
		}
		current = append(current, value)
		currentWidth += tuiRuneWidth(value)
	}
	points[len(ui.draft)] = tuiComposerPoint{row: len(rows), column: currentWidth}
	rows = append(rows, current)
	return tuiComposerGeometry{rows: rows, points: points}
}

func (ui *fullscreenTUI) composerRenderWidth() int {
	width := ui.width
	if width <= 0 && ui.terminal != nil {
		terminalWidth, _, err := ui.terminal.size()
		if err == nil {
			width = terminalWidth
		}
	}
	if width <= 0 {
		width = 80
	}
	if width < 4 {
		width = 4
	}
	return tuiRenderWidth(width)
}

func (ui *fullscreenTUI) moveCursorVertical(direction int) bool {
	if direction == 0 {
		return false
	}
	geometry := ui.composerGeometry(ui.composerRenderWidth())
	cursor := ui.cursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(ui.draft) {
		cursor = len(ui.draft)
	}
	current := geometry.points[cursor]
	targetRow := current.row + direction
	if targetRow < 0 || targetRow >= len(geometry.rows) {
		return false
	}
	targetColumn := ui.preferredColumn
	if targetColumn < 0 {
		targetColumn = current.column
		ui.preferredColumn = targetColumn
	}
	bestIndex := -1
	bestDistance := int(^uint(0) >> 1)
	for index, point := range geometry.points {
		if point.row != targetRow {
			continue
		}
		distance := point.column - targetColumn
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestIndex = index
			bestDistance = distance
		}
	}
	if bestIndex < 0 {
		return false
	}
	ui.cursor = bestIndex
	return true
}

func (ui *fullscreenTUI) composerLayout(width, maxLines int) tuiComposerLayout {
	innerWidth, prefix, prefixWidth, _ := tuiComposerDimensions(width)
	if maxLines < 1 {
		maxLines = 1
	}
	geometry := ui.composerGeometry(width)
	cursor := ui.cursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(ui.draft) {
		cursor = len(ui.draft)
	}
	point := geometry.points[cursor]
	cursorRow := point.row
	cursorInRow := point.column
	rows := geometry.rows

	firstLine := 0
	if cursorRow >= maxLines {
		firstLine = cursorRow - maxLines + 1
	}
	lastLine := firstLine + maxLines
	if lastLine > len(rows) {
		lastLine = len(rows)
	}
	visibleRows := rows[firstLine:lastLine]
	lines := make([]string, 0, len(visibleRows))
	for index, row := range visibleRows {
		linePrefix := strings.Repeat(" ", prefixWidth)
		absoluteRow := firstLine + index
		if absoluteRow == 0 {
			linePrefix = prefix
		} else if index == 0 && firstLine > 0 && prefixWidth > 0 {
			linePrefix = " ↑ "
		} else if index == len(visibleRows)-1 && lastLine < len(rows) && prefixWidth > 0 {
			linePrefix = " ↓ "
		}
		lines = append(lines, tuiPaddedLine(linePrefix+string(row), innerWidth))
	}
	cursorColumn := 2 + prefixWidth + cursorInRow
	maxColumn := width - 1
	if cursorColumn > maxColumn {
		cursorColumn = maxColumn
	}
	return tuiComposerLayout{
		lines:        lines,
		cursorRow:    cursorRow - firstLine,
		cursorColumn: cursorColumn,
		totalLines:   len(rows),
		firstLine:    firstLine,
	}
}

func tuiBoxBorder(left, right rune, label string, width int) string {
	if width <= 1 {
		return string(left)
	}
	inside := width - 2
	label = clipRunes(label, inside)
	fill := inside - tuiDisplayWidth(label)
	if fill < 0 {
		fill = 0
	}
	return string(left) + label + strings.Repeat("─", fill) + string(right)
}

func clipRunes(value string, width int) string {
	if width <= 0 {
		return ""
	}
	used := 0
	var clipped strings.Builder
	for _, current := range value {
		currentWidth := tuiRuneWidth(current)
		if used+currentWidth > width {
			break
		}
		clipped.WriteRune(current)
		used += currentWidth
	}
	return clipped.String()
}

// tuiRenderWidth reserves balanced edge padding. Leaving the final cell
// untouched also avoids arming auto-wrap in terminals and multiplexers when
// the next byte is a control sequence.
func tuiRenderWidth(terminalWidth int) int {
	padding := tuiEdgePadding * 2
	if terminalWidth > 4 {
		return terminalWidth - padding
	}
	return terminalWidth
}

func tuiDisplayWidth(value string) int {
	width := 0
	for _, current := range value {
		width += tuiRuneWidth(current)
	}
	return width
}

func tuiRuneWidth(value rune) int {
	if value == 0 || value < 0x20 || value >= 0x7f && value < 0xa0 {
		return 0
	}
	if unicode.Is(unicode.Mn, value) || unicode.Is(unicode.Me, value) || unicode.Is(unicode.Cf, value) {
		return 0
	}
	if tuiWideRune(value) {
		return 2
	}
	return 1
}

func tuiWideRune(value rune) bool {
	switch {
	case value >= 0x1100 && value <= 0x115f,
		value == 0x2329 || value == 0x232a,
		value >= 0x2e80 && value <= 0xa4cf && value != 0x303f,
		value >= 0xac00 && value <= 0xd7a3,
		value >= 0xf900 && value <= 0xfaff,
		value >= 0xfe10 && value <= 0xfe19,
		value >= 0xfe30 && value <= 0xfe6f,
		value >= 0xff00 && value <= 0xff60,
		value >= 0xffe0 && value <= 0xffe6,
		value >= 0x1f300 && value <= 0x1faff,
		value >= 0x20000 && value <= 0x3fffd:
		return true
	default:
		return false
	}
}

type tuiKeyKind uint8

const (
	tuiKeyRune tuiKeyKind = iota
	tuiKeyPaste
	tuiKeyEnter
	tuiKeyNewline
	tuiKeyBackspace
	tuiKeyDelete
	tuiKeyDeleteWord
	tuiKeyDeleteNextWord
	tuiKeyLeft
	tuiKeyRight
	tuiKeyWordLeft
	tuiKeyWordRight
	tuiKeyHome
	tuiKeyEnd
	tuiKeyUp
	tuiKeyDown
	tuiKeyPageUp
	tuiKeyPageDown
	tuiKeyScrollUp
	tuiKeyScrollDown
	tuiKeyClear
	tuiKeyEscape
	tuiKeyInterrupt
	tuiKeyEOFOrDelete
	tuiKeyEOF
)

type tuiKey struct {
	kind tuiKeyKind
	rune rune
	text string
	err  error
}

func readTUIKeys(terminal tuiTerminalDevice, output chan<- tuiKey, done <-chan struct{}) {
	parser := &tuiKeyParser{}
	if source, ok := terminal.(interface{ eraseByte() byte }); ok {
		value := source.eraseByte()
		if value == 0x08 || value == 0x7f {
			parser.eraseByte = value
		}
	}
	for {
		data, err := terminal.read(50 * time.Millisecond)
		if err != nil {
			select {
			case output <- tuiKey{kind: tuiKeyEOF, err: err}:
			case <-done:
			}
			return
		}
		for _, key := range parser.feed(data) {
			select {
			case output <- key:
			case <-done:
				return
			}
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

type tuiKeyParser struct {
	buffer    []byte
	paste     bool
	eraseByte byte
}

func (parser *tuiKeyParser) feed(data []byte) []tuiKey {
	parser.buffer = append(parser.buffer, data...)
	if len(data) == 0 && len(parser.buffer) == 1 && parser.buffer[0] == 0x1b {
		parser.buffer = nil
		return []tuiKey{{kind: tuiKeyEscape}}
	}
	var keys []tuiKey
	for len(parser.buffer) > 0 {
		if parser.paste {
			const endPaste = "\x1b[201~"
			index := bytes.Index(parser.buffer, []byte(endPaste))
			if index < 0 {
				if len(parser.buffer) > len(endPaste) {
					keep := len(endPaste) - 1
					keys = append(keys, tuiKey{kind: tuiKeyPaste, text: string(parser.buffer[:len(parser.buffer)-keep])})
					parser.buffer = parser.buffer[len(parser.buffer)-keep:]
				}
				break
			}
			if index > 0 {
				keys = append(keys, tuiKey{kind: tuiKeyPaste, text: string(parser.buffer[:index])})
			}
			parser.buffer = parser.buffer[index+len(endPaste):]
			parser.paste = false
			continue
		}

		current := parser.buffer[0]
		switch current {
		case '\r':
			keys = append(keys, tuiKey{kind: tuiKeyEnter})
			parser.buffer = parser.buffer[1:]
		case '\n':
			keys = append(keys, tuiKey{kind: tuiKeyNewline})
			parser.buffer = parser.buffer[1:]
		case 0x7f, 0x08:
			kind := tuiKeyBackspace
			if parser.eraseByte != 0 && current != parser.eraseByte {
				kind = tuiKeyDeleteWord
			}
			keys = append(keys, tuiKey{kind: kind})
			parser.buffer = parser.buffer[1:]
		case 0x03:
			keys = append(keys, tuiKey{kind: tuiKeyInterrupt})
			parser.buffer = parser.buffer[1:]
		case 0x04:
			keys = append(keys, tuiKey{kind: tuiKeyEOFOrDelete})
			parser.buffer = parser.buffer[1:]
		case 0x0c:
			parser.buffer = parser.buffer[1:]
		case 0x15:
			keys = append(keys, tuiKey{kind: tuiKeyClear})
			parser.buffer = parser.buffer[1:]
		case 0x17:
			keys = append(keys, tuiKey{kind: tuiKeyDeleteWord})
			parser.buffer = parser.buffer[1:]
		case 0x01:
			keys = append(keys, tuiKey{kind: tuiKeyHome})
			parser.buffer = parser.buffer[1:]
		case 0x05:
			keys = append(keys, tuiKey{kind: tuiKeyEnd})
			parser.buffer = parser.buffer[1:]
		case 0x1b:
			consumed, key, complete := parseTUIEscape(parser.buffer)
			if !complete {
				return keys
			}
			parser.buffer = parser.buffer[consumed:]
			if key.kind == tuiKeyPaste && key.text == "begin" {
				parser.paste = true
				continue
			}
			if key.kind != tuiKeyRune || key.rune != 0 {
				keys = append(keys, key)
			}
		default:
			if current < 0x20 {
				parser.buffer = parser.buffer[1:]
				continue
			}
			if !utf8.FullRune(parser.buffer) {
				return keys
			}
			r, size := utf8.DecodeRune(parser.buffer)
			if r != utf8.RuneError || size > 1 {
				keys = append(keys, tuiKey{kind: tuiKeyRune, rune: r})
			}
			parser.buffer = parser.buffer[size:]
		}
	}
	return keys
}

func parseTUIEscape(data []byte) (int, tuiKey, bool) {
	if len(data) < 2 {
		return 0, tuiKey{}, false
	}
	if data[1] != '[' {
		if data[1] == 'O' {
			if len(data) < 3 {
				return 0, tuiKey{}, false
			}
			switch data[2] {
			case 'A':
				return 3, tuiKey{kind: tuiKeyUp}, true
			case 'B':
				return 3, tuiKey{kind: tuiKeyDown}, true
			case 'C':
				return 3, tuiKey{kind: tuiKeyRight}, true
			case 'D':
				return 3, tuiKey{kind: tuiKeyLeft}, true
			case 'H':
				return 3, tuiKey{kind: tuiKeyHome}, true
			case 'F':
				return 3, tuiKey{kind: tuiKeyEnd}, true
			default:
				return 3, tuiKey{}, true
			}
		}
		switch data[1] {
		case 'b', 'B':
			return 2, tuiKey{kind: tuiKeyWordLeft}, true
		case 'f', 'F':
			return 2, tuiKey{kind: tuiKeyWordRight}, true
		case 'd', 'D':
			return 2, tuiKey{kind: tuiKeyDeleteNextWord}, true
		case 0x7f, 0x08:
			return 2, tuiKey{kind: tuiKeyDeleteWord}, true
		default:
			return 2, tuiKey{}, true
		}
	}
	end := -1
	for index := 2; index < len(data); index++ {
		if data[index] >= 0x40 && data[index] <= 0x7e {
			end = index
			break
		}
	}
	if end < 0 {
		return 0, tuiKey{}, false
	}
	sequence := string(data[2 : end+1])
	consumed := end + 1
	switch sequence {
	case "A":
		return consumed, tuiKey{kind: tuiKeyUp}, true
	case "B":
		return consumed, tuiKey{kind: tuiKeyDown}, true
	case "C":
		return consumed, tuiKey{kind: tuiKeyRight}, true
	case "D":
		return consumed, tuiKey{kind: tuiKeyLeft}, true
	case "H", "1~", "7~":
		return consumed, tuiKey{kind: tuiKeyHome}, true
	case "F", "4~", "8~":
		return consumed, tuiKey{kind: tuiKeyEnd}, true
	case "3~":
		return consumed, tuiKey{kind: tuiKeyDelete}, true
	case "5~":
		return consumed, tuiKey{kind: tuiKeyPageUp}, true
	case "6~":
		return consumed, tuiKey{kind: tuiKeyPageDown}, true
	case "200~":
		return consumed, tuiKey{kind: tuiKeyPaste, text: "begin"}, true
	}
	if key, ok := tuiModifiedCSIKey(sequence); ok {
		return consumed, key, true
	}
	if strings.HasPrefix(sequence, "<64;") && strings.HasSuffix(sequence, "M") {
		return consumed, tuiKey{kind: tuiKeyScrollUp}, true
	}
	if strings.HasPrefix(sequence, "<65;") && strings.HasSuffix(sequence, "M") {
		return consumed, tuiKey{kind: tuiKeyScrollDown}, true
	}
	return consumed, tuiKey{}, true
}

func tuiModifiedCSIKey(sequence string) (tuiKey, bool) {
	if sequence == "" {
		return tuiKey{}, false
	}
	final := sequence[len(sequence)-1]
	params := strings.Split(sequence[:len(sequence)-1], ";")
	parseParam := func(index int) (int, bool) {
		if index < 0 || index >= len(params) || params[index] == "" {
			return 0, false
		}
		value := params[index]
		if colon := strings.IndexByte(value, ':'); colon >= 0 {
			value = value[:colon]
		}
		parsed, err := strconv.Atoi(value)
		return parsed, err == nil
	}
	modifier := 1
	if len(params) > 1 {
		if parsed, ok := parseParam(1); ok {
			modifier = parsed
		}
	} else if (final == 'C' || final == 'D') && len(params) == 1 {
		if parsed, ok := parseParam(0); ok && parsed >= 2 && parsed <= 16 {
			modifier = parsed
		}
	}
	wordModifier := modifier > 1 && (modifier-1)&(2|4|8) != 0
	switch final {
	case 'A':
		return tuiKey{kind: tuiKeyUp}, true
	case 'B':
		return tuiKey{kind: tuiKeyDown}, true
	case 'C':
		if wordModifier {
			return tuiKey{kind: tuiKeyWordRight}, true
		}
		return tuiKey{kind: tuiKeyRight}, true
	case 'D':
		if wordModifier {
			return tuiKey{kind: tuiKeyWordLeft}, true
		}
		return tuiKey{kind: tuiKeyLeft}, true
	case '~':
		code, ok := parseParam(0)
		if !ok {
			return tuiKey{}, false
		}
		if code == 3 {
			if wordModifier {
				return tuiKey{kind: tuiKeyDeleteNextWord}, true
			}
			return tuiKey{kind: tuiKeyDelete}, true
		}
		if code == 27 && len(params) >= 3 {
			keyCode, ok := parseParam(2)
			if ok && (keyCode == 8 || keyCode == 127) && wordModifier {
				return tuiKey{kind: tuiKeyDeleteWord}, true
			}
		}
	case 'u':
		code, ok := parseParam(0)
		if !ok {
			return tuiKey{}, false
		}
		switch code {
		case 8, 127:
			if wordModifier {
				return tuiKey{kind: tuiKeyDeleteWord}, true
			}
			return tuiKey{kind: tuiKeyBackspace}, true
		case 57349:
			if wordModifier {
				return tuiKey{kind: tuiKeyDeleteNextWord}, true
			}
			return tuiKey{kind: tuiKeyDelete}, true
		case 57350:
			if wordModifier {
				return tuiKey{kind: tuiKeyWordLeft}, true
			}
			return tuiKey{kind: tuiKeyLeft}, true
		case 57351:
			if wordModifier {
				return tuiKey{kind: tuiKeyWordRight}, true
			}
			return tuiKey{kind: tuiKeyRight}, true
		case 57352:
			return tuiKey{kind: tuiKeyUp}, true
		case 57353:
			return tuiKey{kind: tuiKeyDown}, true
		}
	}
	return tuiKey{}, false
}

// tuiSessionGrantHint offers the "approve for this session" key when the
// request names a directory it would make writable.
func tuiSessionGrantHint(request ApprovalRequest) string {
	if request.SessionGrant == "" {
		return ""
	}
	return " · s allow " + tuiCompactText(request.SessionGrant, 40) + " this session"
}
