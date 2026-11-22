package bee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// RiskLevel classifies the side-effect potential of a tool.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// ToolDefinition is the provider-neutral, serializable description of a tool.
type ToolMutability string

const (
	ToolMutabilityUnknown  ToolMutability = "unknown"
	ToolMutabilityReadOnly ToolMutability = "read_only"
	ToolMutabilityMutating ToolMutability = "mutating"
)

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Risk        RiskLevel       `json:"risk,omitempty"`
	Mutability  ToolMutability  `json:"mutability,omitempty"`
	// PlanExempt is a narrow, explicit contract. Parent-only context_status and
	// bounded todo metadata are the only generic
	// plan exemptions. Mutation-capable process tools deliberately go through
	// the same plan gate as file writes.
	PlanExempt bool `json:"plan_exempt,omitempty"`
}

func (definition ToolDefinition) Validate() error {
	if strings.TrimSpace(definition.Name) == "" {
		return errors.New("tool name is required")
	}
	if strings.ContainsAny(definition.Name, " \t\r\n") {
		return fmt.Errorf("tool name %q contains whitespace", definition.Name)
	}
	if definition.PlanExempt && !allowedPlanExemptToolName(definition.Name) {
		return fmt.Errorf("tool %q cannot be plan-exempt", definition.Name)
	}
	if definition.Parameters != nil {
		var value any
		if err := json.Unmarshal(definition.Parameters, &value); err != nil {
			return fmt.Errorf("tool parameters must be valid JSON: %w", err)
		}
		if _, ok := value.(map[string]any); !ok {
			return errors.New("tool parameters must be a JSON object")
		}
	}
	return nil
}

func allowedPlanExemptToolName(name string) bool {
	switch name {
	case ContextStatusToolName, "todo_create", "todo_update", "todo_complete", "todo_archive":
		return true
	default:
		return false
	}
}

// Tool is a small provider-neutral executable capability.
type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, json.RawMessage) (string, error)
}

// UnboundedOutputTool opts out of the executor's generic output ceiling. This
// is reserved for tools whose contract requires returning a complete external
// response rather than a bounded command/tool summary.
type UnboundedOutputTool interface {
	Tool
	AllowsUnboundedOutput() bool
}

// MaxSelfBoundedOutputBytes is the hard ceiling for tools that own a finite,
// structured output contract. It is deliberately separate from the ordinary
// executor scope ceiling and from UnboundedOutputTool: self-bounded tools may
// exceed a caller's generic summary limit, but never this global cap.
const MaxSelfBoundedOutputBytes = 1 << 20

type SelfBoundedOutputTool interface {
	Tool
	SelfBoundedOutputLimit() int
}

// ToolExecutionTimeout optionally lets a bounded tool request a longer
// invocation context than the ordinary scope default. The executor still
// caps the result at maxToolExecutionTimeout and always preserves parent
// context cancellation.
type ToolExecutionTimeout interface {
	ExecutionTimeout() time.Duration
}

// ExplicitApprovalTool marks a capability that must pass a human approval
// hook even when the surrounding policy would otherwise allow it. Policy may
// still deny the call; this marker can only narrow authority.
type ExplicitApprovalTool interface {
	RequiresExplicitApproval() bool
}

// ManualApprovalTool marks the small set of capabilities whose human review
// must not be automated. It only has meaning after policy or
// ExplicitApprovalTool has required approval; like the explicit marker, it can
// narrow authority but never grant it.
type ManualApprovalTool interface {
	RequiresManualApproval() bool
}

// ToolFunc adapts a function into a Tool.
type ToolFunc struct {
	ToolDefinition
	Fn func(context.Context, json.RawMessage) (string, error)
}

func (tool ToolFunc) Definition() ToolDefinition { return tool.ToolDefinition }
func (tool ToolFunc) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Fn == nil {
		return "", errors.New("tool function is nil")
	}
	return tool.Fn(ctx, args)
}

// Registry owns stable tool names and implementations.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry(tools ...Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]Tool)}
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (registry *Registry) Register(tool Tool) error {
	if registry == nil {
		return errors.New("tool registry is nil")
	}
	if tool == nil {
		return errors.New("tool is required")
	}
	definition := tool.Definition()
	if err := definition.Validate(); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.tools == nil {
		registry.tools = make(map[string]Tool)
	}
	if _, exists := registry.tools[definition.Name]; exists {
		return fmt.Errorf("tool %q is already registered", definition.Name)
	}
	registry.tools[definition.Name] = tool
	return nil
}

