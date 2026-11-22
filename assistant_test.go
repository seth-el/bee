package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComposeGeneralAssistantProvidesUnifiedToolsAndScope(t *testing.T) {
	root := t.TempDir()
	jobStore, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobStore, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if app.Executor == nil || app.RequirePlan || app.Executor.ApprovalMode != ApprovalModeManual {
		t.Fatalf("executor=%#v require_plan=%t", app.Executor, app.RequirePlan)
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", ExecReadOnlyToolName, "exec", "host_exec", "repo_clone", "memory_search", "memory_show", "job_create", "job_list", "job_cancel", "bee_plan"} {
		if name == PlanToolName {
			continue
		}
		if _, ok := app.Executor.Registry.Lookup(name); !ok {
			t.Fatalf("missing tool %q", name)
		}
	}
	if app.Capabilities.ScopeRoot != root || app.Capabilities.WriteRoot != root || app.Capabilities.Policy != AssistantPolicyTrusted {
		t.Fatalf("capabilities=%#v", app.Capabilities)
	}
	if !app.Executor.Scope.TrustedForeground {
		t.Fatalf("trusted assistant scope=%#v", app.Executor.Scope)
	}
	if _, ok := app.Executor.Registry.Lookup("run_command"); ok {
		t.Fatal("legacy run_command must not be model-facing in trusted foreground composition")
	}
	// edit_file is model-facing again: folding edits into write_file's oneOf
	// schema confused the local model (see the 2026-11-12 tool review).
	if _, ok := app.Executor.Registry.Lookup("edit_file"); !ok {
		t.Fatal("edit_file must be model-facing")
	}
	tool, ok := app.Executor.Registry.Lookup(ExecToolName)
	if !ok || tool.Definition().PlanExempt {
		t.Fatalf("exec definition=%#v", tool)
	}
	readOnly, ok := app.Executor.Registry.Lookup(ExecReadOnlyToolName)
	if !ok || readOnly.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("exec_readonly definition=%#v", readOnly)
	}
}

func TestComposeGeneralAssistantPlumbsConfiguredApprovalMode(t *testing.T) {
	config := DefaultConfig()
	config.Assistant.ApprovalMode = ApprovalModeAuto
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if app.Executor == nil {
		t.Fatal("executor is nil")
	}
	if app.Executor.ApprovalMode != ApprovalModeAuto {
		t.Fatalf("executor approval mode=%q, want auto", app.Executor.ApprovalMode)
	}
}

func TestApprovalModeDoesNotChangeProcessContainment(t *testing.T) {
	root := t.TempDir()
	compose := func(mode ApprovalMode) ExecutionScope {
		t.Helper()
		config := DefaultConfig()
		config.Assistant.ApprovalMode = mode
		app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if app.Executor == nil {
			t.Fatal("executor is nil")
		}
		return app.Executor.Scope
	}
	manual := compose(ApprovalModeManual)
	auto := compose(ApprovalModeAuto)
	if manual.EnforceWriteRoots != auto.EnforceWriteRoots || manual.Root != auto.Root || manual.WriteRoot != auto.WriteRoot || strings.Join(manual.Roots, "\x00") != strings.Join(auto.Roots, "\x00") || strings.Join(manual.WriteRoots, "\x00") != strings.Join(auto.WriteRoots, "\x00") {
		t.Fatalf("approval mode changed containment:\nmanual=%#v\nauto=%#v", manual, auto)
	}
	if !auto.EnforceWriteRoots {
		t.Fatal("auto approval disabled the process sandbox")
	}
}

func TestComposeGeneralAssistantExplicitUnsafeSandboxOptOut(t *testing.T) {
	config := DefaultConfig()
	config.Assistant.UnsafeDisableProcessSandbox = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if app.Executor.Scope.EnforceWriteRoots {
		t.Fatalf("unsafe opt-out scope=%#v", app.Executor.Scope)
	}
	if _, ok := app.Executor.Registry.Lookup(ExecReadOnlyToolName); ok {
		t.Fatal("exec_readonly must not be exposed without its sandbox boundary")
	}
	if _, ok := app.Executor.Registry.Lookup(ExecToolName); !ok {
		t.Fatal("ordinary exec missing under explicit unsafe opt-out")
	}
}

