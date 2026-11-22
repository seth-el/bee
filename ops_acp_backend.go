package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// OpsCapability is a deliberately small, provider-neutral operation grant.
// It is not a Bee tool name, SSH grant, environment variable, or credential.
type OpsCapability string

const (
	OpsCapabilityObserve  OpsCapability = "observe"
	OpsCapabilityRead     OpsCapability = "read"
	OpsCapabilityExecute  OpsCapability = "execute"
	OpsCapabilityStdin    OpsCapability = "stdin"
	OpsCapabilityMutate   OpsCapability = "mutate"
	OpsCapabilityRollback OpsCapability = "rollback"

	// Compatibility aliases for callers that describe command execution as a
	// command capability and read-only work as a read-only capability.
	OpsCapabilityCommand  OpsCapability = "command"
	OpsCapabilityReadOnly OpsCapability = "read_only"
)

var (
	ErrOpsCapabilityProjectionRequired = errors.New("operations capability projection is required")
	ErrOpsCapabilityDenied             = errors.New("operations capability is not projected")
	ErrOpsTargetMismatch               = errors.New("operations target does not match projected target")
	ErrOpsAuthorityWidened             = errors.New("operations authority widening is not permitted")
)

const (
	defaultOpsACPPacketBytes = 32 << 10
	maxOpsACPEvidenceItems   = 24
	maxOpsACPEvidenceBytes   = 2048
)

// OpsCapabilityProjection is the only authority an ACP operations adapter may
// receive. It contains one exact target and a narrow allowlist of operation
// capabilities. It intentionally has no environment, memory, secret, grant,
// filesystem, or provider fields.
type OpsCapabilityProjection struct {
	Target            OpsTarget       `json:"target"`
	Capabilities      []OpsCapability `json:"capabilities"`
	MaxTimeoutSeconds int             `json:"max_timeout_seconds,omitempty"`
	MaxCommands       int             `json:"max_commands,omitempty"`
	MaxSteps          int             `json:"max_steps,omitempty"`
	MaxOutputBytes    int             `json:"max_output_bytes,omitempty"`
	ReadOnlyScout     bool            `json:"read_only_scout,omitempty"`
}

func (projection OpsCapabilityProjection) effective() OpsCapabilityProjection {
	if projection.MaxTimeoutSeconds <= 0 {
		projection.MaxTimeoutSeconds = defaultOpsMaxTimeoutSeconds
	}
	if projection.MaxCommands <= 0 {
		projection.MaxCommands = defaultOpsMaxCommands
	}
	if projection.MaxSteps <= 0 {
		projection.MaxSteps = defaultOpsMaxSteps
	}
	if projection.MaxOutputBytes <= 0 {
		projection.MaxOutputBytes = defaultOpsMaxOutputBytes
	}
	return projection
}

func canonicalOpsTarget(target OpsTarget) (OpsTarget, error) {
	if err := target.Validate(); err != nil {
		return OpsTarget{}, err
	}
	if target.SSHVM != nil && target.SSHVM.Port == 0 {
		copy := *target.SSHVM
		copy.Port = 22
		target.SSHVM = &copy
	}
	return target, nil
}

