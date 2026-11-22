package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func execTestScope(root string) ExecutionScope {
	return ExecutionScope{Root: root, TrustedForeground: true, MaxOutputBytes: 4096}
}

func decodeExecResult(t *testing.T, output string) execResult {
	t.Helper()
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("exec output=%q: %v", output, err)
	}
	return result
}

func requireFreshBubblewrapLayer(t *testing.T) {
	t.Helper()
	if insideBeeProcessSandbox() {
		t.Skip("fresh Bubblewrap policy assertion is not applicable inside an existing Bee sandbox")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is unavailable")
	}
}

func TestExecToolRunsShellSyntaxEnvAndWorkdir(t *testing.T) {
	root := t.TempDir()
	tool := ExecTool{Scope: execTestScope(root)}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf 'one\\n' | tr o O\nprintf 'two\\n'","workdir":"`+root+`","env":{"BEE_EXEC_TEST":"ok"}}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(result.Output, "One") || !strings.Contains(result.Output, "two") {
		t.Fatalf("result=%#v", result)
	}

	output, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"printf '%s:%s' \"$BEE_EXEC_TEST\" \"$(basename \"$PWD\")\"","workdir":"`+root+`","env":{"BEE_EXEC_TEST":"overlay"}}`))
	if err != nil {
		t.Fatal(err)
	}
	result = decodeExecResult(t, output)
	if result.Output != "overlay:"+filepath.Base(root) {
		t.Fatalf("env/workdir output=%q", result.Output)
	}
}

func TestExecToolReportsExplicitUnsandboxedMode(t *testing.T) {
	scope := execTestScope(t.TempDir())
	scope.EnforceWriteRoots = false
	output, err := (ExecTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"command":"printf explicit-unsafe-mode"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.ExecutionMode != "unsandboxed" || result.Output != "explicit-unsafe-mode" {
		t.Fatalf("result=%#v", result)
	}
}

func TestExecWriteRootSandboxReadsHostAndConfinesPersistentWrites(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := execTestScope(root)
	scope.WriteRoot = workspace
	scope.EnforceWriteRoots = true
	tool := ExecTool{Scope: scope}
	inside := filepath.Join(workspace, "inside.txt")
	blocked := filepath.Join(root, "blocked.txt")
	// Keep command construction explicit: the model-facing shell must be able
	// to read the host file, write the workspace, and observe an outside write
	// failure without turning that expected failure into a tool error.
	command := "cat " + shellSingleQuote(outside) + "; printf inside > " + shellSingleQuote(inside) + "; if printf blocked > " + shellSingleQuote(blocked) + " 2>/dev/null; then exit 19; fi; printf ':confined'"
	output, err := tool.Execute(context.Background(), mustJSON(map[string]any{"command": command}))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(result.Output, "readable") || !strings.Contains(result.Output, ":confined") {
		t.Fatalf("sandbox result=%#v", result)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("inside write data=%q err=%v", data, err)
	}
	if _, err := os.Stat(blocked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside write unexpectedly persisted: %v", err)
	}
}

func TestHostExecRequiresApprovalAndCanMutateOutsideWriteRoot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := execTestScope(root)
	scope.WriteRoot = workspace
	scope.EnforceWriteRoots = true
	registry, err := NewRegistry(HostExecTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "approved.txt")
	args := mustJSON(map[string]any{"command": "printf approved > " + shellSingleQuote(target)})
	executor := &Executor{Registry: registry, Scope: scope, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}}
	if _, err := executor.Execute(context.Background(), HostExecToolName, args); err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("host_exec without approval err=%v", err)
	}
	approvals := 0
	executor.Approval = ApprovalFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
		approvals++
		if request.ToolName != HostExecToolName {
			t.Fatalf("approval request=%#v", request)
		}
		return ApprovalApproved, nil
	})
	if _, err := executor.Execute(context.Background(), HostExecToolName, args); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("approvals=%d", approvals)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "approved" {
		t.Fatalf("host write data=%q err=%v", data, err)
	}
	if _, err := executor.Execute(context.Background(), HostExecToolName, mustJSON(map[string]any{"command": "true", "background": true})); err == nil || !strings.Contains(err.Error(), "foreground-only") {
		t.Fatalf("background host_exec err=%v", err)
	}
}

func TestExecPTYUsesWriteRootSandbox(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := execTestScope(root)
	scope.WriteRoot = workspace
	scope.EnforceWriteRoots = true
	config := DefaultTerminalConfig()
	config.Enabled = true
	config.MaxWaitMS = 500
	manager, err := NewTerminalManager(scope, config)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	inside := filepath.Join(workspace, "pty-inside.txt")
	blocked := filepath.Join(root, "pty-blocked.txt")
	command := "printf inside > " + shellSingleQuote(inside) + "; if printf blocked > " + shellSingleQuote(blocked) + " 2>/dev/null; then printf BAD; else printf CONFINED; fi"
	output, err := (ExecTool{Scope: scope, Terminal: manager}).Execute(context.Background(), mustJSON(map[string]any{"command": command, "pty": true, "yield_ms": 500}))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if !strings.Contains(result.Output, "CONFINED") {
		t.Fatalf("pty result=%#v", result)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("inside data=%q err=%v", data, err)
	}
	if _, err := os.Stat(blocked); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PTY outside write unexpectedly persisted: %v", err)
	}
}

func TestExecToolReturnsExitStatusAndRejectsMalformedInput(t *testing.T) {
	tool := ExecTool{Scope: execTestScope(t.TempDir())}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf failure >&2; exit 7"}`))
	if err != nil {
		t.Fatalf("nonzero process exit became a tool-protocol error: %v", err)
	}
	result := decodeExecResult(t, output)
	if result.Status != "exited_nonzero" || result.ExitCode == nil || *result.ExitCode != 7 || !strings.Contains(result.Output, "failure") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf '\u0000'"}`)); err == nil {
		t.Fatal("NUL command unexpectedly accepted")
	}
}

