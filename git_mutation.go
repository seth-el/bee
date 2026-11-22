package bee

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	maxGitMutationPaths         = 128
	maxGitMutationPathBytes     = 4 << 10
	maxGitMutationPathListBytes = 16 << 10
	maxGitMutationFileBytes     = 8 << 20
	maxGitCommitMessageBytes    = 8 << 10
	maxGitCommitMessageLines    = 128
	maxGitStagedReviewBytes     = 16 << 10
	maxGitMutationCommandTime   = 20 * time.Second
)

type gitStageArgs struct {
	RepoPath string   `json:"repo_path,omitempty"`
	Paths    []string `json:"paths"`
}

type gitCommitArgs struct {
	RepoPath   string `json:"repo_path,omitempty"`
	Message    string `json:"message"`
	ReviewHash string `json:"review_hash"`
}

type gitStagedReviewResult struct {
	Repository string   `json:"repository"`
	Branch     string   `json:"branch"`
	Head       string   `json:"head"`
	IndexHash  string   `json:"index_hash"`
	ReviewHash string   `json:"review_hash"`
	Paths      []string `json:"paths,omitempty"`
	PathCount  int      `json:"path_count"`
	Diff       string   `json:"diff"`
	Truncated  bool     `json:"truncated"`
}

type gitCommitResult struct {
	Repository  string   `json:"repository"`
	Branch      string   `json:"branch"`
	OldHead     string   `json:"old_head"`
	NewHead     string   `json:"new_head"`
	Tree        string   `json:"tree"`
	ReviewHash  string   `json:"review_hash"`
	Paths       []string `json:"paths,omitempty"`
	PathCount   int      `json:"path_count"`
	Diagnostics string   `json:"diagnostics,omitempty"`
}

type gitStageResult struct {
	Repository string   `json:"repository"`
	Branch     string   `json:"branch"`
	Head       string   `json:"head"`
	PathCount  int      `json:"path_count"`
	Paths      []string `json:"paths"`
	Diagnostic string   `json:"diagnostic,omitempty"`
}

type GitStagedReviewTool struct{ Scope ExecutionScope }
type GitStageTool struct{ Scope ExecutionScope }
type GitCommitTool struct{ Scope ExecutionScope }

func isParentOwnedGitMutationTool(name string) bool {
	switch name {
	case "git_staged_review", "git_stage", "git_commit":
		return true
	default:
		return false
	}
}

func (GitStagedReviewTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "git_staged_review", Description: "Review the complete bounded staged Git diff and return the exact cryptographic review_hash required by git_commit. Read-only; never mutates Git state.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","maxLength":4096}},"additionalProperties":false}`)}
}
func (GitStageTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "git_stage", Description: "Stage only an explicit list of exact repository-relative regular files, symlinks, or tracked deletions in the local repository. No globs, directories, hooks, remotes, or generic Git commands.", Risk: RiskHigh, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","maxLength":4096},"paths":{"type":"array","minItems":1,"maxItems":128,"items":{"type":"string","minLength":1,"maxLength":4096}}},"required":["paths"],"additionalProperties":false}`)}
}
func (GitCommitTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "git_commit", Description: "Create one bounded local commit only after a fresh git_staged_review review_hash is supplied and still matches immediately before the guarded branch update. Never pushes or invokes hooks, signing, editors, prompts, or remotes.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"repo_path":{"type":"string","maxLength":4096},"message":{"type":"string","minLength":1,"maxLength":8192},"review_hash":{"type":"string","minLength":64,"maxLength":64}},"required":["message","review_hash"],"additionalProperties":false}`)}
}

func (tool GitStagedReviewTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		RepoPath string `json:"repo_path,omitempty"`
	}
	if err := parseArgs(args, &input); err != nil {
		return "", errors.New("git staged review arguments are invalid")
	}
	repo, err := gitRepo(ctx, tool.Scope, input.RepoPath)
	if err != nil {
		return "", errors.New("Git repository is unavailable")
	}
	lock, err := acquireGitMutationLock(ctx, repo)
	if err != nil {
		return "", errors.New("Git repository is busy")
	}
	defer lock.Close()
	review, err := readGitStagedReview(ctx, repo)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(review)
	if err != nil || len(data) > maxGitStagedReviewBytes {
		return "", errors.New("staged review exceeds the bounded output limit")
	}
	return string(data), nil
}

