package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ExecToolName         = "exec"
	ExecReadOnlyToolName = "exec_readonly"
	HostExecToolName     = "host_exec"
	maxExecCommandSize   = 1 << 20
	maxExecEnvEntries    = 128
	maxExecEnvBytes      = 1 << 20
	maxExecYieldMS       = 60 * 1000
)

var execEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExecTool is the trusted foreground shell capability. It is deliberately a
// separate tool from the legacy argv/SSH run_command contract: the outer
// composition boundary is the authority decision, while this tool preserves
// ordinary shell and OpenSSH behavior without target rewriting.
type ExecTool struct {
	Scope         ExecutionScope
	Terminal      *TerminalManager
	ArtifactStore *ArtifactStore
	unrestricted  bool
	readOnly      bool
}

// ExecReadOnlyTool provides shell-shaped local inspection without persistent
// filesystem, network, or host-process mutation authority.
type ExecReadOnlyTool struct {
	Scope         ExecutionScope
	ArtifactStore *ArtifactStore
}

// HostExecTool is the explicit escape hatch for a command that genuinely must
// mutate outside Bee's configured write roots. It is foreground-only and every
// invocation requires human approval, including under trusted policy.
type HostExecTool struct {
	Scope         ExecutionScope
	ArtifactStore *ArtifactStore
}

type execArgs struct {
	Command     string            `json:"command"`
	Workdir     string            `json:"workdir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Timeout     int               `json:"timeout_ms,omitempty"`
	PTY         bool              `json:"pty,omitempty"`
	Background  bool              `json:"background,omitempty"`
	YieldMS     int               `json:"yield_ms,omitempty"`
	YieldTimeMS int               `json:"yield_time_ms,omitempty"`
}

type execResult struct {
	Status        string                     `json:"status"`
	ExecutionMode string                     `json:"execution_mode,omitempty"`
	ExitCode      *int                       `json:"exit_code,omitempty"`
	Output        string                     `json:"output,omitempty"`
	Truncated     bool                       `json:"truncated,omitempty"`
	Continuation  *CommandOutputContinuation `json:"continuation,omitempty"`
	Session       *TerminalSessionInfo       `json:"session,omitempty"`
	Read          *TerminalReadResult        `json:"read,omitempty"`
	// SandboxHint explains a "Read-only file system" failure: the write was
	// outside the write roots, and how to proceed instead of retrying blindly.
	SandboxHint string `json:"sandbox_hint,omitempty"`
}

// sandboxWriteHint returns guidance when sandboxed output shows a write that
// the write-root sandbox refused. Empty when not applicable.
func (tool ExecTool) sandboxWriteHint(ctx context.Context, output string) string {
	if tool.readOnly || tool.unrestricted || !tool.Scope.EnforceWriteRoots || !strings.Contains(output, "Read-only file system") {
		return ""
	}
	roots, err := tool.Scope.withSessionGrants(ctx).writeRootPaths()
	if err != nil || len(roots) == 0 {
		return ""
	}
	return "A write failed because its path is outside Bee's write roots (" + strings.Join(roots, ", ") + "); the sandbox makes everything else read-only, so retrying the same command will fail again. To change files elsewhere, use write_file (the user is asked to approve, and can allow that project directory for the rest of the session, after which exec can write there too); for a one-off host command the user approves, use host_exec; or ask the user to start Bee from that directory or pass --write-root."
}

// PlanFallbackTool can nominate a lower-authority capability when the plan
// gate denies the originally requested tool. The executor still authorizes
// and runs the nominated tool normally; this interface never weakens a tool's
// own sandbox or policy boundary.
type PlanFallbackTool interface {
	PlanFallback(json.RawMessage) (string, bool)
}

func (tool ExecTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        ExecToolName,
		Description: "Run a shell command with normal OpenSSH behavior and the host readable, but persistent writes confined to Bee's configured write roots. Relative paths and the default workdir use the preferred write root; /tmp is private to Bee and persists across exec calls in this session, but is not the host /tmp and is not visible to the file tools. For interactive or long-running commands use pty/background and terminal follow-ups, and set timeout_ms generously for builds and tests. To read or change a file you are working on, prefer read_file and edit_file over cat, sed, or heredocs: edits are exact, journaled, and undoable. Use host_exec only when the user explicitly approves an unrestricted host mutation. Do not use run_command target:ssh, sshpass, expect, or Python password drivers.",
		Risk:        RiskCritical,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"command":{"type":"string","minLength":1,"maxLength":1048576},"workdir":{"type":"string"},"env":{"type":"object","additionalProperties":{"type":"string"},"maxProperties":128},"timeout_ms":{"type":"integer","minimum":0,"maximum":14400000},"pty":{"type":"boolean"},"background":{"type":"boolean"},"yield_ms":{"type":"integer","minimum":0,"maximum":60000},"yield_time_ms":{"type":"integer","minimum":0,"maximum":60000}},"required":["command"]}`),
	}
}

