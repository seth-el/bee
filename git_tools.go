package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxGitReviewBytes = 32 << 10

type gitReviewArgs struct {
	RepoPath string `json:"repo_path,omitempty"`
	Path     string `json:"path,omitempty"`
	Ref      string `json:"ref,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}
type GitStatusTool struct{ Scope ExecutionScope }
type GitDiffTool struct{ Scope ExecutionScope }

type gitStatusResult struct {
	Repository  string           `json:"repository"`
	Branch      string           `json:"branch,omitempty"`
	AheadBehind string           `json:"ahead_behind,omitempty"`
	Entries     []gitStatusEntry `json:"entries,omitempty"`
	Truncated   bool             `json:"truncated,omitempty"`
}
type gitStatusEntry struct {
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
	Path     string `json:"path"`
}
type gitDiffResult struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Path       string `json:"path"`
	HasChanges bool   `json:"has_changes"`
	Diff       string `json:"diff"`
	Truncated  bool   `json:"truncated"`
	Note       string `json:"note"`
}

func (tool GitStatusTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "git_status", Description: "Inspect structured Git status for a repository within the execution scope. Read-only; never applies or mutates Git state.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string"}},"additionalProperties":false}`)}
}
func (tool GitDiffTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "git_diff", Description: "Return a bounded read-only Git diff/review packet for a repository path and optional safe ref. An empty diff is a successful response with has_changes=false. If truncated=true, repeating identical arguments returns the same prefix; increase max_bytes when possible, narrow path, or continue from the partial diff.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string"},"path":{"type":"string"},"ref":{"type":"string"},"max_bytes":{"type":"integer"}},"additionalProperties":false}`)}
}

func gitRepo(ctx context.Context, scope ExecutionScope, repoPath string) (string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	candidate, err := securePath(scope, repoPath, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("git repo_path must be a directory")
	}
	result, err := runGit(ctx, candidate, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("path is not a Git repository")
	}
	root := filepath.Clean(strings.TrimSpace(result.Stdout))
	scopeRoots, err := scope.rootPaths()
	if err != nil {
		return "", err
	}
	if !pathWithinAny(scopeRoots, root) {
		return "", errors.New("Git repository escapes execution scope")
	}
	return root, nil
}
func (tool GitStatusTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in gitReviewArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("git_status arguments: %w", err)
	}
	repo, err := gitRepo(ctx, tool.Scope, in.RepoPath)
	if err != nil {
		return "", err
	}
	r, err := runGitWithLimit(ctx, repo, maxGitReviewBytes, "status", "--porcelain=v1", "--branch", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	out := gitStatusResult{Repository: repo, Truncated: r.Truncated}
	for _, line := range strings.Split(strings.TrimSuffix(r.Stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			parts := strings.SplitN(strings.TrimPrefix(line, "## "), "...", 2)
			out.Branch = parts[0]
			if len(parts) == 2 {
				out.AheadBehind = parts[1]
			}
			continue
		}
		if len(out.Entries) < 256 {
			if len(line) < 3 {
				continue
			}
			out.Entries = append(out.Entries, gitStatusEntry{Index: string(line[0]), Worktree: string(line[1]), Path: strings.TrimSpace(line[2:])})
		} else {
			out.Truncated = true
		}
	}
	data, err := boundedGitStatusJSON(out)
	return string(data), err
}
func boundedGitStatusJSON(out gitStatusResult) ([]byte, error) {
	for len(out.Entries) >= 0 {
		data, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxGitReviewBytes {
			return data, nil
		}
		if len(out.Entries) == 0 {
			break
		}
		out.Entries = out.Entries[:len(out.Entries)-1]
		out.Truncated = true
	}
	return json.Marshal(struct {
		Repository  string           `json:"repository"`
		Branch      string           `json:"branch,omitempty"`
		AheadBehind string           `json:"ahead_behind,omitempty"`
		Entries     []gitStatusEntry `json:"entries,omitempty"`
		Truncated   bool             `json:"truncated"`
	}{out.Repository, out.Branch, out.AheadBehind, nil, true})
}
func (tool GitDiffTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in gitReviewArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("git_diff arguments: %w", err)
	}
	repo, err := gitRepo(ctx, tool.Scope, in.RepoPath)
	if err != nil {
		return "", err
	}
	pathArg := ""
	if in.Path != "" {
		candidate, err := securePath(ExecutionScope{Root: repo}, in.Path, true)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(repo, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("git diff path escapes repository")
		}
		pathArg = filepath.ToSlash(rel)
	}
	limit := in.MaxBytes
	if limit <= 0 || limit > maxGitReviewBytes {
		limit = maxGitReviewBytes
	}
	argsGit := []string{"diff", "--no-ext-diff", "--binary"}
	if in.Ref != "" {
		if !safeGitRevision(in.Ref) {
			return "", errors.New("git diff ref is not a safe Git revision")
		}
		argsGit = append(argsGit, in.Ref)
	} else {
		// HEAD comparison includes both staged and unstaged tracked changes.
		argsGit = append(argsGit, "HEAD")
	}
	argsGit = append(argsGit, "--")
	if pathArg != "" {
		argsGit = append(argsGit, pathArg)
	}
	r, err := runGitWithLimit(ctx, repo, limit, argsGit...)
	if err != nil {
		return "", err
	}
	effectiveRef := in.Ref
	if effectiveRef == "" {
		effectiveRef = "HEAD"
	}
	out := gitDiffResult{Repository: repo, Ref: effectiveRef, Path: pathArg, HasChanges: r.Stdout != "", Diff: r.Stdout, Truncated: r.Truncated}
	data, err := boundedGitDiffJSON(out, limit)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func boundedGitDiffJSON(out gitDiffResult, effectiveLimit int) ([]byte, error) {
	limit := maxGitReviewBytes
	if effectiveLimit > 0 && effectiveLimit <= maxGitReviewBytes {
		limit = effectiveLimit
	}
	if out.Truncated {
		out.Note = gitDiffTruncationNote(limit)
	} else {
		out.Note = ""
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxGitReviewBytes {
		return data, nil
	}
	out.Truncated = true
	if out.Note == "" {
		out.Note = gitDiffEnvelopeTruncationNote()
	}
	for len(out.Diff) > 0 {
		limit := len(out.Diff) / 2
		if limit < 1 {
			out.Diff = ""
		} else {
			out.Diff = truncateText(out.Diff, limit)
		}
		data, err = json.Marshal(out)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxGitReviewBytes {
			return data, nil
		}
	}
	data, err = json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(data) > maxGitReviewBytes {
		return nil, errors.New("git diff metadata exceeds bounded output limit")
	}
	return data, nil
}

func gitDiffTruncationNote(effectiveLimit int) string {
	if effectiveLimit >= maxGitReviewBytes {
		return fmt.Sprintf("source diff exceeded the maximum %d-byte capture limit; the returned prefix may be shorter to fit the bounded JSON envelope; do not repeat identical arguments because they return the same prefix; narrow path or continue with this partial diff.", maxGitReviewBytes)
	}
	return fmt.Sprintf("source diff exceeded the %d-byte capture limit; the returned prefix may be shorter to fit the bounded JSON envelope; do not repeat identical arguments because they return the same prefix; increase max_bytes (maximum %d), narrow path, or continue with this partial diff.", effectiveLimit, maxGitReviewBytes)
}

func gitDiffEnvelopeTruncationNote() string {
	return fmt.Sprintf("diff truncated to fit the maximum %d-byte bounded envelope; do not repeat identical arguments because they return the same prefix; narrow path or continue with this partial diff.", maxGitReviewBytes)
}
