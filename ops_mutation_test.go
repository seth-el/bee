package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mutationTarget() OpsTarget {
	return OpsTarget{Kind: OpsTargetSSHVM, SSHVM: &SSHVMTarget{Host: "10.0.0.4", User: "ops", Port: 2222, VMID: "game-vm"}}
}

func mutationProjection(target OpsTarget) OpsCapabilityProjection {
	return OpsCapabilityProjection{Target: target, Capabilities: []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute, OpsCapabilityMutate, OpsCapabilityRollback}, MaxTimeoutSeconds: 5, MaxCommands: 8, MaxSteps: 1, MaxOutputBytes: 128}
}

func mutationGrant(target OpsTarget, oneShot bool) *OpsMutationGrant {
	return &OpsMutationGrant{GrantID: "grant-1", ApprovalID: "approval-1", ApprovalReason: "deploy disposable game-server VM", Target: target, Capabilities: []OpsCapability{OpsCapabilityExecute, OpsCapabilityMutate, OpsCapabilityRollback}, OperationClasses: []string{OpsOperationClassSSHVMService, OpsOperationClassSSHVMDeploy}, ExpiresAt: time.Now().UTC().Add(time.Hour), OneShot: oneShot, MaxTimeoutSeconds: 5, MaxCommands: 8, MaxSteps: 1, MaxOutputBytes: 128}
}

func mutationTask(target OpsTarget, command ...string) OpsTask {
	return OpsTask{TaskID: "ops-mutation-1", Objective: "restart disposable game service", Target: target, Intent: OpsIntentMutating, OperationClass: OpsOperationClassSSHVMService, Command: command, Budget: OpsBudget{TimeoutSeconds: 2, MaxCommands: 8, MaxSteps: 1, MaxOutputBytes: 64}, RollbackCriteria: []string{"restore the previous service state"}, AcceptanceCriteria: []string{"service reports active"}}
}

func TestApprovedSSHMutationUsesParentRunnerAndBoundedEvidence(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true, EnvAllowlist: []string{"OPENAI_API_KEY"}, RemoteGrant: &RemoteCapabilityGrant{Host: target.SSHVM.Host, User: target.SSHVM.User, Port: target.SSHVM.Port}}, mutationProjection(target), grant)
	if err != nil {
		t.Fatal(err)
	}
	var got OpsCommandRequest
	backend.Runner = func(_ context.Context, request OpsCommandRequest, scope ExecutionScope) (string, error) {
		got = request
		if !request.Mutation || request.OperationClass != OpsOperationClassSSHVMService || request.ApprovalID != "approval-1" {
			t.Fatalf("request audit metadata=%#v", request)
		}
		if request.Host != target.SSHVM.Host || request.User != target.SSHVM.User || request.Port != target.SSHVM.Port {
			t.Fatalf("request target=%#v", request)
		}
		if scope.RemoteGrant != nil || len(scope.EnvAllowlist) != 0 {
			t.Fatalf("parent authority leaked into runner scope: %#v", scope)
		}
		if scope.TrustedForeground {
			t.Fatal("mutation runner inherited trusted foreground authority")
		}
		return strings.Repeat("x", 200), nil
	}
	completion, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server"))
	if err != nil {
		t.Fatal(err)
	}
	if completion.Status != OpsCompletionSucceeded || !completion.OutputTruncated || len(completion.Output) > 64 {
		t.Fatalf("completion=%#v", completion)
	}
	if !got.Mutation || !strings.Contains(strings.Join(completion.Evidence, " "), "approval_id=approval-1") {
		t.Fatalf("missing bounded audit evidence: %#v", completion.Evidence)
	}
}

func TestApprovedSSHMutationThroughDelegationService(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true}, mutationProjection(target), grant)
	if err != nil {
		t.Fatal(err)
	}
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) { return "active", nil }
	service := NewOperationsDelegationService(backend)
	service.AllowMutations = true
	plan := OpsPlan{PlanID: "plan-mutation-1", Goal: "restart disposable game service", Tasks: []OpsTask{mutationTask(target, "systemctl", "restart", "game-server")}, RollbackCriteria: []string{"restore the previous service state"}, AcceptanceCriteria: []string{"service reports active"}}
	result, err := service.Delegate(context.Background(), OpsDelegationRequest{Backend: backend.Name(), Role: "operator", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completions) != 1 || result.Completions[0].Status != OpsCompletionSucceeded || result.Completions[0].PlanID != plan.PlanID {
		t.Fatalf("result=%#v", result)
	}
}