// PlanFallback lets a mistaken exec selection retain inspection capability
// without trying to classify arbitrary shell syntax. Only invocation shapes
// supported by exec_readonly can be downgraded; the read-only process sandbox
// is the actual security boundary.
func (tool ExecTool) PlanFallback(args json.RawMessage) (string, bool) {
	var input execArgs
	if err := parseArgs(args, &input); err != nil {
		return "", false
	}
	if input.PTY || input.Background || input.YieldMS > 0 || input.YieldTimeMS > 0 {
		return "", false
	}
	return ExecReadOnlyToolName, true
}

func (tool ExecReadOnlyTool) Definition() ToolDefinition {
	definition := (ExecTool{}).Definition()
	definition.Name = ExecReadOnlyToolName
	definition.Description = "Run one synchronous local inspection command with the host filesystem read-only, a private ephemeral /tmp, and host network/process namespaces unavailable. Use for shell pipelines that only inspect local state. PTY, background, yield, SSH, downloads, and persistent writes are unavailable. Use exec for mutation-capable commands; in plan mode exec requires an active plan."
	definition.Risk = RiskLow
	definition.Mutability = ToolMutabilityReadOnly
	return definition
}

func (tool ExecReadOnlyTool) SelfBoundedOutputLimit() int {
	return (ExecTool{Scope: tool.Scope}).SelfBoundedOutputLimit()
}

func (ExecReadOnlyTool) ExecutionTimeout() time.Duration { return maxToolExecutionTimeout }

func (tool ExecReadOnlyTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input execArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("exec_readonly arguments: %w", err)
	}
	if input.PTY || input.Background || input.YieldMS > 0 || input.YieldTimeMS > 0 {
		return "", errors.New("exec_readonly is synchronous foreground-only")
	}
	return (ExecTool{Scope: tool.Scope, ArtifactStore: tool.ArtifactStore, readOnly: true}).Execute(ctx, args)
}

func (tool HostExecTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        HostExecToolName,
		Description: "Run one synchronous unrestricted host shell command outside Bee's write-root sandbox. This is an approval-only escape hatch for an explicitly intended host mutation; prefer exec for ordinary work. PTY, background, and yielded sessions are forbidden so each unrestricted mutation receives a fresh approval.",
		Risk:        RiskCritical,
		Mutability:  ToolMutabilityMutating,
		Parameters:  (ExecTool{}).Definition().Parameters,
	}
}

func (HostExecTool) RequiresExplicitApproval() bool { return true }

func (tool HostExecTool) SelfBoundedOutputLimit() int {
	return (ExecTool{Scope: tool.Scope}).SelfBoundedOutputLimit()
}

func (HostExecTool) ExecutionTimeout() time.Duration { return maxToolExecutionTimeout }

func (tool HostExecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input execArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("host_exec arguments: %w", err)
	}
	if input.PTY || input.Background || input.YieldMS > 0 || input.YieldTimeMS > 0 {
		return "", errors.New("host_exec is synchronous foreground-only")
	}
	return (ExecTool{Scope: tool.Scope, ArtifactStore: tool.ArtifactStore, unrestricted: true}).Execute(ctx, args)
}

func (tool ExecTool) SelfBoundedOutputLimit() int {
	limit := tool.Scope.maxOutput()
	if limit > MaxSelfBoundedOutputBytes {
		return MaxSelfBoundedOutputBytes
	}
	return limit
}

func (tool ExecTool) ExecutionTimeout() time.Duration {
	return maxToolExecutionTimeout
}