func (registry *Registry) Lookup(name string) (Tool, bool) {
	if registry == nil {
		return nil, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	tool, ok := registry.tools[name]
	return tool, ok
}

func (registry *Registry) Unregister(name string) error {
	if registry == nil {
		return errors.New("tool registry is nil")
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("tool name is required")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.tools[name]; !ok {
		return fmt.Errorf("tool %q is not registered", name)
	}
	delete(registry.tools, name)
	return nil
}

func (registry *Registry) Definitions() []ToolDefinition {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	definitions := make([]ToolDefinition, 0, len(registry.tools))
	for _, tool := range registry.tools {
		definitions = append(definitions, tool.Definition())
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

// Clone returns an independent registry with the same immutable tool values.
// Agent composition uses this before merging optional registries so one turn
// cannot mutate the shared application registry and make the next turn see
// duplicate tools.
func (registry *Registry) Clone() (*Registry, error) {
	if registry == nil {
		return nil, errors.New("tool registry is nil")
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	clone := &Registry{tools: make(map[string]Tool, len(registry.tools))}
	for name, tool := range registry.tools {
		if tool == nil {
			return nil, fmt.Errorf("tool registry contains nil tool %q", name)
		}
		clone.tools[name] = tool
	}
	return clone, nil
}

// ExecutionScope separates readable roots from persistent write roots. The
// foreground exec/terminal surface may read the host but uses the write-root
// sandbox unless the distinct approval-only host_exec capability is selected.
type ExecutionScope struct {
	Root  string   `json:"root"`
	Roots []string `json:"roots,omitempty"`
	// WriteRoot is the preferred persistent-write root and the base for relative
	// paths. WriteRoots adds other locations that may be mutated without a
	// separate approval. Reads remain governed by Root and Roots.
	WriteRoot  string   `json:"write_root,omitempty"`
	WriteRoots []string `json:"write_roots,omitempty"`
	// EnforceWriteRoots asks process tools to expose the readable host through a
	// read-only sandbox while rebinding only the configured write roots writable.
	// Direct file tools enforce write roots whenever they are configured.
	EnforceWriteRoots bool     `json:"enforce_write_roots,omitempty"`
	TimeoutSeconds    int      `json:"timeout_seconds,omitempty"`
	MaxOutputBytes    int      `json:"max_output_bytes,omitempty"`
	EnvAllowlist      []string `json:"env_allowlist,omitempty"`
	// TrustedForeground permits the ordinary assistant's explicit exec/terminal
	// host process surface. It is intentionally not inferred from a filesystem
	// scope and is stripped from child executors.
	TrustedForeground bool                   `json:"trusted_foreground,omitempty"`
	RemoteGrant       *RemoteCapabilityGrant `json:"remote_grant,omitempty"`
	// SandboxTmpRoot holds one private host directory per session that the
	// write sandbox mounts as /tmp, so files a command leaves in /tmp are
	// still there for the next command in the same session. Empty keeps the
	// per-invocation tmpfs. SandboxTmp is the resolved directory for one call.
	// Neither is serialized or inherited by child scopes.
	SandboxTmpRoot string `json:"-"`
	SandboxTmp     string `json:"-"`
	// LaunchRoot is the interactive launch directory when it was made a write
	// root; process tools export it as BEE_LAUNCH_DIR. Not inherited by
	// child scopes.
	LaunchRoot string `json:"-"`
	// NetworkOff removes the network namespace from sandboxed processes for
	// a session whose owner ran /network off (G11). Not serialized.
	NetworkOff bool `json:"-"`
}

// MaxSandboxTmpAge bounds how long an idle session /tmp directory is kept.
const MaxSandboxTmpAge = 7 * 24 * time.Hour

// withSessionTmp resolves this call's persistent sandbox /tmp: one directory
// per session (or per process when no session is in context), created 0700.
// Any failure falls back to the per-invocation tmpfs rather than failing the
// command.
func (scope ExecutionScope) withSessionTmp(ctx context.Context) ExecutionScope {
	scope.SandboxTmp = ""
	root := strings.TrimSpace(scope.SandboxTmpRoot)
	if root == "" || !filepath.IsAbs(root) {
		return scope
	}
	key := fmt.Sprintf("process-%d", os.Getpid())
	if ctx != nil {
		if id, ok := SessionIDFromContext(ctx); ok && id != "" && validateSessionID(id) == nil {
			key = "session-" + string(id)
		}
	}
	dir := filepath.Join(filepath.Clean(root), key)
	if err := ensurePrivateDir(filepath.Clean(root), 0o700); err != nil {
		return scope
	}
	if err := ensurePrivateDir(dir, 0o700); err != nil {
		return scope
	}
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	scope.SandboxTmp = dir
	return scope
}

// PruneSandboxTmp removes session /tmp directories idle longer than maxAge.
// Best effort and bounded to direct children; it never follows symlinks.
func PruneSandboxTmp(root string, maxAge time.Duration, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for index, entry := range entries {
		if index >= 4096 {
			return
		}
		name := entry.Name()
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !(strings.HasPrefix(name, "session-") || strings.HasPrefix(name, "process-")) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}

func (scope ExecutionScope) Validate() error {
	roots, err := scope.rootPaths()
	if err != nil {
		return err
	}
	for _, root := range roots {
		info, statErr := os.Stat(root)
		if statErr != nil {
			return fmt.Errorf("stat execution scope root: %w", statErr)
		}
		if !info.IsDir() {
			return errors.New("execution scope root must be a directory")
		}
	}
	writeRoots, err := scope.writeRootPaths()
	if err != nil {
		return err
	}
	for _, root := range writeRoots {
		info, statErr := os.Stat(root)
		if statErr != nil {
			return fmt.Errorf("stat execution write root: %w", statErr)
		}
		if !info.IsDir() {
			return errors.New("execution write root must be a directory")
		}
		if !pathWithinAny(roots, root) {
			return fmt.Errorf("execution write root %q is outside all read roots", root)
		}
	}
	if scope.TimeoutSeconds < 0 {
		return errors.New("execution scope timeout_seconds must not be negative")
	}
	if scope.MaxOutputBytes < 0 {
		return errors.New("execution scope max_output_bytes must not be negative")
	}
	return nil
}

func (scope ExecutionScope) ValidateShape() error {
	if strings.TrimSpace(scope.Root) == "" {
		return errors.New("execution scope root is required")
	}
	for index, root := range scope.Roots {
		if strings.TrimSpace(root) == "" || strings.ContainsRune(root, '\x00') {
			return fmt.Errorf("execution scope roots[%d] is invalid", index)
		}
	}
	if scope.WriteRoot != "" && strings.TrimSpace(scope.WriteRoot) == "" {
		return errors.New("execution write root is invalid")
	}
	if strings.ContainsRune(scope.WriteRoot, '\x00') {
		return errors.New("execution write root is invalid")
	}
	for index, root := range scope.WriteRoots {
		if strings.TrimSpace(root) == "" || strings.ContainsRune(root, '\x00') {
			return fmt.Errorf("execution write roots[%d] is invalid", index)
		}
	}
	if scope.TimeoutSeconds < 0 {
		return errors.New("execution scope timeout_seconds must not be negative")
	}
	if scope.MaxOutputBytes < 0 {
		return errors.New("execution scope max_output_bytes must not be negative")
	}
	return nil
}

func (scope ExecutionScope) rootPath() (string, error) {
	roots, err := scope.rootPaths()
	if err != nil {
		return "", err
	}
	return roots[0], nil
}

func (scope ExecutionScope) rootPaths() ([]string, error) {
	if err := scope.ValidateShape(); err != nil {
		return nil, err
	}
	inputs := append([]string{scope.Root}, scope.Roots...)
	result := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		root, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("resolve execution scope root: %w", err)
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve execution scope root symlinks: %w", err)
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; !ok {
			seen[root] = struct{}{}
			result = append(result, root)
		}
	}
	return result, nil
}

func (scope ExecutionScope) writeRootPaths() ([]string, error) {
	if err := scope.ValidateShape(); err != nil {
		return nil, err
	}
	inputs := append([]string(nil), scope.WriteRoots...)
	if strings.TrimSpace(scope.WriteRoot) != "" {
		inputs = append([]string{scope.WriteRoot}, inputs...)
	}
	if len(inputs) == 0 {
		inputs = []string{scope.Root}
	}
	result := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		root, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("resolve execution write root: %w", err)
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve execution write root symlinks: %w", err)
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; !ok {
			seen[root] = struct{}{}
			result = append(result, root)
		}
	}
	return result, nil
}

func (scope ExecutionScope) preferredRootPath() (string, error) {
	roots, err := scope.writeRootPaths()
	if err != nil {
		return "", err
	}
	return roots[0], nil
}

func pathWithinAny(roots []string, path string) bool {
	for _, root := range roots {
		if filepath.Clean(root) == filepath.Clean(path) || scopeWithin(root, path) {
			return true
		}
	}
	return false
}

func (scope ExecutionScope) timeout() time.Duration {
	if scope.TimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	if scope.TimeoutSeconds > int(maxToolExecutionTimeout/time.Second) {
		return maxToolExecutionTimeout
	}
	return time.Duration(scope.TimeoutSeconds) * time.Second
}

const maxToolExecutionTimeout = 4 * time.Hour

func effectiveToolTimeout(tool Tool, scope ExecutionScope) time.Duration {
	timeout := scope.timeout()
	if provider, ok := tool.(ToolExecutionTimeout); ok {
		if requested := provider.ExecutionTimeout(); requested > timeout {
			timeout = requested
		}
	}
	if timeout > maxToolExecutionTimeout {
		return maxToolExecutionTimeout
	}
	return timeout
}

func (scope ExecutionScope) maxOutput() int {
	if scope.MaxOutputBytes <= 0 {
		return 64 << 10
	}
	return scope.MaxOutputBytes
}

// PolicyDecision is the result of a pre-execution policy check.
type PolicyDecision string

const (
	PolicyAllow           PolicyDecision = "allow"
	PolicyDeny            PolicyDecision = "deny"
	PolicyRequireApproval PolicyDecision = "require_approval"
)

// Policy evaluates a call before its implementation can cause side effects.
type Policy interface {
	Evaluate(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error)
}

// PolicyFunc adapts a function into a Policy.
type PolicyFunc func(context.Context, ToolDefinition, json.RawMessage, ExecutionScope) (PolicyDecision, error)

func (policy PolicyFunc) Evaluate(ctx context.Context, definition ToolDefinition, args json.RawMessage, scope ExecutionScope) (PolicyDecision, error) {
	return policy(ctx, definition, args, scope)
}

// ApprovalRequest is a human-readable request for a policy-required decision.
type ApprovalRequest struct {
	ID       string          `json:"id,omitempty"`
	ToolName string          `json:"tool_name"`
	Summary  string          `json:"summary"`
	Args     json.RawMessage `json:"args,omitempty"`
	Expires  time.Time       `json:"expires,omitempty"`
	// SessionGrant, when set, is the directory an "approved_session" answer
	// makes writable for the rest of the session. Only out-of-bounds writes
	// by the file and git tools offer one.
	SessionGrant string `json:"session_grant,omitempty"`
}

type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalDenied   ApprovalDecision = "denied"
	// ApprovalApprovedForSession approves this call and, when the request
	// offered a SessionGrant directory, makes that directory writable for the
	// rest of the session. Without an offered grant it is a one-time approval.
	ApprovalApprovedForSession ApprovalDecision = "approved_session"
)

// Approves reports whether the decision lets the tool call run.
func (decision ApprovalDecision) Approves() bool {
	return decision == ApprovalApproved || decision == ApprovalApprovedForSession
}

// ValidOwnerDecision reports whether an owner-facing client may send it.
func (decision ApprovalDecision) ValidOwnerDecision() bool {
	return decision == ApprovalApproved || decision == ApprovalDenied || decision == ApprovalApprovedForSession
}

type ApprovalHook interface {
	Approve(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

type ApprovalFunc func(context.Context, ApprovalRequest) (ApprovalDecision, error)

func (approval ApprovalFunc) Approve(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	return approval(ctx, request)
}

type explicitToolApprovalContextKey struct{}

func withExplicitToolApproval(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitToolApprovalContextKey{}, true)
}

func hasExplicitToolApproval(ctx context.Context) bool {
	approved, _ := ctx.Value(explicitToolApprovalContextKey{}).(bool)
	return approved
}

// AuthorizationError identifies the layer that rejected a tool call and gives
// interactive clients a concrete recovery action.
type AuthorizationError struct {
	Tool     string `json:"tool"`
	Layer    string `json:"layer"`
	Recovery string `json:"recovery,omitempty"`
	Cause    error  `json:"-"`
}

func (err *AuthorizationError) Error() string {
	message := fmt.Sprintf("tool %q authorization denied at %s", err.Tool, err.Layer)
	if err.Cause != nil {
		message += ": " + err.Cause.Error()
	}
	if err.Recovery != "" {
		message += "; recovery: " + err.Recovery
	}
	return message
}

func (err *AuthorizationError) Unwrap() error { return err.Cause }

// AuditOutcome classifies the result of a tool authorization/execution
// attempt. Denied outcomes are durable when the executor is attached to a
// session; executed outcomes remain available to the caller's Auditor.
type AuditOutcome string

const (
	AuditExecuted       AuditOutcome = "executed"
	AuditPlanDenied     AuditOutcome = "plan_denied"
	AuditPolicyDenied   AuditOutcome = "policy_denied"
	AuditPolicyError    AuditOutcome = "policy_error"
	AuditApprovalDenied AuditOutcome = "approval_denied"
	AuditApprovalError  AuditOutcome = "approval_error"
)

// AuditEvent records the policy and execution outcome without requiring a
// particular logging or persistence system. Args are bounded and sanitized
// before they reach an Auditor.
type AuditEvent struct {
	Time              time.Time        `json:"time"`
	ToolName          string           `json:"tool_name"`
	EffectiveToolName string           `json:"effective_tool_name,omitempty"`
	Args              json.RawMessage  `json:"args,omitempty"`
	Target            string           `json:"target,omitempty"`
	Decision          PolicyDecision   `json:"decision"`
	Approval          ApprovalDecision `json:"approval,omitempty"`
	AutoApproved      bool             `json:"auto_approved,omitempty"`
	Outcome           AuditOutcome     `json:"outcome"`
	Success           bool             `json:"success"`
	Error             string           `json:"error,omitempty"`
}

type Auditor interface {
	Record(context.Context, AuditEvent) error
}

type AuditorFunc func(context.Context, AuditEvent) error

func (auditor AuditorFunc) Record(ctx context.Context, event AuditEvent) error {
	return auditor(ctx, event)
}

// ToolResult is a structured result suitable for appending as a RoleTool
// message. Error is kept in-band so a model can recover from a failed call.
type ToolResult struct {
	ToolName                 string `json:"tool_name"`
	EffectiveToolName        string `json:"effective_tool_name,omitempty"`
	CallID                   string `json:"call_id,omitempty"`
	Output                   string `json:"output,omitempty"`
	Error                    string `json:"error,omitempty"`
	Truncated                bool   `json:"truncated,omitempty"`
	HookObservation          string `json:"hook_observation,omitempty"`
	HookObservationTruncated bool   `json:"hook_observation_truncated,omitempty"`
}

func (result ToolResult) Content() string {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf(`{"tool_name":%q,"error":%q}`, result.ToolName, err.Error())
	}
	return string(data)
}

// Executor centralizes lookup, validation, policy, approval, bounded context,
// invocation, output limits, and audit.
type Executor struct {
	Registry *Registry
	Scope    ExecutionScope
	Policy   Policy
	// ApprovalMode is orthogonal to Policy and Scope. The zero value is the
	// fail-closed manual mode.
	ApprovalMode ApprovalMode
	Approval     ApprovalHook
	Auditor      Auditor
	EventHook    func(ExecutorEvent)
	Hooks        *ToolLifecycleHooks
	// Unattended marks a run with nobody at the wheel (scheduled jobs).
	// With no approval hook, a gated call then fails with an explanation
	// aimed at the job's prompt and contract, not at an interactive client.
	Unattended bool
	planGate   *PlanGate
}

// ExecutorEvent exposes approval decisions without coupling interfaces to the
// concrete executor implementation.
type ExecutorEvent struct {
	Type     string
	Request  ApprovalRequest
	Decision ApprovalDecision
	Status   ApprovalStatus
	Error    string
}

// NewExecutor constructs the policy-neutral executor. Callers can set Policy,
// Approval, and Auditor on the returned value as needed.
func NewExecutor(registry *Registry, scope ExecutionScope) *Executor {
	return &Executor{Registry: registry, Scope: scope}
}

func (executor *Executor) SetPlan(plan TaskPlan) error {
	if executor == nil {
		return errors.New("executor is nil")
	}
	if executor.planGate == nil {
		executor.planGate = &PlanGate{}
	}
	return executor.planGate.SetPlan(plan)
}

func (executor *Executor) Plan() *TaskPlan {
	if executor == nil || executor.planGate == nil {
		return nil
	}
	return executor.planGate.Plan()
}

// NewCodingExecutor applies the safe default coding policy. Mutating and
// process-running tools still require the supplied approval hook.
func NewCodingExecutor(scope ExecutionScope, approval ApprovalHook, auditor Auditor) (*Executor, error) {
	registry, err := NewCodingRegistry(scope)
	if err != nil {
		return nil, err
	}
	return &Executor{Registry: registry, Scope: scope, Policy: DefaultCodingPolicy{}, Approval: approval, Auditor: auditor, planGate: &PlanGate{}}, nil
}

const maxAuditArgsBytes = 2048

const maxAuditTargetBytes = 512

var autoApprovalTargetKeys = []string{
	"path", "repo_path", "workdir", "destination", "target", "recipient",
	"channel", "chat_id", "to", "host", "url", "endpoint", "session_id", "id",
}

func (executor Executor) recordAudit(ctx context.Context, event AuditEvent) error {
	if executor.Auditor == nil {
		return nil
	}
	return executor.Auditor.Record(ctx, event)
}

func boundedAuditArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(args, &value); err == nil {
		redactAuditValue(value)
		if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
			args = encoded
		}
	}
	if len(args) <= maxAuditArgsBytes {
		return append(json.RawMessage(nil), args...)
	}
	return json.RawMessage(fmt.Sprintf(`{"_redacted":"arguments exceeded %d bytes"}`, maxAuditArgsBytes))
}

func boundedToolAuditArgs(name string, args json.RawMessage) json.RawMessage {
	// MCP arguments are server I/O and may use arbitrary provider-defined key
	// names, so the ordinary content-key redactor cannot safely classify them.
	// Keep the authorization outcome while omitting the payload entirely.
	if strings.HasPrefix(name, "mcp_") {
		return json.RawMessage(`{"_redacted":"MCP tool arguments"}`)
	}
	return boundedAuditArgs(args)
}

// boundedAutoApprovalTarget projects only an allowlisted destination field.
// It deliberately does not serialize arbitrary arguments: the durable audit
// is a paper trail, not a second copy of model/user content.
func boundedAutoApprovalTarget(name string, args json.RawMessage, scope ExecutionScope) string {
	var values map[string]any
	if json.Unmarshal(args, &values) == nil {
		for _, key := range autoApprovalTargetKeys {
			value, ok := values[key]
			if !ok {
				continue
			}
			var text string
			switch typed := value.(type) {
			case string:
				text = typed
			case float64, bool:
				text = fmt.Sprint(typed)
			default:
				continue
			}
			if target := sanitizeAuditTarget(key, text); target != "" {
				return target
			}
		}
	}
	if name == HostExecToolName {
		return "host"
	}
	if name == ExecToolName || name == TerminalToolName {
		if root, err := scope.preferredRootPath(); err == nil {
			return sanitizeAuditTarget("workdir", root)
		}
	}
	return sanitizeAuditTarget("capability", name)
}

func sanitizeAuditTarget(key, value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "?"))
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		value = parsed.String()
	}
	candidate := key + "=" + value
	var bounded strings.Builder
	for _, char := range candidate {
		if unicode.IsControl(char) {
			char = ' '
		}
		encoded := string(char)
		if bounded.Len()+len(encoded) > maxAuditTargetBytes {
			break
		}
		bounded.WriteString(encoded)
	}
	return strings.TrimSpace(bounded.String())
}

func redactAuditValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
			if normalized == "content" || normalized == "old" || normalized == "new" || normalized == "stdin" || normalized == "env" || isSecretKey(key) {
				if text, ok := child.(string); ok {
					typed[key] = fmt.Sprintf("[REDACTED %d bytes]", len(text))
				} else {
					typed[key] = "[REDACTED]"
				}
				continue
			}
			redactAuditValue(child)
		}
	case []any:
		for index, child := range typed {
			redactAuditValue(child)
			if text, ok := child.(string); ok && len(text) > maxAuditArgsBytes/2 {
				typed[index] = fmt.Sprintf("[REDACTED %d bytes]", len(text))
			}
		}
	}
}

func (executor Executor) deniedAudit(ctx context.Context, name, effectiveName string, args json.RawMessage, decision PolicyDecision, approval ApprovalDecision, outcome AuditOutcome, reason error) error {
	event := AuditEvent{Time: time.Now().UTC(), ToolName: name, Args: boundedToolAuditArgs(effectiveName, args), Decision: decision, Approval: approval, Outcome: outcome, Success: false}
	if effectiveName != "" && effectiveName != name {
		event.EffectiveToolName = effectiveName
	}
	if reason != nil {
		event.Error = reason.Error()
	}
	return executor.recordAudit(ctx, event)
}

