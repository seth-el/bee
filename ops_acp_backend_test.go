package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeOpsACPTransport struct {
	config       ACPProcessConfig
	updates      chan ACPUpdate
	prompt       string
	initialized  ACPInitializeResult
	authParams   *ACPAuthenticateParams
	calls        []string
	started      bool
	closed       bool
	promptResult ACPPromptResult
	promptErr    error
}

func (transport *fakeOpsACPTransport) Start(context.Context) error {
	transport.calls = append(transport.calls, "start")
	transport.started = true
	return nil
}
func (transport *fakeOpsACPTransport) Initialize(context.Context, ACPInitializeParams) (ACPInitializeResult, error) {
	transport.calls = append(transport.calls, "initialize")
	if transport.initialized.ProtocolVersion == 0 {
		transport.initialized.ProtocolVersion = ACPProtocolVersion
	}
	return transport.initialized, nil
}
func (transport *fakeOpsACPTransport) Authenticate(_ context.Context, params ACPAuthenticateParams) error {
	transport.calls = append(transport.calls, "authenticate")
	copy := params
	transport.authParams = &copy
	return nil
}
func (transport *fakeOpsACPTransport) NewSession(context.Context, ACPSessionNewParams) (ACPSession, error) {
	transport.calls = append(transport.calls, "session/new")
	return ACPSession{SessionID: "ops-session-1"}, nil
}
func (transport *fakeOpsACPTransport) Prompt(_ context.Context, params ACPPromptParams) (ACPPromptResult, error) {
	transport.calls = append(transport.calls, "session/prompt")
	if len(params.Prompt) != 1 {
		return ACPPromptResult{}, errors.New("prompt shape mismatch")
	}
	transport.prompt = params.Prompt[0].Text
	transport.updates <- ACPUpdate{TaskID: "ops-task-1", ParentID: "ops-parent", Type: "agent_message", Text: "observed target safely", Info: "read-only evidence"}
	if transport.promptErr != nil {
		return ACPPromptResult{}, transport.promptErr
	}
	return transport.promptResult, nil
}
func (transport *fakeOpsACPTransport) Cancel(context.Context, string) error { return nil }
func (transport *fakeOpsACPTransport) Updates() <-chan ACPUpdate            { return transport.updates }
func (transport *fakeOpsACPTransport) Failure() error                       { return nil }
func (transport *fakeOpsACPTransport) Diagnostics() string                  { return "" }
func (transport *fakeOpsACPTransport) Close(context.Context) error {
	transport.closed = true
	return nil
}

func validOpsProjection(readOnly bool) OpsCapabilityProjection {
	return OpsCapabilityProjection{
		Target:            validReadOnlyOpsPlan().Tasks[0].Target,
		Capabilities:      []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute},
		MaxTimeoutSeconds: 5,
		MaxCommands:       8,
		MaxSteps:          16,
		MaxOutputBytes:    4096,
		ReadOnlyScout:     readOnly,
	}
}

func validOpsMutatingProjection() OpsCapabilityProjection {
	projection := validOpsProjection(false)
	projection.Capabilities = append(projection.Capabilities, OpsCapabilityMutate, OpsCapabilityRollback)
	return projection
}