func (tool ExecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if !tool.Scope.TrustedForeground {
		return "", errors.New("exec is available only to the trusted foreground assistant")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var input execArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("exec arguments: %w", err)
	}
	if strings.TrimSpace(input.Command) == "" {
		return "", errors.New("command is required")
	}
	if strings.IndexByte(input.Command, 0) >= 0 {
		return "", errors.New("command contains NUL")
	}
	if len(input.Command) > maxExecCommandSize {
		return "", fmt.Errorf("command exceeds %d bytes", maxExecCommandSize)
	}
	if input.Timeout < 0 || input.Timeout > int(maxToolExecutionTimeout/time.Millisecond) {
		return "", errors.New("timeout_ms is outside the supported bound")
	}
	if input.YieldTimeMS > 0 {
		if input.YieldMS > 0 {
			return "", errors.New("provide only one of yield_ms or yield_time_ms")
		}
		input.YieldMS = input.YieldTimeMS
	}
	if input.YieldMS < 0 || input.YieldMS > maxExecYieldMS {
		return "", errors.New("yield_ms is outside the supported bound")
	}
	env, err := validateExecEnv(input.Env)
	if err != nil {
		return "", err
	}
	env, err = withBeeExecPaths(env, tool.Scope)
	if err != nil {
		return "", err
	}
	if tool.readOnly {
		env = readOnlyExecEnv(env)
	}
	workdir, err := execWorkdir(tool.Scope, input.Workdir)
	if err != nil {
		return "", err
	}
	if input.PTY || input.Background || input.YieldMS > 0 {
		return tool.executeTerminal(ctx, input, workdir, env)
	}
	return tool.executeForeground(ctx, input, workdir, env)
}

// readOnlyExecEnv keeps ordinary inspection utilities usable when /run is
// hidden. On NixOS, PATH often reaches the system profile through
// /run/current-system; resolving directory symlinks before entering the
// sandbox retains those immutable store paths. Host IPC/display agent
// endpoints are removed because AF_UNIX sockets are not blocked by a network
// namespace.
func readOnlyExecEnv(env []string) []string {
	blocked := map[string]struct{}{
		"DBUS_SESSION_BUS_ADDRESS": {},
		"DISPLAY":                  {},
		"GPG_AGENT_INFO":           {},
		"SSH_AUTH_SOCK":            {},
		"WAYLAND_DISPLAY":          {},
	}
	result := make([]string, 0, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, remove := blocked[key]; remove {
			continue
		}
		if key == "PATH" {
			entries := filepath.SplitList(value)
			seen := make(map[string]struct{}, len(entries))
			resolved := entries[:0]
			for _, entry := range entries {
				candidate := entry
				if canonical, err := filepath.EvalSymlinks(entry); err == nil {
					candidate = canonical
				}
				if _, duplicate := seen[candidate]; duplicate {
					continue
				}
				seen[candidate] = struct{}{}
				resolved = append(resolved, candidate)
			}
			value = strings.Join(resolved, string(os.PathListSeparator))
		}
		result = append(result, key+"="+value)
	}
	return result
}

func validateExecEnv(values map[string]string) ([]string, error) {
	if len(values) > maxExecEnvEntries {
		return nil, fmt.Errorf("env exceeds %d entries", maxExecEnvEntries)
	}
	result := append([]string(nil), os.Environ()...)
	positions := make(map[string]int, len(result))
	for index, item := range result {
		if key, _, ok := strings.Cut(item, "="); ok {
			positions[key] = index
		}
	}
	total := 0
	for key, value := range values {
		if !execEnvKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("env key %q is invalid", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("env value for %q contains NUL", key)
		}
		item := key + "=" + value
		total += len(item)
		if index, ok := positions[key]; ok {
			result[index] = item
		} else {
			positions[key] = len(result)
			result = append(result, item)
		}
	}
	if total > maxExecEnvBytes {
		return nil, fmt.Errorf("env overlay exceeds %d bytes", maxExecEnvBytes)
	}
	return result, nil
}

