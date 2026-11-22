package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// OperationsDelegationToolName is the stable model-facing operations seam.
const OperationsDelegationToolName = "delegate_operations"

const (
	OpsTargetKubernetes         OpsTargetKind = "kubernetes"
	OpsTargetSSHVM              OpsTargetKind = "ssh_vm"
	OpsTargetLocalContainer     OpsTargetKind = "local_container"
	OpsTargetKindKubernetes                   = OpsTargetKubernetes
	OpsTargetKindSSHVM                        = OpsTargetSSHVM
	OpsTargetKindLocalContainer               = OpsTargetLocalContainer

	OpsIntentReadOnly OpsIntent = "read_only"
	OpsIntentMutating OpsIntent = "mutating"
	OpsReadOnly                 = OpsIntentReadOnly
	OpsMutating                 = OpsIntentMutating
)

const (
	OpsCompletionSucceeded   OpsCompletionStatus = "succeeded"
	OpsCompletionFailed      OpsCompletionStatus = "failed"
	OpsCompletionRejected    OpsCompletionStatus = "rejected"
	OpsCompletionUnavailable OpsCompletionStatus = "unavailable"
)

var (
	ErrOperationsDelegationNotConfigured = errors.New("operations delegation is not configured")
	ErrOperationsMutationDisabled        = errors.New("mutating operations delegation is disabled")
	ErrOperationsPlanRequired            = errors.New("a valid operations plan is required")
)

const (
	defaultOpsMaxTasks          = 8
	defaultOpsMaxTimeoutSeconds = 5 * 60
	defaultOpsMaxOutputBytes    = 1 << 20
	defaultOpsMaxCommands       = 16
	defaultOpsMaxSteps          = 64
	defaultOpsMaxResultBytes    = 32 << 10
	maxOpsCommandArgBytes       = 4096
)

// OpsTargetKind selects one exact, provider-neutral target shape.
type OpsTargetKind string

// OpsIntent is deliberately explicit: a backend must not infer mutability from
// a command name or a model role.
type OpsIntent string

// OpsCompletionStatus is the bounded terminal status returned by an operations
// backend. It is descriptive and never an executable follow-up.
type OpsCompletionStatus string

// KubernetesTarget identifies one Kubernetes object or workload context. No
// kubeconfig, token, shell, or inherited provider capability crosses this seam.
type KubernetesTarget struct {
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace"`
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
}

// SSHVMTarget identifies one exact SSH/VM endpoint. VMID is an optional
// parent-supplied inventory identifier and is not interpreted by Bee.
type SSHVMTarget struct {
	Host string `json:"host"`
	User string `json:"user"`
	Port int    `json:"port,omitempty"`
	VMID string `json:"vm_id,omitempty"`
}

// LocalContainerTarget identifies one local container through an explicitly
// named runtime. It does not grant host filesystem or container-management
// authority by itself.
type LocalContainerTarget struct {
	Runtime   string `json:"runtime"`
	Container string `json:"container"`
}

// OpsTarget is a tagged union. Exactly one payload must match Kind. The
// wire format accepts both the provider-neutral nested payloads and the flat
// model-facing spelling used by delegate_operations' schema.
type OpsTarget struct {
	Kind           OpsTargetKind         `json:"kind"`
	Kubernetes     *KubernetesTarget     `json:"kubernetes,omitempty"`
	SSHVM          *SSHVMTarget          `json:"ssh_vm,omitempty"`
	LocalContainer *LocalContainerTarget `json:"local_container,omitempty"`
}

