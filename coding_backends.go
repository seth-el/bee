package bee

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CodingBackendKindACP        = "acp"
	CodingBackendKindLocalChild = "local_child"

	CodingBackendNameLocalChild = "local-child"

	DefaultCodingMaxWorktreeBytes = int64(512 << 20)
	DefaultCodingMaxWorktreeFiles = 4096
	DefaultCodingMaxArtifactBytes = int64(32 << 20)
	DefaultCodingMaxArtifactFiles = 256
)

var (
	ErrCodingBackendsDisabled   = errors.New("coding backends are disabled")
	ErrCodingBackendNotFound    = errors.New("coding backend is not configured")
	ErrCodingBackendUnavailable = errors.New("coding backend is unavailable")
)

// CodingBackendFailureKind keeps backend availability distinct from a task
// outcome. Only BackendFailureUnavailable is eligible for the explicit local
// fallback path; all other outcomes are terminal evidence.
type CodingBackendFailureKind string

const (
	BackendFailureUnavailable CodingBackendFailureKind = "backend_unavailable"
	BackendFailureTask        CodingBackendFailureKind = "task_failure"
	BackendFailureTimeout     CodingBackendFailureKind = "timeout"
	BackendFailureCancelled   CodingBackendFailureKind = "cancelled"
	BackendFailurePermission  CodingBackendFailureKind = "permission_denied"
	BackendFailureMalformed   CodingBackendFailureKind = "malformed_result"
)

type CodingBackendFailure struct {
	Kind CodingBackendFailureKind
	Err  error
}

// CodingBackendAttempt is bounded terminal evidence for one backend attempt.
// Paths are descriptive only; the parent still owns review and integration.
type CodingBackendAttempt struct {
	Backend       string                   `json:"backend"`
	AttemptID     string                   `json:"attempt_id,omitempty"`
	WorkspacePath string                   `json:"workspace_path,omitempty"`
	Status        ChildStatus              `json:"status"`
	FailureKind   CodingBackendFailureKind `json:"failure_kind,omitempty"`
	Summary       string                   `json:"summary,omitempty"`
}

func (failure *CodingBackendFailure) Error() string {
	if failure == nil || failure.Err == nil {
		return string(BackendFailureTask)
	}
	return failure.Err.Error()
}

func (failure *CodingBackendFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}

func newCodingBackendFailure(kind CodingBackendFailureKind, err error) error {
	if err == nil {
		err = errors.New(string(kind))
	}
	if kind == BackendFailureUnavailable {
		return fmt.Errorf("%w: %w", ErrCodingBackendUnavailable, &CodingBackendFailure{Kind: kind, Err: err})
	}
	return &CodingBackendFailure{Kind: kind, Err: err}
}

func codingBackendFailureKind(err error, packet ChildCompletionPacket) CodingBackendFailureKind {
	if packet.FailureKind != "" {
		return packet.FailureKind
	}
	var failure *CodingBackendFailure
	if errors.As(err, &failure) && failure != nil && failure.Kind != "" {
		return failure.Kind
	}
	if errors.Is(err, ErrCodingBackendUnavailable) {
		return BackendFailureUnavailable
	}
	switch packet.Status {
	case ChildTimedOut:
		return BackendFailureTimeout
	case ChildCancelled:
		return BackendFailureCancelled
	case ChildMalformed:
		return BackendFailureMalformed
	default:
		return BackendFailureTask
	}
}

// CodingBackendFailureKindOf is the public classifier used by parent
// orchestration and integration consumers.
func CodingBackendFailureKindOf(err error, packet ChildCompletionPacket) CodingBackendFailureKind {
	return codingBackendFailureKind(err, packet)
}

func IsCodingBackendUnavailable(err error) bool {
	return errors.Is(err, ErrCodingBackendUnavailable)
}

// CodingBackendRole selects the explicit job role for a coding child. Role
// selection never inspects prompts and never performs automatic escalation.
type CodingBackendRole string

const (
	CodingRoleImplementer CodingBackendRole = "implementer"
	CodingRoleReviewer    CodingBackendRole = "reviewer"
	CodingRoleScout       CodingBackendRole = "scout"

	// Compatibility aliases for callers that prefer the longer names.
	CodingBackendRoleImplementer = CodingRoleImplementer
	CodingBackendRoleReviewer    = CodingRoleReviewer
	CodingBackendRoleScout       = CodingRoleScout
)

