package bee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultACPStartupTimeout = 5 * time.Second
	defaultACPMaxFrameBytes  = 1 << 20
	defaultACPMaxMessageSize = 4 << 20
)

// ACPProcessConfig is an explicit provider-neutral ACP adapter process
// configuration. Bee never discovers adapter commands or imports provider
// OAuth/config state.
type ACPProcessConfig struct {
	Command      string   `json:"command"`
	Args         []string `json:"args,omitempty"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	EnvAllowlist []string `json:"env_allowlist,omitempty"`
	// AuthMethod is an explicit ACP authentication selection. An empty value
	// preserves the legacy no-auth behavior until an adapter advertises auth;
	// when AuthEnvVar is set, it is treated as api-key for compatibility.
	AuthMethod string `json:"auth_method,omitempty"`
	// AuthEnvVar is an explicit parent-owned authentication opt-in. It is not
	// copied into the adapter environment and is read only when authenticate is
	// required by initialize.
	AuthEnvVar            string `json:"auth_env_var,omitempty"`
	StartupTimeoutSeconds int    `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes         int    `json:"max_frame_bytes,omitempty"`
	MaxMessageBytes       int    `json:"max_message_bytes,omitempty"`
	// AutoApprovePermissions is opt-in and only takes effect when the
	// individual parent-owned task also enables its permission policy.
	AutoApprovePermissions bool `json:"auto_approve_permissions,omitempty"`
}

