package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeReadOnlyOpsBackend struct {
	name string
	got  []OpsTask
}

func (backend *fakeReadOnlyOpsBackend) Name() string { return backend.name }
func (backend *fakeReadOnlyOpsBackend) Execute(_ context.Context, task OpsTask) (OpsCompletion, error) {
	backend.got = append(backend.got, task)
	return OpsCompletion{Status: OpsCompletionSucceeded, Summary: "Gemma scout observed target", Output: strings.Repeat("observation ", 500)}, nil
}

func validReadOnlyOpsPlan() OpsPlan {
	return OpsPlan{
		PlanID: "ops-plan-1", Goal: "inspect service health",
		Tasks: []OpsTask{{
			TaskID: "ops-task-1", Objective: "read service status", Intent: OpsIntentReadOnly,
			Target:             OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "lab", Namespace: "media", Resource: "deployment", Name: "api"}},
			Command:            []string{"get", "deployment/api", "-o", "wide"},
			Budget:             OpsBudget{TimeoutSeconds: 2, MaxCommands: 4, MaxSteps: 4, MaxOutputBytes: 4096},
			AcceptanceCriteria: []string{"report observed status"},
		}},
		AcceptanceCriteria: []string{"return a bounded status report"},
	}
}

func TestOpsTargetValidationExactPayloads(t *testing.T) {
	cases := []struct {
		name   string
		target OpsTarget
		ok     bool
	}{
		{"kubernetes", validReadOnlyOpsPlan().Tasks[0].Target, true},
		{"kubernetes context with user", OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Context: "admin@cluster", Namespace: "cert-manager", Resource: "pod", Name: "cert-manager"}}, true},
		{"ssh vm", OpsTarget{Kind: OpsTargetSSHVM, SSHVM: &SSHVMTarget{Host: "10.0.0.4", User: "ops", Port: 22, VMID: "vm-4"}}, true},
		{"local container", OpsTarget{Kind: OpsTargetLocalContainer, LocalContainer: &LocalContainerTarget{Runtime: "podman", Container: "api"}}, true},
		{"wrong payload", OpsTarget{Kind: OpsTargetKubernetes, SSHVM: &SSHVMTarget{Host: "10.0.0.4", User: "ops"}}, false},
		{"root user", OpsTarget{Kind: OpsTargetSSHVM, SSHVM: &SSHVMTarget{Host: "10.0.0.4", User: "root"}}, false},
		{"unsafe kubernetes name", OpsTarget{Kind: OpsTargetKubernetes, Kubernetes: &KubernetesTarget{Namespace: "media", Resource: "deployment", Name: "api;rm"}}, false},
		{"bad runtime", OpsTarget{Kind: OpsTargetLocalContainer, LocalContainer: &LocalContainerTarget{Runtime: "shell", Container: "api"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.target.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("validate error=%v, want ok=%t", err, tc.ok)
			}
		})
	}
}

func TestOpsPlanMutationRequiresRollback(t *testing.T) {
	plan := validReadOnlyOpsPlan()
	plan.Tasks[0].Intent = OpsIntentMutating
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("error = %v", err)
	}
	plan.RollbackCriteria = []string{"restore the previous deployment revision"}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationsDelegationDisabledWithoutExplicitBackend(t *testing.T) {
	service := NewOperationsDelegationService(nil)
	_, err := service.Delegate(context.Background(), OpsDelegationRequest{Plan: validReadOnlyOpsPlan()})
	if !errors.Is(err, ErrOperationsDelegationNotConfigured) {
		t.Fatalf("error = %v", err)
	}
}

func TestOperationsDelegationLocalChildScoutIsReadOnlyAndBounded(t *testing.T) {
	backend := &fakeReadOnlyOpsBackend{name: CodingBackendNameLocalChild}
	service := NewOperationsDelegationService(backend)
	service.Limits.MaxResultBytes = 4096
	result, err := service.Delegate(context.Background(), OpsDelegationRequest{Backend: CodingBackendNameLocalChild, Role: "scout", Plan: validReadOnlyOpsPlan()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != CodingBackendNameLocalChild || len(backend.got) != 1 || backend.got[0].Intent != OpsIntentReadOnly {
		t.Fatalf("result=%#v tasks=%#v", result, backend.got)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 4096 {
		t.Fatalf("result exceeded bound: %d", len(encoded))
	}
	if backend.got[0].Stdin != "" {
		t.Fatal("read-only scout unexpectedly received stdin")
	}
}

func TestOperationsDelegationMutationGating(t *testing.T) {
	backend := &fakeReadOnlyOpsBackend{name: "fake-ops"}
	service := NewOperationsDelegationService(backend)
	plan := validReadOnlyOpsPlan()
	plan.Tasks[0].Intent = OpsIntentMutating
	plan.RollbackCriteria = []string{"restore prior state"}
	_, err := service.Delegate(context.Background(), OpsDelegationRequest{Plan: plan})
	if !errors.Is(err, ErrOperationsMutationDisabled) {
		t.Fatalf("error = %v", err)
	}
	if len(backend.got) != 0 {
		t.Fatal("backend called while mutation was disabled")
	}
	service.AllowMutations = true
	if _, err := service.Delegate(context.Background(), OpsDelegationRequest{Plan: plan}); !errors.Is(err, ErrOpsMutationGrantRequired) {
		t.Fatalf("error=%v, want missing mutation grant", err)
	}
}

func TestOperationsDelegationDoesNotInheritParentAuthority(t *testing.T) {
	backend := &fakeReadOnlyOpsBackend{name: CodingBackendNameLocalChild}
	service := NewOperationsDelegationService(backend)
	plan := validReadOnlyOpsPlan()
	result, err := service.Delegate(context.Background(), OpsDelegationRequest{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.got) != 1 || result.ParentID != "bee-parent" {
		t.Fatalf("result=%#v task=%#v", result, backend.got)
	}
	wire, err := json.Marshal(backend.got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "remote_grant") || strings.Contains(string(wire), "trusted_foreground") || strings.Contains(string(wire), "credential") {
		t.Fatalf("authority crossed operations task boundary: %s", wire)
	}
}

func TestDelegateOperationsToolRejectsUnknownFields(t *testing.T) {
	backend := &fakeReadOnlyOpsBackend{name: CodingBackendNameLocalChild}
	tool := NewOperationsDelegationTool(NewOperationsDelegationService(backend))
	_, err := tool.Execute(context.Background(), []byte(`{"unexpected":true}`))
	if err == nil {
		t.Fatal("expected strict argument error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpsCompletionValidation(t *testing.T) {
	completion := OpsCompletion{TaskID: "task", ParentID: "parent", Status: OpsCompletionSucceeded, Summary: "ok"}
	if err := completion.Validate(); err != nil {
		t.Fatal(err)
	}
}