func TestImplicitAssistantScopeUsesBeeWorkspaceThenHome(t *testing.T) {
	home := t.TempDir()
	beeHome := filepath.Join(home, ".bee")
	workspace := filepath.Join(beeHome, "workspace")
	memory := filepath.Join(workspace, "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory, "today.md"), []byte("workspace memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(home, "outside-workspace.txt")
	if err := os.WriteFile(homeFile, []byte("still allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", beeHome)

	roots, fullMachine, err := resolveAssistantScopes(AssistantConfig{}, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if fullMachine || len(roots) != 2 || roots[0] != workspace || roots[1] != home {
		t.Fatalf("implicit roots=%#v full_machine=%t", roots, fullMachine)
	}
	writeRoots, err := resolveAssistantWriteScopes(AssistantConfig{}, "", nil, roots, fullMachine)
	if err != nil {
		t.Fatal(err)
	}
	if len(writeRoots) != 1 || writeRoots[0] != workspace {
		t.Fatalf("implicit write roots=%#v", writeRoots)
	}
	scope := ExecutionScope{Root: roots[0], Roots: roots[1:]}
	listing, err := (ListFilesTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"path":"memory"}`))
	if err != nil {
		t.Fatal(err)
	}
	if listing != "today.md" {
		t.Fatalf("relative memory listing=%q", listing)
	}
	contents, err := (ReadFileTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"path":"`+homeFile+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if contents != "still allowed" {
		t.Fatalf("secondary home read=%q", contents)
	}
}

func TestTrustedFileWritesPreferWorkspaceAndApproveSecondaryReadRoot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	secondary := filepath.Join(root, "home")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secondary, 0o700); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: workspace, Roots: []string{secondary}, WriteRoot: workspace}
	registry, err := NewRegistry(WriteFileTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	executor := &Executor{
		Registry: registry,
		Scope:    scope,
		Policy:   GeneralAssistantPolicy{Mode: AssistantPolicyTrusted},
		Approval: ApprovalFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
			approvals++
			return ApprovalApproved, nil
		}),
	}
	if _, err := executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"memory/2026-10-26.md","content":"dated memory"}`)); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("workspace write approvals=%d", approvals)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "memory", "2026-10-26.md")); err != nil || string(data) != "dated memory" {
		t.Fatalf("dated memory data=%q err=%v", data, err)
	}
	external := filepath.Join(secondary, "approved.txt")
	args := mustJSON(map[string]any{"path": external, "content": "approved"})
	if _, err := executor.Execute(context.Background(), "write_file", args); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 {
		t.Fatalf("secondary write approvals=%d", approvals)
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "approved" {
		t.Fatalf("secondary data=%q err=%v", data, err)
	}
	link := filepath.Join(workspace, "linked-home")
	if err := os.Symlink(secondary, link); err != nil {
		t.Fatal(err)
	}
	linkedTarget := filepath.Join(link, "linked-approved.txt")
	if _, err := executor.Execute(context.Background(), "write_file", mustJSON(map[string]any{"path": linkedTarget, "content": "linked"})); err != nil {
		t.Fatal(err)
	}
	if approvals != 2 {
		t.Fatalf("symlinked secondary write approvals=%d", approvals)
	}
	if data, err := os.ReadFile(filepath.Join(secondary, "linked-approved.txt")); err != nil || string(data) != "linked" {
		t.Fatalf("linked secondary data=%q err=%v", data, err)
	}
	if _, err := (WriteFileTool{Scope: scope}).Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "requires explicit approval") {
		t.Fatalf("direct outside write err=%v", err)
	}
}

func TestImplicitAssistantScopeFallbackAndExplicitOverride(t *testing.T) {
	home := t.TempDir()
	beeHome := filepath.Join(home, ".bee-not-initialized")
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", beeHome)

	roots, _, err := resolveAssistantScopes(AssistantConfig{}, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != home {
		t.Fatalf("uninitialized implicit roots=%#v", roots)
	}

	workspace := filepath.Join(beeHome, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	explicit := t.TempDir()
	roots, _, err = resolveAssistantScopes(AssistantConfig{}, explicit, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != explicit {
		t.Fatalf("explicit roots=%#v", roots)
	}
}

func TestComposeGeneralAssistantWiresExplicitCodingProjectAndParentWorkspace(t *testing.T) {
	root, head := newWorkspaceTestRepo(t)
	config := DefaultConfig()
	config.Coding.Enabled = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{
		Store:             mustAssistantStore(t),
		ScopeRoot:         filepath.Dir(root),
		CodingProjectRoot: root,
		CodingManagedRoot: filepath.Join(t.TempDir(), "managed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.CodingDelegation == nil {
		t.Fatal("coding delegation was not composed")
	}
	manager, ok := app.CodingDelegation.Workspace.(*GitWorkspaceManager)
	if !ok || filepath.Clean(manager.RepositoryRoot()) != filepath.Clean(root) || app.CodingDelegation.BaseCommit != head || filepath.Clean(app.CodingDelegation.ProjectRoot) != filepath.Clean(root) {
		t.Fatalf("coding composition workspace=%T service=%#v", app.CodingDelegation.Workspace, app.CodingDelegation)
	}
	localRunner, ok := app.CodingDelegation.LocalExecutor.(*ChildRunner)
	if !ok || localRunner.RequirePlanOverride == nil || *localRunner.RequirePlanOverride {
		t.Fatalf("coding local executor must inherit the parent-approved plan without a second child plan gate: %#v", app.CodingDelegation.LocalExecutor)
	}
}

func TestComposeGeneralAssistantWiresConfiguredSemanticRetrievalToToolAndContext(t *testing.T) {
	root := t.TempDir()
	config := DefaultConfig()
	config.Semantic = SemanticConfig{Enabled: true, Endpoint: "http://127.0.0.1:8090", Model: "test-embedding", IndexRoot: filepath.Join(t.TempDir(), "missing-index")}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := app.Executor.Registry.Lookup("memory_search")
	if !ok {
		t.Fatal("memory_search is not registered")
	}
	search, ok := tool.(MemorySearchTool)
	if !ok || !search.Retrieval.SemanticEnabled {
		t.Fatalf("memory search semantic wiring=%#v", tool)
	}
	options := app.agentOptions(nil, nil, "")
	if !options.MemoryRetrieval.SemanticEnabled {
		t.Fatalf("agent context semantic wiring=%#v", options.MemoryRetrieval)
	}
}

func TestComposeGeneralAssistantHybridRecallSkipsTrivialPrompt(t *testing.T) {
	provider := &fakeProvider{}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	memory := &runtimeContextMemoryService{result: MemorySearchResult{Hits: []MemoryDisplayHit{{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "memory-1", Namespace: MemoryNamespaceDurableMemory, Key: "k", Content: "must stay out of ordinary chat", Sensitivity: MemorySensitivityPublic}}}}}}
	app.Provider = provider
	app.MemoryService = memory
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(memory.queries) != 0 {
		t.Fatalf("ordinary assistant unexpectedly searched memory: %#v", memory.queries)
	}
	if len(provider.requests) != 1 || strings.Contains(ContextText(ContextResult{Messages: provider.requests[0].Messages}), "must stay out of ordinary chat") {
		t.Fatalf("ordinary assistant memory context=%#v", provider.requests)
	}
}

func TestComposeGeneralAssistantHybridRecallSearchesSubstantivePrompt(t *testing.T) {
	provider := &fakeProvider{}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	memory := &runtimeContextMemoryService{result: MemorySearchResult{Hits: []MemoryDisplayHit{{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "memory-1", Namespace: MemoryNamespaceDurableMemory, Key: "project", Content: "the remembered project context", Sensitivity: MemorySensitivityPublic}}, Score: MemoryScore{Total: 0.6, Value: 0.6}}}}}
	app.Provider = provider
	app.MemoryService = memory
	events, err := app.Run(context.Background(), RunRequest{Prompt: "Tell me about the project."})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(memory.queries) != 1 || memory.queries[0].Text != "project" {
		t.Fatalf("hybrid memory queries = %#v", memory.queries)
	}
	if len(provider.requests) != 1 || !strings.Contains(ContextText(ContextResult{Messages: provider.requests[0].Messages}), "the remembered project context") {
		t.Fatalf("ordinary assistant memory context=%#v", provider.requests)
	}
}

func TestGeneralAssistantSSHPolicyOnlyAllowsTrustedForeground(t *testing.T) {
	args := json.RawMessage(`{"command":"ssh vm.example hostname"}`)
	definition := ToolDefinition{Name: ExecToolName, Mutability: ToolMutabilityMutating}
	trusted := GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}
	if decision, err := trusted.Evaluate(context.Background(), definition, args, ExecutionScope{TrustedForeground: true}); err != nil || decision != PolicyAllow {
		t.Fatalf("trusted SSH decision=%v err=%v", decision, err)
	}
	for _, mode := range []AssistantPolicyMode{AssistantPolicyPrompted, AssistantPolicyLocked} {
		policy := GeneralAssistantPolicy{Mode: mode}
		scope := ExecutionScope{}
		if mode == AssistantPolicyPrompted {
			scope.TrustedForeground = true
		}
		decision, err := policy.Evaluate(context.Background(), definition, args, scope)
		if err != nil {
			t.Fatalf("mode=%s err=%v", mode, err)
		}
		want := PolicyDeny
		if mode == AssistantPolicyPrompted {
			want = PolicyRequireApproval
		}
		if decision != want {
			t.Fatalf("mode=%s decision=%v, want %v", mode, decision, want)
		}
	}
}

func TestGeneralAssistantPlanGateIsExplicitAndUnclassifiedBehaviorIsConfigurable(t *testing.T) {
	config := DefaultConfig()
	config.Assistant.RequirePlan = true
	config.Assistant.AllowUnclassifiedBeforePlan = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !app.RequirePlan || app.Executor.planGate == nil || !app.Executor.planGate.AllowUnclassifiedBeforePlan {
		t.Fatalf("plan configuration was not applied: %#v", app.Executor.planGate)
	}
}

func TestExecutionScopeAllowsMultipleRoots(t *testing.T) {
	primary := t.TempDir()
	secondary := t.TempDir()
	path := filepath.Join(secondary, "shared.txt")
	if err := os.WriteFile(path, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := ReadFileTool{Scope: ExecutionScope{Root: primary, Roots: []string{secondary}}}
	output, err := tool.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	if err != nil || output != "shared" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestTrustedAssistantExecutesSSHWithoutRemoteGrant(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "ssh-args.log")
	sshPath := filepath.Join(root, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	jobStore, err := NewJobStore(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobStore, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Executor.Execute(context.Background(), ExecToolName, json.RawMessage(`{"command":"ssh testuser@vm.example hostname"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "testuser@vm.example") || strings.Contains(string(data), "BatchMode") || strings.Contains(string(data), "StrictHostKeyChecking") {
		t.Fatalf("ssh args=%q", data)
	}
}