func (executor Executor) Execute(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	result := ToolResult{ToolName: name}
	if executor.Registry == nil {
		return result, errors.New("tool registry is required")
	}
	tool, ok := executor.Registry.Lookup(name)
	if !ok {
		return result, fmt.Errorf("tool %q is not registered", name)
	}
	if err := executor.Scope.Validate(); err != nil {
		return result, err
	}
	// Directories the user approved for this session are write roots for
	// policy evaluation too, so they stop asking.
	executor.Scope = executor.Scope.withSessionGrants(ctx)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var input any
	if err := json.Unmarshal(args, &input); err != nil {
		return result, fmt.Errorf("tool %q arguments must be valid JSON: %w", name, err)
	}
	if _, ok := input.(map[string]any); !ok {
		return result, fmt.Errorf("tool %q arguments must be a JSON object", name)
	}
	effectiveName := name
	definition := tool.Definition()
	if executor.planGate != nil {
		planErr := executor.planGate.Check(definition)
		if planErr != nil {
			if fallback, ok := tool.(PlanFallbackTool); ok {
				if fallbackName, eligible := fallback.PlanFallback(args); eligible {
					if fallbackTool, exists := executor.Registry.Lookup(fallbackName); exists {
						fallbackDefinition := fallbackTool.Definition()
						if fallbackErr := executor.planGate.Check(fallbackDefinition); fallbackErr == nil {
							tool = fallbackTool
							definition = fallbackDefinition
							effectiveName = fallbackName
							result.EffectiveToolName = fallbackName
							planErr = nil
						}
					}
				}
			}
		}
		if planErr != nil {
			err := &AuthorizationError{Tool: name, Layer: "plan", Cause: planErr, Recovery: "use a read-only tool for inspection; otherwise submit or repair the active bee_plan, use /plan off, or disable assistant.require_plan"}
			if executor.EventHook != nil {
				executor.EventHook(ExecutorEvent{Type: "plan_gate", Decision: ApprovalDenied, Error: err.Error()})
			}
			if auditErr := executor.deniedAudit(ctx, name, name, args, PolicyDeny, ApprovalDenied, AuditPlanDenied, err); auditErr != nil {
				return result, errors.Join(err, fmt.Errorf("record plan denial audit: %w", auditErr))
			}
			return result, err
		}
	}
	if NetworkOffFromContext(ctx) && toolNeedsNetwork(tool, effectiveName, args) {
		networkErr := &AuthorizationError{Tool: name, Layer: "network", Cause: ErrNetworkOff, Recovery: "work without the network, or ask the owner to run /network on"}
		if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, PolicyDeny, ApprovalDenied, AuditPolicyDenied, networkErr); auditErr != nil {
			return result, errors.Join(networkErr, fmt.Errorf("record network denial audit: %w", auditErr))
		}
		return result, networkErr
	}
	executor.Scope.NetworkOff = NetworkOffFromContext(ctx)
	decision := PolicyAllow
	if executor.Policy != nil {
		var err error
		decision, err = executor.Policy.Evaluate(ctx, definition, args, executor.Scope)
		if err != nil {
			policyErr := fmt.Errorf("tool %q policy: %w", name, err)
			if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, decision, ApprovalDenied, AuditPolicyError, policyErr); auditErr != nil {
				return result, errors.Join(policyErr, fmt.Errorf("record policy audit: %w", auditErr))
			}
			return result, policyErr
		}
	}
	if decision == "" {
		decision = PolicyDeny
	}
	if decision == PolicyAllow {
		if guarded, ok := tool.(ExplicitApprovalTool); ok && guarded.RequiresExplicitApproval() {
			decision = PolicyRequireApproval
		}
	}
	if decision != PolicyAllow && decision != PolicyDeny && decision != PolicyRequireApproval {
		return result, fmt.Errorf("tool %q policy returned unknown decision %q", name, decision)
	}
	if decision == PolicyDeny {
		deniedErr := &AuthorizationError{Tool: name, Layer: "policy", Cause: errors.New("tool denied by policy"), Recovery: "inspect assistant.policy and the tool-specific capability settings"}
		if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, decision, ApprovalDenied, AuditPolicyDenied, deniedErr); auditErr != nil {
			return result, errors.Join(deniedErr, fmt.Errorf("record policy denial audit: %w", auditErr))
		}
		return result, deniedErr
	}
	approved := false
	autoApproved := false
	if decision == PolicyRequireApproval {
		request := ApprovalRequest{ID: "approval-" + newStableID(), ToolName: effectiveName, Summary: definition.Description, Args: append(json.RawMessage(nil), args...), Expires: time.Now().Add(executor.Scope.timeout())}
		manualOnly := false
		if guarded, ok := tool.(ManualApprovalTool); ok {
			manualOnly = guarded.RequiresManualApproval()
		}
		explicitOnly := false
		if guarded, ok := tool.(ExplicitApprovalTool); ok && guarded.RequiresExplicitApproval() {
			explicitOnly = true
		}
		if !manualOnly && !explicitOnly {
			request.SessionGrant = executor.offeredSessionGrant(ctx, effectiveName, args)
		}
		if executor.ApprovalMode.effective() == ApprovalModeAuto && !manualOnly {
			approved = true
			autoApproved = true
		} else if executor.Approval == nil {
			approvalErr := executor.noApprovalHookError(name, effectiveName, args)
			if executor.EventHook != nil {
				executor.EventHook(ExecutorEvent{Type: "approval", Request: request, Decision: ApprovalDenied, Status: ApprovalStatusError, Error: approvalErr.Error()})
			}
			if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, decision, ApprovalDenied, AuditApprovalError, approvalErr); auditErr != nil {
				return result, errors.Join(approvalErr, fmt.Errorf("record approval audit: %w", auditErr))
			}
			return result, approvalErr
		} else {
			approval, err := executor.Approval.Approve(ctx, request)
			if err != nil {
				approvalErr := fmt.Errorf("tool %q approval: %w", name, err)
				if executor.EventHook != nil {
					executor.EventHook(ExecutorEvent{Type: "approval", Request: request, Decision: ApprovalDenied, Status: ApprovalStatusError, Error: approvalErr.Error()})
				}
				if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, decision, ApprovalDenied, AuditApprovalError, approvalErr); auditErr != nil {
					return result, errors.Join(approvalErr, fmt.Errorf("record approval audit: %w", auditErr))
				}
				return result, approvalErr
			}
			if executor.EventHook != nil {
				executor.EventHook(ExecutorEvent{Type: "approval", Request: request, Decision: approval, Status: approvalStatus(approval, "")})
			}
			if !approval.Approves() {
				deniedErr := &AuthorizationError{Tool: name, Layer: "approval", Cause: errors.New("approval was not granted"), Recovery: "approve the pending request and retry"}
				if auditErr := executor.deniedAudit(ctx, name, effectiveName, args, decision, approval, AuditApprovalDenied, deniedErr); auditErr != nil {
					return result, errors.Join(deniedErr, fmt.Errorf("record approval denial audit: %w", auditErr))
				}
				return result, deniedErr
			}
			if approval == ApprovalApprovedForSession && request.SessionGrant != "" {
				if store, id := sessionWriteGrantsFor(ctx); store != nil {
					store.Add(id, request.SessionGrant)
				}
			}
			approved = true
		}
	}

	if approved {
		ctx = withExplicitToolApproval(ctx)
	}
	preHookObservations := executor.Hooks.pre(ctx, effectiveName, args)
	toolCtx, cancel := context.WithTimeout(ctx, effectiveToolTimeout(tool, executor.Scope))
	defer cancel()
	output, err := tool.Execute(toolCtx, args)
	truncated := false
	if bounded, ok := tool.(SelfBoundedOutputTool); ok {
		limit := bounded.SelfBoundedOutputLimit()
		if limit <= 0 || limit > MaxSelfBoundedOutputBytes {
			output = ""
			boundErr := fmt.Errorf("tool %q declared invalid self-bounded output limit %d", name, limit)
			if err != nil {
				err = errors.Join(err, boundErr)
			} else {
				err = boundErr
			}
		} else if len(output) > limit {
			output = ""
			boundErr := fmt.Errorf("tool %q exceeded self-bounded output limit %d", name, limit)
			if err != nil {
				err = errors.Join(err, boundErr)
			} else {
				err = boundErr
			}
		}
	} else if unbounded, ok := tool.(UnboundedOutputTool); !ok || !unbounded.AllowsUnboundedOutput() {
		output, truncated = truncateOutput(output, executor.Scope.maxOutput())
	}
	postHookObservations := executor.Hooks.post(ctx, effectiveName, args, toolHookOutcome(err))
	result.Output = output
	result.Truncated = truncated
	result.Error = errorString(err)
	result.HookObservation, result.HookObservationTruncated = boundedHookObservationContext(preHookObservations, postHookObservations)
	event := AuditEvent{Time: time.Now().UTC(), ToolName: name, Args: boundedToolAuditArgs(effectiveName, args), Decision: decision, Outcome: AuditExecuted, Success: err == nil}
	if autoApproved {
		event.AutoApproved = true
		event.Approval = ApprovalApproved
		event.Target = boundedAutoApprovalTarget(effectiveName, args, executor.Scope)
	}
	if effectiveName != name {
		event.EffectiveToolName = effectiveName
	}
	if err != nil {
		event.Error = err.Error()
	}
	if executor.Auditor != nil {
		if auditErr := executor.Auditor.Record(ctx, event); auditErr != nil {
			return result, fmt.Errorf("tool %q audit: %w", name, auditErr)
		}
	}
	return result, err
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncateOutput(output string, limit int) (string, bool) {
	if limit <= 0 || len(output) <= limit {
		return output, false
	}
	marker := fmt.Sprintf("\n...[output truncated after %d bytes]", limit)
	if len(marker) >= limit {
		return output[:limit], true
	}
	return output[:limit-len(marker)] + marker, true
}

// DefaultCodingPolicy allows read-only tools and requires approval for tools
// that can mutate files or execute processes.
type DefaultCodingPolicy struct{}

func (DefaultCodingPolicy) Evaluate(_ context.Context, definition ToolDefinition, args json.RawMessage, scope ExecutionScope) (PolicyDecision, error) {
	if definition.Name == "run_command" {
		var input runArgs
		if err := parseArgs(args, &input); err == nil {
			target := input.Target
			if target == CommandTargetLocal || target == CommandTargetSSH {
				if target == CommandTargetSSH && scope.TrustedForeground && scope.RemoteGrant == nil {
					return PolicyAllow, nil
				}
				if scope.RemoteGrant != nil && scope.RemoteGrant.permits(commandRequest{Target: target, Host: input.Host, User: input.User, Port: input.Port, Elevated: input.Elevated, Stdin: input.Stdin}, time.Now().UTC()) == nil {
					return PolicyAllow, nil
				}
				return PolicyDeny, nil
			}
		}
	}
	if definition.Risk == RiskHigh || definition.Risk == RiskCritical {
		return PolicyRequireApproval, nil
	}
	return PolicyAllow, nil
}

func securePath(scope ExecutionScope, path string, allowMissing bool) (string, error) {
	candidate, _, err := resolveSecurePath(scope, path, allowMissing)
	return candidate, err
}