func TestMutationThroughDelegationUsesGrantDefaultsForOmittedBudgets(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir()}, mutationProjection(target), grant)
	if err != nil {
		t.Fatal(err)
	}
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) { return "active", nil }
	service := NewOperationsDelegationService(backend)
	service.AllowMutations = true
	task := mutationTask(target, "systemctl", "restart", "game-server")
	task.Budget = OpsBudget{}
	plan := OpsPlan{PlanID: "plan-mutation-defaults", Goal: "restart disposable game service", Tasks: []OpsTask{task}, RollbackCriteria: []string{"restore the previous service state"}, AcceptanceCriteria: []string{"service reports active"}}
	if _, err := service.Delegate(context.Background(), OpsDelegationRequest{Backend: backend.Name(), Role: "operator", Plan: plan}); err != nil {
		t.Fatalf("delegation with omitted budgets failed: %v", err)
	}
}

func TestMutationRequiresGrantAndApproval(t *testing.T) {
	target := mutationTarget()
	readOnlyProjection := mutationProjection(target)
	readOnlyProjection.Capabilities = []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute}
	readOnlyProjection.ReadOnlyScout = true
	backend, err := NewOpsReadOnlyCommandBackend(ExecutionScope{Root: t.TempDir()}, readOnlyProjection)
	if err != nil {
		t.Fatal(err)
	}
	task := mutationTask(target, "systemctl", "restart", "game-server")
	_, err = backend.Execute(context.Background(), task)
	if !errors.Is(err, ErrOpsMutationGrantRequired) && !errors.Is(err, ErrOperationsMutationDisabled) {
		t.Fatalf("error=%v, want grant denial", err)
	}
	service := NewOperationsDelegationService(backend)
	service.AllowMutations = true
	_, err = service.Delegate(context.Background(), OpsDelegationRequest{Plan: OpsPlan{PlanID: "plan-1", Goal: "mutate", Tasks: []OpsTask{task}, RollbackCriteria: []string{"restore"}, AcceptanceCriteria: []string{"done"}}})
	if !errors.Is(err, ErrOpsMutationGrantRequired) {
		t.Fatalf("service error=%v, want grant denial", err)
	}
}

func TestMutationTargetMismatchExpiredAndReusedGrant(t *testing.T) {
	target := mutationTarget()
	wrong := target
	wrong.SSHVM = &SSHVMTarget{Host: "10.0.0.5", User: "ops", Port: 2222, VMID: "game-vm"}
	grant := mutationGrant(target, true)
	projection := mutationProjection(target)
	projection.Capabilities = append(projection.Capabilities, OpsCapabilityStdin)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true}, projection, grant)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Execute(context.Background(), mutationTask(wrong, "systemctl", "restart", "game-server"))
	if !errors.Is(err, ErrOpsTargetMismatch) {
		t.Fatalf("mismatch error=%v", err)
	}
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) { return "ok", nil }
	if _, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server")); !errors.Is(err, ErrOpsMutationGrantReused) {
		t.Fatalf("reuse error=%v", err)
	}
	expired := mutationGrant(target, false)
	expired.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if _, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true}, mutationProjection(target), expired); !errors.Is(err, ErrOpsMutationGrantExpired) {
		t.Fatalf("expired constructor error=%v", err)
	}
}