func (role CodingBackendRole) valid() bool {
	return role == CodingRoleImplementer || role == CodingRoleReviewer || role == CodingRoleScout
}

// CodingBackendLimits are parent-side bounds associated with a backend. Zero
// values use the conservative defaults. They are metadata/configuration only;
// a backend never receives authority to widen a task's own scope.
type CodingBackendLimits struct {
	MaxWorktreeBytes int64 `json:"max_worktree_bytes,omitempty"`
	MaxWorktreeFiles int   `json:"max_worktree_files,omitempty"`
	MaxArtifactBytes int64 `json:"max_artifact_bytes,omitempty"`
	MaxArtifactFiles int   `json:"max_artifact_files,omitempty"`
}

func (limits CodingBackendLimits) withDefaults() CodingBackendLimits {
	if limits.MaxWorktreeBytes <= 0 {
		limits.MaxWorktreeBytes = DefaultCodingMaxWorktreeBytes
	}
	if limits.MaxWorktreeFiles <= 0 {
		limits.MaxWorktreeFiles = DefaultCodingMaxWorktreeFiles
	}
	if limits.MaxArtifactBytes <= 0 {
		limits.MaxArtifactBytes = DefaultCodingMaxArtifactBytes
	}
	if limits.MaxArtifactFiles <= 0 {
		limits.MaxArtifactFiles = DefaultCodingMaxArtifactFiles
	}
	return limits
}

func (limits CodingBackendLimits) Validate() error {
	if limits.MaxWorktreeBytes < 0 || limits.MaxWorktreeFiles < 0 || limits.MaxArtifactBytes < 0 || limits.MaxArtifactFiles < 0 {
		return errors.New("coding backend limits must not be negative")
	}
	effective := limits.withDefaults()
	if effective.MaxArtifactBytes > effective.MaxWorktreeBytes {
		return errors.New("coding backend max_artifact_bytes must not exceed max_worktree_bytes")
	}
	return nil
}

