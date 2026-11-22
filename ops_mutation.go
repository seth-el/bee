package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	OpsOperationClassSSHVMService = "ssh_vm_service"
	OpsOperationClassSSHVMDeploy  = "ssh_vm_deploy"
)

var (
	ErrOpsMutationGrantRequired = errors.New("a parent-owned mutation grant and approval are required")
	ErrOpsMutationGrantExpired  = errors.New("operations mutation grant is expired")
	ErrOpsMutationGrantReused   = errors.New("operations mutation grant has already been used")
	ErrOpsMutationStdinDenied   = errors.New("operations mutation stdin was not explicitly granted")
	ErrOpsMutationCommandDenied = errors.New("operations mutation command is not permitted")
)

// OpsMutationGrant is parent-owned authority. It is deliberately not a field
// on OpsTask or OpsACPPacket: grants never cross the model/ACP boundary.
// Credentials, environment, kubeconfig, and memory are intentionally absent.
type OpsMutationGrant struct {
	GrantID           string          `json:"grant_id"`
	ApprovalID        string          `json:"approval_id"`
	ApprovalReason    string          `json:"approval_reason"`
	Target            OpsTarget       `json:"target"`
	Capabilities      []OpsCapability `json:"capabilities"`
	OperationClasses  []string        `json:"operation_classes"`
	ExpiresAt         time.Time       `json:"expires_at,omitempty"`
	OneShot           bool            `json:"one_shot,omitempty"`
	MaxTimeoutSeconds int             `json:"max_timeout_seconds"`
	MaxCommands       int             `json:"max_commands"`
	MaxSteps          int             `json:"max_steps"`
	MaxOutputBytes    int             `json:"max_output_bytes"`
	AllowStdin        bool            `json:"allow_stdin,omitempty"`

	mu   sync.Mutex
	used bool
}