func resolveSecurePath(scope ExecutionScope, path string, allowMissing bool) (string, string, error) {
	roots, err := scope.rootPaths()
	if err != nil {
		return "", "", err
	}
	root, err := scope.preferredRootPath()
	if err != nil {
		return "", "", err
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}
	candidate = filepath.Clean(candidate)
	check := candidate
	for {
		if _, statErr := os.Lstat(check); statErr == nil {
			break
		} else if !allowMissing || !errors.Is(statErr, os.ErrNotExist) {
			return "", "", fmt.Errorf("stat path %q: %w", path, statErr)
		}
		parent := filepath.Dir(check)
		if parent == check {
			return "", "", fmt.Errorf("path %q has no existing parent", path)
		}
		check = parent
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", "", fmt.Errorf("resolve path symlinks: %w", err)
	}
	if check != candidate {
		suffix, suffixErr := filepath.Rel(check, candidate)
		if suffixErr != nil {
			return "", "", fmt.Errorf("resolve path suffix: %w", suffixErr)
		}
		resolved = filepath.Join(resolved, suffix)
	}
	resolved = filepath.Clean(resolved)
	if beeSecretsPath(candidate) || beeSecretsPath(resolved) {
		return "", "", fmt.Errorf("path %q is inside Bee's secrets directory, which tools cannot access; the harness reads its own secrets directly", path)
	}
	for _, allowedRoot := range roots {
		rel, relErr := filepath.Rel(allowedRoot, resolved)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return candidate, resolved, nil
		}
	}
	return "", "", fmt.Errorf("path %q escapes all execution scope roots", path)
}

// beeSecretsPath reports whether path is Bee's secrets directory or inside
// it, comparing against both the lexical and symlink-resolved secrets root.
// The home directory is a default readable root, so without this the file
// tools could read provider keys and serve tokens.
func beeSecretsPath(path string) bool {
	secrets, err := DefaultBeeSecretsRoot()
	if err != nil || strings.TrimSpace(path) == "" {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range []string{filepath.Clean(secrets), resolvedOrSelf(secrets)} {
		if path == root || scopeWithin(root, path) {
			return true
		}
	}
	return false
}

func resolvedOrSelf(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// offeredSessionGrant returns the directory to offer for "approve for this
// session" on an out-of-bounds write by a path-addressed file or git tool,
// or "" when none applies (no session, not a path tool, or no eligible
// directory). host_exec and everything else are approve-each-time.
func (executor Executor) offeredSessionGrant(ctx context.Context, name string, args json.RawMessage) string {
	if store, _ := sessionWriteGrantsFor(ctx); store == nil {
		return ""
	}
	var path string
	switch name {
	case "write_file":
		var input writeArgs
		if parseArgs(args, &input) != nil {
			return ""
		}
		path = input.Path
	case "edit_file":
		var input editArgs
		if parseArgs(args, &input) != nil {
			return ""
		}
		path = input.Path
	case "git_stage":
		var input gitStageArgs
		if parseArgs(args, &input) != nil {
			return ""
		}
		path = input.RepoPath
	case "git_commit":
		var input gitCommitArgs
		if parseArgs(args, &input) != nil {
			return ""
		}
		path = input.RepoPath
	default:
		return ""
	}
	if strings.TrimSpace(path) == "" {
		return ""
	}
	_, resolved, err := resolveSecurePath(executor.Scope, path, true)
	if err != nil {
		return ""
	}
	if needs, err := pathNeedsWriteApproval(executor.Scope, path, true); err != nil || !needs {
		return ""
	}
	start := resolved
	if name == "write_file" || name == "edit_file" {
		start = filepath.Dir(resolved) // repository paths already name a directory
	}
	return sessionGrantDirectory(executor.Scope, start)
}

// noApprovalHookError explains a gated call that nobody can approve. An
// interactive client without a broker keeps the generic message. An
// unattended run says what it tried: for a write outside the write roots,
// the tool, target and roots; otherwise that the tool needs the owner.
func (executor *Executor) noApprovalHookError(name, effectiveName string, args json.RawMessage) *AuthorizationError {
	if !executor.Unattended {
		return &AuthorizationError{Tool: name, Layer: "approval", Cause: errors.New("requires approval but no approval hook is configured"), Recovery: "use an interactive client or configure an approval broker"}
	}
	if path, allowMissing, handled, err := assistantMutationTarget(effectiveName, args); err == nil && handled {
		if outside, err := pathNeedsWriteApproval(executor.Scope, path, allowMissing); err == nil && outside {
			roots, _ := executor.Scope.writeRootPaths()
			return &AuthorizationError{Tool: name, Layer: "approval",
				Cause:    fmt.Errorf("%s target %s is outside this job's write roots (%s)", effectiveName, describeWriteTarget(executor.Scope, path, allowMissing), describeWriteRoots(roots)),
				Recovery: "jobs never auto-approve; change the job's prompt or the contract's workspace_paths so its writes stay inside the write roots"}
		}
	}
	return &AuthorizationError{Tool: name, Layer: "approval",
		Cause:    errors.New("requires owner approval, and scheduled jobs never auto-approve"),
		Recovery: "change the job's prompt or contract so it does not need this tool, or do the step interactively"}
}

const maxDescribedPathBytes = 256

// describeWriteTarget names a refused target for an error message: the
// resolved absolute path when it resolves, bounded and quoted, and never a
// path inside the secrets directory.
func describeWriteTarget(scope ExecutionScope, path string, allowMissing bool) string {
	if _, resolved, err := resolveSecurePath(scope, path, allowMissing); err == nil && resolved != "" {
		path = resolved
	}
	if secrets, err := DefaultBeeSecretsRoot(); err == nil && pathWithinAny([]string{secrets}, filepath.Clean(path)) {
		return "(a path in the secrets directory)"
	}
	if len(path) > maxDescribedPathBytes {
		cut := maxDescribedPathBytes
		for cut > 0 && !utf8.RuneStart(path[cut]) {
			cut--
		}
		path = path[:cut] + "…"
	}
	return strconv.Quote(path)
}

func describeWriteRoots(roots []string) string {
	if len(roots) == 0 {
		return "none"
	}
	quoted := make([]string, 0, len(roots))
	for _, root := range roots {
		quoted = append(quoted, strconv.Quote(root))
	}
	return strings.Join(quoted, ", ")
}

func pathNeedsWriteApproval(scope ExecutionScope, path string, allowMissing bool) (bool, error) {
	_, resolved, err := resolveSecurePath(scope, path, allowMissing)
	if err != nil {
		return false, err
	}
	writeRoots, err := scope.writeRootPaths()
	if err != nil {
		return false, err
	}
	return !pathWithinAny(writeRoots, resolved), nil
}

func secureMutationPath(ctx context.Context, scope ExecutionScope, path string, allowMissing bool) (string, error) {
	scope = scope.withSessionGrants(ctx)
	candidate, resolved, err := resolveSecurePath(scope, path, allowMissing)
	if err != nil {
		return "", err
	}
	writeRoots, err := scope.writeRootPaths()
	if err != nil {
		return "", err
	}
	if pathWithinAny(writeRoots, resolved) || hasExplicitToolApproval(ctx) {
		return candidate, nil
	}
	return "", fmt.Errorf("path %q is outside the configured write roots and requires explicit approval", path)
}

func parseArgs(args json.RawMessage, target any) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("arguments contain multiple JSON values")
		}
		return err
	}
	return nil
}

type pathArgs struct {
	Path string `json:"path"`
}

// ReadFileTool reads a bounded UTF-8/file byte stream within the scope root.
type ReadFileTool struct{ Scope ExecutionScope }

