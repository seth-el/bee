package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func testOpsProjection(target OpsTarget) OpsCapabilityProjection {
	return OpsCapabilityProjection{
		Target: target,
		Capabilities: []OpsCapability{
			OpsCapabilityRead,
			OpsCapabilityExecute,
		},
		MaxTimeoutSeconds: 3,
		MaxCommands:       8,
		MaxSteps:          8,
		MaxOutputBytes:    4096,
		ReadOnlyScout:     true,
	}
}

func testOpsTask(target OpsTarget, command ...string) OpsTask {
	return OpsTask{
		TaskID:    "ops-task-1",
		Objective: "inspect target",
		Target:    target,
		Intent:    OpsIntentReadOnly,
		Command:   command,
		Budget: OpsBudget{
			TimeoutSeconds: 2,
			MaxCommands:    8,
			MaxSteps:       8,
			MaxOutputBytes: 128,
		},
		AcceptanceCriteria: []string{"return bounded evidence"},
	}
}

func TestOpsTargetAcceptsFlatModelFacingJSON(t *testing.T) {
	var target OpsTarget
	if err := json.Unmarshal([]byte(`{"kind":"kubernetes","context":"lab","namespace":"cert-manager","resource":"certificate","name":"api-cert"}`), &target); err != nil {
		t.Fatal(err)
	}
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	if target.Kubernetes == nil || target.Kubernetes.Name != "api-cert" {
		t.Fatalf("target=%#v", target)
	}
}

func TestOpsCommandBackendBindsKubernetesTarget(t *testing.T) {
	target := OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "cert-manager", Resource: "certificate", Name: "api-cert"}}
	backend, err := NewOpsReadOnlyCommandBackend(ExecutionScope{Root: t.TempDir()}, testOpsProjection(target))
	if err != nil {
		t.Fatal(err)
	}
	var got OpsCommandRequest
	backend.Runner = func(_ context.Context, request OpsCommandRequest, scope ExecutionScope) (string, error) {
		got = request
		if len(scope.EnvAllowlist) != 0 || scope.RemoteGrant != nil {
			t.Fatalf("authority leaked into command runner: %#v", scope)
		}
		return "certificate ready", nil
	}
	completion, err := backend.Execute(context.Background(), testOpsTask(target, "get", "certificate/api-cert", "-o", "wide"))
	if err != nil {
		t.Fatal(err)
	}
	if completion.Status != OpsCompletionSucceeded || completion.Output != "certificate ready" {
		t.Fatalf("completion=%#v", completion)
	}
	want := []string{"kubectl", "--context", "lab", "--namespace", "cert-manager", "get", "certificate/api-cert", "-o", "wide"}
	if strings.Join(got.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv=%#v want %#v", got.Argv, want)
	}
}

func TestOpsCommandBackendNormalizesCompactKubernetesCommand(t *testing.T) {
	task := validReadOnlyOpsPlan().Tasks[0]
	task.Command = []string{"describe deployment/api"}
	request, err := opsCommandRequest(task)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "--context", "lab", "--namespace", "media", "describe", "deployment/api"}
	if !reflect.DeepEqual(request.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", request.Argv, want)
	}
}

func TestOpsCommandBackendRejectsTargetMismatch(t *testing.T) {
	projected := OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "cert-manager", Resource: "certificate", Name: "api-cert"}}
	requested := OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "cert-manager", Resource: "certificate", Name: "other-cert"}}
	backend, err := NewOpsReadOnlyCommandBackend(ExecutionScope{Root: t.TempDir()}, testOpsProjection(projected))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	backend.Runner = func(context.Context, OpsCommandRequest, ExecutionScope) (string, error) {
		called = true
		return "", nil
	}
	_, err = backend.Execute(context.Background(), testOpsTask(requested, "get", "certificate/other-cert"))
	if !errors.Is(err, ErrOpsTargetMismatch) {
		t.Fatalf("error=%v, want target mismatch", err)
	}
	if called {
		t.Fatal("runner called after exact target mismatch")
	}
}

