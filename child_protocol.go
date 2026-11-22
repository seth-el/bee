package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ChildProtocolVersion identifies the isolated-process child wire contract.
// It is intentionally distinct from both the Phase 4 client RPC and the
// Phase 5 extension protocol.
const ChildProtocolVersion = "bee.child.v1"

const (
	ChildMethodInitialize    = "initialize"
	ChildMethodTask          = "task"
	ChildMethodShutdown      = "shutdown"
	ChildMethodCancelRequest = "$/cancelRequest"
	ChildMethodEvent         = "event"
	ChildMethodProvider      = "provider/stream"
	ChildMethodApproval      = "approval/request"
)

const (
	ChildRPCParseError     = -32700
	ChildRPCInvalidRequest = -32600
	ChildRPCMethodNotFound = -32601
	ChildRPCInvalidParams  = -32602
	ChildRPCInternalError  = -32603
	ChildRPCServerError    = -32000
)

// ChildProcessConfig controls one explicitly configured child executable.
// Empty limits select conservative defaults. Environment values are copied
// only for the named allowlist; secret-shaped names are rejected.
type ChildProcessConfig struct {
	Command               string   `json:"command"`
	Args                  []string `json:"args,omitempty"`
	WorkingDir            string   `json:"working_dir,omitempty"`
	EnvAllowlist          []string `json:"env_allowlist,omitempty"`
	StartupTimeoutSeconds int      `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes         int      `json:"max_frame_bytes,omitempty"`
	MaxMessageBytes       int      `json:"max_message_bytes,omitempty"`
}

func (config ChildProcessConfig) Validate() error {
	if strings.TrimSpace(config.Command) == "" {
		return errors.New("child process command is required")
	}
	if strings.TrimSpace(config.WorkingDir) == "" && config.WorkingDir != "" {
		return errors.New("child process working_dir must not be blank")
	}
	if config.StartupTimeoutSeconds < 0 || config.RequestTimeoutSeconds < 0 {
		return errors.New("child process timeouts must not be negative")
	}
	if config.MaxFrameBytes < 0 || config.MaxMessageBytes < 0 {
		return errors.New("child process limits must not be negative")
	}
	if config.MaxFrameBytes > 0 && config.MaxMessageBytes > 0 && config.MaxFrameBytes > config.MaxMessageBytes {
		return errors.New("child process max_frame_bytes must not exceed max_message_bytes")
	}
	seen := make(map[string]struct{}, len(config.EnvAllowlist))
	for _, key := range config.EnvAllowlist {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\r\n") {
			return fmt.Errorf("child process has invalid environment key %q", key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("child process repeats environment key %q", key)
		}
		if secretEnvironmentKey(key) {
			return fmt.Errorf("child process refuses secret-shaped environment key %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (config ChildProcessConfig) startupTimeout() time.Duration {
	if config.StartupTimeoutSeconds <= 0 {
		return defaultChildStartupTimeout
	}
	return time.Duration(config.StartupTimeoutSeconds) * time.Second
}

func (config ChildProcessConfig) requestTimeout() time.Duration {
	if config.RequestTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(config.RequestTimeoutSeconds) * time.Second
}

func (config ChildProcessConfig) maxFrameBytes() int {
	if config.MaxFrameBytes <= 0 {
		return defaultChildMaxFrameBytes
	}
	return config.MaxFrameBytes
}

func (config ChildProcessConfig) maxMessageBytes() int {
	if config.MaxMessageBytes <= 0 {
		return defaultChildMaxMessageSize
	}
	return config.MaxMessageBytes
}

// ChildRuntimeConfig is the minimal child configuration. APIKeyFile is always
// blank on the wire; provider credentials stay in the parent broker.
type ChildRuntimeConfig struct {
	Provider    ProviderConfig   `json:"provider"`
	Generation  GenerationConfig `json:"generation"`
	Agent       AgentConfig      `json:"agent"`
	RequirePlan bool             `json:"require_plan,omitempty"`
}

func (config ChildRuntimeConfig) fullConfig() (Config, error) {
	if config.Provider.APIKeyFile != "" {
		return Config{}, errors.New("child runtime config must not contain provider credentials")
	}
	full := Config{Provider: config.Provider, Generation: config.Generation, Agent: config.Agent}
	if err := full.Validate(); err != nil {
		return Config{}, err
	}
	return full, nil
}

type ChildInitializeParams struct {
	Protocol string             `json:"protocol"`
	Host     string             `json:"host"`
	Version  string             `json:"version"`
	Task     ChildTask          `json:"task"`
	Config   ChildRuntimeConfig `json:"config"`
}

type ChildInitializeResult struct {
	Protocol string   `json:"protocol"`
	Version  string   `json:"version"`
	TaskID   string   `json:"task_id"`
	Features []string `json:"features,omitempty"`
}

func (result ChildInitializeResult) Validate(task ChildTask) error {
	if result.Protocol != ChildProtocolVersion {
		return fmt.Errorf("unsupported child protocol %q", result.Protocol)
	}
	if result.Version == "" {
		return errors.New("child version is required")
	}
	if result.TaskID != task.TaskID {
		return fmt.Errorf("child task id %q does not match %q", result.TaskID, task.TaskID)
	}
	return nil
}

type ChildEvent struct {
	TaskID     string                 `json:"task_id"`
	ParentID   string                 `json:"parent_id,omitempty"`
	Type       string                 `json:"type"`
	Delta      string                 `json:"delta,omitempty"`
	Info       string                 `json:"info,omitempty"`
	Message    *Message               `json:"message,omitempty"`
	ToolCall   *ToolCall              `json:"tool_call,omitempty"`
	ToolResult *ToolResultEvent       `json:"tool_result,omitempty"`
	Approval   *ApprovalEvent         `json:"approval,omitempty"`
	Usage      *UsageEvent            `json:"usage,omitempty"`
	Completion *ChildCompletionPacket `json:"completion,omitempty"`
}

func (event ChildEvent) Observation() ChildObservation {
	return ChildObservation{TaskID: event.TaskID, ParentID: event.ParentID, Type: event.Type, At: time.Now().UTC(), Delta: event.Delta, Info: event.Info, Message: event.Message, ToolCall: event.ToolCall, ToolResult: event.ToolResult, Approval: event.Approval, Usage: event.Usage, Completion: event.Completion}
}

func childEventFromObservation(observation ChildObservation) ChildEvent {
	return ChildEvent{TaskID: observation.TaskID, ParentID: observation.ParentID, Type: observation.Type, Delta: observation.Delta, Info: observation.Info, Message: observation.Message, ToolCall: observation.ToolCall, ToolResult: observation.ToolResult, Approval: observation.Approval, Usage: observation.Usage, Completion: observation.Completion}
}

type childRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type childRPCResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Result  any            `json:"result,omitempty"`
	Error   *childRPCError `json:"error,omitempty"`
}

type childRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (err *childRPCError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("child RPC error %d: %s", err.Code, err.Message)
}

func childID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("child request id is required")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("child request id must be valid JSON")
	}
	switch value := value.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return "", errors.New("child request id must not be blank")
		}
		return value, nil
	case float64:
		return fmt.Sprintf("%v", value), nil
	default:
		return "", errors.New("child request id must be a string or number")
	}
}

func validateChildRPCRequest(request childRPCRequest) (string, error) {
	if request.JSONRPC != "2.0" {
		return "", errors.New("child jsonrpc must be 2.0")
	}
	if strings.TrimSpace(request.Method) == "" {
		return "", errors.New("child method is required")
	}
	if request.Method == ChildMethodEvent || request.Method == ChildMethodCancelRequest {
		return "", nil
	}
	return childID(request.ID)
}

// childRPCPending is shared by the child-side broker and parent process
// runner. It deliberately allows only one response per request ID.
type childRPCPending struct {
	mu      sync.Mutex
	waiters map[string]chan childRPCResponse
}

func newChildRPCPending() *childRPCPending {
	return &childRPCPending{waiters: make(map[string]chan childRPCResponse)}
}

func (pending *childRPCPending) add(id string) (chan childRPCResponse, error) {
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if _, exists := pending.waiters[id]; exists {
		return nil, fmt.Errorf("child request id %q is already pending", id)
	}
	wait := make(chan childRPCResponse, 1)
	pending.waiters[id] = wait
	return wait, nil
}

func (pending *childRPCPending) take(id string) (chan childRPCResponse, bool) {
	pending.mu.Lock()
	defer pending.mu.Unlock()
	wait, ok := pending.waiters[id]
	if ok {
		delete(pending.waiters, id)
	}
	return wait, ok
}

func (pending *childRPCPending) failAll(response childRPCResponse) {
	pending.mu.Lock()
	waiters := make([]chan childRPCResponse, 0, len(pending.waiters))
	for id, wait := range pending.waiters {
		delete(pending.waiters, id)
		waiters = append(waiters, wait)
	}
	pending.mu.Unlock()
	for _, wait := range waiters {
		wait <- response
	}
}

const (
	defaultChildStartupTimeout = 5 * time.Second
	defaultChildMaxFrameBytes  = 1 << 20
	defaultChildMaxMessageSize = 4 << 20
)

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "PASSWORD", "SECRET", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