func TestExecToolKeepsStructuredResultWithinExecutorBound(t *testing.T) {
	scope := execTestScope(t.TempDir())
	scope.MaxOutputBytes = 1024
	registry, err := NewRegistry(ExecTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, scope)
	result, err := executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"i=0; while [ $i -lt 4000 ]; do printf '\"\\n\\t'; i=$((i+1)); done"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatalf("executor truncated structured exec JSON: %q", result.Output)
	}
	if len(result.Output) > scope.MaxOutputBytes {
		t.Fatalf("exec result bytes=%d max=%d", len(result.Output), scope.MaxOutputBytes)
	}
	decoded := decodeExecResult(t, result.Output)
	if !decoded.Truncated || decoded.Output == "" {
		t.Fatalf("decoded=%#v", decoded)
	}
}

func TestExecToolRequestsMaximumOuterTimeout(t *testing.T) {
	if got := (ExecTool{}).ExecutionTimeout(); got != maxToolExecutionTimeout {
		t.Fatalf("exec outer timeout=%s want=%s", got, maxToolExecutionTimeout)
	}
}

func TestExecReadOnlyToolUsesReadOnlyFilesystemSandbox(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := ExecReadOnlyTool{Scope: execTestScope(root)}
	if definition := tool.Definition(); definition.Mutability != ToolMutabilityReadOnly || definition.PlanExempt {
		t.Fatalf("definition=%#v", definition)
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"cat input.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result := decodeExecResult(t, output); strings.Contains(result.Output, "Operation not permitted") && strings.Contains(result.Output, "bwrap:") {
		t.Skip("test sandbox does not permit a nested network namespace")
	} else if !strings.Contains(result.Output, "visible") || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("read result=%#v", result)
	}
	output, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"printf blocked > persistent.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result := decodeExecResult(t, output); result.ExitCode == nil || *result.ExitCode == 0 {
		t.Fatalf("write unexpectedly succeeded: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "persistent.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("persistent write exists or stat failed: %v", err)
	}
}

func TestReadOnlySandboxInvocationIsIsolatedAndPreservesTmpWorkdir(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	program, args, err := sandboxReadOnlyCommandInvocation(execTestScope(root), "bash", []string{"-lc", "true"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(program) != "bwrap" {
		t.Fatalf("program=%q", program)
	}
	joined := strings.Join(args, "\x00")
	for _, expected := range []string{"--unshare-net", "--unshare-pid", "--unshare-ipc", "--tmpfs\x00/tmp", "--tmpfs\x00/run", "--ro-bind\x00" + root + "\x00" + root, "--chdir\x00" + root} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("sandbox args omit %q: %#v", expected, args)
		}
	}
}

