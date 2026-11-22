package bee

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type commandCountingProvider struct {
	mu       sync.Mutex
	requests int
}

func (provider *commandCountingProvider) Complete(context.Context, Request) (Response, error) {
	provider.mu.Lock()
	provider.requests++
	provider.mu.Unlock()
	return Response{Message: Message{Role: RoleAssistant, Content: "complete"}}, nil
}

func (provider *commandCountingProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.requests++
	provider.mu.Unlock()
	if err := onDelta("reply"); err != nil {
		return Response{}, err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "reply"}}, nil
}

func (provider *commandCountingProvider) count() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.requests
}

func TestDispatchCommandRecognizesOnlySupportedCommands(t *testing.T) {
	for _, test := range []struct {
		prompt  string
		handled bool
		name    CommandName
		kind    string
	}{
		{prompt: " hello ", handled: false},
		{prompt: "  /status  ", handled: true, name: CommandStatus, kind: commandKindStatus},
		{prompt: "/help", handled: true, name: CommandHelp, kind: commandKindHelp},
		{prompt: "/video https://example.com/watch", handled: true, name: CommandVideo, kind: commandKindMedia},
		{prompt: "/image https://example.com/picture.png", handled: true, name: CommandImage, kind: commandKindMedia},
		{prompt: "/status extra", handled: true, kind: commandKindError},
		{prompt: "/unknown", handled: true, kind: commandKindError},
	} {
		t.Run(test.prompt, func(t *testing.T) {
			result := DispatchCommand(context.Background(), test.prompt, CommandContext{MediaEnabled: true})
			if result.Handled != test.handled || result.Name != test.name || result.Kind != test.kind {
				t.Fatalf("result = %#v, want handled=%t name=%q kind=%q", result, test.handled, test.name, test.kind)
			}
			if result.Kind == commandKindError {
				if len(result.Text) > maxCommandNoticeBytes || !strings.Contains(result.Text, "available commands") {
					t.Fatalf("bounded command error = %q", result.Text)
				}
			}
		})
	}
}

func TestDisplayCommandsToggleProcessLocalVisibility(t *testing.T) {
	app := &Application{}
	if initial := app.DisplayVisibility(); !initial.ToolCalls || !initial.Thinking {
		t.Fatalf("initial visibility=%#v", initial)
	}

	tools := DispatchCommand(context.Background(), "/tools off", CommandContext{Application: app})
	if !tools.Handled || tools.Kind != commandKindDisplay || tools.Display == nil || tools.Display.ToolCalls || tools.Display.ToolMode != ToolDisplayOff || !tools.Display.Thinking || tools.Text != "tool-call details are hidden" {
		t.Fatalf("tools result=%#v", tools)
	}
	thinking := DispatchCommand(context.Background(), "/thinking off", CommandContext{Application: app})
	if !thinking.Handled || thinking.Kind != commandKindDisplay || thinking.Display == nil || thinking.Display.ToolCalls || thinking.Display.Thinking || thinking.Text != "thinking is hidden" {
		t.Fatalf("thinking result=%#v", thinking)
	}
	query := DispatchCommand(context.Background(), "/tools", CommandContext{Application: app})
	if query.Display == nil || query.Display.ToolCalls || query.Display.Thinking {
		t.Fatalf("query result=%#v", query)
	}
	if invalid := DispatchCommand(context.Background(), "/thinking maybe", CommandContext{Application: app}); invalid.Kind != commandKindError || !strings.Contains(invalid.Text, "usage: /thinking [on|off]") {
		t.Fatalf("invalid result=%#v", invalid)
	}
	compact := DispatchCommand(context.Background(), "/tools compact", CommandContext{Application: app})
	if compact.Display == nil || compact.Display.ToolMode != ToolDisplayCompact || compact.Text != "tool-call details are compact" {
		t.Fatalf("compact result=%#v", compact)
	}
	DispatchCommand(context.Background(), "/tools on", CommandContext{Application: app})
	DispatchCommand(context.Background(), "/thinking on", CommandContext{Application: app})
	if final := app.DisplayVisibility(); !final.ToolCalls || final.ToolMode != ToolDisplayFull || !final.Thinking {
		t.Fatalf("final visibility=%#v", final)
	}
	if invalid := DispatchCommand(context.Background(), "/tools maybe", CommandContext{Application: app}); invalid.Kind != commandKindError || !strings.Contains(invalid.Text, "usage: /tools [compact|full|off]") {
		t.Fatalf("invalid tools result=%#v", invalid)
	}
}