func TestOpsCommandBackendDeniesMutation(t *testing.T) {
	target := OpsTarget{Kind: OpsTargetSSHVM, SSHVM: &SSHVMTarget{Host: "10.0.0.4", User: "ops", Port: 22}}
	backend, err := NewOpsReadOnlyCommandBackend(ExecutionScope{Root: t.TempDir(), TrustedForeground: true}, testOpsProjection(target))
	if err != nil {
		t.Fatal(err)
	}
	task := testOpsTask(target, "systemctl", "restart", "api")
	task.Intent = OpsIntentMutating
	_, err = backend.Execute(context.Background(), task)
	if !errors.Is(err, ErrOperationsMutationDisabled) {
		t.Fatalf("error=%v, want mutation denial", err)
	}
}

func TestOpsCommandBackendDoesNotInheritSecretsOrParentGrants(t *testing.T) {
	target := OpsTarget{Kind: OpsTargetLocalContainer, LocalContainer: &LocalContainerTarget{Runtime: "podman", Container: "api"}}
	backend, err := NewOpsLocalChildReadOnlyScoutBackend(ExecutionScope{
		Root:              t.TempDir(),
		EnvAllowlist:      []string{"PATH", "OPENAI_API_KEY"},
		RemoteGrant:       &RemoteCapabilityGrant{Host: "10.0.0.4", User: "ops", Port: 22},
		TrustedForeground: true,
	}, testOpsProjection(target))
	if err != nil {
		t.Fatal(err)
	}
	backend.Runner = func(_ context.Context, request OpsCommandRequest, scope ExecutionScope) (string, error) {
		if len(scope.EnvAllowlist) != 0 {
			t.Fatalf("secret-capable env allowlist inherited: %#v", scope.EnvAllowlist)
		}
		if scope.RemoteGrant != nil {
			t.Fatal("remote grant inherited")
		}
		if !scope.TrustedForeground {
			t.Fatal("parent command authority lost trusted foreground execution")
		}
		if strings.Join(request.Argv, " ") != "podman exec api ps" {
			t.Fatalf("argv=%#v", request.Argv)
		}
		return "PID 1", nil
	}
	if _, err := backend.Execute(context.Background(), testOpsTask(target, "ps")); err != nil {
		t.Fatal(err)
	}
}

func TestComposeGeneralAssistantExposesConfiguredOperationsTool(t *testing.T) {
	target := OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "cert-manager", Resource: "certificate", Name: "api-cert"}}
	config := DefaultConfig()
	memoryRoot := t.TempDir()
	if err := os.Chmod(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config.Operations = OperationsAssistantConfig{
		Enabled:       true,
		Backend:       CodingBackendNameLocalChild,
		Target:        target,
		Capabilities:  []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute},
		ReadOnlyScout: true,
	}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{
		Store:      mustAssistantStore(t),
		ScopeRoot:  t.TempDir(),
		MemoryRoot: memoryRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.OperationsDelegation == nil {
		t.Fatal("configured operations service missing")
	}
	if _, ok := app.Executor.Registry.Lookup(OperationsDelegationToolName); !ok {
		t.Fatal("delegate_operations missing from ordinary assistant registry")
	}
	found := 0
	for _, name := range app.Capabilities.ToolNames {
		if name == OperationsDelegationToolName {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("delegate_operations exposure count=%d names=%#v", found, app.Capabilities.ToolNames)
	}
}

func TestOperationsAssistantConfigKeepsMutationsDisabled(t *testing.T) {
	config := DefaultOperationsAssistantConfig()
	config.Enabled = true
	config.Target = OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "cert-manager", Resource: "certificate", Name: "api-cert"}}
	config.AllowMutations = true
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error=%v, want read-only mutation rejection", err)
	}
}