func TestReadOnlyExecEnvCanonicalizesPathAndDropsHostIPC(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	env := readOnlyExecEnv([]string{
		"PATH=" + linkedDirectory + string(os.PathListSeparator) + realDirectory,
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/bus",
		"KEEP=value",
	})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH="+realDirectory) || strings.Count(joined, realDirectory) != 1 {
		t.Fatalf("canonical PATH env=%#v", env)
	}
	if strings.Contains(joined, "SSH_AUTH_SOCK") || strings.Contains(joined, "DBUS_SESSION_BUS_ADDRESS") || !strings.Contains(joined, "KEEP=value") {
		t.Fatalf("filtered env=%#v", env)
	}
}

func TestMutationCapableProcessToolsCannotBypassPlanGate(t *testing.T) {
	for _, name := range []string{ExecToolName, HostExecToolName, TerminalToolName} {
		definition := ToolDefinition{Name: name, Mutability: ToolMutabilityMutating, PlanExempt: true, Parameters: json.RawMessage(`{"type":"object"}`)}
		if err := definition.Validate(); err == nil {
			t.Fatalf("%s unexpectedly accepted plan exemption", name)
		}
	}
	definition := ToolDefinition{Name: "write_file", Mutability: ToolMutabilityMutating, PlanExempt: true, Parameters: json.RawMessage(`{"type":"object"}`)}
	if err := definition.Validate(); err == nil {
		t.Fatal("ordinary mutating tool unexpectedly accepted plan exemption")
	}
}

func TestExecToolCancellationCleansProcessGroup(t *testing.T) {
	root := t.TempDir()
	tool := ExecTool{Scope: execTestScope(root)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 30"}`))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation err=%v output=%q", err, output)
	}
	result := decodeExecResult(t, output)
	if result.Status != "canceled" {
		t.Fatalf("cancellation result=%#v", result)
	}
}

func TestTrustedCompositionPlanGatesExecAndExposesReadOnlyShell(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	config := DefaultConfig()
	config.Assistant.RequirePlan = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Executor.Registry.Lookup(ExecToolName); !ok {
		t.Fatal("trusted composition omitted exec")
	}
	readOnly, ok := app.Executor.Registry.Lookup(ExecReadOnlyToolName)
	if !ok || readOnly.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("trusted composition exec_readonly=%#v", readOnly)
	}
	if _, ok := app.Executor.Registry.Lookup("run_command"); ok {
		t.Fatal("trusted composition exposed legacy run_command")
	}
	result, err := app.Executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"printf gated"}`))
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeExecResult(t, result.Output)
	if strings.Contains(decoded.Output, "bwrap:") && strings.Contains(decoded.Output, "Operation not permitted") {
		t.Skip("test sandbox does not permit a nested network namespace")
	}
	if result.EffectiveToolName != ExecReadOnlyToolName || decoded.ExecutionMode != "read_only" || decoded.Output != "gated" {
		t.Fatalf("downgraded exec result=%#v decoded=%#v", result, decoded)
	}
	if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"blocked","content":"x"}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("write_file plan gate err=%v", err)
	}
	child, err := copyChildExecutor(app.Executor, ChildTask{TaskID: "exec-child", ParentID: "parent", Objective: "test", Prompt: "test", AllowedTools: []string{ExecToolName}, Scope: ExecutionScope{Root: root}, Budget: ChildBudget{}})
	if err == nil || child != nil || !strings.Contains(err.Error(), "trusted foreground-only") {
		t.Fatalf("child exec projection err=%v child=%#v", err, child)
	}
}

func TestExecutorDowngradesBlockedExecWithoutGrantingMutation(t *testing.T) {
	requireFreshBubblewrapLayer(t)
	root := t.TempDir()
	inputPath := filepath.Join(root, "input.txt")
	if err := os.WriteFile(inputPath, []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := execTestScope(root)
	registry, err := NewRegistry(ExecTool{Scope: scope}, ExecReadOnlyTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	var audits []AuditEvent
	executor := &Executor{Registry: registry, Scope: scope, Policy: DefaultCodingPolicy{}, Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalApproved, nil
	}), Auditor: AuditorFunc(func(_ context.Context, event AuditEvent) error {
		audits = append(audits, event)
		return nil
	}), planGate: &PlanGate{}}

	result, err := executor.Execute(context.Background(), ExecToolName, mustJSON(map[string]any{"command": "cat input.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeExecResult(t, result.Output)
	if strings.Contains(decoded.Output, "bwrap:") && strings.Contains(decoded.Output, "Operation not permitted") {
		t.Skip("test sandbox does not permit a nested network namespace")
	}
	if result.ToolName != ExecToolName || result.EffectiveToolName != ExecReadOnlyToolName || decoded.ExecutionMode != "read_only" || decoded.Output != "visible" {
		t.Fatalf("inspection downgrade result=%#v decoded=%#v", result, decoded)
	}
	if len(audits) != 1 || audits[0].ToolName != ExecToolName || audits[0].EffectiveToolName != ExecReadOnlyToolName || !audits[0].Success {
		t.Fatalf("downgrade audits=%#v", audits)
	}

	outputPath := filepath.Join(root, "blocked.txt")
	result, err = executor.Execute(context.Background(), ExecToolName, mustJSON(map[string]any{"command": "printf blocked > blocked.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	decoded = decodeExecResult(t, result.Output)
	if decoded.ExecutionMode != "read_only" || decoded.ExitCode == nil || *decoded.ExitCode == 0 {
		t.Fatalf("write downgrade result=%#v decoded=%#v", result, decoded)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downgraded exec persisted a write: %v", err)
	}

	if _, err := executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"printf interactive","pty":true}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("interactive exec plan gate err=%v", err)
	}

	ambiguous := TaskPlan{ID: "ambiguous", Goal: "implement", Inputs: []string{"workspace"}, Outputs: []string{"result"}, Steps: []string{"inspect", "implement"}, Acceptance: []string{"result works"}, Questions: []string{"Which implementation is intended?"}}
	if err := executor.SetPlan(ambiguous); err != nil {
		t.Fatal(err)
	}
	result, err = executor.Execute(context.Background(), ExecToolName, mustJSON(map[string]any{"command": "cat input.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveToolName != ExecReadOnlyToolName || decodeExecResult(t, result.Output).Output != "visible" {
		t.Fatalf("ambiguous-plan inspection result=%#v", result)
	}
	if _, err := executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"printf interactive","background":true}`)); !errors.Is(err, ErrPlanHasClarification) {
		t.Fatalf("background exec with unresolved question err=%v", err)
	}
}

