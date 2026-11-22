package bee

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
)

const (
	CommandStatus    CommandName = "status"
	CommandHelp      CommandName = "help"
	CommandSubagents CommandName = "subagents"
	CommandTasks     CommandName = "tasks"
	CommandSpeak     CommandName = "speak"
	CommandSay       CommandName = "say"
	CommandVideo     CommandName = "video"
	CommandImage     CommandName = "image"
	CommandStop      CommandName = "stop"
	CommandKill      CommandName = "kill"
	CommandCancel    CommandName = "cancel"
	CommandCompact   CommandName = "compact"
	CommandPlan      CommandName = "plan"
	CommandTools     CommandName = "tools"
	CommandThinking  CommandName = "thinking"
	CommandToolLimit CommandName = "tool-limit"
	CommandResume    CommandName = "resume"
	CommandContinue  CommandName = "continue"
	CommandNew       CommandName = "new"
	CommandSteer     CommandName = "steer"
	CommandInbox     CommandName = "inbox"
	CommandNetwork   CommandName = "network"
)

// CommandName is the provider-neutral name of a local Bee command.
type CommandName string

// CommandContext carries safe runtime metadata to local commands. It is
// intentionally separate from provider requests so local commands can be
// handled without constructing or calling a provider.
type CommandContext struct {
	Application  *Application
	SessionID    SessionID
	Model        string
	GatewayReady *bool
	MediaEnabled bool
	RunActive    bool
	// Title names a session that /new creates. Channels pass their session
	// title; the TUI leaves it empty.
	Title string
	// TitleProvisional lets the automatic title replace Title.
	TitleProvisional bool
}

// CommandStatusData is the bounded, non-secret status projection exposed by
// /status. Empty optional fields mean that the corresponding runtime data was
// not available.
type CommandStatusData struct {
	SessionID           SessionID               `json:"session_id,omitempty"`
	SessionTitle        string                  `json:"session_title,omitempty"`
	Provider            string                  `json:"provider,omitempty"`
	Profile             string                  `json:"profile,omitempty"`
	Model               string                  `json:"model,omitempty"`
	ReasoningEffort     string                  `json:"reasoning_effort,omitempty"`
	Directory           string                  `json:"directory,omitempty"`
	Scope               string                  `json:"scope,omitempty"`
	ContextWindowTokens int                     `json:"context_window_tokens,omitempty"`
	ToolCount           int                     `json:"tool_count,omitempty"`
	PlanModeEnabled     bool                    `json:"plan_mode_enabled"`
	NetworkOff          bool                    `json:"network_off,omitempty"`
	PlanModeSource      string                  `json:"plan_mode_source,omitempty"`
	PlanModeAvailable   bool                    `json:"plan_mode_available"`
	Display             *DisplayVisibility      `json:"display,omitempty"`
	ToolCallLimit       *ToolCallLimitState     `json:"tool_call_limit,omitempty"`
	ApplicationReady    bool                    `json:"application_ready"`
	GatewayReady        *bool                   `json:"gateway_ready,omitempty"`
	CodingRouteReady    bool                    `json:"coding_route_ready"`
	RecentUsage         *Usage                  `json:"recent_usage,omitempty"`
	RecentProviderCalls int                     `json:"recent_provider_calls,omitempty"`
	LastProviderCall    *ProviderCallTiming     `json:"last_provider_call,omitempty"`
	RequestContext      *RequestContextUsage    `json:"request_context,omitempty"`
	ContextDiagnostics  *ContextDiagnostics     `json:"context_diagnostics,omitempty"`
	MemoryExtraction    *MemoryExtractionStatus `json:"memory_extraction,omitempty"`
}

// MemoryExtractionStatus is a content-free summary of the most recent
// post-turn extraction attempt for a durable session.
type MemoryExtractionStatus struct {
	Attempted bool   `json:"attempted"`
	Saved     int    `json:"saved"`
	Unchanged int    `json:"unchanged"`
	Proposed  int    `json:"proposed"`
	Rejected  int    `json:"rejected"`
	Error     string `json:"error,omitempty"`
}

// CommandSubagentsData is the bounded, non-secret projection exposed by
// /subagents. It describes configured child capacity and coding routes; it
// does not expose credentials or start any child process.
type CommandSubagentsData struct {
	ChildProfile       string   `json:"child_profile,omitempty"`
	ChildModel         string   `json:"child_model,omitempty"`
	MaxChildren        int      `json:"max_children,omitempty"`
	CodingRouteReady   bool     `json:"coding_route_ready"`
	CodingBackends     []string `json:"coding_backends,omitempty"`
	CodingBackendsLive bool     `json:"coding_backends_live"`
}

// CommandTaskData is a safe task/status row for /tasks. The Source field keeps
// the session-plan origin explicit for future parent-owned task projections.
type CommandTaskData struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Source    string    `json:"source"`
	SessionID SessionID `json:"session_id,omitempty"`
}

type CommandTasksData struct {
	SessionID    SessionID         `json:"session_id,omitempty"`
	SessionTitle string            `json:"session_title,omitempty"`
	Tasks        []CommandTaskData `json:"tasks,omitempty"`
}

