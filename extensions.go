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
	"sort"
	"strings"
	"sync"
	"time"
)

// ExtensionProtocolVersion is the versioned wire contract for external Bee
// extensions. It is intentionally separate from the client-facing RPC API.
const ExtensionProtocolVersion = "bee.extension.v1"

const (
	ExtensionMethodInitialize    = "initialize"
	ExtensionMethodToolsCall     = "tools/call"
	ExtensionMethodShutdown      = "shutdown"
	ExtensionMethodCancelRequest = "$/cancelRequest"
)

const (
	ExtensionRPCParseError     = -32700
	ExtensionRPCInvalidRequest = -32600
	ExtensionRPCMethodNotFound = -32601
	ExtensionRPCInvalidParams  = -32602
	ExtensionRPCInternalError  = -32603
	ExtensionRPCServerError    = -32000
)

const (
	defaultExtensionStartupTimeout = 5 * time.Second
	defaultExtensionRequestTimeout = 30 * time.Second
	defaultExtensionMaxFrameBytes  = 1 << 20
	defaultExtensionMaxMessageSize = 4 << 20
)

// ExtensionConfig is an explicit extension allowlist entry. Bee never scans
// PATH or discovers executables automatically; only enabled entries run.
type ExtensionConfig struct {
	Name                  string   `json:"name"`
	Enabled               bool     `json:"enabled"`
	Command               string   `json:"command"`
	Args                  []string `json:"args,omitempty"`
	WorkingDir            string   `json:"working_dir,omitempty"`
	EnvAllowlist          []string `json:"env_allowlist,omitempty"`
	StartupTimeoutSeconds int      `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes         int      `json:"max_frame_bytes,omitempty"`
	MaxMessageBytes       int      `json:"max_message_bytes,omitempty"`
}

func (config ExtensionConfig) Validate() error {
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("extension name is required")
	}
	if strings.ContainsAny(config.Name, " \t\r\n") {
		return fmt.Errorf("extension name %q contains whitespace", config.Name)
	}
	if config.Enabled && strings.TrimSpace(config.Command) == "" {
		return fmt.Errorf("extension %q command is required when enabled", config.Name)
	}
	if config.WorkingDir == "" {
		// Empty means inherit Bee's working directory.
	} else if strings.TrimSpace(config.WorkingDir) == "" {
		return fmt.Errorf("extension %q working_dir must not be blank", config.Name)
	}
	if config.StartupTimeoutSeconds < 0 || config.RequestTimeoutSeconds < 0 {
		return fmt.Errorf("extension %q timeouts must not be negative", config.Name)
	}
	if config.MaxFrameBytes < 0 || config.MaxMessageBytes < 0 {
		return fmt.Errorf("extension %q message limits must not be negative", config.Name)
	}
	if config.MaxFrameBytes > 0 && config.MaxMessageBytes > 0 && config.MaxFrameBytes > config.MaxMessageBytes {
		return fmt.Errorf("extension %q max_frame_bytes must not exceed max_message_bytes", config.Name)
	}
	seen := make(map[string]struct{}, len(config.EnvAllowlist))
	for _, key := range config.EnvAllowlist {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\r\n") {
			return fmt.Errorf("extension %q has invalid environment key %q", config.Name, key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("extension %q repeats environment key %q", config.Name, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (config ExtensionConfig) startupTimeout() time.Duration {
	if config.StartupTimeoutSeconds <= 0 {
		return defaultExtensionStartupTimeout
	}
	return time.Duration(config.StartupTimeoutSeconds) * time.Second
}

func (config ExtensionConfig) requestTimeout() time.Duration {
	if config.RequestTimeoutSeconds <= 0 {
		return defaultExtensionRequestTimeout
	}
	return time.Duration(config.RequestTimeoutSeconds) * time.Second
}

func (config ExtensionConfig) maxFrameBytes() int {
	if config.MaxFrameBytes <= 0 {
		return defaultExtensionMaxFrameBytes
	}
	return config.MaxFrameBytes
}

func (config ExtensionConfig) maxMessageBytes() int {
	if config.MaxMessageBytes <= 0 {
		return defaultExtensionMaxMessageSize
	}
	return config.MaxMessageBytes
}

// ExtensionManifest identifies an extension and its wire contract.
type ExtensionManifest struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Protocol        string `json:"protocol,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	Description     string `json:"description,omitempty"`
}

