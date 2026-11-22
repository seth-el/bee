package bee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultLSPStartupTimeout = 5 * time.Second
	defaultLSPRequestTimeout = 5 * time.Second
	defaultLSPMaxFrameBytes  = 4 << 20
	defaultLSPMaxFileBytes   = 1 << 20
	defaultLSPMaxOutputBytes = 32 << 10
	maxLSPServers            = 16
	maxLSPPatterns           = 32
	maxLSPDiagnostics        = 100
	maxLSPPosition           = 10_000_000
	maxLSPDocuments          = 256
	maxLSPPendingRequests    = 64
	maxLSPDiagnosticBytes    = 64 << 10
)

// LSPConfig is an explicit, owner-authored set of language servers. Bee never
// discovers executables or lets a model choose a server command.
type LSPConfig struct {
	Servers []LSPServerConfig `json:"servers,omitempty"`
}

// LSPServerConfig binds one server to one writable workspace and a bounded
// set of file patterns. The server is launched lazily on first matching use.
type LSPServerConfig struct {
	Name                  string   `json:"name"`
	Enabled               bool     `json:"enabled"`
	Command               string   `json:"command"`
	Args                  []string `json:"args,omitempty"`
	WorkingDir            string   `json:"working_dir"`
	LanguageID            string   `json:"language_id"`
	FilePatterns          []string `json:"file_patterns"`
	EnvAllowlist          []string `json:"env_allowlist,omitempty"`
	StartupTimeoutSeconds int      `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes         int      `json:"max_frame_bytes,omitempty"`
	MaxFileBytes          int      `json:"max_file_bytes,omitempty"`
	MaxOutputBytes        int      `json:"max_output_bytes,omitempty"`
}

func cloneLSPConfig(config LSPConfig) LSPConfig {
	result := LSPConfig{Servers: make([]LSPServerConfig, len(config.Servers))}
	for index, server := range config.Servers {
		result.Servers[index] = server
		result.Servers[index].Args = append([]string(nil), server.Args...)
		result.Servers[index].FilePatterns = append([]string(nil), server.FilePatterns...)
		result.Servers[index].EnvAllowlist = append([]string(nil), server.EnvAllowlist...)
	}
	return result
}

func (config LSPConfig) Validate() error {
	if len(config.Servers) > maxLSPServers {
		return fmt.Errorf("LSP configuration must not contain more than %d servers", maxLSPServers)
	}
	seen := make(map[string]struct{}, len(config.Servers))
	for index, server := range config.Servers {
		if _, ok := seen[server.Name]; ok {
			return fmt.Errorf("duplicate LSP server name %q", server.Name)
		}
		seen[server.Name] = struct{}{}
		if err := server.Validate(); err != nil {
			return fmt.Errorf("LSP servers[%d]: %w", index, err)
		}
	}
	return nil
}

func (config LSPServerConfig) Validate() error {
	if !validMCPName(config.Name) {
		return fmt.Errorf("server name %q is invalid", config.Name)
	}
	if !config.Enabled {
		return nil
	}
	if strings.TrimSpace(config.Command) == "" || strings.ContainsRune(config.Command, '\x00') {
		return errors.New("command is required")
	}
	if strings.TrimSpace(config.WorkingDir) == "" || strings.ContainsRune(config.WorkingDir, '\x00') {
		return errors.New("working_dir is required")
	}
	if strings.TrimSpace(config.LanguageID) == "" || strings.ContainsAny(config.LanguageID, "\x00\r\n") {
		return errors.New("language_id is required")
	}
	if len(config.FilePatterns) == 0 || len(config.FilePatterns) > maxLSPPatterns {
		return fmt.Errorf("file_patterns must contain between 1 and %d entries", maxLSPPatterns)
	}
	for index, pattern := range config.FilePatterns {
		if strings.TrimSpace(pattern) == "" || strings.ContainsRune(pattern, '\x00') {
			return fmt.Errorf("file_patterns[%d] is invalid", index)
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("file_patterns[%d]: %w", index, err)
		}
	}
	for index, arg := range config.Args {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("args[%d] is invalid", index)
		}
	}
	seenEnv := make(map[string]struct{}, len(config.EnvAllowlist))
	for _, key := range config.EnvAllowlist {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\r\n") {
			return fmt.Errorf("invalid environment key %q", key)
		}
		if _, ok := seenEnv[key]; ok {
			return fmt.Errorf("repeated environment key %q", key)
		}
		seenEnv[key] = struct{}{}
	}
	if config.StartupTimeoutSeconds < 0 || config.StartupTimeoutSeconds > 300 || config.RequestTimeoutSeconds < 0 || config.RequestTimeoutSeconds > 300 {
		return errors.New("timeouts must be between 0 and 300 seconds")
	}
	if config.MaxFrameBytes < 0 || config.MaxFrameBytes > 16<<20 {
		return errors.New("max_frame_bytes must be between 0 and 16777216")
	}
	if config.MaxFileBytes < 0 || config.MaxFileBytes > 8<<20 {
		return errors.New("max_file_bytes must be between 0 and 8388608")
	}
	if config.MaxOutputBytes < 0 || config.MaxOutputBytes > 1<<20 {
		return errors.New("max_output_bytes must be between 0 and 1048576")
	}
	return nil
}

func (config LSPServerConfig) startupTimeout() time.Duration {
	if config.StartupTimeoutSeconds <= 0 {
		return defaultLSPStartupTimeout
	}
	return time.Duration(config.StartupTimeoutSeconds) * time.Second
}

func (config LSPServerConfig) requestTimeout() time.Duration {
	if config.RequestTimeoutSeconds <= 0 {
		return defaultLSPRequestTimeout
	}
	return time.Duration(config.RequestTimeoutSeconds) * time.Second
}

func (config LSPServerConfig) maxFrameBytes() int {
	if config.MaxFrameBytes <= 0 {
		return defaultLSPMaxFrameBytes
	}
	return config.MaxFrameBytes
}

func (config LSPServerConfig) maxFileBytes() int {
	if config.MaxFileBytes <= 0 {
		return defaultLSPMaxFileBytes
	}
	return config.MaxFileBytes
}

func (config LSPServerConfig) maxOutputBytes() int {
	if config.MaxOutputBytes <= 0 {
		return defaultLSPMaxOutputBytes
	}
	return config.MaxOutputBytes
}

type lspRPCError struct {
	Code int `json:"code"`
}

type lspResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspRPCError    `json:"error,omitempty"`
}

type lspDiagnostic struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
		End struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"end"`
	} `json:"range"`
	Severity int             `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

type lspPublishedDiagnostics struct {
	URI         string          `json:"uri"`
	Diagnostics []lspDiagnostic `json:"diagnostics"`
}

type lspDiagnosticState struct {
	sequence    uint64
	diagnostics []lspDiagnostic
}

// LSPHost owns one lazy language-server subprocess for an Application.
type LSPHost struct {
	config     LSPServerConfig
	scope      ExecutionScope
	workingDir string

	startMu sync.Mutex
	writeMu sync.Mutex
	mu      sync.Mutex
	stdin   io.WriteCloser
	process *exec.Cmd
	done    chan struct{}
	doneOne sync.Once
	closed  bool
	started bool
	ready   bool
	failure error
	nextID  uint64
	pending map[string]chan lspResponse

	documents   map[string]int
	diagnostics map[string]lspDiagnosticState
	diagnosticC chan struct{}
}

func NewLSPHost(config LSPServerConfig, scope ExecutionScope) (*LSPHost, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if !config.Enabled {
		return nil, errors.New("LSP server is disabled")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	workingDir, err := existingResolvedDir(config.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("LSP server %q working_dir: %w", config.Name, err)
	}
	writable, err := scope.writeRootPaths()
	if err != nil {
		return nil, err
	}
	if !pathWithinAny(writable, workingDir) {
		return nil, fmt.Errorf("LSP server %q working_dir must be within an execution write root", config.Name)
	}
	return &LSPHost{
		config: config, scope: scope, workingDir: workingDir, done: make(chan struct{}),
		pending: make(map[string]chan lspResponse), documents: make(map[string]int),
		diagnostics: make(map[string]lspDiagnosticState), diagnosticC: make(chan struct{}),
	}, nil
}

func (host *LSPHost) matches(path string) bool {
	if host == nil {
		return false
	}
	relative, err := filepath.Rel(host.workingDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	relative = filepath.ToSlash(relative)
	for _, pattern := range host.config.FilePatterns {
		pattern = filepath.ToSlash(pattern)
		matched, _ := filepath.Match(pattern, relative)
		if !matched && !strings.Contains(pattern, "/") {
			matched, _ = filepath.Match(pattern, filepath.Base(relative))
		}
		if matched {
			return true
		}
	}
	return false
}

func (host *LSPHost) Start(ctx context.Context) error {
	if host == nil {
		return errors.New("LSP host is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	host.startMu.Lock()
	defer host.startMu.Unlock()
	host.mu.Lock()
	if host.closed || host.failure != nil {
		host.mu.Unlock()
		return errors.New("language server is unavailable")
	}
	if host.started {
		ready := host.ready
		host.mu.Unlock()
		if ready {
			return nil
		}
		return errors.New("language server is unavailable")
	}
	host.mu.Unlock()

	program, args, err := sandboxCommandInvocation(host.scope, host.config.Command, host.config.Args, host.workingDir)
	if err != nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	command := exec.Command(program, args...)
	if !host.scope.EnforceWriteRoots {
		command.Dir = host.workingDir
	}
	setAllowlistedChildEnvironment(command, host.config.EnvAllowlist, host.scope.EnforceWriteRoots)
	if err := configureChildProcessGroup(command); err != nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		host.fail()
		return errors.New("language server is unavailable")
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		host.fail()
		return errors.New("language server is unavailable")
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		host.fail()
		return errors.New("language server is unavailable")
	}
	host.mu.Lock()
	host.stdin = stdin
	host.process = command
	host.started = true
	host.mu.Unlock()
	go host.readLoop(stdout)
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go func() {
		_ = command.Wait()
		host.fail()
	}()

	startupCtx, cancel := context.WithTimeout(ctx, host.config.startupTimeout())
	defer cancel()
	rootURI := lspFileURI(host.workingDir)
	result, err := host.request(startupCtx, "initialize", map[string]any{
		"processId": nil, "rootUri": rootURI,
		"workspaceFolders": []map[string]string{{"uri": rootURI, "name": filepath.Base(host.workingDir)}},
		"capabilities":     map[string]any{"textDocument": map[string]any{"publishDiagnostics": map[string]any{}}, "workspace": map[string]any{"configuration": false}},
		"clientInfo":       map[string]string{"name": "bee", "version": "5"},
	})
	if err != nil || len(result) == 0 {
		host.fail()
		return errors.New("language server is unavailable")
	}
	var initialized struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if json.Unmarshal(result, &initialized) != nil || initialized.Capabilities == nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	if err := host.notify("initialized", map[string]any{}); err != nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	host.mu.Lock()
	host.ready = true
	host.mu.Unlock()
	return nil
}

func (host *LSPHost) ensureStarted(ctx context.Context) error {
	host.mu.Lock()
	started := host.ready && host.failure == nil && !host.closed
	host.mu.Unlock()
	if started {
		return nil
	}
	return host.Start(ctx)
}

func (host *LSPHost) request(parent context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, host.config.requestTimeout())
	defer cancel()
	host.mu.Lock()
	if !host.started || host.closed || host.failure != nil {
		host.mu.Unlock()
		return nil, errors.New("language server is unavailable")
	}
	host.nextID++
	if len(host.pending) >= maxLSPPendingRequests {
		host.mu.Unlock()
		return nil, errors.New("language server request limit reached")
	}
	numericID := host.nextID
	id := strconv.FormatUint(numericID, 10)
	wait := make(chan lspResponse, 1)
	host.pending[id] = wait
	host.mu.Unlock()
	if err := host.write(map[string]any{"jsonrpc": "2.0", "id": numericID, "method": method, "params": params}); err != nil {
		host.removePending(id)
		return nil, errors.New("language server is unavailable")
	}
	select {
	case response := <-wait:
		if response.Error != nil {
			return nil, fmt.Errorf("language server returned RPC error %d", response.Error.Code)
		}
		return response.Result, nil
	case <-ctx.Done():
		host.removePending(id)
		return nil, ctx.Err()
	case <-host.done:
		host.removePending(id)
		return nil, errors.New("language server is unavailable")
	}
}

func (host *LSPHost) removePending(id string) {
	host.mu.Lock()
	delete(host.pending, id)
	host.mu.Unlock()
}

func (host *LSPHost) notify(method string, params any) error {
	return host.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (host *LSPHost) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode language-server frame")
	}
	if len(data) > host.config.maxFrameBytes() {
		return errors.New("language-server frame is too large")
	}
	header := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data)))
	host.writeMu.Lock()
	defer host.writeMu.Unlock()
	host.mu.Lock()
	stdin := host.stdin
	available := host.started && !host.closed && host.failure == nil
	host.mu.Unlock()
	if stdin == nil || !available {
		return errors.New("language server is unavailable")
	}
	if _, err := stdin.Write(append(header, data...)); err != nil {
		host.fail()
		return errors.New("language server is unavailable")
	}
	return nil
}

func readLSPFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	contentLength := -1
	headerBytes := 0
	for {
		line, err := reader.ReadString('\n')
		headerBytes += len(line)
		if headerBytes > 8<<10 {
			return nil, errors.New("language-server header is too large")
		}
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("language-server header is malformed")
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			if contentLength >= 0 {
				return nil, errors.New("language-server content length is repeated")
			}
			contentLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || contentLength < 0 || contentLength > limit {
				return nil, errors.New("language-server content length is invalid")
			}
		}
	}
	if contentLength < 0 {
		return nil, errors.New("language-server content length is required")
	}
	data := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (host *LSPHost) readLoop(stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, 32<<10)
	for {
		frame, err := readLSPFrame(reader, host.config.maxFrameBytes())
		if err != nil {
			host.fail()
			return
		}
		var envelope struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(frame, &envelope) != nil || envelope.JSONRPC != "2.0" {
			host.fail()
			return
		}
		if envelope.Method != "" {
			if len(envelope.ID) != 0 && !bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
				host.respondToServerRequest(envelope.ID, envelope.Method, envelope.Params)
				continue
			}
			if envelope.Method == "textDocument/publishDiagnostics" {
				host.publishDiagnostics(envelope.Params)
			}
			continue
		}
		var response lspResponse
		if json.Unmarshal(frame, &response) != nil || (response.Error != nil && len(response.Result) != 0) || (response.Error == nil && len(response.Result) == 0) {
			host.fail()
			return
		}
		id, ok := mcpResponseID(response.ID)
		if !ok {
			host.fail()
			return
		}
		host.mu.Lock()
		wait, pending := host.pending[id]
		if pending {
			delete(host.pending, id)
		}
		host.mu.Unlock()
		if !pending {
			host.fail()
			return
		}
		wait <- response
	}
}

func (host *LSPHost) respondToServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		var request struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &request)
		result = make([]any, len(request.Items))
	case "workspace/applyEdit":
		result = map[string]any{"applied": false}
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		result = nil
	default:
		_ = host.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": -32601, "message": "unsupported"}})
		return
	}
	_ = host.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (host *LSPHost) publishDiagnostics(raw json.RawMessage) {
	var published lspPublishedDiagnostics
	if json.Unmarshal(raw, &published) != nil || published.URI == "" || len(published.Diagnostics) > maxLSPDiagnostics {
		return
	}
	totalBytes := 0
	for _, diagnostic := range published.Diagnostics {
		totalBytes += len(diagnostic.Message) + len(diagnostic.Source) + len(diagnostic.Code)
		if !utf8.ValidString(diagnostic.Message) || len(diagnostic.Message) > 8<<10 || len(diagnostic.Source) > 256 || len(diagnostic.Code) > 1024 || totalBytes > maxLSPDiagnosticBytes || diagnostic.Range.Start.Line < 0 || diagnostic.Range.Start.Line > maxLSPPosition || diagnostic.Range.End.Line < 0 || diagnostic.Range.End.Line > maxLSPPosition || diagnostic.Range.Start.Character < 0 || diagnostic.Range.Start.Character > maxLSPPosition || diagnostic.Range.End.Character < 0 || diagnostic.Range.End.Character > maxLSPPosition {
			return
		}
	}
	host.mu.Lock()
	if _, open := host.documents[published.URI]; !open {
		host.mu.Unlock()
		return
	}
	state := host.diagnostics[published.URI]
	state.sequence++
	state.diagnostics = append([]lspDiagnostic(nil), published.Diagnostics...)
	host.diagnostics[published.URI] = state
	close(host.diagnosticC)
	host.diagnosticC = make(chan struct{})
	host.mu.Unlock()
}

func lspFileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func (host *LSPHost) syncDocument(ctx context.Context, path string) (string, uint64, error) {
	if err := host.ensureStarted(ctx); err != nil {
		return "", 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, int64(host.config.maxFileBytes())+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > host.config.maxFileBytes() || !utf8.Valid(data) {
		return "", 0, errors.New("language-server source file is unavailable or too large")
	}
	uri := lspFileURI(path)
	host.mu.Lock()
	version, open := host.documents[uri]
	if !open && len(host.documents) >= maxLSPDocuments {
		host.mu.Unlock()
		return "", 0, errors.New("language-server document limit reached")
	}
	version++
	host.documents[uri] = version
	prior := host.diagnostics[uri].sequence
	host.mu.Unlock()
	if !open {
		err = host.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": host.config.LanguageID, "version": version, "text": string(data)}})
	} else {
		err = host.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": version}, "contentChanges": []map[string]string{{"text": string(data)}}})
	}
	if err != nil {
		return "", 0, err
	}
	return uri, prior, nil
}

func (host *LSPHost) Diagnostics(ctx context.Context, path string) ([]lspDiagnostic, error) {
	uri, prior, err := host.syncDocument(ctx, path)
	if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, host.config.requestTimeout())
	defer cancel()
	for {
		host.mu.Lock()
		state := host.diagnostics[uri]
		changed := host.diagnosticC
		host.mu.Unlock()
		if state.sequence > prior {
			return append([]lspDiagnostic(nil), state.diagnostics...), nil
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-host.done:
			return nil, errors.New("language server is unavailable")
		case <-changed:
		}
	}
}

func (host *LSPHost) query(ctx context.Context, path, method string, params map[string]any) (json.RawMessage, error) {
	uri, _, err := host.syncDocument(ctx, path)
	if err != nil {
		return nil, err
	}
	params["textDocument"] = map[string]string{"uri": uri}
	return host.request(ctx, method, params)
}

func (host *LSPHost) fail() {
	if host == nil {
		return
	}
	host.mu.Lock()
	if host.failure != nil || host.closed {
		host.mu.Unlock()
		return
	}
	host.failure = errors.New("language server is unavailable")
	process := host.process
	waits := make([]chan lspResponse, 0, len(host.pending))
	for id, wait := range host.pending {
		waits = append(waits, wait)
		delete(host.pending, id)
	}
	host.mu.Unlock()
	for _, wait := range waits {
		wait <- lspResponse{Error: &lspRPCError{Code: -32603}}
	}
	_ = terminateCommandProcessGroup(process)
	host.doneOne.Do(func() { close(host.done) })
}

func (host *LSPHost) Close(ctx context.Context) error {
	if host == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	host.startMu.Lock()
	defer host.startMu.Unlock()
	host.mu.Lock()
	if host.closed {
		host.mu.Unlock()
		return nil
	}
	started := host.started && host.failure == nil
	host.mu.Unlock()
	if started {
		shutdownCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, _ = host.request(shutdownCtx, "shutdown", nil)
		cancel()
		_ = host.notify("exit", nil)
	}
	host.mu.Lock()
	host.closed = true
	stdin := host.stdin
	process := host.process
	host.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = terminateCommandProcessGroup(process)
	host.doneOne.Do(func() { close(host.done) })
	return nil
}

// LSPManager chooses the configured server solely from the normalized path.
type LSPManager struct {
	scope ExecutionScope
	hosts []*LSPHost
}

func NewLSPManager(config LSPConfig, scope ExecutionScope) (*LSPManager, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	manager := &LSPManager{scope: scope}
	for _, server := range config.Servers {
		if !server.Enabled {
			continue
		}
		host, err := NewLSPHost(server, scope)
		if err != nil {
			return nil, err
		}
		manager.hosts = append(manager.hosts, host)
	}
	return manager, nil
}

func (manager *LSPManager) hostForPath(path string) (*LSPHost, string, error) {
	if manager == nil {
		return nil, "", errors.New("language-server support is not configured")
	}
	resolved, err := securePath(manager.scope, path, false)
	if err != nil {
		return nil, "", err
	}
	var selected *LSPHost
	for _, host := range manager.hosts {
		if !host.matches(resolved) {
			continue
		}
		if selected != nil {
			return nil, "", errors.New("file matches multiple configured language servers")
		}
		selected = host
	}
	if selected == nil {
		return nil, "", errors.New("no configured language server matches the file")
	}
	return selected, resolved, nil
}

func (manager *LSPManager) PostEditDiagnostics(ctx context.Context, name string, args json.RawMessage, outcome string) []HookObservation {
	if manager == nil || outcome != "succeeded" || (name != "write_file" && name != "edit_file") {
		return nil
	}
	if NetworkOffFromContext(ctx) {
		// A language server can reach the network (module downloads); it is
		// shared, so it can't be restarted without network for one session.
		return nil
	}
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &input) != nil || strings.TrimSpace(input.Path) == "" {
		return nil
	}
	host, path, err := manager.hostForPath(input.Path)
	if err != nil {
		return nil
	}
	diagnostics, err := host.Diagnostics(ctx, path)
	if err != nil {
		return nil
	}
	payload := struct {
		Path        string          `json:"path"`
		Diagnostics []lspDiagnostic `json:"diagnostics"`
	}{Path: input.Path, Diagnostics: diagnostics}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	output, truncated := truncateOutput("Language-server diagnostics (untrusted): "+string(encoded), host.config.maxOutputBytes())
	return []HookObservation{{Point: HookPostToolCall, Index: -1, Outcome: HookSucceeded, Output: output, Truncated: truncated}}
}

func (manager *LSPManager) Close(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	var first error
	for _, host := range manager.hosts {
		if err := host.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type lspPositionArgs struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

func (input lspPositionArgs) validate() error {
	if strings.TrimSpace(input.Path) == "" {
		return errors.New("path is required")
	}
	if input.Line < 0 || input.Line > maxLSPPosition || input.Character < 0 || input.Character > maxLSPPosition {
		return errors.New("line and character must be bounded non-negative positions")
	}
	return nil
}

type LSPQueryTool struct {
	Manager *LSPManager
	Kind    string
}

func (tool LSPQueryTool) Definition() ToolDefinition {
	description := "Query the owner-configured language server for " + tool.Kind + ". Results are bounded untrusted observations; the server and command are never model-selected."
	parameters := `{"type":"object","properties":{"path":{"type":"string"},"line":{"type":"integer","minimum":0},"character":{"type":"integer","minimum":0}},"required":["path","line","character"],"additionalProperties":false}`
	if tool.Kind == "references" {
		parameters = `{"type":"object","properties":{"path":{"type":"string"},"line":{"type":"integer","minimum":0},"character":{"type":"integer","minimum":0},"include_declaration":{"type":"boolean"}},"required":["path","line","character"],"additionalProperties":false}`
	}
	return ToolDefinition{Name: "lsp_" + tool.Kind, Description: description, Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(parameters)}
}

func (tool LSPQueryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		lspPositionArgs
		IncludeDeclaration bool `json:"include_declaration"`
	}
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("lsp_%s arguments: %w", tool.Kind, err)
	}
	if err := input.lspPositionArgs.validate(); err != nil {
		return "", err
	}
	host, path, err := tool.Manager.hostForPath(input.Path)
	if err != nil {
		return "", err
	}
	method := "textDocument/" + tool.Kind
	params := map[string]any{"position": map[string]int{"line": input.Line, "character": input.Character}}
	if tool.Kind == "references" {
		params["context"] = map[string]bool{"includeDeclaration": input.IncludeDeclaration}
	}
	result, err := host.query(ctx, path, method, params)
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	output, err := json.Marshal(struct {
		Kind   string          `json:"kind"`
		Path   string          `json:"path"`
		Result json.RawMessage `json:"result"`
	}{Kind: tool.Kind, Path: input.Path, Result: result})
	if err != nil || len(output) > host.config.maxOutputBytes() {
		return "", errors.New("language-server result exceeds the configured output limit")
	}
	return string(output), nil
}
