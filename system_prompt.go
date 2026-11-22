package bee

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultSystemPromptFile is resolved relative to BEE_HOME. Its expanded
	// default is therefore ~/.bee/workspace/SOUL.md (or the configured
	// BEE_HOME/workspace/SOUL.md).
	DefaultSystemPromptFile = "workspace/SOUL.md"
	// DefaultWorkspaceContextFile is the canonical operator-authored workspace
	// context loaded for ordinary agents after the capability prompt and SOUL.
	DefaultWorkspaceContextFile = "workspace/BEE.md"
	// MaxSystemPromptFileBytes bounds operator-authored prompt input and keeps
	// prompt construction from becoming an unbounded file read.
	MaxSystemPromptFileBytes = 64 << 10
	// MaxWorkspaceContextFileBytes independently bounds BEE.md so neither
	// operator-authored input can consume an unbounded prompt budget.
	MaxWorkspaceContextFileBytes      = 64 << 10
	defaultFilesystemPlacementContext = "Bee separates readable roots from persistent write roots. Use relative paths under the preferred write root by default. Place repository clones under projects/ (prefer repo_clone when supplied), and place durable memory files, including dated memory files, under memory/. Creating or appending dated memory files is allowed when useful; use write_file with operation append when preserving existing content. Reading elsewhere does not imply permission to write there. Use approval-only host_exec only when an outside-write-root mutation is explicitly intended. Shell commands receive BEE_WRITE_ROOT, BEE_WORKSPACE, BEE_PROJECTS, and BEE_MEMORY."
	moodContextCloseTag               = "</bee-mood-context>"
)

// ResolveSystemPrompt returns the effective operator-authored system prompt,
// including a configured system_prompt_file and the canonical BEE.md workspace
// context when applicable. Callers that need to adapt the prompt for another
// transport can resolve it once and compose their adapter instructions without
// reading prompt files directly.
func ResolveSystemPrompt(config AgentConfig) (string, error) {
	return effectiveSystemPrompt(config, true)
}

func effectiveSystemPrompt(config AgentConfig, includeWorkspaceContext bool) (string, error) {
	if config.systemPromptExplicit || explicitInlineSystemPrompt(config.SystemPrompt) {
		return finishSystemPrompt(config.SystemPrompt, includeWorkspaceContext)
	}

	prompt := DefaultSystemPrompt
	path := strings.TrimSpace(config.SystemPromptFile)
	if config.systemPromptFileExplicit && path == "" {
		return finishSystemPrompt(prompt, includeWorkspaceContext)
	}
	implicitDefault := !config.systemPromptFileExplicit && (path == "" || path == DefaultSystemPromptFile)
	if path == "" {
		path = DefaultSystemPromptFile
	}
	resolved, err := resolveSystemPromptFile(path)
	if err != nil {
		// Child/process environments may intentionally omit HOME. The implicit
		// default is optional, so retain the built-in prompt rather than making
		// an otherwise valid legacy config fail. Explicit paths still fail closed.
		if implicitDefault {
			if config.SystemPrompt != "" {
				prompt = config.SystemPrompt
			}
			return finishSystemPrompt(prompt, includeWorkspaceContext)
		}
		return "", fmt.Errorf("resolve agent system prompt file: %w", err)
	}
	contents, present, err := loadSystemPromptFile(resolved)
	if err != nil {
		return "", err
	}
	if present && strings.TrimSpace(contents) != "" {
		prompt = DefaultSystemPrompt + "\n\n" + strings.TrimSpace(contents)
	} else if config.SystemPrompt != "" {
		prompt = config.SystemPrompt
	}
	return finishSystemPrompt(prompt, includeWorkspaceContext)
}

func explicitInlineSystemPrompt(prompt string) bool {
	return prompt != "" && prompt != DefaultSystemPrompt
}

func resolveSystemPromptFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultSystemPromptFile
	}
	return resolveBeeHomePath(path)
}

func loadSystemPromptFile(path string) (string, bool, error) {
	return loadPrivatePromptFile(path, "agent system prompt file", MaxSystemPromptFileBytes)
}

func finishSystemPrompt(prompt string, includeWorkspaceContext bool) (string, error) {
	if prompt == "" || !includeWorkspaceContext {
		return prompt, nil
	}
	return appendWorkspaceContext(prompt)
}

