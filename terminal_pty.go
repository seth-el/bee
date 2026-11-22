package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const TerminalToolName = "terminal"

var (
	ErrTerminalDisabled        = errors.New("terminal capability is disabled; enable terminal.enabled only for private/development use")
	ErrTerminalSessionNotFound = errors.New("terminal session was not found (sessions are in-memory and do not survive restart)")
)

type TerminalSessionStatus string

const (
	TerminalSessionRunning TerminalSessionStatus = "running"
	TerminalSessionExited  TerminalSessionStatus = "exited"
	TerminalSessionClosed  TerminalSessionStatus = "closed"
)

type TerminalAuditEvent struct {
	Time      time.Time `json:"time"`
	Action    string    `json:"action"`
	SessionID string    `json:"session_id,omitempty"`
	Command   string    `json:"command,omitempty"`
	Source    string    `json:"source,omitempty"`
}

type TerminalAuditHook func(context.Context, TerminalAuditEvent)

type TerminalManager struct {
	mu       sync.RWMutex
	sessions map[string]*terminalSession
	scope    ExecutionScope
	config   TerminalConfig
	opening  int
	sequence atomic.Uint64
	hook     TerminalAuditHook
}

type terminalSession struct {
	mu            sync.Mutex
	opMu          sync.Mutex
	submitMu      sync.Mutex
	id            string
	cmd           *exec.Cmd
	pty           *os.File
	cwd           string
	status        TerminalSessionStatus
	executionMode string
	exitCode      int
	exitError     string
	started       time.Time
	lastUsed      time.Time
	buffer        []byte
	base          uint64
	cursor        uint64
	done          chan struct{}
	closed        bool
	// ownerSession opened this terminal; networked is whether its shell
	// started with network (G11).
	ownerSession SessionID
	networked    bool
}

type TerminalSessionInfo struct {
	ID            string                `json:"id"`
	Status        TerminalSessionStatus `json:"status"`
	ExecutionMode string                `json:"execution_mode,omitempty"`
	Cwd           string                `json:"cwd,omitempty"`
	Shell         string                `json:"shell,omitempty"`
	StartedAt     time.Time             `json:"started_at"`
	LastUsedAt    time.Time             `json:"last_used_at"`
	Cursor        uint64                `json:"cursor"`
	BaseCursor    uint64                `json:"base_cursor"`
	ExitCode      int                   `json:"exit_code,omitempty"`
	ExitError     string                `json:"exit_error,omitempty"`
	Truncated     bool                  `json:"truncated,omitempty"`
}

type TerminalReadResult struct {
	Output     string                `json:"output,omitempty"`
	Cursor     uint64                `json:"cursor"`
	BaseCursor uint64                `json:"base_cursor"`
	Truncated  bool                  `json:"truncated,omitempty"`
	Status     TerminalSessionStatus `json:"status"`
	Running    bool                  `json:"running"`
	Completed  bool                  `json:"completed"`
	ExitCode   int                   `json:"exit_code,omitempty"`
}