func (target *OpsTarget) UnmarshalJSON(data []byte) error {
	type wireTarget struct {
		Kind           OpsTargetKind         `json:"kind"`
		Kubernetes     *KubernetesTarget     `json:"kubernetes,omitempty"`
		SSHVM          *SSHVMTarget          `json:"ssh_vm,omitempty"`
		LocalContainer *LocalContainerTarget `json:"local_container,omitempty"`
		Context        string                `json:"context,omitempty"`
		Namespace      string                `json:"namespace,omitempty"`
		Resource       string                `json:"resource,omitempty"`
		Name           string                `json:"name,omitempty"`
		Container      string                `json:"container,omitempty"`
		Host           string                `json:"host,omitempty"`
		User           string                `json:"user,omitempty"`
		Port           int                   `json:"port,omitempty"`
		VMID           string                `json:"vm_id,omitempty"`
		Runtime        string                `json:"runtime,omitempty"`
	}
	var wire wireTarget
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var flat bool
	switch wire.Kind {
	case OpsTargetKubernetes:
		flat = wire.Context != "" || wire.Namespace != "" || wire.Resource != "" || wire.Name != "" || wire.Container != ""
		if flat && wire.Kubernetes != nil {
			return errors.New("operations target must use nested or flat Kubernetes fields, not both")
		}
		if wire.Kubernetes == nil && flat {
			wire.Kubernetes = &KubernetesTarget{Context: wire.Context, Namespace: wire.Namespace, Resource: wire.Resource, Name: wire.Name, Container: wire.Container}
		}
	case OpsTargetSSHVM:
		flat = wire.Host != "" || wire.User != "" || wire.Port != 0 || wire.VMID != ""
		if flat && wire.SSHVM != nil {
			return errors.New("operations target must use nested or flat SSH/VM fields, not both")
		}
		if wire.SSHVM == nil && flat {
			wire.SSHVM = &SSHVMTarget{Host: wire.Host, User: wire.User, Port: wire.Port, VMID: wire.VMID}
		}
	case OpsTargetLocalContainer:
		flat = wire.Runtime != "" || wire.Container != ""
		if flat && wire.LocalContainer != nil {
			return errors.New("operations target must use nested or flat local/container fields, not both")
		}
		if wire.LocalContainer == nil && flat {
			wire.LocalContainer = &LocalContainerTarget{Runtime: wire.Runtime, Container: wire.Container}
		}
	}
	*target = OpsTarget{Kind: wire.Kind, Kubernetes: wire.Kubernetes, SSHVM: wire.SSHVM, LocalContainer: wire.LocalContainer}
	return nil
}

func (target OpsTarget) Validate() error {
	if target.Kind != OpsTargetKubernetes && target.Kind != OpsTargetSSHVM && target.Kind != OpsTargetLocalContainer {
		return fmt.Errorf("operations target kind %q is invalid", target.Kind)
	}
	present := 0
	if target.Kubernetes != nil {
		present++
	}
	if target.SSHVM != nil {
		present++
	}
	if target.LocalContainer != nil {
		present++
	}
	if present != 1 {
		return errors.New("operations target must contain exactly one target payload")
	}
	switch target.Kind {
	case OpsTargetKubernetes:
		if target.Kubernetes == nil || target.SSHVM != nil || target.LocalContainer != nil {
			return errors.New("kubernetes target payload does not match kind")
		}
		return target.Kubernetes.validate()
	case OpsTargetSSHVM:
		if target.SSHVM == nil || target.Kubernetes != nil || target.LocalContainer != nil {
			return errors.New("SSH/VM target payload does not match kind")
		}
		return target.SSHVM.validate()
	case OpsTargetLocalContainer:
		if target.LocalContainer == nil || target.Kubernetes != nil || target.SSHVM != nil {
			return errors.New("local/container target payload does not match kind")
		}
		return target.LocalContainer.validate()
	}
	return nil
}

var opsNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var opsContextPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

func validateOpsName(label, value string) error {
	if !opsNamePattern.MatchString(value) {
		return fmt.Errorf("operations %s %q is invalid", label, value)
	}
	return nil
}

func (target KubernetesTarget) validate() error {
	if strings.TrimSpace(target.Context) == "" {
		return errors.New("kubernetes context is required")
	}
	if err := validateOpsName("kubernetes namespace", target.Namespace); err != nil {
		return err
	}
	if err := validateOpsName("kubernetes resource", target.Resource); err != nil {
		return err
	}
	if err := validateOpsName("kubernetes name", target.Name); err != nil {
		return err
	}
	if !opsContextPattern.MatchString(target.Context) || strings.Contains(target.Context, "..") {
		return fmt.Errorf("operations kubernetes context %q is invalid", target.Context)
	}
	if target.Container != "" {
		if err := validateOpsName("kubernetes container", target.Container); err != nil {
			return err
		}
	}
	return nil
}