func (config ACPProcessConfig) Validate() error {
	if strings.TrimSpace(config.Command) == "" {
		return errors.New("ACP command is required")
	}
	if config.WorkingDir != "" && strings.TrimSpace(config.WorkingDir) == "" {
		return errors.New("ACP working_dir must not be blank")
	}
	if config.StartupTimeoutSeconds < 0 || config.RequestTimeoutSeconds < 0 {
		return errors.New("ACP timeouts must not be negative")
	}
	if config.MaxFrameBytes < 0 || config.MaxMessageBytes < 0 {
		return errors.New("ACP frame limits must not be negative")
	}
	if config.MaxFrameBytes > 0 && config.MaxMessageBytes > 0 && config.MaxFrameBytes > config.MaxMessageBytes {
		return errors.New("ACP max_frame_bytes must not exceed max_message_bytes")
	}
	if config.AuthMethod != "" && config.AuthMethod != ACPAuthMethodAPIKey {
		return fmt.Errorf("ACP auth_method %q is not allowed; use %s", config.AuthMethod, ACPAuthMethodAPIKey)
	}
	if config.AuthEnvVar != "" && config.AuthEnvVar != ACPAuthEnvOpenAI {
		return fmt.Errorf("ACP auth_env_var %q is not allowed; use %s", config.AuthEnvVar, ACPAuthEnvOpenAI)
	}
	seen := make(map[string]struct{}, len(config.EnvAllowlist))
	for _, key := range config.EnvAllowlist {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\r\n") {
			return fmt.Errorf("ACP environment key %q is invalid", key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("ACP environment key %q is duplicated", key)
		}
		if secretEnvironmentKey(key) {
			return fmt.Errorf("ACP refuses secret-shaped environment key %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (config ACPProcessConfig) startupTimeout() time.Duration {
	if config.StartupTimeoutSeconds <= 0 {
		return defaultACPStartupTimeout
	}
	return time.Duration(config.StartupTimeoutSeconds) * time.Second
}

func (config ACPProcessConfig) requestTimeout() time.Duration {
	if config.RequestTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(config.RequestTimeoutSeconds) * time.Second
}

func (config ACPProcessConfig) maxFrameBytes() int {
	if config.MaxFrameBytes <= 0 {
		return defaultACPMaxFrameBytes
	}
	return config.MaxFrameBytes
}

func (config ACPProcessConfig) maxMessageBytes() int {
	if config.MaxMessageBytes <= 0 {
		return defaultACPMaxMessageSize
	}
	return config.MaxMessageBytes
}

// ACPImplementation and capability types mirror the ACP v1 wire schema. The
// transport only advertises capabilities Bee can safely service.
type ACPImplementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type ACPFileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

type ACPAuthCapabilities struct {
	Terminal bool `json:"terminal,omitempty"`
}

type ACPClientCapabilities struct {
	FS       ACPFileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                      `json:"terminal,omitempty"`
	Auth     ACPAuthCapabilities       `json:"auth,omitempty"`
}

type ACPPromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type ACPMcpCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type ACPSessionCapabilities struct {
	Resume                *struct{} `json:"resume,omitempty"`
	Close                 *struct{} `json:"close,omitempty"`
	AdditionalDirectories *struct{} `json:"additionalDirectories,omitempty"`
}

type ACPAgentAuthCapabilities struct {
	Logout *struct{} `json:"logout,omitempty"`
}

type ACPAgentCapabilities struct {
	LoadSession         bool                     `json:"loadSession,omitempty"`
	PromptCapabilities  ACPPromptCapabilities    `json:"promptCapabilities,omitempty"`
	McpCapabilities     ACPMcpCapabilities       `json:"mcpCapabilities,omitempty"`
	SessionCapabilities ACPSessionCapabilities   `json:"sessionCapabilities,omitempty"`
	Auth                ACPAgentAuthCapabilities `json:"auth,omitempty"`
}

// ACPInitializeParams is standard ACP v1 initialize params. The deprecated
// fields are in-memory compatibility aliases only and are never put on the
// wire with legacy snake_case names.
type ACPInitializeParams struct {
	ProtocolVersion    uint16                `json:"protocolVersion,omitempty"`
	ClientCapabilities ACPClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         ACPImplementation     `json:"clientInfo,omitempty"`

	Protocol      any    `json:"-"`
	ClientName    string `json:"-"`
	ClientVersion string `json:"-"`
}

func (params ACPInitializeParams) standard() ACPInitializeParams {
	if params.ProtocolVersion == 0 {
		switch value := params.Protocol.(type) {
		case uint16:
			params.ProtocolVersion = value
		case int:
			if value > 0 && value <= 65535 {
				params.ProtocolVersion = uint16(value)
			}
		case string:
			if value == "1" {
				params.ProtocolVersion = ACPProtocolVersion
			}
		}
	}
	if params.ProtocolVersion == 0 {
		params.ProtocolVersion = ACPProtocolVersion
	}
	if params.ClientInfo.Name == "" {
		params.ClientInfo.Name = params.ClientName
	}
	if params.ClientInfo.Version == "" {
		params.ClientInfo.Version = params.ClientVersion
	}
	if params.ClientInfo.Name == "" {
		params.ClientInfo.Name = "bee"
	}
	if params.ClientInfo.Version == "" {
		params.ClientInfo.Version = "dev"
	}
	return params
}

type ACPInitializeResult struct {
	ProtocolVersion   uint16               `json:"protocolVersion"`
	AgentCapabilities ACPAgentCapabilities `json:"agentCapabilities,omitempty"`
	AuthMethods       []ACPAuthMethod      `json:"authMethods,omitempty"`
	AgentInfo         *ACPImplementation   `json:"agentInfo,omitempty"`

	// Convenience compatibility fields are populated from agentInfo and are
	// intentionally excluded from JSON.
	Protocol any      `json:"-"`
	Agent    string   `json:"-"`
	Version  string   `json:"-"`
	Features []string `json:"-"`
}

// ACPSessionNewParams contains only standard session/new fields on the wire.
// Task metadata remains parent-owned and is carried in the prompt text.
type ACPSessionNewParams struct {
	Cwd                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	MCPServers            []any    `json:"mcpServers"`

	TaskID       string   `json:"-"`
	ParentID     string   `json:"-"`
	Prompt       string   `json:"-"`
	Plan         []string `json:"-"`
	AllowedTools []string `json:"-"`
}

type ACPSession struct {
	SessionID string `json:"sessionId"`
}

func (session ACPSession) Validate() error {
	if strings.TrimSpace(session.SessionID) == "" {
		return errors.New("ACP session id is required")
	}
	return nil
}

// ACPContentBlock is the standard content block union's text subset. Bee
// deliberately sends bounded text prompts only; agents still receive the
// protocol's required array-of-blocks shape.
type ACPContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (block ACPContentBlock) Validate() error {
	if block.Type != "text" {
		return fmt.Errorf("unsupported ACP content block type %q", block.Type)
	}
	if block.Text == "" {
		return errors.New("ACP text content is empty")
	}
	return nil
}

type ACPPromptParams struct {
	SessionID string            `json:"sessionId"`
	Prompt    []ACPContentBlock `json:"prompt,omitempty"`
	Text      string            `json:"-"`
}

func (params ACPPromptParams) standard() (acpPromptWire, error) {
	content := append([]ACPContentBlock(nil), params.Prompt...)
	if len(content) == 0 && params.Text != "" {
		content = []ACPContentBlock{{Type: "text", Text: params.Text}}
	}
	if strings.TrimSpace(params.SessionID) == "" {
		return acpPromptWire{}, errors.New("ACP prompt session id is required")
	}
	if len(content) == 0 {
		return acpPromptWire{}, errors.New("ACP prompt content is required")
	}
	for _, block := range content {
		if err := block.Validate(); err != nil {
			return acpPromptWire{}, err
		}
	}
	return acpPromptWire{SessionID: params.SessionID, Prompt: content}, nil
}

type acpPromptWire struct {
	SessionID string            `json:"sessionId"`
	Prompt    []ACPContentBlock `json:"prompt"`
}

type ACPPromptResult struct {
	StopReason string `json:"stopReason"`

	Summary    string                 `json:"-"`
	Completion *ChildCompletionPacket `json:"-"`
	Payload    json.RawMessage        `json:"-"`
}

func (result ACPPromptResult) Validate() error {
	if strings.TrimSpace(result.StopReason) == "" {
		return errors.New("ACP prompt stopReason is required")
	}
	switch result.StopReason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
		return nil
	default:
		return fmt.Errorf("ACP prompt stopReason %q is invalid", result.StopReason)
	}
}

// ACPUpdate is the normalized notification envelope emitted by an ACP agent.
// Provider-specific fields remain in Payload; text/artifact/diff fields are
// bounded by the transport before they reach observers or completion packets.
type ACPUpdate struct {
	SessionID  string                 `json:"session_id,omitempty"`
	TaskID     string                 `json:"task_id,omitempty"`
	ParentID   string                 `json:"parent_id,omitempty"`
	Type       string                 `json:"type"`
	Text       string                 `json:"text,omitempty"`
	Info       string                 `json:"info,omitempty"`
	Artifact   string                 `json:"artifact,omitempty"`
	Diff       string                 `json:"diff,omitempty"`
	Completion *ChildCompletionPacket `json:"completion,omitempty"`
	Payload    json.RawMessage        `json:"payload,omitempty"`
}

func (update ACPUpdate) Validate() error {
	if strings.TrimSpace(update.Type) == "" {
		return errors.New("ACP update type is required")
	}
	if len(update.Payload) > 1<<20 {
		return errors.New("ACP update payload exceeds 1 MiB")
	}
	if update.Completion != nil {
		if err := update.Completion.Validate(); err != nil {
			return fmt.Errorf("ACP update completion: %w", err)
		}
	}
	return nil
}

type ACPTransport interface {
	Start(context.Context) error
	Initialize(context.Context, ACPInitializeParams) (ACPInitializeResult, error)
	NewSession(context.Context, ACPSessionNewParams) (ACPSession, error)
	Prompt(context.Context, ACPPromptParams) (ACPPromptResult, error)
	Cancel(context.Context, string) error
	Updates() <-chan ACPUpdate
	Failure() error
	Diagnostics() string
	Close(context.Context) error
}

// ACPAuthenticator is optional so existing provider-neutral test/embedding
// transports remain source-compatible. A transport that receives an
// api-key-authenticated initialize result must implement this capability or
// the parent fails closed before session/new.
type ACPAuthenticator interface {
	Authenticate(context.Context, ACPAuthenticateParams) error
}

type ACPTransportFactory func(ACPProcessConfig) (ACPTransport, error)

// ACPStdioTransport is a bounded newline-delimited JSON-RPC 2.0 transport.
// ACP v1 uses JSON-RPC over stdio; all protocol objects use camelCase fields.
type ACPStdioTransport struct {
	config ACPProcessConfig

	mu                sync.RWMutex
	command           *exec.Cmd
	stdin             io.WriteCloser
	started           bool
	closed            bool
	failure           error
	done              chan struct{}
	doneOnce          sync.Once
	updates           chan ACPUpdate
	permissionMu      sync.RWMutex
	permissionHandler ACPPermissionHandler
	sessionID         string

	pendingMu sync.Mutex
	pending   map[string]chan acpRPCResponse
	nextID    atomic.Uint64
	writeMu   sync.Mutex
	waitDone  chan error

	diagnosticsMu sync.Mutex
	diagnostics   bytes.Buffer
}

var _ ACPTransport = (*ACPStdioTransport)(nil)

func NewACPStdioTransport(config ACPProcessConfig) (*ACPStdioTransport, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ACPStdioTransport{config: config, done: make(chan struct{}), updates: make(chan ACPUpdate, 64), pending: make(map[string]chan acpRPCResponse), waitDone: make(chan error, 1)}, nil
}

func (transport *ACPStdioTransport) Start(ctx context.Context) error {
	if transport == nil {
		return errors.New("ACP transport is nil")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.started {
		if transport.failure != nil {
			return transport.failure
		}
		return errors.New("ACP transport already started")
	}
	if transport.closed {
		return errors.New("ACP transport is closed")
	}
	command := exec.Command(transport.config.Command, transport.config.Args...)
	if transport.config.WorkingDir != "" {
		command.Dir = transport.config.WorkingDir
	}
	setAllowlistedChildEnvironment(command, transport.config.EnvAllowlist, false)
	if err := configureChildProcessGroup(command); err != nil {
		return err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("ACP stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("ACP stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("ACP stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("start ACP process: %w", err)
	}
	transport.command = command
	transport.stdin = stdin
	transport.started = true
	go transport.readLoop(stdout)
	go transport.readDiagnostics(stderr)
	go func() {
		err := command.Wait()
		transport.waitDone <- err
		if err != nil {
			transport.fail(fmt.Errorf("ACP process exited: %w", err))
		} else {
			transport.fail(errors.New("ACP process exited"))
		}
	}()
	return nil
}

func (transport *ACPStdioTransport) Initialize(ctx context.Context, params ACPInitializeParams) (ACPInitializeResult, error) {
	params = params.standard()
	var result ACPInitializeResult
	response, err := transport.request(ctx, ACPMethodInitialize, params)
	if err != nil {
		return result, err
	}
	if err := decodeACPResult(response, &result); err != nil {
		return result, err
	}
	if result.ProtocolVersion == 0 {
		return result, errors.New("ACP initialize response protocolVersion is required")
	}
	if result.AgentInfo != nil {
		result.Agent = result.AgentInfo.Name
		result.Version = result.AgentInfo.Version
	}
	result.Protocol = result.ProtocolVersion
	return result, nil
}

func (transport *ACPStdioTransport) Authenticate(ctx context.Context, params ACPAuthenticateParams) error {
	if err := params.Validate(); err != nil {
		return err
	}
	response, err := transport.request(ctx, ACPMethodAuthenticate, params)
	if err != nil {
		return err
	}
	var result map[string]any
	return decodeACPResult(response, &result)
}

func (transport *ACPStdioTransport) NewSession(ctx context.Context, params ACPSessionNewParams) (ACPSession, error) {
	var result ACPSession
	if strings.TrimSpace(params.Cwd) == "" {
		return result, errors.New("ACP session cwd is required")
	}
	if !filepath.IsAbs(params.Cwd) {
		return result, errors.New("ACP session cwd must be absolute")
	}
	for _, directory := range params.AdditionalDirectories {
		if !filepath.IsAbs(directory) {
			return result, errors.New("ACP additional directory must be absolute")
		}
	}
	if params.MCPServers == nil {
		params.MCPServers = []any{}
	}
	response, err := transport.request(ctx, ACPMethodNewSession, params)
	if err != nil {
		return result, err
	}
	if err := decodeACPResult(response, &result); err != nil {
		return result, err
	}
	if err := result.Validate(); err != nil {
		return result, err
	}
	transport.mu.Lock()
	transport.sessionID = result.SessionID
	transport.mu.Unlock()
	return result, nil
}

// SetACPPermissionHandler installs a parent-owned handler before or during a
// session. The transport still validates the selected option against the
// request, so a handler can never invent an optionId.
func (transport *ACPStdioTransport) SetACPPermissionHandler(handler ACPPermissionHandler) {
	if transport == nil {
		return
	}
	transport.permissionMu.Lock()
	transport.permissionHandler = handler
	transport.permissionMu.Unlock()
}

// SetPermissionHandler is the concise compatibility spelling used by
// embedders that do not need the ACP prefix.
func (transport *ACPStdioTransport) SetPermissionHandler(handler ACPPermissionHandler) {
	transport.SetACPPermissionHandler(handler)
}

func (transport *ACPStdioTransport) Prompt(ctx context.Context, params ACPPromptParams) (ACPPromptResult, error) {
	var result ACPPromptResult
	wire, err := params.standard()
	if err != nil {
		return result, err
	}
	response, err := transport.request(ctx, ACPMethodPrompt, wire)
	if err != nil {
		return result, err
	}
	if err := decodeACPResult(response, &result); err != nil {
		return result, err
	}
	if err := result.Validate(); err != nil {
		return result, err
	}
	return result, nil
}

func (transport *ACPStdioTransport) Cancel(_ context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("ACP cancellation session id is required")
	}
	return transport.notify(ACPMethodCancel, map[string]string{"sessionId": sessionID})
}

func (transport *ACPStdioTransport) Updates() <-chan ACPUpdate {
	if transport == nil {
		return nil
	}
	return transport.updates
}

func (transport *ACPStdioTransport) Failure() error {
	if transport == nil {
		return errors.New("ACP transport is nil")
	}
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	return transport.failure
}

func (transport *ACPStdioTransport) Diagnostics() string {
	if transport == nil {
		return ""
	}
	transport.diagnosticsMu.Lock()
	defer transport.diagnosticsMu.Unlock()
	return transport.diagnostics.String()
}

func (transport *ACPStdioTransport) request(ctx context.Context, method string, params any) (acpRPCResponse, error) {
	if transport == nil {
		return acpRPCResponse{}, errors.New("ACP transport is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := contextWithOptionalTimeout(ctx, transport.config.requestTimeout())
	defer cancel()
	transport.mu.RLock()
	started, closed, failure := transport.started, transport.closed, transport.failure
	transport.mu.RUnlock()
	if !started {
		return acpRPCResponse{}, errors.New("ACP transport is not started")
	}
	if closed {
		return acpRPCResponse{}, errors.New("ACP transport is closed")
	}
	if failure != nil {
		return acpRPCResponse{}, failure
	}
	id := fmt.Sprintf("%d", transport.nextID.Add(1))
	wait := make(chan acpRPCResponse, 1)
	transport.pendingMu.Lock()
	transport.pending[id] = wait
	transport.pendingMu.Unlock()
	request := acpRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: mustRawJSON(params)}
	if err := transport.writeFrame(request); err != nil {
		transport.removePending(id)
		return acpRPCResponse{}, err
	}
	select {
	case response := <-wait:
		if response.Error != nil {
			return response, response.Error
		}
		return response, nil
	case <-requestCtx.Done():
		transport.removePending(id)
		return acpRPCResponse{}, requestCtx.Err()
	case <-transport.done:
		transport.removePending(id)
		if failure := transport.Failure(); failure != nil {
			return acpRPCResponse{}, failure
		}
		return acpRPCResponse{}, errors.New("ACP transport stopped")
	}
}

func (transport *ACPStdioTransport) notify(method string, params any) error {
	return transport.writeFrame(acpRPCRequest{JSONRPC: "2.0", Method: method, Params: mustRawJSON(params)})
}

func (transport *ACPStdioTransport) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode ACP frame: %w", err)
	}
	if len(data) > transport.config.maxMessageBytes() {
		return fmt.Errorf("ACP frame exceeds max message size (%d bytes)", transport.config.maxMessageBytes())
	}
	data = append(data, '\n')
	if len(data) > transport.config.maxFrameBytes() {
		return fmt.Errorf("ACP frame exceeds max frame size (%d bytes)", transport.config.maxFrameBytes())
	}
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	transport.mu.RLock()
	stdin := transport.stdin
	transport.mu.RUnlock()
	if stdin == nil {
		return errors.New("ACP stdin is not available")
	}
	if _, err := stdin.Write(data); err != nil {
		transport.fail(fmt.Errorf("write ACP frame: %w", err))
		return err
	}
	return nil
}

func (transport *ACPStdioTransport) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		frame, err := readBoundedFrame(reader, transport.config.maxFrameBytes(), transport.config.maxMessageBytes(), "ACP")
		if err != nil {
			if errors.Is(err, io.EOF) {
				transport.fail(errors.New("ACP stdout closed"))
			} else {
				transport.fail(fmt.Errorf("ACP protocol read: %w", err))
			}
			return
		}
		var envelope acpRPCEnvelope
		if err := json.Unmarshal(frame, &envelope); err != nil {
			transport.fail(fmt.Errorf("ACP protocol malformed JSON: %w", err))
			return
		}
		if envelope.JSONRPC != "2.0" {
			transport.fail(errors.New("ACP protocol jsonrpc must be 2.0"))
			return
		}
		if envelope.Method != "" {
			if envelope.Method == ACPMethodUpdate {
				update, err := decodeStandardACPUpdate(envelope.Params)
				if err != nil {
					transport.fail(fmt.Errorf("ACP update malformed: %w", err))
					return
				}
				select {
				case transport.updates <- update:
				case <-transport.done:
					return
				}
				continue
			}
			if envelope.Method == ACPMethodCancel || envelope.Method == "$/cancelRequest" {
				continue
			}
			if envelope.Method == ACPMethodRequestPermission {
				if len(envelope.ID) == 0 {
					transport.fail(errors.New("ACP permission request id is required"))
					return
				}
				permission, err := decodeACPPermissionRequest(envelope.Params)
				if err != nil {
					_ = transport.writeFrame(acpRPCResponseWire{JSONRPC: "2.0", ID: envelope.ID, Result: mustRawJSON(ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "cancelled"}})})
					continue
				}
				response := transport.handleACPPermission(permission)
				response = normalizeACPPermissionResponse(permission, response)
				_ = transport.writeFrame(acpRPCResponseWire{JSONRPC: "2.0", ID: envelope.ID, Result: mustRawJSON(response)})
				continue
			}
			// ACP agents may call client methods (terminal/fs/permission). Bee
			// does not advertise those capabilities, so answer with the standard
			// JSON-RPC method-not-found error instead of corrupting the stream.
			if len(envelope.ID) != 0 {
				_ = transport.writeFrame(acpRPCResponseWire{JSONRPC: "2.0", ID: envelope.ID, Error: &acpRPCError{Code: -32601, Message: "method not found"}})
				continue
			}
			transport.fail(fmt.Errorf("ACP unsupported agent notification %q", envelope.Method))
			return
		}
		if len(envelope.ID) == 0 {
			transport.fail(errors.New("ACP response id is required"))
			return
		}
		id, err := acpID(envelope.ID)
		if err != nil {
			transport.fail(err)
			return
		}
		if envelope.Error != nil && len(envelope.Result) != 0 {
			transport.fail(errors.New("ACP response cannot contain both result and error"))
			return
		}
		response := acpRPCResponse{JSONRPC: envelope.JSONRPC, ID: id, Result: append(json.RawMessage(nil), envelope.Result...), Error: envelope.Error}
		transport.pendingMu.Lock()
		wait, ok := transport.pending[id]
		if ok {
			delete(transport.pending, id)
		}
		transport.pendingMu.Unlock()
		if !ok {
			transport.fail(fmt.Errorf("ACP response has unknown or duplicate id %q", id))
			return
		}
		wait <- response
	}
}