func TestGeneralAssistantJobToolsAreBoundedAndRedacted(t *testing.T) {
	root := t.TempDir()
	jobStore, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobStore, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	plan := TaskPlan{ID: "job-plan", Goal: "manage a foreground job", Inputs: []string{"job request"}, Outputs: []string{"job state"}, Steps: []string{"record", "inspect"}, Acceptance: []string{"job remains explicit"}}
	if err := app.Executor.SetPlan(plan); err != nil {
		t.Fatal(err)
	}
	createArgs := json.RawMessage(`{"name":"nightly note","owner":"parent","idempotency_key":"job-test-1","prompt":"secret prompt should never be returned","run_at":"2030-01-01T00:00:00Z","max_attempts":2}`)
	created, err := app.Executor.Execute(context.Background(), "job_create", createArgs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(created.Output, "secret prompt") || strings.Contains(created.Output, "attempts_log") {
		t.Fatalf("job_create leaked bounded fields: %q", created.Output)
	}
	var view struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Output), &view); err != nil || view.ID == "" {
		t.Fatalf("created output=%q err=%v", created.Output, err)
	}
	listed, err := app.Executor.Execute(context.Background(), "job_list", json.RawMessage(`{"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed.Output, "secret prompt") || !strings.Contains(listed.Output, view.ID) {
		t.Fatalf("job_list output=%q", listed.Output)
	}
	// job_approve is no longer model-facing (approval is the human's call);
	// its bounded output is still checked on the tool itself.
	approvedOutput, err := (JobApproveTool{Store: jobStore}).Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q}`, view.ID)))
	approved := ToolResult{Output: approvedOutput}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(approved.Output, `"approval":"approved"`) {
		t.Fatalf("job_approve output=%q", approved.Output)
	}
	if _, err := app.Executor.Execute(context.Background(), "job_cancel", json.RawMessage(fmt.Sprintf(`{"id":%q}`, view.ID))); err != nil {
		t.Fatal(err)
	}
}