func (target SSHVMTarget) validate() error {
	if err := validateRemoteHost(target.Host); err != nil {
		return err
	}
	if err := validateRemoteUser(target.User); err != nil {
		return err
	}
	if target.Port == 0 {
		target.Port = 22
	}
	if target.Port < 1 || target.Port > 65535 {
		return errors.New("operations SSH/VM port must be between 1 and 65535")
	}
	if target.VMID != "" {
		if err := validateOpsName("VM id", target.VMID); err != nil {
			return err
		}
	}
	return nil
}

func (target LocalContainerTarget) validate() error {
	switch target.Runtime {
	case "docker", "podman", "nerdctl", "containerd":
	default:
		return fmt.Errorf("operations local/container runtime %q is invalid", target.Runtime)
	}
	return validateOpsName("container", target.Container)
}

// OpsBudget bounds one task. Zero values are filled from parent-owned limits.
type OpsBudget struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxCommands    int `json:"max_commands,omitempty"`
	MaxSteps       int `json:"max_steps,omitempty"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

func (budget OpsBudget) Validate() error {
	if budget.TimeoutSeconds < 0 || budget.MaxCommands < 0 || budget.MaxSteps < 0 || budget.MaxOutputBytes < 0 {
		return errors.New("operations budget values must not be negative")
	}
	return nil
}

// OpsTask is the exact, bounded work item sent to an explicit backend.
type OpsTask struct {
	TaskID             string    `json:"task_id"`
	Objective          string    `json:"objective"`
	Target             OpsTarget `json:"target"`
	Intent             OpsIntent `json:"intent"`
	OperationClass     string    `json:"operation_class,omitempty"`
	Command            []string  `json:"command"`
	Stdin              string    `json:"stdin,omitempty"`
	Budget             OpsBudget `json:"budget"`
	RollbackCriteria   []string  `json:"rollback_criteria,omitempty"`
	AcceptanceCriteria []string  `json:"acceptance_criteria"`
}

func (task OpsTask) Validate() error {
	if err := validateChildID("operations task_id", task.TaskID); err != nil {
		return err
	}
	if strings.TrimSpace(task.Objective) == "" {
		return errors.New("operations task objective is required")
	}
	if err := task.Target.Validate(); err != nil {
		return fmt.Errorf("operations target: %w", err)
	}
	if task.Intent != OpsIntentReadOnly && task.Intent != OpsIntentMutating {
		return fmt.Errorf("operations intent %q is invalid", task.Intent)
	}
	if len(task.Command) == 0 {
		return errors.New("operations task command is required")
	}
	if len(task.Command) > defaultOpsMaxCommands {
		return fmt.Errorf("operations task command must contain at most %d arguments", defaultOpsMaxCommands)
	}
	for i, arg := range task.Command {
		if arg == "" {
			return fmt.Errorf("operations command argument %d is empty", i)
		}
		if len(arg) > maxOpsCommandArgBytes {
			return fmt.Errorf("operations command argument %d is too large", i)
		}
		for _, r := range arg {
			if r == 0 || r < 0x20 || r == '\n' || r == '\r' {
				return fmt.Errorf("operations command argument %d contains unsafe control syntax", i)
			}
		}
	}
	if len(task.Stdin) > maxOpsCommandArgBytes*4 {
		return errors.New("operations stdin is too large")
	}
	if err := task.Budget.Validate(); err != nil {
		return err
	}
	if len(task.AcceptanceCriteria) == 0 {
		return errors.New("operations task acceptance criteria are required")
	}
	for i, criterion := range task.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("operations acceptance criterion %d is empty", i)
		}
	}
	for i, criterion := range task.RollbackCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("operations rollback criterion %d is empty", i)
		}
	}
	return nil
}

// OpsPlan is parent-authored intent. It contains no credentials, grants, or
// executable backend implementation.
type OpsPlan struct {
	PlanID             string    `json:"plan_id"`
	Goal               string    `json:"goal"`
	Tasks              []OpsTask `json:"tasks"`
	RollbackCriteria   []string  `json:"rollback_criteria,omitempty"`
	AcceptanceCriteria []string  `json:"acceptance_criteria"`
}

func (plan OpsPlan) Validate() error {
	if err := validateChildID("operations plan_id", plan.PlanID); err != nil {
		return err
	}
	if strings.TrimSpace(plan.Goal) == "" {
		return errors.New("operations plan goal is required")
	}
	if len(plan.Tasks) == 0 || len(plan.Tasks) > defaultOpsMaxTasks {
		return fmt.Errorf("operations plan tasks must be between 1 and %d", defaultOpsMaxTasks)
	}
	if len(plan.AcceptanceCriteria) == 0 {
		return errors.New("operations plan acceptance criteria are required")
	}
	for i, criterion := range plan.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("operations plan acceptance criterion %d is empty", i)
		}
	}
	for i, criterion := range plan.RollbackCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("operations plan rollback criterion %d is empty", i)
		}
	}
	seen := make(map[string]struct{}, len(plan.Tasks))
	for i, task := range plan.Tasks {
		if err := task.Validate(); err != nil {
			return fmt.Errorf("operations task %d: %w", i, err)
		}
		if _, ok := seen[task.TaskID]; ok {
			return fmt.Errorf("operations task id %q repeats", task.TaskID)
		}
		seen[task.TaskID] = struct{}{}
		if task.Intent == OpsIntentMutating && len(plan.RollbackCriteria) == 0 && len(task.RollbackCriteria) == 0 {
			return fmt.Errorf("mutating operations task %q requires rollback criteria", task.TaskID)
		}
	}
	return nil
}

// OpsCompletion is compact evidence returned to the parent.
type OpsCompletion struct {
	PlanID          string              `json:"plan_id"`
	TaskID          string              `json:"task_id"`
	ParentID        string              `json:"parent_id"`
	Status          OpsCompletionStatus `json:"status"`
	Summary         string              `json:"summary"`
	Output          string              `json:"output,omitempty"`
	Evidence        []string            `json:"evidence,omitempty"`
	RollbackApplied bool                `json:"rollback_applied,omitempty"`
	OutputTruncated bool                `json:"output_truncated,omitempty"`
	Blockers        []string            `json:"blockers,omitempty"`
}

func (completion OpsCompletion) Validate() error {
	if err := validateChildID("operations completion task_id", completion.TaskID); err != nil {
		return err
	}
	if err := validateChildID("operations completion parent_id", completion.ParentID); err != nil {
		return err
	}
	if completion.Status != OpsCompletionSucceeded && completion.Status != OpsCompletionFailed && completion.Status != OpsCompletionRejected && completion.Status != OpsCompletionUnavailable {
		return fmt.Errorf("operations completion status %q is invalid", completion.Status)
	}
	if strings.TrimSpace(completion.Summary) == "" {
		return errors.New("operations completion summary is required")
	}
	return nil
}

// OpsDelegationRequest is the only model-facing request shape.
type OpsDelegationRequest struct {
	Backend string  `json:"backend,omitempty"`
	Role    string  `json:"role,omitempty"`
	Plan    OpsPlan `json:"plan"`
}

func (request OpsDelegationRequest) Validate() error {
	if request.Role != "" && request.Role != "scout" && request.Role != "operator" {
		return fmt.Errorf("operations role %q is invalid", request.Role)
	}
	if request.Backend != "" && !opsNamePattern.MatchString(request.Backend) {
		return fmt.Errorf("operations backend %q is invalid", request.Backend)
	}
	return request.Plan.Validate()
}

// OpsDelegationResult is bounded before it is returned through the tool seam.
type OpsDelegationResult struct {
	PlanID      string          `json:"plan_id"`
	ParentID    string          `json:"parent_id"`
	Backend     string          `json:"backend,omitempty"`
	Completions []OpsCompletion `json:"completions,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// OpsDelegationLimits are parent-owned ceilings; model requests cannot raise
