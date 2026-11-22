package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ACPProtocolVersion is Bee's provider-neutral ACP integration seam. The
// transport intentionally keeps the provider payload opaque so ACP agents can
// evolve behind the same parent-owned boundary.
const ACPProtocolVersion uint16 = 1

const (
	ACPMethodInitialize        = "initialize"
	ACPMethodAuthenticate      = "authenticate"
	ACPMethodNewSession        = "session/new"
	ACPMethodPrompt            = "session/prompt"
	ACPMethodUpdate            = "session/update"
	ACPMethodCancel            = "session/cancel"
	ACPMethodRequestPermission = "session/request_permission"
	ACPMethodShutdown          = "shutdown"
)

const (
	ACPAuthMethodAPIKey = "api-key"
	ACPAuthEnvOpenAI    = "OPENAI_API_KEY"
)

// ACPAuthMethod describes an authentication method advertised by the agent
// during initialize. Bee only supports explicitly configured methods.
type ACPAuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ACPAPIKeyAuth and ACPAuthenticateParams are intentionally scoped to the
// ACP authenticate request. Credentials never appear in CodingBackendTask,
// completion packets, events, or artifacts.
type ACPAPIKeyAuth struct {
	APIKey string `json:"apiKey"`
}

type ACPAuthenticateMeta struct {
	APIKey ACPAPIKeyAuth `json:"api-key"`
}

type ACPAuthenticateParams struct {
	MethodID string              `json:"methodId"`
	Meta     ACPAuthenticateMeta `json:"_meta"`
}

func (params ACPAuthenticateParams) Validate() error {
	switch params.MethodID {
	case ACPAuthMethodAPIKey:
		if strings.TrimSpace(params.Meta.APIKey.APIKey) == "" {
			return errors.New("ACP api-key authentication secret is required")
		}
	default:
		return fmt.Errorf("unsupported ACP authentication method %q", params.MethodID)
	}
	return nil
}

// ACPPermissionOption and the surrounding types mirror the standard ACP
// session/request_permission wire shape. ToolCall remains opaque: Bee never
// interprets it as executable authority.
type ACPPermissionOption struct {
	OptionID    string `json:"optionId"`
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type ACPPermissionRequest struct {
	SessionID string                `json:"sessionId"`
	ToolCall  json.RawMessage       `json:"toolCall"`
	Options   []ACPPermissionOption `json:"options"`
}

func (request ACPPermissionRequest) Validate() error {
	if strings.TrimSpace(request.SessionID) == "" {
		return errors.New("ACP permission session id is required")
	}
	if len(request.ToolCall) > defaultACPMaxMessageSize {
		return errors.New("ACP permission tool call exceeds max message size")
	}
	if len(request.Options) == 0 {
		return errors.New("ACP permission options are required")
	}
	for index, option := range request.Options {
		if strings.TrimSpace(option.OptionID) == "" {
			return fmt.Errorf("ACP permission option %d id is required", index)
		}
		if strings.TrimSpace(option.Kind) == "" {
			return fmt.Errorf("ACP permission option %d kind is required", index)
		}
	}
	return nil
}

type ACPPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type ACPPermissionResponse struct {
	Outcome ACPPermissionOutcome `json:"outcome"`
}

// ACPPermissionPolicy is parent-owned. Auto-approval is deliberately a
// two-key capability: the backend configuration and the individual task must
// both opt in. Reviewers/scouts and tasks without an isolated lease fail
// closed even when a backend has auto approval enabled.
type ACPPermissionPolicy struct {
	AutoApprovePermissions bool              `json:"auto_approve_permissions,omitempty"`
	AutoApprove            bool              `json:"auto_approve,omitempty"` // compatibility spelling
	Role                   CodingBackendRole `json:"role,omitempty"`
}

func (policy ACPPermissionPolicy) autoApproveEnabled() bool {
	return policy.AutoApprovePermissions || policy.AutoApprove
}

type ACPPermissionHandler interface {
	HandleACPPermission(context.Context, ACPPermissionRequest) ACPPermissionResponse
}

type ACPPermissionHandlerFunc func(context.Context, ACPPermissionRequest) ACPPermissionResponse

func (fn ACPPermissionHandlerFunc) HandleACPPermission(ctx context.Context, request ACPPermissionRequest) ACPPermissionResponse {
	return fn(ctx, request)
}

// NewBoundedACPPermissionHandler returns the only built-in handler capable of
// selecting an allow option. It selects a valid one-time option from the
// agent-provided list and never fabricates an optionId.
func NewBoundedACPPermissionHandler(config ACPProcessConfig, task CodingBackendTask) ACPPermissionHandler {
	return ACPPermissionHandlerFunc(func(_ context.Context, request ACPPermissionRequest) ACPPermissionResponse {
		cancel := ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "cancelled"}}
		if !config.AutoApprovePermissions || !taskPermissionAutoApprove(task) || !acpTaskMayAutoApprove(task) {
			return cancel
		}
		if task.Role == CodingRoleReviewer || task.Role == CodingRoleScout || task.PermissionPolicy.Role == CodingRoleReviewer || task.PermissionPolicy.Role == CodingRoleScout {
			return cancel
		}
		if err := request.Validate(); err != nil {
			return cancel
		}
		for _, preferredKind := range []string{"allow_once", "proceed_once"} {
			for _, option := range request.Options {
				if strings.EqualFold(strings.TrimSpace(option.Kind), preferredKind) && strings.TrimSpace(option.OptionID) != "" {
					return ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "selected", OptionID: option.OptionID}}
				}
			}
		}
		return cancel
	})
}