func decodeACPPermissionRequest(raw json.RawMessage) (ACPPermissionRequest, error) {
	var request ACPPermissionRequest
	if len(raw) == 0 || string(raw) == "null" {
		return request, errors.New("ACP permission request params are required")
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, fmt.Errorf("decode ACP permission request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func (transport *ACPStdioTransport) handleACPPermission(request ACPPermissionRequest) ACPPermissionResponse {
	cancel := ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "cancelled"}}
	transport.mu.RLock()
	activeSession := transport.sessionID
	transport.mu.RUnlock()
	if activeSession == "" || request.SessionID != activeSession {
		return cancel
	}
	transport.permissionMu.RLock()
	handler := transport.permissionHandler
	transport.permissionMu.RUnlock()
	if handler == nil {
		return cancel
	}
	ctx, cancelContext := contextWithOptionalTimeout(context.Background(), transport.config.requestTimeout())
	defer cancelContext()
	return handler.HandleACPPermission(ctx, request)
}

func normalizeACPPermissionResponse(request ACPPermissionRequest, response ACPPermissionResponse) ACPPermissionResponse {
	cancel := ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "cancelled"}}
	if response.Outcome.Outcome != "selected" {
		return cancel
	}
	optionID := strings.TrimSpace(response.Outcome.OptionID)
	if optionID == "" {
		return cancel
	}
	for _, option := range request.Options {
		if option.OptionID == optionID {
			kind := strings.ToLower(strings.TrimSpace(option.Kind))
			if kind != "allow_once" && kind != "proceed_once" {
				return cancel
			}
			return ACPPermissionResponse{Outcome: ACPPermissionOutcome{Outcome: "selected", OptionID: option.OptionID}}
		}
	}
	return cancel
}