func TestGeneralAssistantJobToolsAreNotProjectedToChildren(t *testing.T) {
	root := t.TempDir()
	jobStore, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobStore, ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = copyChildExecutor(app.Executor, ChildTask{TaskID: "child-job", ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{"job_list"}, Scope: ExecutionScope{Root: root}, Budget: ChildBudget{}, AcceptanceCriteria: []string{"done"}})
	if err == nil || !strings.Contains(err.Error(), "parent-owned") {
		t.Fatalf("job tool projected to child: %v", err)
	}
}

func TestGeneralAssistantReadOnlyBeforePlanAndMutationAfterPlan(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Assistant.RequirePlan = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := app.Executor.Execute(context.Background(), "read_file", json.RawMessage(`{"path":"note.txt"}`)); err != nil || result.Output != "hello" {
		t.Fatalf("read result=%#v err=%v", result, err)
	}
	if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"out.txt","content":"data"}`)); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("mutation without plan err=%v", err)
	}
	plan := TaskPlan{ID: "assistant-plan", Goal: "write", Inputs: []string{"note"}, Outputs: []string{"out"}, Steps: []string{"inspect", "write"}, Acceptance: []string{"out exists"}}
	if err := app.Executor.SetPlan(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"out.txt","content":"data"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryToolsBoundAndRedactSensitiveContent(t *testing.T) {
	root := t.TempDir()
	policy := DefaultMemoryPolicy()
	policy.AllowSensitiveMemory = true
	service, err := NewLocalMemoryService(filepath.Join(root, "memory"), policy)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	actor := MemoryActor{Kind: MemoryActorParent, ID: "test"}
	if _, err := service.Remember(context.Background(), RememberRequest{Actor: actor, Record: MemoryRecord{ID: "memory-sensitive", Namespace: MemoryNamespaceDurableMemory, Key: "secret", Content: "do-not-show", Sensitivity: MemorySensitivitySensitive, CreatedAt: now, UpdatedAt: now, Confidence: 0.9}, AuditID: "remember-sensitive", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	search := MemorySearchTool{Service: service, Actor: actor, Policy: policy}
	output, err := search.Execute(context.Background(), json.RawMessage(`{"query":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "do-not-show") {
		t.Fatalf("sensitive content leaked: %q", output)
	}
	if _, err := search.Execute(context.Background(), json.RawMessage(`{"query":""}`)); err == nil {
		t.Fatal("empty memory query accepted")
	}
	show := MemoryShowTool{Service: service, Actor: actor, Policy: policy}
	output, err = show.Execute(context.Background(), json.RawMessage(`{"id":"memory-sensitive"}`))
	if err != nil && !errors.Is(err, ErrMemoryNotFound) {
		t.Fatal(err)
	}
	if strings.Contains(output, "do-not-show") {
		t.Fatalf("sensitive show leaked: %q", output)
	}
}

