package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpsCommandRequest is the parent-owned command authority request produced
// from one validated OpsTask. It contains only argv and the exact target
// binding; it never contains credentials, environment variables, or memory.
type OpsCommandRequest struct {
	Target         CommandTarget
	Host           string
	User           string
	Port           int
	Argv           []string
	Stdin          string
	KubeconfigPath string // parent-only; never model supplied or emitted as evidence
	Mutation       bool   // parent-owned audit metadata; never model authority
	OperationClass string // parent-owned audit metadata
	ApprovalID     string // bounded approval reference, not a grant
}

// OpsCommandRunner is injectable for tests and alternate parent-owned command
// authorities. The production runner delegates to Bee's existing local/SSH
// run_command implementation.
type OpsCommandRunner func(context.Context, OpsCommandRequest, ExecutionScope) (string, error)

func defaultOpsCommandRunner(ctx context.Context, request OpsCommandRequest, scope ExecutionScope) (string, error) {
	argv := append([]string(nil), request.Argv...)
	if request.Mutation && request.Target == CommandTargetSSH {
		return executeParentOwnedMutationSSH(ctx, request, scope)
	}
	if request.Target == CommandTargetLocal && request.KubeconfigPath != "" && len(argv) > 0 && argv[0] == "kubectl" {
		argv = append([]string{"kubectl", "--kubeconfig", request.KubeconfigPath}, argv[1:]...)
	}
	data, err := json.Marshal(runArgs{Target: request.Target, Host: request.Host, User: request.User, Port: request.Port, Argv: argv, Stdin: request.Stdin})
	if err != nil {
		return "", err
	}
	return (RunCommandTool{Scope: scope}).Execute(ctx, data)
}

// OpsCommandBackend is a parent-owned, read-only adapter over Bee's existing
// argv command authority. It is deliberately not an ACP child: an ACP/local-child
// scout can receive only the bounded OpsCompletion evidence after the parent
// has executed the command.
type OpsCommandBackend struct {
	BackendName string
	ParentID    string
	Scope       ExecutionScope
	Projection  OpsCapabilityProjection
	Runner      OpsCommandRunner
	// MutationGrant is parent-owned and is never serialized into an OpsTask or
	// ACP packet. A nil grant keeps this backend read-only.
	MutationGrant *OpsMutationGrant
}

// ReadOnlyOpsCommandBackend is a descriptive compatibility name.
type ReadOnlyOpsCommandBackend = OpsCommandBackend

var _ OpsBackend = (*OpsCommandBackend)(nil)

// NewOpsReadOnlyCommandBackend binds one exact target and constructs the
// parent-owned local/SSH command backend. The projection is forced into
// read-only scout mode and cannot carry mutation authority.
func NewOpsReadOnlyCommandBackend(scope ExecutionScope, projection OpsCapabilityProjection) (*OpsCommandBackend, error) {
	projection.ReadOnlyScout = true
	if err := projection.Validate(); err != nil {
		return nil, err
	}
	if err := scope.ValidateShape(); err != nil {
		return nil, err
	}
	return &OpsCommandBackend{
		BackendName: "bee-run-command",
		ParentID:    "bee-parent",
		Scope:       scope,
		Projection:  projection,
		Runner:      defaultOpsCommandRunner,
	}, nil
}

// NewOpsCommandBackend is an explicit alias for callers that do not want the
// read-only constructor's longer name.
func NewOpsCommandBackend(scope ExecutionScope, projection OpsCapabilityProjection) (*OpsCommandBackend, error) {
	return NewOpsReadOnlyCommandBackend(scope, projection)
}

