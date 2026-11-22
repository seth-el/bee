package bee

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMemoryDashboardFileName is the always-in-context state dashboard
	// at <memory root>/dashboard/MEMORY.md: current threads, pointers into
	// memory/topics/ and durable memory, and the agent's own notes on using
	// its memory. It is a derived layer, rebuildable from dailies and session
	// transcripts, and is never session-scoped.
	DefaultMemoryDashboardFileName = "MEMORY.md"
	// DefaultMemoryDashboardDir holds the dashboard in its own directory so a
	// recurring job can be granted it as a write root while the dated daily
	// notes beside it in the memory root stay read-only.
	DefaultMemoryDashboardDir = "dashboard"
	// MaxMemoryDashboardFileBytes bounds the dashboard exactly like the mood
	// baseline. A wrong note at the top of context poisons everything below
	// it, so the file should stay far smaller than this guard.
	MaxMemoryDashboardFileBytes = MaxMoodFileBytes

	memoryDashboardCloseTag = "</bee-memory-dashboard>"
)

// MemoryDashboard owns the one global dashboard file. Like mood, it is read
// once per session activation and never re-read mid-session, so a nightly
// rewrite cannot change the prefix of a session that is already open.
type MemoryDashboard struct {
	root string
}

func NewMemoryDashboard(root string) (*MemoryDashboard, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("memory dashboard root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &MemoryDashboard{root: filepath.Clean(abs)}, nil
}

func (dashboard *MemoryDashboard) Path() string {
	if dashboard == nil {
		return ""
	}
	return filepath.Join(dashboard.root, DefaultMemoryDashboardDir, DefaultMemoryDashboardFileName)
}

// MemoryDashboardRelativePath is the dashboard path relative to the memory root.
func MemoryDashboardRelativePath() string {
	return filepath.Join(DefaultMemoryDashboardDir, DefaultMemoryDashboardFileName)
}

// Snapshot returns the bounded dashboard text. Missing, unsafe, oversized,
// invalid-UTF-8, or empty files deliberately compose as absent: the dashboard
// can never prevent a session from starting.
//
// Unsafe means: a symlink or non-regular file, a group/world-writable file,
// a symlinked or group/world-writable dashboard directory, or a memory root
// that grants any group/world access. Unlike SOUL.md and
// mood.md, a group/world-readable file is accepted, because the ordinary
// write_file tool creates new files 0644 and the dreaming pass writes the
// dashboard with it; privacy is enforced by the private (0700) memory root,
// which already holds the 0644 daily notes.
func (dashboard *MemoryDashboard) Snapshot() string {
	if dashboard == nil {
		return ""
	}
	rootInfo, err := os.Stat(dashboard.root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 {
		return ""
	}
	dirInfo, err := os.Lstat(filepath.Dir(dashboard.Path()))
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o022 != 0 {
		return ""
	}
	contents, present, err := loadPromptFile(dashboard.Path(), "Bee memory dashboard", MaxMemoryDashboardFileBytes, true)
	if err != nil || !present || !utf8.ValidString(contents) {
		return ""
	}
	return strings.TrimSpace(contents)
}

// appendMemoryDashboardContext composes one activation's dashboard after the
// mood block. It describes what the layer is for, not how to use it: salience
// and retrieval judgment stay with the model.
func appendMemoryDashboardContext(prompt, dashboard string) string {
	prompt = strings.TrimSpace(prompt)
	dashboard = strings.TrimSpace(dashboard)
	if prompt == "" || dashboard == "" {
		return prompt
	}
	dashboard = strings.ReplaceAll(dashboard, memoryDashboardCloseTag, "&lt;/bee-memory-dashboard&gt;")
	return prompt + "\n\nThe following is your memory dashboard (memory/dashboard/MEMORY.md): a small, always-present summary of current state that points into memory/topics/, the dated daily notes, and durable memory. It is a derived, fallible layer; daily notes index what happened, session transcripts are the authority when a fact is contested, and live conversation context wins whenever it conflicts.\n<bee-memory-dashboard>\n" + dashboard + "\n" + memoryDashboardCloseTag
}