// CodingBackendDefinition describes one named backend. ACP process settings
// are explicit and opaque: Bee does not discover adapter binaries, import
// OAuth files, or read OpenClaw state.
type CodingBackendDefinition struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`

	// Process is the preferred structured form. ACP is a compatibility alias;
	// Command/Args/etc. are convenient JSON-compatible aliases for small
	// hand-written configs.
	Process                ACPProcessConfig `json:"process,omitempty"`
	ACP                    ACPProcessConfig `json:"acp,omitempty"`
	Command                string           `json:"command,omitempty"`
	Args                   []string         `json:"args,omitempty"`
	AdapterCommand         string           `json:"adapter_command,omitempty"`
	AdapterArgs            []string         `json:"adapter_args,omitempty"`
	EnvAllowlist           []string         `json:"env_allowlist,omitempty"`
	AuthMethod             string           `json:"auth_method,omitempty"`
	AuthEnvVar             string           `json:"auth_env_var,omitempty"`
	StartupTimeoutSeconds  int              `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds  int              `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes          int              `json:"max_frame_bytes,omitempty"`
	MaxMessageBytes        int              `json:"max_message_bytes,omitempty"`
	AutoApprovePermissions bool             `json:"auto_approve_permissions,omitempty"`

	Limits CodingBackendLimits `json:"limits,omitempty"`
}

// CodingBackendSpec is a short compatibility name for backend definitions.
type CodingBackendSpec = CodingBackendDefinition

func (definition CodingBackendDefinition) processConfig() ACPProcessConfig {
	process := definition.Process
	if acpProcessConfigEmpty(process) {
		process = definition.ACP
	}
	if definition.AdapterCommand != "" {
		process.Command = definition.AdapterCommand
	}
	if definition.AdapterArgs != nil {
		process.Args = append([]string(nil), definition.AdapterArgs...)
	}
	if definition.Command != "" {
		process.Command = definition.Command
	}
	if definition.Args != nil {
		process.Args = append([]string(nil), definition.Args...)
	}
	if definition.EnvAllowlist != nil {
		process.EnvAllowlist = append([]string(nil), definition.EnvAllowlist...)
	}
	if definition.AuthMethod != "" {
		process.AuthMethod = definition.AuthMethod
	}
	if definition.AuthEnvVar != "" {
		process.AuthEnvVar = definition.AuthEnvVar
	}
	if definition.StartupTimeoutSeconds != 0 {
		process.StartupTimeoutSeconds = definition.StartupTimeoutSeconds
	}
	if definition.RequestTimeoutSeconds != 0 {
		process.RequestTimeoutSeconds = definition.RequestTimeoutSeconds
	}
	if definition.MaxFrameBytes != 0 {
		process.MaxFrameBytes = definition.MaxFrameBytes
	}
	if definition.MaxMessageBytes != 0 {
		process.MaxMessageBytes = definition.MaxMessageBytes
	}
	if definition.AutoApprovePermissions {
		process.AutoApprovePermissions = true
	}
	return process
}

func acpProcessConfigEmpty(config ACPProcessConfig) bool {
	return config.Command == "" && len(config.Args) == 0 && config.WorkingDir == "" && len(config.EnvAllowlist) == 0 && config.AuthMethod == "" && config.AuthEnvVar == "" && config.StartupTimeoutSeconds == 0 && config.RequestTimeoutSeconds == 0 && config.MaxFrameBytes == 0 && config.MaxMessageBytes == 0 && !config.AutoApprovePermissions
}

func (definition CodingBackendDefinition) Validate() error {
	if err := validateCodingBackendName(definition.Name); err != nil {
		return err
	}
	kind := strings.TrimSpace(definition.Kind)
	if kind == "" {
		kind = CodingBackendKindACP
	}
	switch kind {
	case CodingBackendKindACP:
		process := definition.processConfig()
		if !definition.Enabled && strings.TrimSpace(process.Command) == "" {
			// Disabled entries may be checked into a shared config without an
			// adapter installed on every host.
			return definition.Limits.Validate()
		}
		if err := process.Validate(); err != nil {
			return fmt.Errorf("coding backend %q process: %w", definition.Name, err)
		}
	case CodingBackendKindLocalChild:
		if strings.TrimSpace(definition.Command) != "" || strings.TrimSpace(definition.AdapterCommand) != "" || strings.TrimSpace(definition.Process.Command) != "" || strings.TrimSpace(definition.ACP.Command) != "" {
			return fmt.Errorf("coding backend %q local_child must not configure an adapter command", definition.Name)
		}
	default:
		return fmt.Errorf("coding backend %q has unsupported kind %q", definition.Name, kind)
	}
	return definition.Limits.Validate()
}

// CodingBackendConfig is the explicit, disabled-by-default coding backend
// policy. It can be embedded in Bee's main Config without changing existing
// local child behavior.
type CodingBackendConfig struct {
	Enabled bool `json:"enabled,omitempty"`

	DefaultImplementer string `json:"default_implementer,omitempty"`
	DefaultReviewer    string `json:"default_reviewer,omitempty"`
	DefaultScout       string `json:"default_scout,omitempty"`

	FallbackImplementer string `json:"fallback_implementer,omitempty"`
	FallbackReviewer    string `json:"fallback_reviewer,omitempty"`
	FallbackScout       string `json:"fallback_scout,omitempty"`

	Backends []CodingBackendDefinition `json:"backends,omitempty"`
	Limits   CodingBackendLimits       `json:"limits,omitempty"`

	// Defaults is an optional grouped spelling accepted for operator-facing
	// configs. Direct default_* fields take precedence when both are present.
	Defaults  CodingBackendRoleDefaults `json:"defaults,omitempty"`
	Fallbacks CodingBackendRoleDefaults `json:"fallbacks,omitempty"`
}

type CodingBackendRoleDefaults struct {
	Implementer string `json:"implementer,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	Scout       string `json:"scout,omitempty"`
}

// CodingBackendDefaults is a compatibility name for role defaults.
type CodingBackendDefaults = CodingBackendRoleDefaults

// DefaultCodingBackendDefinitions returns explicit, disabled-by-default
// backend definitions. Registry construction never launches adapter commands.
func DefaultCodingBackendDefinitions() []CodingBackendDefinition {
	return []CodingBackendDefinition{
		{
			Name:    CodingBackendNameLocalChild,
			Kind:    CodingBackendKindLocalChild,
			Enabled: false,
		},
	}
}