// NewOpsSSHMutationCommandBackend explicitly composes the narrow parent-owned
// SSH/VM mutation path. Normal/read-only constructors cannot be widened into
// this backend without a valid grant and approval.
func NewOpsSSHMutationCommandBackend(scope ExecutionScope, projection OpsCapabilityProjection, grant *OpsMutationGrant) (*OpsCommandBackend, error) {
	if projection.ReadOnlyScout {
		return nil, fmt.Errorf("%w: read-only scout cannot be a mutation backend", ErrOperationsMutationDisabled)
	}
	if projection.Target.Kind != OpsTargetSSHVM {
		return nil, errors.New("SSH mutation backend requires an SSH/VM projection target")
	}
	if err := projection.Validate(); err != nil {
		return nil, err
	}
	if err := scope.ValidateShape(); err != nil {
		return nil, err
	}
	if err := grant.Validate(); err != nil {
		return nil, err
	}
	if !sameOpsTarget(projection.Target, grant.Target) {
		return nil, ErrOpsTargetMismatch
	}
	effectiveProjection := projection.effective()
	if grant.MaxTimeoutSeconds > effectiveProjection.MaxTimeoutSeconds || grant.MaxCommands > effectiveProjection.MaxCommands || grant.MaxSteps > effectiveProjection.MaxSteps || grant.MaxOutputBytes > effectiveProjection.MaxOutputBytes {
		return nil, errors.New("operations mutation grant exceeds projected capability budget")
	}
	return &OpsCommandBackend{BackendName: "bee-run-command-mutation", ParentID: "bee-parent", Scope: scope, Projection: projection, Runner: defaultOpsCommandRunner, MutationGrant: grant}, nil
}

// NewOpsMutationCommandBackend is a compatibility alias for the explicit
// SSH/VM mutation constructor.
func NewOpsMutationCommandBackend(scope ExecutionScope, projection OpsCapabilityProjection, grant *OpsMutationGrant) (*OpsCommandBackend, error) {
	return NewOpsSSHMutationCommandBackend(scope, projection, grant)
}

// NewOpsLocalChildReadOnlyScoutBackend provides the local-child scout
// compatible route. The child is never given command authority; it is represented
// by the backend name and receives only the resulting bounded completion.
func NewOpsLocalChildReadOnlyScoutBackend(scope ExecutionScope, projection OpsCapabilityProjection) (*OpsCommandBackend, error) {
	backend, err := NewOpsReadOnlyCommandBackend(scope, projection)
	if err != nil {
		return nil, err
	}
	backend.BackendName = CodingBackendNameLocalChild
	return backend, nil
}

// NewLocalChildOpsBackend is a compatibility alias for the explicit scout
// constructor.
func NewLocalChildOpsBackend(scope ExecutionScope, projection OpsCapabilityProjection) (*OpsCommandBackend, error) {
	return NewOpsLocalChildReadOnlyScoutBackend(scope, projection)
}

func (backend *OpsCommandBackend) Name() string {
	if backend == nil || strings.TrimSpace(backend.BackendName) == "" {
		return "bee-run-command"
	}
	return backend.BackendName
}

// AuthorizeMutation is a non-consuming preflight used by the parent service.
// Execute performs the final one-shot reservation immediately before invoking
// the parent-owned runner.
func (backend *OpsCommandBackend) AuthorizeMutation(task OpsTask) error {
	if backend == nil || backend.MutationGrant == nil {
		return ErrOpsMutationGrantRequired
	}
	if backend.Projection.ReadOnlyScout {
		return ErrOperationsMutationDisabled
	}
	_, err := PreviewOpsMutation(task, backend.MutationGrant)
	return err
}

