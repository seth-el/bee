package bee

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type bubblewrapLookupFunc func(string) (string, error)
type bubblewrapProbeFunc func(string) (bool, error)
type bubblewrapExistsFunc func(string) error

const (
	processSandboxMarker      = "BEE_SANDBOX"
	processSandboxMarkerValue = "1"
)

type bubblewrapAvailability struct {
	once sync.Once
	path string
	err  error
}

var (
	processBubblewrapAvailability bubblewrapAvailability
	processSandboxWarningOnce     = &sync.Once{}
)

func (availability *bubblewrapAvailability) resolve() (string, error) {
	availability.once.Do(func() {
		availability.path, availability.err = detectBubblewrap(exec.LookPath, probeBubblewrap, bubblewrapPathExists)
	})
	return availability.path, availability.err
}

func probeBubblewrap(path string) (bool, error) {
	command := exec.Command(path, "--version")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return false, err
	}
	return true, command.Wait()
}

func bubblewrapPathExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func detectBubblewrap(lookup bubblewrapLookupFunc, probe bubblewrapProbeFunc, exists bubblewrapExistsFunc) (string, error) {
	path, err := lookup("bwrap")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return "", errors.New("bubblewrap (bwrap) was not found in PATH; on NixOS, add pkgs.bubblewrap to environment.systemPackages; Bee did not run the command unsandboxed (set assistant.unsafe_disable_process_sandbox=true only to accept that security downgrade explicitly)")
		}
		return "", fmt.Errorf("look up bubblewrap (bwrap): %w; inspect PATH and executable permissions; Bee did not run the command unsandboxed", err)
	}
	started, probeErr := probe(path)
	if probeErr == nil {
		return path, nil
	}
	if !started && errors.Is(probeErr, os.ErrNotExist) && exists(path) == nil {
		return "", fmt.Errorf("bubblewrap was found at %q but the operating system could not load it; on NixOS this indicates a missing dynamic loader rather than a missing bwrap executable, so enable programs.nix-ld or use a Nix-built pkgs.bubblewrap; Bee did not run the command unsandboxed", path)
	}
	if !started {
		return "", fmt.Errorf("bubblewrap was found at %q but could not be started: %w; inspect the executable, permissions, and its Nix runtime closure; Bee did not run the command unsandboxed", path, probeErr)
	}
	return "", fmt.Errorf("bubblewrap at %q started but its --version check failed: %w; verify the installed bwrap executable; Bee did not run the command unsandboxed", path, probeErr)
}

func insideBeeProcessSandbox() bool {
	return os.Getenv(processSandboxMarker) == processSandboxMarkerValue
}

func warnNestedProcessSandbox() {
	processSandboxWarningOnce.Do(func() {
		log.Print("process sandbox: already inside a Bee sandbox; running unsandboxed-within-sandbox")
	})
}

// preserveProcessSandboxMarker keeps the advisory nesting marker on a direct
// child invocation when an already-sandboxed Bee skips a structurally
// impossible second Bubblewrap layer. First-level Bubblewrap invocations set
// and overwrite the marker themselves with --setenv.
func preserveProcessSandboxMarker(env []string, sandboxed bool) []string {
	if !sandboxed || !insideBeeProcessSandbox() {
		return env
	}
	marked := processSandboxMarker + "=" + processSandboxMarkerValue
	result := append([]string(nil), env...)
	for index, item := range result {
		key, _, ok := strings.Cut(item, "=")
		if ok && key == processSandboxMarker {
			result[index] = marked
			return result
		}
	}
	return append(result, marked)
}

