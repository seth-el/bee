package bee

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultBeeHomeDirName = ".bee"

// DefaultBeeHome is Bee's user-owned root. BEE_HOME is an explicit escape
// hatch for tests, portable installs, and alternate profiles.
func DefaultBeeHome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("BEE_HOME")); configured != "" {
		abs, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve BEE_HOME: %w", err)
		}
		return filepath.Clean(abs), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, DefaultBeeHomeDirName), nil
}

func DefaultBeeWorkspaceRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspace"), nil
}

// DefaultBeeSkillsRoot is the private, versioned package registry used by
// instruction skills and optional extension-backed skills.
func DefaultBeeSkillsRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skills"), nil
}

// DefaultBeePluginsRoot is the private registry for versioned external-process
// plugin packages.
func DefaultBeePluginsRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "plugins"), nil
}

// DefaultBeeProjectsRoot is Bee's canonical root for project workspace
// directories. Project metadata remains in the session store; only opt-in
// project directory creation uses this location.
func DefaultBeeProjectsRoot() (string, error) {
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(workspace, "projects"), nil
}

// EnsureBeeProjectsRoot creates and protects Bee's canonical project root.
func EnsureBeeProjectsRoot() (string, error) {
	if _, err := EnsureBeeLayout(); err != nil {
		return "", err
	}
	return DefaultBeeProjectsRoot()
}

func DefaultBeeMemoryRoot() (string, error) {
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(workspace, "memory"), nil
}

func DefaultBeeStateRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "state"), nil
}

func DefaultBeeRuntimeDiagnosticsPath() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "diagnostics", "runtime.jsonl"), nil
}

// DefaultBeeArtifactRoot is Bee's durable, user-owned artifact payload root.
// It is deliberately separate from session JSONL and project directories.
func DefaultBeeArtifactRoot() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "artifacts"), nil
}

// DefaultBeeJobStorePath is the private durable store used by the composed
// assistant's model-facing foreground job tools.
func DefaultBeeJobStorePath() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "jobs.json"), nil
}

// DefaultBeeTodoStorePath is the private durable store used by the ordinary
// parent todo ledger. Todos are deliberately separate from sessions, memory,
// jobs, and coding plans.
func DefaultBeeTodoStorePath() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "todos.json"), nil
}

// DefaultBeeNotificationStorePath is the private durable job notification
// outbox/inbox snapshot used by the web gateway and foreground job runner.
func DefaultBeeNotificationStorePath() (string, error) {
	jobStore, err := DefaultBeeJobStorePath()
	if err != nil {
		return "", err
	}
	return NotificationStorePathForJobStore(jobStore), nil
}

// DefaultBeeOpenAIRouteBindingPath stores the durable mapping from external
// X-Session-Id conversation IDs to Bee session IDs.
func DefaultBeeOpenAIRouteBindingPath() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "openai-http-routes.json"), nil
}

// NotificationStorePathForJobStore keeps foreground CLI and gateway
// compositions on the same private notification store for a given job store.
func NotificationStorePathForJobStore(jobStorePath string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(jobStorePath)), "notifications.json")
}

// DefaultBeeSessionRoot is the session store Bee opens by default:
// BEE_SESSION_ROOT when set, otherwise $BEE_HOME/state/sessions (normally
// ~/.bee/state/sessions). XDG_DATA_HOME no longer chooses the store. One
// transition rule keeps existing installs working until they migrate: if
// the legacy XDG store (LegacyBeeSessionRoot) holds sessions and the
// configured root holds none, the legacy store is still used. `bee sessions
// migrate` moves it; `bee doctor` reports either state.
func DefaultBeeSessionRoot() (string, error) {
	configured, explicit, err := ConfiguredBeeSessionRoot()
	if err != nil || explicit {
		return configured, err
	}
	if legacy := LegacyBeeSessionRoot(); legacy != "" && SessionRootHasSessions(legacy) && !SessionRootHasSessions(configured) {
		return legacy, nil
	}
	return configured, nil
}

// ConfiguredBeeSessionRoot is where sessions belong: BEE_SESSION_ROOT
// (explicit=true) or $BEE_HOME/state/sessions.
func ConfiguredBeeSessionRoot() (root string, explicit bool, err error) {
	if override := strings.TrimSpace(os.Getenv("BEE_SESSION_ROOT")); override != "" {
		if !filepath.IsAbs(override) {
			return "", true, errors.New("BEE_SESSION_ROOT must be an absolute path")
		}
		return filepath.Clean(override), true, nil
	}
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(state, "sessions"), false, nil
}

// LegacyBeeSessionRoot is the store older Bee builds used when
// XDG_DATA_HOME was set and BEE_HOME was not: $XDG_DATA_HOME/bee/sessions.
// It is empty when no such store applies or it is the configured root.
func LegacyBeeSessionRoot() string {
	if os.Getenv("BEE_HOME") != "" || strings.TrimSpace(os.Getenv("BEE_SESSION_ROOT")) != "" {
		return ""
	}
	dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	legacy := filepath.Join(filepath.Clean(dir), "bee", "sessions")
	if configured, _, err := ConfiguredBeeSessionRoot(); err == nil && configured == legacy {
		return ""
	}
	return legacy
}