func (tool GitStageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input gitStageArgs
	if err := parseArgs(args, &input); err != nil || len(input.Paths) == 0 {
		return "", errors.New("git stage arguments are invalid")
	}
	if len(input.Paths) > maxGitMutationPaths {
		return "", errors.New("too many Git paths")
	}
	repo, err := gitRepo(ctx, tool.Scope, input.RepoPath)
	if err != nil {
		return "", errors.New("Git repository is unavailable")
	}
	if _, err := secureMutationPath(ctx, tool.Scope, repo, false); err != nil {
		return "", err
	}
	lock, err := acquireGitMutationLock(ctx, repo)
	if err != nil {
		return "", errors.New("Git repository is busy")
	}
	defer lock.Close()
	state, err := inspectGitMutationState(ctx, repo)
	if err != nil {
		return "", err
	}
	paths, err := validateGitMutationPaths(ctx, repo, input.Paths, state)
	if err != nil {
		return "", err
	}
	result := gitStageResult{Repository: repo, Branch: state.branch, Head: state.head, Paths: paths, PathCount: len(paths)}
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxGitStagedReviewBytes {
		return "", errors.New("Git stage result exceeds the bounded output limit")
	}
	if err := stageGitPaths(ctx, repo, paths, state); err != nil {
		return "", err
	}
	return string(data), nil
}

func (tool GitCommitTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input gitCommitArgs
	if err := parseArgs(args, &input); err != nil || !validReviewHash(input.ReviewHash) || !validCommitMessage(input.Message) {
		return "", errors.New("git commit arguments are invalid")
	}
	repo, err := gitRepo(ctx, tool.Scope, input.RepoPath)
	if err != nil {
		return "", errors.New("Git repository is unavailable")
	}
	if _, err := secureMutationPath(ctx, tool.Scope, repo, false); err != nil {
		return "", err
	}
	beeLock, err := acquireGitMutationLock(ctx, repo)
	if err != nil {
		return "", errors.New("Git repository is busy")
	}
	defer beeLock.Close()
	state, err := inspectGitMutationState(ctx, repo)
	if err != nil {
		return "", err
	}
	fresh, err := readGitStagedReview(ctx, repo)
	if err != nil || fresh.ReviewHash != input.ReviewHash || len(fresh.Paths) == 0 {
		return "", errors.New("staged review is stale or invalid")
	}
	indexLock, err := acquireGitIndexLock(ctx, state.indexPath)
	if err != nil {
		return "", errors.New("Git repository index is busy")
	}
	defer indexLock.Close()
	if err := verifyGitMutationState(ctx, repo, state); err != nil {
		return "", errors.New("Git repository changed before commit")
	}
	fresh, err = readGitStagedReview(ctx, repo)
	if err != nil || fresh.ReviewHash != input.ReviewHash {
		return "", errors.New("staged review changed before commit")
	}
	tempIndex, err := copyGitIndex(state.indexPath)
	if err != nil {
		return "", errors.New("temporary Git index creation failed")
	}
	defer removeGitTempIndex(tempIndex)
	tree, err := gitMutationCommand(ctx, repo, tempIndex, "write-tree")
	if err != nil {
		return "", errors.New("Git tree creation failed")
	}
	treeID := strings.TrimSpace(tree.Stdout)
	if err := verifyGitMutationState(ctx, repo, state); err != nil {
		return "", errors.New("Git repository changed during commit")
	}
	newHead, err := createGitCommit(ctx, repo, state, treeID, input.Message)
	if err != nil {
		return "", errors.New("Git commit creation failed")
	}
	if err := verifyGitMutationState(ctx, repo, state); err != nil {
		return "", errors.New("Git repository changed before branch update")
	}
	result := gitCommitResult{Repository: repo, Branch: state.branch, OldHead: state.head, NewHead: newHead, Tree: treeID, ReviewHash: input.ReviewHash, Paths: fresh.Paths, PathCount: len(fresh.Paths), Diagnostics: "local commit created"}
	data, err := json.Marshal(result)
	if err != nil || len(data) > maxGitStagedReviewBytes {
		return "", errors.New("Git commit result exceeds the bounded output limit")
	}
	if _, err := gitMutationCommand(ctx, repo, state.indexPath, "update-ref", state.branchRef, newHead, state.head); err != nil {
		return "", errors.New("Git branch update failed")
	}
	return string(data), nil
}

func validReviewHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func validCommitMessage(message string) bool {
	if strings.TrimSpace(message) == "" || len(message) > maxGitCommitMessageBytes || strings.ContainsRune(message, '\x00') {
		return false
	}
	for _, r := range message {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return strings.Count(message, "\n")+1 <= maxGitCommitMessageLines
}

func validateGitMutationPaths(ctx context.Context, repo string, input []string, state gitMutationState) ([]string, error) {
	seen := make(map[string]struct{}, len(input))
	paths := make([]string, 0, len(input))
	indexEntries, err := readGitIndexEntries(state.indexData)
	if err != nil {
		return nil, errors.New("Git index is invalid")
	}
	total := 0
	for _, raw := range input {
		if raw == "" || len(raw) > maxGitMutationPathBytes || filepath.IsAbs(raw) || strings.ContainsRune(raw, '\x00') || strings.ContainsAny(raw, "\r\n") {
			return nil, errors.New("Git paths must be exact repository-relative paths")
		}
		for _, r := range raw {
			if unicode.IsControl(r) {
				return nil, errors.New("Git paths must not contain control characters")
			}
		}
		clean := filepath.Clean(filepath.FromSlash(raw))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || hasGitInternalComponent(clean) {
			return nil, errors.New("Git path is outside the authorized repository")
		}
		if filepath.ToSlash(clean) != raw {
			return nil, errors.New("Git paths must be canonical exact relative paths")
		}
		if strings.ContainsAny(raw, "*?[]{}") || strings.HasPrefix(raw, ":") {
			return nil, errors.New("Git path syntax is not permitted")
		}
		rel := filepath.ToSlash(clean)
		if _, ok := seen[rel]; ok {
			return nil, errors.New("Git paths must be unique")
		}
		seen[rel] = struct{}{}
		total += len(rel)
		if total > maxGitMutationPathListBytes {
			return nil, errors.New("Git path list is too large")
		}
		if entry, ok := indexEntries[rel]; ok {
			if entry.mode == 0o160000 {
				return nil, errors.New("Git submodules and gitlinks cannot be staged")
			}
			if entry.flags&0xc000 != 0 {
				return nil, errors.New("Git index flags are not supported")
			}
		}
		candidate := filepath.Join(repo, filepath.FromSlash(rel))
		if hasSymlinkedGitParent(repo, rel) {
			return nil, errors.New("Git path has a symlinked parent")
		}
		if _, err := securePath(ExecutionScope{Root: repo}, candidate, true); err != nil {
			return nil, errors.New("Git path is not contained by the repository")
		}
		info, err := os.Lstat(candidate)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("Git path cannot be inspected")
		}
		if err == nil {
			if info.IsDir() || info.Mode()&os.ModeNamedPipe != 0 || info.Mode()&os.ModeSocket != 0 || info.Mode()&os.ModeDevice != 0 {
				return nil, errors.New("Git path must be a regular file or symlink")
			}
			if info.Mode()&os.ModeSymlink == 0 && !info.Mode().IsRegular() {
				return nil, errors.New("Git path must be a regular file or symlink")
			}
			if info.Size() > maxGitMutationFileBytes {
				return nil, errors.New("Git file exceeds the bounded size")
			}
			if _, trackedErr := gitMutationCommand(ctx, repo, "", "ls-files", "--error-unmatch", "--", rel); trackedErr != nil {
				if _, ignoredErr := gitMutationCommand(ctx, repo, "", "check-ignore", "--quiet", "--", rel); ignoredErr == nil {
					return nil, errors.New("ignored Git paths cannot be staged")
				}
			}
		}
		if err != nil {
			if _, lookupErr := gitMutationCommand(ctx, repo, "", "ls-files", "--error-unmatch", "--", rel); lookupErr != nil {
				return nil, errors.New("Git deletion path is not tracked")
			}
		}
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	return paths, nil
}

func hasGitInternalComponent(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

func hasSymlinkedGitParent(repo, rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	parent := repo
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, filepath.FromSlash(part))
		info, err := os.Lstat(parent)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

type gitMutationState struct {
	repo, gitDir, indexPath, branchRef, branch, head, indexHash string
	indexData                                                   []byte
}

func inspectGitMutationState(ctx context.Context, repo string) (gitMutationState, error) {
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return gitMutationState{}, errors.New("Git repository path is unavailable")
	}
	gitMeta, err := os.Lstat(filepath.Join(repo, ".git"))
	if err != nil || !gitMeta.IsDir() || gitMeta.Mode()&os.ModeSymlink != 0 {
		return gitMutationState{}, errors.New("only ordinary top-level Git repositories are supported")
	}
	gitDirResult, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--absolute-git-dir")
	if err != nil {
		return gitMutationState{}, errors.New("Git repository state is unavailable")
	}
	gitDir := filepath.Clean(strings.TrimSpace(gitDirResult.Stdout))
	if !filepath.IsAbs(gitDir) {
		return gitMutationState{}, errors.New("Git repository state is invalid")
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil || filepath.Clean(gitDir) != filepath.Clean(filepath.Join(repo, ".git")) {
		return gitMutationState{}, errors.New("Git metadata escapes the repository")
	}
	for _, metadata := range []string{"HEAD", "config", "index"} {
		if !containedExistingRegularMetadataPath(gitDir, filepath.Join(gitDir, metadata)) {
			return gitMutationState{}, errors.New("Git metadata is unsafe")
		}
	}
	bare, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(bare.Stdout) == "true" {
		return gitMutationState{}, errors.New("bare Git repositories are not supported")
	}
	commonDirResult, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
	commonDir, commonErr := filepath.EvalSymlinks(filepath.Clean(strings.TrimSpace(commonDirResult.Stdout)))
	if err != nil || commonErr != nil || commonDir != gitDir {
		return gitMutationState{}, errors.New("Git worktrees are not supported")
	}
	worktreeResult, worktreeErr := gitMutationCommand(ctx, repo, "", "config", "--local", "--no-includes", "--get", "core.worktree")
	if worktreeErr == nil && strings.TrimSpace(worktreeResult.Stdout) != "" {
		worktree := strings.TrimSpace(worktreeResult.Stdout)
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(gitDir, worktree)
		}
		resolvedWorktree, resolveErr := filepath.EvalSymlinks(worktree)
		if resolveErr != nil || filepath.Clean(resolvedWorktree) != filepath.Clean(repo) {
			return gitMutationState{}, errors.New("external core.worktree is not supported")
		}
	}
	branchRefResult, err := gitMutationCommand(ctx, repo, "", "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return gitMutationState{}, errors.New("detached Git HEAD is not permitted")
	}
	branchRef := strings.TrimSpace(branchRefResult.Stdout)
	if !strings.HasPrefix(branchRef, "refs/heads/") || strings.ContainsAny(branchRef, "\r\n") || !safeGitRef(branchRef) {
		return gitMutationState{}, errors.New("Git branch is invalid")
	}
	branch := strings.TrimPrefix(branchRef, "refs/heads/")
	if _, err := gitMutationCommand(ctx, repo, "", "check-ref-format", "--branch", branch); err != nil {
		return gitMutationState{}, errors.New("Git branch is invalid")
	}
	headResult, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return gitMutationState{}, errors.New("unborn Git HEAD is not permitted")
	}
	head := strings.TrimSpace(headResult.Stdout)
	if len(head) != 40 || !safeGitRevision(head) {
		return gitMutationState{}, errors.New("non-SHA-1 Git object format is not supported")
	}
	objectFormat, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--show-object-format")
	if err != nil || strings.TrimSpace(objectFormat.Stdout) != "sha1" {
		return gitMutationState{}, errors.New("non-SHA-1 Git object format is not supported")
	}
	indexResult, err := gitMutationCommand(ctx, repo, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return gitMutationState{}, errors.New("Git index is unavailable")
	}
	indexPath := strings.TrimSpace(indexResult.Stdout)
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo, indexPath)
	}
	indexPath, err = filepath.Abs(indexPath)
	if err != nil || filepath.Clean(indexPath) != filepath.Join(gitDir, "index") {
		return gitMutationState{}, errors.New("Git index escapes repository metadata")
	}
	indexInfo, err := os.Lstat(indexPath)
	if err != nil || !indexInfo.Mode().IsRegular() || indexInfo.Mode()&os.ModeSymlink != 0 {
		return gitMutationState{}, errors.New("Git index is unsafe")
	}
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return gitMutationState{}, errors.New("Git index is unavailable")
	}
	if len(indexData) > maxGitMutationFileBytes {
		return gitMutationState{}, errors.New("Git index exceeds the bounded size")
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply", "sequencer"} {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return gitMutationState{}, errors.New("Git operation state is not supported")
		}
	}
	if err := validateGitIndex(indexData); err != nil {
		return gitMutationState{}, errors.New("sparse or split Git indexes are not supported")
	}
	refPath := filepath.Join(gitDir, filepath.FromSlash(branchRef))
	if !containedMetadataPathAllowPackedRefs(gitDir, refPath) {
		return gitMutationState{}, errors.New("Git branch metadata escapes repository")
	}
	if !containedMetadataWritePath(gitDir, filepath.Join(gitDir, "logs", filepath.FromSlash(branchRef))) {
		return gitMutationState{}, errors.New("Git branch reflog metadata escapes repository")
	}
	return gitMutationState{repo: repo, gitDir: gitDir, indexPath: indexPath, branchRef: branchRef, branch: branch, head: head, indexHash: sha256Hex(indexData), indexData: indexData}, nil
}