// them. A nil/zero limit set uses conservative defaults.
type OpsDelegationLimits struct {
	MaxTasks          int `json:"max_tasks,omitempty"`
	MaxTimeoutSeconds int `json:"max_timeout_seconds,omitempty"`
	MaxCommands       int `json:"max_commands,omitempty"`
	MaxSteps          int `json:"max_steps,omitempty"`
	MaxOutputBytes    int `json:"max_output_bytes,omitempty"`
	MaxResultBytes    int `json:"max_result_bytes,omitempty"`
}

func (limits OpsDelegationLimits) withDefaults() OpsDelegationLimits {
	if limits.MaxTasks <= 0 {
		limits.MaxTasks = defaultOpsMaxTasks
	}
	if limits.MaxTimeoutSeconds <= 0 {
		limits.MaxTimeoutSeconds = defaultOpsMaxTimeoutSeconds
	}
	if limits.MaxCommands <= 0 {
		limits.MaxCommands = defaultOpsMaxCommands
	}
	if limits.MaxSteps <= 0 {
		limits.MaxSteps = defaultOpsMaxSteps
	}
	if limits.MaxOutputBytes <= 0 {
		limits.MaxOutputBytes = defaultOpsMaxOutputBytes
	}
	if limits.MaxResultBytes <= 0 {
		limits.MaxResultBytes = defaultOpsMaxResultBytes
	}
	return limits
}