func withBeeExecPaths(env []string, scope ExecutionScope) ([]string, error) {
	writeRoot, err := scope.preferredRootPath()
	if err != nil {
		return nil, err
	}
	values := map[string]string{"BEE_WRITE_ROOT": writeRoot}
	if launch := strings.TrimSpace(scope.LaunchRoot); launch != "" {
		values["BEE_LAUNCH_DIR"] = launch
	}
	if workspace, workspaceErr := DefaultBeeWorkspaceRoot(); workspaceErr == nil {
		values["BEE_WORKSPACE"] = workspace
	}
	if projects, projectsErr := DefaultBeeProjectsRoot(); projectsErr == nil {
		values["BEE_PROJECTS"] = projects
	}
	if memory, memoryErr := DefaultBeeMemoryRoot(); memoryErr == nil {
		values["BEE_MEMORY"] = memory
	}
	positions := make(map[string]int, len(env))
	for index, item := range env {
		if key, _, ok := strings.Cut(item, "="); ok {
			positions[key] = index
		}
	}
	for key, value := range values {
		item := key + "=" + value
		if index, ok := positions[key]; ok {
			env[index] = item
		} else {
			positions[key] = len(env)
			env = append(env, item)
		}
	}
	return env, nil
}

func execWorkdir(scope ExecutionScope, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		var err error
		requested, err = scope.preferredRootPath()
		if err != nil {
			return "", err
		}
	} else if !filepath.IsAbs(requested) {
		root, err := scope.preferredRootPath()
		if err != nil {
			return "", err
		}
		requested = filepath.Join(root, requested)
	}
	if strings.IndexByte(requested, 0) >= 0 {
		return "", errors.New("workdir contains NUL")
	}
	resolved, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve workdir: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workdir: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workdir must be a directory")
	}
	return resolved, nil
}

func (tool ExecTool) executeForeground(parent context.Context, input execArgs, workdir string, env []string) (string, error) {
	ctx := parent
	cancel := func() {}
	if input.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(input.Timeout)*time.Millisecond)
	}
	defer cancel()
	shell, err := trustedBashPath()
	if err != nil {
		return "", err
	}
	program, argv := shell, []string{"-lc", input.Command}
	if tool.readOnly {
		// Inspection does not need login-shell startup files. Avoid letting a
		// profile replace the canonicalized PATH or contact a host-side agent.
		argv = []string{"-c", input.Command}
		var sandboxErr error
		program, argv, sandboxErr = sandboxReadOnlyCommandInvocation(tool.Scope.withSessionGrants(ctx).withSessionTmp(ctx), shell, argv, workdir)
		if sandboxErr != nil {
			return "", sandboxErr
		}
	} else if !tool.unrestricted {
		var sandboxErr error
		program, argv, sandboxErr = sandboxCommandInvocation(tool.Scope.withSessionGrants(ctx).withSessionTmp(ctx).withNetworkPolicy(ctx), shell, argv, workdir)
		if sandboxErr != nil {
			return "", sandboxErr
		}
	}
	command := exec.Command(program, argv...)
	command.Dir = workdir
	command.Env = preserveProcessSandboxMarker(env, tool.readOnly || (!tool.unrestricted && tool.Scope.EnforceWriteRoots))
	configureCommandProcessGroup(command)
	limit := tool.execOutputLimit()
	capture := newCommandCapture(limit, commandArtifactContextAvailable(ctx, tool.ArtifactStore))
	command.Stdout, command.Stderr = &capture.stdout, &capture.stderr
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start exec: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	var runErr error
	select {
	case runErr = <-waitCh:
	case <-ctx.Done():
		_ = terminateCommandProcessGroup(command)
		runErr = <-waitCh
		if runErr == nil {
			runErr = ctx.Err()
		} else {
			runErr = errors.Join(ctx.Err(), runErr)
		}
	}
	output, truncated := capture.visibleOutput(limit)
	var continuation *CommandOutputContinuation
	if truncated {
		continuation = retainCommandOutput(ctx, tool.ArtifactStore, &capture)
	}
	code := -1
	if command.ProcessState != nil {
		code = command.ProcessState.ExitCode()
	}
	result := execResult{Status: execStatus(runErr, ctx), ExitCode: &code, Output: output, Truncated: truncated, Continuation: continuation}
	if code != 0 {
		result.SandboxHint = tool.sandboxWriteHint(ctx, output)
	}
	encoded, encodeErr := tool.marshalResult(result)
	if encodeErr != nil {
		return "", encodeErr
	}
	if ctx.Err() != nil {
		return string(encoded), ctx.Err()
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return string(encoded), runErr
	}
	return string(encoded), nil
}

func execStatus(err error, ctx context.Context) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	if err == nil {
		return "exited"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "exited_nonzero"
	}
	return "failed"
}

