package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const RepoCloneToolName = "repo_clone"

type RepoCloneTool struct{ Scope ExecutionScope }

type repoCloneArgs struct {
	Repository string `json:"repository"`
	Name       string `json:"name,omitempty"`
}

type repoCloneResult struct {
	Repository  string `json:"repository"`
	Destination string `json:"destination"`
	Output      string `json:"output,omitempty"`
}

func (tool RepoCloneTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        RepoCloneToolName,
		Description: "Clone one Git repository into Bee's canonical workspace/projects directory. The destination is derived safely; no arbitrary destination path is accepted. Prefer this over exec with git clone.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"repository":{"type":"string","minLength":1,"maxLength":4096},"name":{"type":"string","minLength":1,"maxLength":128}},"required":["repository"]}`),
	}
}

func (tool RepoCloneTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input repoCloneArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("repo_clone arguments: %w", err)
	}
	repository := strings.TrimSpace(input.Repository)
	if repository == "" || len(repository) > 4096 || strings.ContainsRune(repository, '\x00') {
		return "", errors.New("repository is invalid")
	}
	if err := rejectCloneURLCredentials(repository); err != nil {
		return "", err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = cloneRepositoryName(repository)
	}
	if name == "" || projectSlug(name) != name {
		return "", errors.New("clone name must be a lowercase slug containing only letters, digits, and single dashes")
	}
	projectsRoot, err := DefaultBeeProjectsRoot()
	if err != nil {
		return "", err
	}
	destination := filepath.Join(projectsRoot, name)
	if _, err := secureMutationPath(ctx, tool.Scope, destination, true); err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", errors.New("clone destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(projectsRoot, 0o700); err != nil {
		return "", fmt.Errorf("create projects root: %w", err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return "", errors.New("git is unavailable")
	}
	program, commandArgs, err := sandboxCommandInvocation(tool.Scope, git, []string{"clone", "--", repository, destination}, projectsRoot)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, program, commandArgs...)
	command.Dir = projectsRoot
	command.Env = preserveProcessSandboxMarker(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), tool.Scope.EnforceWriteRoots)
	output, _, runErr := runCapturedCommand(command, tool.Scope.maxOutput(), false)
	if runErr != nil {
		return output.Text, fmt.Errorf("git clone failed: %w", runErr)
	}
	encoded, err := json.Marshal(repoCloneResult{Repository: repository, Destination: destination, Output: output.Text})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func cloneRepositoryName(repository string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(repository, "/"), ".git")
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Path != "" {
		trimmed = parsed.Path
	}
	if colon := strings.LastIndex(trimmed, ":"); colon >= 0 && !strings.Contains(trimmed[colon+1:], string(filepath.Separator)) {
		trimmed = trimmed[colon+1:]
	}
	return projectSlug(filepath.Base(trimmed))
}

func rejectCloneURLCredentials(repository string) error {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme == "" || parsed.User == nil {
		return nil
	}
	if _, hasPassword := parsed.User.Password(); hasPassword || parsed.Scheme == "http" || parsed.Scheme == "https" {
		return errors.New("repository URLs must not contain embedded credentials")
	}
	return nil
}