func (manifest ExtensionManifest) Validate() error {
	if strings.TrimSpace(manifest.Name) == "" {
		return errors.New("extension manifest name is required")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return errors.New("extension manifest version is required")
	}
	protocol := manifest.Protocol
	if protocol == "" {
		protocol = manifest.ProtocolVersion
	}
	if protocol != ExtensionProtocolVersion {
		return fmt.Errorf("unsupported extension protocol %q", protocol)
	}
	return nil
}

// ExtensionCapabilities is deliberately small. Providers, hooks, and context
// are discovery seams only; Bee does not execute them in Phase 5.
type ExtensionCapabilities struct {
	Tools         []ToolDefinition `json:"tools,omitempty"`
	Notifications []string         `json:"notifications,omitempty"`
	Providers     []string         `json:"providers,omitempty"`
	Hooks         []string         `json:"hooks,omitempty"`
	Context       []string         `json:"context,omitempty"`
}

func (capabilities ExtensionCapabilities) Validate() error {
	seenTools := make(map[string]struct{}, len(capabilities.Tools))
	for i, tool := range capabilities.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("capability tool %d: %w", i, err)
		}
		if _, ok := seenTools[tool.Name]; ok {
			return fmt.Errorf("duplicate extension tool capability %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
	}
	for label, values := range map[string][]string{
		"notification": capabilities.Notifications,
		"provider":     capabilities.Providers,
		"hook":         capabilities.Hooks,
		"context":      capabilities.Context,
	} {
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, " \t\r\n") {
				return fmt.Errorf("invalid %s capability %q", label, value)
			}
			if _, ok := seen[value]; ok {
				return fmt.Errorf("duplicate extension %s capability %q", label, value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

// ExtensionInitializeParams is sent by Bee during the startup handshake.
type ExtensionInitializeParams struct {
	Protocol        string `json:"protocol"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	Host            string `json:"host"`
	Version         string `json:"version"`
}

// ExtensionInitializeResult is returned by an extension after initialize.
type ExtensionInitializeResult struct {
	Manifest     ExtensionManifest     `json:"manifest"`
	Capabilities ExtensionCapabilities `json:"capabilities"`
}

// ExtensionToolCallParams is the provider-neutral tools/call payload.
type ExtensionToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ExtensionToolCallResult is the successful result envelope from an extension.
type ExtensionToolCallResult struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ExtensionNotification is an unsolicited extension event. Its params are
// untrusted data and are never treated as Bee instructions or context.
type ExtensionNotification struct {
	Method string
	Params json.RawMessage
}

type ExtensionNotificationHandler func(ExtensionNotification)

// ExtensionRPCError is a structured JSON-RPC error returned by an extension.
type ExtensionRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *ExtensionRPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("extension RPC error %d", e.Code)
	}
	return fmt.Sprintf("extension RPC error %d: %s", e.Code, e.Message)
}

type extensionRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type extensionResponse struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      string             `json:"id"`
	Result  json.RawMessage    `json:"result,omitempty"`
	Error   *ExtensionRPCError `json:"error,omitempty"`
}

// ExtensionRequest and ExtensionResponse are the public wire-envelope
// contracts. The host uses the same types internally while keeping the Phase
// 4 client-facing RPC envelopes separate.
type ExtensionRequest = extensionRequest
type ExtensionResponse = extensionResponse

// ExtensionHost owns one external process and isolates its protocol failures
// from Bee. stdout is protocol-only; stderr is retained as diagnostics.
type ExtensionHost struct {
	config ExtensionConfig

	mu            sync.RWMutex
	startMu       sync.Mutex
	writeMu       sync.Mutex
	stdin         io.WriteCloser
	manifest      ExtensionManifest
	capabilities  ExtensionCapabilities
	started       bool
	closed        bool
	failure       error
	nextID        uint64
	pending       map[string]chan extensionResponse
	done          chan struct{}
	doneOnce      sync.Once
	closeOnce     sync.Once
	process       *exec.Cmd
	processDone   chan error
	diagnosticsMu sync.Mutex
	diagnostics   bytes.Buffer
	notification  ExtensionNotificationHandler
}

func NewExtensionHost(config ExtensionConfig) (*ExtensionHost, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ExtensionHost{config: config, pending: make(map[string]chan extensionResponse), done: make(chan struct{}), processDone: make(chan error, 1)}, nil
}

func NewExtensionHosts(configs []ExtensionConfig) ([]*ExtensionHost, error) {
	seen := make(map[string]struct{}, len(configs))
	hosts := make([]*ExtensionHost, 0, len(configs))
	for _, config := range configs {
		if _, ok := seen[config.Name]; ok {
			return nil, fmt.Errorf("duplicate extension name %q", config.Name)
		}
		seen[config.Name] = struct{}{}
		if !config.Enabled {
			continue
		}
		host, err := NewExtensionHost(config)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func (host *ExtensionHost) Config() ExtensionConfig {
	if host == nil {
		return ExtensionConfig{}
	}
	return host.config
}

func (host *ExtensionHost) Manifest() ExtensionManifest {
	if host == nil {
		return ExtensionManifest{}
	}
	host.mu.RLock()
	defer host.mu.RUnlock()
	return host.manifest
}

func (host *ExtensionHost) Capabilities() ExtensionCapabilities {
	if host == nil {
		return ExtensionCapabilities{}
	}
	host.mu.RLock()
	defer host.mu.RUnlock()
	return copyExtensionCapabilities(host.capabilities)
}

func copyExtensionCapabilities(input ExtensionCapabilities) ExtensionCapabilities {
	output := input
	output.Tools = append([]ToolDefinition(nil), input.Tools...)
	for i := range output.Tools {
		output.Tools[i].Parameters = append(json.RawMessage(nil), input.Tools[i].Parameters...)
	}
	output.Notifications = append([]string(nil), input.Notifications...)
	output.Providers = append([]string(nil), input.Providers...)
	output.Hooks = append([]string(nil), input.Hooks...)
	output.Context = append([]string(nil), input.Context...)
	return output
}

func (host *ExtensionHost) SetNotificationHandler(handler ExtensionNotificationHandler) {
	if host == nil {
		return
	}
	host.mu.Lock()
	host.notification = handler
	host.mu.Unlock()
}

// Start launches the explicitly configured process and completes initialize.
func (host *ExtensionHost) Start(ctx context.Context) error {
	if host == nil {
		return errors.New("extension host is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !host.config.Enabled {
		return fmt.Errorf("extension %q is disabled", host.config.Name)
	}
	if err := host.config.Validate(); err != nil {
		return err
	}
	host.startMu.Lock()
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		host.startMu.Unlock()
		return errors.New("extension host is closed")
	}
	if host.started {
		failure := host.failure
		closed := host.closed
		host.mu.Unlock()
		if failure != nil {
			host.startMu.Unlock()
			return failure
		}
		if closed {
			host.startMu.Unlock()
			return errors.New("extension host is closed")
		}
		host.startMu.Unlock()
		return nil
	}
	host.mu.Unlock()

	command := exec.Command(host.config.Command, host.config.Args...)
	command.Dir = host.config.WorkingDir
	setAllowlistedChildEnvironment(command, host.config.EnvAllowlist, false)
	stdin, err := command.StdinPipe()
	if err != nil {
		host.startMu.Unlock()
		return fmt.Errorf("extension %q stdin: %w", host.config.Name, err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		host.startMu.Unlock()
		return fmt.Errorf("extension %q stdout: %w", host.config.Name, err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		host.startMu.Unlock()
		return fmt.Errorf("extension %q stderr: %w", host.config.Name, err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		host.startMu.Unlock()
		return fmt.Errorf("start extension %q: %w", host.config.Name, err)
	}
	host.mu.Lock()
	host.stdin = stdin
	host.process = command
	host.started = true
	host.mu.Unlock()
	host.startMu.Unlock()
	go host.readLoop(stdout)
	go host.captureStderr(stderr)
	go func() {
		err := command.Wait()
		host.processDone <- err
		if err != nil {
			host.fail(fmt.Errorf("extension %q exited: %w", host.config.Name, err))
		} else {
			host.fail(fmt.Errorf("extension %q exited", host.config.Name))
		}
	}()

	startupCtx, cancel := context.WithTimeout(ctx, host.config.startupTimeout())
	defer cancel()
	result, err := host.initializeResult(startupCtx)
	if err != nil {
		_ = host.Close(context.Background())
		return fmt.Errorf("initialize extension %q: %w", host.config.Name, err)
	}
	host.mu.Lock()
	host.manifest = result.Manifest
	host.capabilities = result.Capabilities
	host.mu.Unlock()
	return nil
}

func (host *ExtensionHost) initializeResult(ctx context.Context) (ExtensionInitializeResult, error) {
	response, err := host.request(ctx, ExtensionMethodInitialize, ExtensionInitializeParams{Protocol: ExtensionProtocolVersion, Host: "bee", Version: "5"})
	if err != nil {
		return ExtensionInitializeResult{}, err
	}
	var result ExtensionInitializeResult
	if err := decodeResult(response, &result); err != nil {
		return result, err
	}
	if err := result.Manifest.Validate(); err != nil {
		return result, err
	}
	if result.Manifest.Protocol == "" {
		result.Manifest.Protocol = result.Manifest.ProtocolVersion
	}
	if result.Manifest.Name != host.config.Name {
		return result, fmt.Errorf("extension manifest name %q does not match configured name %q", result.Manifest.Name, host.config.Name)
	}
	if err := result.Capabilities.Validate(); err != nil {
		return result, err
	}
	return result, nil
}

// Initialize performs the handshake on a process already started by Start.
// It is exposed for protocol tests; normal callers should use Start.
func (host *ExtensionHost) Initialize(ctx context.Context) error {
	if host == nil {
		return errors.New("extension host is nil")
	}
	result, err := host.initializeResult(ctx)
	if err != nil {
		return err
	}
	host.mu.Lock()
	host.manifest = result.Manifest
	host.capabilities = result.Capabilities
	host.mu.Unlock()
	return nil
}

func decodeResult(response extensionResponse, target any) error {
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return errors.New("extension response result is required")
	}
	if err := json.Unmarshal(response.Result, target); err != nil {
		return fmt.Errorf("decode extension response: %w", err)
	}
	return nil
}

func (host *ExtensionHost) request(ctx context.Context, method string, params any) (extensionResponse, error) {
	if host == nil {
		return extensionResponse{}, errors.New("extension host is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	host.mu.Lock()
	if !host.started || host.closed || host.failure != nil {
		failure := host.failure
		host.mu.Unlock()
		if failure != nil {
			return extensionResponse{}, failure
		}
		return extensionResponse{}, errors.New("extension host is not running")
	}
	host.nextID++
	id := fmt.Sprintf("%d", host.nextID)
	wait := make(chan extensionResponse, 1)
	host.pending[id] = wait
	host.mu.Unlock()
	request := extensionRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := host.writeFrame(request); err != nil {
		host.removePending(id)
		return extensionResponse{}, host.failureOr(err)
	}
	select {
	case response := <-wait:
		return response, nil
	case <-ctx.Done():
		host.removePending(id)
		_ = host.CancelRequest(id)
		return extensionResponse{}, ctx.Err()
	case <-host.done:
		host.removePending(id)
		return extensionResponse{}, host.failureOrDefault()
	}
}

func (host *ExtensionHost) failureOr(err error) error {
	host.mu.RLock()
	defer host.mu.RUnlock()
	if host.failure != nil {
		return host.failure
	}
	return err
}

func (host *ExtensionHost) failureOrDefault() error {
	return host.failureOr(errors.New("extension host stopped"))
}

func (host *ExtensionHost) removePending(id string) {
	host.mu.Lock()
	delete(host.pending, id)
	host.mu.Unlock()
}

func (host *ExtensionHost) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode extension frame: %w", err)
	}
	if len(data) > host.config.maxMessageBytes() {
		return fmt.Errorf("extension frame exceeds max message size (%d bytes)", host.config.maxMessageBytes())
	}
	data = append(data, '\n')
	if len(data) > host.config.maxFrameBytes() {
		return fmt.Errorf("extension frame exceeds max frame size (%d bytes)", host.config.maxFrameBytes())
	}
	host.writeMu.Lock()
	defer host.writeMu.Unlock()
	host.mu.RLock()
	stdin := host.stdin
	host.mu.RUnlock()
	if stdin == nil {
		return errors.New("extension stdin is not available")
	}
	if _, err := stdin.Write(data); err != nil {
		host.fail(fmt.Errorf("write extension frame: %w", err))
		return err
	}
	return nil
}

// Call sends an extension request. It is provider-neutral and intentionally
// not exposed through the Phase 4 client RPC surface.
func (host *ExtensionHost) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("extension method is required")
	}
	requestCtx := ctx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	var cancel context.CancelFunc
	requestCtx, cancel = context.WithTimeout(requestCtx, host.config.requestTimeout())
	defer cancel()
	response, err := host.request(requestCtx, method, params)
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return append(json.RawMessage(nil), response.Result...), nil
}

// CallTool invokes one advertised tool through the extension protocol.
func (host *ExtensionHost) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	result := ToolResult{ToolName: name}
	if strings.TrimSpace(name) == "" {
		return result, errors.New("extension tool name is required")
	}
	host.mu.RLock()
	advertised := false
	for _, definition := range host.capabilities.Tools {
		if definition.Name == name {
			advertised = true
			break
		}
	}
	host.mu.RUnlock()
	if !advertised {
		return result, fmt.Errorf("extension tool %q is not advertised", name)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return result, errors.New("extension tool arguments must be valid JSON")
	}
	requestCtx := ctx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	var cancel context.CancelFunc
	requestCtx, cancel = context.WithTimeout(requestCtx, host.config.requestTimeout())
	defer cancel()
	response, err := host.request(requestCtx, ExtensionMethodToolsCall, ExtensionToolCallParams{Name: name, Arguments: args})
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	if response.Error != nil {
		result.Error = response.Error.Error()
		return result, response.Error
	}
	var callResult ExtensionToolCallResult
	if err := decodeResult(response, &callResult); err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.Output = callResult.Output
	result.Error = callResult.Error
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

// CancelRequest sends the standard cancellation notification for a request ID.
func (host *ExtensionHost) CancelRequest(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("extension request id is required")
	}
	return host.writeFrame(extensionRequest{JSONRPC: "2.0", Method: ExtensionMethodCancelRequest, Params: map[string]string{"id": id}})
}

// RegisterTools wraps advertised extension tools in Bee's normal Tool path.
func (host *ExtensionHost) RegisterTools(registry *Registry) error {
	if host == nil || registry == nil {
		return errors.New("extension host and registry are required")
	}
	host.mu.RLock()
	started := host.started && !host.closed && host.failure == nil
	capabilities := copyExtensionCapabilities(host.capabilities)
	host.mu.RUnlock()
	if !started {
		return errors.New("extension host is not initialized")
	}
	registered := make([]string, 0, len(capabilities.Tools))
	for _, definition := range capabilities.Tools {
		tool := extensionTool{host: host, definition: definition}
		if err := registry.Register(tool); err != nil {
			for _, name := range registered {
				_ = registry.Unregister(name)
			}
			return fmt.Errorf("register extension %q tool %q: %w", host.config.Name, definition.Name, err)
		}
		registered = append(registered, definition.Name)
	}
	return nil
}

func (host *ExtensionHost) ToolNames() []string {
	if host == nil {
		return nil
	}
	host.mu.RLock()
	defer host.mu.RUnlock()
	result := make([]string, 0, len(host.capabilities.Tools))
	for _, definition := range host.capabilities.Tools {
		result = append(result, definition.Name)
	}
	return result
}

type extensionTool struct {
	host       *ExtensionHost
	definition ToolDefinition
}

func (tool extensionTool) Definition() ToolDefinition { return tool.definition }
func (tool extensionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	result, err := tool.host.CallTool(ctx, tool.definition.Name, args)
	if err != nil {
		return result.Output, err
	}
	return result.Output, nil
}

func (host *ExtensionHost) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		frame, err := readExtensionFrame(reader, host.config.maxFrameBytes(), host.config.maxMessageBytes())
		if err != nil {
			if !errors.Is(err, io.EOF) {
				host.fail(fmt.Errorf("extension protocol read: %w", err))
			} else {
				host.fail(fmt.Errorf("extension stdout closed"))
			}
			return
		}
		var envelope struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			host.fail(fmt.Errorf("extension protocol malformed JSON: %w", err))
			return
		}
		if envelope.JSONRPC != "2.0" {
			host.fail(errors.New("extension protocol jsonrpc must be 2.0"))
			return
		}
		if envelope.Method != "" {
			var notification struct {
				JSONRPC string          `json:"jsonrpc"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params,omitempty"`
			}
			if err := json.Unmarshal(frame, &notification); err != nil || strings.TrimSpace(notification.Method) == "" {
				host.fail(errors.New("extension notification method is required"))
				return
			}
			host.mu.RLock()
			handler := host.notification
			host.mu.RUnlock()
			if handler != nil {
				handler(ExtensionNotification{Method: notification.Method, Params: append(json.RawMessage(nil), notification.Params...)})
			}
			continue
		}
		var response extensionResponse
		if err := json.Unmarshal(frame, &response); err != nil {
			host.fail(fmt.Errorf("extension response malformed: %w", err))
			return
		}
		if response.ID == "" {
			host.fail(errors.New("extension response id is required"))
			return
		}
		if response.Error != nil && len(response.Result) != 0 {
			host.fail(errors.New("extension response cannot contain both result and error"))
			return
		}
		host.mu.Lock()
		wait, ok := host.pending[response.ID]
		if ok {
			delete(host.pending, response.ID)
		}
		host.mu.Unlock()
		if !ok {
			host.fail(fmt.Errorf("extension response has unknown or duplicate id %q", response.ID))
			return
		}
		wait <- response
	}
}