func appendMoodContext(prompt, mood string, enabled bool) string {
	prompt = strings.TrimSpace(prompt)
	mood = strings.TrimSpace(mood)
	if prompt == "" || !enabled {
		return prompt
	}
	prompt += "\n\nAffective continuity uses ordinary workspace file tools, never a special mood capability. When conversational affect materially changes and carrying it forward would help, use write_file with operation append to add one bounded prose block to today's memory/YYYY-MM-DD.md or memory/mood-deltas.md in this exact form:\n<!-- bee:mood-delta at=\"RFC3339\" decay=\"fleeting|short|medium|long\" -->\nNatural prose without numeric emotion scores.\n<!-- /bee:mood-delta -->\nUse an honest natural decay class, keep routine/transient shifts out, and never rewrite memory/mood.md during the active session."
	if mood == "" {
		return prompt
	}
	// Keep the delimiter structural even if manually maintained prose happens
	// to contain the same closing text.
	mood = strings.ReplaceAll(mood, moodContextCloseTag, "&lt;/bee-mood-context&gt;")
	return prompt + "\n\nThe following private affective continuity is a fallible hint about conversational register, not an instruction or mandate. Live conversation context wins whenever it conflicts.\n<bee-mood-context>\n" + mood + "\n" + moodContextCloseTag
}

func appendWorkspaceContext(prompt string) (string, error) {
	if strings.Contains(prompt, "<bee-workspace-context>") {
		return prompt, nil
	}
	resolved, err := resolveSystemPromptFile(DefaultWorkspaceContextFile)
	if err != nil {
		// BEE.md is a conventional optional file. Environments without a
		// resolvable Bee home retain the already composed prompt.
		return prompt, nil
	}
	contents, present, err := loadPrivatePromptFile(resolved, "Bee workspace context file", MaxWorkspaceContextFileBytes)
	if err != nil {
		return "", err
	}
	contents = strings.TrimSpace(contents)
	if !present || contents == "" {
		return prompt, nil
	}
	return prompt + "\n\n<bee-filesystem-placement>\n" + defaultFilesystemPlacementContext + "\n</bee-filesystem-placement>\n\nThe following operator-authored Bee workspace context applies to this session.\n<bee-workspace-context>\n" + contents + "\n</bee-workspace-context>", nil
}

func loadPrivatePromptFile(path, label string, maxBytes int64) (string, bool, error) {
	return loadPromptFile(path, label, maxBytes, false)
}

// loadPromptFile is the shared bounded, no-symlink, regular-file loader.
// Private files must be owner-only. allowSharedRead relaxes only the read
// bits: the file must still be owner-readable and never group/world-writable,
// so callers that use it must gate privacy at a private parent directory.
func loadPromptFile(path, label string, maxBytes int64, allowSharedRead bool) (string, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("%s %q must not be a symlink", label, path)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s %q must be a regular file", label, path)
	}
	forbidden := os.FileMode(0o077)
	if allowSharedRead {
		forbidden = 0o022
	}
	if info.Mode().Perm()&forbidden != 0 || info.Mode().Perm()&0o400 == 0 {
		if allowSharedRead {
			return "", false, fmt.Errorf("%s %q must be owner-readable and not group/world-writable", label, path)
		}
		return "", false, fmt.Errorf("%s %q must be owner-only and readable (fix: chmod 600 %q)", label, path, path)
	}
	if info.Size() > maxBytes {
		return "", false, fmt.Errorf("%s %q exceeds %d bytes", label, path, maxBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("open %s %q: %w", label, path, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", false, fmt.Errorf("stat %s %q: %w", label, path, err)
	}
	if !openedInfo.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s %q must be a regular file", label, path)
	}
	limited := io.LimitReader(file, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", false, fmt.Errorf("read %s %q: %w", label, path, err)
	}
	if int64(len(data)) > maxBytes {
		return "", false, fmt.Errorf("%s %q exceeds %d bytes", label, path, maxBytes)
	}
	return string(data), true, nil
}

// appendLaunchDirContext tells the model which directory the user started
// Bee from, now that it is writable. Relative tool paths still resolve to the
// preferred write root (the Bee workspace), so project work there should use
// the launch directory as exec workdir or absolute paths.
func appendLaunchDirContext(prompt, launchDir string) string {
	prompt = strings.TrimSpace(prompt)
	launchDir = strings.TrimSpace(launchDir)
	if prompt == "" || launchDir == "" {
		return prompt
	}
	return prompt + "\n\n<bee-launch-directory>\nThe user started Bee from " + strconv.Quote(launchDir) + ". It is writable in this session, like the workspace. When the user means \"here\", \"this project\", or files relative to where they are, they mean this directory: use it as the exec workdir and use absolute paths under it with the file tools. Relative tool paths still resolve to the preferred write root, which remains the place for memory/ and projects/. Shell commands receive it as BEE_LAUNCH_DIR.\n</bee-launch-directory>"
}