func TestToolLimitCommandDefaultsOnAndControlsSubsequentAgents(t *testing.T) {
	config := DefaultConfig()
	config.Agent.MaxToolSteps = 1
	app := &Application{Config: config, Executor: testEchoExecutor(t)}

	query := DispatchCommand(context.Background(), "/tool-limit", CommandContext{Application: app})
	if !query.Handled || query.Kind != commandKindToolLimit || query.ToolCallLimit == nil || !query.ToolCallLimit.Enabled || query.ToolCallLimit.Maximum != 1 || query.Text != "tool-call limit is on (1 provider tool batch per segment)" {
		t.Fatalf("default result=%#v", query)
	}

	off := DispatchCommand(context.Background(), "/tool-limit off", CommandContext{Application: app})
	if off.ToolCallLimit == nil || off.ToolCallLimit.Enabled || off.ToolCallLimit.Maximum != 0 || off.Text != "tool-call limit is off" {
		t.Fatalf("off result=%#v", off)
	}
	unlimitedProvider := &toolLoopProvider{toolResponses: 2}
	unlimited, err := NewAgentWithOptions(config, unlimitedProvider, app.agentOptions(nil, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	response, err := unlimited.Run(context.Background(), "continue", nil)
	if err != nil || response.Message.Content != "done" || unlimitedProvider.calls != 3 {
		t.Fatalf("unlimited response=%#v err=%v calls=%d", response, err, unlimitedProvider.calls)
	}

	on := DispatchCommand(context.Background(), "/tool-limit on", CommandContext{Application: app})
	if on.ToolCallLimit == nil || !on.ToolCallLimit.Enabled || on.ToolCallLimit.Maximum != 1 {
		t.Fatalf("on result=%#v", on)
	}
	boundedProvider := &toolLoopProvider{alwaysTool: true}
	bounded, err := NewAgentWithOptions(config, boundedProvider, app.agentOptions(nil, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if response, err := bounded.Run(context.Background(), "stop", nil); err != nil || response.Message.Content == "" {
		t.Fatalf("bounded response=%#v err=%v, want graceful landing", response, err)
	}
	if invalid := DispatchCommand(context.Background(), "/tool-limit maybe", CommandContext{Application: app}); invalid.Kind != commandKindError || !strings.Contains(invalid.Text, "usage: /tool-limit [on|off]") {
		t.Fatalf("invalid result=%#v", invalid)
	}
	if help := DispatchCommand(context.Background(), "/help", CommandContext{Application: app}); !strings.Contains(help.Text, "/tool-limit [on|off]") {
		t.Fatalf("help=%q", help.Text)
	}
}

func TestResumeCommandListsPreviewsAndSelectsByUniquePrefix(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.Create(SessionMetadata{Title: "Older work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := older.AppendMessages(
		Message{Role: RoleUser, Content: "first request"},
		Message{Role: RoleAssistant, Content: "first answer"},
		Message{Role: RoleUser, Content: "latest\nrequest with context"},
	); err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(SessionMetadata{Title: "Current work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.AppendMessage(Message{Role: RoleUser, Content: "current request"}); err != nil {
		t.Fatal(err)
	}
	listed := DispatchCommand(context.Background(), "/resume", CommandContext{Application: app, SessionID: newer.ID})
	if !listed.Handled || listed.Name != CommandResume || listed.Kind != commandKindResume || listed.Resume == nil || len(listed.Resume.Sessions) != 2 {
		t.Fatalf("resume list=%#v", listed)
	}
	var olderEntry *CommandResumeSession
	for index := range listed.Resume.Sessions {
		if listed.Resume.Sessions[index].ID == older.ID {
			olderEntry = &listed.Resume.Sessions[index]
		}
	}
	if olderEntry == nil || olderEntry.Title != "Older work" || olderEntry.Preview != "latest request with context" || olderEntry.Current {
		t.Fatalf("older entry=%#v", olderEntry)
	}
	if provider.count() != 0 {
		t.Fatalf("resume listing called provider %d times", provider.count())
	}

	prefix := string(older.ID)[:12]
	selected := DispatchCommand(context.Background(), "/resume "+prefix, CommandContext{Application: app, SessionID: newer.ID})
	if selected.Resume == nil || selected.Resume.Selected == nil || selected.Resume.Selected.ID != older.ID || !strings.Contains(selected.Text, string(older.ID)) {
		t.Fatalf("selected resume=%#v", selected)
	}
	if missing := DispatchCommand(context.Background(), "/resume does-not-exist", CommandContext{Application: app}); missing.Kind != commandKindError || !strings.Contains(missing.Text, "not found") {
		t.Fatalf("missing resume=%#v", missing)
	}
	if provider.count() != 0 {
		t.Fatalf("resume selection called provider %d times", provider.count())
	}
}

func TestContinueCommandSelectsLatestPendingCheckpoint(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &commandCountingProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "Continuation"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopDeadline, NextAction: "first", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.AppendTurnCheckpoint(TurnCheckpoint{WhyStopped: TurnStopToolBudget, NextAction: "second", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	result := DispatchCommand(context.Background(), "/continue", CommandContext{Application: app, SessionID: session.ID})
	if result.Kind != commandKindContinue || result.Continue == nil || result.Continue.CheckpointID != second.ID || !strings.Contains(result.Continue.Prompt, "unresolved work") {
		t.Fatalf("continue result=%#v", result)
	}
	if err := session.AppendTurnCheckpointResume(second.ID); err != nil {
		t.Fatal(err)
	}
	result = DispatchCommand(context.Background(), "/continue", CommandContext{Application: app, SessionID: session.ID})
	if result.Continue == nil || result.Continue.CheckpointID != first.ID {
		t.Fatalf("pending continue result=%#v", result)
	}
	result = DispatchCommand(context.Background(), "/continue "+second.ID, CommandContext{Application: app, SessionID: session.ID})
	if result.Continue == nil || result.Continue.CheckpointID != second.ID {
		t.Fatalf("explicit continue result=%#v", result)
	}
}

func TestNewCommandCreatesAndSelectsFreshDurableSession(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(SessionMetadata{Title: "Current"})
	if err != nil {
		t.Fatal(err)
	}
	result := DispatchCommand(context.Background(), "/new", CommandContext{Application: app, SessionID: current.ID})
	if !result.Handled || result.Name != CommandNew || result.Kind != commandKindNew || result.NewSession == nil {
		t.Fatalf("new result=%#v", result)
	}
	fresh := result.NewSession.Session
	if fresh.ID == "" || fresh.ID == current.ID || !fresh.Current || !strings.Contains(result.Text, string(fresh.ID)) {
		t.Fatalf("fresh session=%#v text=%q", fresh, result.Text)
	}
	loaded, err := store.Load(fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := loaded.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 || provider.count() != 0 {
		t.Fatalf("fresh messages=%#v provider calls=%d", messages, provider.count())
	}
}

func TestSteerCommandRequiresActiveRunAndCorrection(t *testing.T) {
	for _, prompt := range []string{"/steer", "/steer   "} {
		result := DispatchCommand(context.Background(), prompt, CommandContext{RunActive: true})
		if !result.Handled || result.Kind != commandKindError || !strings.Contains(result.Text, "usage: /steer") {
			t.Fatalf("missing correction result=%#v", result)
		}
	}
	if idle := DispatchCommand(context.Background(), "/steer turn left", CommandContext{}); idle.Kind != commandKindError || !strings.Contains(idle.Text, "response in progress") {
		t.Fatalf("idle steer=%#v", idle)
	}
	active := DispatchCommand(context.Background(), "/steer   focus on tests first  ", CommandContext{RunActive: true})
	if !active.Handled || active.Name != CommandSteer || active.Kind != commandKindSteer || active.Steer == nil || active.Steer.Prompt != "focus on tests first" {
		t.Fatalf("active steer=%#v", active)
	}
}

func TestLineTUIResumeSwitchesTheNextProviderTurn(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(SessionMetadata{Title: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AppendMessage(Message{Role: RoleUser, Content: "earlier turn"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(SessionMetadata{Title: "Current"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	input := strings.NewReader("/resume " + string(target.ID)[:12] + "\nafter resume\n/exit\n")
	if err := RunTUIWithSessionAndModel(context.Background(), app, input, &output, current.ID, ""); err != nil {
		t.Fatal(err)
	}
	if provider.count() != 1 {
		t.Fatalf("provider calls=%d want=1", provider.count())
	}
	reloadedTarget, err := store.Load(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetMessages, err := reloadedTarget.Messages()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range targetMessages {
		if message.Role == RoleUser && message.Content == "after resume" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resumed session messages=%#v", targetMessages)
	}
	currentMessages, err := current.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(currentMessages) != 0 {
		t.Fatalf("original current session was mutated: %#v", currentMessages)
	}
}

func TestLineTUINewSwitchesTheNextProviderTurn(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(SessionMetadata{Title: "Current"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := RunTUIWithSessionAndModel(context.Background(), app, strings.NewReader("/new\nfresh request\n/exit\n"), &output, current.ID, ""); err != nil {
		t.Fatal(err)
	}
	if provider.count() != 1 {
		t.Fatalf("provider calls=%d want=1", provider.count())
	}
	currentMessages, err := current.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(currentMessages) != 0 {
		t.Fatalf("original current session was mutated: %#v", currentMessages)
	}
	summaries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("sessions=%#v", summaries)
	}
	for _, summary := range summaries {
		if summary.ID == current.ID {
			continue
		}
		fresh, loadErr := store.Load(summary.ID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		messages, messagesErr := fresh.Messages()
		if messagesErr != nil {
			t.Fatal(messagesErr)
		}
		if len(messages) < 1 || messages[0].Role != RoleUser || messages[0].Content != "fresh request" {
			t.Fatalf("fresh session messages=%#v", messages)
		}
	}
}

func TestDispatchCommandMediaRequiresURLAndPreservesNormalChat(t *testing.T) {
	for _, name := range []CommandName{CommandVideo, CommandImage} {
		result := DispatchCommand(context.Background(), "/"+string(name), CommandContext{})
		if !result.Handled || result.Kind != commandKindError || result.Media != nil {
			t.Fatalf("missing URL result for %s = %#v", name, result)
		}
	}
	result := DispatchCommand(context.Background(), "/video https://example.com/a?x=1", CommandContext{MediaEnabled: true})
	if result.Media == nil || result.Media.URL != "https://example.com/a?x=1" || result.Media.Kind != CommandVideo {
		t.Fatalf("media command = %#v", result)
	}
	if ordinary := DispatchCommand(context.Background(), "please show me a video", CommandContext{}); ordinary.Handled {
		t.Fatalf("ordinary chat was intercepted: %#v", ordinary)
	}
	if gated := DispatchCommand(context.Background(), "/image https://example.com/a.png", CommandContext{}); gated.Kind != commandKindError || !strings.Contains(gated.Text, "not enabled") {
		t.Fatalf("media command was not gated for non-web interface: %#v", gated)
	}
}

func TestDispatchCommandSubagentsAndTasksStayLocal(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "task session"})
	if err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "plan-1", Goal: "ship", Inputs: []string{"repo"}, Outputs: []string{"change"}, Steps: []string{"test"}, Acceptance: []string{"green"}, Tasks: []ScopedTask{{ID: "task-1", Title: "test", Scope: ExecutionScope{Root: t.TempDir()}, Checklist: []ChecklistItem{{ID: "check-1", Text: "run tests", Done: true}}}}}
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	result := DispatchCommand(context.Background(), "/subagents", CommandContext{Application: app})
	if !result.Handled || result.Name != CommandSubagents || result.Subagents == nil || provider.count() != 0 {
		t.Fatalf("subagents result = %#v provider calls=%d", result, provider.count())
	}
	result = DispatchCommand(context.Background(), "/tasks", CommandContext{Application: app, SessionID: session.ID})
	if !result.Handled || result.Name != CommandTasks || result.Tasks == nil || len(result.Tasks.Tasks) != 1 || result.Tasks.Tasks[0].Status != "done" {
		t.Fatalf("tasks result = %#v", result)
	}
}

func TestDispatchCommandParsesVoiceModesAndPreservesLiteralText(t *testing.T) {
	for _, test := range []struct {
		prompt   string
		name     CommandName
		generate bool
		mode     string
		text     string
	}{
		{prompt: "/speak explain this briefly", name: CommandSpeak, generate: true, mode: voiceTTSModeDirection, text: "explain this briefly"},
		{prompt: "/say  literal   spacing  ", name: CommandSay, mode: voiceTTSModeClone, text: "literal   spacing"},
		{prompt: "/say say this", name: CommandSay, mode: voiceTTSModeClone, text: "say this"},
	} {
		result := DispatchCommand(context.Background(), test.prompt, CommandContext{})
		if !result.Handled || result.Name != test.name || result.Kind != commandKindVoice || result.Voice == nil {
			t.Fatalf("result = %#v", result)
		}
		if result.Voice.Generate != test.generate || result.Voice.TTSMode != test.mode || result.Voice.Text != test.text {
			t.Fatalf("voice = %#v, want generate=%t mode=%q text=%q", result.Voice, test.generate, test.mode, test.text)
		}
	}
	for _, prompt := range []string{"/speak", "/say"} {
		result := DispatchCommand(context.Background(), prompt, CommandContext{})
		if !result.Handled || result.Kind != commandKindError || result.Voice != nil {
			t.Fatalf("missing-argument result for %q = %#v", prompt, result)
		}
	}
}

func TestApplicationLocalCommandsDoNotCallProvider(t *testing.T) {
	provider := &commandCountingProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "command session"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "\n/status\n", SessionID: session.ID, Model: "status-model"})
	if err != nil {
		t.Fatal(err)
	}
	var local *RunEvent
	var sawEnd bool
	for event := range events {
		if event.Type == RunLocalCommand {
			copy := event
			local = &copy
		}
		if event.Type == RunEnd {
			sawEnd = true
		}
	}
	if provider.count() != 0 {
		t.Fatalf("local command called provider %d times", provider.count())
	}
	if local == nil || local.Command == nil || local.Command.Name != CommandStatus || local.Command.Status == nil {
		t.Fatalf("local status event = %#v", local)
	}
	if local.Command.Status.SessionID != session.ID || local.Command.Status.SessionTitle != "command session" || local.Command.Status.Model != "status-model" {
		t.Fatalf("status payload = %#v", local.Command.Status)
	}
	if !sawEnd {
		t.Fatal("local command did not emit run_end")
	}

	events, err = app.Run(context.Background(), RunRequest{Prompt: "/not-a-command"})
	if err != nil {
		t.Fatal(err)
	}
	var unknown *LocalCommandEvent
	for event := range events {
		if event.Type == RunLocalCommand {
			unknown = event.Command
		}
	}
	if provider.count() != 0 {
		t.Fatalf("unknown command called provider %d times", provider.count())
	}
	if unknown == nil || unknown.Kind != commandKindError || len(unknown.Text) > maxCommandNoticeBytes {
		t.Fatalf("unknown command event = %#v", unknown)
	}
}

func TestCommandStatusIncludesModelReasoningDirectoryAndContextWindowCard(t *testing.T) {
	config := DefaultConfig()
	config.Generation.ReasoningEffort = "high"
	config.Provider.ContextWindowTokens = 131072
	provider := &commandCountingProvider{}
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.ProviderProfile = "main"
	app.Capabilities = AssistantCapabilities{ScopeRoot: "/work/demo", ToolNames: []string{"read_file", "git_diff"}}
	result := DispatchCommand(context.Background(), "/status", CommandContext{Application: app, Model: "override-model"})
	if result.Status == nil {
		t.Fatal("status payload is nil")
	}
	status := result.Status
	if status.Model != "override-model" || status.ReasoningEffort != "high" || status.Directory == "" || status.ContextWindowTokens != 131072 || status.Scope != "/work/demo" || status.ToolCount != 5 || status.Display == nil || !status.Display.ToolCalls || !status.Display.Thinking || status.ToolCallLimit == nil || !status.ToolCallLimit.Enabled || status.ToolCallLimit.Maximum != DefaultMaxToolSteps {
		t.Fatalf("status=%#v", status)
	}
	for _, want := range []string{"╭─ Bee status", "Model", "override-model", "Reasoning", "high", "Directory", "Context window", "131,072 tokens", "Tool-call limit", "on (32 batches/segment)", "Tool calls", "compact", "Thinking", "Scope", "/work/demo"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("status card=%q missing %q", result.Text, want)
		}
	}
	if provider.count() != 0 {
		t.Fatalf("status called provider %d times", provider.count())
	}
}

func TestCommandStatusRetainsLatestPerSessionUsageAndContext(t *testing.T) {
	app := &Application{Config: DefaultConfig()}
	sessionID := SessionID("session-context")
	runID := "run-context"
	app.observeCommandStatusEvent(RunEvent{Type: RunStart, SessionID: sessionID, RunID: runID})
	contextUsage := RequestContextUsage{WindowTokens: 131072, WindowKnown: true, PromptBudget: 122880, PromptUsed: 12000, PromptRemaining: 110880}
	app.observeCommandStatusEvent(RunEvent{Type: RunContextUsage, SessionID: sessionID, RunID: runID, ContextUsage: &contextUsage})
	sources := make([]ContextSourceDiagnostics, MaxContextStatusSources+4)
	for index := range sources {
		sources[index] = ContextSourceDiagnostics{Source: ContextSource("source"), Included: 1, Bytes: index + 1}
	}
	diagnostics := ContextDiagnostics{Sources: sources, IncludedItems: len(sources), DroppedItems: 2, IncludedBytes: 500}
	app.observeCommandStatusEvent(RunEvent{Type: RunContextDiagnostics, SessionID: sessionID, RunID: runID, ContextDiagnostics: &diagnostics})
	first := Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
	second := Usage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60}
	app.observeCommandStatusEvent(RunEvent{Type: RunUsage, SessionID: sessionID, RunID: runID, Usage: &UsageEvent{Usage: first}})
	app.observeCommandStatusEvent(RunEvent{Type: RunUsage, SessionID: sessionID, RunID: runID, Usage: &UsageEvent{Usage: second}})

	result := DispatchCommand(context.Background(), "/status", CommandContext{Application: app, SessionID: sessionID, Model: "local"})
	if result.Status == nil || result.Status.RequestContext == nil || result.Status.ContextDiagnostics == nil || result.Status.RecentUsage == nil {
		t.Fatalf("status=%#v", result.Status)
	}
	status := result.Status
	if status.ContextWindowTokens != 131072 || status.RequestContext.PromptUsed != 12000 || status.RecentProviderCalls != 2 || status.RecentUsage.TotalTokens != 180 || len(status.ContextDiagnostics.Sources) != MaxContextStatusSources {
		t.Fatalf("status=%#v", status)
	}
	for _, want := range []string{"12,000 used", "110,880 remaining", "10% of prompt budget", "2 calls · 180 tokens", "Injected context", "20 included · 2 dropped · 500 bytes"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("status card=%q missing %q", result.Text, want)
		}
	}

	other := DispatchCommand(context.Background(), "/status", CommandContext{Application: app, SessionID: "other-session"})
	if other.Status == nil || other.Status.RequestContext != nil || other.Status.RecentUsage != nil || !strings.Contains(other.Text, "no request snapshot recorded") {
		t.Fatalf("other session status=%#v text=%q", other.Status, other.Text)
	}
}

func TestCommandStatusReportsContentFreeMemoryExtractionOutcome(t *testing.T) {
	app := &Application{Config: DefaultConfig()}
	sessionID := SessionID("session-memory")
	app.observeCommandStatusEvent(RunEvent{Type: RunStart, SessionID: sessionID, RunID: "run-memory"})
	app.observeCommandStatusEvent(RunEvent{Type: RunMemoryExtraction, SessionID: sessionID, RunID: "run-memory", MemoryExtraction: &MemoryExtractionEvent{Attempted: true, Saved: []string{"secret-id"}, Unchanged: []string{"same-id"}, Proposed: []string{"proposal-id"}, Rejected: 2}})
	result := DispatchCommand(context.Background(), "/status", CommandContext{Application: app, SessionID: sessionID})
	if result.Status == nil || result.Status.MemoryExtraction == nil {
		t.Fatalf("status=%#v", result.Status)
	}
	status := result.Status.MemoryExtraction
	if !status.Attempted || status.Saved != 1 || status.Unchanged != 1 || status.Proposed != 1 || status.Rejected != 2 {
		t.Fatalf("memory extraction=%#v", status)
	}
	if !strings.Contains(result.Text, "1 saved · 1 unchanged · 1 proposed · 2 rejected") || strings.Contains(result.Text, "secret-id") {
		t.Fatalf("status card=%q", result.Text)
	}
}

func TestCommandStatusShowsRequestEstimateWhenWindowIsUnknown(t *testing.T) {
	app := &Application{Config: DefaultConfig()}
	usage := RequestContextUsage{PromptUsed: 12345}
	app.observeCommandStatusEvent(RunEvent{Type: RunContextUsage, RunID: "run-1", ContextUsage: &usage})
	result := DispatchCommand(context.Background(), "/status", CommandContext{Application: app})
	if result.Status == nil || result.Status.RequestContext == nil {
		t.Fatalf("status=%#v", result.Status)
	}
	for _, want := range []string{"unknown (not configured)", "~12,345 used · window unknown"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("status card=%q missing %q", result.Text, want)
		}
	}
}
