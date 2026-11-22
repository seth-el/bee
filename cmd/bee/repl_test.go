package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

type markedTerminalBuffer struct {
	bytes.Buffer
}

func (markedTerminalBuffer) IsTerminal() bool { return true }

func TestOrdinaryREPLIncludesUnifiedAssistantTools(t *testing.T) {
	provider := &modelCaptureProvider{}
	config := bee.DefaultConfig()
	app, err := bee.ComposeGeneralAssistant(config, "", bee.AssistantCompositionOptions{Store: mustTestSessionStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	app.Provider = provider
	memory := &replMemoryCaptureService{}
	app.MemoryService = memory
	if err := repl(app, strings.NewReader("hello\n/exit\n"), &bytes.Buffer{}, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Tools) == 0 {
		t.Fatalf("ordinary request tools=%#v", provider.requests)
	}
	foundPrompt := false
	for _, message := range provider.requests[0].Messages {
		if strings.HasPrefix(message.Content, bee.DefaultSystemPrompt) {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatalf("ordinary request system prompt=%#v", provider.requests[0].Messages)
	}
	if memory.searches != 0 {
		t.Fatalf("ordinary REPL unexpectedly searched memory: %d", memory.searches)
	}
	for _, message := range provider.requests[0].Messages {
		if strings.Contains(message.Content, "repl memory") {
			t.Fatalf("ordinary REPL injected memory: %#v", provider.requests[0].Messages)
		}
	}
}

type replMemoryCaptureService struct {
	bee.MemoryService
	searches int
}

func (service *replMemoryCaptureService) Search(context.Context, bee.SearchMemoryRequest) (bee.MemorySearchResult, error) {
	service.searches++
	return bee.MemorySearchResult{Hits: []bee.MemoryDisplayHit{{Record: bee.MemoryDisplayRecord{Record: bee.MemoryRecord{Content: "repl memory"}}}}}, nil
}

func TestOrdinaryREPLHybridRecallSkipsTrivialPrompt(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.ComposeGeneralAssistant(bee.DefaultConfig(), "", bee.AssistantCompositionOptions{Store: mustTestSessionStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	memory := &replMemoryCaptureService{}
	app.Provider = provider
	app.MemoryService = memory
	if err := repl(app, strings.NewReader("hello\n/exit\n"), &bytes.Buffer{}, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if memory.searches != 0 {
		t.Fatalf("ordinary REPL unexpectedly searched memory: %d", memory.searches)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	for _, message := range provider.requests[0].Messages {
		if strings.Contains(message.Content, "repl memory") {
			t.Fatalf("ordinary REPL injected memory: %#v", provider.requests[0].Messages)
		}
	}
}

func TestREPLContinueSelectsAndConsumesLatestCheckpoint(t *testing.T) {
	provider := &modelCaptureProvider{}
	store := mustTestSessionStore(t)
	session, err := store.Create(bee.SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := session.AppendTurnCheckpoint(bee.TurnCheckpoint{
		ID:         "checkpoint-repl",
		WhyStopped: bee.TurnStopDeadline,
		Summary:    "inspection complete; implementation remains",
		NextAction: "implement the pending change",
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := bee.ComposeGeneralAssistant(bee.DefaultConfig(), "", bee.AssistantCompositionOptions{Store: store, ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	app.Provider = provider
	if err := repl(app, strings.NewReader("/continue\n/exit\n"), &bytes.Buffer{}, bee.RunRequest{SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	foundCheckpoint := false
	for _, message := range provider.requests[0].Messages {
		if strings.Contains(message.Content, "<turn_checkpoint>") && strings.Contains(message.Content, checkpoint.ID) {
			foundCheckpoint = true
			break
		}
	}
	if !foundCheckpoint {
		t.Fatalf("continued request omitted checkpoint: %#v", provider.requests[0].Messages)
	}
	fresh, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.LatestPendingTurnCheckpoint(); err == nil || !strings.Contains(err.Error(), "no pending") {
		t.Fatalf("continued checkpoint remained pending: %v", err)
	}
}

func TestOrdinaryREPLExposesExecSchema(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.ComposeGeneralAssistant(bee.DefaultConfig(), "", bee.AssistantCompositionOptions{Store: mustTestSessionStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	app.Provider = provider
	if err := repl(app, strings.NewReader("inspect a host\n/exit\n"), &bytes.Buffer{}, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	var execTool *bee.ToolDefinition
	for i := range provider.requests[0].Tools {
		if provider.requests[0].Tools[i].Name == bee.ExecToolName {
			execTool = &provider.requests[0].Tools[i]
			break
		}
	}
	if execTool == nil {
		t.Fatal("exec schema missing")
	}
	if !strings.Contains(execTool.Description, "OpenSSH") || !strings.Contains(execTool.Description, "Do not use run_command target:ssh") {
		t.Fatalf("exec schema guidance=%q", execTool.Description)
	}
	for _, field := range []string{"command", "workdir", "env", "timeout_ms", "pty", "background", "yield_ms"} {
		if !strings.Contains(string(execTool.Parameters), `"`+field+`"`) {
			t.Fatalf("exec schema=%s missing %q", execTool.Parameters, field)
		}
	}
}

func mustTestSessionStore(t *testing.T) *bee.SessionStore {
	t.Helper()
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestInteractiveREPLUsesPlainLabelsAndBlankLineSpacingForBufferedOutput(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := repl(app, strings.NewReader("hello\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "you> \n\nbee> ok\n\nyou> "; got != want {
		t.Fatalf("output=%q, want %q", got, want)
	}
	if strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("buffered output unexpectedly contains ANSI escapes: %q", output.String())
	}
}

func TestInteractiveREPLUsesANSILabelsForTerminalOutput(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output markedTerminalBuffer
	if err := repl(app, strings.NewReader("hello\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"\x1b[36myou>\x1b[0m ", "\x1b[32mbee>\x1b[0m ", "\n\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output=%q missing %q", got, want)
		}
	}
}

func TestTerminalREPLStatusShowsBoundedOperatorStateWithoutRunningProvider(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.ProviderProfile = "cloud"
	app.Capabilities = bee.AssistantCapabilities{ScopeRoot: "/tmp/bee-scope", ToolNames: []string{"read_file", "write_file", "read_file"}}
	var output markedTerminalBuffer
	if err := repl(app, strings.NewReader("/status\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"Bee status",
		"Session",
		"<none>",
		"Provider",
		"openai-compatible",
		"Profile",
		"cloud",
		"Model",
		"local",
		"Reasoning",
		"provider default",
		"Directory",
		"Context window",
		"Scope",
		"/tmp/bee-scope",
		"Tools",
		"2",
		"Recent usage",
		"none recorded",
		"Current context",
		"no request snapshot recorded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output=%q missing %q", got, want)
		}
	}
	if len(provider.requests) != 0 {
		t.Fatalf("status unexpectedly ran provider: %#v", provider.requests)
	}
}

func TestTerminalREPLStatusReflectsCompletedRunUsageAndSession(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, mustTestSessionStore(t))
	if err != nil {
		t.Fatal(err)
	}
	var output markedTerminalBuffer
	if err := repl(app, strings.NewReader("hello\n/status\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "│ Session ") && !strings.HasPrefix(line, "│ Session title") && strings.Contains(line, "<none>") {
			t.Fatalf("status lost durable session: %q", got)
		}
	}
	if !strings.Contains(got, "1 call · 5 tokens (3 in / 2 out)") {
		t.Fatalf("status did not show recent usage: %q", got)
	}
}

func TestRedirectedREPLStatusAndActivityStayMachineClean(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := repl(app, strings.NewReader("/status\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "you> you> " {
		t.Fatalf("redirected /status output=%q, want prompts only", got)
	}
	activity := newTerminalActivity(&output, nil)
	activity.Start("tool\nwith args")
	activity.Stop()
	if strings.Contains(output.String(), "tool") || strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("redirected activity polluted output: %q", output.String())
	}
}

func TestREPLStatusTracksUsageAndBoundedContextDiagnostics(t *testing.T) {
	status := newREPLStatus(nil, "session-1", "model-1")
	status.profile = "profile-1"
	status.observe(bee.RunEvent{Type: bee.RunStart})
	status.observe(bee.RunEvent{Type: bee.RunUsage, Usage: &bee.UsageEvent{Usage: bee.Usage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10}}})
	sources := make([]bee.ContextSourceDiagnostics, 20)
	for index := range sources {
		sources[index].Source = bee.ContextSource("source")
	}
	diagnostics := bee.ContextDiagnostics{Sources: sources, IncludedItems: 3, DroppedItems: 1, IncludedBytes: 42}
	status.observe(bee.RunEvent{Type: bee.RunContextDiagnostics, ContextDiagnostics: &diagnostics})
	var output bytes.Buffer
	if err := status.render(&output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"1 call · 10 tokens (4 in / 6 out)", "3 included · 1 dropped · 42 bytes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status=%q missing %q", got, want)
		}
	}
	if count := strings.Count(got, "│   source"); count != 16 {
		t.Fatalf("rendered %d context sources, want bounded 16: %q", count, got)
	}
}

func TestBoundedActivityLabelRemovesControlsAndCapsLength(t *testing.T) {
	got := boundedActivityLabel("\x1b[31m" + strings.Repeat("x", 80) + "\n")
	if strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("activity label retained control characters: %q", got)
	}
	if len(got) != 51 || !strings.HasSuffix(got, "...") {
		t.Fatalf("activity label length/content=%q", got)
	}
}