func taskPermissionAutoApprove(task CodingBackendTask) bool {
	return task.PermissionPolicy.autoApproveEnabled() || task.AutoApprovePermissions
}

func acpTaskMayAutoApprove(task CodingBackendTask) bool {
	if task.Task.Mutability != TaskMutable || task.Workspace == nil {
		return false
	}
	lease := task.Workspace
	if strings.TrimSpace(lease.WorkspacePath) == "" || strings.TrimSpace(lease.ScopePath) == "" || filepath.Clean(lease.WorkspacePath) != filepath.Clean(lease.ScopePath) {
		return false
	}
	parentRoot := task.ParentScopeRoot
	if strings.TrimSpace(parentRoot) == "" {
		parentRoot = task.Task.Scope.Root
	}
	parentRoot, err := filepath.Abs(parentRoot)
	if err != nil {
		return false
	}
	workspaceRoot, err := filepath.Abs(lease.WorkspacePath)
	if err != nil {
		return false
	}
	return filepath.Clean(parentRoot) != filepath.Clean(workspaceRoot)
}

// CodingBackendTask is the provider-neutral authorization contract for one
// supervised coding backend run. ChildTask remains the source of truth for
// identity, scope, budgets, tools, artifacts, and acceptance criteria.
type CodingBackendTask struct {
	Task                   ChildTask           `json:"task"`
	Plan                   []string            `json:"plan,omitempty"`
	Backend                string              `json:"backend,omitempty"`
	Workspace              *WorkspaceLease     `json:"workspace,omitempty"`
	Role                   CodingBackendRole   `json:"role,omitempty"`
	PermissionPolicy       ACPPermissionPolicy `json:"permission_policy,omitempty"`
	AutoApprovePermissions bool                `json:"auto_approve_permissions,omitempty"`
	ParentScopeRoot        string              `json:"parent_scope_root,omitempty"`
}