func (tool ReadFileTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "read_file", Description: "Read a text file. Small files are returned whole. For a large file, or to read part of one, pass offset (1-based first line) and/or limit (number of lines); output is capped at about 64 KB, and a paged or capped read ends with a [read_file: lines A-B of N] trailer giving the offset to continue from. Prefer this over cat/sed for reading files you will edit.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1,"description":"1-based line to start at"},"limit":{"type":"integer","minimum":1,"maximum":100000,"description":"maximum lines to return"}},"required":["path"],"additionalProperties":false}`)}
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// readFileScanLimit bounds how far past the returned page read_file scans to
// report the file's total line count.
const readFileScanLimit = 64 << 20

func (tool ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("read_file arguments: %w", err)
	}
	if input.Offset < 0 || input.Limit < 0 || input.Limit > 100000 {
		return "", errors.New("read_file offset must be >= 1 and limit between 1 and 100000")
	}
	path, err := securePath(tool.Scope, input.Path, false)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	budget := tool.Scope.maxOutput()
	if input.Offset == 0 && input.Limit == 0 {
		data, err := io.ReadAll(io.LimitReader(file, int64(budget)+1))
		if err != nil {
			return "", err
		}
		if len(data) <= budget {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
			return string(data), nil
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	return readFileLines(ctx, file, input.Offset, input.Limit, budget)
}

// readFileLines returns whole lines from offset (1-based) up to limit lines
// and the byte budget, then a trailer with the range shown, the total line
// count (scanned up to readFileScanLimit), and where to continue.
func readFileLines(ctx context.Context, file io.Reader, offset, limit, budget int) (string, error) {
	start := offset
	if start <= 0 {
		start = 1
	}
	const trailerRoom = 160
	keep := budget - trailerRoom
	if keep < 1 {
		keep = budget
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	var out strings.Builder
	line, shown, first, last, next := 0, 0, 0, 0, 0
	scanned := 0
	totalKnown := true
	longLine := false
	for {
		if line%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		counting := shown > 0 && (next != 0 || (limit > 0 && shown >= limit))
		if counting && scanned > readFileScanLimit {
			totalKnown = false
			break
		}
		lineKeep := keep - out.Len()
		if line+1 < start || counting {
			lineKeep = 0
		}
		data, size, ok, eof, err := readFileLine(reader, lineKeep)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		line++
		scanned += size
		truncated := len(data) < size
		if line >= start && !counting {
			switch {
			case shown > 0 && truncated:
				next = line
			default:
				out.Write(data)
				if truncated {
					longLine = true
					out.WriteString("…[line truncated]\n")
				}
				shown++
				if first == 0 {
					first = line
				}
				last = line
			}
		}
		if eof {
			break
		}
	}
	if shown == 0 {
		if line == 0 {
			return "", nil
		}
		return "", fmt.Errorf("read_file offset %d is past the end of the file (%d lines)", start, line)
	}
	if next == 0 && last < line {
		next = last + 1
	}
	total := fmt.Sprintf("%d", line)
	if !totalKnown {
		total = fmt.Sprintf("more than %d", line)
	}
	trailer := fmt.Sprintf("[read_file: lines %d-%d of %s", first, last, total)
	if next != 0 {
		trailer += fmt.Sprintf("; continue with offset=%d", next)
	}
	if longLine {
		trailer += "; long lines were cut at the output cap"
	}
	trailer += "]"
	text := out.String()
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + trailer, nil
}

// readFileLine reads one line (including its newline), keeping at most keep
// bytes of it; n is the line's full length. ok is false at a clean end.
func readFileLine(reader *bufio.Reader, keep int) (data []byte, n int, ok, eof bool, err error) {
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			ok = true
			n += len(chunk)
			if room := keep - len(data); room > 0 {
				data = append(data, chunk[:min(room, len(chunk))]...)
			}
		}
		switch {
		case readErr == nil:
			return data, n, ok, false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return data, n, ok, true, nil
		default:
			return nil, 0, false, false, readErr
		}
	}
}

type listArgs struct {
	Path string `json:"path"`
}

// ListFilesTool lists direct children, sorted for deterministic results.
type ListFilesTool struct{ Scope ExecutionScope }

func (tool ListFilesTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "list_files", Description: "List the entries directly inside one directory, one per line, sorted, with a trailing / on subdirectories. Not recursive: use find_files to search by name or grep to search contents.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
}
func (tool ListFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input listArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("list_files arguments: %w", err)
	}
	path := input.Path
	if path == "" {
		path = "."
	}
	path, err := securePath(tool.Scope, path, false)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		lines = append(lines, entry.Name()+suffix)
	}
	return strings.Join(lines, "\n"), nil
}

type findArgs struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

// FindFilesTool recursively finds files by filepath.Match pattern.
type FindFilesTool struct{ Scope ExecutionScope }

func (tool FindFilesTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "find_files", Description: "Find files by name, recursively. pattern is a shell glob matched against the file name (e.g. *.go, Makefile, test_*.py), not the path. Skips .git, node_modules, and similar directories. Returns sorted paths (relative to the Bee workspace, absolute elsewhere), at most 1000.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"directory to search (default: the preferred root)"},"pattern":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)}
}

// maxFindFilesResults bounds find_files output.
const maxFindFilesResults = 1000

func (tool FindFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input findArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("find_files arguments: %w", err)
	}
	if input.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	if _, err := filepath.Match(input.Pattern, ""); err != nil {
		return "", fmt.Errorf("find_files pattern %q is not a valid glob: %w", input.Pattern, err)
	}
	if input.Path == "" {
		input.Path = "."
	}
	root, err := securePath(tool.Scope, input.Path, false)
	if err != nil {
		return "", err
	}
	scopeRoot, err := tool.Scope.rootPath()
	if err != nil {
		return "", err
	}
	var matches []string
	more := false
	err = walkSearchFiles(ctx, root, func(path string, entry os.DirEntry) error {
		if matched, _ := filepath.Match(input.Pattern, entry.Name()); matched {
			if len(matches) >= maxFindFilesResults {
				more = true
				return errStopSearch
			}
			matches = append(matches, displaySearchPath(scopeRoot, path))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	output := strings.Join(matches, "\n")
	if more {
		output += fmt.Sprintf("\n[find_files: showing the first %d matches; narrow the path or pattern]", maxFindFilesResults)
	}
	return output, nil
}

type grepArgs struct {
	Path       string `json:"path"`
	Query      string `json:"query"`
	IgnoreCase bool   `json:"ignore_case"`
	Regex      bool   `json:"regex,omitempty"`
	Glob       string `json:"glob,omitempty"`
	Context    int    `json:"context,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

const (
	defaultGrepResults   = 200
	maxGrepResults       = 1000
	maxGrepContext       = 5
	maxGrepLineBytes     = 400
	maxGrepFileBytes     = 16 << 20
	maxGrepScanLineBytes = 1 << 20
)

// GrepTool searches text files line by line and returns path:line:content rows.
type GrepTool struct{ Scope ExecutionScope }

func (tool GrepTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "grep", Description: "Search file contents. query is a literal substring unless regex is true (Go RE2 syntax, e.g. func \\w+Tool). Optional glob limits files by name (e.g. *.go), context adds up to 5 lines around each match, max_results caps output (default 200). Skips .git, node_modules, and similar directories, binary files, and files over 16 MB. Output is path:line:text, with path-line-text for context lines; paths are relative to the Bee workspace, absolute elsewhere.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file or directory to search (default: the preferred root)"},"query":{"type":"string"},"ignore_case":{"type":"boolean"},"regex":{"type":"boolean"},"glob":{"type":"string"},"context":{"type":"integer","minimum":0,"maximum":5},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["query"],"additionalProperties":false}`)}
}

func (tool GrepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input grepArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("grep arguments: %w", err)
	}
	if input.Query == "" {
		return "", errors.New("query is required")
	}
	if input.Context < 0 || input.Context > maxGrepContext {
		return "", fmt.Errorf("grep context must be between 0 and %d", maxGrepContext)
	}
	limit := input.MaxResults
	if limit <= 0 {
		limit = defaultGrepResults
	}
	if limit > maxGrepResults {
		limit = maxGrepResults
	}
	if input.Glob != "" {
		if _, err := filepath.Match(input.Glob, ""); err != nil {
			return "", fmt.Errorf("grep glob %q is not valid: %w", input.Glob, err)
		}
	}
	match, err := grepMatcher(input)
	if err != nil {
		return "", err
	}
	if input.Path == "" {
		input.Path = "."
	}
	root, err := securePath(tool.Scope, input.Path, false)
	if err != nil {
		return "", err
	}
	scopeRoot, err := tool.Scope.rootPath()
	if err != nil {
		return "", err
	}
	var out []string
	matches := 0
	more := false
	search := func(path string) error {
		lines, found, truncatedSearch, err := grepFile(path, displaySearchPath(scopeRoot, path), match, input.Context, limit-matches)
		if err != nil {
			return nil // unreadable or binary: skip
		}
		if found > 0 {
			if len(out) > 0 && input.Context > 0 {
				out = append(out, "--")
			}
			out = append(out, lines...)
			matches += found
		}
		if truncatedSearch {
			more = true
			return errStopSearch
		}
		return nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		err = walkSearchFiles(ctx, root, func(path string, entry os.DirEntry) error {
			if input.Glob != "" {
				if ok, _ := filepath.Match(input.Glob, entry.Name()); !ok {
					return nil
				}
			}
			return search(path)
		})
	} else {
		err = search(root)
	}
	if err != nil {
		return "", err
	}
	output := strings.Join(out, "\n")
	if more {
		output += fmt.Sprintf("\n[grep: showing the first %d matches; narrow with path or glob, or raise max_results]", matches)
	}
	return output, nil
}

func grepMatcher(input grepArgs) (func(string) bool, error) {
	if input.Regex {
		pattern := input.Query
		if input.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("grep regex is invalid (%v); set regex false to search for the literal text", err)
		}
		return expression.MatchString, nil
	}
	if input.IgnoreCase {
		needle := strings.ToLower(input.Query)
		return func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }, nil
	}
	return func(line string) bool { return strings.Contains(line, input.Query) }, nil
}