func (limits OpsDelegationLimits) Validate() error {
	if limits.MaxTasks < 0 || limits.MaxTimeoutSeconds < 0 || limits.MaxCommands < 0 || limits.MaxSteps < 0 || limits.MaxOutputBytes < 0 || limits.MaxResultBytes < 0 {
		return errors.New("operations delegation limits must not be negative")
	}
	return nil
}

// OpsBackend is intentionally narrow. Implementations are supplied by the
// parent composition root; Bee ships no SSH/Kubernetes/container mutator.
type OpsBackend interface {
	Name() string
	Execute(context.Context, OpsTask) (OpsCompletion, error)
}

// OpsMutationAuthorizer is implemented only by parent-owned mutation
// backends. A legacy/read-only backend cannot opt into mutation merely by
// setting OperationsDelegationService.AllowMutations.
type OpsMutationAuthorizer interface {
	AuthorizeMutation(OpsTask) error
}

var opsDelegationSequence atomic.Uint64

// OperationsDelegationService is the parent-owned validation and execution
// seam. Backend nil means disabled. Mutations remain denied unless the parent
// explicitly sets AllowMutations after supplying an operations backend.
type OperationsDelegationService struct {
	Backend        OpsBackend
	ParentID       string
	AllowMutations bool
	Limits         OpsDelegationLimits
	// CapabilityProjection is an optional parent-owned projection applied
	// before a backend sees any task. It is intentionally narrower than Bee's
	// normal command/SSH capability model and is never inferred from it.
	CapabilityProjection *OpsCapabilityProjection
}

func NewOperationsDelegationService(backend OpsBackend) *OperationsDelegationService {
	return &OperationsDelegationService{Backend: backend, ParentID: "bee-parent", Limits: OpsDelegationLimits{}.withDefaults()}
}