func canonicalOpsCapabilities(values []OpsCapability) ([]OpsCapability, error) {
	if len(values) == 0 {
		return nil, ErrOpsCapabilityProjectionRequired
	}
	seen := make(map[OpsCapability]struct{}, len(values))
	result := make([]OpsCapability, 0, len(values))
	for _, raw := range values {
		value := raw
		switch raw {
		case OpsCapabilityCommand:
			value = OpsCapabilityExecute
		case OpsCapabilityReadOnly:
			value = OpsCapabilityRead
		}
		switch value {
		case OpsCapabilityObserve, OpsCapabilityRead, OpsCapabilityExecute, OpsCapabilityStdin, OpsCapabilityMutate, OpsCapabilityRollback:
		default:
			return nil, fmt.Errorf("%w: %q is unknown", ErrOpsAuthorityWidened, raw)
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("operations capability %q repeats", value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func (projection OpsCapabilityProjection) Validate() error {
	target, err := canonicalOpsTarget(projection.Target)
	if err != nil {
		return fmt.Errorf("operations projection target: %w", err)
	}
	projection.Target = target
	capabilities, err := canonicalOpsCapabilities(projection.Capabilities)
	if err != nil {
		return err
	}
	projection.Capabilities = capabilities
	if projection.MaxTimeoutSeconds < 0 || projection.MaxCommands < 0 || projection.MaxSteps < 0 || projection.MaxOutputBytes < 0 {
		return errors.New("operations projection limits must not be negative")
	}
	if projection.ReadOnlyScout && hasOpsCapability(capabilities, OpsCapabilityMutate) {
		return fmt.Errorf("%w: read-only scout cannot project mutate", ErrOpsAuthorityWidened)
	}
	return nil
}

func hasOpsCapability(capabilities []OpsCapability, wanted OpsCapability) bool {
	for _, value := range capabilities {
		if value == wanted || (wanted == OpsCapabilityExecute && value == OpsCapabilityCommand) || (wanted == OpsCapabilityRead && value == OpsCapabilityReadOnly) {
			return true
		}
	}
	return false
}

func (projection OpsCapabilityProjection) Authorize(task OpsTask) error {
	if err := task.Validate(); err != nil {
		return fmt.Errorf("operations task: %w", err)
	}
	if err := projection.Validate(); err != nil {
		return err
	}
	projected, _ := canonicalOpsTarget(projection.Target)
	requested, _ := canonicalOpsTarget(task.Target)
	projectedJSON, _ := json.Marshal(projected)
	requestedJSON, _ := json.Marshal(requested)
	if string(projectedJSON) != string(requestedJSON) {
		return fmt.Errorf("%w: requested=%s projected=%s", ErrOpsTargetMismatch, requestedJSON, projectedJSON)
	}
	effective := projection.effective()
	if task.Budget.TimeoutSeconds > effective.MaxTimeoutSeconds || task.Budget.MaxCommands > effective.MaxCommands || task.Budget.MaxSteps > effective.MaxSteps || task.Budget.MaxOutputBytes > effective.MaxOutputBytes {
		return errors.New("operations task exceeds projected capability budget")
	}
	if !hasOpsCapability(projection.Capabilities, OpsCapabilityRead) && !hasOpsCapability(projection.Capabilities, OpsCapabilityObserve) {
		return fmt.Errorf("%w: read or observe is required", ErrOpsCapabilityDenied)
	}
	if !hasOpsCapability(projection.Capabilities, OpsCapabilityExecute) {
		return fmt.Errorf("%w: execute is required", ErrOpsCapabilityDenied)
	}
	if task.Stdin != "" && !hasOpsCapability(projection.Capabilities, OpsCapabilityStdin) {
		return fmt.Errorf("%w: stdin is not projected", ErrOpsCapabilityDenied)
	}
	if task.Intent == OpsIntentMutating {
		if projection.ReadOnlyScout {
			return fmt.Errorf("%w: read-only scout cannot execute mutating tasks", ErrOpsCapabilityDenied)
		}
		if !hasOpsCapability(projection.Capabilities, OpsCapabilityMutate) || !hasOpsCapability(projection.Capabilities, OpsCapabilityRollback) {
			return fmt.Errorf("%w: mutating tasks require mutate and rollback", ErrOpsCapabilityDenied)
		}
	}
	return nil
}

// OpsACPPacket is the bounded, auditable task packet rendered to an ACP
// adapter. It is descriptive only; it does not contain credentials or grants.
type OpsACPPacket struct {
	Protocol           string          `json:"protocol"`
	PlanID             string          `json:"plan_id,omitempty"`
	ParentID           string          `json:"parent_id"`
	TaskID             string          `json:"task_id"`
	Goal               string          `json:"goal"`
	Objective          string          `json:"objective"`
	Intent             OpsIntent       `json:"intent"`
	Target             OpsTarget       `json:"target"`
	Capabilities       []OpsCapability `json:"capabilities"`
	OperationClass     string          `json:"operation_class,omitempty"`
	Command            []string        `json:"command"`
	Stdin              string          `json:"stdin,omitempty"`
	Budget             OpsBudget       `json:"budget"`
	RollbackCriteria   []string        `json:"rollback_criteria,omitempty"`
	AcceptanceCriteria []string        `json:"acceptance_criteria"`
}

func (packet OpsACPPacket) Validate() error {
	if packet.Protocol != "bee.ops.v1" {
		return errors.New("operations ACP packet protocol is invalid")
	}
	if err := validateChildID("operations ACP parent_id", packet.ParentID); err != nil {
		return err
	}
	if err := validateChildID("operations ACP task_id", packet.TaskID); err != nil {
		return err
	}
	if strings.TrimSpace(packet.Goal) == "" || strings.TrimSpace(packet.Objective) == "" {
		return errors.New("operations ACP packet goal and objective are required")
	}
	if err := (OpsTask{TaskID: packet.TaskID, Objective: packet.Objective, Target: packet.Target, Intent: packet.Intent, OperationClass: packet.OperationClass, Command: packet.Command, Stdin: packet.Stdin, Budget: packet.Budget, RollbackCriteria: packet.RollbackCriteria, AcceptanceCriteria: packet.AcceptanceCriteria}).Validate(); err != nil {
		return err
	}
	_, err := canonicalOpsCapabilities(packet.Capabilities)
	return err
}

func renderOpsACPPacket(plan OpsPlan, task OpsTask, parentID string, projection OpsCapabilityProjection, maxBytes int) (string, error) {
	if err := plan.Validate(); err != nil {
		return "", fmt.Errorf("operations plan: %w", err)
	}
	if err := projection.Authorize(task); err != nil {
		return "", err
	}
	if strings.TrimSpace(parentID) == "" {
		return "", errors.New("operations ACP parent id is required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultOpsACPPacketBytes
	}
	packetTarget, _ := canonicalOpsTarget(task.Target)
	packet := OpsACPPacket{Protocol: "bee.ops.v1", PlanID: plan.PlanID, ParentID: parentID, TaskID: task.TaskID, Goal: plan.Goal, Objective: task.Objective, Intent: task.Intent, OperationClass: task.OperationClass, Target: packetTarget, Capabilities: append([]OpsCapability(nil), projection.Capabilities...), Command: append([]string(nil), task.Command...), Budget: task.Budget, RollbackCriteria: append([]string(nil), plan.RollbackCriteria...), AcceptanceCriteria: append(append([]string(nil), plan.AcceptanceCriteria...), task.AcceptanceCriteria...)}
	if task.Intent == OpsIntentMutating {
		packet.RollbackCriteria = append(packet.RollbackCriteria, task.RollbackCriteria...)
	}
	if task.Stdin != "" {
		packet.Stdin = task.Stdin
	}
	if err := packet.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return "", err
	}
	if len(data) > maxBytes {
		return "", errors.New("operations ACP packet exceeds bound")
	}
	return string(data), nil
}

// OpsACPBackend is a parent-owned ACP operations adapter. It never invokes
// commands itself. An ACP process receives only the packet and a fresh empty
// working directory; permission requests remain denied unless a future,
// separately reviewed operations authority explicitly composes a handler.
type OpsACPBackend struct {
	Config           ACPProcessConfig
	BackendName      string
	ParentID         string
	Projection       OpsCapabilityProjection
	ReadOnlyScout    bool
	MaxPacketBytes   int
	TransportFactory ACPTransportFactory
}

var _ OpsBackend = (*OpsACPBackend)(nil)

func NewOpsACPBackend(config ACPProcessConfig, projection OpsCapabilityProjection) (*OpsACPBackend, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if len(config.EnvAllowlist) != 0 {
		return nil, errors.New("operations ACP backend refuses environment allowlists")
	}
	if err := projection.Validate(); err != nil {
		return nil, err
	}
	return &OpsACPBackend{Config: config, BackendName: "acp-ops", ParentID: "bee-parent", Projection: projection, MaxPacketBytes: defaultOpsACPPacketBytes, TransportFactory: func(config ACPProcessConfig) (ACPTransport, error) {
		return NewACPStdioTransport(config)
	}}, nil
}

// NewOpsACPReadOnlyScoutBackend constructs the separate read-only scout path.
func NewOpsACPReadOnlyScoutBackend(config ACPProcessConfig, projection OpsCapabilityProjection) (*OpsACPBackend, error) {
	projection.ReadOnlyScout = true
	backend, err := NewOpsACPBackend(config, projection)
	if err != nil {
		return nil, err
	}
	backend.BackendName = "acp-ops-scout"
	backend.ReadOnlyScout = true
	return backend, nil
}

func (backend *OpsACPBackend) Name() string {
	if backend == nil || strings.TrimSpace(backend.BackendName) == "" {
		return "acp-ops"
	}
	return backend.BackendName
}

// Execute implements OpsBackend for the existing per-task delegation seam.
// Plan identity is intentionally left for the parent service to fill in.
func (backend *OpsACPBackend) Execute(ctx context.Context, task OpsTask) (OpsCompletion, error) {
	plan := OpsPlan{PlanID: "ops-plan-unknown", Goal: task.Objective, Tasks: []OpsTask{task}, AcceptanceCriteria: append([]string(nil), task.AcceptanceCriteria...)}
	if task.Intent == OpsIntentMutating {
		plan.RollbackCriteria = append([]string(nil), task.RollbackCriteria...)
	}
	completion, err := backend.executeTask(ctx, plan, task)
	// The legacy OpsBackend interface has no plan argument. Leave identity
	// blank so OperationsDelegationService can bind the parent request's plan
	// id instead of accepting a backend-invented identity.
	completion.PlanID = ""
	return completion, err
}

// ExecutePlan consumes a validated parent-owned plan and projection in one
// explicit call. It is useful to composition roots that want plan identity in
// ACP packets rather than using the legacy OpsBackend per-task seam.
func (backend *OpsACPBackend) ExecutePlan(ctx context.Context, plan OpsPlan, projection OpsCapabilityProjection) ([]OpsCompletion, error) {
	if backend == nil {
		return nil, errors.New("operations ACP backend is nil")
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := projection.Validate(); err != nil {
		return nil, err
	}
	if err := projectionWithin(backend.Projection, projection); err != nil {
		return nil, err
	}
	if backend.ReadOnlyScout && !projection.ReadOnlyScout {
		return nil, fmt.Errorf("%w: scout backend cannot widen projection", ErrOpsAuthorityWidened)
	}
	completions := make([]OpsCompletion, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		completion, err := backend.executeTaskWithProjection(ctx, plan, task, projection)
		completions = append(completions, completion)
		if err != nil {
			return completions, err
		}
	}
	return completions, nil
}

func (backend *OpsACPBackend) executeTask(ctx context.Context, plan OpsPlan, task OpsTask) (OpsCompletion, error) {
	return backend.executeTaskWithProjection(ctx, plan, task, backend.Projection)
}

func projectionWithin(approved, requested OpsCapabilityProjection) error {
	if err := approved.Validate(); err != nil {
		return err
	}
	if err := requested.Validate(); err != nil {
		return err
	}
	approvedTarget, _ := canonicalOpsTarget(approved.Target)
	requestedTarget, _ := canonicalOpsTarget(requested.Target)
	approvedJSON, _ := json.Marshal(approvedTarget)
	requestedJSON, _ := json.Marshal(requestedTarget)
	if string(approvedJSON) != string(requestedJSON) {
		return fmt.Errorf("%w: projection target changed", ErrOpsTargetMismatch)
	}
	approvedCaps, _ := canonicalOpsCapabilities(approved.Capabilities)
	requestedCaps, _ := canonicalOpsCapabilities(requested.Capabilities)
	for _, capability := range requestedCaps {
		if !hasOpsCapability(approvedCaps, capability) {
			return fmt.Errorf("%w: capability %q was not parent-approved", ErrOpsAuthorityWidened, capability)
		}
	}
	approvedLimits := approved.effective()
	requestedLimits := requested.effective()
	if requestedLimits.MaxTimeoutSeconds > approvedLimits.MaxTimeoutSeconds || requestedLimits.MaxCommands > approvedLimits.MaxCommands || requestedLimits.MaxSteps > approvedLimits.MaxSteps || requestedLimits.MaxOutputBytes > approvedLimits.MaxOutputBytes {
		return fmt.Errorf("%w: projection budget widened", ErrOpsAuthorityWidened)
	}
	if approved.ReadOnlyScout && !requested.ReadOnlyScout {
		return fmt.Errorf("%w: read-only scout flag was removed", ErrOpsAuthorityWidened)
	}
	return nil
}

func (backend *OpsACPBackend) executeTaskWithProjection(ctx context.Context, plan OpsPlan, task OpsTask, projection OpsCapabilityProjection) (OpsCompletion, error) {
	completion := OpsCompletion{PlanID: plan.PlanID, TaskID: task.TaskID, ParentID: backend.ParentID, Status: OpsCompletionUnavailable}
	if backend == nil {
		return completion, errors.New("operations ACP backend is nil")
	}
	if strings.TrimSpace(completion.ParentID) == "" {
		completion.ParentID = "bee-parent"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := plan.Validate(); err != nil {
		return completion, err
	}
	if err := projection.Authorize(task); err != nil {
		completion.Status = OpsCompletionRejected
		completion.Summary = truncateText(err.Error(), 2048)
		completion.Blockers = []string{truncateText(err.Error(), 1024)}
		return completion, err
	}
	if backend.ReadOnlyScout && task.Intent != OpsIntentReadOnly {
		return completion, fmt.Errorf("%w: scout backend only accepts read-only tasks", ErrOpsCapabilityDenied)
	}
	packet, err := renderOpsACPPacket(plan, task, completion.ParentID, projection, backend.MaxPacketBytes)
	if err != nil {
		completion.Status = OpsCompletionRejected
		completion.Summary = truncateText(err.Error(), 2048)
		completion.Blockers = []string{truncateText(err.Error(), 1024)}
		return completion, err
	}
	return backend.runACP(ctx, task, completion, packet, projection)
}

func (backend *OpsACPBackend) runACP(ctx context.Context, task OpsTask, completion OpsCompletion, packet string, projection OpsCapabilityProjection) (OpsCompletion, error) {
	projection = projection.effective()
	budget := task.Budget
	if budget.TimeoutSeconds <= 0 || budget.TimeoutSeconds > projection.MaxTimeoutSeconds {
		budget.TimeoutSeconds = projection.MaxTimeoutSeconds
	}
	maxOutput := budget.MaxOutputBytes
	if maxOutput <= 0 || maxOutput > projection.MaxOutputBytes {
		maxOutput = projection.MaxOutputBytes
	}
	callCtx, cancel := contextWithOptionalTimeout(ctx, time.Duration(budget.TimeoutSeconds)*time.Second)
	defer cancel()

	workdir, err := os.MkdirTemp("", "bee-ops-acp-")
	if err != nil {
		return completion, err
	}
	defer os.RemoveAll(workdir)
	config := backend.Config
	config.WorkingDir = workdir
	config.EnvAllowlist = nil
	factory := backend.TransportFactory
	if factory == nil {
		factory = func(config ACPProcessConfig) (ACPTransport, error) { return NewACPStdioTransport(config) }
	}
	transport, err := factory(config)
	if err != nil {
		completion.Status = OpsCompletionFailed
		completion.Summary = "operations ACP transport construction failed: " + truncateText(err.Error(), 1024)
		return completion, err
	}
	if transport == nil {
		completion.Status = OpsCompletionFailed
		completion.Summary = "operations ACP transport factory returned nil"
		return completion, errors.New("operations ACP transport factory returned nil")
	}
	defer func() { _ = transport.Close(context.Background()) }()
	if err := transport.Start(callCtx); err != nil {
		completion.Status = opsStatusForError(callCtx, err)
		completion.Summary = "operations ACP transport start failed: " + truncateText(err.Error(), 1024)
		return completion, err
	}
	initializeCtx, cancelInitialize := context.WithTimeout(callCtx, config.startupTimeout())
	initialized, err := transport.Initialize(initializeCtx, ACPInitializeParams{ProtocolVersion: ACPProtocolVersion, ClientCapabilities: ACPClientCapabilities{}, ClientInfo: ACPImplementation{Name: "bee-ops", Version: "dev"}})
	cancelInitialize()
	if err != nil {
		completion.Status = opsStatusForError(callCtx, err)
		completion.Summary = acpFailureSummary(transport, "operations ACP initialize failed: "+err.Error(), "")
		return completion, nil
	}
	authCtx, cancelAuth := contextWithOptionalTimeout(callCtx, config.requestTimeout())
	authSecret, authErr := authenticateACPIfRequired(authCtx, config, transport, initialized)
	cancelAuth()
	if authErr != nil {
		completion.Status = opsStatusForError(callCtx, authErr)
		completion.Summary = acpFailureSummary(transport, "operations ACP authentication failed: "+authErr.Error(), authSecret)
		return completion, nil
	}
	sessionCtx, cancelSession := contextWithOptionalTimeout(callCtx, config.requestTimeout())
	session, err := transport.NewSession(sessionCtx, ACPSessionNewParams{Cwd: workdir, MCPServers: []any{}})
	cancelSession()
	if err != nil {
		completion.Status = opsStatusForError(callCtx, err)
		completion.Summary = acpFailureSummary(transport, "operations ACP session creation failed: "+err.Error(), authSecret)
		return completion, nil
	}
	collector := &opsACPCollector{taskID: task.TaskID, parentID: completion.ParentID, maxOutput: maxOutput}
	stopUpdates := make(chan struct{})
	updatesDone := make(chan struct{})
	go func() {
		defer close(updatesDone)
		for {
			select {
			case update, ok := <-transport.Updates():
				if !ok {
					return
				}
				collector.observe(redactACPUpdate(update, authSecret))
			case <-stopUpdates:
				return
			case <-callCtx.Done():
				return
			}
		}
	}()
	promptCtx, cancelPrompt := contextWithOptionalTimeout(callCtx, config.requestTimeout())
	prompt := buildOpsACPPrompt(packet, backend.ReadOnlyScout || projection.ReadOnlyScout)
	promptResult, promptErr := transport.Prompt(promptCtx, ACPPromptParams{SessionID: session.SessionID, Prompt: []ACPContentBlock{{Type: "text", Text: prompt}}})
	cancelPrompt()
	close(stopUpdates)
	<-updatesDone
	if failure := collector.failure(); failure != nil {
		completion.Status = OpsCompletionFailed
		completion.Summary = "operations ACP update rejected: " + truncateText(failure.Error(), 1024)
		completion.Blockers = []string{truncateText(failure.Error(), 1024)}
		return completion, failure
	}
	if promptErr != nil {
		completion.Status = opsStatusForError(callCtx, promptErr)
		completion.Summary = acpFailureSummary(transport, "operations ACP prompt failed: "+promptErr.Error(), authSecret)
		completion.Output, completion.OutputTruncated = truncateOutput(collector.summary(), maxOutput)
		completion.Evidence = collector.evidence()
		return completion, nil
	}
	completion.Status = OpsCompletionSucceeded
	completion.Summary = redactACPSecret(strings.TrimSpace(promptResult.Summary), authSecret)
	if completion.Summary == "" {
		completion.Summary = collector.summary()
	}
	if completion.Summary == "" {
		completion.Summary = "operations ACP adapter completed"
	}
	completion.Summary = truncateText(completion.Summary, 2048)
	completion.Output, completion.OutputTruncated = truncateOutput(collector.summary(), maxOutput)
	completion.Evidence = collector.evidence()
	if promptResult.StopReason == "cancelled" {
		completion.Status = OpsCompletionUnavailable
		completion.Blockers = []string{"operations ACP prompt cancelled"}
	}
	if err := completion.Validate(); err != nil {
		completion.Status = OpsCompletionFailed
		completion.Summary = "operations ACP produced an invalid completion"
		completion.Blockers = appendUnique(completion.Blockers, err.Error())
		return completion, err
	}
	return boundOpsCompletion(completion, maxOutput), nil
}

func buildOpsACPPrompt(packet string, readOnly bool) string {
	mode := "operator"
	if readOnly {
		mode = "read-only scout"
	}
	return "You are Bee's parent-owned operations ACP adapter in " + mode + " mode. The JSON packet below is the complete authority for this task. Do not request or infer credentials, SSH grants, kubeconfig, memory, secrets, filesystem paths, environment variables, extra targets, extra tools, or broader capabilities. Do not widen the target or command. Return bounded descriptive evidence only; the Bee parent decides whether any reported action is accepted. For read-only scout mode, never mutate. For mutating mode, rollback and acceptance criteria are mandatory and must be reported.\n\nOPS_PACKET_JSON:\n" + packet
}

func opsStatusForError(ctx context.Context, err error) OpsCompletionStatus {
	if errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return OpsCompletionUnavailable
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return OpsCompletionUnavailable
	}
	return OpsCompletionFailed
}

type opsACPCollector struct {
	mu              sync.Mutex
	taskID          string
	parentID        string
	maxOutput       int
	text            strings.Builder
	outputBytes     int
	outputTruncated bool
	evidenceValues  []string
	failureErr      error
}

func (collector *opsACPCollector) observe(update ACPUpdate) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.failureErr != nil {
		return
	}
	if update.TaskID != "" && update.TaskID != collector.taskID {
		collector.failureErr = fmt.Errorf("operations ACP update task id %q does not match %q", update.TaskID, collector.taskID)
		return
	}
	if update.ParentID != "" && update.ParentID != collector.parentID {
		collector.failureErr = fmt.Errorf("operations ACP update parent id %q does not match %q", update.ParentID, collector.parentID)
		return
	}
	if update.Text != "" {
		remaining := collector.maxOutput - collector.outputBytes
		if remaining <= 0 {
			collector.outputTruncated = true
		} else {
			value := update.Text
			if len(value) > remaining {
				value = value[:remaining]
				collector.outputTruncated = true
			}
			collector.text.WriteString(value)
			collector.outputBytes += len(value)
		}
	}
	for _, value := range []string{update.Info, update.Artifact, update.Diff} {
		if strings.TrimSpace(value) != "" {
			collector.evidenceValues = appendBoundedEvidence(collector.evidenceValues, value)
		}
	}
	if update.Completion != nil {
		if update.Completion.TaskID != "" && update.Completion.TaskID != collector.taskID {
			collector.failureErr = fmt.Errorf("operations ACP completion task id %q does not match %q", update.Completion.TaskID, collector.taskID)
			return
		}
		if update.Completion.ParentID != "" && update.Completion.ParentID != collector.parentID {
			collector.failureErr = fmt.Errorf("operations ACP completion parent id %q does not match %q", update.Completion.ParentID, collector.parentID)
			return
		}
		collector.evidenceValues = appendBoundedEvidence(collector.evidenceValues, update.Completion.Summary)
	}
}

func appendBoundedEvidence(values []string, value string) []string {
	value = truncateText(value, maxOpsACPEvidenceBytes)
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if len(values) >= maxOpsACPEvidenceItems {
		return values
	}
	return append(values, value)
}

func (collector *opsACPCollector) summary() string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return strings.TrimSpace(collector.text.String())
}

func (collector *opsACPCollector) evidence() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]string(nil), collector.evidenceValues...)
}

func (collector *opsACPCollector) failure() error {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.failureErr
}
