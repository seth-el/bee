package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testScope(t *testing.T) ExecutionScope {
	t.Helper()
	return ExecutionScope{Root: t.TempDir(), TimeoutSeconds: 2, MaxOutputBytes: 32}
}

type timeoutProbeTool struct {
	wait    time.Duration
	timeout time.Duration
}

func (tool timeoutProbeTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "timeout_probe"}
}

func (tool timeoutProbeTool) ExecutionTimeout() time.Duration { return tool.timeout }

func (tool timeoutProbeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	timer := time.NewTimer(tool.wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return "completed", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestRegistryDefinitionsAreSortedAndRejectDuplicates(t *testing.T) {
	registry, err := NewRegistry(
		ToolFunc{ToolDefinition: ToolDefinition{Name: "z", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }},
		ToolFunc{ToolDefinition: ToolDefinition{Name: "a", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 2 || definitions[0].Name != "a" || definitions[1].Name != "z" {
		t.Fatalf("definitions = %#v", definitions)
	}
	if err := registry.Register(ToolFunc{ToolDefinition: ToolDefinition{Name: "a"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }}); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestExecutorPlanGateProtectsMutationsButAllowsReads(t *testing.T) {
	scope := testScope(t)
	called := false
	registry, err := NewRegistry(
		ToolFunc{ToolDefinition: ToolDefinition{Name: "read", Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) { return "read", nil }},
		ToolFunc{ToolDefinition: ToolDefinition{Name: "mutate", Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) { called = true; return "mutated", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Registry: registry, Scope: scope, planGate: &PlanGate{}}
	if result, err := executor.Execute(context.Background(), "read", json.RawMessage(`{}`)); err != nil || result.Output != "read" {
		t.Fatalf("read result=%#v err=%v", result, err)
	}
	if _, err := executor.Execute(context.Background(), "mutate", json.RawMessage(`{}`)); !errors.Is(err, ErrPlanRequired) || called {
		t.Fatalf("mutation err=%v called=%v", err, called)
	}
	if err := executor.SetPlan(TaskPlan{ID: "p", Goal: "mutate", Inputs: []string{"root"}, Outputs: []string{"file"}, Steps: []string{"mutate"}, Acceptance: []string{"done"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), "mutate", json.RawMessage(`{}`)); err != nil || !called {
		t.Fatalf("planned mutation err=%v called=%v", err, called)
	}
}

func TestExecutorApprovalAndAudit(t *testing.T) {
	scope := testScope(t)
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "write", Description: "write", Risk: RiskHigh}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	approved := false
	audited := false
	executor := Executor{Registry: registry, Scope: scope, Policy: DefaultCodingPolicy{}, Approval: ApprovalFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
		approved = request.ToolName == "write"
		return ApprovalApproved, nil
	}), Auditor: AuditorFunc(func(_ context.Context, event AuditEvent) error { audited = event.Success; return nil })}
	result, err := executor.Execute(context.Background(), "write", json.RawMessage(`{}`))
	if err != nil || result.Output != "ok" || !approved || !audited {
		t.Fatalf("result=%#v err=%v approved=%v audited=%v", result, err, approved, audited)
	}
	noApproval := executor
	noApproval.Approval = nil
	if _, err := noApproval.Execute(context.Background(), "write", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("err=%v", err)
	}
}

func TestExecutorAutoApprovalSkipsPromptAndSuppliesExplicitApproval(t *testing.T) {
	scope := testScope(t)
	prompted := 0
	explicit := false
	approvalEvents := 0
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "write", Description: "write", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
		explicit = hasExplicitToolApproval(ctx)
		return "ok", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{
		Registry:     registry,
		Scope:        scope,
		Policy:       DefaultCodingPolicy{},
		ApprovalMode: ApprovalModeAuto,
		Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
			prompted++
			return ApprovalDenied, nil
		}),
		EventHook: func(ExecutorEvent) { approvalEvents++ },
	}
	result, err := executor.Execute(context.Background(), "write", json.RawMessage(`{"path":"note.txt","content":"private"}`))
	if err != nil || result.Output != "ok" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if prompted != 0 {
		t.Fatalf("approval hook calls=%d, want 0", prompted)
	}
	if !explicit {
		t.Fatal("auto-approved tool did not receive explicit-approval context")
	}
	if approvalEvents != 0 {
		t.Fatalf("auto approval emitted %d prompt events", approvalEvents)
	}
}

func TestExecutorAutoApprovalKeepsPackageReviewManual(t *testing.T) {
	scope := testScope(t)
	tools := []Tool{
		SkillTestTool{Scope: scope},
		PluginTestTool{Scope: scope},
		SkillEnableTool{Scope: scope},
		PluginEnableTool{Scope: scope},
	}
	for _, tool := range tools {
		tool := tool
		t.Run(tool.Definition().Name, func(t *testing.T) {
			registry, err := NewRegistry(tool)
			if err != nil {
				t.Fatal(err)
			}
			prompted := 0
			executor := Executor{
				Registry:     registry,
				Scope:        scope,
				Policy:       GeneralAssistantPolicy{Mode: AssistantPolicyTrusted},
				ApprovalMode: ApprovalModeAuto,
				Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
					prompted++
					return ApprovalDenied, nil
				}),
			}
			if _, err := executor.Execute(context.Background(), tool.Definition().Name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
				t.Fatalf("error=%v", err)
			}
			if prompted != 1 {
				t.Fatalf("approval hook calls=%d, want 1", prompted)
			}
		})
	}
}

func TestAutoApprovedWriteHasDurableBoundedAuditAndMutationJournal(t *testing.T) {
	root := t.TempDir()
	scope := ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true, TimeoutSeconds: 2, MaxOutputBytes: 4096}
	journal, err := NewMutationJournal(filepath.Join(t.TempDir(), "mutations"), root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "auto approval audit"})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(WriteFileTool{Scope: scope, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{
		Registry:     registry,
		Scope:        scope,
		Policy:       GeneralAssistantPolicy{Mode: AssistantPolicyPrompted},
		ApprovalMode: ApprovalModeAuto,
		Auditor:      sessionAuditor{session: session},
	}
	const privateContent = "operator-private identity prose"
	result, err := executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"SOUL.md","content":"`+privateContent+`"}`))
	if err != nil || !strings.Contains(result.Output, "created") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "SOUL.md"))
	if err != nil || string(contents) != privateContent {
		t.Fatalf("SOUL.md=%q err=%v", contents, err)
	}
	mutations, err := journal.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 1 || mutations[0].Tool != "write_file" || mutations[0].Path != "SOUL.md" {
		t.Fatalf("mutation entries=%#v", mutations)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audits []AuditEvent
	for _, event := range loaded.Events {
		if event.Type != EventAudit {
			continue
		}
		payload, decodeErr := decodeEventPayload[AuditEventPayload](event, EventAudit)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		audits = append(audits, payload.Audit)
	}
	if len(audits) != 1 {
		t.Fatalf("audit count=%d, want 1", len(audits))
	}
	audit := audits[0]
	if !audit.AutoApproved || audit.Approval != ApprovalApproved || audit.ToolName != "write_file" || audit.Target != "path=SOUL.md" || audit.Outcome != AuditExecuted || !audit.Success {
		t.Fatalf("auto-approval audit=%#v", audit)
	}
	if len(audit.Args) != 0 || audit.Error != "" {
		t.Fatalf("durable auto-approval audit retained content-shaped fields: %#v", audit)
	}
	stored, err := os.ReadFile(filepath.Join(store.Root(), string(session.ID)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), privateContent) {
		t.Fatalf("durable auto-approval journal leaked content: %s", stored)
	}
}

func TestAutoApprovalAuditBoundsTargetsAndRedactsFailure(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "failed auto approval"})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "must-not-enter-durable-audit"
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "external_send", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New(secret)
	}})
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Registry: registry, Scope: testScope(t), Policy: DefaultCodingPolicy{}, ApprovalMode: ApprovalModeAuto, Auditor: sessionAuditor{session: session}}
	args := json.RawMessage(`{"url":"https://owner:password@example.test/send?token=secret#fragment","content":"` + secret + `"}`)
	if _, err := executor.Execute(context.Background(), "external_send", args); err == nil || err.Error() != secret {
		t.Fatalf("execution error=%v", err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audit AuditEvent
	for _, event := range loaded.Events {
		if event.Type == EventAudit {
			payload, decodeErr := decodeEventPayload[AuditEventPayload](event, EventAudit)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			audit = payload.Audit
		}
	}
	if !audit.AutoApproved || audit.Success || audit.Error != "tool execution failed" || audit.Target != "url=https://example.test/send" {
		t.Fatalf("failure audit=%#v", audit)
	}
	if len(audit.Target) > maxAuditTargetBytes || len(audit.Args) != 0 {
		t.Fatalf("failure audit is not bounded/content-free: %#v", audit)
	}
	stored, err := os.ReadFile(filepath.Join(store.Root(), string(session.ID)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "password", "token=secret", "fragment"} {
		if strings.Contains(string(stored), forbidden) {
			t.Fatalf("durable auto-approval journal leaked %q: %s", forbidden, stored)
		}
	}
}

func TestExecutorDeniedActionsAreAuditedAndDurable(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "audit"})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "write", Description: "write", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
		called = true
		return "mutated", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	policyDenied := Executor{Registry: registry, Scope: testScope(t), Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyDeny, nil
	}), Auditor: sessionAuditor{session: session}}
	args := json.RawMessage(`{"path":"config.yaml","content":"TOP SECRET","token":"Bearer abc123"}`)
	if _, err := policyDenied.Execute(context.Background(), "write", args); err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("policy denial error=%v", err)
	}
	if called {
		t.Fatal("policy-denied tool executed")
	}
	approvalDenied := Executor{Registry: registry, Scope: testScope(t), Policy: DefaultCodingPolicy{}, Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDenied, nil
	}), Auditor: sessionAuditor{session: session}}
	if _, err := approvalDenied.Execute(context.Background(), "write", args); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
		t.Fatalf("approval denial error=%v", err)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audits []AuditEvent
	for _, event := range loaded.Events {
		if event.Type != EventAudit {
			continue
		}
		payload, decodeErr := decodeEventPayload[AuditEventPayload](event, EventAudit)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		audits = append(audits, payload.Audit)
	}
	if len(audits) != 2 || audits[0].Outcome != AuditPolicyDenied || audits[1].Outcome != AuditApprovalDenied {
		t.Fatalf("audits=%#v", audits)
	}
	stored, err := os.ReadFile(filepath.Join(store.Root(), string(session.ID)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(stored)
	for _, secret := range []string{"TOP SECRET", "Bearer abc123"} {
		if strings.Contains(text, secret) {
			t.Fatalf("audit persisted sensitive argument %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "REDACTED") {
		t.Fatalf("audit redaction marker missing: %s", text)
	}
}

func TestExecutorAuditFailureFailsClosed(t *testing.T) {
	called := false
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "mutate", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
		called = true
		return "mutated", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	auditErr := errors.New("audit store unavailable")
	executor := Executor{Registry: registry, Scope: testScope(t), Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyDeny, nil
	}), Auditor: AuditorFunc(func(context.Context, AuditEvent) error { return auditErr })}
	if _, err := executor.Execute(context.Background(), "mutate", json.RawMessage(`{}`)); err == nil || !errors.Is(err, auditErr) {
		t.Fatalf("error=%v, want audit failure", err)
	}
	if called {
		t.Fatal("tool executed after audit failure")
	}
}

func TestExecutorAllRefusalPathsReachAuditor(t *testing.T) {
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "mutate", Description: "mutate", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("refused tool executed")
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var events []AuditEvent
	auditor := AuditorFunc(func(_ context.Context, event AuditEvent) error {
		events = append(events, event)
		return nil
	})
	executor := Executor{Registry: registry, Scope: testScope(t), Policy: DefaultCodingPolicy{}, Auditor: auditor, planGate: &PlanGate{}}
	if _, err := executor.Execute(context.Background(), "mutate", json.RawMessage(`{"content":"secret"}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("plan gate error=%v", err)
	}
	noApproval := executor
	noApproval.planGate = nil
	if _, err := noApproval.Execute(context.Background(), "mutate", json.RawMessage(`{"content":"secret"}`)); err == nil || !strings.Contains(err.Error(), "no approval hook") {
		t.Fatalf("missing approval error=%v", err)
	}
	approvalError := noApproval
	approvalError.Approval = ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalDenied, errors.New("operator input failed")
	})
	if _, err := approvalError.Execute(context.Background(), "mutate", json.RawMessage(`{"content":"secret"}`)); err == nil || !strings.Contains(err.Error(), "operator input failed") {
		t.Fatalf("approval hook error=%v", err)
	}
	approvalDenied := noApproval
	approvalDenied.Approval = ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalDenied, nil })
	if _, err := approvalDenied.Execute(context.Background(), "mutate", json.RawMessage(`{"content":"secret"}`)); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
		t.Fatalf("approval denial error=%v", err)
	}
	want := []AuditOutcome{AuditPlanDenied, AuditApprovalError, AuditApprovalError, AuditApprovalDenied}
	if len(events) != len(want) {
		t.Fatalf("audit events=%#v", events)
	}
	for index, outcome := range want {
		if events[index].Outcome != outcome || events[index].Success || events[index].ToolName != "mutate" {
			t.Fatalf("event[%d]=%#v", index, events[index])
		}
	}
}