// grepFile returns formatted match (and context) lines for one file, up to
// remaining matches. A binary or oversized file is an error (skipped).
func grepFile(path, display string, match func(string) bool, context, remaining int) ([]string, int, bool, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGrepFileBytes {
		return nil, 0, false, errors.New("skipped")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	head := make([]byte, 8000)
	n, _ := io.ReadFull(file, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, 0, false, errors.New("binary")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	clip := func(text string) string {
		if len(text) > maxGrepLineBytes {
			return truncateUTF8(text, maxGrepLineBytes) + "…"
		}
		return text
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxGrepScanLineBytes)
	var out []string
	var before []string
	found := 0
	after := 0
	lastPrinted := 0
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if match(text) {
			if found >= remaining {
				return out, found, true, nil
			}
			if context > 0 && lastPrinted > 0 && line-len(before) > lastPrinted+1 {
				out = append(out, "--")
			}
			for index, previous := range before {
				out = append(out, fmt.Sprintf("%s-%d-%s", display, line-len(before)+index, clip(previous)))
			}
			before = before[:0]
			out = append(out, fmt.Sprintf("%s:%d:%s", display, line, clip(text)))
			lastPrinted = line
			found++
			after = context
			continue
		}
		if after > 0 {
			out = append(out, fmt.Sprintf("%s-%d-%s", display, line, clip(text)))
			lastPrinted = line
			after--
			continue
		}
		if context > 0 {
			before = append(before, text)
			if len(before) > context {
				before = before[1:]
			}
		}
	}
	return out, found, false, nil
}

// errStopSearch ends a bounded search walk early without being an error.
var errStopSearch = errors.New("stop search")

// searchSkipDirs are never descended into by grep and find_files: version
// control internals and dependency or tool caches drown real results.
var searchSkipDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true, "__pycache__": true, ".venv": true, ".direnv": true, ".mypy_cache": true, ".pytest_cache": true}

// walkSearchFiles visits regular files below root, skipping noise
// directories, the secrets directory, symlinks, and anything unreadable
// (a permission-denied directory no longer aborts the whole search).
func walkSearchFiles(ctx context.Context, root string, visit func(string, os.DirEntry) error) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() && path != root {
				return filepath.SkipDir
			}
			if path == root {
				return walkErr
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if path != root && (searchSkipDirs[entry.Name()] || beeSecretsPath(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		return visit(path, entry)
	})
	if errors.Is(err, errStopSearch) {
		return nil
	}
	return err
}

// displaySearchPath shows a result relative to the primary root when it is
// inside it, and absolute otherwise (never ../../ chains).
func displaySearchPath(scopeRoot, path string) string {
	if rel, err := filepath.Rel(scopeRoot, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

const (
	writeOperationCreate  = "create"
	writeOperationAppend  = "append"
	writeOperationEdit    = "edit"
	writeOperationReplace = "replace"
)

type writeArgs struct {
	Path           string  `json:"path"`
	Operation      string  `json:"operation,omitempty"`
	Content        *string `json:"content,omitempty"`
	OldText        *string `json:"old_text,omitempty"`
	NewText        *string `json:"new_text,omitempty"`
	ExpectedSHA256 string  `json:"expected_sha256,omitempty"`
}

// WriteFileTool writes a file atomically enough for the small local tool
// boundary: it creates parents, but never follows a path outside Root.
type WriteFileTool struct {
	Scope   ExecutionScope
	Journal *MutationJournal
}

func (tool WriteFileTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "write_file",
		Description: "Write a whole file. operation is required: create makes a new file (fails if it already exists), append adds content to the end (creating the file if missing), replace overwrites an existing file entirely. To change part of a file, use edit_file instead. Parent directories are created. expected_sha256 optionally refuses the write if the file changed since you read it. Relative paths resolve against the preferred Bee write root; a path outside the write roots asks the user for approval.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"operation":{"type":"string","enum":["create","append","replace"]},"content":{"type":"string","maxLength":8388608},"expected_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}},"required":["path","operation","content"],"additionalProperties":false}`),
	}
}

func (tool WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input writeArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("write_file arguments: %w", err)
	}
	// Still accepts operation "edit" (and an omitted operation as create) for
	// older transcripts and habits; edit_file is the advertised edit path.
	return applyFileMutation(ctx, "write_file", tool.Scope, tool.Journal, input, false)
}

// applyFileMutation is the one journaled mutation path behind write_file
// and edit_file; name labels errors and the journal entry.
func applyFileMutation(ctx context.Context, name string, scope ExecutionScope, journal *MutationJournal, input writeArgs, replaceAll bool) (string, error) {
	operation, err := validateWriteArgs(input)
	if err != nil {
		return "", fmt.Errorf("%s arguments: %w", name, err)
	}
	allowMissing := operation == writeOperationCreate || operation == writeOperationAppend
	path, err := secureMutationPath(ctx, scope, input.Path, allowMissing)
	if err != nil {
		return "", err
	}
	if allowMissing {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
	}
	before, beforeData, err := fileState(path)
	if err != nil {
		return "", err
	}
	if input.ExpectedSHA256 != "" {
		if !before.Exists || before.SHA256 != input.ExpectedSHA256 {
			return "", fmt.Errorf("%s refused: current file does not match expected_sha256", name)
		}
	}

	var updated []byte
	var result string
	switch operation {
	case writeOperationCreate:
		if before.Exists {
			return "", fmt.Errorf("%s create refused: the file already exists; use operation replace to overwrite it, append to add to it, or edit_file to change part of it", name)
		}
		updated = []byte(*input.Content)
		result = fmt.Sprintf("created %d bytes", len(updated))
	case writeOperationAppend:
		if len(beforeData) > mutationMaxFileBytes-len(*input.Content) {
			return "", fmt.Errorf("%s result exceeds file limit of %d bytes", name, mutationMaxFileBytes)
		}
		updated = make([]byte, 0, len(beforeData)+len(*input.Content))
		updated = append(updated, beforeData...)
		updated = append(updated, (*input.Content)...)
		result = fmt.Sprintf("appended %d bytes", len(*input.Content))
	case writeOperationEdit:
		if !before.Exists {
			return "", fmt.Errorf("%s edit requires an existing file", name)
		}
		count := bytes.Count(beforeData, []byte(*input.OldText))
		if count == 0 || (count > 1 && !replaceAll) {
			return "", editMatchError(name, beforeData, *input.OldText, count)
		}
		replacements := 1
		if replaceAll {
			replacements = count
		}
		if int64(len(beforeData))+int64(replacements)*(int64(len(*input.NewText))-int64(len(*input.OldText))) > mutationMaxFileBytes {
			return "", fmt.Errorf("%s result exceeds file limit of %d bytes", name, mutationMaxFileBytes)
		}
		updated = bytes.Replace(beforeData, []byte(*input.OldText), []byte(*input.NewText), replacements)
		result = "edited one exact text region"
		if replacements > 1 {
			result = fmt.Sprintf("replaced %d occurrences", replacements)
		}
	case writeOperationReplace:
		if !before.Exists {
			return "", fmt.Errorf("%s replace requires an existing file; use create for a new file", name)
		}
		updated = []byte(*input.Content)
		result = fmt.Sprintf("replaced file with %d bytes", len(updated))
	}
	if len(updated) > mutationMaxFileBytes {
		return "", fmt.Errorf("%s result exceeds file limit of %d bytes", name, mutationMaxFileBytes)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	mode := os.FileMode(0o644)
	if before.Exists {
		mode = os.FileMode(before.Mode)
	}
	write := atomicWriteFile
	if !before.Exists {
		write = atomicCreateFile
	}
	if err := write(path, updated, mode); err != nil {
		if !before.Exists && errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%s refused: file appeared concurrently; retry with an explicit operation", name)
		}
		return "", err
	}
	if journal != nil {
		after, _, stateErr := fileState(path)
		if stateErr != nil {
			if revertErr := restoreMutationFile(path, before, beforeData); revertErr != nil {
				return "", fmt.Errorf("%s postimage inspection failed (%v) and mutation reversion failed: %w", name, stateErr, revertErr)
			}
			return "", fmt.Errorf("%s postimage inspection failed; mutation reverted: %w", name, stateErr)
		}
		if _, journalErr := journal.Record(ctx, name, path, before, beforeData, after, result); journalErr != nil {
			if revertErr := restoreMutationFile(path, before, beforeData); revertErr != nil {
				return "", fmt.Errorf("%s journal commit failed (%v) and mutation reversion failed: %w", name, journalErr, revertErr)
			}
			return "", fmt.Errorf("%s journal commit failed; mutation reverted: %w", name, journalErr)
		}
	}
	return fmt.Sprintf("%s in %s", result, input.Path), nil
}

// EditFileTool replaces exact text in an existing file. It shares the
// journaled mutation path, approvals, and session grants with write_file.
type EditFileTool struct {
	Scope   ExecutionScope
	Journal *MutationJournal
}

type editArgs struct {
	Path           string  `json:"path"`
	OldText        *string `json:"old_text"`
	NewText        *string `json:"new_text"`
	ReplaceAll     bool    `json:"replace_all,omitempty"`
	ExpectedSHA256 string  `json:"expected_sha256,omitempty"`
}

func (tool EditFileTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "edit_file",
		Description: "Change part of an existing file by replacing old_text with new_text. old_text must match the file exactly, including indentation, tabs, and line endings; copy it from read_file output. It must occur exactly once unless replace_all is true; if it matches zero or several places, the error says where, so add surrounding lines to make it unique. Use write_file to create or overwrite a whole file. expected_sha256 optionally refuses the edit if the file changed since you read it.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"old_text":{"type":"string","minLength":1,"maxLength":8388608},"new_text":{"type":"string","maxLength":8388608},"replace_all":{"type":"boolean"},"expected_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}},"required":["path","old_text","new_text"],"additionalProperties":false}`),
	}
}

func (tool EditFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input editArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("edit_file arguments: %w", err)
	}
	if input.OldText == nil || input.NewText == nil {
		return "", errors.New("edit_file arguments: old_text and new_text are required")
	}
	return applyFileMutation(ctx, "edit_file", tool.Scope, tool.Journal, writeArgs{Path: input.Path, Operation: writeOperationEdit, OldText: input.OldText, NewText: input.NewText, ExpectedSHA256: input.ExpectedSHA256}, input.ReplaceAll)
}