func decodeStandardACPUpdate(raw json.RawMessage) (ACPUpdate, error) {
	var notification struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil {
		return ACPUpdate{}, err
	}
	if strings.TrimSpace(notification.SessionID) == "" || len(notification.Update) == 0 || string(notification.Update) == "null" {
		return ACPUpdate{}, errors.New("session/update requires sessionId and update")
	}
	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
		Title         string          `json:"title"`
		Status        string          `json:"status"`
		Entries       json.RawMessage `json:"entries"`
		ToolCallID    string          `json:"toolCallId"`
	}
	if err := json.Unmarshal(notification.Update, &update); err != nil {
		return ACPUpdate{}, err
	}
	if strings.TrimSpace(update.SessionUpdate) == "" {
		return ACPUpdate{}, errors.New("session/update update.sessionUpdate is required")
	}
	normalized := ACPUpdate{SessionID: notification.SessionID, Type: update.SessionUpdate, Payload: append(json.RawMessage(nil), notification.Update...)}
	if len(update.Content) > 0 && string(update.Content) != "null" {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(update.Content, &block); err == nil {
			if block.Type == "text" {
				normalized.Text = block.Text
			}
		}
		var blocks []struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(update.Content, &blocks); err == nil {
			for _, block := range blocks {
				if block.Content.Type == "text" {
					normalized.Text += block.Content.Text
				}
			}
		}
	}
	if update.Title != "" {
		normalized.Info = update.Title
	}
	if update.Status != "" {
		if normalized.Info != "" {
			normalized.Info += ": "
		}
		normalized.Info += update.Status
	}
	if update.ToolCallID != "" {
		if normalized.Info != "" {
			normalized.Info += " "
		}
		normalized.Info += "tool=" + update.ToolCallID
	}
	if err := normalized.Validate(); err != nil {
		return ACPUpdate{}, err
	}
	return normalized, nil
}