func TestOpsCapabilityProjectionFailsClosedOnTargetAndAuthorityWidening(t *testing.T) {
	projection := validOpsProjection(true)
	if err := projection.Authorize(validReadOnlyOpsPlan().Tasks[0]); err != nil {
		t.Fatal(err)
	}
	mismatch := validReadOnlyOpsPlan().Tasks[0]
	mismatch.Target.Kubernetes.Name = "other"
	if err := projection.Authorize(mismatch); !errors.Is(err, ErrOpsTargetMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	mutating := validReadOnlyOpsPlan().Tasks[0]
	mutating.Intent = OpsIntentMutating
	mutating.RollbackCriteria = []string{"restore prior deployment"}
	if err := projection.Authorize(mutating); !errors.Is(err, ErrOpsCapabilityDenied) {
		t.Fatalf("mutating error = %v", err)
	}
	projection.Capabilities = append(projection.Capabilities, OpsCapability("all_tools"))
	if err := projection.Validate(); !errors.Is(err, ErrOpsAuthorityWidened) {
		t.Fatalf("widened error = %v", err)
	}
}

func TestOpsACPPacketContainsOnlyProjectedAuthority(t *testing.T) {
	plan := validReadOnlyOpsPlan()
	projection := validOpsProjection(true)
	packet, err := renderOpsACPPacket(plan, plan.Tasks[0], "ops-parent", projection, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) > 4096 {
		t.Fatalf("packet too large: %d", len(packet))
	}
	for _, forbidden := range []string{"remote_grant", "memory_projection", "OPENAI_API_KEY", "credential", "full_environment"} {
		if strings.Contains(packet, forbidden) {
			t.Fatalf("packet contains forbidden authority %q: %s", forbidden, packet)
		}
	}
	var decoded OpsACPPacket
	if err := json.Unmarshal([]byte(packet), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOpsACPReadOnlyScoutFakeBackendNeverMutates(t *testing.T) {
	projection := validOpsProjection(true)
	transport := &fakeOpsACPTransport{updates: make(chan ACPUpdate, 2), promptResult: ACPPromptResult{StopReason: "end_turn", Summary: "scout complete"}}
	backend, err := NewOpsACPReadOnlyScoutBackend(ACPProcessConfig{Command: "/bin/false"}, projection)
	if err != nil {
		t.Fatal(err)
	}
	backend.ParentID = "ops-parent"
	backend.TransportFactory = func(config ACPProcessConfig) (ACPTransport, error) {
		transport.config = config
		return transport, nil
	}
	plan := validReadOnlyOpsPlan()
	plan.PlanID = "ops-plan-scout"
	completions, err := backend.ExecutePlan(context.Background(), plan, projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(completions) != 1 || completions[0].Status != OpsCompletionSucceeded {
		t.Fatalf("completions = %#v", completions)
	}
	if !transport.started || !transport.closed {
		t.Fatalf("transport lifecycle started=%t closed=%t", transport.started, transport.closed)
	}
	if transport.config.EnvAllowlist != nil || transport.config.WorkingDir == "" {
		t.Fatalf("unsafe ACP config projected: %#v", transport.config)
	}
	if !strings.Contains(transport.prompt, "read-only scout") {
		t.Fatal("scout prompt did not retain read-only mode")
	}
}

func TestOpsACPRejectsMutatingScoutBeforeTransport(t *testing.T) {
	projection := validOpsProjection(true)
	transportCalls := 0
	backend, err := NewOpsACPReadOnlyScoutBackend(ACPProcessConfig{Command: "/bin/false"}, projection)
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) {
		transportCalls++
		return nil, errors.New("must not construct transport")
	}
	plan := validReadOnlyOpsPlan()
	plan.Tasks[0].Intent = OpsIntentMutating
	plan.Tasks[0].RollbackCriteria = []string{"restore previous revision"}
	plan.RollbackCriteria = []string{"restore previous revision"}
	mutatingProjection := projection
	mutatingProjection.ReadOnlyScout = false
	mutatingProjection.Capabilities = append(mutatingProjection.Capabilities, OpsCapabilityMutate, OpsCapabilityRollback)
	_, err = backend.ExecutePlan(context.Background(), plan, mutatingProjection)
	if err == nil {
		t.Fatal("mutating scout task accepted")
	}
	if transportCalls != 0 {
		t.Fatal("transport constructed for rejected mutation")
	}
}

func TestOpsACPRejectsEnvironmentAllowlist(t *testing.T) {
	_, err := NewOpsACPBackend(ACPProcessConfig{Command: "/bin/false", EnvAllowlist: []string{"PATH"}}, validOpsProjection(true))
	if err == nil || !strings.Contains(err.Error(), "environment allowlists") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpsACPMutatingProjectionRequiresRollbackAndAcceptance(t *testing.T) {
	projection := validOpsMutatingProjection()
	plan := validReadOnlyOpsPlan()
	plan.Tasks[0].Intent = OpsIntentMutating
	plan.Tasks[0].RollbackCriteria = []string{"restore previous deployment revision"}
	plan.RollbackCriteria = nil
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := projection.Authorize(plan.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	plan.Tasks[0].AcceptanceCriteria = nil
	if err := plan.Validate(); err == nil {
		t.Fatal("missing acceptance criteria accepted")
	}
}

func TestOpsACPTimeoutIsBounded(t *testing.T) {
	projection := validOpsProjection(true)
	transport := &fakeOpsACPTransport{updates: make(chan ACPUpdate, 1), promptResult: ACPPromptResult{StopReason: "end_turn"}, promptErr: context.DeadlineExceeded}
	backend, err := NewOpsACPReadOnlyScoutBackend(ACPProcessConfig{Command: "/bin/false", RequestTimeoutSeconds: 1}, projection)
	if err != nil {
		t.Fatal(err)
	}
	backend.ParentID = "ops-parent"
	backend.TransportFactory = func(config ACPProcessConfig) (ACPTransport, error) {
		transport.config = config
		return transport, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	completion, err := backend.Execute(ctx, validReadOnlyOpsPlan().Tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	if completion.Status != OpsCompletionUnavailable {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestOpsACPBackendIntegratesWithParentDelegationService(t *testing.T) {
	projection := validOpsProjection(true)
	transport := &fakeOpsACPTransport{updates: make(chan ACPUpdate, 2), promptResult: ACPPromptResult{StopReason: "end_turn", Summary: "service scout complete"}}
	backend, err := NewOpsACPReadOnlyScoutBackend(ACPProcessConfig{Command: "/bin/false"}, projection)
	if err != nil {
		t.Fatal(err)
	}
	backend.ParentID = "ops-parent"
	backend.TransportFactory = func(config ACPProcessConfig) (ACPTransport, error) {
		transport.config = config
		return transport, nil
	}
	service := NewOperationsDelegationService(backend)
	service.ParentID = "ops-parent"
	service.CapabilityProjection = &projection
	result, err := service.Delegate(context.Background(), OpsDelegationRequest{Backend: backend.Name(), Role: "scout", Plan: validReadOnlyOpsPlan()})
	if err != nil {
		t.Fatal(err)
	}
	if result.PlanID != "ops-plan-1" || result.ParentID != "ops-parent" || len(result.Completions) != 1 {
		t.Fatalf("result = %#v", result)
	}
}