// OpsMutationPreview is parent-facing dry-run evidence for a future approval
// UI. It contains the exact argv and bounded ceilings, but not credentials or
// any inherited authority.
type OpsMutationPreview struct {
	DryRun         bool      `json:"dry_run"`
	Target         OpsTarget `json:"target"`
	Argv           []string  `json:"argv"`
	OperationClass string    `json:"operation_class"`
	GrantID        string    `json:"grant_id"`
	ApprovalID     string    `json:"approval_id"`
	ApprovalReason string    `json:"approval_reason"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	OneShot        bool      `json:"one_shot,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds"`
	MaxCommands    int       `json:"max_commands"`
	MaxSteps       int       `json:"max_steps"`
	MaxOutputBytes int       `json:"max_output_bytes"`
	StdinAllowed   bool      `json:"stdin_allowed,omitempty"`
}

func (grant *OpsMutationGrant) Validate() error {
	return grant.validateAt(time.Now().UTC(), false)
}

func (grant *OpsMutationGrant) validateAt(now time.Time, checkUse bool) error {
	if grant == nil {
		return ErrOpsMutationGrantRequired
	}
	if err := validateChildID("operations mutation grant_id", grant.GrantID); err != nil {
		return err
	}
	if err := validateChildID("operations mutation approval_id", grant.ApprovalID); err != nil {
		return err
	}
	if strings.TrimSpace(grant.ApprovalReason) == "" || len(grant.ApprovalReason) > 1024 {
		return errors.New("operations mutation approval reason is required and bounded")
	}
	if err := grant.Target.Validate(); err != nil {
		return fmt.Errorf("operations mutation target: %w", err)
	}
	if grant.Target.Kind != OpsTargetSSHVM {
		return errors.New("operations mutation grant currently supports SSH/VM targets only")
	}
	caps, err := canonicalOpsCapabilities(grant.Capabilities)
	if err != nil {
		return err
	}
	for _, required := range []OpsCapability{OpsCapabilityExecute, OpsCapabilityMutate, OpsCapabilityRollback} {
		if !hasOpsCapability(caps, required) {
			return fmt.Errorf("operations mutation grant requires %q capability", required)
		}
	}
	if len(grant.OperationClasses) == 0 || len(grant.OperationClasses) > 8 {
		return errors.New("operations mutation grant requires bounded operation classes")
	}
	seen := make(map[string]struct{}, len(grant.OperationClasses))
	for _, class := range grant.OperationClasses {
		if !mutationOperationClassPattern.MatchString(class) {
			return fmt.Errorf("operations mutation operation class %q is invalid", class)
		}
		if _, ok := seen[class]; ok {
			return fmt.Errorf("operations mutation operation class %q repeats", class)
		}
		seen[class] = struct{}{}
	}
	if grant.ExpiresAt.IsZero() && !grant.OneShot {
		return errors.New("operations mutation grant requires expiry or one-shot semantics")
	}
	if !grant.ExpiresAt.IsZero() && !grant.ExpiresAt.After(now) {
		return ErrOpsMutationGrantExpired
	}
	if grant.MaxTimeoutSeconds <= 0 || grant.MaxTimeoutSeconds > defaultOpsMaxTimeoutSeconds || grant.MaxCommands <= 0 || grant.MaxCommands > defaultOpsMaxCommands || grant.MaxSteps <= 0 || grant.MaxSteps > defaultOpsMaxSteps || grant.MaxOutputBytes <= 0 || grant.MaxOutputBytes > defaultOpsMaxOutputBytes {
		return errors.New("operations mutation grant budgets are out of bounds")
	}
	if checkUse && grant.OneShot && grant.used {
		return ErrOpsMutationGrantReused
	}
	return nil
}

func (grant *OpsMutationGrant) authorize(task OpsTask, consume bool) (OpsMutationPreview, error) {
	if grant == nil {
		return OpsMutationPreview{}, ErrOpsMutationGrantRequired
	}
	grant.mu.Lock()
	defer grant.mu.Unlock()
	if err := grant.validateAt(time.Now().UTC(), true); err != nil {
		return OpsMutationPreview{}, err
	}
	if task.Intent != OpsIntentMutating {
		return OpsMutationPreview{}, errors.New("mutation grant cannot authorize read-only task")
	}
	if err := task.Validate(); err != nil {
		return OpsMutationPreview{}, err
	}
	if !sameOpsTarget(grant.Target, task.Target) {
		return OpsMutationPreview{}, ErrOpsTargetMismatch
	}
	command := normalizeOpsCommand(task.Command)
	class, err := validateMutatingSSHCommand(command, task.OperationClass)
	if err != nil {
		return OpsMutationPreview{}, err
	}
	if !opsMutationContainsString(grant.OperationClasses, class) {
		return OpsMutationPreview{}, fmt.Errorf("%w: operation class %q is not approved", ErrOpsMutationCommandDenied, class)
	}
	if task.Stdin != "" && !grant.AllowStdin {
		return OpsMutationPreview{}, ErrOpsMutationStdinDenied
	}
	timeoutSeconds := task.Budget.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = grant.MaxTimeoutSeconds
	}
	maxCommands := task.Budget.MaxCommands
	if maxCommands <= 0 {
		maxCommands = grant.MaxCommands
	}
	maxSteps := task.Budget.MaxSteps
	if maxSteps <= 0 {
		maxSteps = grant.MaxSteps
	}
	maxOutputBytes := task.Budget.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = grant.MaxOutputBytes
	}
	if timeoutSeconds > grant.MaxTimeoutSeconds || maxCommands > grant.MaxCommands || len(command) > maxCommands || maxSteps > grant.MaxSteps || maxOutputBytes > grant.MaxOutputBytes {
		return OpsMutationPreview{}, errors.New("operations mutation task exceeds grant budget")
	}
	preview := OpsMutationPreview{DryRun: true, Target: task.Target, Argv: append([]string(nil), command...), OperationClass: class, GrantID: grant.GrantID, ApprovalID: grant.ApprovalID, ApprovalReason: truncateText(grant.ApprovalReason, 1024), ExpiresAt: grant.ExpiresAt, OneShot: grant.OneShot, TimeoutSeconds: timeoutSeconds, MaxCommands: maxCommands, MaxSteps: maxSteps, MaxOutputBytes: maxOutputBytes, StdinAllowed: grant.AllowStdin}
	if consume && grant.OneShot {
		grant.used = true
	}
	return preview, nil
}

// PreviewOpsMutation validates an approved mutation without consuming a
// one-shot grant or invoking any runner.
func PreviewOpsMutation(task OpsTask, grant *OpsMutationGrant) (OpsMutationPreview, error) {
	return grant.authorize(task, false)
}

func (grant *OpsMutationGrant) consume(task OpsTask) (OpsMutationPreview, error) {
	return grant.authorize(task, true)
}

func sameOpsTarget(a, b OpsTarget) bool {
	left, errLeft := canonicalOpsTarget(a)
	right, errRight := canonicalOpsTarget(b)
	if errLeft != nil || errRight != nil {
		return false
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func opsMutationContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var mutationServiceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,127}$`)
var mutationOperationClassPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validateMutatingSSHCommand(command []string, requestedClass string) (string, error) {
	if len(command) == 0 {
		return "", ErrOpsMutationCommandDenied
	}
	for _, arg := range command {
		if strings.ContainsAny(arg, ";|&><$`(){}[]\\\n\r\x00") {
			return "", fmt.Errorf("%w: shell syntax is not allowed", ErrOpsMutationCommandDenied)
		}
	}
	base := command[0]
	class := ""
	switch base {
	case "systemctl":
		class = OpsOperationClassSSHVMService
		if len(command) == 2 && command[1] == "daemon-reload" {
			break
		}
		if len(command) != 3 || !opsMutationContainsString([]string{"start", "stop", "restart", "enable", "disable"}, command[1]) || !mutationServiceNamePattern.MatchString(command[2]) {
			return "", fmt.Errorf("%w: systemctl allows one bounded service action", ErrOpsMutationCommandDenied)
		}
	case "docker", "podman":
		class = OpsOperationClassSSHVMDeploy
		if len(command) != 3 || !opsMutationContainsString([]string{"start", "stop", "restart"}, command[1]) || !mutationServiceNamePattern.MatchString(command[2]) {
			return "", fmt.Errorf("%w: container command allows one bounded lifecycle action", ErrOpsMutationCommandDenied)
		}
	default:
		return "", fmt.Errorf("%w: executable %q is not allowlisted", ErrOpsMutationCommandDenied, base)
	}
	if requestedClass != "" && requestedClass != class {
		return "", fmt.Errorf("%w: requested operation class %q does not match %q", ErrOpsMutationCommandDenied, requestedClass, class)
	}
	return class, nil
}