func TestMutationRejectsUnsafeCommandsAndControlsStdin(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, false)
	projection := mutationProjection(target)
	projection.Capabilities = append(projection.Capabilities, OpsCapabilityStdin)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true}, projection, grant)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) {
		called = true
		return "ok", nil
	}
	unsafe := mutationTask(target, "sh", "-c", "systemctl restart game-server")
	if _, err := backend.Execute(context.Background(), unsafe); !errors.Is(err, ErrOpsMutationCommandDenied) {
		t.Fatalf("unsafe command error=%v", err)
	}
	if called {
		t.Fatal("unsafe command reached runner")
	}
	stdinTask := mutationTask(target, "systemctl", "restart", "game-server")
	stdinTask.Stdin = "unexpected"
	if _, err := backend.Execute(context.Background(), stdinTask); !errors.Is(err, ErrOpsMutationStdinDenied) {
		t.Fatalf("stdin error=%v", err)
	}
	grant.AllowStdin = true
	if _, err := backend.Execute(context.Background(), stdinTask); err != nil {
		t.Fatalf("explicit stdin grant error=%v", err)
	}
}

func TestMutationPreviewAndGrantNeverEnterModelPacket(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	task := mutationTask(target, "systemctl", "restart", "game-server")
	preview, err := PreviewOpsMutation(task, grant)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.ApprovalID != "approval-1" || preview.Argv[0] != "systemctl" || preview.MaxCommands != 8 || preview.MaxSteps != 1 {
		t.Fatalf("preview=%#v", preview)
	}
	wire, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"grant-1", "approval-1", "OPENAI_API_KEY", "remote_grant", "kubeconfig", "memory"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("task leaked parent authority %q: %s", forbidden, wire)
		}
	}
	plan := OpsPlan{PlanID: "plan-1", Goal: "mutate", Tasks: []OpsTask{task}, RollbackCriteria: []string{"restore"}, AcceptanceCriteria: []string{"done"}}
	projection := mutationProjection(target)
	packet, err := renderOpsACPPacket(plan, task, "bee-parent", projection, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"grant-1", "approval-1", "approval_reason", "remote_grant", "kubeconfig", "memory"} {
		if strings.Contains(packet, forbidden) {
			t.Fatalf("ACP packet leaked parent grant %q: %s", forbidden, packet)
		}
	}
}

func TestMutationOneShotIsConsumedOnRunnerFailure(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir()}, mutationProjection(target), grant)
	if err != nil {
		t.Fatal(err)
	}
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) {
		return "failed", errors.New("runner failure")
	}
	if completion, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server")); err != nil || completion.Status != OpsCompletionFailed {
		t.Fatalf("first execution completion=%#v err=%v", completion, err)
	}
	if _, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server")); !errors.Is(err, ErrOpsMutationGrantReused) {
		t.Fatalf("reuse after runner failure error=%v", err)
	}
}

func TestMutationOneShotAllowsOnlyOneConcurrentExecution(t *testing.T) {
	target := mutationTarget()
	grant := mutationGrant(target, true)
	backend, err := NewOpsSSHMutationCommandBackend(ExecutionScope{Root: t.TempDir()}, mutationProjection(target), grant)
	if err != nil {
		t.Fatal(err)
	}
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) { return "ok", nil }
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := backend.Execute(context.Background(), mutationTask(target, "systemctl", "restart", "game-server"))
			results <- err
		}()
	}
	var successes, reuses int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrOpsMutationGrantReused):
			reuses++
		default:
			t.Fatalf("concurrent execution error=%v", err)
		}
	}
	if successes != 1 || reuses != 1 {
		t.Fatalf("concurrent one-shot results: successes=%d reuses=%d", successes, reuses)
	}
}

func TestMutationServiceRequiresExplicitBackendAuthorizer(t *testing.T) {
	target := mutationTarget()
	task := mutationTask(target, "systemctl", "restart", "game-server")
	plan := OpsPlan{PlanID: "plan-1", Goal: "mutate", Tasks: []OpsTask{task}, RollbackCriteria: []string{"restore"}, AcceptanceCriteria: []string{"done"}}
	service := NewOperationsDelegationService(&fakeReadOnlyOpsBackend{name: "legacy"})
	service.AllowMutations = true
	if _, err := service.Delegate(context.Background(), OpsDelegationRequest{Plan: plan}); !errors.Is(err, ErrOpsMutationGrantRequired) {
		t.Fatalf("error=%v", err)
	}
}