func (transport *ACPStdioTransport) readDiagnostics(stderr io.Reader) {
	limit := transport.config.maxMessageBytes()
	data, _ := io.ReadAll(io.LimitReader(stderr, int64(limit)+1))
	if len(data) > limit {
		data = data[:limit]
	}
	transport.diagnosticsMu.Lock()
	transport.diagnostics.Write(data)
	transport.diagnosticsMu.Unlock()
}

func (transport *ACPStdioTransport) removePending(id string) {
	transport.pendingMu.Lock()
	delete(transport.pending, id)
	transport.pendingMu.Unlock()
}

func (transport *ACPStdioTransport) fail(err error) {
	if transport == nil || err == nil {
		return
	}
	transport.mu.Lock()
	if transport.failure != nil {
		transport.mu.Unlock()
		return
	}
	transport.failure = err
	transport.mu.Unlock()
	transport.pendingMu.Lock()
	waiters := make([]chan acpRPCResponse, 0, len(transport.pending))
	for id, wait := range transport.pending {
		delete(transport.pending, id)
		waiters = append(waiters, wait)
	}
	transport.pendingMu.Unlock()
	for _, wait := range waiters {
		wait <- acpRPCResponse{Error: &acpRPCError{Code: -32603, Message: err.Error()}}
	}
	transport.doneOnce.Do(func() {
		close(transport.done)
		close(transport.updates)
	})
}