// sandboxCommandInvocation returns a Bubblewrap invocation that exposes the
// host filesystem read-only, then rebinds only Bee's configured write roots as
// writable. Network and process access remain unchanged. /tmp is private and
// ephemeral unless a configured write root itself lives below /tmp (the common
// test/development case), where the host /tmp remains read-only except for that
// explicit rebind.
func sandboxCommandInvocation(scope ExecutionScope, executable string, args []string, workdir string) (string, []string, error) {
	if !scope.EnforceWriteRoots {
		if scope.NetworkOff {
			return "", nil, fmt.Errorf("%w; the process sandbox that would remove it is disabled (assistant.unsafe_disable_process_sandbox)", ErrNetworkOff)
		}
		return executable, append([]string(nil), args...), nil
	}
	if runtime.GOOS != "linux" {
		return "", nil, errors.New("write-root process sandboxing is supported only on Linux")
	}
	if err := scope.Validate(); err != nil {
		return "", nil, err
	}
	nested := insideBeeProcessSandbox()
	bwrap := ""
	if !nested {
		var err error
		bwrap, err = processBubblewrapAvailability.resolve()
		if err != nil {
			return "", nil, fmt.Errorf("write-root process sandbox unavailable: %w", err)
		}
	}
	program := executable
	var err error
	if !filepath.IsAbs(program) {
		program, err = exec.LookPath(program)
		if err != nil {
			return "", nil, fmt.Errorf("resolve sandbox command %q: %w", executable, err)
		}
	}
	if workdir == "" {
		workdir, err = scope.preferredRootPath()
		if err != nil {
			return "", nil, err
		}
	}
	workdir, err = filepath.Abs(workdir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox workdir: %w", err)
	}
	writeRoots, err := scope.writeRootPaths()
	if err != nil {
		return "", nil, err
	}
	if nested {
		if scope.NetworkOff {
			// The outer sandbox decides the network; this one can't remove it.
			return "", nil, fmt.Errorf("%w; Bee is already inside a sandbox, so it can't remove network from this command", ErrNetworkOff)
		}
		warnNestedProcessSandbox()
		return program, append([]string(nil), args...), nil
	}
	bwrapArgs := []string{
		"--die-with-parent", "--unshare-user", "--cap-drop", "ALL",
		"--setenv", processSandboxMarker, processSandboxMarkerValue,
		"--ro-bind", "/", "/", "--dev", "/dev",
	}
	if scope.NetworkOff {
		// /network off: a private, empty network namespace (loopback only),
		// the same isolation exec_readonly always has.
		bwrapArgs = append(bwrapArgs, "--unshare-net")
	}
	tmp := filepath.Clean("/tmp")
	writeRootInTmp := false
	for _, root := range writeRoots {
		if filepath.Clean(root) == tmp || scopeWithin(tmp, root) {
			writeRootInTmp = true
			break
		}
	}
	if !writeRootInTmp {
		if dir := scope.SandboxTmp; dir != "" && filepath.IsAbs(dir) {
			bwrapArgs = append(bwrapArgs, "--bind", dir, tmp)
		} else {
			bwrapArgs = append(bwrapArgs, "--tmpfs", tmp)
		}
	}
	for _, root := range writeRoots {
		bwrapArgs = append(bwrapArgs, "--bind", root, root)
	}
	// Hides go after the write-root binds so a write root that contains them
	// (e.g. ~/.bee) cannot re-expose them.
	bwrapArgs = append(bwrapArgs, sandboxRuntimeDirArgs()...)
	bwrapArgs = append(bwrapArgs, sandboxSecretsArgs()...)
	bwrapArgs = append(bwrapArgs, "--chdir", workdir, "--", program)
	bwrapArgs = append(bwrapArgs, args...)
	return bwrap, bwrapArgs, nil
}

// sandboxRuntimeDirArgs hides the user runtime directory (XDG_RUNTIME_DIR,
// normally /run/user/<uid>). It holds control sockets for the systemd user
// manager and session bus, tmux, editors, and similar daemons. Connecting to
// a Unix socket works on a read-only mount, so leaving them visible lets a
// sandboxed command ask an unsandboxed process (`systemd-run --user`, `tmux
// send-keys`, `emacsclient --eval`) to write outside the write roots. Only an
// ssh-agent socket inside it is re-exposed, so OpenSSH behavior is unchanged.
func sandboxRuntimeDirArgs() []string {
	runtimeDir := sandboxUserRuntimeDir()
	if runtimeDir == "" {
		return nil
	}
	args := []string{"--tmpfs", runtimeDir}
	if agent := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK")); agent != "" && filepath.IsAbs(agent) {
		agent = filepath.Clean(agent)
		if scopeWithin(runtimeDir, agent) {
			if info, err := os.Stat(agent); err == nil && info.Mode()&os.ModeSocket != 0 {
				args = append(args, "--ro-bind", agent, agent)
			}
		}
	}
	return args
}