func TestExecutorPlanFallbackUsesLowerAuthorityDefinition(t *testing.T) {
	root := t.TempDir()
	scope := execTestScope(root)
	executed := false
	registry, err := NewRegistry(
		ExecTool{Scope: scope},
		ToolFunc{ToolDefinition: ToolDefinition{Name: ExecReadOnlyToolName, Risk: RiskLow, Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) {
			executed = true
			return "inspected", nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var policyDefinition ToolDefinition
	var audit AuditEvent
	executor := &Executor{
		Registry: registry,
		Scope:    scope,
		Policy: PolicyFunc(func(_ context.Context, definition ToolDefinition, _ json.RawMessage, _ ExecutionScope) (PolicyDecision, error) {
			policyDefinition = definition
			return PolicyAllow, nil
		}),
		Auditor:  AuditorFunc(func(_ context.Context, event AuditEvent) error { audit = event; return nil }),
		planGate: &PlanGate{},
	}
	result, err := executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"git status --short"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !executed || result.ToolName != ExecToolName || result.EffectiveToolName != ExecReadOnlyToolName || result.Output != "inspected" {
		t.Fatalf("fallback result=%#v executed=%t", result, executed)
	}
	if policyDefinition.Name != ExecReadOnlyToolName || policyDefinition.Mutability != ToolMutabilityReadOnly {
		t.Fatalf("policy definition=%#v", policyDefinition)
	}
	if audit.ToolName != ExecToolName || audit.EffectiveToolName != ExecReadOnlyToolName || !audit.Success {
		t.Fatalf("audit=%#v", audit)
	}
	if _, err := executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"git status","pty":true}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("interactive fallback err=%v", err)
	}
}

func TestExecExposureIncludesPromptedButStopsAtLockedBoundary(t *testing.T) {
	for _, mode := range []AssistantPolicyMode{AssistantPolicyPrompted, AssistantPolicyLocked} {
		options := AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir(), Policy: mode}
		app, err := ComposeGeneralAssistant(DefaultConfig(), "", options)
		if err != nil {
			t.Fatal(err)
		}
		if app.Executor != nil {
			if _, ok := app.Executor.Registry.Lookup(ExecToolName); !ok {
				t.Fatalf("mode=%s omitted exec", mode)
			}
		} else if mode != AssistantPolicyLocked {
			t.Fatalf("mode=%s unexpectedly omitted executor", mode)
		}
	}
}