func DefaultCodingBackendConfig() CodingBackendConfig {
	return CodingBackendConfig{
		Enabled:             false,
		DefaultImplementer:  CodingBackendNameLocalChild,
		DefaultReviewer:     CodingBackendNameLocalChild,
		DefaultScout:        CodingBackendNameLocalChild,
		FallbackImplementer: CodingBackendNameLocalChild,
		FallbackReviewer:    CodingBackendNameLocalChild,
		FallbackScout:       CodingBackendNameLocalChild,
		Backends:            DefaultCodingBackendDefinitions(),
		Limits:              CodingBackendLimits{MaxWorktreeBytes: DefaultCodingMaxWorktreeBytes, MaxWorktreeFiles: DefaultCodingMaxWorktreeFiles, MaxArtifactBytes: DefaultCodingMaxArtifactBytes, MaxArtifactFiles: DefaultCodingMaxArtifactFiles},
	}
}

func (config CodingBackendConfig) effectiveFallback(role CodingBackendRole) string {
	switch role {
	case CodingRoleImplementer:
		if strings.TrimSpace(config.FallbackImplementer) != "" {
			return strings.TrimSpace(config.FallbackImplementer)
		}
		return strings.TrimSpace(config.Fallbacks.Implementer)
	case CodingRoleReviewer:
		if strings.TrimSpace(config.FallbackReviewer) != "" {
			return strings.TrimSpace(config.FallbackReviewer)
		}
		return strings.TrimSpace(config.Fallbacks.Reviewer)
	case CodingRoleScout:
		if strings.TrimSpace(config.FallbackScout) != "" {
			return strings.TrimSpace(config.FallbackScout)
		}
		return strings.TrimSpace(config.Fallbacks.Scout)
	default:
		return ""
	}
}

func (config CodingBackendConfig) EffectiveFallbacks() CodingBackendRoleDefaults {
	return CodingBackendRoleDefaults{
		Implementer: config.effectiveFallback(CodingRoleImplementer),
		Reviewer:    config.effectiveFallback(CodingRoleReviewer),
		Scout:       config.effectiveFallback(CodingRoleScout),
	}
}

func (config CodingBackendConfig) ResolveFallbackName(role CodingBackendRole, override string) (string, error) {
	if !role.valid() {
		return "", fmt.Errorf("coding backend role %q is invalid", role)
	}
	name := strings.TrimSpace(override)
	if name == "" {
		name = config.effectiveFallback(role)
	}
	if name == "" {
		return "", nil
	}
	if err := validateCodingBackendName(name); err != nil {
		return "", err
	}
	if name != CodingBackendNameLocalChild {
		return "", fmt.Errorf("coding fallback backend %q is not permitted; only local-child is supported", name)
	}
	return name, nil
}

func (config CodingBackendConfig) effectiveDefault(role CodingBackendRole) string {
	switch role {
	case CodingRoleImplementer:
		if strings.TrimSpace(config.DefaultImplementer) != "" {
			return strings.TrimSpace(config.DefaultImplementer)
		}
		return strings.TrimSpace(config.Defaults.Implementer)
	case CodingRoleReviewer:
		if strings.TrimSpace(config.DefaultReviewer) != "" {
			return strings.TrimSpace(config.DefaultReviewer)
		}
		return strings.TrimSpace(config.Defaults.Reviewer)
	case CodingRoleScout:
		if strings.TrimSpace(config.DefaultScout) != "" {
			return strings.TrimSpace(config.DefaultScout)
		}
		return strings.TrimSpace(config.Defaults.Scout)
	default:
		return ""
	}
}

func (config CodingBackendConfig) EffectiveDefaults() CodingBackendRoleDefaults {
	return CodingBackendRoleDefaults{
		Implementer: config.effectiveDefault(CodingRoleImplementer),
		Reviewer:    config.effectiveDefault(CodingRoleReviewer),
		Scout:       config.effectiveDefault(CodingRoleScout),
	}
}

func (config CodingBackendConfig) EffectiveLimits() CodingBackendLimits {
	return config.Limits.withDefaults()
}

func (config CodingBackendConfig) mergedLimits(backend CodingBackendLimits) CodingBackendLimits {
	effective := config.EffectiveLimits()
	if backend.MaxWorktreeBytes > 0 {
		effective.MaxWorktreeBytes = backend.MaxWorktreeBytes
	}
	if backend.MaxWorktreeFiles > 0 {
		effective.MaxWorktreeFiles = backend.MaxWorktreeFiles
	}
	if backend.MaxArtifactBytes > 0 {
		effective.MaxArtifactBytes = backend.MaxArtifactBytes
	}
	if backend.MaxArtifactFiles > 0 {
		effective.MaxArtifactFiles = backend.MaxArtifactFiles
	}
	return effective
}

