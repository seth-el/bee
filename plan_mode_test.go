package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testPlanModeApplication(t *testing.T, store *SessionStore, requirePlan bool) *Application {
	t.Helper()
	config := DefaultConfig()
	config.Assistant.RequirePlan = requirePlan
	registry := mustTestRegistry(t)
	return &Application{
		Config:       config,
		Provider:     &fakeProvider{},
		ProviderRole: ProviderRoleMain,
		Store:        store,
		Executor:     &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}},
		RequirePlan:  requirePlan,
	}
}

func validPlanModeTestPlan(id string) TaskPlan {
	return TaskPlan{ID: id, Goal: "make a bounded change", Inputs: []string{"request"}, Outputs: []string{"result"}, Steps: []string{"change", "verify"}, Acceptance: []string{"verified"}}
}

func TestPlanModePersistsAndReenableStartsFresh(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "plan mode"})
	if err != nil {
		t.Fatal(err)
	}
	app := testPlanModeApplication(t, store, false)
	if changed, err := app.SetPlanMode(session.ID, false); err != nil || changed {
		t.Fatalf("initialize off changed=%t err=%v", changed, err)
	}
	if err := session.AppendPlan(validPlanModeTestPlan("old-plan")); err != nil {
		t.Fatal(err)
	}
	if changed, err := app.SetPlanMode(session.ID, true); err != nil || !changed {
		t.Fatalf("enable changed=%t err=%v", changed, err)
	}
	reloaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := latestSessionPlan(reloaded.EventsSnapshot()); err != nil || plan != nil {
		t.Fatalf("stale plan survived enable: plan=%#v err=%v", plan, err)
	}
	state, err := app.PlanMode(session.ID)
	if err != nil || !state.Enabled || state.Source != "session" {
		t.Fatalf("enabled state=%#v err=%v", state, err)
	}
	if err := reloaded.AppendPlan(validPlanModeTestPlan("current-plan")); err != nil {
		t.Fatal(err)
	}
	if changed, err := app.SetPlanMode(session.ID, false); err != nil || !changed {
		t.Fatalf("disable changed=%t err=%v", changed, err)
	}
	reloaded, err = store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := latestSessionPlan(reloaded.EventsSnapshot()); err != nil || plan == nil || plan.ID != "current-plan" {
		t.Fatalf("disable did not preserve plan history: plan=%#v err=%v", plan, err)
	}
	resumed := testPlanModeApplication(t, store, true)
	state, err = resumed.PlanMode(session.ID)
	if err != nil || state.Enabled {
		t.Fatalf("persisted session override lost: state=%#v err=%v", state, err)
	}
}

func TestPrepareSessionPlanModePinsDefaultAndAppliesQueuedOverride(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	app := testPlanModeApplication(t, store, false)
	if changed, err := app.SetPlanMode("", true); err != nil || !changed {
		t.Fatalf("queue enable changed=%t err=%v", changed, err)
	}
	enabled, err := app.prepareSessionPlanMode(session)
	if err != nil || !enabled {
		t.Fatalf("prepare enabled=%t err=%v", enabled, err)
	}
	reloaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	mode, explicit, err := latestSessionPlanMode(reloaded.EventsSnapshot(), false)
	if err != nil || !mode || !explicit {
		t.Fatalf("persisted mode=%t explicit=%t err=%v", mode, explicit, err)
	}
	state, err := app.PlanMode("")
	if err != nil || state.Enabled || state.Source != "configuration" {
		t.Fatalf("queued override was not consumed: state=%#v err=%v", state, err)
	}
}

func TestAgentOptionsUseEffectiveSessionPlanMode(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	app := testPlanModeApplication(t, store, false)
	if err := session.AppendPlanMode(false, "test"); err != nil {
		t.Fatal(err)
	}
	options := app.agentOptions(nil, session, "")
	if options.RequirePlan || !options.ExposePlanTracking || options.Executor.planGate != nil {
		t.Fatalf("disabled options=%#v gate=%#v", options, options.Executor.planGate)
	}
	if _, err := app.SetPlanMode(session.ID, true); err != nil {
		t.Fatal(err)
	}
	session, err = store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	options = app.agentOptions(nil, session, "")
	if !options.RequirePlan || !options.ExposePlanTracking || options.Executor.planGate == nil {
		t.Fatalf("enabled options=%#v gate=%#v", options, options.Executor.planGate)
	}
}