func TestAgentSessionComposesLiveAndDurableAuditors(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "agent audit"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "deny-1", Name: "mutate", Arguments: `{}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "blocked safely"}},
	}}
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "mutate", Risk: RiskHigh, Mutability: ToolMutabilityMutating}, Fn: func(context.Context, json.RawMessage) (string, error) {
		t.Fatal("refused tool executed")
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var live []AuditEvent
	executor := &Executor{Registry: registry, Scope: testScope(t), Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyDeny, nil
	}), Auditor: AuditorFunc(func(_ context.Context, event AuditEvent) error {
		live = append(live, event)
		return nil
	})}
	agent, err := NewAgentWithOptions(DefaultConfig(), provider, AgentOptions{Executor: executor, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "try the mutation", nil); err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Outcome != AuditPolicyDenied {
		t.Fatalf("live audits=%#v", live)
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var durable int
	for _, event := range loaded.Events {
		if event.Type == EventAudit {
			durable++
		}
	}
	if durable != 1 {
		t.Fatalf("durable audit events=%d", durable)
	}
}

func TestExecutorOutputTruncationAndUnknownTool(t *testing.T) {
	scope := testScope(t)
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "out"}, Fn: func(context.Context, json.RawMessage) (string, error) { return strings.Repeat("x", 100), nil }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Executor{Registry: registry, Scope: scope}).Execute(context.Background(), "out", nil)
	if err != nil || !result.Truncated || len(result.Output) > scope.MaxOutputBytes {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := (Executor{Registry: registry, Scope: scope}).Execute(context.Background(), "missing", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestExecutorToolSpecificTimeoutKeepsOrdinaryToolsBounded(t *testing.T) {
	scope := ExecutionScope{Root: t.TempDir(), TimeoutSeconds: 1}
	longWait := 1250 * time.Millisecond
	registry, err := NewRegistry(
		ToolFunc{ToolDefinition: ToolDefinition{Name: "ordinary"}, Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			timer := time.NewTimer(longWait)
			defer timer.Stop()
			select {
			case <-timer.C:
				return "completed", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		timeoutProbeTool{wait: longWait, timeout: 2 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{Registry: registry, Scope: scope}
	if _, err := executor.Execute(context.Background(), "ordinary", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ordinary tool error=%v, want scope timeout", err)
	}
	result, err := executor.Execute(context.Background(), "timeout_probe", nil)
	if err != nil || result.Output != "completed" {
		t.Fatalf("extended tool result=%#v err=%v", result, err)
	}
	if got := effectiveToolTimeout(timeoutProbeTool{timeout: 24 * time.Hour}, scope); got != maxToolExecutionTimeout {
		t.Fatalf("effective timeout=%s, want global cap %s", got, maxToolExecutionTimeout)
	}
}

func TestCodingToolsStayWithinRootAndWriteEditExactlyOnce(t *testing.T) {
	scope := testScope(t)
	inside := filepath.Join(scope.Root, "file.txt")
	if err := os.WriteFile(inside, []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := ReadFileTool{Scope: scope}
	output, err := read.Execute(context.Background(), json.RawMessage(`{"path":"file.txt"}`))
	if err != nil || output != "hello\nworld\n" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"../escape"}`)); err == nil {
		t.Fatal("expected path escape rejection")
	}
	write := WriteFileTool{Scope: scope}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","operation":"edit","old_text":"hello","new_text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","operation":"edit","old_text":"world","new_text":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","operation":"edit","old_text":"x","new_text":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","operation":"edit","old_text":"missing","new_text":"x"}`)); err == nil {
		t.Fatal("expected missing exact replacement error")
	}
}