func readExtensionFrame(reader *bufio.Reader, maxFrame, maxMessage int) ([]byte, error) {
	frame, err := reader.ReadBytes('\n')
	if len(frame) > maxFrame {
		return nil, fmt.Errorf("extension frame exceeds max frame size (%d bytes)", maxFrame)
	}
	if len(frame) == 0 {
		return nil, err
	}
	frame = bytes.TrimSuffix(frame, []byte{'\n'})
	frame = bytes.TrimSuffix(frame, []byte{'\r'})
	if len(frame) > maxMessage {
		return nil, fmt.Errorf("extension message exceeds max message size (%d bytes)", maxMessage)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return frame, nil
}

func (host *ExtensionHost) captureStderr(stderr io.Reader) {
	limit := host.config.maxMessageBytes()
	data, _ := io.ReadAll(io.LimitReader(stderr, int64(limit)+1))
	if len(data) > limit {
		data = data[:limit]
	}
	host.diagnosticsMu.Lock()
	host.diagnostics.Write(data)
	host.diagnosticsMu.Unlock()
}

func (host *ExtensionHost) fail(err error) {
	if host == nil || err == nil {
		return
	}
	host.mu.Lock()
	if host.closed || host.failure != nil {
		host.mu.Unlock()
		return
	}
	host.failure = err
	process := host.process
	pending := make([]chan extensionResponse, 0, len(host.pending))
	for id, wait := range host.pending {
		pending = append(pending, wait)
		delete(host.pending, id)
	}
	host.mu.Unlock()
	for _, wait := range pending {
		wait <- extensionResponse{Error: &ExtensionRPCError{Code: ExtensionRPCInternalError, Message: err.Error()}}
	}
	if process != nil && process.Process != nil {
		_ = process.Process.Kill()
	}
	host.doneOnce.Do(func() { close(host.done) })
}

func (host *ExtensionHost) Diagnostics() string {
	if host == nil {
		return ""
	}
	host.diagnosticsMu.Lock()
	defer host.diagnosticsMu.Unlock()
	return host.diagnostics.String()
}

// Close requests a graceful shutdown, then closes pipes and kills the process
// if it does not exit within the supplied context.
func (host *ExtensionHost) Close(ctx context.Context) error {
	if host == nil {
		return nil
	}
	host.startMu.Lock()
	defer host.startMu.Unlock()
	var closeErr error
	host.closeOnce.Do(func() {
		host.mu.Lock()
		if !host.started {
			host.closed = true
			host.mu.Unlock()
			host.doneOnce.Do(func() { close(host.done) })
			return
		}
		host.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if _, err := host.request(shutdownCtx, ExtensionMethodShutdown, nil); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			closeErr = err
		}
		host.mu.Lock()
		stdin := host.stdin
		process := host.process
		host.closed = true
		host.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		if process != nil {
			select {
			case <-host.processDone:
			case <-ctx.Done():
				_ = process.Process.Kill()
				<-host.processDone
			case <-time.After(500 * time.Millisecond):
				_ = process.Process.Kill()
				<-host.processDone
			}
		}
		host.doneOnce.Do(func() { close(host.done) })
	})
	return closeErr
}

// SortCapabilities returns a deterministic copy useful for diagnostics/docs.
func (host *ExtensionHost) SortCapabilities() ExtensionCapabilities {
	capabilities := host.Capabilities()
	sort.Slice(capabilities.Tools, func(i, j int) bool { return capabilities.Tools[i].Name < capabilities.Tools[j].Name })
	sort.Strings(capabilities.Notifications)
	sort.Strings(capabilities.Providers)
	sort.Strings(capabilities.Hooks)
	sort.Strings(capabilities.Context)
	return capabilities
}