func (config CodingBackendConfig) Validate() error {
	// Still reject malformed explicitly supplied entries, while permitting
	// disabled ACP definitions without a host-installed adapter command.
	if err := config.Limits.Validate(); err != nil {
		return fmt.Errorf("coding config limits: %w", err)
	}
	seen := make(map[string]struct{}, len(config.Backends))
	for _, backend := range config.Backends {
		if _, exists := seen[backend.Name]; exists {
			return fmt.Errorf("duplicate coding backend %q", backend.Name)
		}
		seen[backend.Name] = struct{}{}
		if err := backend.Validate(); err != nil {
			return err
		}
		if err := config.mergedLimits(backend.Limits).Validate(); err != nil {
			return fmt.Errorf("coding backend %q limits: %w", backend.Name, err)
		}
	}
	for role, name := range map[CodingBackendRole]string{
		CodingRoleImplementer: config.effectiveDefault(CodingRoleImplementer),
		CodingRoleReviewer:    config.effectiveDefault(CodingRoleReviewer),
		CodingRoleScout:       config.effectiveDefault(CodingRoleScout),
	} {
		if name == "" {
			if config.Enabled {
				return fmt.Errorf("coding config default %s backend is required when enabled", role)
			}
			continue
		}
		if err := validateCodingBackendName(name); err != nil {
			return fmt.Errorf("coding config default %s: %w", role, err)
		}
		if config.Enabled {
			if _, known := seen[name]; !known && name != CodingBackendNameLocalChild {
				return fmt.Errorf("coding config default %s backend %q is not configured", role, name)
			}
		}
	}
	for role, name := range map[CodingBackendRole]string{
		CodingRoleImplementer: config.effectiveFallback(CodingRoleImplementer),
		CodingRoleReviewer:    config.effectiveFallback(CodingRoleReviewer),
		CodingRoleScout:       config.effectiveFallback(CodingRoleScout),
	} {
		if name == "" {
			continue
		}
		if err := validateCodingBackendName(name); err != nil {
			return fmt.Errorf("coding config fallback %s: %w", role, err)
		}
		if name != CodingBackendNameLocalChild {
			return fmt.Errorf("coding config fallback %s backend %q is not permitted; only local-child is supported", role, name)
		}
	}
	return nil
}

func (config CodingBackendConfig) backend(name string) (CodingBackendDefinition, bool) {
	for _, definition := range config.Backends {
		if definition.Name == name {
			return definition, true
		}
	}
	return CodingBackendDefinition{}, false
}

func (config CodingBackendConfig) ResolveName(role CodingBackendRole, override string) (string, error) {
	if !role.valid() {
		return "", fmt.Errorf("coding backend role %q is invalid", role)
	}
	name := strings.TrimSpace(override)
	if name == "" {
		name = config.effectiveDefault(role)
	}
	if name == "" {
		return "", fmt.Errorf("coding backend for role %q is not configured", role)
	}
	if err := validateCodingBackendName(name); err != nil {
		return "", err
	}
	return name, nil
}

func validateCodingBackendName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("coding backend name is required")
	}
	if len(name) > 64 {
		return errors.New("coding backend name exceeds 64 bytes")
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return fmt.Errorf("coding backend name %q contains an unsafe character", name)
		}
	}
	return nil
}

// CodingBackendFactory constructs a backend from one explicit definition.
// Construction must be side-effect free; adapter processes start only when
// ACPBackend.Run is called.
type CodingBackendFactory func(CodingBackendDefinition) (CodingBackend, error)

// CodingBackendRoute is the parent-owned result of role routing. A disabled
// route is valid and intentionally carries no backend instance.
type CodingBackendRoute struct {
	Role        CodingBackendRole `json:"role"`
	BackendName string            `json:"backend"`
	Enabled     bool              `json:"enabled"`
	Backend     CodingBackend     `json:"-"`
}

// CodingBackendRegistry validates explicit definitions, builds ACP backends,
// and permits the existing local child path to be registered by the
// composition root without making it an ACP process.
type CodingBackendRegistry struct {
	config    CodingBackendConfig
	factories map[string]CodingBackendFactory
}

func NewCodingBackendRegistry(config CodingBackendConfig) (*CodingBackendRegistry, error) {
	return NewCodingBackendRegistryWithFactories(config, nil)
}