func (task CodingBackendTask) Validate() error {
	if err := task.Task.Validate(); err != nil {
		return fmt.Errorf("coding backend task: %w", err)
	}
	if task.Role != "" && !task.Role.valid() {
		return fmt.Errorf("coding backend task role %q is invalid", task.Role)
	}
	for i, item := range task.Plan {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("coding backend plan item %d is empty", i)
		}
	}
	if task.Workspace == nil {
		return nil
	}
	lease := task.Workspace
	if lease.TaskID != task.Task.TaskID || lease.ScopePath == "" {
		return errors.New("coding backend workspace lease does not match task")
	}
	parentScopeRoot := task.ParentScopeRoot
	if strings.TrimSpace(parentScopeRoot) == "" {
		parentScopeRoot = task.Task.Scope.Root
	}
	parentScope, err := filepath.Abs(parentScopeRoot)
	if err != nil {
		return fmt.Errorf("resolve coding backend task scope: %w", err)
	}
	leasedScope, err := filepath.Abs(lease.ScopePath)
	if err != nil {
		return fmt.Errorf("resolve coding backend leased scope: %w", err)
	}
	if strings.TrimSpace(lease.WorkspacePath) != "" {
		workspacePath, pathErr := filepath.Abs(lease.WorkspacePath)
		if pathErr != nil || filepath.Clean(workspacePath) != filepath.Clean(leasedScope) {
			return errors.New("coding backend leased scope must match workspace path")
		}
		if strings.TrimSpace(lease.RepositoryRoot) == "" {
			return errors.New("coding backend external workspace lease requires repository root")
		}
		repositoryRoot, pathErr := filepath.Abs(lease.RepositoryRoot)
		if pathErr != nil || !scopeWithin(repositoryRoot, parentScope) {
			return errors.New("coding backend workspace repository must contain task scope")
		}
		if filepath.Clean(parentScope) == filepath.Clean(leasedScope) {
			return errors.New("coding backend workspace lease must isolate task scope")
		}
	} else if !scopeWithin(parentScope, leasedScope) {
		return errors.New("coding backend leased scope must remain within task scope")
	}
	if lease.ChildArtifactDir != "" {
		artifact, err := filepath.Abs(lease.ChildArtifactDir)
		if err != nil || !scopeWithin(leasedScope, artifact) || filepath.Clean(artifact) == filepath.Clean(leasedScope) {
			return errors.New("coding backend leased artifact directory must remain within leased scope")
		}
	}
	return nil
}

// EffectiveTask translates an optional workspace lease into the child task
// sent to the ACP process. This is deliberately a copy; the parent contract is
// never mutated by an adapter.
func (task CodingBackendTask) EffectiveTask() (ChildTask, error) {
	if err := task.Validate(); err != nil {
		return ChildTask{}, err
	}
	effective := task.Task
	if task.Workspace != nil {
		effective.Scope.Root = task.Workspace.ScopePath
		effective.ArtifactDir = task.Workspace.ChildArtifactDir
	}
	if err := effective.Validate(); err != nil {
		return ChildTask{}, fmt.Errorf("effective coding backend task: %w", err)
	}
	return effective, nil
}

// CodingBackendEvent is a bounded observation from an ACP process. Payload is
// provider-specific and never becomes executable authority in Bee.
type CodingBackendEvent struct {
	TaskID     string                 `json:"task_id"`
	ParentID   string                 `json:"parent_id,omitempty"`
	SessionID  string                 `json:"session_id,omitempty"`
	Type       string                 `json:"type"`
	Text       string                 `json:"text,omitempty"`
	Info       string                 `json:"info,omitempty"`
	Payload    json.RawMessage        `json:"payload,omitempty"`
	Artifact   string                 `json:"artifact,omitempty"`
	Diff       string                 `json:"diff,omitempty"`
	Completion *ChildCompletionPacket `json:"completion,omitempty"`
	At         time.Time              `json:"at"`
}

func (event CodingBackendEvent) Validate() error {
	if err := validateChildID("task_id", event.TaskID); err != nil {
		return err
	}
	if strings.TrimSpace(event.Type) == "" {
		return errors.New("coding backend event type is required")
	}
	if len(event.Payload) > 1<<20 {
		return errors.New("coding backend event payload exceeds 1 MiB")
	}
	if event.Completion != nil {
		if err := event.Completion.Validate(); err != nil {
			return fmt.Errorf("coding backend event completion: %w", err)
		}
	}
	return nil
}

type CodingBackendObserver interface {
	ObserveCodingBackendEvent(CodingBackendEvent)
}

type codingBackendObserverFunc func(CodingBackendEvent)

func (fn codingBackendObserverFunc) ObserveCodingBackendEvent(event CodingBackendEvent) { fn(event) }