func (service *OperationsDelegationService) Delegate(ctx context.Context, request OpsDelegationRequest) (OpsDelegationResult, error) {
	result := OpsDelegationResult{PlanID: request.Plan.PlanID}
	if service == nil || service.Backend == nil {
		return result, ErrOperationsDelegationNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	limits := service.Limits.withDefaults()
	if err := service.Limits.Validate(); err != nil {
		return result, err
	}
	if len(request.Plan.Tasks) > limits.MaxTasks {
		return result, errors.New("operations plan exceeds parent task limit")
	}
	if service.CapabilityProjection != nil {
		if err := service.CapabilityProjection.Validate(); err != nil {
			return result, err
		}
		for _, task := range request.Plan.Tasks {
			if err := service.CapabilityProjection.Authorize(task); err != nil {
				return result, err
			}
		}
	}
	backendName := strings.TrimSpace(service.Backend.Name())
	if backendName == "" {
		return result, errors.New("operations backend name is required")
	}
	if request.Backend != "" && request.Backend != backendName {
		return result, fmt.Errorf("operations backend %q is not available", request.Backend)
	}
	result.Backend = backendName
	parentID := service.ParentID
	if parentID == "" {
		parentID = "bee-parent"
	}
	result.ParentID = parentID
	result.Completions = make([]OpsCompletion, 0, len(request.Plan.Tasks))
	for _, original := range request.Plan.Tasks {
		if original.Intent == OpsIntentMutating && !service.AllowMutations {
			return result, ErrOperationsMutationDisabled
		}
	}
	for _, original := range request.Plan.Tasks {
		task := original
		if task.Intent == OpsIntentMutating {
			if task.Budget.TimeoutSeconds > limits.MaxTimeoutSeconds {
				return result, errors.New("operations task timeout exceeds parent limit")
			}
			if task.Budget.MaxCommands > limits.MaxCommands || (task.Budget.MaxCommands > 0 && len(task.Command) > task.Budget.MaxCommands) || (task.Budget.MaxCommands <= 0 && len(task.Command) > limits.MaxCommands) {
				return result, errors.New("operations task command budget exceeded")
			}
			if task.Budget.MaxSteps > limits.MaxSteps {
				return result, errors.New("operations task step budget exceeds parent limit")
			}
			if task.Budget.MaxOutputBytes > limits.MaxOutputBytes {
				return result, errors.New("operations task output budget exceeds parent limit")
			}
		} else {
			if task.Budget.TimeoutSeconds <= 0 {
				task.Budget.TimeoutSeconds = limits.MaxTimeoutSeconds
			}
			if task.Budget.TimeoutSeconds > limits.MaxTimeoutSeconds {
				return result, errors.New("operations task timeout exceeds parent limit")
			}
			if task.Budget.MaxCommands <= 0 {
				task.Budget.MaxCommands = limits.MaxCommands
			}
			if task.Budget.MaxCommands > limits.MaxCommands || len(task.Command) > task.Budget.MaxCommands {
				return result, errors.New("operations task command budget exceeded")
			}
			if task.Budget.MaxSteps <= 0 {
				task.Budget.MaxSteps = limits.MaxSteps
			}
			if task.Budget.MaxSteps > limits.MaxSteps {
				return result, errors.New("operations task step budget exceeds parent limit")
			}
			if task.Budget.MaxOutputBytes <= 0 {
				task.Budget.MaxOutputBytes = limits.MaxOutputBytes
			}
			if task.Budget.MaxOutputBytes > limits.MaxOutputBytes {
				return result, errors.New("operations task output budget exceeds parent limit")
			}
		}
		if task.Intent == OpsIntentMutating {
			authorizer, ok := service.Backend.(OpsMutationAuthorizer)
			if !ok {
				return result, ErrOpsMutationGrantRequired
			}
			if err := authorizer.AuthorizeMutation(task); err != nil {
				return result, err
			}
		}
		executionTimeout := task.Budget.TimeoutSeconds
		if executionTimeout <= 0 {
			executionTimeout = limits.MaxTimeoutSeconds
		}
		outputBudget := task.Budget.MaxOutputBytes
		if outputBudget <= 0 {
			outputBudget = limits.MaxOutputBytes
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(executionTimeout)*time.Second)
		completion, err := service.Backend.Execute(callCtx, task)
		cancel()
		if completion.PlanID == "" {
			completion.PlanID = request.Plan.PlanID
		}
		if completion.TaskID == "" {
			completion.TaskID = task.TaskID
		}
		if completion.ParentID == "" {
			completion.ParentID = parentID
		}
		if completion.Status == "" {
			completion.Status = OpsCompletionSucceeded
		}
		if completion.Summary == "" {
			completion.Summary = "operations backend completed without a summary"
		}
		if completion.PlanID != request.Plan.PlanID {
			return result, errors.New("operations backend returned a mismatched plan id")
		}
		if completion.TaskID != task.TaskID {
			return result, errors.New("operations backend returned a mismatched task id")
		}
		if completion.ParentID != parentID {
			return result, errors.New("operations backend returned a mismatched parent id")
		}
		if validateErr := completion.Validate(); validateErr != nil {
			return result, fmt.Errorf("validate operations completion: %w", validateErr)
		}
		completion = boundOpsCompletion(completion, outputBudget)
		result.Completions = append(result.Completions, completion)
		if err != nil {
			result.Error = truncateText(err.Error(), 2048)
			return boundOpsResult(result, limits.MaxResultBytes), err
		}
	}
	return boundOpsResult(result, limits.MaxResultBytes), nil
}

func boundOpsCompletion(completion OpsCompletion, maxOutput int) OpsCompletion {
	if maxOutput <= 0 {
		maxOutput = defaultOpsMaxOutputBytes
	}
	completion.Summary = truncateText(completion.Summary, 2048)
	previouslyTruncated := completion.OutputTruncated
	completion.Output, completion.OutputTruncated = truncateOutput(completion.Output, maxOutput)
	completion.OutputTruncated = completion.OutputTruncated || previouslyTruncated
	completion.Evidence = boundedStrings(completion.Evidence, 16, 1024)
	completion.Blockers = boundedStrings(completion.Blockers, 16, 512)
	return completion
}

func boundOpsResult(result OpsDelegationResult, maxBytes int) OpsDelegationResult {
	if maxBytes <= 0 {
		maxBytes = defaultOpsMaxResultBytes
	}
	result.Error = truncateText(result.Error, 2048)
	for i := range result.Completions {
		result.Completions[i] = boundOpsCompletion(result.Completions[i], 8192)
	}
	data, err := json.Marshal(result)
	if err == nil && len(data) <= maxBytes {
		return result
	}
	result.Completions = nil
	return result
}

func nextOpsTaskID() string {
	return fmt.Sprintf("ops-%d-%d", time.Now().UTC().UnixNano(), opsDelegationSequence.Add(1))
}

func NewOperationsDelegationTool(service *OperationsDelegationService) Tool {
	return ToolFunc{ToolDefinition: operationsDelegationToolDefinition(), Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		var request OpsDelegationRequest
		if err := parseArgs(args, &request); err != nil {
			return "", fmt.Errorf("delegate_operations arguments: %w", err)
		}
		if service == nil {
			return "", ErrOperationsDelegationNotConfigured
		}
		result, err := service.Delegate(ctx, request)
		data, marshalErr := json.Marshal(boundOpsResult(result, service.Limits.withDefaults().MaxResultBytes))
		if marshalErr != nil {
			return "", marshalErr
		}
		return string(data), err
	}}
}