// Execute runs exactly one read-only task. Plan identity is intentionally left
// blank for OperationsDelegationService to bind to the parent request.
func (backend *OpsCommandBackend) Execute(ctx context.Context, task OpsTask) (OpsCompletion, error) {
	completion := OpsCompletion{TaskID: task.TaskID, ParentID: backendParentID(backend), Status: OpsCompletionUnavailable}
	if backend == nil {
		return completion, errors.New("operations command backend is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := task.Validate(); err != nil {
		completion.Status = OpsCompletionRejected
		completion.Summary = truncateText(err.Error(), 2048)
		completion.Blockers = []string{truncateText(err.Error(), 1024)}
		return completion, err
	}
	if task.Intent == OpsIntentReadOnly && task.Stdin != "" {
		err := errors.New("read-only operations command does not accept stdin")
		completion.Status = OpsCompletionRejected
		completion.Summary = err.Error()
		completion.Blockers = []string{err.Error()}
		return completion, err
	}
	if task.Intent == OpsIntentMutating && (backend.MutationGrant == nil || backend.Projection.ReadOnlyScout) {
		err := ErrOpsMutationGrantRequired
		if backend.Projection.ReadOnlyScout {
			err = ErrOperationsMutationDisabled
		}
		completion.Status = OpsCompletionRejected
		completion.Summary = err.Error()
		completion.Blockers = []string{err.Error()}
		return completion, err
	}
	projection := backend.Projection
	if task.Intent == OpsIntentReadOnly {
		projection.ReadOnlyScout = true
	}
	if err := projection.Authorize(task); err != nil {
		completion.Status = OpsCompletionRejected
		completion.Summary = truncateText(err.Error(), 2048)
		completion.Blockers = []string{truncateText(err.Error(), 1024)}
		return completion, err
	}
	var request OpsCommandRequest
	var err error
	var mutationPreview OpsMutationPreview
	if task.Intent == OpsIntentMutating {
		mutationPreview, err = PreviewOpsMutation(task, backend.MutationGrant)
		if err != nil {
			completion.Status = OpsCompletionRejected
			completion.Summary = truncateText(err.Error(), 2048)
			completion.Blockers = []string{truncateText(err.Error(), 1024)}
			return completion, err
		}
		request, err = opsMutatingCommandRequest(task, mutationPreview)
	} else {
		request, err = opsCommandRequest(task)
	}
	if err != nil {
		completion.Status = OpsCompletionRejected
		completion.Summary = truncateText(err.Error(), 2048)
		completion.Blockers = []string{truncateText(err.Error(), 1024)}
		return completion, err
	}
	if task.Target.Kind == OpsTargetKubernetes {
		request.KubeconfigPath = opsKubeconfigPath()
	}
	runner := backend.Runner
	if runner == nil {
		runner = defaultOpsCommandRunner
	}
	scope := backend.Scope
	budget := task.Budget
	if budget.TimeoutSeconds <= 0 {
		budget.TimeoutSeconds = projection.effective().MaxTimeoutSeconds
	}
	if budget.MaxOutputBytes <= 0 {
		budget.MaxOutputBytes = projection.effective().MaxOutputBytes
	}
	if budget.TimeoutSeconds <= 0 {
		budget.TimeoutSeconds = defaultOpsMaxTimeoutSeconds
	}
	if budget.MaxOutputBytes <= 0 {
		budget.MaxOutputBytes = defaultOpsMaxOutputBytes
	}
	if task.Intent == OpsIntentMutating {
		if budget.TimeoutSeconds > backend.MutationGrant.MaxTimeoutSeconds || budget.MaxCommands > backend.MutationGrant.MaxCommands || budget.MaxSteps > backend.MutationGrant.MaxSteps || budget.MaxOutputBytes > backend.MutationGrant.MaxOutputBytes {
			err := errors.New("operations mutation task exceeds grant budget")
			completion.Status = OpsCompletionRejected
			completion.Summary = err.Error()
			completion.Blockers = []string{err.Error()}
			return completion, err
		}
		if _, err := backend.MutationGrant.consume(task); err != nil {
			completion.Status = OpsCompletionRejected
			completion.Summary = truncateText(err.Error(), 2048)
			completion.Blockers = []string{truncateText(err.Error(), 1024)}
			return completion, err
		}
	}
	if scope.TimeoutSeconds <= 0 || scope.TimeoutSeconds > budget.TimeoutSeconds {
		scope.TimeoutSeconds = budget.TimeoutSeconds
	}
	if scope.MaxOutputBytes <= 0 || scope.MaxOutputBytes > budget.MaxOutputBytes {
		scope.MaxOutputBytes = budget.MaxOutputBytes
	}
	// Never inherit foreground remote grants or environment allowlists into the
	// operations authority. The parent binds the exact target in the mutation
	// grant and argv instead. Mutation SSH uses its dedicated parent-owned
	// runner path rather than the general trusted-foreground SSH gate.
	scope.RemoteGrant = nil
	scope.EnvAllowlist = nil
	if task.Intent == OpsIntentMutating {
		scope.TrustedForeground = false
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(budget.TimeoutSeconds)*time.Second)
	defer cancel()
	output, runErr := runner(callCtx, request, scope)
	completion.Output, completion.OutputTruncated = truncateOutput(output, budget.MaxOutputBytes)
	completion.Evidence = []string{fmt.Sprintf("target=%s", opsTargetEvidence(task.Target)), fmt.Sprintf("argv=%s", strings.Join(request.Argv, " "))}
	if task.Intent == OpsIntentMutating {
		completion.Evidence = append(completion.Evidence, "mutation=true", "operation_class="+mutationPreview.OperationClass, "approval_id="+mutationPreview.ApprovalID)
	}
	if runErr != nil {
		completion.Status = opsStatusForError(callCtx, runErr)
		completion.Summary = "operations command failed: " + truncateText(runErr.Error(), 1024)
		completion.Blockers = []string{truncateText(runErr.Error(), 1024)}
		return completion, nil
	}
	completion.Status = OpsCompletionSucceeded
	completion.Summary = "operations command completed"
	return boundOpsCompletion(completion, budget.MaxOutputBytes), nil
}

func opsKubeconfigPath() string {
	if configured := strings.TrimSpace(os.Getenv("KUBECONFIG")); configured != "" {
		parts := strings.Split(configured, string(os.PathListSeparator))
		if len(parts) == 1 && filepath.IsAbs(parts[0]) {
			return parts[0]
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func backendParentID(backend *OpsCommandBackend) string {
	if backend != nil && strings.TrimSpace(backend.ParentID) != "" {
		return backend.ParentID
	}
	return "bee-parent"
}

func opsTargetEvidence(target OpsTarget) string {
	if target.Kubernetes != nil {
		value := target.Kubernetes
		return fmt.Sprintf("kubernetes/%s/%s/%s/%s", value.Context, value.Namespace, value.Resource, value.Name)
	}
	if target.SSHVM != nil {
		port := target.SSHVM.Port
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("ssh_vm/%s@%s:%d", target.SSHVM.User, target.SSHVM.Host, port)
	}
	if target.LocalContainer != nil {
		return fmt.Sprintf("local_container/%s/%s", target.LocalContainer.Runtime, target.LocalContainer.Container)
	}
	return "unknown"
}

func opsCommandRequest(task OpsTask) (OpsCommandRequest, error) {
	command := normalizeOpsCommand(task.Command)
	if err := validateReadOnlyOpsCommand(command, task.Target.Kind); err != nil {
		return OpsCommandRequest{}, err
	}
	switch task.Target.Kind {
	case OpsTargetKubernetes:
		argv, err := bindKubernetesOpsCommand(*task.Target.Kubernetes, command)
		if err != nil {
			return OpsCommandRequest{}, err
		}
		return OpsCommandRequest{Target: CommandTargetLocal, Argv: argv}, nil
	case OpsTargetSSHVM:
		target := task.Target.SSHVM
		port := target.Port
		if port == 0 {
			port = 22
		}
		return OpsCommandRequest{Target: CommandTargetSSH, Host: target.Host, User: target.User, Port: port, Argv: append([]string(nil), command...), Stdin: ""}, nil
	case OpsTargetLocalContainer:
		target := task.Target.LocalContainer
		argv := append([]string{target.Runtime, "exec", target.Container}, command...)
		return OpsCommandRequest{Target: CommandTargetLocal, Argv: argv}, nil
	default:
		return OpsCommandRequest{}, fmt.Errorf("operations target kind %q is invalid", task.Target.Kind)
	}
}

func opsMutatingCommandRequest(task OpsTask, preview OpsMutationPreview) (OpsCommandRequest, error) {
	if task.Target.Kind != OpsTargetSSHVM || task.Target.SSHVM == nil {
		return OpsCommandRequest{}, errors.New("mutating operations currently support SSH/VM targets only")
	}
	target := task.Target.SSHVM
	port := target.Port
	if port == 0 {
		port = 22
	}
	return OpsCommandRequest{Target: CommandTargetSSH, Host: target.Host, User: target.User, Port: port, Argv: append([]string(nil), preview.Argv...), Stdin: task.Stdin, Mutation: true, OperationClass: preview.OperationClass, ApprovalID: preview.ApprovalID}, nil
}

func executeParentOwnedMutationSSH(ctx context.Context, request OpsCommandRequest, scope ExecutionScope) (string, error) {
	if err := validateRemoteHost(request.Host); err != nil {
		return "", err
	}
	if err := validateRemoteUser(request.User); err != nil {
		return "", err
	}
	port := request.Port
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", errors.New("operations SSH/VM port must be between 1 and 65535")
	}
	for _, arg := range request.Argv {
		if err := validateRemoteArg(arg); err != nil {
			return "", err
		}
	}
	sshArgv := []string{"ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-p", fmt.Sprint(port), request.User + "@" + request.Host}
	sshArgv = append(sshArgv, request.Argv...)
	return executeLocalCommand(ctx, sshArgv, request.Stdin, "", scope, nil)
}

// normalizeOpsCommand tolerates the compact one-string form models commonly
// emit for an argv-shaped field while keeping the authority boundary argv
// based. Quoting is intentionally unsupported; commands that need it should
// be rejected rather than interpreted as a shell.
func normalizeOpsCommand(command []string) []string {
	if len(command) == 1 && strings.ContainsAny(command[0], " \t\n\r") {
		return strings.Fields(command[0])
	}
	return append([]string(nil), command...)
}

func validateReadOnlyOpsCommand(command []string, target OpsTargetKind) error {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return errors.New("operations command is required")
	}
	base := command[0]
	if target == OpsTargetKubernetes {
		if base != "get" && base != "describe" && base != "logs" && base != "top" && base != "version" && base != "api-resources" && base != "explain" {
			return fmt.Errorf("read-only Kubernetes command %q is not permitted", base)
		}
	}
	for _, arg := range command {
		lower := strings.ToLower(arg)
		if lower == "apply" || lower == "create" || lower == "delete" || lower == "edit" || lower == "exec" || lower == "patch" || lower == "replace" || lower == "scale" || lower == "rollout" || lower == "run" || lower == "rm" || lower == "mv" || lower == "cp" || lower == "touch" || lower == "mkdir" || lower == "tee" || lower == "sudo" || lower == "sh" || lower == "bash" || lower == "zsh" {
			return fmt.Errorf("read-only operations command contains mutating or shell argument %q", arg)
		}
	}
	return nil
}

func bindKubernetesOpsCommand(target KubernetesTarget, command []string) ([]string, error) {
	for _, arg := range command {
		if arg == "--context" || strings.HasPrefix(arg, "--context=") || arg == "--kubeconfig" || strings.HasPrefix(arg, "--kubeconfig=") || arg == "-n" || arg == "--namespace" || strings.HasPrefix(arg, "--namespace=") {
			return nil, errors.New("Kubernetes command cannot override its exact target context or namespace")
		}
	}
	if len(command) < 2 || (command[0] != "version" && command[0] != "api-resources" && command[0] != "explain") {
		if err := bindKubernetesSelector(target, command); err != nil {
			return nil, err
		}
	}
	argv := []string{"kubectl", "--context", target.Context, "--namespace", target.Namespace}
	argv = append(argv, command...)
	return argv, nil
}

func bindKubernetesSelector(target KubernetesTarget, command []string) error {
	aliases := kubernetesResourceAliases(target.Resource)
	for _, arg := range command[1:] {
		for _, resource := range aliases {
			if arg == resource+"/"+target.Name {
				return nil
			}
		}
	}
	for i := 1; i+1 < len(command); i++ {
		if opsContainsString(aliases, command[i]) {
			if command[i+1] == target.Name {
				return nil
			}
			return fmt.Errorf("Kubernetes command target %q does not match exact target %q", command[i+1], target.Name)
		}
	}
	return fmt.Errorf("Kubernetes command must select exact target %q/%q", target.Resource, target.Name)
}

func kubernetesResourceAliases(resource string) []string {
	aliases := []string{resource}
	if strings.HasSuffix(resource, "s") {
		aliases = append(aliases, strings.TrimSuffix(resource, "s"))
	} else {
		aliases = append(aliases, resource+"s")
	}
	if strings.HasSuffix(resource, "y") {
		aliases = append(aliases, strings.TrimSuffix(resource, "y")+"ies")
	}
	if strings.HasSuffix(resource, "e") {
		aliases = append(aliases, resource+"s")
	}
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if !opsContainsString(result, alias) {
			result = append(result, alias)
		}
	}
	return result
}

func opsContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// OperationsAssistantConfig is the explicitly enabled primary-config route
// for delegate_operations. It describes one exact target projection and
// contains no secret, kubeconfig, memory, or environment fields.
type OperationsAssistantConfig struct {
	Enabled           bool            `json:"enabled,omitempty"`
	Backend           string          `json:"backend,omitempty"`
	Target            OpsTarget       `json:"target,omitempty"`
	Capabilities      []OpsCapability `json:"capabilities,omitempty"`
	ReadOnlyScout     bool            `json:"read_only_scout,omitempty"`
	AllowMutations    bool            `json:"allow_mutations,omitempty"`
	MaxTasks          int             `json:"max_tasks,omitempty"`
	MaxTimeoutSeconds int             `json:"max_timeout_seconds,omitempty"`
	MaxCommands       int             `json:"max_commands,omitempty"`
	MaxSteps          int             `json:"max_steps,omitempty"`
	MaxOutputBytes    int             `json:"max_output_bytes,omitempty"`
	MaxResultBytes    int             `json:"max_result_bytes,omitempty"`
}

func DefaultOperationsAssistantConfig() OperationsAssistantConfig {
	return OperationsAssistantConfig{Backend: CodingBackendNameLocalChild, ReadOnlyScout: true}
}

func (config OperationsAssistantConfig) Validate() error {
	if config.Backend != "" {
		if err := validateCodingBackendName(config.Backend); err != nil {
			return fmt.Errorf("operations backend: %w", err)
		}
	}
	limits := OpsDelegationLimits{MaxTasks: config.MaxTasks, MaxTimeoutSeconds: config.MaxTimeoutSeconds, MaxCommands: config.MaxCommands, MaxSteps: config.MaxSteps, MaxOutputBytes: config.MaxOutputBytes, MaxResultBytes: config.MaxResultBytes}
	if err := limits.Validate(); err != nil {
		return err
	}
	if !config.Enabled {
		return nil
	}
	backend := strings.TrimSpace(config.Backend)
	if backend != "" && backend != CodingBackendNameLocalChild && backend != "bee-run-command" && backend != "local" && backend != "run-command" {
		return fmt.Errorf("operations backend %q is not available in the built-in parent", backend)
	}
	if err := config.Target.Validate(); err != nil {
		return fmt.Errorf("operations target: %w", err)
	}
	projection := OpsCapabilityProjection{Target: config.Target, Capabilities: append([]OpsCapability(nil), config.Capabilities...), MaxTimeoutSeconds: config.MaxTimeoutSeconds, MaxCommands: config.MaxCommands, MaxSteps: config.MaxSteps, MaxOutputBytes: config.MaxOutputBytes, ReadOnlyScout: true}
	if len(projection.Capabilities) == 0 {
		projection.Capabilities = []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute}
	}
	if err := projection.Validate(); err != nil {
		return err
	}
	if config.AllowMutations {
		return errors.New("operations command backend is read-only; allow_mutations must remain false")
	}
	return nil
}