func TestExecToolPTYUsesTerminalManagerLifecycle(t *testing.T) {
	root := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	config.MaxWaitMS = 50
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf 'ready'","pty":true,"yield_ms":100}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Session == nil || !strings.Contains(result.Output, "ready") {
		t.Fatalf("pty result=%#v", result)
	}
	if err := manager.Close(context.Background(), result.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(result.Session.ID); err != nil {
		// Inspect remains available after close; this branch documents that the
		// session store owns lifecycle rather than the exec tool.
		t.Fatal(err)
	}
}

func TestExecToolPTYPreservesQuotedShellCommand(t *testing.T) {
	root := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	config.MaxWaitMS = 100
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf '%s' \"a'b\"","pty":true,"yield_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Status != "exited" || !strings.Contains(result.Output, "a'b") {
		t.Fatalf("quoted PTY result=%#v", result)
	}
}

func TestExecToolBackgroundReturnsTerminalSession(t *testing.T) {
	root := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"sleep 0.1; printf done","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Session == nil {
		t.Fatalf("background result=%#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for {
		read, err := manager.Read(context.Background(), result.Session.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if read.Completed && strings.Contains(read.Output, "done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background output=%q status=%s", read.Output, read.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecToolBackgroundTimeoutClosesSession(t *testing.T) {
	root := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"sleep 30","background":true,"timeout_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Session == nil {
		t.Fatalf("background timeout result=%#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for {
		read, err := manager.Read(context.Background(), result.Session.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if read.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed background session remained running: %#v", read)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecToolPTYInteractiveWrite(t *testing.T) {
	root := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	config.MaxWaitMS = 50
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf 'PROMPT:'; read value; printf 'VALUE:%s' \"$value\"","pty":true,"yield_ms":25}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Session == nil {
		t.Fatalf("interactive result=%#v", result)
	}
	if err := manager.Write(context.Background(), result.Session.ID, "simulated-input\n"); err != nil {
		t.Fatal(err)
	}
	read, err := manager.Read(context.Background(), result.Session.ID, result.Read.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(read.Output, "VALUE:simulated-input") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		read, err = manager.Read(context.Background(), result.Session.ID, result.Read.Cursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(read.Output, "VALUE:simulated-input") {
		t.Fatalf("interactive output=%q", read.Output)
	}
}

func TestExecToolPTYAllowsTrustedWorkdirOutsideScope(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	config := DefaultTerminalConfig()
	config.Enabled = true
	config.MaxWaitMS = 100
	manager, err := NewTerminalManager(execTestScope(root), config)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecTool{Scope: execTestScope(root), Terminal: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"pwd","workdir":"`+outside+`","pty":true,"yield_ms":250}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, output)
	if result.Session == nil || !strings.Contains(result.Output, outside) {
		t.Fatalf("outside workdir result=%#v", result)
	}
}

func TestTrustedTerminalFollowupIsPlanExempt(t *testing.T) {
	config := DefaultConfig()
	config.Terminal.Enabled = true
	config.Terminal.MaxWaitMS = 50
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started, err := app.Executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"printf 'PROMPT:'; read value; printf 'VALUE:%s' \"$value\"","pty":true,"yield_ms":25}`))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeExecResult(t, started.Output)
	if result.Session == nil {
		t.Fatalf("exec result=%#v", result)
	}
	if _, err := app.Executor.Execute(context.Background(), TerminalToolName, json.RawMessage(`{"action":"write","session_id":"`+result.Session.ID+`","input":"simulated-input\n"}`)); err != nil {
		t.Fatalf("terminal follow-up retained a plan gate: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		read, err := app.Terminal.Read(context.Background(), result.Session.ID, result.Read.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(read.Output, "VALUE:simulated-input") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal follow-up output=%q", read.Output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecToolSSHCommandIsNotRewritten(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "ssh-args.log")
	sshPath := filepath.Join(root, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	tool := ExecTool{Scope: execTestScope(root)}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"ssh testuser@example.test hostname"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "BatchMode") || strings.Contains(string(data), "StrictHostKeyChecking") || !strings.Contains(string(data), "testuser@example.test") {
		t.Fatalf("rewritten ssh args=%q", data)
	}
}