func sandboxUserRuntimeDir() string {
	candidates := []string{strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))}
	if runtime.GOOS == "linux" {
		candidates = append(candidates, fmt.Sprintf("/run/user/%d", os.Getuid()))
	}
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		candidate = filepath.Clean(candidate)
		switch candidate {
		case "/", "/run", "/tmp", "/home", "/var":
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// sandboxSecretsArgs hides Bee's secrets directory (provider keys, serve
// tokens) from every sandboxed command. Bee reads those files itself, in the
// harness, never through a model-driven process; a sandboxed command that can
// read them and also reach the network could send them elsewhere.
func sandboxSecretsArgs() []string {
	secrets, err := DefaultBeeSecretsRoot()
	if err != nil {
		return nil
	}
	secrets = filepath.Clean(secrets)
	if info, err := os.Stat(secrets); err != nil || !info.IsDir() {
		return nil
	}
	return []string{"--tmpfs", secrets}
}

// sandboxReadOnlyCommandInvocation is the local-inspection shell boundary. It
// exposes the host filesystem read-only, gives the command a private temporary
// directory, and removes host network/process namespaces. Unlike exec, it
// never rebinds a persistent write root.
func sandboxReadOnlyCommandInvocation(scope ExecutionScope, executable string, args []string, workdir string) (string, []string, error) {
	if runtime.GOOS != "linux" {
		return "", nil, errors.New("read-only process sandboxing is supported only on Linux")
	}
	if err := scope.Validate(); err != nil {
		return "", nil, err
	}
	nested := scope.EnforceWriteRoots && insideBeeProcessSandbox()
	bwrap := ""
	if !nested {
		var err error
		bwrap, err = processBubblewrapAvailability.resolve()
		if err != nil {
			return "", nil, fmt.Errorf("read-only process sandbox unavailable: %w", err)
		}
	}
	program := executable
	var err error
	if !filepath.IsAbs(program) {
		program, err = exec.LookPath(program)
		if err != nil {
			return "", nil, fmt.Errorf("resolve sandbox command %q: %w", executable, err)
		}
	}
	// NixOS commonly resolves bash through /run/current-system. /run is hidden
	// below so Unix sockets and other host runtime state are not reachable;
	// canonicalize the executable first so the sandbox can still launch it.
	program, err = filepath.EvalSymlinks(program)
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox command %q: %w", executable, err)
	}
	if workdir == "" {
		workdir, err = scope.preferredRootPath()
		if err != nil {
			return "", nil, err
		}
	}
	workdir, err = filepath.Abs(workdir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve sandbox workdir: %w", err)
	}
	if nested {
		warnNestedProcessSandbox()
		return program, append([]string(nil), args...), nil
	}
	bwrapArgs := []string{
		"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc",
		"--cap-drop", "ALL", "--setenv", processSandboxMarker, processSandboxMarkerValue,
		"--ro-bind", "/", "/", "--dev", "/dev",
		"--proc", "/proc", "--tmpfs", "/tmp",
	}
	// Show the session's persistent /tmp read-only, so inspection can read
	// what an earlier exec call left there. Skipped when the workdir itself is
	// below /tmp, which needs the writable tmpfs to recreate its parents.
	if dir := scope.SandboxTmp; dir != "" && filepath.IsAbs(dir) && !scopeWithin("/tmp", workdir) {
		bwrapArgs = append(bwrapArgs, "--ro-bind", dir, "/tmp")
	}
	// Test/development scopes commonly live below /tmp. Recreate the selected
	// workdir as a read-only mount inside the otherwise private temporary tree.
	// This preserves absolute and relative inspection paths without making any
	// host /tmp location writable or visible beyond the chosen workdir.
	if filepath.Clean(workdir) != filepath.Clean("/tmp") && scopeWithin("/tmp", workdir) {
		parent := filepath.Dir(workdir)
		relative, relErr := filepath.Rel("/tmp", parent)
		if relErr != nil {
			return "", nil, fmt.Errorf("resolve sandbox workdir parents: %w", relErr)
		}
		current := filepath.Clean("/tmp")
		if relative != "." {
			for _, component := range strings.Split(relative, string(filepath.Separator)) {
				current = filepath.Join(current, component)
				bwrapArgs = append(bwrapArgs, "--dir", current)
			}
		}
		bwrapArgs = append(bwrapArgs, "--ro-bind", workdir, workdir)
	}
	bwrapArgs = append(bwrapArgs, "--tmpfs", "/run")
	// /run is already hidden here; a runtime directory elsewhere is not.
	if runtimeDir := sandboxUserRuntimeDir(); runtimeDir != "" && !scopeWithin("/run", runtimeDir) {
		bwrapArgs = append(bwrapArgs, "--tmpfs", runtimeDir)
	}
	bwrapArgs = append(bwrapArgs, sandboxSecretsArgs()...)
	bwrapArgs = append(bwrapArgs, "--chdir", workdir, "--", program)
	bwrapArgs = append(bwrapArgs, args...)
	return bwrap, bwrapArgs, nil
}