// CodingEvidence is review-only evidence. It contains paths and bounded
// summaries, never authority to apply or merge a change.
type CodingEvidence struct {
	ChangedFiles     []string `json:"changed_files,omitempty"`
	UntrackedFiles   []string `json:"untracked_files,omitempty"`
	DiffArtifact     string   `json:"diff_artifact,omitempty"`
	ProposalArtifact string   `json:"proposal_artifact,omitempty"`
	Artifacts        []string `json:"artifacts,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

func (evidence CodingEvidence) Validate() error {
	for label, values := range map[string][]string{
		"changed_files":   evidence.ChangedFiles,
		"untracked_files": evidence.UntrackedFiles,
		"artifacts":       evidence.Artifacts,
		"notes":           evidence.Notes,
	} {
		if len(values) > 256 {
			return fmt.Errorf("coding evidence %s exceeds 256 entries", label)
		}
		for _, value := range values {
			if len(value) > 4096 {
				return fmt.Errorf("coding evidence %s entry exceeds 4096 bytes", label)
			}
		}
	}
	for label, value := range map[string]string{"diff_artifact": evidence.DiffArtifact, "proposal_artifact": evidence.ProposalArtifact} {
		if len(value) > 4096 {
			return fmt.Errorf("coding evidence %s exceeds 4096 bytes", label)
		}
	}
	return nil
}

// CodingEvidenceCollector lets a parent attach workspace/diff evidence after
// a backend run. WorkspaceManager is the standard implementation seam.
type CodingEvidenceCollector interface {
	CollectCodingEvidence(context.Context, CodingBackendTask, ChildCompletionPacket) (CodingEvidence, error)
}

type CodingEvidenceCollectorFunc func(context.Context, CodingBackendTask, ChildCompletionPacket) (CodingEvidence, error)

func (fn CodingEvidenceCollectorFunc) CollectCodingEvidence(ctx context.Context, task CodingBackendTask, packet ChildCompletionPacket) (CodingEvidence, error) {
	return fn(ctx, task, packet)
}

// WorkspaceCodingEvidenceCollector adapts the existing parent-owned workspace
// lease manager into the ACP completion/evidence seam.
type WorkspaceCodingEvidenceCollector struct {
	Manager WorkspaceManager
}

func (collector WorkspaceCodingEvidenceCollector) CollectCodingEvidence(ctx context.Context, task CodingBackendTask, packet ChildCompletionPacket) (CodingEvidence, error) {
	if collector.Manager == nil || task.Workspace == nil {
		return CodingEvidence{}, nil
	}
	proposal, err := collector.Manager.CollectProposal(ctx, *task.Workspace, packet)
	if err != nil {
		return CodingEvidence{DiffArtifact: proposal.DiffArtifact, ProposalArtifact: proposal.ProposalArtifact, Artifacts: proposal.Artifacts, Notes: proposal.Errors}, err
	}
	return CodingEvidence{ChangedFiles: append([]string(nil), proposal.ChangedFiles...), UntrackedFiles: append([]string(nil), proposal.UntrackedFiles...), DiffArtifact: proposal.DiffArtifact, ProposalArtifact: proposal.ProposalArtifact, Artifacts: append([]string(nil), proposal.Artifacts...), Notes: append([]string(nil), proposal.Errors...)}, nil
}

// CodingBackendCompletion is the normalized terminal result of one coding
// backend. ChildCompletionPacket is embedded to preserve existing orchestration
// and completion.json consumers.
type CodingBackendCompletion struct {
	ChildCompletionPacket
	Backend         string                   `json:"backend,omitempty"`
	SessionID       string                   `json:"session_id,omitempty"`
	FailureKind     CodingBackendFailureKind `json:"failure_kind,omitempty"`
	BackendEvidence CodingEvidence           `json:"backend_evidence,omitempty"`
}

func (completion CodingBackendCompletion) Validate() error {
	if err := completion.ChildCompletionPacket.Validate(); err != nil {
		return err
	}
	if err := completion.BackendEvidence.Validate(); err != nil {
		return err
	}
	return nil
}

type CodingBackend interface {
	Run(context.Context, CodingBackendTask, CodingBackendObserver) (CodingBackendCompletion, error)
}