func (config OperationsAssistantConfig) projection() OpsCapabilityProjection {
	capabilities := append([]OpsCapability(nil), config.Capabilities...)
	if len(capabilities) == 0 {
		capabilities = []OpsCapability{OpsCapabilityRead, OpsCapabilityExecute}
	}
	return OpsCapabilityProjection{Target: config.Target, Capabilities: capabilities, MaxTimeoutSeconds: config.MaxTimeoutSeconds, MaxCommands: config.MaxCommands, MaxSteps: config.MaxSteps, MaxOutputBytes: config.MaxOutputBytes, ReadOnlyScout: true}
}

func (config OperationsAssistantConfig) limits() OpsDelegationLimits {
	return OpsDelegationLimits{MaxTasks: config.MaxTasks, MaxTimeoutSeconds: config.MaxTimeoutSeconds, MaxCommands: config.MaxCommands, MaxSteps: config.MaxSteps, MaxOutputBytes: config.MaxOutputBytes, MaxResultBytes: config.MaxResultBytes}
}

func newConfiguredOperationsDelegation(config OperationsAssistantConfig, scope ExecutionScope) (*OperationsDelegationService, error) {
	if !config.Enabled {
		return nil, nil
	}
	backendName := strings.TrimSpace(config.Backend)
	if backendName == "" {
		backendName = CodingBackendNameLocalChild
	}
	var backend OpsBackend
	var err error
	switch backendName {
	case CodingBackendNameLocalChild:
		backend, err = NewOpsLocalChildReadOnlyScoutBackend(scope, config.projection())
	case "bee-run-command", "local", "run-command":
		backend, err = NewOpsReadOnlyCommandBackend(scope, config.projection())
	default:
		return nil, fmt.Errorf("operations backend %q is not available in the built-in parent", backendName)
	}
	if err != nil {
		return nil, err
	}
	service := NewOperationsDelegationService(backend)
	service.AllowMutations = false
	service.Limits = config.limits()
	return service, nil
}