func safeGitRef(ref string) bool {
	return ref != "" && !strings.ContainsAny(ref, "\x00\r\n") && !strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/") && !strings.Contains(ref, "\\")
}

func containedExistingRegularMetadataPath(root, path string) bool {
	if !metadataParentsContained(root, path) || !scopeWithin(root, filepath.Clean(path)) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func containedMetadataPathAllowPackedRefs(root, path string) bool {
	if !metadataParentsContained(root, path) {
		return false
	}
	if info, err := os.Lstat(path); err == nil {
		return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	packed := filepath.Join(root, "packed-refs")
	return containedExistingRegularMetadataPath(root, packed)
}

func containedMetadataWritePath(root, path string) bool {
	if !metadataParentsContained(root, path) || !scopeWithin(root, filepath.Clean(path)) {
		return false
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func metadataParentsContained(root, path string) bool {
	root = filepath.Clean(root)
	parent := filepath.Dir(filepath.Clean(path))
	for {
		if !scopeWithin(root, parent) {
			return false
		}
		if parent != root {
			info, err := os.Lstat(parent)
			if err == nil {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return false
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
		if parent == root {
			return true
		}
		parent = filepath.Dir(parent)
	}
}

type gitIndexEntry struct {
	mode  uint32
	flags uint16
}

func readGitIndexEntries(data []byte) (map[string]gitIndexEntry, error) {
	if err := validateGitIndex(data); err != nil {
		return nil, err
	}
	entries := int(binaryBigEndianUint32(data[8:12]))
	out := make(map[string]gitIndexEntry, entries)
	offset := 12
	for i := 0; i < entries; i++ {
		mode := binaryBigEndianUint32(data[offset+24 : offset+28])
		flags := uint16(data[offset+60])<<8 | uint16(data[offset+61])
		offset += 62
		end := bytes.IndexByte(data[offset:], 0)
		if end < 0 {
			return nil, errors.New("invalid Git index path")
		}
		path := string(data[offset : offset+end])
		offset += end + 1
		for (offset-12)%8 != 0 {
			offset++
		}
		out[path] = gitIndexEntry{mode: mode, flags: flags}
	}
	return out, nil
}

func validateGitIndex(data []byte) error {
	if len(data) < 12 || string(data[:4]) != "DIRC" {
		return errors.New("invalid Git index header")
	}
	version := binaryBigEndianUint32(data[4:8])
	if version < 2 || version > 3 {
		return errors.New("unsupported Git index version")
	}
	entries := int(binaryBigEndianUint32(data[8:12]))
	if entries < 0 || entries > maxGitMutationPaths*1024 {
		return errors.New("Git index has too many entries")
	}
	offset := 12
	for i := 0; i < entries; i++ {
		if offset+62 > len(data) {
			return errors.New("truncated Git index entry")
		}
		flags := uint16(data[offset+60])<<8 | uint16(data[offset+61])
		if flags&0xc000 != 0 {
			return errors.New("unsupported Git index flags")
		}
		offset += 62
		end := bytes.IndexByte(data[offset:], 0)
		if end < 0 {
			return errors.New("unterminated Git index path")
		}
		offset += end + 1
		for (offset-12)%8 != 0 {
			offset++
		}
		if offset > len(data) {
			return errors.New("invalid Git index alignment")
		}
	}
	if offset+20 > len(data) {
		return errors.New("missing Git index checksum")
	}
	for offset+8 <= len(data)-20 {
		signature := string(data[offset : offset+4])
		size := int(binaryBigEndianUint32(data[offset+4 : offset+8]))
		if size < 0 || offset+8+size > len(data)-20 {
			return errors.New("invalid Git index extension")
		}
		if signature == "sdir" || signature == "link" || signature == "UNTR" || signature == "FSMN" {
			return errors.New("unsupported Git index extension")
		}
		offset += 8 + size
	}
	if offset != len(data)-20 {
		return errors.New("invalid Git index trailer")
	}
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return errors.New("Git index checksum mismatch")
	}
	return nil
}

func binaryBigEndianUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func verifyGitMutationState(ctx context.Context, repo string, before gitMutationState) error {
	after, err := inspectGitMutationState(ctx, repo)
	if err != nil {
		return err
	}
	if after.gitDir != before.gitDir || after.indexPath != before.indexPath || after.branchRef != before.branchRef || after.branch != before.branch || after.head != before.head || after.indexHash != before.indexHash {
		return errors.New("Git repository state changed")
	}
	return nil
}

func readGitStagedReview(ctx context.Context, repo string) (gitStagedReviewResult, error) {
	state, err := inspectGitMutationState(ctx, repo)
	if err != nil {
		return gitStagedReviewResult{}, err
	}
	tempIndex, err := copyGitIndex(state.indexPath)
	if err != nil {
		return gitStagedReviewResult{}, errors.New("temporary Git index creation failed")
	}
	defer removeGitTempIndex(tempIndex)
	diff, err := gitMutationCommand(ctx, repo, tempIndex, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--binary", "--no-renames", "--no-color", "--")
	if err != nil || diff.Truncated || len(diff.Stdout) > maxGitStagedReviewBytes {
		return gitStagedReviewResult{}, errors.New("staged Git review exceeds the bounded limit")
	}
	pathsResult, err := gitMutationCommand(ctx, repo, tempIndex, "diff", "--cached", "--name-only", "-z", "--no-renames", "--")
	if err != nil || pathsResult.Truncated || len(pathsResult.Stdout) > maxGitStagedReviewBytes {
		return gitStagedReviewResult{}, errors.New("staged Git paths exceed the bounded limit")
	}
	paths := splitNulPaths(pathsResult.Stdout)
	identity := strings.Join([]string{repo, state.gitDir, state.branchRef, state.head, state.indexHash, strings.Join(paths, "\x00"), diff.Stdout}, "\x00")
	hash := sha256.Sum256([]byte(identity))
	return gitStagedReviewResult{Repository: repo, Branch: state.branch, Head: state.head, IndexHash: state.indexHash, ReviewHash: hex.EncodeToString(hash[:]), Paths: paths, PathCount: len(paths), Diff: diff.Stdout, Truncated: false}, nil
}

func splitNulPaths(value string) []string {
	raw := strings.Split(value, "\x00")
	out := make([]string, 0, len(raw))
	for _, path := range raw {
		if path != "" {
			out = append(out, filepath.ToSlash(path))
		}
	}
	sort.Strings(out)
	return out
}

func stageGitPaths(ctx context.Context, repo string, paths []string, before gitMutationState) error {
	if err := verifyGitMutationState(ctx, repo, before); err != nil {
		return errors.New("Git repository changed before staging")
	}
	data, err := os.ReadFile(before.indexPath)
	if err != nil {
		return errors.New("Git index is unavailable")
	}
	tmp, err := os.CreateTemp(filepath.Dir(before.indexPath), ".bee-index-")
	if err != nil {
		return errors.New("temporary Git index creation failed")
	}
	tmpPath := tmp.Name()
	defer removeGitTempIndex(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("temporary Git index permissions failed")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("temporary Git index write failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("temporary Git index close failed")
	}
	for _, rel := range paths {
		candidate := filepath.Join(repo, filepath.FromSlash(rel))
		info, statErr := os.Lstat(candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			if _, err := gitMutationCommand(ctx, repo, tmpPath, "update-index", "--remove", "--", rel); err != nil {
				return errors.New("Git deletion staging failed")
			}
			continue
		}
		if statErr != nil {
			return errors.New("Git path inspection failed")
		}
		mode := "100644"
		var content []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(candidate)
			if err != nil || len(target) > maxGitMutationFileBytes {
				return errors.New("Git symlink staging failed")
			}
			mode = "120000"
			content = []byte(target)
		} else {
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			content, err = os.ReadFile(candidate)
			if err != nil || len(content) > maxGitMutationFileBytes {
				return errors.New("Git file staging failed")
			}
		}
		blob, err := gitMutationCommandWithInput(ctx, repo, tmpPath, content, "hash-object", "--no-filters", "-w", "--stdin")
		if err != nil {
			return errors.New("Git object creation failed")
		}
		if _, err := gitMutationCommand(ctx, repo, tmpPath, "update-index", "--add", "--cacheinfo", mode, strings.TrimSpace(blob.Stdout), rel); err != nil {
			return errors.New("Git index update failed")
		}
	}
	newIndex, err := os.ReadFile(tmpPath)
	if err != nil || len(newIndex) > maxGitMutationFileBytes || validateGitIndex(newIndex) != nil {
		return errors.New("produced Git index is invalid or unsupported")
	}
	indexLock, err := acquireGitIndexLock(ctx, before.indexPath)
	if err != nil {
		return errors.New("Git repository index is busy")
	}
	defer indexLock.Close()
	if err := verifyGitMutationState(ctx, repo, before); err != nil {
		return errors.New("Git repository changed while staging")
	}
	current, err := os.ReadFile(before.indexPath)
	if err != nil || !bytes.Equal(current, data) {
		return errors.New("Git index changed while staging")
	}
	if err := writeAndSyncGitIndexLock(indexLock, newIndex); err != nil {
		return errors.New("Git index lock write failed")
	}
	if err := indexLock.Commit(); err != nil {
		return errors.New("Git index replacement failed")
	}
	return nil
}

type gitIndexLock struct {
	file      *os.File
	identity  os.FileInfo
	index     string
	lockPath  string
	committed bool
}

func acquireGitIndexLock(ctx context.Context, indexPath string) (*gitIndexLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lockPath := indexPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	lock := &gitIndexLock{file: file, identity: identity, index: indexPath, lockPath: lockPath}
	if err := file.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func writeAndSyncGitIndexLock(lock *gitIndexLock, data []byte) error {
	if lock == nil || lock.file == nil {
		return errors.New("Git index lock is unavailable")
	}
	if err := lock.file.Truncate(0); err != nil {
		return err
	}
	if _, err := lock.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := lock.file.Write(data); err != nil {
		return err
	}
	return lock.file.Sync()
}

func (lock *gitIndexLock) Commit() error {
	if lock == nil || lock.file == nil || lock.committed {
		return errors.New("Git index lock is unavailable")
	}
	if err := lock.file.Close(); err != nil {
		return err
	}
	lock.file = nil
	if err := os.Rename(lock.lockPath, lock.index); err != nil {
		return err
	}
	lock.committed = true
	dir, err := os.Open(filepath.Dir(lock.index))
	if err != nil {
		// The atomic rename already committed the visible index. Avoid reporting
		// a false mutation failure after that point; directory sync is best effort.
		return nil
	}
	defer dir.Close()
	_ = dir.Sync()
	return nil
}

func (lock *gitIndexLock) Close() error {
	if lock == nil {
		return nil
	}
	var closeErr error
	if lock.file != nil {
		closeErr = lock.file.Close()
		lock.file = nil
	}
	if lock.committed {
		return closeErr
	}
	current, err := os.Lstat(lock.lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return closeErr
	}
	if err != nil || lock.identity == nil || !os.SameFile(lock.identity, current) {
		// The path disappeared or was replaced after creation. Never unlink a
		// lock unless it is still the exact inode created by this instance.
		return closeErr
	}
	if removeErr := os.Remove(lock.lockPath); removeErr != nil && closeErr == nil {
		return removeErr
	}
	return closeErr
}

func copyGitIndex(indexPath string) (string, error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(indexPath), ".bee-index-")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(path) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func removeGitTempIndex(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".lock")
}

func createGitCommit(ctx context.Context, repo string, state gitMutationState, tree, message string) (string, error) {
	if len(tree) != 40 || !safeGitRevision(tree) {
		return "", errors.New("invalid Git tree")
	}
	name, email, err := gitLocalIdentity(ctx, repo)
	if err != nil {
		return "", err
	}
	env := map[string]string{"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email}
	result, err := gitMutationCommandEnv(ctx, repo, state.indexPath, env, "commit-tree", tree, "-p", state.head, "-m", message)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(result.Stdout)
	if len(id) != 40 {
		return "", errors.New("invalid Git commit")
	}
	return id, nil
}

func gitLocalIdentity(ctx context.Context, repo string) (string, string, error) {
	nameResult, err := gitMutationCommand(ctx, repo, "", "config", "--local", "--no-includes", "--get", "user.name")
	if err != nil {
		return "", "", errors.New("repository-local user.name is required")
	}
	emailResult, err := gitMutationCommand(ctx, repo, "", "config", "--local", "--no-includes", "--get", "user.email")
	if err != nil {
		return "", "", errors.New("repository-local user.email is required")
	}
	name, email := strings.TrimSpace(nameResult.Stdout), strings.TrimSpace(emailResult.Stdout)
	if !validIdentityValue(name, 256) || !validIdentityValue(email, 512) || !strings.Contains(email, "@") {
		return "", "", errors.New("repository-local identity is invalid")
	}
	return name, email, nil
}

func validIdentityValue(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func acquireGitMutationLock(ctx context.Context, repo string) (*SessionFileLock, error) {
	hash := sha256.Sum256([]byte(filepath.Clean(repo)))
	path := filepath.Join(os.TempDir(), "bee-git-"+hex.EncodeToString(hash[:8])+".lock")
	return acquireSessionFileLockContext(ctx, path)
}

func gitMutationCommand(ctx context.Context, repo, indexPath string, args ...string) (gitResult, error) {
	return gitMutationCommandEnv(ctx, repo, indexPath, nil, args...)
}
func gitMutationCommandWithInput(ctx context.Context, repo, indexPath string, input []byte, args ...string) (gitResult, error) {
	return gitMutationCommandInputEnv(ctx, repo, indexPath, input, nil, args...)
}
func gitMutationCommandEnv(ctx context.Context, repo, indexPath string, extra map[string]string, args ...string) (gitResult, error) {
	return gitMutationCommandInputEnv(ctx, repo, indexPath, nil, extra, args...)
}
func gitMutationCommandInputEnv(ctx context.Context, repo, indexPath string, input []byte, extra map[string]string, args ...string) (gitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, maxGitMutationCommandTime)
	defer cancel()
	commandArgs := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "core.splitIndex=false", "-c", "index.version=2", "-c", "index.sparse=false", "-c", "core.untrackedCache=false", "-c", "credential.helper=", "-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "gpg.program=", "-c", "core.editor=:", "-c", "sequence.editor=:"}
	commandArgs = append(commandArgs, args...)
	gitBinary := gitMutationBinary()
	command := exec.CommandContext(commandCtx, gitBinary, commandArgs...)
	command.Dir = repo
	env := []string{"PATH=" + filepath.Dir(gitBinary), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_PAGER=cat", "GIT_EXTERNAL_DIFF=", "GIT_DIFF_OPTS="}
	if indexPath != "" {
		env = append(env, "GIT_INDEX_FILE="+indexPath)
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	command.Env = env
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	stdout := &boundedBuffer{limit: maxGitStagedReviewBytes}
	stderr := &boundedBuffer{limit: maxGitStagedReviewBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return gitResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated}, errors.New("bounded Git operation failed")
	}
	return gitResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated}, nil
}

func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func gitMutationBinary() string {
	for _, candidate := range []string{"/usr/bin/git", "/bin/git"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o022 == 0 {
			return candidate
		}
	}
	if candidate, err := exec.LookPath("git"); err == nil {
		if resolved, resolveErr := filepath.Abs(candidate); resolveErr == nil {
			if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o022 == 0 {
				return resolved
			}
		}
	}
	return "git"
}