func TestWriteFileRequiresExplicitExistingFileOperation(t *testing.T) {
	scope := testScope(t)
	tool := WriteFileTool{Scope: scope}
	path := filepath.Join(scope.Root, "notes.md")

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","content":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","content":"second"}`)); err == nil || !strings.Contains(err.Error(), "file already exists") {
		t.Fatalf("implicit overwrite error=%v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first" {
		t.Fatalf("implicit overwrite changed file: data=%q err=%v", data, err)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","operation":"append","content":"\nsecond"}`)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first\nsecond" {
		t.Fatalf("append data=%q err=%v", data, err)
	}

	state, _, err := fileState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "notes.md", "operation": "edit", "old_text": "second", "new_text": "third", "expected_sha256": state.SHA256,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "notes.md", "operation": "append", "content": "stale", "expected_sha256": state.SHA256,
	})); err == nil || !strings.Contains(err.Error(), "expected_sha256") {
		t.Fatalf("stale hash error=%v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first\nthird" {
		t.Fatalf("edit data=%q err=%v", data, err)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"notes.md","operation":"replace","content":"replacement"}`)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatalf("replace data=%q err=%v", data, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"new.md","operation":"replace","content":"no"}`)); err == nil {
		t.Fatal("replace unexpectedly created a missing file")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"append-created.md","operation":"append","content":"created by append"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"duplicates.txt","content":"same same"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"duplicates.txt","operation":"edit","old_text":"same","new_text":"different"}`)); err == nil || !strings.Contains(err.Error(), "matches 2 places") {
		t.Fatalf("ambiguous edit error=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(scope.Root, "duplicates.txt")); err != nil || string(data) != "same same" {
		t.Fatalf("ambiguous edit changed file: data=%q err=%v", data, err)
	}
}

func TestWriteAndEditDefinitionsAreFlatAndStrict(t *testing.T) {
	registry, err := NewCodingRegistry(testScope(t))
	if err != nil {
		t.Fatal(err)
	}
	write, ok := registry.Lookup("write_file")
	if !ok {
		t.Fatal("write_file is not registered")
	}
	edit, ok := registry.Lookup("edit_file")
	if !ok {
		t.Fatal("edit_file is not registered")
	}
	// Flat object schemas: no top-level oneOf, which local backends and
	// grammar-constrained models handle poorly.
	for name, definition := range map[string]ToolDefinition{"write_file": write.Definition(), "edit_file": edit.Definition()} {
		var schema map[string]any
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if schema["type"] != "object" || schema["oneOf"] != nil || schema["additionalProperties"] != false {
			t.Fatalf("%s schema is not a flat strict object: %s", name, definition.Parameters)
		}
	}
	writeSchema := string(write.Definition().Parameters)
	for _, want := range []string{`"create"`, `"append"`, `"replace"`, `"expected_sha256"`, `"required":["path","operation","content"]`} {
		if !strings.Contains(writeSchema, want) {
			t.Fatalf("write_file schema missing %s: %s", want, writeSchema)
		}
	}
	if strings.Contains(writeSchema, `"old_text"`) || strings.Contains(writeSchema, `"edit"`) {
		t.Fatalf("write_file still advertises edits: %s", writeSchema)
	}
	editSchema := string(edit.Definition().Parameters)
	for _, want := range []string{`"old_text"`, `"new_text"`, `"replace_all"`, `"required":["path","old_text","new_text"]`} {
		if !strings.Contains(editSchema, want) {
			t.Fatalf("edit_file schema missing %s: %s", want, editSchema)
		}
	}
	tool := WriteFileTool{Scope: testScope(t)}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"x","operation":"truncate","content":"x"}`)); err == nil {
		t.Fatal("unsupported operation was accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"x","content":"x","unknown":true}`)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestWritingThroughSymlinkedParentCannotEscapeRoot(t *testing.T) {
	scope := testScope(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(scope.Root, "link")); err != nil {
		t.Fatal(err)
	}
	writer := WriteFileTool{Scope: scope}
	if _, err := writer.Execute(context.Background(), json.RawMessage(`{"path":"link/escape.txt","content":"nope"}`)); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
}

func TestGrepSkipsSymlinksOutsideScope(t *testing.T) {
	scope := testScope(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOPSECRET-DATA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope.Root, "inside.txt"), []byte("SAFE-DATA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(scope.Root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	output, err := (GrepTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"query":"DATA"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "inside.txt:1:SAFE-DATA") {
		t.Fatalf("output missing in-scope match: %q", output)
	}
	if strings.Contains(output, "link.txt") || strings.Contains(output, "TOPSECRET-DATA") {
		t.Fatalf("grep followed symlink outside scope: %q", output)
	}
}

func TestWriteAndRunCommandUseScopeAndCancellation(t *testing.T) {
	scope := testScope(t)
	writer := WriteFileTool{Scope: scope}
	if _, err := writer.Execute(context.Background(), json.RawMessage(`{"path":"nested/new.txt","content":"data"}`)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(scope.Root, "nested/new.txt")); err != nil || string(data) != "data" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	runner := RunCommandTool{Scope: scope}
	output, err := runner.Execute(context.Background(), json.RawMessage(`{"argv":["sh","-c","printf ok"]}`))
	if err != nil || output != "ok" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := runner.Execute(ctx, json.RawMessage(`{"argv":["sh","-c","sleep 1"]}`)); err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestExecutorFailsClosedForUnknownPolicyDecision(t *testing.T) {
	scope := testScope(t)
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "x"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (Executor{Registry: registry, Scope: scope, Policy: PolicyFunc(func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error) {
		return PolicyDecision("maybe"), nil
	})}).Execute(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown decision") {
		t.Fatalf("err=%v", err)
	}
}

func TestToolArgumentsRejectTrailingJSON(t *testing.T) {
	scope := testScope(t)
	_, err := (ReadFileTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"path":"x"}{"path":"y"}`))
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("err=%v", err)
	}
}

func TestToolResultContentIsStructured(t *testing.T) {
	content := (ToolResult{ToolName: "x", Output: "ok"}).Content()
	var decoded ToolResult
	if err := json.Unmarshal([]byte(content), &decoded); err != nil || decoded.ToolName != "x" || decoded.Output != "ok" {
		t.Fatalf("content=%s decoded=%#v err=%v", content, decoded, err)
	}
}