func NewCodingBackendRegistryWithFactories(config CodingBackendConfig, factories map[string]CodingBackendFactory) (*CodingBackendRegistry, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	registry := &CodingBackendRegistry{config: config, factories: make(map[string]CodingBackendFactory, len(factories))}
	for name, factory := range factories {
		if err := registry.Register(name, factory); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (registry *CodingBackendRegistry) Config() CodingBackendConfig {
	if registry == nil {
		return CodingBackendConfig{}
	}
	return registry.config
}

func (registry *CodingBackendRegistry) Register(name string, factory CodingBackendFactory) error {
	if registry == nil {
		return errors.New("coding backend registry is nil")
	}
	if err := validateCodingBackendName(name); err != nil {
		return err
	}
	if factory == nil {
		return fmt.Errorf("coding backend factory %q is nil", name)
	}
	registry.factories[name] = factory
	return nil
}

func (registry *CodingBackendRegistry) Build(name string) (CodingBackend, error) {
	if registry == nil {
		return nil, errors.New("coding backend registry is nil")
	}
	name = strings.TrimSpace(name)
	if err := validateCodingBackendName(name); err != nil {
		return nil, err
	}
	if !registry.config.Enabled {
		return nil, ErrCodingBackendsDisabled
	}
	definition, ok := registry.config.backend(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCodingBackendNotFound, name)
	}
	if !definition.Enabled {
		return nil, newCodingBackendFailure(BackendFailureUnavailable, fmt.Errorf("coding backend %q is disabled", name))
	}
	definition.Limits = registry.config.mergedLimits(definition.Limits)
	factory := registry.factories[name]
	if factory == nil {
		kind := strings.TrimSpace(definition.Kind)
		if kind == "" || kind == CodingBackendKindACP {
			factory = newACPBackendFactory
		}
	}
	if factory == nil {
		return nil, fmt.Errorf("%w: %s has no registered factory", ErrCodingBackendUnavailable, name)
	}
	backend, err := factory(definition)
	if err != nil {
		return nil, newCodingBackendFailure(BackendFailureUnavailable, fmt.Errorf("construct coding backend %q: %w", name, err))
	}
	if backend == nil {
		return nil, fmt.Errorf("construct coding backend %q: factory returned nil", name)
	}
	return backend, nil
}

// NewACPBackendFromDefinition constructs an ACP backend without starting its
// process. The definition must carry the adapter command explicitly.
func NewACPBackendFromDefinition(definition CodingBackendDefinition) (*ACPBackend, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	backend, err := NewACPBackend(definition.processConfig())
	if err != nil {
		return nil, err
	}
	backend.BackendName = definition.Name
	backend.Limits = definition.Limits.withDefaults()
	return backend, nil
}

func newACPBackendFactory(definition CodingBackendDefinition) (CodingBackend, error) {
	backend, err := NewACPBackendFromDefinition(definition)
	if err != nil {
		return nil, err
	}
	return backend, nil
}

// BackendForRole is a provider-neutral convenience alias for Resolve.
func (registry *CodingBackendRegistry) BackendForRole(role CodingBackendRole, override string) (CodingBackend, error) {
	return registry.Resolve(role, override)
}

// RouteForRole is a provider-neutral convenience alias for Route.
func (registry *CodingBackendRegistry) RouteForRole(role CodingBackendRole, override string) (CodingBackendRoute, error) {
	return registry.Route(role, override)
}

// Route resolves an explicit role and optional user override. It does not
// launch a process. When the whole coding feature is disabled, it returns a
// valid disabled route so existing local child orchestration can continue.
func (registry *CodingBackendRegistry) Route(role CodingBackendRole, override string) (CodingBackendRoute, error) {
	if registry == nil {
		return CodingBackendRoute{}, errors.New("coding backend registry is nil")
	}
	name, err := registry.config.ResolveName(role, override)
	if err != nil {
		return CodingBackendRoute{}, err
	}
	route := CodingBackendRoute{Role: role, BackendName: name, Enabled: registry.config.Enabled}
	if !registry.config.Enabled {
		return route, nil
	}
	backend, err := registry.Build(name)
	if err != nil {
		return CodingBackendRoute{}, err
	}
	route.Backend = backend
	return route, nil
}

func (registry *CodingBackendRegistry) Resolve(role CodingBackendRole, override string) (CodingBackend, error) {
	route, err := registry.Route(role, override)
	if err != nil {
		return nil, err
	}
	if !route.Enabled || route.Backend == nil {
		return nil, ErrCodingBackendsDisabled
	}
	return route.Backend, nil
}