// CommandResumeSession is the bounded session projection used by /resume.
// Preview contains only a short excerpt from a durable user or assistant
// message; callers that select a row resume by the full ID.
type CommandResumeSession struct {
	ID         SessionID `json:"id"`
	Title      string    `json:"title,omitempty"`
	Preview    string    `json:"preview,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	EventCount int       `json:"event_count,omitempty"`
	Pinned     bool      `json:"pinned,omitempty"`
	Current    bool      `json:"current,omitempty"`
}

// CommandResumeData either lists selectable sessions or identifies the
// session selected by /resume <session-id>.
type CommandResumeData struct {
	Sessions []CommandResumeSession `json:"sessions,omitempty"`
	Selected *CommandResumeSession  `json:"selected,omitempty"`
}

// CommandNewSessionData identifies the fresh durable session created by /new.
type CommandNewSessionData struct {
	Session CommandResumeSession `json:"session"`
}

// CommandSteerData carries the user's correction to an interactive adapter.
// The adapter owns interrupting the current run and submitting Prompt as the
// next turn in the same durable session.
type CommandSteerData struct {
	Prompt string `json:"prompt"`
}

// CommandContinueData tells an interactive adapter to start a normal provider
// turn while explicitly selecting one durable continuation checkpoint.
type CommandContinueData struct {
	CheckpointID string `json:"checkpoint_id"`
	Prompt       string `json:"prompt"`
}

// LocalCommandEvent is emitted through the normal Run/SSE event stream for a
// handled local command. Text is suitable for a notice or assistant-style
// display; Status retains structured /status data for consumers that need it.
type LocalCommandEvent struct {
	Name          CommandName            `json:"name"`
	Kind          string                 `json:"kind"`
	Text          string                 `json:"text,omitempty"`
	Status        *CommandStatusData     `json:"status,omitempty"`
	Subagents     *CommandSubagentsData  `json:"subagents,omitempty"`
	Tasks         *CommandTasksData      `json:"tasks,omitempty"`
	Media         *MediaJobEvent         `json:"media,omitempty"`
	CancelledRuns int                    `json:"cancelled_runs,omitempty"`
	PlanMode      *PlanModeState         `json:"plan_mode,omitempty"`
	Display       *DisplayVisibility     `json:"display,omitempty"`
	ToolCallLimit *ToolCallLimitState    `json:"tool_call_limit,omitempty"`
	Resume        *CommandResumeData     `json:"resume,omitempty"`
	NewSession    *CommandNewSessionData `json:"new_session,omitempty"`
	Steer         *CommandSteerData      `json:"steer,omitempty"`
	Continue      *CommandContinueData   `json:"continue,omitempty"`
	Inbox         *CommandInboxData      `json:"inbox,omitempty"`
}

// CommandResult is the result of parsing and, when recognized, handling a
// local slash command. Handled is true for both supported and unknown slash
// commands; ordinary prompts return Handled=false.
type CommandResult struct {
	Handled       bool
	Name          CommandName
	Kind          string
	Text          string
	Voice         *VoiceCommand
	Status        *CommandStatusData
	Subagents     *CommandSubagentsData
	Tasks         *CommandTasksData
	Media         *MediaCommand
	CancelledRuns int
	PlanMode      *PlanModeState
	Display       *DisplayVisibility
	ToolCallLimit *ToolCallLimitState
	Resume        *CommandResumeData
	NewSession    *CommandNewSessionData
	Steer         *CommandSteerData
	Continue      *CommandContinueData
	Inbox         *CommandInboxData
}

const (
	commandKindStatus    = "status"
	commandKindHelp      = "help"
	commandKindSubagents = "subagents"
	commandKindTasks     = "tasks"
	commandKindVoice     = "voice"
	commandKindMedia     = "media"
	commandKindCancel    = "cancel"
	commandKindCompact   = "compact"
	commandKindPlan      = "plan"
	commandKindDisplay   = "display"
	commandKindToolLimit = "tool_limit"
	commandKindResume    = "resume"
	commandKindNew       = "new"
	commandKindSteer     = "steer"
	commandKindContinue  = "continue"
	commandKindInbox     = "inbox"
	commandKindNetwork   = "network"
	commandKindError     = "error"
)

const (
	maxCommandInputBytes  = 4096
	maxCommandNoticeBytes = 512
	maxResumeSessions     = 50
	maxResumePreviewRunes = 160
)

// DispatchCommand recognizes and handles the supported local commands. Leading
// and trailing whitespace is ignored. Any slash-prefixed input is handled as
// a local notice, including unknown commands, and is never provider-bound.
// Non-slash prompts return Handled=false and remain ordinary chat requests.
func DispatchCommand(ctx context.Context, prompt string, commandContext CommandContext) CommandResult {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return CommandResult{}
	}
	if len(trimmed) > maxCommandInputBytes {
		return commandError("slash command is too long")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return commandError("command cancelled")
	}

	commandName, args := splitCommandInput(trimmed)
	if commandName == "" {
		return CommandResult{}
	}
	name := strings.TrimPrefix(commandName, "/")
	if !isKnownCommand(name) {
		return commandError(fmt.Sprintf("unknown command %q; available commands: %s", boundedCommandText(commandName), commandAvailability(commandContext.MediaEnabled)))
	}
	if strings.TrimSpace(args) != "" && CommandName(name) != CommandSpeak && CommandName(name) != CommandSay && CommandName(name) != CommandVideo && CommandName(name) != CommandImage && CommandName(name) != CommandPlan && CommandName(name) != CommandTools && CommandName(name) != CommandThinking && CommandName(name) != CommandToolLimit && CommandName(name) != CommandResume && CommandName(name) != CommandContinue && CommandName(name) != CommandSteer && CommandName(name) != CommandInbox && CommandName(name) != CommandNetwork {
		return commandError(fmt.Sprintf("command %q does not accept arguments; available commands: %s", boundedCommandText(commandName), commandAvailability(commandContext.MediaEnabled)))
	}

	switch CommandName(name) {
	case CommandHelp:
		return CommandResult{Handled: true, Name: CommandHelp, Kind: commandKindHelp, Text: formatCommandHelp(commandContext.MediaEnabled)}
	case CommandStatus:
		status := commandStatus(ctx, commandContext)
		return CommandResult{Handled: true, Name: CommandStatus, Kind: commandKindStatus, Text: formatCommandStatus(status), Status: &status}
	case CommandSubagents:
		data := commandSubagents(commandContext.Application)
		return CommandResult{Handled: true, Name: CommandSubagents, Kind: commandKindSubagents, Text: formatCommandSubagents(data), Subagents: &data}
	case CommandTasks:
		data := commandTasks(commandContext)
		return CommandResult{Handled: true, Name: CommandTasks, Kind: commandKindTasks, Text: formatCommandTasks(data), Tasks: &data}
	case CommandCompact:
		if commandContext.Application == nil || commandContext.SessionID == "" {
			return commandError("/compact requires a durable session")
		}
		if err := commandContext.Application.CompactSessionWithModel(ctx, commandContext.SessionID, commandContext.Model); err != nil {
			return commandError("context compaction failed: " + boundedCommandText(err.Error()))
		}
		return CommandResult{Handled: true, Name: CommandCompact, Kind: commandKindCompact, Text: "context compacted; durable source transcript retained"}
	case CommandPlan:
		return commandPlan(commandContext, args)
	case CommandResume:
		return commandResume(commandContext, args)
	case CommandContinue:
		return commandContinue(commandContext, args)
	case CommandNew:
		return commandNew(commandContext)
	case CommandSteer:
		return commandSteer(commandContext, args)
	case CommandInbox:
		return commandInbox(ctx, commandContext, args)
	case CommandNetwork:
		return commandNetwork(ctx, commandContext, args)
	case CommandTools, CommandThinking:
		return commandDisplay(commandContext, CommandName(name), args)
	case CommandToolLimit:
		return commandToolLimit(commandContext, args)
	case CommandStop, CommandKill, CommandCancel:
		cancelled := 0
		if commandContext.Application != nil {
			cancelled = commandContext.Application.CancelActiveRuns()
		}
		return CommandResult{Handled: true, Name: CommandName(name), Kind: commandKindCancel, Text: formatCancelledRuns(cancelled), CancelledRuns: cancelled}
	case CommandVideo, CommandImage:
		if !commandContext.MediaEnabled {
			return commandError("media commands are not enabled for this interface")
		}
		if strings.TrimSpace(args) == "" {
			return commandError(fmt.Sprintf("usage: /%s <public http(s) URL>", name))
		}
		return CommandResult{Handled: true, Name: CommandName(name), Kind: commandKindMedia, Text: fmt.Sprintf("Preparing /%s media…", name), Media: &MediaCommand{Kind: CommandName(name), URL: strings.TrimSpace(args)}}
	case CommandSpeak, CommandSay:
		if strings.TrimSpace(args) == "" {
			if CommandName(name) == CommandSpeak {
				return commandError("usage: /speak <what you want answered as a voice note>")
			}
			return commandError(fmt.Sprintf("usage: /%s <text to synthesize>", name))
		}
		generate := CommandName(name) == CommandSpeak
		mode := voiceTTSModeClone
		if generate {
			mode = voiceTTSModeDirection
		}
		voice := &VoiceCommand{Generate: generate, TTSMode: mode, Text: strings.TrimSpace(args)}
		return CommandResult{Handled: true, Name: CommandName(name), Kind: commandKindVoice, Text: "Generating voice…", Voice: voice}
	default:
		return commandError("unknown slash command")
	}
}

func isKnownCommand(name string) bool {
	switch CommandName(name) {
	case CommandStatus, CommandHelp, CommandSubagents, CommandTasks, CommandCompact, CommandPlan, CommandTools, CommandThinking, CommandToolLimit, CommandResume, CommandContinue, CommandNew, CommandSteer, CommandVideo, CommandImage, CommandSpeak, CommandSay, CommandStop, CommandKill, CommandCancel, CommandInbox, CommandNetwork:
		return true
	default:
		return false
	}
}

func commandAvailability(mediaEnabled bool) string {
	available := "/status, /help, /inbox, /new, /resume, /continue, /steer, /plan, /tools, /thinking, /tool-limit, /subagents, /tasks, /compact, /stop, /kill, /cancel"
	if mediaEnabled {
		available += ", /video, /image"
	}
	return available + ", /speak, /say"
}

func formatCommandHelp(mediaEnabled bool) string {
	lines := []string{"/status — show local Bee and session status", "/new — start a fresh durable session", "/resume [session-id] — choose or directly resume a durable session", "/continue [checkpoint-id] — resume the latest or named saved checkpoint", "/steer <correction> — interrupt the current response and redirect it", "/plan [on|off|status|reset|complete] — inspect or change plan mode and lifecycle", "/tools [compact|full|off] — show or change tool-call detail", "/thinking [on|off] — show or change thinking visibility", "/tool-limit [on|off] — show or change the per-turn tool-call limit", "/subagents — show configured child capacity and coding routes", "/tasks — show current session plan tasks", "/inbox [ack] — show unread job notifications, or mark them all read", "/network [on|off] — show or change this session's network access", "/compact — append a context checkpoint while retaining the source transcript", "/stop — cancel active top-level runs", "/kill — alias for /stop", "/cancel — compatibility alias for /stop"}
	if mediaEnabled {
		lines = append(lines, "/video <public URL> — retrieve one public video item", "/image <public image URL> — retrieve one public image")
	}
	lines = append(lines, "/speak <prompt> — answer briefly as a voice note (direction mode)", "/say <text> — read literal text aloud (clone mode)", "/help — show this help")
	return strings.Join(lines, "\n")
}

func formatCancelledRuns(count int) string {
	return fmt.Sprintf("cancelled %d active run(s)", count)
}

func commandContinue(commandContext CommandContext, args string) CommandResult {
	if commandContext.Application == nil || commandContext.Application.Store == nil || commandContext.SessionID == "" {
		return commandError("/continue requires a durable session")
	}
	checkpointID := strings.TrimSpace(args)
	var checkpoint TurnCheckpoint
	var err error
	if checkpointID == "" {
		checkpoint, err = commandContext.Application.LatestPendingTurnCheckpoint(commandContext.SessionID)
	} else {
		if strings.ContainsAny(checkpointID, " \t\r\n") || len(checkpointID) > 256 {
			return commandError("usage: /continue [checkpoint-id]")
		}
		var session *Session
		session, err = commandContext.Application.Resume(commandContext.SessionID)
		if err == nil {
			checkpoint, err = session.TurnCheckpoint(checkpointID)
		}
	}
	if err != nil {
		return commandError("continue checkpoint: " + boundedCommandText(err.Error()))
	}
	prompt := "Continue from the saved checkpoint. Reuse completed evidence and perform only the unresolved work."
	return CommandResult{
		Handled:  true,
		Name:     CommandContinue,
		Kind:     commandKindContinue,
		Text:     "continuing from checkpoint " + checkpoint.ID,
		Continue: &CommandContinueData{CheckpointID: checkpoint.ID, Prompt: prompt},
	}
}

func commandPlan(commandContext CommandContext, args string) CommandResult {
	if commandContext.Application == nil {
		return commandError("plan mode requires an initialized application")
	}
	argumentText := strings.TrimSpace(args)
	fields := strings.Fields(argumentText)
	action := ""
	if len(fields) > 0 {
		action = strings.ToLower(fields[0])
	}
	if action == "" || (action == "status" && len(fields) == 1) {
		state, err := commandContext.Application.PlanMode(commandContext.SessionID)
		if err != nil {
			return commandError("read plan mode: " + boundedCommandText(err.Error()))
		}
		return CommandResult{Handled: true, Name: CommandPlan, Kind: commandKindPlan, Text: formatPlanModeState(state), PlanMode: &state}
	}
	if action == "reset" || action == "complete" {
		if commandContext.SessionID == "" {
			return commandError("plan lifecycle changes require a durable session")
		}
		reason := strings.TrimSpace(strings.TrimPrefix(argumentText, fields[0]))
		if action == "complete" {
			if reason == "" {
				reason = "plan completed by user"
			} else {
				reason = "plan completed by user: " + reason
			}
		} else if reason == "" {
			reason = "plan reset by user"
		}
		if err := commandContext.Application.ResetPlan(commandContext.SessionID, reason); err != nil {
			return commandError(action + " plan: " + boundedCommandText(err.Error()))
		}
		state, err := commandContext.Application.PlanMode(commandContext.SessionID)
		if err != nil {
			return commandError("read plan mode: " + boundedCommandText(err.Error()))
		}
		verb := "reset"
		if action == "complete" {
			verb = "completed"
		}
		return CommandResult{Handled: true, Name: CommandPlan, Kind: commandKindPlan, Text: "plan " + verb + "; the next gated mutation requires a fresh plan", PlanMode: &state}
	}
	if (action != "on" && action != "off") || len(fields) != 1 {
		return commandError("usage: /plan [on|off|status|reset [reason]|complete [note]]")
	}
	enabled := action == "on"
	changed, err := commandContext.Application.SetPlanMode(commandContext.SessionID, enabled)
	if err != nil {
		return commandError("change plan mode: " + boundedCommandText(err.Error()))
	}
	state, err := commandContext.Application.PlanMode(commandContext.SessionID)
	if err != nil {
		return commandError("read plan mode: " + boundedCommandText(err.Error()))
	}
	if !changed {
		return CommandResult{Handled: true, Name: CommandPlan, Kind: commandKindPlan, Text: fmt.Sprintf("plan mode is already %s (%s)", onOffLabel(enabled), state.Source), PlanMode: &state}
	}
	text := "plan mode disabled; planning history is retained, but mutations no longer require a Bee plan"
	if enabled {
		if commandContext.SessionID == "" {
			text = "plan mode enabled for the next session; a fresh plan will be required before gated mutations"
		} else {
			text = "plan mode enabled; the previous plan was cleared and a fresh plan is required before gated mutations"
		}
	}
	return CommandResult{Handled: true, Name: CommandPlan, Kind: commandKindPlan, Text: text, PlanMode: &state}
}

func formatPlanModeState(state PlanModeState) string {
	base := fmt.Sprintf("plan mode is %s (%s)", onOffLabel(state.Enabled), state.Source)
	if !state.ActivePlan {
		return base + "; no active plan"
	}
	progress := fmt.Sprintf("pending=%d in_progress=%d passed=%d failed=%d blocked=%d", state.CriteriaPending, state.CriteriaInProgress, state.CriteriaPassed, state.CriteriaFailed, state.CriteriaBlocked)
	goal := strings.TrimSpace(state.PlanGoal)
	if goal == "" {
		goal = "<unspecified>"
	}
	return fmt.Sprintf("%s\nactive plan: %s\ngoal: %s\ncriteria: %s", base, state.PlanID, goal, progress)
}

func onOffLabel(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func visibleHiddenLabel(visible bool) string {
	if visible {
		return "visible"
	}
	return "hidden"
}

func commandDisplay(commandContext CommandContext, name CommandName, args string) CommandResult {
	if commandContext.Application == nil {
		return commandError("display controls require an initialized application")
	}
	argument := strings.ToLower(strings.TrimSpace(args))
	visibility := commandContext.Application.DisplayVisibility()
	if name == CommandTools {
		if argument == "on" {
			argument = string(ToolDisplayFull)
		}
		if argument != "" && argument != string(ToolDisplayOff) && argument != string(ToolDisplayCompact) && argument != string(ToolDisplayFull) {
			return commandError("usage: /tools [compact|full|off]")
		}
		if argument != "" {
			visibility = commandContext.Application.SetToolDisplayMode(ToolDisplayMode(argument))
		}
		state := string(visibility.toolDisplayMode())
		if state == string(ToolDisplayOff) {
			state = "hidden"
		}
		return CommandResult{Handled: true, Name: name, Kind: commandKindDisplay, Text: "tool-call details are " + state, Display: &visibility}
	}
	if argument != "" && argument != "on" && argument != "off" {
		return commandError("usage: /thinking [on|off]")
	}
	if argument != "" {
		visibility = commandContext.Application.SetThinkingVisible(argument == "on")
	}
	state := "hidden"
	if visibility.Thinking {
		state = "visible"
	}
	return CommandResult{Handled: true, Name: name, Kind: commandKindDisplay, Text: "thinking is " + state, Display: &visibility}
}

func commandToolLimit(commandContext CommandContext, args string) CommandResult {
	if commandContext.Application == nil {
		return commandError("tool-call limit controls require an initialized application")
	}
	argument := strings.ToLower(strings.TrimSpace(args))
	if argument != "" && argument != "on" && argument != "off" {
		return commandError("usage: /tool-limit [on|off]")
	}
	state := commandContext.Application.ToolCallLimit()
	if argument != "" {
		state = commandContext.Application.SetToolCallLimitEnabled(argument == "on")
	}
	text := "tool-call limit is off"
	if state.Enabled {
		text = fmt.Sprintf("tool-call limit is on (%d provider tool batch%s per segment)", state.Maximum, pluralSuffix(state.Maximum))
	}
	return CommandResult{Handled: true, Name: CommandToolLimit, Kind: commandKindToolLimit, Text: text, ToolCallLimit: &state}
}

func commandNew(commandContext CommandContext) CommandResult {
	app := commandContext.Application
	if app == nil || app.Store == nil {
		return commandError("/new requires a configured session store")
	}
	session, err := app.Store.Create(SessionMetadata{Title: commandContext.Title, TitleSource: provisionalTitleSource(commandContext.Title, commandContext.TitleProvisional)})
	if err != nil {
		return commandError("create session: " + boundedCommandText(err.Error()))
	}
	app.beginMoodSession(session.ID)
	entry := CommandResumeSession{
		ID:         session.ID,
		UpdatedAt:  session.Metadata.UpdatedAt,
		EventCount: len(session.EventsSnapshot()),
		Current:    true,
	}
	data := CommandNewSessionData{Session: entry}
	return CommandResult{Handled: true, Name: CommandNew, Kind: commandKindNew, Text: "started new session " + compactResumePreview(string(session.ID)), NewSession: &data}
}

func commandSteer(commandContext CommandContext, args string) CommandResult {
	prompt := strings.TrimSpace(args)
	if prompt == "" {
		return commandError("usage: /steer <correction>")
	}
	if !commandContext.RunActive {
		return commandError("/steer requires a response in progress")
	}
	data := CommandSteerData{Prompt: prompt}
	return CommandResult{Handled: true, Name: CommandSteer, Kind: commandKindSteer, Text: "steering current response…", Steer: &data}
}

func commandResume(commandContext CommandContext, args string) CommandResult {
	app := commandContext.Application
	if app == nil || app.Store == nil {
		return commandError("/resume requires a configured session store")
	}
	argument := strings.TrimSpace(args)
	if argument != "" {
		if len(strings.Fields(argument)) != 1 {
			return commandError("usage: /resume [session-id]")
		}
		session, err := resolveResumeSession(app, argument)
		if err != nil {
			return commandError("resume session: " + boundedCommandText(err.Error()))
		}
		app.beginMoodSession(session.ID)
		entry := resumeSessionProjection(session, SessionSummary{
			ID:         session.ID,
			Title:      session.Metadata.Title,
			Pinned:     session.Metadata.Pinned,
			UpdatedAt:  session.Metadata.UpdatedAt,
			EventCount: len(session.EventsSnapshot()),
		}, commandContext.SessionID)
		label := entry.Title
		if label == "" {
			label = entry.Preview
		}
		text := "resumed session " + compactResumePreview(string(entry.ID))
		if label != "" {
			text += " · " + compactResumePreview(label)
		}
		return CommandResult{Handled: true, Name: CommandResume, Kind: commandKindResume, Text: text, Resume: &CommandResumeData{Selected: &entry}}
	}

	summaries, err := app.ListSessions()
	if err != nil {
		return commandError("list sessions: " + boundedCommandText(err.Error()))
	}
	if len(summaries) > maxResumeSessions {
		summaries = summaries[:maxResumeSessions]
	}
	data := CommandResumeData{Sessions: make([]CommandResumeSession, 0, len(summaries))}
	for _, summary := range summaries {
		session, loadErr := app.Resume(summary.ID)
		if loadErr != nil {
			data.Sessions = append(data.Sessions, resumeSessionProjection(nil, summary, commandContext.SessionID))
			continue
		}
		data.Sessions = append(data.Sessions, resumeSessionProjection(session, summary, commandContext.SessionID))
	}
	return CommandResult{Handled: true, Name: CommandResume, Kind: commandKindResume, Text: formatResumeSessions(data.Sessions), Resume: &data}
}

func resolveResumeSession(app *Application, value string) (*Session, error) {
	id := SessionID(value)
	session, err := app.Resume(id)
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, ErrSessionNotFound) {
		return nil, err
	}
	summaries, listErr := app.ListSessions()
	if listErr != nil {
		return nil, listErr
	}
	var match SessionID
	for _, summary := range summaries {
		if !strings.HasPrefix(string(summary.ID), value) {
			continue
		}
		if match != "" {
			return nil, fmt.Errorf("session id prefix %q is ambiguous", value)
		}
		match = summary.ID
	}
	if match == "" {
		return nil, fmt.Errorf("session %q not found", value)
	}
	return app.Resume(match)
}

func resumeSessionProjection(session *Session, summary SessionSummary, current SessionID) CommandResumeSession {
	entry := CommandResumeSession{
		ID:         summary.ID,
		Title:      compactResumePreview(summary.Title),
		UpdatedAt:  summary.UpdatedAt,
		EventCount: summary.EventCount,
		Pinned:     summary.Pinned,
		Current:    summary.ID != "" && summary.ID == current,
	}
	if session == nil {
		return entry
	}
	messages, err := session.Messages()
	if err != nil {
		return entry
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != RoleUser || strings.TrimSpace(messages[index].Content) == "" {
			continue
		}
		entry.Preview = compactResumePreview(messages[index].Content)
		return entry
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == RoleAssistant && strings.TrimSpace(messages[index].Content) != "" {
			entry.Preview = compactResumePreview(messages[index].Content)
			return entry
		}
	}
	return entry
}

func compactResumePreview(value string) string {
	value = strings.Map(func(value rune) rune {
		if unicode.IsControl(value) && !unicode.IsSpace(value) {
			return -1
		}
		return value
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= maxResumePreviewRunes {
		return value
	}
	return string(runes[:maxResumePreviewRunes-1]) + "…"
}

func formatResumeSessions(sessions []CommandResumeSession) string {
	if len(sessions) == 0 {
		return "no durable sessions found"
	}
	lines := []string{"recent sessions"}
	for _, session := range sessions {
		label := session.Title
		if label != "" && session.Preview != "" && session.Preview != label {
			label += " — " + session.Preview
		} else if label == "" {
			label = session.Preview
		}
		if label == "" {
			label = "<untitled>"
		}
		current := ""
		if session.Current {
			current = " (current)"
		}
		updated := "unknown time"
		if !session.UpdatedAt.IsZero() {
			updated = session.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		lines = append(lines, fmt.Sprintf("- %s · %s · %s%s", compactResumePreview(string(session.ID)), updated, label, current))
	}
	lines = append(lines, "use /resume <session-id> to continue one")
	return strings.Join(lines, "\n")
}

func splitCommandInput(input string) (string, string) {
	input = strings.TrimSpace(input)
	if input == "" || !strings.HasPrefix(input, "/") {
		return "", ""
	}
	for index, r := range input {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return input[:index], strings.TrimSpace(input[index:])
		}
	}
	return input, ""
}

func commandStatus(ctx context.Context, commandContext CommandContext) CommandStatusData {
	status := CommandStatusData{ApplicationReady: commandContext.Application != nil, GatewayReady: commandContext.GatewayReady, SessionID: commandContext.SessionID, Model: commandContext.Model}
	if directory, err := os.Getwd(); err == nil {
		status.Directory = directory
	}
	if commandContext.Application == nil {
		return status
	}
	app := commandContext.Application
	status.Provider = app.Config.Provider.Type
	status.Profile = app.ProviderProfile
	status.Model = commandContext.Model
	if status.Model == "" {
		status.Model = app.Config.Provider.Model
	}
	status.ReasoningEffort = strings.ToLower(strings.TrimSpace(app.Config.Generation.ReasoningEffort))
	if status.ReasoningEffort == "" {
		status.ReasoningEffort = "provider default"
	}
	status.ContextWindowTokens = app.Config.Provider.ContextWindowTokens
	if status.ContextWindowTokens <= 0 {
		if discoverer, ok := app.Provider.(ProviderContextWindow); ok {
			status.ContextWindowTokens, _ = discoverer.ContextWindowTokens(status.Model)
		}
	}
	status.Scope = strings.TrimSpace(app.Capabilities.ScopeRoot)
	if status.Scope == "" && app.Executor != nil {
		status.Scope = strings.TrimSpace(app.Executor.Scope.Root)
	}
	if status.Scope != "" && app.Capabilities.FullMachine {
		status.Scope += " (full-machine)"
	}
	status.PlanModeAvailable = app.supportsPlanMode()
	visibility := app.DisplayVisibility()
	status.Display = &visibility
	toolCallLimit := app.ToolCallLimit()
	status.ToolCallLimit = &toolCallLimit
	if state, err := app.PlanMode(status.SessionID); err == nil {
		status.PlanModeEnabled = state.Enabled
		status.PlanModeSource = state.Source
	} else {
		status.PlanModeEnabled = true
		status.PlanModeSource = "invalid session state; fail closed"
	}
	status.ToolCount = commandToolCount(app, status.PlanModeEnabled)
	if enabled, err := app.SessionNetworkEnabled(status.SessionID); err == nil {
		status.NetworkOff = !enabled
	}
	status.CodingRouteReady = app.CodingDelegation != nil
	telemetry := app.commandStatusTelemetry(status.SessionID)
	if telemetry.recentProviderCalls > 0 {
		usage := telemetry.recentUsage
		status.RecentUsage = &usage
		status.RecentProviderCalls = telemetry.recentProviderCalls
	}
	if telemetry.lastTiming != nil {
		timing := *telemetry.lastTiming
		status.LastProviderCall = &timing
	}
	if telemetry.contextUsageSeen {
		usage := telemetry.contextUsage
		status.RequestContext = &usage
		if status.ContextWindowTokens <= 0 && usage.WindowKnown {
			status.ContextWindowTokens = usage.WindowTokens
		}
	}
	if telemetry.contextSeen {
		diagnostics := telemetry.contextDiagnostics
		status.ContextDiagnostics = &diagnostics
	}
	if telemetry.memoryExtractionSeen {
		extraction := telemetry.memoryExtraction
		status.MemoryExtraction = &extraction
	}
	if status.SessionID != "" && app.Store != nil {
		if err := ctx.Err(); err == nil {
			if session, err := app.Resume(status.SessionID); err == nil && session != nil {
				status.SessionTitle = session.Metadata.Title
			}
		}
	}
	return status
}

func formatCommandStatus(status CommandStatusData) string {
	return FormatCommandStatus(status, 88)
}

type commandStatusRow struct {
	label     string
	value     string
	separator bool
}

// FormatCommandStatus returns a bounded terminal-friendly status card. Width
// is measured in runes and capped so unusually long paths cannot dominate the
// transcript.
func FormatCommandStatus(status CommandStatusData, width int) string {
	if width <= 0 {
		width = 88
	}
	if width > 100 {
		width = 100
	}
	if width < 32 {
		width = 32
	}
	model := commandStatusValue(status.Model, "<configured>")
	reasoning := commandStatusValue(status.ReasoningEffort, "provider default")
	directory := commandStatusValue(status.Directory, "<unknown>")
	contextWindow := "unknown (not configured)"
	if status.ContextWindowTokens > 0 {
		contextWindow = formatStatusInteger(status.ContextWindowTokens) + " tokens"
	}
	rows := []commandStatusRow{
		{label: "Model", value: model},
		{label: "Reasoning effort", value: reasoning},
		{label: "Directory", value: directory},
		{label: "Context window", value: contextWindow},
		{label: "Plan mode", value: onOffLabel(status.PlanModeEnabled) + planModeSourceSuffix(status.PlanModeSource)},
		{label: "Network", value: networkStateText(!status.NetworkOff)},
		{separator: true},
		{label: "Session", value: commandStatusValue(string(status.SessionID), "<none>")},
		{label: "Session title", value: commandStatusValue(status.SessionTitle, "<none>")},
		{label: "Profile", value: commandStatusValue(status.Profile, "<default>")},
		{label: "Provider", value: commandStatusValue(status.Provider, "<unknown>")},
	}
	if status.Display != nil {
		rows = append(rows[:5], append([]commandStatusRow{
			{label: "Tool calls", value: toolDisplayModeLabel(*status.Display)},
			{label: "Thinking", value: visibleHiddenLabel(status.Display.Thinking)},
		}, rows[5:]...)...)
	}
	if status.ToolCallLimit != nil {
		value := "off (unlimited)"
		if status.ToolCallLimit.Enabled {
			value = fmt.Sprintf("on (%s batches/segment)", formatStatusInteger(status.ToolCallLimit.Maximum))
		}
		rows = append(rows[:5], append([]commandStatusRow{{label: "Tool-call limit", value: value}}, rows[5:]...)...)
	}
	if status.Scope != "" {
		rows = append(rows, commandStatusRow{label: "Scope", value: status.Scope})
	}
	if status.ToolCount > 0 {
		rows = append(rows, commandStatusRow{label: "Tools", value: formatStatusInteger(status.ToolCount)})
	}
	gateway := "unknown"
	if status.GatewayReady != nil {
		gateway = statusReadyLabel(*status.GatewayReady)
	}
	rows = append(rows,
		commandStatusRow{label: "Application", value: statusReadyLabel(status.ApplicationReady)},
		commandStatusRow{label: "Gateway", value: gateway},
		commandStatusRow{label: "Coding route", value: statusReadyLabel(status.CodingRouteReady)},
	)
	rows = append(rows, commandStatusRow{separator: true})
	if status.RecentUsage != nil {
		usage := status.RecentUsage
		value := fmt.Sprintf("%d call%s · %s tokens (%s in / %s out)", status.RecentProviderCalls, pluralSuffix(status.RecentProviderCalls), formatStatusInteger(usage.TotalTokens), formatStatusInteger(usage.PromptTokens), formatStatusInteger(usage.CompletionTokens))
		if usage.CacheReadTokens > 0 {
			value += fmt.Sprintf(", %s cached", formatStatusInteger(usage.CacheReadTokens))
		}
		rows = append(rows, commandStatusRow{label: "Recent usage", value: value})
	} else {
		rows = append(rows, commandStatusRow{label: "Recent usage", value: "none recorded"})
	}
	if status.LastProviderCall != nil {
		rows = append(rows, commandStatusRow{label: "Last model call", value: formatProviderCallTiming(*status.LastProviderCall)})
	}
	if status.RequestContext != nil {
		rows = append(rows, commandStatusRow{label: "Current context", value: formatRequestContextUsage(*status.RequestContext)})
	} else {
		rows = append(rows, commandStatusRow{label: "Current context", value: "no request snapshot recorded"})
	}
	if status.ContextDiagnostics != nil {
		diagnostics := status.ContextDiagnostics
		rows = append(rows, commandStatusRow{label: "Injected context", value: fmt.Sprintf("%s included · %s dropped · %s bytes", formatStatusInteger(diagnostics.IncludedItems), formatStatusInteger(diagnostics.DroppedItems), formatStatusInteger(diagnostics.IncludedBytes))})
		for index, source := range diagnostics.Sources {
			if index >= 16 {
				break
			}
			rows = append(rows, commandStatusRow{label: "  " + string(source.Source), value: fmt.Sprintf("%s included · %s dropped · %s bytes", formatStatusInteger(source.Included), formatStatusInteger(source.Dropped), formatStatusInteger(source.Bytes))})
		}
	}
	if status.MemoryExtraction != nil {
		extraction := status.MemoryExtraction
		value := fmt.Sprintf("%s saved · %s unchanged · %s proposed · %s rejected", formatStatusInteger(extraction.Saved), formatStatusInteger(extraction.Unchanged), formatStatusInteger(extraction.Proposed), formatStatusInteger(extraction.Rejected))
		if extraction.Error != "" {
			value += " · " + extraction.Error
		}
		rows = append(rows, commandStatusRow{label: "Memory", value: value})
	}

	labelWidth := 16
	valueWidth := width - labelWidth - 6
	var builder strings.Builder
	builder.WriteString(tuiBoxBorder('╭', '╮', "─ Bee status ", width))
	for _, row := range rows {
		builder.WriteByte('\n')
		if row.separator {
			builder.WriteString("├" + strings.Repeat("─", width-2) + "┤")
			continue
		}
		label := fitStatusCell(row.label, labelWidth)
		value := fitStatusCell(commandStatusValue(row.value, "<none>"), valueWidth)
		fmt.Fprintf(&builder, "│ %-*s  %-*s │", labelWidth, label, valueWidth, value)
	}
	builder.WriteByte('\n')
	builder.WriteString("╰" + strings.Repeat("─", width-2) + "╯")
	return builder.String()
}

func toolDisplayModeLabel(visibility DisplayVisibility) string {
	if visibility.toolDisplayMode() == ToolDisplayOff {
		return "hidden"
	}
	return string(visibility.toolDisplayMode())
}

func formatRequestContextUsage(usage RequestContextUsage) string {
	used := "~" + formatStatusInteger(usage.PromptUsed) + " used"
	if !usage.WindowKnown {
		return used + " · window unknown"
	}
	if usage.PromptBudget <= 0 {
		return used + " · no prompt budget available"
	}
	percent := (usage.PromptUsed*100 + usage.PromptBudget/2) / usage.PromptBudget
	value := fmt.Sprintf("%s · ~%s remaining · %d%% of prompt budget", used, formatStatusInteger(usage.PromptRemaining), percent)
	if usage.OverBudget {
		value += " · over budget"
	}
	return value
}

func commandToolCount(app *Application, _ bool) int {
	if app == nil {
		return 0
	}
	seen := make(map[string]struct{}, len(app.Capabilities.ToolNames))
	for _, name := range app.Capabilities.ToolNames {
		name = strings.TrimSpace(name)
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 && app.Executor != nil && app.Executor.Registry != nil {
		for _, definition := range app.Executor.Registry.Definitions() {
			if name := strings.TrimSpace(definition.Name); name != "" {
				seen[name] = struct{}{}
			}
		}
	}
	if app.supportsPlanMode() && app.ProviderRole != ProviderRoleChild {
		seen[PlanToolName] = struct{}{}
		seen[PlanStatusToolName] = struct{}{}
		seen[PlanUpdateToolName] = struct{}{}
	} else {
		delete(seen, PlanToolName)
		delete(seen, PlanStatusToolName)
		delete(seen, PlanUpdateToolName)
	}
	return len(seen)
}

func planModeSourceSuffix(source string) string {
	source = commandStatusValue(source, "")
	if source == "" {
		return ""
	}
	return " (" + source + ")"
}

func commandStatusValue(value, fallback string) string {
	value = strings.Join(strings.Fields(tuiSafeText(value)), " ")
	if value == "" {
		return fallback
	}
	return value
}

func fitStatusCell(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func formatStatusInteger(value int) string {
	negative := value < 0
	digits := fmt.Sprintf("%d", value)
	if negative {
		digits = strings.TrimPrefix(digits, "-")
	}
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	if negative {
		return "-" + digits
	}
	return digits
}

func statusReadyLabel(ready bool) string {
	if ready {
		return "ready"
	}
	return "unavailable"
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func commandSubagents(app *Application) CommandSubagentsData {
	data := CommandSubagentsData{}
	if app == nil {
		return data
	}
	data.ChildProfile = app.ChildProviderProfile
	data.ChildModel = app.ChildConfigOrMain().Provider.Model
	data.MaxChildren = app.ChildMax
	data.CodingRouteReady = app.CodingDelegation != nil
	if app.CodingDelegation != nil && app.CodingDelegation.Registry != nil {
		config := app.CodingDelegation.Registry.Config()
		data.CodingBackendsLive = config.Enabled
		for _, backend := range config.Backends {
			if backend.Enabled && strings.TrimSpace(backend.Name) != "" {
				data.CodingBackends = append(data.CodingBackends, backend.Name)
			}
		}
	}
	return data
}

func formatCommandSubagents(data CommandSubagentsData) string {
	profile := data.ChildProfile
	if profile == "" {
		profile = "<none>"
	}
	model := data.ChildModel
	if model == "" {
		model = "<configured>"
	}
	backends := strings.Join(data.CodingBackends, ", ")
	if backends == "" {
		backends = "<none enabled>"
	}
	return fmt.Sprintf("subagents\nchild profile: %s\nchild model: %s\nmax children: %d\ncoding route ready: %t\ncoding backends live: %t\ncoding backends: %s", profile, model, data.MaxChildren, data.CodingRouteReady, data.CodingBackendsLive, backends)
}

func commandTasks(commandContext CommandContext) CommandTasksData {
	data := CommandTasksData{SessionID: commandContext.SessionID}
	if commandContext.Application == nil || commandContext.Application.Store == nil || commandContext.SessionID == "" {
		return data
	}
	session, err := commandContext.Application.Resume(commandContext.SessionID)
	if err != nil || session == nil {
		return data
	}
	data.SessionTitle = session.Metadata.Title
	latest, _ := latestPlanStateFromEvents(session.EventsSnapshot())
	if latest == nil {
		return data
	}
	for _, task := range latest.Tasks {
		status := "planned"
		if len(task.Checklist) > 0 {
			status = "in progress"
			allDone := true
			for _, item := range task.Checklist {
				if !item.Done {
					allDone = false
					break
				}
			}
			if allDone {
				status = "done"
			}
		}
		data.Tasks = append(data.Tasks, CommandTaskData{ID: task.ID, Title: task.Title, Status: status, Source: "session_plan", SessionID: commandContext.SessionID})
	}
	return data
}

func formatCommandTasks(data CommandTasksData) string {
	title := data.SessionTitle
	if title == "" {
		title = "<none>"
	}
	lines := []string{"tasks", fmt.Sprintf("session: %s", data.SessionID), fmt.Sprintf("session title: %s", title)}
	if len(data.Tasks) == 0 {
		lines = append(lines, "no recorded plan tasks")
		return strings.Join(lines, "\n")
	}
	for _, task := range data.Tasks {
		lines = append(lines, fmt.Sprintf("- [%s] %s: %s", task.Status, task.ID, task.Title))
	}
	return strings.Join(lines, "\n")
}

func commandError(message string) CommandResult {
	return CommandResult{Handled: true, Kind: commandKindError, Text: boundedCommandText(message)}
}

func boundedCommandText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxCommandNoticeBytes {
		return text
	}
	return text[:maxCommandNoticeBytes-3] + "..."
}

// CommandError reports whether a command result is a handled local error.
func (result CommandResult) CommandError() error {
	if !result.Handled || result.Kind != commandKindError {
		return nil
	}
	if result.Text == "" {
		return errors.New("local command failed")
	}
	return errors.New(result.Text)
}

// localCommandEvent projects one shared command result into the structured
// adapter event used by the runtime and direct owner-facing transports.
// Keeping this projection in core prevents HTTP adapters from inventing a
// second command result shape.
func localCommandEvent(result CommandResult) LocalCommandEvent {
	return LocalCommandEvent{
		Name:          result.Name,
		Kind:          result.Kind,
		Text:          result.Text,
		Status:        result.Status,
		Subagents:     result.Subagents,
		Tasks:         result.Tasks,
		CancelledRuns: result.CancelledRuns,
		PlanMode:      result.PlanMode,
		Display:       result.Display,
		ToolCallLimit: result.ToolCallLimit,
		Resume:        result.Resume,
		NewSession:    result.NewSession,
		Steer:         result.Steer,
		Continue:      result.Continue,
		Inbox:         result.Inbox,
	}
}