func TestPlanModeCommandsStayLocal(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	app := testPlanModeApplication(t, store, false)
	result := DispatchCommand(context.Background(), "/plan", CommandContext{Application: app, SessionID: session.ID})
	if result.Kind != commandKindPlan || result.PlanMode == nil || result.PlanMode.Enabled {
		t.Fatalf("status result=%#v", result)
	}
	result = DispatchCommand(context.Background(), "/plan on", CommandContext{Application: app, SessionID: session.ID})
	if result.Kind != commandKindPlan || result.PlanMode == nil || !result.PlanMode.Enabled {
		t.Fatalf("enable result=%#v", result)
	}
	if invalid := DispatchCommand(context.Background(), "/plan maybe", CommandContext{Application: app, SessionID: session.ID}); invalid.Kind != commandKindError {
		t.Fatalf("invalid result=%#v", invalid)
	}
}

func TestPlanCommandReportsAndResetsActivePlan(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	app := testPlanModeApplication(t, store, true)
	if err := session.AppendPlanMode(true, "test"); err != nil {
		t.Fatal(err)
	}
	plan := validPlanModeTestPlan("active-plan")
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	status := DispatchCommand(context.Background(), "/plan status", CommandContext{Application: app, SessionID: session.ID})
	if status.PlanMode == nil || !status.PlanMode.ActivePlan || status.PlanMode.PlanID != plan.ID || !strings.Contains(status.Text, plan.Goal) {
		t.Fatalf("status=%#v", status)
	}
	reset := DispatchCommand(context.Background(), "/plan reset new task", CommandContext{Application: app, SessionID: session.ID})
	if reset.PlanMode == nil || reset.PlanMode.ActivePlan || !strings.Contains(reset.Text, "fresh plan") {
		t.Fatalf("reset=%#v", reset)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current := latestPlanFromEvents(loaded.EventsSnapshot()); current != nil {
		t.Fatalf("reset retained active plan=%#v", current)
	}
}

func TestPlanResetAppendsSupersedingControlRecord(t *testing.T) {
	sessionID := SessionID("plan-context")
	plan, err := NewPlanEvent(sessionID, validPlanModeTestPlan("stale-plan"))
	if err != nil {
		t.Fatal(err)
	}
	plan.Seq = 1
	reset, err := NewPlanResetEvent(sessionID, "new epoch")
	if err != nil {
		t.Fatal(err)
	}
	reset.Seq = 2
	items, err := contextItemsFromEvents([]Event{plan, reset})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Source != ContextSourcePlan || !strings.Contains(items[0].Message.Content, `"kind":"plan_reset"`) {
		t.Fatalf("append-only reset projection=%#v", items)
	}
}

func TestPlanModeTogglePreservesDurableRequestPrefix(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendPlanMode(false, "initial"); err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{Role: RoleUser, Content: "first"},
		{Role: RoleAssistant, Content: "first answer"},
		{Role: RoleUser, Content: "second"},
	} {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultConfig()
	executor := &Executor{Registry: mustTestRegistry(t), Scope: ExecutionScope{Root: t.TempDir()}}
	off, err := NewAgentWithOptions(config, &fakeProvider{}, AgentOptions{Executor: executor, ExposePlanTracking: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	offRequest, err := off.request()
	if err != nil {
		t.Fatal(err)
	}
	offFingerprint, _, offPrefix := stableRequestPrefixFingerprint(offRequest)

	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "second answer"}); err != nil {
		t.Fatal(err)
	}
	if err := session.EnablePlanMode("enable for next task"); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "third"}); err != nil {
		t.Fatal(err)
	}
	on, err := NewAgentWithOptions(config, &fakeProvider{}, AgentOptions{Executor: executor, RequirePlan: true, ExposePlanTracking: true, Session: session, PriorPrefixFingerprint: offFingerprint, PriorPrefixBytes: len(offPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	onRequest, err := on.request()
	if err != nil {
		t.Fatal(err)
	}
	_, _, onPrefix := stableRequestPrefixFingerprint(onRequest)
	if !bytes.HasPrefix(onPrefix, offPrefix) {
		t.Fatalf("plan-mode transition rewrote the durable prefix:\noff=%s\non=%s", offPrefix, onPrefix)
	}
	on.observeProviderRequest(onRequest)
	if on.lastContextUsage.PrefixChanged {
		t.Fatalf("cross-agent cache telemetry reported a break: %#v", on.lastContextUsage)
	}
	offTools, _ := json.Marshal(offRequest.Tools)
	onTools, _ := json.Marshal(onRequest.Tools)
	if !bytes.Equal(offTools, onTools) {
		t.Fatalf("plan-mode transition changed tool schemas:\noff=%s\non=%s", offTools, onTools)
	}
}

func TestApplicationPlanModeToggleKeepsProviderCachePrefix(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, Content: "first answer"}},
		{Message: Message{Role: RoleAssistant, Content: "second answer"}},
	}}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = &Executor{Registry: mustTestRegistry(t), Scope: ExecutionScope{Root: t.TempDir()}}

	first, err := app.Run(context.Background(), RunRequest{Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	var sessionID SessionID
	for event := range first {
		if event.SessionID != "" {
			sessionID = event.SessionID
		}
	}
	if sessionID == "" {
		t.Fatal("first run did not report a durable session")
	}
	if changed, err := app.SetPlanMode(sessionID, true); err != nil || !changed {
		t.Fatalf("enable plan mode: changed=%t err=%v", changed, err)
	}
	second, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	var secondUsage *RequestContextUsage
	for event := range second {
		if event.ContextUsage != nil {
			copy := *event.ContextUsage
			secondUsage = &copy
		}
	}
	if secondUsage == nil || secondUsage.PrefixChanged {
		t.Fatalf("plan-mode transition cache telemetry=%#v", secondUsage)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	_, _, firstPrefix := stableRequestPrefixFingerprint(provider.requests[0])
	_, _, secondPrefix := stableRequestPrefixFingerprint(provider.requests[1])
	if !bytes.HasPrefix(secondPrefix, firstPrefix) {
		t.Fatalf("runtime plan-mode transition rewrote provider prefix:\nfirst=%s\nsecond=%s", firstPrefix, secondPrefix)
	}
}

func TestPlanAcceptanceExtendsTranscriptWithoutRewritingPrefix(t *testing.T) {
	config := DefaultConfig()
	agent, err := NewAgentWithOptions(config, &fakeProvider{}, AgentOptions{
		Executor:    &Executor{Registry: mustTestRegistry(t), Scope: ExecutionScope{Root: t.TempDir()}},
		RequirePlan: true, ExposePlanTracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.messages = append(agent.messages, Message{Role: RoleUser, Content: "make a change"})
	before, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	_, _, beforePrefix := stableRequestPrefixFingerprint(before)
	leadingMessages := append([]Message(nil), agent.messages...)

	plan := validPlanModeTestPlan("append-only-plan")
	arguments, _ := json.Marshal(plan)
	call := ToolCall{ID: "plan-call", Name: PlanToolName, Arguments: string(arguments)}
	result := agent.acceptPlan(arguments)
	if result.Error != "" || !strings.Contains(result.Output, taskPlanFingerprint(*agent.plan)) {
		t.Fatalf("plan result=%#v active=%#v", result, agent.plan)
	}
	if len(agent.messages) != len(leadingMessages) {
		t.Fatalf("accepting a plan inserted provider messages: before=%#v after=%#v", leadingMessages, agent.messages)
	}
	agent.messages = append(agent.messages,
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{call}},
		Message{Role: RoleTool, Name: PlanToolName, ToolCallID: call.ID, Content: result.Output},
	)
	after, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	_, _, afterPrefix := stableRequestPrefixFingerprint(after)
	if !bytes.Equal(afterPrefix, beforePrefix) {
		t.Fatalf("same-turn plan acceptance changed the prior-turn prefix:\nbefore=%s\nafter=%s", beforePrefix, afterPrefix)
	}
	agent.messages = append(agent.messages, Message{Role: RoleAssistant, Content: "done"}, Message{Role: RoleUser, Content: "next"})
	next, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	_, _, nextPrefix := stableRequestPrefixFingerprint(next)
	if !bytes.HasPrefix(nextPrefix, beforePrefix) {
		t.Fatalf("plan transcript did not grow from the old prefix:\nbefore=%s\nnext=%s", beforePrefix, nextPrefix)
	}
}

func TestPlanEventDoesNotSplitReplayedToolGroup(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	plan := validPlanModeTestPlan("replayed-plan")
	arguments, _ := json.Marshal(plan)
	call := ToolCall{ID: "plan-call", Name: PlanToolName, Arguments: string(arguments)}
	if err := session.AppendMessage(Message{Role: RoleAssistant, ToolCalls: []ToolCall{call}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(call); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	output := canonicalPlanToolOutput("accepted", plan, 3, "accepted")
	if err := session.AppendMessage(Message{Role: RoleTool, Name: PlanToolName, ToolCallID: call.ID, Content: output}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: call.ID, ToolName: PlanToolName, Output: output}); err != nil {
		t.Fatal(err)
	}
	result, err := AssembleContext(session.EventsSnapshot(), ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[0].Role != RoleAssistant || result.Messages[1].Role != RoleTool || result.Messages[1].ToolCallID != call.ID {
		t.Fatalf("replayed plan tool group=%#v", result.Messages)
	}
}