func TestComposeGeneralAssistantWebRegistryIsOptional(t *testing.T) {
	config := DefaultConfig()
	if app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	} else if app.Capabilities.WebEnabled || assistantContainsString(app.Capabilities.ToolNames, "web_search") {
		t.Fatalf("web unexpectedly enabled: %#v", app.Capabilities)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	config.Web = WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 2, MaxChars: 256}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !app.Capabilities.WebEnabled || !assistantContainsString(app.Capabilities.ToolNames, "web_search") {
		t.Fatalf("web registry missing: %#v", app.Capabilities)
	}
}

func TestComposeGeneralAssistantMediaSearchIsExplicitlyGated(t *testing.T) {
	config := DefaultConfig()
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if app.MediaSearch != nil || app.Executor == nil {
		t.Fatalf("media search unexpectedly configured: app=%#v executor=%#v", app.MediaSearch, app.Executor)
	}
	if _, ok := app.Executor.Registry.Lookup(MediaRetrieveToolName); ok {
		t.Fatal("media_retrieve unexpectedly registered outside web media composition")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	config.Web = WebConfig{MediaSearchEnabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 2, MaxChars: 256}
	app, err = ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir(), EnableMediaSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	if app.MediaSearch == nil || app.Executor == nil {
		t.Fatalf("media search was not configured: app=%#v executor=%#v", app.MediaSearch, app.Executor)
	}
	if _, ok := app.Executor.Registry.Lookup("media_search"); !ok {
		t.Fatal("media_search tool was not registered")
	}
	if _, ok := app.Executor.Registry.Lookup(MediaRetrieveToolName); !ok {
		t.Fatal("media_retrieve tool was not registered")
	}
}

func TestComposeGeneralAssistantLockedHasNoTools(t *testing.T) {
	config := DefaultConfig()
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir(), Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if app.Executor != nil || len(app.Capabilities.ToolNames) != 0 {
		t.Fatalf("locked assistant exposed tools: app=%#v capabilities=%#v", app.Executor, app.Capabilities)
	}
}

func mustAssistantStore(t *testing.T) *SessionStore {
	t.Helper()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assistantContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