// editMatchError explains why old_text did not match exactly once, with the
// line numbers involved, so the next attempt can succeed instead of guessing.
func editMatchError(name string, data []byte, oldText string, count int) error {
	lineOf := func(offset int) int { return bytes.Count(data[:offset], []byte("\n")) + 1 }
	if count > 1 {
		lines := make([]string, 0, 5)
		for index, offset := 0, 0; index < 5; index++ {
			found := bytes.Index(data[offset:], []byte(oldText))
			if found < 0 {
				break
			}
			if number := strconv.Itoa(lineOf(offset + found)); len(lines) == 0 || lines[len(lines)-1] != number {
				lines = append(lines, number)
			}
			offset += found + max(1, len(oldText))
		}
		return fmt.Errorf("%s: old_text matches %d places (lines %s); include more surrounding lines to make it unique, or set replace_all to change every occurrence", name, count, strings.Join(lines, ", "))
	}
	collapse := func(value string) string { return strings.Join(strings.Fields(value), " ") }
	firstLine := ""
	for _, line := range strings.Split(oldText, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			firstLine = trimmed
			break
		}
	}
	nearLine := 0
	if firstLine != "" {
		for index, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.TrimSpace(line), firstLine) {
				nearLine = index + 1
				break
			}
		}
	}
	if collapsed := collapse(oldText); collapsed != "" && strings.Contains(collapse(string(data)), collapsed) {
		where := ""
		if nearLine > 0 {
			where = fmt.Sprintf(" near line %d", nearLine)
		}
		return fmt.Errorf("%s: old_text was not found exactly, but matches%s when whitespace is ignored; copy the exact indentation, tabs, and line endings from read_file", name, where)
	}
	if nearLine > 0 {
		return fmt.Errorf("%s: old_text was not found; its first line appears at line %d but the lines after it differ; re-read the file there with read_file offset=%d", name, nearLine, max(1, nearLine-2))
	}
	return fmt.Errorf("%s: old_text was not found in the file; re-read it with read_file (it may have changed)", name)
}

func validateWriteArgs(input writeArgs) (string, error) {
	if strings.TrimSpace(input.Path) == "" {
		return "", errors.New("path is required")
	}
	operation := input.Operation
	if operation == "" {
		operation = writeOperationCreate
	}
	if input.ExpectedSHA256 != "" {
		if len(input.ExpectedSHA256) != 64 || strings.ToLower(input.ExpectedSHA256) != input.ExpectedSHA256 {
			return "", errors.New("expected_sha256 must be 64 lowercase hexadecimal characters")
		}
		if _, err := hex.DecodeString(input.ExpectedSHA256); err != nil {
			return "", errors.New("expected_sha256 must be 64 lowercase hexadecimal characters")
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"content", input.Content}, {"old_text", input.OldText}, {"new_text", input.NewText}} {
		if field.value != nil && len(*field.value) > mutationMaxFileBytes {
			return "", fmt.Errorf("%s exceeds file limit of %d bytes", field.name, mutationMaxFileBytes)
		}
	}
	rejectEditFields := func() error {
		if input.OldText != nil || input.NewText != nil {
			return errors.New("old_text and new_text are valid only for operation edit")
		}
		return nil
	}
	switch operation {
	case writeOperationCreate:
		if input.Content == nil {
			return "", errors.New("content is required for operation create")
		}
		if input.ExpectedSHA256 != "" {
			return "", errors.New("expected_sha256 is not valid for operation create")
		}
		if err := rejectEditFields(); err != nil {
			return "", err
		}
	case writeOperationAppend:
		if input.Content == nil || *input.Content == "" {
			return "", errors.New("non-empty content is required for operation append")
		}
		if err := rejectEditFields(); err != nil {
			return "", err
		}
	case writeOperationEdit:
		if input.Content != nil {
			return "", errors.New("content is not valid for operation edit; use old_text and new_text")
		}
		if input.OldText == nil || *input.OldText == "" {
			return "", errors.New("non-empty old_text is required for operation edit")
		}
		if input.NewText == nil {
			return "", errors.New("new_text is required for operation edit")
		}
	case writeOperationReplace:
		if input.Content == nil {
			return "", errors.New("content is required for operation replace")
		}
		if err := rejectEditFields(); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported operation %q", input.Operation)
	}
	return operation, nil
}

func restoreMutationFile(path string, before MutationFileState, data []byte) error {
	if before.Exists {
		return atomicWriteFile(path, data, os.FileMode(before.Mode))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type runArgs struct {
	Target   CommandTarget `json:"target,omitempty"`
	Host     string        `json:"host,omitempty"`
	User     string        `json:"user,omitempty"`
	Port     int           `json:"port,omitempty"`
	Argv     []string      `json:"argv"`
	Dir      string        `json:"dir,omitempty"`
	Elevated bool          `json:"elevated,omitempty"`
	Stdin    string        `json:"stdin,omitempty"`
}

// RunCommandTool executes an argv directly, with a filtered environment and
// working directory containment. Shell syntax is intentionally unavailable.
type RunCommandTool struct {
	Scope         ExecutionScope
	ArtifactStore *ArtifactStore
}

func (tool RunCommandTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "run_command", Description: "Run a bounded argv command in the workspace, on the local host, or over SSH. Omitted target means workspace. For target ssh, provide the exact safe host and user and optionally port; the trusted foreground assistant may execute that target directly, while an active parent-owned grant can narrow or supply the target in stricter contexts.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","enum":["workspace","local","ssh"]},"host":{"type":"string"},"user":{"type":"string"},"port":{"type":"integer"},"argv":{"type":"array","items":{"type":"string"}},"dir":{"type":"string"},"elevated":{"type":"boolean"},"stdin":{"type":"string"}},"required":["argv"]}`)}
}
func (tool RunCommandTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input runArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("run_command arguments: %w", err)
	}
	if len(input.Argv) == 0 || strings.TrimSpace(input.Argv[0]) == "" {
		return "", errors.New("argv is required")
	}
	if input.Target == "" {
		input.Target = CommandTargetWorkspace
	}
	for _, arg := range input.Argv {
		if input.Target == CommandTargetSSH {
			if err := validateRemoteArg(arg); err != nil {
				return "", err
			}
		}
	}
	if input.Target == CommandTargetSSH || input.Target == CommandTargetLocal {
		if input.Target == CommandTargetSSH {
			if tool.Scope.RemoteGrant != nil {
				resolved := tool.Scope.RemoteGrant.resolveSSHRequest(commandRequest{Target: input.Target, Host: input.Host, User: input.User, Port: input.Port})
				input.Host, input.User, input.Port = resolved.Host, resolved.User, resolved.Port
			} else if !tool.Scope.TrustedForeground {
				return "", errors.New("run_command SSH target requires trusted foreground scope or a parent-owned session capability grant")
			}
		}
		if tool.Scope.RemoteGrant != nil {
			if err := tool.Scope.RemoteGrant.permits(commandRequest{Target: input.Target, Host: input.Host, User: input.User, Port: input.Port, Elevated: input.Elevated, Stdin: input.Stdin}, time.Now().UTC()); err != nil {
				return "", err
			}
		}
		if input.Target == CommandTargetSSH {
			return executeSSHCommand(ctx, input, tool.Scope, tool.ArtifactStore)
		}
		return executeLocalCommand(ctx, input.Argv, input.Stdin, "", tool.Scope, tool.ArtifactStore)
	}
	if input.Elevated {
		return "", errors.New("elevated commands require an explicitly granted local or SSH target")
	}
	dir := input.Dir
	if dir == "" {
		dir = "."
	}
	working, err := securePath(tool.Scope, dir, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(working)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("command dir must be a directory")
	}
	return executeLocalCommand(ctx, input.Argv, input.Stdin, working, tool.Scope, tool.ArtifactStore)
}

func filteredEnvironment(allowlist []string) []string {
	if len(allowlist) == 0 {
		return []string{}
	}
	allowed := make(map[string]bool, len(allowlist))
	for _, key := range allowlist {
		allowed[key] = true
	}
	env := os.Environ()
	result := make([]string, 0, len(env))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			result = append(result, item)
		}
	}
	return result
}

// setAllowlistedChildEnvironment is the shared environment boundary for
// allowlist-based supervised children. The optional sandbox marker is
// Bee-owned nesting metadata, not inherited authority. No other parent
// environment crosses this seam.
func setAllowlistedChildEnvironment(command *exec.Cmd, allowlist []string, keepSandboxMarker bool) {
	command.Env = preserveProcessSandboxMarker(filteredEnvironment(allowlist), keepSandboxMarker)
}

// NewCodingRegistry returns the initial safe coding tool set.
func NewCodingRegistry(scope ExecutionScope) (*Registry, error) {
	return NewRegistry(ReadFileTool{Scope: scope}, ListFilesTool{Scope: scope}, FindFilesTool{Scope: scope}, GrepTool{Scope: scope}, WriteFileTool{Scope: scope}, EditFileTool{Scope: scope}, RunCommandTool{Scope: scope})
}

// withNetworkPolicy applies the calling session's /network setting.
func (scope ExecutionScope) withNetworkPolicy(ctx context.Context) ExecutionScope {
	if NetworkOffFromContext(ctx) {
		scope.NetworkOff = true
	}
	return scope
}