func operationsDelegationToolDefinition() ToolDefinition {
	return ToolDefinition{
		Name:        OperationsDelegationToolName,
		Description: "Delegate one explicitly bounded Kubernetes, SSH/VM, or local/container operations plan. Read-only intent is the default; mutating intent is denied unless a parent supplies an operations backend and explicitly enables mutations. No credentials or inherited grants are accepted.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"backend":{"type":"string"},"role":{"type":"string","enum":["scout","operator"]},"plan":{"type":"object","properties":{"plan_id":{"type":"string"},"goal":{"type":"string"},"tasks":{"type":"array","items":{"type":"object","properties":{"task_id":{"type":"string"},"objective":{"type":"string"},"target":{"type":"object","properties":{"kind":{"type":"string","enum":["kubernetes","ssh_vm","local_container"]},"context":{"type":"string"},"namespace":{"type":"string"},"resource":{"type":"string"},"name":{"type":"string"},"container":{"type":"string"},"host":{"type":"string"},"user":{"type":"string"},"port":{"type":"integer"},"vm_id":{"type":"string"},"runtime":{"type":"string"}},"required":["kind"]},"intent":{"type":"string","enum":["read_only","mutating"]},"operation_class":{"type":"string"},"command":{"type":"array","items":{"type":"string"}},"stdin":{"type":"string"},"budget":{"type":"object","properties":{"timeout_seconds":{"type":"integer","minimum":0},"max_commands":{"type":"integer","minimum":0},"max_steps":{"type":"integer","minimum":0},"max_output_bytes":{"type":"integer","minimum":0}}},"rollback_criteria":{"type":"array","items":{"type":"string"}},"acceptance_criteria":{"type":"array","items":{"type":"string"}}},"required":["task_id","objective","target","intent","command","budget","acceptance_criteria"]}},"rollback_criteria":{"type":"array","items":{"type":"string"}},"acceptance_criteria":{"type":"array","items":{"type":"string"}}},"required":["plan_id","goal","tasks","acceptance_criteria"]}},"required":["plan"]}`),
	}
}
