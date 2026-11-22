package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChildStatus is the terminal state reported by one bounded child execution.
type ChildStatus string

const (
	ChildSucceeded      ChildStatus = "succeeded"
	ChildFailed         ChildStatus = "failed"
	ChildTimedOut       ChildStatus = "timed_out"
	ChildCancelled      ChildStatus = "cancelled"
	ChildBudgetExceeded ChildStatus = "budget_exceeded"
	ChildMalformed      ChildStatus = "malformed"
)

// ChildBudget limits one child execution. Zero values select conservative
// defaults from the child execution scope or Agent configuration.
type ChildBudget struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxSteps       int `json:"max_steps,omitempty"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

func (budget ChildBudget) ValidateShape() error {
	if budget.TimeoutSeconds < 0 {
		return errors.New("child budget timeout_seconds must not be negative")
	}
	if budget.MaxSteps < 0 {
		return errors.New("child budget max_steps must not be negative")
	}
	if budget.MaxOutputBytes < 0 {
		return errors.New("child budget max_output_bytes must not be negative")
	}
	return nil
}

func (budget ChildBudget) timeout(scope ExecutionScope) time.Duration {
	if budget.TimeoutSeconds > 0 {
		return time.Duration(budget.TimeoutSeconds) * time.Second
	}
	return scope.timeout()
}

func (budget ChildBudget) maxSteps(config Config) int {
	if budget.MaxSteps > 0 {
		return budget.MaxSteps
	}
	if config.Agent.MaxToolSteps > 0 || config.Agent.maxToolStepsExplicit {
		return config.Agent.MaxToolSteps
	}
	return DefaultMaxToolSteps
}

func (budget ChildBudget) maxOutput(scope ExecutionScope) int {
	if budget.MaxOutputBytes > 0 {
		return budget.MaxOutputBytes
	}
	return scope.maxOutput()
}

// TaskMutability declares whether a child may use side-effecting tools.
// An unspecified value is intentionally not a promise of read-only behavior:
// parallel orchestration may infer read-only only for an empty or known
// read-only tool set.
type TaskMutability string

// ChildTaskMutability is an explicit compatibility name for callers that
// prefer the contract type to be tied to ChildTask.
type ChildTaskMutability = TaskMutability

type ChildMutability = TaskMutability

const (
	TaskMutabilityUnspecified TaskMutability = ""
	TaskReadOnly              TaskMutability = "read_only"
	TaskMutable               TaskMutability = "mutable"

	// Compatibility aliases make the contract easy to discover without
	// creating a second representation on the wire.
	ChildMutabilityUnspecified = TaskMutabilityUnspecified
	ChildReadOnly              = TaskReadOnly
	ChildMutable               = TaskMutable
	MutabilityReadOnly         = TaskReadOnly
	MutabilityMutable          = TaskMutable
)

func (mutability TaskMutability) valid() bool {
	return mutability == TaskMutabilityUnspecified || mutability == TaskReadOnly || mutability == TaskMutable
}

// ChildMemoryProjection is a parent-created, read-only subset of transient
// memory context. It carries no MemoryStore or MemoryService capability and
// cannot be used to perform durable writes.
type ChildMemoryProjection struct {
	Packet         MemoryContextPacket `json:"packet"`
	Namespaces     []MemoryNamespace   `json:"namespaces,omitempty"`
	MaxItems       int                 `json:"max_items,omitempty"`
	MaxSensitivity MemorySensitivity   `json:"max_sensitivity,omitempty"`
}

func (projection ChildMemoryProjection) Validate(policy MemoryPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if projection.MaxItems < 0 || projection.MaxItems > policy.MaxContextRecords {
		return fmt.Errorf("child memory projection max_items must be between 0 and %d", policy.MaxContextRecords)
	}
	if projection.MaxSensitivity == MemorySensitivitySensitive || projection.MaxSensitivity == MemorySensitivitySecret {
		return errors.New("child memory projection may not include sensitive memory")
	}
	if projection.MaxSensitivity != "" && projection.MaxSensitivity != MemorySensitivityPublic && projection.MaxSensitivity != MemorySensitivityPersonal {
		return fmt.Errorf("child memory projection max_sensitivity %q is invalid", projection.MaxSensitivity)
	}
	for _, namespace := range projection.Namespaces {
		if err := validateDurableNamespace(string(namespace)); err != nil {
			return err
		}
	}
	if err := projection.Packet.Validate(policy, MemoryActor{Kind: MemoryActorChild, ID: "projection"}); err != nil {
		return err
	}
	// Namespace and sensitivity bounds are applied while projecting below. The
	// packet itself is still validated as a child-readable, non-sensitive
	// packet, but out-of-scope items are intentionally filtered rather than
	// causing the whole parent task to fail.
	return nil
}

// ProjectForChild filters a parent packet before handing it to a child. The
// returned packet is bounded, privacy-filtered, and independent of the input.
func (projection ChildMemoryProjection) ProjectForChild(policy MemoryPolicy) (MemoryContextPacket, error) {
	if err := projection.Validate(policy); err != nil {
		return MemoryContextPacket{}, err
	}
	allowed := make(map[MemoryNamespace]struct{}, len(projection.Namespaces))
	for _, namespace := range projection.Namespaces {
		allowed[namespace] = struct{}{}
	}
	maxItems := projection.MaxItems
	if maxItems == 0 || maxItems > policy.MaxContextRecords {
		maxItems = policy.MaxContextRecords
	}
	packet := MemoryContextPacket{SessionID: projection.Packet.SessionID}
	for _, item := range projection.Packet.Items {
		if len(allowed) > 0 {
			if _, ok := allowed[item.Namespace]; !ok {
				packet.Truncated = true
				continue
			}
		}
		if projection.MaxSensitivity == MemorySensitivityPublic && item.Sensitivity != MemorySensitivityPublic {
			packet.Truncated = true
			continue
		}
		if len(packet.Items) >= maxItems {
			packet.Truncated = true
			break
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return MemoryContextPacket{}, fmt.Errorf("encode child memory projection: %w", err)
		}
		if packet.UsedBytes+len(encoded) > policy.MaxContextBytes {
			packet.Truncated = true
			break
		}
		packet.Items = append(packet.Items, item)
		packet.UsedBytes += len(encoded)
	}
	if err := packet.Validate(policy, MemoryActor{Kind: MemoryActorChild, ID: "projection"}); err != nil {
		return MemoryContextPacket{}, err
	}
	return packet, nil
}

// ChildTask is the complete authorization and objective contract for exactly
// one child. The child cannot expand this contract at runtime.
type ChildTask struct {
	TaskID             string                 `json:"task_id"`
	ParentID           string                 `json:"parent_id"`
	Objective          string                 `json:"objective"`
	Prompt             string                 `json:"prompt"`
	AllowedTools       []string               `json:"allowed_tools,omitempty"`
	Mutability         TaskMutability         `json:"mutability,omitempty"`
	Scope              ExecutionScope         `json:"scope"`
	Budget             ChildBudget            `json:"budget"`
	ArtifactDir        string                 `json:"artifact_dir,omitempty"`
	AcceptanceCriteria []string               `json:"acceptance_criteria"`
	MemoryProjection   *ChildMemoryProjection `json:"memory_projection,omitempty"`
	// Workspace and ParentScopeRoot are parent-only transport metadata. They
	// are not serialized into the child protocol or exposed as child authority.
	Workspace       *WorkspaceLease `json:"-"`
	ParentScopeRoot string          `json:"-"`
}

func (task ChildTask) Validate() error {
	if err := validateChildID("task_id", task.TaskID); err != nil {
		return err
	}
	if err := validateChildID("parent_id", task.ParentID); err != nil {
		return err
	}
	if strings.TrimSpace(task.Objective) == "" {
		return errors.New("child objective is required")
	}
	if strings.TrimSpace(task.Prompt) == "" {
		return errors.New("child prompt is required")
	}
	if err := task.Scope.ValidateShape(); err != nil {
		return fmt.Errorf("child scope: %w", err)
	}
	if err := task.Budget.ValidateShape(); err != nil {
		return fmt.Errorf("child budget: %w", err)
	}
	if !task.Mutability.valid() {
		return fmt.Errorf("child mutability %q is invalid", task.Mutability)
	}
	if task.MemoryProjection != nil {
		if err := task.MemoryProjection.Validate(DefaultMemoryPolicy()); err != nil {
			return fmt.Errorf("child memory projection: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(task.AllowedTools))
	for _, name := range task.AllowedTools {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\r\n") {
			return fmt.Errorf("child allowed tool %q is invalid", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("child allowed tools repeat %q", name)
		}
		seen[name] = struct{}{}
	}
	if len(task.AcceptanceCriteria) == 0 {
		return errors.New("child acceptance criteria are required")
	}
	for i, criterion := range task.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("child acceptance criterion %d is empty", i)
		}
	}
	return nil
}

func validateChildID(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("child %s is required", label)
	}
	if strings.ContainsAny(value, "\\/ \t\r\n") {
		return fmt.Errorf("child %s contains an unsafe character", label)
	}
	return nil
}

// ChildCompletionPacket is the only result a child should hand to its parent.
// It is intentionally descriptive rather than executable: the parent decides
// whether any reported action is accepted or followed up.
type ChildCompletionPacket struct {
	TaskID           string                       `json:"task_id"`
	ParentID         string                       `json:"parent_id"`
	Status           ChildStatus                  `json:"status"`
	Summary          string                       `json:"summary"`
	ChangedFiles     []string                     `json:"changed_files,omitempty"`
	Commands         []string                     `json:"commands,omitempty"`
	Tests            []string                     `json:"tests,omitempty"`
	Evidence         []string                     `json:"evidence,omitempty"`
	Uncertainty      []string                     `json:"uncertainty,omitempty"`
	Artifacts        []string                     `json:"artifacts,omitempty"`
	Questions        []string                     `json:"questions,omitempty"`
	Blockers         []string                     `json:"blockers,omitempty"`
	OutputTruncated  bool                         `json:"output_truncated,omitempty"`
	Steps            int                          `json:"steps,omitempty"`
	RequestedBackend string                       `json:"requested_backend,omitempty"`
	EffectiveBackend string                       `json:"effective_backend,omitempty"`
	FallbackUsed     bool                         `json:"fallback_used,omitempty"`
	FallbackReason   string                       `json:"fallback_reason,omitempty"`
	Attempts         []CodingBackendAttempt       `json:"attempts,omitempty"`
	FailureKind      CodingBackendFailureKind     `json:"failure_kind,omitempty"`
	Manifest         *CompletionManifestReference `json:"completion_manifest,omitempty"`
}

func (packet ChildCompletionPacket) Validate() error {
	if err := validateChildID("task_id", packet.TaskID); err != nil {
		return err
	}
	if err := validateChildID("parent_id", packet.ParentID); err != nil {
		return err
	}
	switch packet.Status {
	case ChildSucceeded, ChildFailed, ChildTimedOut, ChildCancelled, ChildBudgetExceeded, ChildMalformed:
	default:
		return fmt.Errorf("child status %q is invalid", packet.Status)
	}
	if packet.FailureKind != "" {
		switch packet.FailureKind {
		case BackendFailureUnavailable, BackendFailureTask, BackendFailureTimeout, BackendFailureCancelled, BackendFailurePermission, BackendFailureMalformed:
		default:
			return fmt.Errorf("child failure kind %q is invalid", packet.FailureKind)
		}
	}
	if strings.TrimSpace(packet.Summary) == "" {
		return errors.New("child summary is required")
	}
	if packet.Steps < 0 {
		return errors.New("child steps must not be negative")
	}
	if packet.Manifest != nil {
		if err := packet.Manifest.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DecodeChildCompletionPacket validates a packet received from a child
// boundary. It is useful for stdio/RPC adapters and deliberately rejects
// incomplete results instead of guessing missing fields.
func DecodeChildCompletionPacket(data []byte) (ChildCompletionPacket, error) {
	var packet ChildCompletionPacket
	if len(data) == 0 {
		return packet, errors.New("child completion packet is empty")
	}
	if err := json.Unmarshal(data, &packet); err != nil {
		return packet, fmt.Errorf("decode child completion packet: %w", err)
	}
	if err := packet.Validate(); err != nil {
		return packet, fmt.Errorf("validate child completion packet: %w", err)
	}
	return packet, nil
}

// ChildRunner launches exactly one child Agent with a copied executor and a
// child-specific context. It is intentionally not a scheduler: one Run call
// means one child and there is no retry, fan-out, or approval delegation.
type ChildRunner struct {
	Application         *Application
	Executor            *Executor
	RequirePlanOverride *bool
	// ProviderOverride and ConfigOverride are specialized-child seams. Normal
	// children leave them nil and continue using Application.childProvider.
	ProviderOverride Provider
	ConfigOverride   *Config
	// MaxTokensOverride is nil by default so existing child runs inherit the
	// configured generation cap. Specialized children may set it to zero when
	// their provider request must omit max_tokens entirely.
	MaxTokensOverride *int
	mu                sync.Mutex
	running           bool
}

var _ ChildExecutor = (*ChildRunner)(nil)

func NewChildRunner(app *Application) (*ChildRunner, error) {
	if app == nil || app.Provider == nil {
		return nil, errors.New("child runner application provider is required")
	}
	return &ChildRunner{Application: app, Executor: app.Executor}, nil
}

type childRunResult struct {
	response Response
	err      error
}

var errChildOutputBudget = errors.New("child output budget exceeded")

func (runner *ChildRunner) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return runner.RunObserved(ctx, task, nil)
}

func (runner *ChildRunner) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (packet ChildCompletionPacket, runErr error) {
	packet = ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID}
	if runner == nil || runner.Application == nil || runner.Application.Provider == nil {
		return packet, errors.New("child runner application provider is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := task.Validate(); err != nil {
		return packet, err
	}
	if err := task.Scope.Validate(); err != nil {
		return packet, fmt.Errorf("child scope: %w", err)
	}
	runner.mu.Lock()
	if runner.running {
		runner.mu.Unlock()
		return packet, errors.New("child runner already has a child in progress")
	}
	runner.running = true
	runner.mu.Unlock()
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "started"})
	defer func() {
		runner.mu.Lock()
		runner.running = false
		runner.mu.Unlock()
		status := packet.Status
		if status == "" {
			status = ChildFailed
		}
		info := string(status)
		if runErr != nil {
			info = runErr.Error()
		}
		notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "completed", Info: info, Completion: &packet})
	}()
	baseExecutor := runner.Executor
	if baseExecutor == nil {
		baseExecutor = runner.Application.Executor
	}
	requirePlan := runner.Application.RequirePlan
	if runner.RequirePlanOverride != nil {
		requirePlan = *runner.RequirePlanOverride
	}
	childExecutor, err := copyChildExecutor(baseExecutor, task)
	if err != nil {
		return packet, err
	}
	if !requirePlan && childExecutor != nil {
		// The parent has already approved the coding plan. A coding child must
		// not be forced to invent a second plan, but it also must not inherit
		// the parent's mutable plan gate.
		childExecutor.planGate = nil
	}
	artifactDir, err := childArtifactDir(task)
	if err != nil {
		return packet, err
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return packet, fmt.Errorf("create child artifact directory: %w", err)
	}
	if err := os.Chmod(artifactDir, 0o700); err != nil {
		return packet, fmt.Errorf("secure child artifact directory: %w", err)
	}

	before, snapshotNotes := snapshotScope(task.Scope.Root)
	budget := task.Budget
	maxOutput := budget.maxOutput(task.Scope)
	if scopeLimit := task.Scope.maxOutput(); scopeLimit < maxOutput {
		maxOutput = scopeLimit
	}
	var outputMu sync.Mutex
	var output strings.Builder
	var outputTruncated bool
	var commands, tests, evidence []string
	steps := 0
	evidenceBytes := 0
	appendOutput := func(delta string) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		remaining := maxOutput - output.Len()
		if remaining <= 0 {
			outputTruncated = true
			return errChildOutputBudget
		}
		if len(delta) > remaining {
			output.WriteString(delta[:remaining])
			outputTruncated = true
			return errChildOutputBudget
		}
		output.WriteString(delta)
		return nil
	}
	hook := func(event AgentEvent) {
		observation := ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: event.Type, Profile: event.Profile, Role: event.Role}
		switch event.Type {
		case "assistant_message":
			message := event.Message
			observation.Message = &message
		case "tool_call":
			steps++
			call := event.ToolCall
			observation.ToolCall = &call
		case "tool_result":
			result := ToolResultEvent{CallID: event.CallID, ToolName: event.ToolResult.ToolName, Output: event.ToolResult.Output, Error: event.ToolResult.Error}
			observation.ToolResult = &result
		case "approval":
			approval := ApprovalEvent{Request: event.Approval, Decision: event.Decision, Reason: event.Reason}
			observation.Approval = &approval
		case "usage":
			usage := UsageEvent{Provider: event.Provider, Model: event.Model, Usage: event.Usage}
			observation.Usage = &usage
		case "plan":
			observation.Info = event.Plan.Goal
		}
		notifyChild(observer, observation)
		switch event.Type {
		case "tool_call":
			if event.ToolCall.Name == "run_command" {
				var args runArgs
				if parseArgs(json.RawMessage(event.ToolCall.Arguments), &args) == nil {
					commands = append(commands, strings.Join(args.Argv, " "))
					if len(args.Argv) > 0 && isTestCommand(args.Argv[0]) {
						tests = append(tests, strings.Join(args.Argv, " "))
					}
				}
			}
		case "tool_result":
			if event.ToolResult.Output != "" {
				value := truncateText(event.ToolResult.Output, maxOutput)
				if remaining := maxOutput - evidenceBytes; remaining > 0 {
					if len(value) > remaining {
						value = value[:remaining]
					}
					evidence = append(evidence, value)
					evidenceBytes += len(value)
				}
			}
			if event.ToolResult.Error != "" {
				value := event.ToolResult.Error
				if remaining := maxOutput - evidenceBytes; remaining > 0 {
					if len(value) > remaining {
						value = value[:remaining]
					}
					evidence = append(evidence, value)
					evidenceBytes += len(value)
				}
			}
		}
	}

	childTimeout := budget.timeout(task.Scope)
	if scopeTimeout := task.Scope.timeout(); scopeTimeout < childTimeout {
		childTimeout = scopeTimeout
	}
	childCtx, cancel := context.WithTimeout(ctx, childTimeout)
	defer cancel()
	childConfig := runner.Application.ChildConfigOrMain()
	if runner.ConfigOverride != nil {
		childConfig = *runner.ConfigOverride
	}
	childProvider := runner.ProviderOverride
	if childProvider == nil {
		var providerErr error
		childProvider, providerErr = runner.Application.childProvider()
		if providerErr != nil {
			return packet, providerErr
		}
	}
	childProvider = brokerChildProvider(childProvider, runner.Application.ChildBroker)
	options := AgentOptions{Executor: childExecutor, MaxToolSteps: budget.maxSteps(childConfig), RequirePlan: requirePlan, ExposePlanTracking: false, EventHook: hook, MaxTokensOverride: cloneIntPointer(runner.MaxTokensOverride)}
	if task.MemoryProjection != nil {
		memoryPolicy := runner.Application.MemoryPolicy
		if memoryPolicy == (MemoryPolicy{}) {
			memoryPolicy = DefaultMemoryPolicy()
		}
		projection, projectionErr := task.MemoryProjection.ProjectForChild(memoryPolicy)
		if projectionErr != nil {
			return packet, fmt.Errorf("project child memory context: %w", projectionErr)
		}
		options.Context.MemoryContext = &projection
	}
	agent, err := NewAgentWithOptions(childConfig, childProvider, options)
	if err != nil {
		return packet, err
	}
	prompt := buildChildPrompt(task)
	done := make(chan childRunResult, 1)
	go func() {
		response, runErr := agent.Run(childCtx, prompt, func(delta string) error {
			notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "provider_delta", Delta: truncateText(delta, maxOutput)})
			return appendOutput(delta)
		})
		done <- childRunResult{response: response, err: runErr}
	}()

	status := ChildSucceeded
	var response Response
	select {
	case result := <-done:
		response = result.response
		runErr = result.err
		if errors.Is(runErr, errChildOutputBudget) {
			status = ChildBudgetExceeded
		} else if runErr != nil {
			status = ChildFailed
		}
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = ChildTimedOut
		} else {
			status = ChildCancelled
		}
		cancel()
	case <-childCtx.Done():
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			status = ChildTimedOut
		} else {
			status = ChildCancelled
		}
		cancel()
	}
	if runErr == nil && status == ChildSucceeded {
		// A child that returned no assistant text still has a valid, bounded
		// result; the summary below records that fact explicitly.
	}
	outputMu.Lock()
	summary := strings.TrimSpace(output.String())
	outputMu.Unlock()
	if summary == "" {
		summary = strings.TrimSpace(response.Message.Content)
	}
	if summary == "" {
		if runErr != nil {
			summary = runErr.Error()
		} else {
			summary = "child completed without assistant output"
		}
	}
	packet = ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: status, Summary: truncateText(summary, maxOutput), Commands: uniqueStrings(commands), Tests: uniqueStrings(tests), Evidence: uniqueStrings(evidence), OutputTruncated: outputTruncated, Steps: steps}
	packet.ChangedFiles, packet.Uncertainty = diffScope(task.Scope.Root, before, snapshotNotes)
	packet.Artifacts = listArtifacts(artifactDir, task.Scope.Root)
	if runErr != nil && !errors.Is(runErr, errChildOutputBudget) {
		packet.Blockers = append(packet.Blockers, runErr.Error())
	}
	if status == ChildTimedOut {
		packet.Blockers = append(packet.Blockers, "child wall-clock budget expired")
	}
	if status == ChildCancelled {
		packet.Blockers = append(packet.Blockers, "parent cancelled child execution")
	}
	if status == ChildBudgetExceeded {
		packet.Blockers = append(packet.Blockers, "child output budget expired")
	}
	if status == ChildSucceeded && len(task.AcceptanceCriteria) > 0 {
		packet.Evidence = append(packet.Evidence, "acceptance criteria supplied by parent: "+strings.Join(task.AcceptanceCriteria, "; "))
	}
	if err := packet.Validate(); err != nil {
		packet.Status = ChildMalformed
		packet.Summary = "child produced an incomplete completion packet"
		packet.Blockers = append(packet.Blockers, err.Error())
	}
	completionPath := filepath.Join(artifactDir, "completion.json")
	if relative, relErr := filepath.Rel(task.Scope.Root, completionPath); relErr == nil {
		packet.Artifacts = appendUnique(packet.Artifacts, filepath.ToSlash(relative))
	}
	definitions := []ToolDefinition(nil)
	if childExecutor != nil && childExecutor.Registry != nil {
		definitions = childExecutor.Registry.Definitions()
	}
	manifestPacket, manifestErr := finalizeDelegatedCompletion(task, packet, artifactDir, childConfig, definitions, requirePlan, CompletionManifestDelegated, "", "", CompletionProviderIdentity{})
	if manifestErr != nil {
		packet.Uncertainty = appendUnique(packet.Uncertainty, "write completion manifest: "+manifestErr.Error())
	} else {
		packet = manifestPacket
	}
	if err := writeChildPacket(artifactDir, packet); err != nil {
		packet.Uncertainty = append(packet.Uncertainty, err.Error())
	}
	return packet, nil
}

func buildChildPrompt(task ChildTask) string {
	var b strings.Builder
	b.WriteString(task.Prompt)
	b.WriteString("\n\nChild objective: ")
	b.WriteString(task.Objective)
	b.WriteString("\nAcceptance criteria:\n")
	for _, criterion := range task.AcceptanceCriteria {
		b.WriteString("- ")
		b.WriteString(criterion)
		b.WriteByte('\n')
	}
	b.WriteString("You have only the tools and execution scope supplied by the parent. Do not request or assume additional permissions, tools, paths, approvals, or agents.")
	return b.String()
}

func copyChildExecutor(base *Executor, task ChildTask) (*Executor, error) {
	if base == nil {
		if len(task.AllowedTools) > 0 {
			return nil, errors.New("child allowed tools require a parent executor")
		}
		return nil, nil
	}
	if err := base.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("parent executor scope: %w", err)
	}
	if base.Registry == nil {
		return nil, errors.New("parent executor registry is required")
	}
	if !scopeWithin(base.Scope.Root, task.Scope.Root) {
		return nil, errors.New("child scope must remain within parent executor scope")
	}
	if task.Scope.timeout() > base.Scope.timeout() {
		return nil, errors.New("child scope timeout must not exceed parent executor timeout")
	}
	if task.Scope.maxOutput() > base.Scope.maxOutput() {
		return nil, errors.New("child scope output limit must not exceed parent executor output limit")
	}
	if !allowlistSubset(task.Scope.EnvAllowlist, base.Scope.EnvAllowlist) {
		return nil, errors.New("child scope environment allowlist must remain within parent allowlist")
	}
	// Trusted foreground SSH is parent/foreground-only. A child task cannot
	// opt itself into unrestricted SSH, even when its filesystem root matches
	// the parent's root.
	task.Scope.TrustedForeground = false
	// A child cannot discard the parent's process write-root boundary. By
	// default its own delegated root is its only persistent write root; explicit
	// child write roots must remain within a parent write root.
	task.Scope.EnforceWriteRoots = base.Scope.EnforceWriteRoots
	if strings.TrimSpace(task.Scope.WriteRoot) == "" && len(task.Scope.WriteRoots) == 0 {
		task.Scope.WriteRoot = task.Scope.Root
	}
	parentWriteRoots, err := base.Scope.writeRootPaths()
	if err != nil {
		return nil, fmt.Errorf("parent write scope: %w", err)
	}
	childWriteRoots, err := task.Scope.writeRootPaths()
	if err != nil {
		return nil, fmt.Errorf("child write scope: %w", err)
	}
	for _, root := range childWriteRoots {
		if !pathWithinAny(parentWriteRoots, root) {
			return nil, errors.New("child write scope must remain within parent write roots")
		}
	}
	parentReadRoots, err := base.Scope.rootPaths()
	if err != nil {
		return nil, fmt.Errorf("parent read scope: %w", err)
	}
	childReadRoots, err := task.Scope.rootPaths()
	if err != nil {
		return nil, fmt.Errorf("child read scope: %w", err)
	}
	sameScope := sameResolvedPathSet(parentReadRoots, childReadRoots) && sameResolvedPathSet(parentWriteRoots, childWriteRoots)
	// Dynamic SSH/local grants are parent-owned capabilities. They are stripped
	// before rebinding tools so same-root children cannot retain a grant through
	// a copied tool value.
	task.Scope.RemoteGrant = nil
	registry, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	for _, name := range task.AllowedTools {
		if isParentOwnedJobTool(name) || isParentOwnedTodoTool(name) || isParentOwnedGitMutationTool(name) {
			return nil, fmt.Errorf("child tool %q is parent-owned and is not projected", name)
		}
		tool, ok := base.Registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("child tool %q is not registered in parent executor", name)
		}
		switch tool.(type) {
		case ExecTool, *ExecTool:
			return nil, fmt.Errorf("child tool %q is trusted foreground-only and is not projected", name)
		case MediaSearchTool, MediaRetrieveTool, ArtifactReadTool, *ArtifactReadTool:
			return nil, fmt.Errorf("child tool %q is parent-owned and is not projected", name)
		case MemorySearchTool, MemoryShowTool:
			return nil, fmt.Errorf("child tool %q requires an explicit parent memory projection", name)
		}
		rebound, err := rebindChildTool(tool, task.Scope, sameScope)
		if err != nil {
			return nil, fmt.Errorf("child tool %q: %w", name, err)
		}
		if err := registry.Register(rebound); err != nil {
			return nil, err
		}
	}
	copy := *base
	copy.Registry = registry
	copy.Scope = task.Scope
	// Dynamic SSH/local grants are parent-owned capabilities. A child never
	// inherits the parent's grant merely because its filesystem scope is
	// narrower; an explicit child grant would have to be created by the parent
	// boundary and carried in the child task contract.
	copy.Scope.RemoteGrant = nil
	// Plan state is child-local. Never share the parent's mutable plan gate
	// across concurrent children; each child must establish its own plan.
	if base.planGate != nil {
		copy.planGate = &PlanGate{}
	}
	// Child execution may reuse the parent's policy/audit hooks, but it never
	// receives a new approval path or a way to alter the task contract.
	return &copy, nil
}

func sameResolvedPathSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, path := range left {
		seen[filepath.Clean(path)] = struct{}{}
	}
	for _, path := range right {
		if _, ok := seen[filepath.Clean(path)]; !ok {
			return false
		}
	}
	return true
}

func rebindChildTool(tool Tool, scope ExecutionScope, sameRoot bool) (Tool, error) {
	// RunCommandTool carries its execution scope in the tool value. Always
	// rebind it so same-root children cannot retain the parent's trusted SSH
	// capability through a copied tool instance.
	if _, ok := tool.(RunCommandTool); ok {
		return RunCommandTool{Scope: scope}, nil
	}
	if sameRoot {
		return tool, nil
	}
	switch tool.(type) {
	case ReadFileTool:
		return ReadFileTool{Scope: scope}, nil
	case ListFilesTool:
		return ListFilesTool{Scope: scope}, nil
	case FindFilesTool:
		return FindFilesTool{Scope: scope}, nil
	case GrepTool:
		return GrepTool{Scope: scope}, nil
	case WriteFileTool:
		return WriteFileTool{Scope: scope}, nil
	case EditFileTool:
		return EditFileTool{Scope: scope}, nil
	default:
		return nil, errors.New("custom tool cannot be rebound to a narrower child scope")
	}
}

func scopeWithin(parent, child string) bool {
	parentAbs, parentErr := filepath.Abs(parent)
	childAbs, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	parentAbs = filepath.Clean(parentAbs)
	childAbs = filepath.Clean(childAbs)
	rel, err := filepath.Rel(parentAbs, childAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func allowlistSubset(child, parent []string) bool {
	allowed := make(map[string]struct{}, len(parent))
	for _, value := range parent {
		allowed[value] = struct{}{}
	}
	for _, value := range child {
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	return true
}

func childArtifactDir(task ChildTask) (string, error) {
	dir := task.ArtifactDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join("generated", "children", task.TaskID)
	}
	path, err := securePath(task.Scope, dir, true)
	if err != nil {
		return "", fmt.Errorf("child artifact directory: %w", err)
	}
	return path, nil
}

type fileStamp struct {
	Size    int64
	ModTime int64
	Hash    string
}

const maxSnapshotFiles = 4096
const maxSnapshotBytes = 8 << 20

func snapshotScope(root string) (map[string]fileStamp, []string) {
	stamps := make(map[string]fileStamp)
	var notes []string
	count, total := 0, int64(0)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			notes = append(notes, fmt.Sprintf("snapshot skipped %s: %v", path, walkErr))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if count >= maxSnapshotFiles || total >= maxSnapshotBytes {
			return fs.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			notes = append(notes, fmt.Sprintf("snapshot stat skipped %s: %v", path, err))
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		stamp := fileStamp{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		if info.Size() <= maxSnapshotBytes-total {
			if data, readErr := os.ReadFile(path); readErr == nil {
				hash := sha256.Sum256(data)
				stamp.Hash = hex.EncodeToString(hash[:])
				total += int64(len(data))
			}
		}
		stamps[filepath.ToSlash(rel)] = stamp
		count++
		return nil
	})
	if err != nil {
		notes = append(notes, fmt.Sprintf("snapshot incomplete: %v", err))
	}
	if count >= maxSnapshotFiles || total >= maxSnapshotBytes {
		notes = append(notes, "snapshot bounded before scanning every file")
	}
	return stamps, uniqueStrings(notes)
}

func diffScope(root string, before map[string]fileStamp, notes []string) ([]string, []string) {
	after, afterNotes := snapshotScope(root)
	changed := make([]string, 0)
	for path, stamp := range after {
		previous, ok := before[path]
		if !ok || previous != stamp {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed, append(notes, afterNotes...)
}

func listArtifacts(dir, root string) []string {
	var artifacts []string
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if filepath.Clean(filepath.Dir(path)) == filepath.Clean(dir) {
			if base := filepath.Base(path); base == "completion.json" || base == CompletionManifestFilename || base == IntegrationManifestFilename {
				return nil
			}
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			artifacts = append(artifacts, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(artifacts)
	return artifacts
}

func writeChildPacket(dir string, packet ChildCompletionPacket) error {
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode child completion packet: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "completion.json"), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write child completion packet: %w", err)
	}
	return nil
}

func truncateText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isTestCommand(command string) bool {
	base := filepath.Base(command)
	return base == "go" || base == "gotestsum" || base == "pytest" || base == "npm" || base == "cargo"
}