func (transport *ACPStdioTransport) Close(ctx context.Context) error {
	if transport == nil {
		return nil
	}
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return nil
	}
	transport.closed = true
	command := transport.command
	stdin := transport.stdin
	transport.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if command == nil {
		return nil
	}
	select {
	case <-transport.waitDone:
	case <-ctx.Done():
		_ = killChildProcessGroup(command)
	case <-time.After(300 * time.Millisecond):
		_ = killChildProcessGroup(command)
		select {
		case <-transport.waitDone:
		case <-time.After(500 * time.Millisecond):
		}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if transport.Failure() == nil {
		transport.fail(errors.New("ACP transport closed"))
	}
	return nil
}

type acpRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type acpRPCResponseWire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

type acpRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

type acpRPCResponse struct {
	JSONRPC string
	ID      string
	Result  json.RawMessage
	Error   *acpRPCError
}

type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (err *acpRPCError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("ACP RPC error %d: %s", err.Code, err.Message)
}

func acpID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("ACP response id is required")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("ACP response id must be valid JSON")
	}
	switch value := value.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return "", errors.New("ACP response id must not be blank")
		}
		return value, nil
	case float64:
		return fmt.Sprintf("%v", value), nil
	default:
		return "", errors.New("ACP response id must be a string or number")
	}
}

func decodeACPResult(response acpRPCResponse, target any) error {
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return errors.New("ACP response result is required")
	}
	if err := json.Unmarshal(response.Result, target); err != nil {
		return fmt.Errorf("decode ACP response: %w", err)
	}
	return nil
}

// Keep ACP on the same bounded newline framing as the existing child
// transport so frame and message limits have one clear implementation.