func (tool ExecTool) executeTerminal(ctx context.Context, input execArgs, workdir string, env []string) (string, error) {
	if tool.Terminal == nil {
		return "", errors.New("exec pty/background mode requires terminal capability")
	}
	info, err := tool.Terminal.OpenSandboxedWithEnv(ctx, workdir, 0, 0, env)
	if err != nil {
		return "", err
	}
	if input.Timeout > 0 {
		timeout := time.Duration(input.Timeout) * time.Millisecond
		go func(sessionID string) {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			<-timer.C
			_ = tool.Terminal.Close(context.Background(), sessionID)
		}(info.ID)
	}
	shell, err := trustedBashPath()
	if err != nil {
		_ = tool.Terminal.Close(ctx, info.ID)
		return "", err
	}
	commandLine := "exec " + shellSingleQuote(shell) + " -lc " + shellSingleQuote(input.Command) + "\n"
	if input.Background {
		if err := tool.Terminal.Write(ctx, info.ID, commandLine); err != nil {
			_ = tool.Terminal.Close(ctx, info.ID)
			return "", err
		}
		updated, inspectErr := tool.Terminal.Inspect(info.ID)
		if inspectErr == nil {
			info = updated
		}
		status := "running"
		if info.Status != TerminalSessionRunning {
			status = "exited"
		}
		result := execResult{Status: status, Session: &info}
		encoded, encodeErr := tool.marshalResult(result)
		return string(encoded), encodeErr
	}
	if err := tool.Terminal.Write(ctx, info.ID, commandLine); err != nil {
		_ = tool.Terminal.Close(ctx, info.ID)
		return "", err
	}
	wait := time.Duration(input.YieldMS) * time.Millisecond
	if wait <= 0 {
		wait = time.Duration(tool.Terminal.config.MaxWaitMS) * time.Millisecond
	}
	if wait > time.Duration(maxExecYieldMS)*time.Millisecond {
		wait = time.Duration(maxExecYieldMS) * time.Millisecond
	}
	start := info.Cursor
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var read TerminalReadResult
	for {
		read, err = tool.Terminal.Read(ctx, info.ID, start)
		if err != nil {
			return "", err
		}
		if !read.Running {
			break
		}
		select {
		case <-deadline.C:
			goto done
		case <-ctx.Done():
			_ = tool.Terminal.Close(context.Background(), info.ID)
			goto done
		case <-time.After(10 * time.Millisecond):
		}
	}
done:
	updated, inspectErr := tool.Terminal.Inspect(info.ID)
	if inspectErr == nil {
		info = updated
	}
	status := "running"
	if read.Completed {
		status = "exited"
	}
	output, outputTruncated := truncateOutput(read.Output, tool.execOutputLimit())
	readSummary := read
	readSummary.Output = ""
	result := execResult{Status: status, Session: &info, Read: &readSummary, Output: output, Truncated: read.Truncated || outputTruncated, SandboxHint: tool.sandboxWriteHint(ctx, output)}
	encoded, encodeErr := tool.marshalResult(result)
	return string(encoded), encodeErr
}

func trustedBashPath() (string, error) {
	path, err := exec.LookPath("bash")
	if err != nil {
		return "", errors.New("bash is unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", errors.New("bash path is unavailable")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("bash executable is unavailable")
	}
	return path, nil
}

func (tool ExecTool) marshalResult(result execResult) ([]byte, error) {
	if tool.readOnly {
		result.ExecutionMode = "read_only"
	} else if !tool.unrestricted && !tool.Scope.EnforceWriteRoots {
		result.ExecutionMode = "unsandboxed"
	}
	limit := tool.SelfBoundedOutputLimit()
	if limit <= 0 {
		return nil, errors.New("exec output limit is invalid")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= limit {
		return encoded, nil
	}
	result.Truncated = true
	original := result.Output
	low, high := 0, len(original)
	for low < high {
		mid := low + (high-low+1)/2
		result.Output = truncateUTF8(original, mid)
		candidate, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(candidate) <= limit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	result.Output = truncateUTF8(original, low)
	encoded, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(encoded) > limit {
		return nil, fmt.Errorf("exec result metadata exceeds %d-byte output bound", limit)
	}
	return encoded, nil
}

func (tool ExecTool) execOutputLimit() int {
	limit := tool.Scope.maxOutput()
	const envelopeReserve = 4 << 10
	if limit > envelopeReserve {
		return limit - envelopeReserve
	}
	if limit > 1 {
		return limit / 2
	}
	return limit
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