// SessionStoreRoots lists every place sessions live or may live: the store
// in use, the configured root, and the legacy XDG root. Guards that keep
// transcripts out of agent write roots check all of them, so a migration
// never opens a gap.
func SessionStoreRoots() []string {
	var roots []string
	addRoot := func(root string) {
		if root == "" {
			return
		}
		for _, existing := range roots {
			if existing == root {
				return
			}
		}
		roots = append(roots, root)
	}
	if root, err := DefaultBeeSessionRoot(); err == nil {
		addRoot(root)
	}
	if root, _, err := ConfiguredBeeSessionRoot(); err == nil {
		addRoot(root)
	}
	addRoot(LegacyBeeSessionRoot())
	return roots
}

// SessionRootHasSessions reports whether root directly holds at least one
// session transcript (*.jsonl).
func SessionRootHasSessions(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

func DefaultBeeSemanticIndexRoot() (string, error) {
	state, err := DefaultBeeStateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "memory-semantic"), nil
}

func DefaultBeeSecretsRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "secrets"), nil
}

func DefaultBeeConfigPath() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.json"), nil
}

// ResolveBeeHomePath expands a user-configured path without tying portable
// profiles to one operating-system home. Relative paths are BEE_HOME-relative;
// the conventional ~/.bee prefix follows BEE_HOME when it is overridden.
func ResolveBeeHomePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if path == "~/.bee" || strings.HasPrefix(path, "~/.bee/") || path == `~\.bee` || strings.HasPrefix(path, `~\.bee\`) {
			home, err := DefaultBeeHome()
			if err != nil {
				return "", err
			}
			suffix := strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(path, "~/.bee"), `~\.bee`), `/\`)
			path = filepath.Join(home, suffix)
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("find home directory: %w", err)
			}
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
	} else if !filepath.IsAbs(path) {
		home, err := DefaultBeeHome()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func resolveBeeHomePath(path string) (string, error) { return ResolveBeeHomePath(path) }

func EnsureBeeLayout() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return ensureBeeLayoutAt(home)
}

func ensureBeeLayoutAt(home string) (string, error) {
	if err := ValidateBeePath(home); err != nil {
		return "", err
	}
	if err := rejectBeeSymlinkAncestors(home); err != nil {
		return "", err
	}
	workspace := filepath.Join(home, "workspace")
	state := filepath.Join(home, "state")
	secrets := filepath.Join(home, "secrets")
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{home, 0o700}, {filepath.Join(home, "skills"), 0o700}, {filepath.Join(home, "plugins"), 0o700}, {workspace, 0o700}, {filepath.Join(workspace, "memory"), 0o700}, {filepath.Join(workspace, "memory", "dashboard"), 0o700}, {filepath.Join(workspace, "projects"), 0o700}, {filepath.Join(workspace, "exports"), 0o700}, {state, 0o700}, {filepath.Join(state, "sessions"), 0o700}, {filepath.Join(state, "runtime"), 0o700}, {filepath.Join(state, "artifacts"), 0o700}, {secrets, 0o700}} {
		if err := ensurePrivateDir(item.path, item.mode); err != nil {
			return "", fmt.Errorf("protect Bee layout path %q: %w", item.path, err)
		}
	}
	marker := filepath.Join(workspace, ".bee-workspace")
	if err := ensurePrivateFile(marker, []byte("Bee workspace marker v1\n")); err != nil {
		return "", fmt.Errorf("protect Bee workspace marker: %w", err)
	}
	contextFile := filepath.Join(workspace, "BEE.md")
	if err := ensurePrivateFile(contextFile, []byte("# Bee workspace\n\nThis private workspace stores Bee context and durable memory.\n\n## Filesystem placement\n\n- Use this workspace as the default location for persistent files.\n- Place repository clones and project directories under `projects/`.\n- Place durable memory files, including dated memory files, under `memory/`. Creating and appending those files is allowed when useful.\n- Files elsewhere on the readable host may be inspected, but persistent writes outside this workspace require explicit user approval.\n- Use the approval-only `host_exec` capability only when an outside-workspace mutation is genuinely intended.\n")); err != nil {
		return "", fmt.Errorf("protect Bee workspace context: %w", err)
	}
	// SOUL.md is optional and never created here, but when present it is read
	// with the same owner-only rule as BEE.md, so repair a restored 0644 copy.
	if err := repairPrivateFileIfPresent(filepath.Join(home, DefaultSystemPromptFile)); err != nil {
		return "", fmt.Errorf("protect Bee system prompt file: %w", err)
	}
	return home, nil
}

func repairPrivateFileIfPresent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must be a real regular file")
	}
	if info.Mode().Perm() == 0o600 {
		return nil
	}
	return os.Chmod(path, 0o600)
}

func ensurePrivateDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path must be a real directory")
	}
	return os.Chmod(path, mode)
}

func ensurePrivateFile(path string, contents []byte) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path must be a real regular file")
	}
	return os.Chmod(path, 0o600)
}

func rejectBeeSymlinkAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Bee path uses a symlink")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect Bee path: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func ValidateBeePath(path string) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("Bee path must be an absolute non-root path")
	}
	return nil
}