func NewTerminalManager(scope ExecutionScope, config TerminalConfig) (*TerminalManager, error) {
	if !config.Enabled {
		return nil, ErrTerminalDisabled
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	config = config.effective()
	return &TerminalManager{sessions: make(map[string]*terminalSession), scope: scope, config: config}, nil
}

func (manager *TerminalManager) SetAuditHook(hook TerminalAuditHook) {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	manager.hook = hook
	manager.mu.Unlock()
}

func (manager *TerminalManager) emit(ctx context.Context, event TerminalAuditEvent) {
	manager.mu.RLock()
	hook := manager.hook
	manager.mu.RUnlock()
	if hook != nil {
		hook(ctx, event)
	}
}

func (manager *TerminalManager) Open(ctx context.Context, cwd string, cols, rows uint16) (TerminalSessionInfo, error) {
	return manager.OpenWithEnv(ctx, cwd, cols, rows, nil)
}

// OpenWithEnv opens a terminal session with an explicit environment. The
// ordinary terminal tool retains its filtered environment; exec uses the
// sandboxed seam below to preserve the gateway process environment plus its
// validated overlay without creating a second session store.
func (manager *TerminalManager) OpenWithEnv(ctx context.Context, cwd string, cols, rows uint16, env []string) (TerminalSessionInfo, error) {
	return manager.openWithEnv(ctx, cwd, cols, rows, env, false)
}

// OpenSandboxedWithEnv permits an arbitrary readable host workdir while the
// process remains confined to Bee's configured persistent write roots.
func (manager *TerminalManager) OpenSandboxedWithEnv(ctx context.Context, cwd string, cols, rows uint16, env []string) (TerminalSessionInfo, error) {
	if manager == nil || !manager.scope.TrustedForeground {
		return TerminalSessionInfo{}, errors.New("sandboxed terminal open requires trusted foreground scope")
	}
	return manager.openWithEnv(ctx, cwd, cols, rows, env, true)
}

func (manager *TerminalManager) openWithEnv(ctx context.Context, cwd string, cols, rows uint16, env []string, trustedWorkdir bool) (TerminalSessionInfo, error) {
	if manager == nil {
		return TerminalSessionInfo{}, ErrTerminalDisabled
	}
	if err := ctx.Err(); err != nil {
		return TerminalSessionInfo{}, err
	}
	manager.mu.Lock()
	active := 0
	for _, existing := range manager.sessions {
		if existing.isRunning() {
			active++
		}
	}
	if active+manager.opening >= manager.config.MaxSessions {
		manager.mu.Unlock()
		return TerminalSessionInfo{}, errors.New("terminal maximum session count reached")
	}
	manager.opening++
	manager.mu.Unlock()
	if cwd == "" {
		cwd = "."
	}
	var working string
	var err error
	if trustedWorkdir {
		working, err = filepath.Abs(cwd)
	} else {
		working, err = securePath(manager.scope, cwd, false)
	}
	if err != nil {
		manager.releaseOpening()
		return TerminalSessionInfo{}, err
	}
	if info, statErr := os.Stat(working); statErr != nil || !info.IsDir() {
		manager.releaseOpening()
		if statErr != nil {
			return TerminalSessionInfo{}, statErr
		}
		return TerminalSessionInfo{}, errors.New("terminal cwd must be a directory")
	}
	shell := strings.TrimSpace(manager.config.Shell)
	if shell == "" {
		shell = "/bin/sh"
	}
	if !filepath.IsAbs(shell) {
		resolved, lookErr := exec.LookPath(shell)
		if lookErr != nil {
			manager.releaseOpening()
			return TerminalSessionInfo{}, lookErr
		}
		shell = resolved
	}
	program, argv := shell, []string(nil)
	program, argv, err = sandboxCommandInvocation(manager.scope.withSessionGrants(ctx).withSessionTmp(ctx).withNetworkPolicy(ctx), shell, nil, working)
	if err != nil {
		manager.releaseOpening()
		return TerminalSessionInfo{}, err
	}
	cmd := exec.Command(program, argv...)
	cmd.Dir = working
	if env == nil {
		setAllowlistedChildEnvironment(cmd, manager.scope.EnvAllowlist, manager.scope.EnforceWriteRoots)
	} else {
		cmd.Env = preserveProcessSandboxMarker(append([]string(nil), env...), manager.scope.EnforceWriteRoots)
	}
	var size *pty.Winsize
	if cols > 0 && rows > 0 {
		size = &pty.Winsize{Cols: cols, Rows: rows}
	}
	terminal, err := pty.StartWithSize(cmd, size)
	if err != nil {
		manager.mu.Lock()
		manager.opening--
		manager.mu.Unlock()
		return TerminalSessionInfo{}, fmt.Errorf("start terminal shell: %w", err)
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("term-%d", manager.sequence.Add(1))
	executionMode := ""
	if !manager.scope.EnforceWriteRoots {
		executionMode = "unsandboxed"
	}
	session := &terminalSession{id: id, cmd: cmd, pty: terminal, cwd: working, status: TerminalSessionRunning, executionMode: executionMode, started: now, lastUsed: now, done: make(chan struct{}), networked: !NetworkOffFromContext(ctx)}
	session.ownerSession, _ = SessionIDFromContext(ctx)
	manager.mu.Lock()
	manager.opening--
	active = 0
	for _, existing := range manager.sessions {
		if existing.isRunning() {
			active++
		}
	}
	if active >= manager.config.MaxSessions {
		manager.mu.Unlock()
		_ = killProcessGroup(cmd.Process.Pid)
		_ = terminal.Close()
		return TerminalSessionInfo{}, errors.New("terminal maximum session count reached")
	}
	manager.sessions[id] = session
	manager.mu.Unlock()
	go session.capture(manager)
	go func() {
		timer := time.NewTimer(time.Duration(manager.config.MaxLifetimeSeconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			session.closeWithReason("lifetime limit")
		case <-session.done:
		}
	}()
	manager.emit(ctx, TerminalAuditEvent{Time: now, Action: "open", SessionID: id, Source: "native_terminal"})
	return session.info(working), nil
}

func (manager *TerminalManager) releaseOpening() {
	manager.mu.Lock()
	if manager.opening > 0 {
		manager.opening--
	}
	manager.mu.Unlock()
}

func (manager *TerminalManager) get(id string) (*terminalSession, error) {
	manager.mu.RLock()
	session := manager.sessions[id]
	manager.mu.RUnlock()
	if session == nil {
		return nil, ErrTerminalSessionNotFound
	}
	return session, nil
}

// getForInput is get for actions that drive the terminal (write, run,
// resize, signal): a session whose network is off may not drive a terminal
// that still has network. Reading and closing stay allowed.
func (manager *TerminalManager) getForInput(ctx context.Context, id string) (*terminalSession, error) {
	session, err := manager.get(id)
	if err != nil {
		return nil, err
	}
	if NetworkOffFromContext(ctx) && session.networked {
		return nil, fmt.Errorf("%w; terminal %s was opened with network, so this session can only read or close it", ErrNetworkOff, id)
	}
	return session, nil
}

// CloseNetworkedForSession closes the running terminals owner opened while
// it had network (/network off) and returns how many.
func (manager *TerminalManager) CloseNetworkedForSession(ctx context.Context, owner SessionID) int {
	if manager == nil || owner == "" {
		return 0
	}
	manager.mu.RLock()
	var ids []string
	for id, session := range manager.sessions {
		if session.ownerSession == owner && session.networked && session.isRunning() {
			ids = append(ids, id)
		}
	}
	manager.mu.RUnlock()
	closed := 0
	for _, id := range ids {
		if manager.Close(ctx, id) == nil {
			closed++
		}
	}
	return closed
}

func (manager *TerminalManager) List() []TerminalSessionInfo {
	if manager == nil {
		return nil
	}
	manager.mu.RLock()
	sessions := make([]*terminalSession, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.RUnlock()
	out := make([]TerminalSessionInfo, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, session.info(""))
	}
	return out
}

func (manager *TerminalManager) Inspect(id string) (TerminalSessionInfo, error) {
	session, err := manager.get(id)
	if err != nil {
		return TerminalSessionInfo{}, err
	}
	return session.info(""), nil
}

func (manager *TerminalManager) Write(ctx context.Context, id, input string) error {
	if len(input) > manager.config.MaxInputBytes {
		return errors.New("terminal input exceeds configured bound")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session, err := manager.getForInput(ctx, id)
	if err != nil {
		return err
	}
	if err := session.write(input); err != nil {
		return err
	}
	manager.emit(ctx, TerminalAuditEvent{Time: time.Now().UTC(), Action: "write", SessionID: id, Source: "native_terminal"})
	return nil
}

func (manager *TerminalManager) Submit(ctx context.Context, id, command string, wait time.Duration) (TerminalReadResult, error) {
	if strings.TrimSpace(command) == "" {
		return TerminalReadResult{}, errors.New("terminal command is required")
	}
	if len(command) > manager.config.MaxInputBytes {
		return TerminalReadResult{}, errors.New("terminal command exceeds configured bound")
	}
	session, err := manager.getForInput(ctx, id)
	if err != nil {
		return TerminalReadResult{}, err
	}
	// Keep command submissions serialized so completion markers cannot be
	// confused between concurrent run calls. Raw writes, reads, signals, and
	// close remain available while a submitted command is running.
	session.submitMu.Lock()
	defer session.submitMu.Unlock()
	start := session.currentCursor()
	marker := fmt.Sprintf("__BEE_TERM_DONE_%d__", manager.sequence.Add(1))
	payload := command + "\nprintf '\\n" + marker + "%s\\n' \"$?\"\n"
	if err := session.write(payload); err != nil {
		return TerminalReadResult{}, err
	}
	manager.emit(ctx, TerminalAuditEvent{Time: time.Now().UTC(), Action: "run", SessionID: id, Command: command, Source: "native_terminal"})
	if wait <= 0 {
		wait = time.Duration(manager.config.MaxWaitMS) * time.Millisecond
	}
	maxWait := time.Duration(manager.config.MaxWaitMS) * time.Millisecond
	if maxWait > 0 && wait > maxWait {
		wait = maxWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		output := session.readFrom(start)
		if terminalCommandComplete(output.Output, marker) || session.isDone() {
			commandDone := terminalCommandComplete(output.Output, marker)
			output.Output = strings.ReplaceAll(output.Output, marker, "")
			if commandDone {
				output.Running = false
				output.Completed = true
			}
			return output, nil
		}
		select {
		case <-session.done:
			output := session.readFrom(start)
			output.Output = strings.ReplaceAll(output.Output, marker, "")
			return output, nil
		case <-deadline.C:
			result := session.readFrom(start)
			result.Output = strings.ReplaceAll(result.Output, marker, "")
			return result, nil
		case <-ctx.Done():
			output := session.readFrom(start)
			output.Output = strings.ReplaceAll(output.Output, marker, "")
			return output, nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func terminalCommandComplete(output, marker string) bool {
	for offset := 0; ; {
		index := strings.Index(output[offset:], marker)
		if index < 0 {
			return false
		}
		index += offset
		rest := output[index+len(marker):]
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits > 0 && strings.ContainsAny(rest[digits:], "\r\n") {
			return true
		}
		offset = index + len(marker)
	}
}

func (manager *TerminalManager) Read(_ context.Context, id string, cursor uint64) (TerminalReadResult, error) {
	session, err := manager.get(id)
	if err != nil {
		return TerminalReadResult{}, err
	}
	return session.readFrom(cursor), nil
}

// ReadWait returns immediately when output is already available after cursor
// or the session has exited. Otherwise it waits, up to the configured bound,
// for either condition. This keeps the terminal tool's wait_ms contract useful
// without allowing one read call to monopolize a tool worker indefinitely.
func (manager *TerminalManager) ReadWait(ctx context.Context, id string, cursor uint64, wait time.Duration) (TerminalReadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := manager.get(id)
	if err != nil {
		return TerminalReadResult{}, err
	}
	current := session.readFrom(cursor)
	if current.Output != "" || current.Completed || wait <= 0 {
		return current, nil
	}
	maxWait := time.Duration(manager.config.MaxWaitMS) * time.Millisecond
	if maxWait > 0 && wait > maxWait {
		wait = maxWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-session.done:
			return session.readFrom(cursor), nil
		case <-timer.C:
			return session.readFrom(cursor), nil
		case <-ctx.Done():
			return session.readFrom(cursor), nil
		case <-ticker.C:
			current = session.readFrom(cursor)
			if current.Output != "" || current.Completed {
				return current, nil
			}
		}
	}
}

func (manager *TerminalManager) Resize(ctx context.Context, id string, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("terminal resize requires positive cols and rows")
	}
	session, err := manager.getForInput(ctx, id)
	if err != nil {
		return err
	}
	if err := session.resize(cols, rows); err != nil {
		return err
	}
	manager.emit(ctx, TerminalAuditEvent{Time: time.Now().UTC(), Action: "resize", SessionID: id, Source: "native_terminal"})
	return nil
}

func (manager *TerminalManager) Signal(ctx context.Context, id string, signal os.Signal) error {
	session, err := manager.getForInput(ctx, id)
	if err != nil {
		return err
	}
	if err := session.signal(signal); err != nil {
		return err
	}
	manager.emit(ctx, TerminalAuditEvent{Time: time.Now().UTC(), Action: "signal", SessionID: id, Source: "native_terminal"})
	return nil
}
func (manager *TerminalManager) Interrupt(ctx context.Context, id string) error {
	return manager.Signal(ctx, id, os.Interrupt)
}
func (manager *TerminalManager) Close(ctx context.Context, id string) error {
	session, err := manager.get(id)
	if err != nil {
		return err
	}
	session.closeWithReason("closed")
	manager.emit(ctx, TerminalAuditEvent{Time: time.Now().UTC(), Action: "close", SessionID: id, Source: "native_terminal"})
	return nil
}

func (manager *TerminalManager) CloseAll() {
	if manager == nil {
		return
	}
	manager.mu.RLock()
	sessions := make([]*terminalSession, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.RUnlock()
	for _, session := range sessions {
		session.closeWithReason("manager closed")
	}
}

func (session *terminalSession) capture(manager *TerminalManager) {
	waitCh := make(chan error, 1)
	go func() {
		waitErr := session.cmd.Wait()
		waitCh <- waitErr
		// A descendant may retain the PTY after the shell exits. Closing the
		// master here ensures the reader cannot leak behind a dead shell.
		_ = session.pty.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := session.pty.Read(buf)
		if n > 0 {
			session.append(buf[:n], manager.config.MaxBufferBytes)
		}
		if err != nil {
			break
		}
	}
	waitErr := <-waitCh
	session.mu.Lock()
	if session.status == TerminalSessionRunning {
		session.status = TerminalSessionExited
	}
	if session.status == TerminalSessionClosed {
		session.closed = true
	}
	if session.cmd.ProcessState != nil {
		session.exitCode = session.cmd.ProcessState.ExitCode()
	}
	if waitErr != nil && session.status == TerminalSessionExited {
		session.exitError = waitErr.Error()
	}
	close(session.done)
	session.mu.Unlock()
}

func (session *terminalSession) append(data []byte, max int) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.buffer = append(session.buffer, data...)
	session.cursor += uint64(len(data))
	if len(session.buffer) > max {
		drop := len(session.buffer) - max
		session.buffer = append([]byte(nil), session.buffer[drop:]...)
		session.base += uint64(drop)
	}
	session.lastUsed = time.Now().UTC()
}
func (session *terminalSession) currentCursor() uint64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.cursor
}
func (session *terminalSession) isRunning() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.status == TerminalSessionRunning
}
func (session *terminalSession) isDone() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.status != TerminalSessionRunning
}
func (session *terminalSession) readFrom(cursor uint64) TerminalReadResult {
	session.mu.Lock()
	defer session.mu.Unlock()
	if cursor < session.base {
		cursor = session.base
	}
	if cursor > session.cursor {
		cursor = session.cursor
	}
	start := int(cursor - session.base)
	return TerminalReadResult{Output: string(session.buffer[start:]), Cursor: session.cursor, BaseCursor: session.base, Truncated: session.base > 0, Status: session.status, Running: session.status == TerminalSessionRunning, Completed: session.status != TerminalSessionRunning, ExitCode: session.exitCode}
}
func (session *terminalSession) write(input string) error {
	session.opMu.Lock()
	defer session.opMu.Unlock()
	session.mu.Lock()
	alive := session.status == TerminalSessionRunning
	session.lastUsed = time.Now().UTC()
	session.mu.Unlock()
	if !alive {
		return errors.New("terminal session is not running")
	}
	_, err := session.pty.Write([]byte(input))
	return err
}
func (session *terminalSession) resize(cols, rows uint16) error {
	session.opMu.Lock()
	defer session.opMu.Unlock()
	session.mu.Lock()
	alive := session.status == TerminalSessionRunning
	session.mu.Unlock()
	if !alive {
		return errors.New("terminal session is not running")
	}
	return pty.Setsize(session.pty, &pty.Winsize{Cols: cols, Rows: rows})
}
func (session *terminalSession) signal(signal os.Signal) error {
	session.opMu.Lock()
	defer session.opMu.Unlock()
	session.mu.Lock()
	alive := session.status == TerminalSessionRunning
	pid := 0
	if session.cmd.Process != nil {
		pid = session.cmd.Process.Pid
	}
	session.mu.Unlock()
	if !alive {
		return errors.New("terminal session is not running")
	}
	return signalProcessGroup(pid, signal)
}
func (session *terminalSession) closeWithReason(reason string) {
	session.opMu.Lock()
	defer session.opMu.Unlock()
	session.mu.Lock()
	if session.status != TerminalSessionRunning {
		session.mu.Unlock()
		return
	}
	session.status = TerminalSessionClosed
	pid := 0
	if session.cmd.Process != nil {
		pid = session.cmd.Process.Pid
	}
	session.exitError = reason
	session.mu.Unlock()
	_ = killProcessGroup(pid)
	_ = session.pty.Close()
}
func (session *terminalSession) info(cwd string) TerminalSessionInfo {
	session.mu.Lock()
	defer session.mu.Unlock()
	if cwd == "" {
		cwd = session.cwd
	}
	return TerminalSessionInfo{ID: session.id, Status: session.status, ExecutionMode: session.executionMode, Cwd: cwd, Shell: session.cmd.Path, StartedAt: session.started, LastUsedAt: session.lastUsed, Cursor: session.cursor, BaseCursor: session.base, ExitCode: session.exitCode, ExitError: session.exitError, Truncated: session.base > 0}
}

type terminalToolArgs struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id,omitempty"`
	Command   string `json:"command,omitempty"`
	Input     string `json:"input,omitempty"`
	Cursor    uint64 `json:"cursor,omitempty"`
	WaitMS    int    `json:"wait_ms,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
	Rows      uint16 `json:"rows,omitempty"`
	Signal    string `json:"signal,omitempty"`
}

type TerminalTool struct{ Manager *TerminalManager }

func (tool TerminalTool) Definition() ToolDefinition {
	return ToolDefinition{Name: TerminalToolName, Description: "Persistent bounded PTY shell for private/development use. Host paths are readable, while persistent writes remain confined to Bee's configured write roots; unrestricted host shells are never reusable terminal sessions. open creates a session and optionally starts command/input; run executes command/input and waits up to wait_ms for completion/output; submit sends command/input plus Enter and returns immediately; write sends raw input exactly; read returns bytes after cursor and waits up to wait_ms for new bytes. Reuse the returned session_id and latest cursor. Sessions are in-memory and disappear on process restart. Passwords sent through input may be recorded in tool-call history; prefer key authentication.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["open","run","submit","write","read","resize","signal","interrupt","close","list","inspect"]},"session_id":{"type":"string"},"command":{"type":"string","description":"Shell command for open, run, or submit. Use either command or input, not both."},"input":{"type":"string","description":"Command alias for open/run/submit, or exact raw bytes for write."},"cursor":{"type":"integer","minimum":0},"wait_ms":{"type":"integer","minimum":0,"maximum":60000},"cwd":{"type":"string"},"cols":{"type":"integer","minimum":0,"maximum":65535},"rows":{"type":"integer","minimum":0,"maximum":65535},"signal":{"type":"string"}},"required":["action"]}`)}
}
func (tool TerminalTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Manager == nil {
		return "", ErrTerminalDisabled
	}
	var input terminalToolArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("terminal arguments: %w", err)
	}
	if input.Action == "" {
		return "", errors.New("terminal action is required")
	}
	var result any
	var err error
	switch input.Action {
	case "open":
		var info TerminalSessionInfo
		info, err = tool.Manager.Open(ctx, input.Cwd, input.Cols, input.Rows)
		if err == nil {
			var command string
			var supplied bool
			command, supplied, err = terminalToolCommand(input)
			if err == nil && supplied {
				err = tool.Manager.Write(ctx, info.ID, terminalCommandInput(command))
				if err == nil {
					info, err = tool.Manager.Inspect(info.ID)
				}
			}
			if err != nil {
				_ = tool.Manager.Close(context.Background(), info.ID)
			}
		}
		result = info
	case "list":
		result = tool.Manager.List()
	case "inspect":
		result, err = tool.Manager.Inspect(input.SessionID)
	case "read":
		var wait time.Duration
		if input.WaitMS > 0 {
			wait = time.Duration(input.WaitMS) * time.Millisecond
		}
		result, err = tool.Manager.ReadWait(ctx, input.SessionID, input.Cursor, wait)
	case "write":
		err = tool.Manager.Write(ctx, input.SessionID, input.Input)
		result = map[string]any{"ok": err == nil}
	case "submit":
		var command string
		var supplied bool
		command, supplied, err = terminalToolCommand(input)
		if err == nil && !supplied {
			err = errors.New("terminal command is required")
		}
		if err == nil {
			err = tool.Manager.Write(ctx, input.SessionID, terminalCommandInput(command))
		}
		result = map[string]any{"ok": err == nil}
	case "run":
		var command string
		var supplied bool
		command, supplied, err = terminalToolCommand(input)
		if err == nil && !supplied {
			err = errors.New("terminal command is required")
		}
		if err == nil {
			var wait time.Duration
			if input.WaitMS > 0 {
				wait = time.Duration(input.WaitMS) * time.Millisecond
			}
			result, err = tool.Manager.Submit(ctx, input.SessionID, command, wait)
		}
	case "resize":
		err = tool.Manager.Resize(ctx, input.SessionID, input.Cols, input.Rows)
		result = map[string]any{"ok": err == nil}
	case "interrupt":
		err = tool.Manager.Interrupt(ctx, input.SessionID)
		result = map[string]any{"ok": err == nil}
	case "signal":
		var signal os.Signal
		signal, err = parseTerminalSignal(input.Signal)
		if err == nil {
			err = tool.Manager.Signal(ctx, input.SessionID, signal)
		}
		result = map[string]any{"ok": err == nil}
	case "close":
		err = tool.Manager.Close(ctx, input.SessionID)
		result = map[string]any{"ok": err == nil}
	default:
		return "", fmt.Errorf("terminal action %q is unsupported", input.Action)
	}
	if err != nil {
		return "", err
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(data), nil
}

func terminalToolCommand(input terminalToolArgs) (string, bool, error) {
	commandSet := input.Command != ""
	inputSet := input.Input != ""
	if commandSet && inputSet {
		return "", false, errors.New("provide only one of command or input")
	}
	command := input.Command
	if inputSet {
		command = input.Input
	}
	if !commandSet && !inputSet {
		return "", false, nil
	}
	if strings.TrimSpace(command) == "" {
		return "", false, errors.New("terminal command is required")
	}
	return command, true, nil
}

func terminalCommandInput(command string) string {
	if strings.HasSuffix(command, "\n") || strings.HasSuffix(command, "\r") {
		return command
	}
	return command + "\n"
}

func parseTerminalSignal(value string) (os.Signal, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SIGTERM", "TERM":
		return syscall.SIGTERM, nil
	case "SIGKILL", "KILL":
		return syscall.SIGKILL, nil
	case "SIGHUP", "HUP":
		return syscall.SIGHUP, nil
	case "SIGQUIT", "QUIT":
		return syscall.SIGQUIT, nil
	case "SIGINT", "INT":
		return os.Interrupt, nil
	default:
		return nil, fmt.Errorf("terminal signal %q is unsupported", value)
	}
}
