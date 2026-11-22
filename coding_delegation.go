package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	CodingDelegationToolName = "delegate_coding"
	DelegateCodingToolName   = CodingDelegationToolName
	codingDelegationParentID = "bee-parent"
	codingDelegationMaxTasks = 1
)

var (
	ErrCodingDelegationNotConfigured = errors.New("coding delegation is not configured")
	ErrCodingDelegationPlanRequired  = errors.New("a valid parent-owned coding plan is required before delegation")
)

var codingDelegationSequence atomic.Uint64

// CodingDelegationBudget is the model-facing bounded budget. Zero values use
// the service's conservative defaults; callers may never exceed those bounds.
type CodingDelegationBudget struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxSteps       int `json:"max_steps,omitempty"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

func (budget CodingDelegationBudget) Validate() error {
	if budget.TimeoutSeconds < 0 || budget.MaxSteps < 0 || budget.MaxOutputBytes < 0 {
		return errors.New("coding delegation budget values must not be negative")
	}
	return nil
}

// CodingDelegationRequest is the structured model-facing request. It contains
// no provider credentials, shell command, SSH grant, or memory capability.
type CodingDelegationRequest struct {
	Role               CodingBackendRole      `json:"role"`
	Backend            string                 `json:"backend,omitempty"`
	Goal               string                 `json:"goal"`
	Prompt             string                 `json:"prompt,omitempty"`
	ProjectRoot        string                 `json:"project_root,omitempty"`
	AcceptanceCriteria []string               `json:"acceptance_criteria"`
	Budget             CodingDelegationBudget `json:"budget,omitempty"`
}

func (request CodingDelegationRequest) Validate() error {
	if !request.Role.valid() {
		return fmt.Errorf("coding delegation role %q is invalid", request.Role)
	}
	if strings.TrimSpace(request.Goal) == "" {
		return errors.New("coding delegation goal is required")
	}
	if err := request.Budget.Validate(); err != nil {
		return err
	}
	if len(request.AcceptanceCriteria) == 0 {
		return errors.New("coding delegation acceptance criteria are required")
	}
	for index, criterion := range request.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("coding delegation acceptance criterion %d is empty", index)
		}
	}
	if strings.TrimSpace(request.Backend) != "" {
		if err := validateCodingBackendName(request.Backend); err != nil {
			return err
		}
	}
	return nil
}

// CodingDelegationLimits are parent-owned maximums for one model request.
type CodingDelegationLimits struct {
	MaxTimeoutSeconds int `json:"max_timeout_seconds,omitempty"`
	MaxSteps          int `json:"max_steps,omitempty"`
	MaxOutputBytes    int `json:"max_output_bytes,omitempty"`
	MaxResultBytes    int `json:"max_result_bytes,omitempty"`
}

func (limits CodingDelegationLimits) withDefaults() CodingDelegationLimits {
	if limits.MaxTimeoutSeconds <= 0 {
		limits.MaxTimeoutSeconds = 15 * 60
	}
	if limits.MaxSteps <= 0 {
		limits.MaxSteps = 64
	}
	if limits.MaxOutputBytes <= 0 {
		limits.MaxOutputBytes = 1 << 20
	}
	if limits.MaxResultBytes <= 0 {
		limits.MaxResultBytes = 32 << 10
	}
	return limits
}

func (limits CodingDelegationLimits) Validate() error {
	if limits.MaxTimeoutSeconds < 0 || limits.MaxSteps < 0 || limits.MaxOutputBytes < 0 || limits.MaxResultBytes < 0 {
		return errors.New("coding delegation limits must not be negative")
	}
	return nil
}

// CodingDelegationEvent is bounded progress or terminal evidence from the
// parent-owned delegation service. It is descriptive and never executable.
type CodingDelegationEvent struct {
	TaskID           string                 `json:"task_id"`
	ParentID         string                 `json:"parent_id,omitempty"`
	Role             CodingBackendRole      `json:"role,omitempty"`
	Backend          string                 `json:"backend,omitempty"`
	RequestedBackend string                 `json:"requested_backend,omitempty"`
	EffectiveBackend string                 `json:"effective_backend,omitempty"`
	FallbackUsed     bool                   `json:"fallback_used,omitempty"`
	FallbackReason   string                 `json:"fallback_reason,omitempty"`
	Attempts         []CodingBackendAttempt `json:"attempts,omitempty"`
	Type             string                 `json:"type"`
	Text             string                 `json:"text,omitempty"`
	Info             string                 `json:"info,omitempty"`
	Completion       *ChildCompletionPacket `json:"completion,omitempty"`
	At               time.Time              `json:"at"`
}

type CodingDelegationObserver func(CodingDelegationEvent)

type codingDelegationContext struct {
	planProvider func() *TaskPlan
	parentID     string
	observer     CodingDelegationObserver
}

type codingDelegationContextKey struct{}

func withCodingDelegationContext(ctx context.Context, value codingDelegationContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codingDelegationContextKey{}, value)
}

func codingDelegationContextValue(ctx context.Context) codingDelegationContext {
	if ctx == nil {
		return codingDelegationContext{}
	}
	value, _ := ctx.Value(codingDelegationContextKey{}).(codingDelegationContext)
	return value
}

// CodingDelegationResult is the compact result returned to the model-facing
// tool. The completion packet remains the authoritative child result.
type CodingDelegationResult struct {
	TaskID           string                  `json:"task_id"`
	ParentID         string                  `json:"parent_id,omitempty"`
	Role             CodingBackendRole       `json:"role"`
	Backend          string                  `json:"backend,omitempty"`
	RequestedBackend string                  `json:"requested_backend,omitempty"`
	EffectiveBackend string                  `json:"effective_backend,omitempty"`
	FallbackUsed     bool                    `json:"fallback_used,omitempty"`
	FallbackReason   string                  `json:"fallback_reason,omitempty"`
	Attempts         []CodingBackendAttempt  `json:"attempts,omitempty"`
	Completion       ChildCompletionPacket   `json:"completion"`
	Progress         []CodingDelegationEvent `json:"progress,omitempty"`
	Error            string                  `json:"error,omitempty"`
}

func (result CodingDelegationResult) bounded(maxBytes int) CodingDelegationResult {
	result.Completion = boundCompletion(result.Completion)
	result.Progress = append([]CodingDelegationEvent(nil), result.Progress...)
	if len(result.Progress) > 16 {
		result.Progress = result.Progress[:16]
	}
	for index := range result.Progress {
		result.Progress[index].Text = truncateText(result.Progress[index].Text, 1024)
		result.Progress[index].Info = truncateText(result.Progress[index].Info, 2048)
		result.Progress[index].RequestedBackend = truncateText(result.Progress[index].RequestedBackend, 128)
		result.Progress[index].EffectiveBackend = truncateText(result.Progress[index].EffectiveBackend, 128)
		result.Progress[index].FallbackReason = truncateText(result.Progress[index].FallbackReason, 1024)
		if len(result.Progress[index].Attempts) > 8 {
			result.Progress[index].Attempts = result.Progress[index].Attempts[:8]
		}
		if result.Progress[index].Completion != nil {
			packet := boundCompletion(*result.Progress[index].Completion)
			result.Progress[index].Completion = &packet
		}
	}
	result.Error = truncateText(result.Error, 2048)
	result.RequestedBackend = truncateText(result.RequestedBackend, 128)
	result.EffectiveBackend = truncateText(result.EffectiveBackend, 128)
	result.FallbackReason = truncateText(result.FallbackReason, 1024)
	if len(result.Attempts) > 8 {
		result.Attempts = result.Attempts[:8]
	}
	for index := range result.Attempts {
		result.Attempts[index].Backend = truncateText(result.Attempts[index].Backend, 128)
		result.Attempts[index].AttemptID = truncateText(result.Attempts[index].AttemptID, 128)
		result.Attempts[index].WorkspacePath = truncateText(result.Attempts[index].WorkspacePath, 512)
		result.Attempts[index].Summary = truncateText(result.Attempts[index].Summary, 2048)
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 10
	}
	data, err := json.Marshal(result)
	if err == nil && len(data) <= maxBytes {
		return result
	}
	result.Progress = nil
	result.Completion = compactCompletion(result.Completion, maxBytes)
	if data, err = json.Marshal(result); err == nil && len(data) <= maxBytes {
		return result
	}
	result.Completion.ChangedFiles = nil
	result.Completion.Commands = nil
	result.Completion.Tests = nil
	result.Completion.Evidence = nil
	result.Completion.Uncertainty = nil
	result.Completion.Artifacts = nil
	result.Completion.Questions = nil
	result.Completion.Blockers = nil
	result.Completion.Summary = truncateText(result.Completion.Summary, 1024)
	return result
}

func compactCompletion(packet ChildCompletionPacket, maxBytes int) ChildCompletionPacket {
	packet = boundCompletion(packet)
	packet.Summary = truncateText(packet.Summary, 2048)
	packet.ChangedFiles = boundedStrings(packet.ChangedFiles, 32, 256)
	packet.Tests = boundedStrings(packet.Tests, 32, 512)
	packet.Evidence = boundedStrings(packet.Evidence, 32, 1024)
	packet.Artifacts = boundedStrings(packet.Artifacts, 32, 256)
	packet.Blockers = boundedStrings(packet.Blockers, 16, 512)
	_ = maxBytes
	return packet
}

// CodingDelegationService is the parent-owned execution seam. It is the only
// component allowed to combine a routed backend with a ChildTask and optional
// worktree orchestration.
type CodingDelegationService struct {
	Registry      *CodingBackendRegistry
	ParentScope   ExecutionScope
	ParentID      string
	Workspace     WorkspaceManager
	BaseCommit    string
	ProjectRoot   string
	Trace         TraceSink
	LocalExecutor ChildExecutor
	Limits        CodingDelegationLimits
	PlanProvider  func() *TaskPlan
	Observer      CodingDelegationObserver
}

func NewCodingDelegationService(registry *CodingBackendRegistry, scope ExecutionScope, workspace WorkspaceManager, baseCommit string) (*CodingDelegationService, error) {
	if registry == nil {
		return nil, errors.New("coding backend registry is required")
	}
	if err := scope.ValidateShape(); err != nil {
		return nil, fmt.Errorf("coding delegation parent scope: %w", err)
	}
	return &CodingDelegationService{Registry: registry, ParentScope: scope, ParentID: codingDelegationParentID, Workspace: workspace, BaseCommit: baseCommit, Limits: CodingDelegationLimits{}.withDefaults()}, nil
}

func (service *CodingDelegationService) Delegate(ctx context.Context, request CodingDelegationRequest) (CodingDelegationResult, error) {
	result := CodingDelegationResult{Role: request.Role}
	if service == nil || service.Registry == nil {
		return result, ErrCodingDelegationNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	limits := service.Limits.withDefaults()
	if err := limits.Validate(); err != nil {
		return result, err
	}
	if request.Budget.TimeoutSeconds > limits.MaxTimeoutSeconds || request.Budget.MaxSteps > limits.MaxSteps || request.Budget.MaxOutputBytes > limits.MaxOutputBytes {
		return result, errors.New("coding delegation budget exceeds parent limit")
	}
	parentScope, err := service.ParentScope.rootPath()
	if err != nil {
		return result, err
	}
	projectRoot := strings.TrimSpace(request.ProjectRoot)
	projectBase := strings.TrimSpace(service.ProjectRoot)
	if projectBase == "" {
		projectBase = parentScope
	}
	if projectRoot == "" {
		projectRoot = projectBase
	}
	if !filepath.IsAbs(projectRoot) {
		projectRoot = filepath.Join(projectBase, projectRoot)
	}
	projectRoot, err = existingResolvedDir(projectRoot)
	if err != nil {
		return result, fmt.Errorf("coding delegation project root: %w", err)
	}
	if !scopeWithin(parentScope, projectRoot) {
		return result, errors.New("coding delegation project root must remain within parent scope")
	}
	plan := service.parentPlan(ctx)
	if plan == nil {
		return result, ErrCodingDelegationPlanRequired
	}
	if err := plan.Validate(); err != nil {
		return result, fmt.Errorf("validate parent coding plan: %w", err)
	}
	if plan.NeedsClarification() {
		return result, ErrPlanHasClarification
	}
	parentID := service.ParentID
	contextValue := codingDelegationContextValue(ctx)
	if strings.TrimSpace(contextValue.parentID) != "" {
		parentID = contextValue.parentID
	}
	if strings.TrimSpace(parentID) == "" {
		parentID = codingDelegationParentID
	}
	taskID := nextCodingDelegationTaskID()
	result.TaskID, result.ParentID = taskID, parentID
	backendName := strings.TrimSpace(request.Backend)
	route, routeErr := service.Registry.Route(request.Role, backendName)
	if routeErr != nil {
		// A composition root may continue to own the existing local child
		// executor without registering a second backend factory.
		resolvedName, resolveErr := service.Registry.Config().ResolveName(request.Role, backendName)
		if service.LocalExecutor != nil && resolveErr == nil && resolvedName == CodingBackendNameLocalChild {
			route = CodingBackendRoute{Role: request.Role, BackendName: resolvedName, Enabled: true}
			routeErr = nil
		} else {
			fallbackName, _ := service.Registry.Config().ResolveFallbackName(request.Role, "")
			if service.LocalExecutor == nil || fallbackName != CodingBackendNameLocalChild || !errors.Is(routeErr, ErrCodingBackendUnavailable) {
				return result, routeErr
			}
			route = CodingBackendRoute{Role: request.Role, BackendName: strings.TrimSpace(resolvedName), Enabled: true}
			result.RequestedBackend = route.BackendName
			result.EffectiveBackend = fallbackName
			result.FallbackUsed = true
			result.FallbackReason = "requested backend unavailable before execution"
			result.Attempts = []CodingBackendAttempt{{Backend: route.BackendName, Status: ChildFailed, FailureKind: BackendFailureUnavailable, Summary: truncateText(routeErr.Error(), 2048)}}
			backendName = fallbackName
			route.BackendName = fallbackName
		}
	}
	result.Backend = route.BackendName
	result.RequestedBackend = strings.TrimSpace(result.RequestedBackend)
	if result.RequestedBackend == "" {
		result.RequestedBackend = route.BackendName
	}
	var childExecutor ChildExecutor
	if !route.Enabled || route.Backend == nil {
		if service.LocalExecutor != nil && route.BackendName == CodingBackendNameLocalChild {
			childExecutor = service.LocalExecutor
			result.Backend = CodingBackendNameLocalChild
			result.EffectiveBackend = CodingBackendNameLocalChild
		} else {
			return result, ErrCodingBackendsDisabled
		}
	} else {
		primaryExecutor := &ACPChildExecutor{
			CodingBackend: route.Backend,
			Plan:          append([]string(nil), plan.Steps...),
			Role:          request.Role,
			// An implementer delegation is parent-approved by the validated
			// plan and isolated worktree. The backend config remains a second,
			// explicit opt-in; reviewer/scout tasks never receive this key.
			PermissionPolicy: ACPPermissionPolicy{AutoApprovePermissions: request.Role == CodingRoleImplementer, Role: request.Role},
		}
		childExecutor = primaryExecutor
		fallbackName, _ := service.Registry.Config().ResolveFallbackName(request.Role, "")
		if service.LocalExecutor != nil && fallbackName == CodingBackendNameLocalChild && fallbackName != route.BackendName {
			childExecutor = &codingFallbackExecutor{primary: primaryExecutor, fallback: service.LocalExecutor, workspace: service.Workspace, baseCommit: service.BaseCommit, requestedName: route.BackendName, fallbackName: fallbackName}
		}
	}
	budget := request.Budget
	if budget.TimeoutSeconds <= 0 {
		budget.TimeoutSeconds = minPositive(service.ParentScope.TimeoutSeconds, limits.MaxTimeoutSeconds)
	}
	if budget.MaxSteps <= 0 {
		budget.MaxSteps = limits.MaxSteps
	}
	if budget.MaxOutputBytes <= 0 {
		budget.MaxOutputBytes = minPositive(service.ParentScope.MaxOutputBytes, limits.MaxOutputBytes)
	}
	if budget.MaxOutputBytes <= 0 {
		budget.MaxOutputBytes = limits.MaxOutputBytes
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(request.Goal)
	}
	mutability := TaskMutable
	if request.Role == CodingRoleScout {
		mutability = TaskReadOnly
	}
	task := ChildTask{
		TaskID: taskID, ParentID: parentID, Objective: strings.TrimSpace(request.Goal), Prompt: prompt,
		AllowedTools: codingDelegationTools(request.Role), Mutability: mutability,
		Scope:       ExecutionScope{Root: projectRoot, TimeoutSeconds: budget.TimeoutSeconds, MaxOutputBytes: budget.MaxOutputBytes},
		Budget:      ChildBudget{TimeoutSeconds: budget.TimeoutSeconds, MaxSteps: budget.MaxSteps, MaxOutputBytes: budget.MaxOutputBytes},
		ArtifactDir: filepath.Join("generated", "children", taskID), AcceptanceCriteria: append([]string(nil), request.AcceptanceCriteria...),
	}
	if err := task.Validate(); err != nil {
		return result, err
	}
	orchestrationPlan := OrchestrationPlan{
		OrchestrationID: taskID + "-orchestration", ParentID: parentID,
		Tasks: []PlannedChild{{Task: task}}, Budgets: OrchestrationBudgets{TimeoutSeconds: budget.TimeoutSeconds, MaxSteps: budget.MaxSteps, MaxOutputBytes: budget.MaxOutputBytes},
		MaxConcurrent: codingDelegationMaxTasks,
	}
	var packet ChildCompletionPacket
	var runErr error
	if mutability == TaskMutable {
		if service.Workspace == nil {
			return result, errors.New("mutable coding delegation requires a parent-owned workspace manager")
		}
		orchestrator, err := NewWorktreeOrchestrator(childExecutor, service.Workspace, service.BaseCommit, service.traceSink(ctx, result.Backend, request.Role))
		if err != nil {
			return result, err
		}
		packet, runErr = runCodingOrchestration(ctx, orchestrator, orchestrationPlan, service, result.Backend, request.Role, &result)
	} else {
		orchestrator, err := NewSequentialOrchestrator(childExecutor, service.traceSink(ctx, result.Backend, request.Role))
		if err != nil {
			return result, err
		}
		packet, runErr = runCodingOrchestration(ctx, orchestrator, orchestrationPlan, service, result.Backend, request.Role, &result)
	}
	if packet.TaskID == "" {
		packet.TaskID = taskID
	}
	if packet.ParentID == "" {
		packet.ParentID = parentID
	}
	if packet.Status == "" {
		packet.Status = ChildFailed
	}
	if strings.TrimSpace(packet.Summary) == "" {
		packet.Summary = "coding delegation completed without a summary"
	}
	if result.FallbackUsed && !packet.FallbackUsed {
		packet.FallbackUsed = true
		packet.FallbackReason = result.FallbackReason
		packet.Attempts = append([]CodingBackendAttempt(nil), result.Attempts...)
	}
	result.Completion = boundCompletion(packet)
	if result.Completion.RequestedBackend == "" {
		result.Completion.RequestedBackend = result.RequestedBackend
	}
	if result.Completion.EffectiveBackend == "" {
		result.Completion.EffectiveBackend = result.Backend
	}
	result.RequestedBackend = result.Completion.RequestedBackend
	result.EffectiveBackend = result.Completion.EffectiveBackend
	if result.Completion.FallbackUsed {
		result.FallbackUsed = true
	}
	if result.Completion.FallbackReason != "" {
		result.FallbackReason = result.Completion.FallbackReason
	}
	result.Attempts = append(result.Attempts, result.Completion.Attempts...)
	result = result.bounded(limits.MaxResultBytes)
	if runErr != nil {
		result.Error = truncateText(runErr.Error(), 2048)
		return result, runErr
	}
	return result, nil
}

func runCodingOrchestration(ctx context.Context, orchestrator interface {
	Run(context.Context, OrchestrationPlan) (OrchestrationResult, error)
}, plan OrchestrationPlan, service *CodingDelegationService, backend string, role CodingBackendRole, result *CodingDelegationResult) (ChildCompletionPacket, error) {
	service.emit(ctx, CodingDelegationEvent{TaskID: plan.Tasks[0].Task.TaskID, ParentID: plan.ParentID, Role: role, Backend: backend, Type: "started", Info: "parent-owned coding delegation started"})
	orchestration, err := orchestrator.Run(ctx, plan)
	if len(orchestration.Tasks) > 0 && orchestration.Tasks[0].Packet != nil {
		packet := *orchestration.Tasks[0].Packet
		event := CodingDelegationEvent{TaskID: packet.TaskID, ParentID: packet.ParentID, Role: role, Backend: backend, Type: "completed", Completion: &packet, Info: truncateText(packet.Summary, 2048), RequestedBackend: packet.RequestedBackend, EffectiveBackend: packet.EffectiveBackend, FallbackUsed: packet.FallbackUsed, FallbackReason: packet.FallbackReason, Attempts: append([]CodingBackendAttempt(nil), packet.Attempts...)}
		service.emit(ctx, event)
		result.Progress = append(result.Progress, event)
		return packet, err
	}
	if err != nil {
		service.emit(ctx, CodingDelegationEvent{TaskID: plan.Tasks[0].Task.TaskID, ParentID: plan.ParentID, Role: role, Backend: backend, Type: "failed", Info: truncateText(err.Error(), 2048)})
	}
	return ChildCompletionPacket{TaskID: plan.Tasks[0].Task.TaskID, ParentID: plan.ParentID, Status: ChildFailed, Summary: "coding delegation did not produce a completion packet"}, err
}

func (service *CodingDelegationService) parentPlan(ctx context.Context) *TaskPlan {
	if value := codingDelegationContextValue(ctx); value.planProvider != nil {
		return value.planProvider()
	}
	if service != nil && service.PlanProvider != nil {
		return service.PlanProvider()
	}
	return nil
}

type codingDelegationTrace struct {
	service *CodingDelegationService
	ctx     context.Context
	role    CodingBackendRole
	backend string
	sink    TraceSink
}

func (trace *codingDelegationTrace) AppendTrace(ctx context.Context, event TraceEvent) error {
	if trace == nil {
		return errors.New("coding delegation trace is nil")
	}
	if trace.sink != nil {
		if err := trace.sink.AppendTrace(ctx, event); err != nil {
			return err
		}
	}
	if event.Observation != nil {
		observation := event.Observation
		trace.service.emit(trace.ctx, CodingDelegationEvent{TaskID: observation.TaskID, ParentID: observation.ParentID, Role: trace.role, Backend: trace.backend, Type: "progress", Text: observation.Delta, Info: observation.Info, Completion: observation.Completion})
	}
	if event.Completion != nil {
		trace.service.emit(trace.ctx, CodingDelegationEvent{TaskID: event.TaskID, ParentID: event.ParentID, Role: trace.role, Backend: trace.backend, Type: "progress", Info: "completion evidence recorded", Completion: event.Completion})
	}
	return nil
}

func (service *CodingDelegationService) traceSink(ctx context.Context, backend string, role CodingBackendRole) TraceSink {
	if service == nil {
		return nil
	}
	return &codingDelegationTrace{service: service, ctx: ctx, role: role, backend: backend, sink: service.Trace}
}

func (service *CodingDelegationService) emit(ctx context.Context, event CodingDelegationEvent) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Text = truncateText(event.Text, 4096)
	event.Info = truncateText(event.Info, 4096)
	if event.Completion != nil {
		packet := boundCompletion(*event.Completion)
		event.Completion = &packet
		event.RequestedBackend = packet.RequestedBackend
		event.EffectiveBackend = packet.EffectiveBackend
		event.FallbackUsed = packet.FallbackUsed
		event.FallbackReason = packet.FallbackReason
		event.Attempts = append([]CodingBackendAttempt(nil), packet.Attempts...)
	}
	if len(event.Attempts) > 8 {
		event.Attempts = event.Attempts[:8]
	}
	if value := codingDelegationContextValue(ctx); value.observer != nil {
		value.observer(event)
	}
	if service != nil && service.Observer != nil {
		service.Observer(event)
	}
}

func nextCodingDelegationTaskID() string {
	sequence := codingDelegationSequence.Add(1)
	return fmt.Sprintf("coding-%d-%d", time.Now().UTC().UnixNano(), sequence)
}

func codingDelegationTools(role CodingBackendRole) []string {
	tools := []string{"read_file", "list_files", "find_files", "grep"}
	if role != CodingRoleScout {
		tools = append(tools, "write_file", "edit_file", "run_command")
	}
	return tools
}

func minPositive(value, maximum int) int {
	if value > 0 && (maximum <= 0 || value < maximum) {
		return value
	}
	return maximum
}

func existingResolvedDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	stat, err := os.Stat(info)
	if err != nil {
		return "", err
	}
	if !stat.IsDir() {
		return "", errors.New("path must be a directory")
	}
	return filepath.Clean(info), nil
}

func NewCodingDelegationTool(service *CodingDelegationService) Tool {
	return ToolFunc{ToolDefinition: codingDelegationToolDefinition(), Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		var request CodingDelegationRequest
		if err := parseArgs(args, &request); err != nil {
			return "", fmt.Errorf("delegate_coding arguments: %w", err)
		}
		if service == nil {
			return "", ErrCodingDelegationNotConfigured
		}
		result, err := service.Delegate(ctx, request)
		result = result.bounded(service.Limits.withDefaults().MaxResultBytes)
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return "", marshalErr
		}
		return string(data), err
	}}
}

func codingDelegationToolDefinition() ToolDefinition {
	return ToolDefinition{Name: CodingDelegationToolName, Description: "Delegate one ordinary, bounded coding task to a parent-routed backend in an isolated worktree. A valid parent-owned bee_plan is required.", Risk: RiskHigh, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string","enum":["implementer","reviewer","scout"]},"backend":{"type":"string"},"goal":{"type":"string"},"prompt":{"type":"string"},"project_root":{"type":"string"},"acceptance_criteria":{"type":"array","items":{"type":"string"}},"budget":{"type":"object","properties":{"timeout_seconds":{"type":"integer","minimum":0},"max_steps":{"type":"integer","minimum":0},"max_output_bytes":{"type":"integer","minimum":0}}}},"required":["role","goal","acceptance_criteria"]}`)}
}
