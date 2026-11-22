package bee

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedBuffersLimitIOCopyWithoutReadFromBypass(t *testing.T) {
	const limit = 4096
	payload := strings.Repeat("x", limit*4)

	var gitOutput boundedBuffer
	gitOutput.limit = limit
	written, err := io.Copy(&gitOutput, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(payload)) || len(gitOutput.String()) != limit || !gitOutput.truncated {
		t.Fatalf("git buffer written=%d len=%d truncated=%v", written, len(gitOutput.String()), gitOutput.truncated)
	}

	var commandOutput boundedCommandBuffer
	commandOutput.limit = limit
	written, err = io.Copy(&commandOutput, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(payload)) || commandOutput.Len() != limit || !commandOutput.truncated {
		t.Fatalf("command buffer written=%d len=%d truncated=%v", written, commandOutput.Len(), commandOutput.truncated)
	}
}

func TestRunBoundedCommandCapsEachStreamBeforeCombining(t *testing.T) {
	command := exec.Command("sh", "-c", "printf OUTMARK; yes O | head -c 1000; printf ERRMARK >&2; yes E | head -c 1000 >&2")
	output, err := runBoundedCommand(command, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(output) > 100 {
		t.Fatalf("output length=%d exceeds bound", len(output))
	}
	if !strings.Contains(output, "OUTMARK") || !strings.Contains(output, "ERRMARK") {
		t.Fatalf("output did not retain both bounded streams: %q", output)
	}
}

func TestRunGitWithLimitAndGitDiffHonorRequestedDiffBytes(t *testing.T) {
	root := newGitDiffTestRepo(t)
	if err := writeLargeGitChange(root); err != nil {
		t.Fatal(err)
	}
	const limit = 4096
	result, err := runGitWithLimit(context.Background(), root, limit, "diff", "--no-ext-diff", "--binary", "HEAD", "--")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) > limit || !result.Truncated {
		t.Fatalf("runGitWithLimit len=%d truncated=%v", len(result.Stdout), result.Truncated)
	}

	tool := GitDiffTool{Scope: ExecutionScope{Root: root}}
	output, err := tool.Execute(context.Background(), []byte(`{"max_bytes":4096}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		HasChanges bool   `json:"has_changes"`
		Diff       string `json:"diff"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Diff) > limit || !envelope.HasChanges || !envelope.Truncated {
		t.Fatalf("git_diff diff len=%d has_changes=%v truncated=%v", len(envelope.Diff), envelope.HasChanges, envelope.Truncated)
	}
}

func TestBoundedGitDiffJSONPreservesHasChangesWhenEnvelopeTruncates(t *testing.T) {
	out, err := boundedGitDiffJSON(gitDiffResult{
		Repository: "/tmp/repository",
		Ref:        "HEAD",
		Path:       "",
		HasChanges: true,
		Diff:       strings.Repeat("diff --git a/file b/file\n", maxGitReviewBytes),
		Truncated:  false,
	}, maxGitReviewBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxGitReviewBytes {
		t.Fatalf("envelope length=%d exceeds bound", len(out))
	}
	var envelope struct {
		HasChanges bool   `json:"has_changes"`
		Diff       string `json:"diff"`
		Truncated  bool   `json:"truncated"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.HasChanges || !envelope.Truncated || envelope.Diff == "" || !strings.Contains(envelope.Note, "do not repeat") || !strings.Contains(envelope.Note, "bounded envelope") || strings.Contains(envelope.Note, "increase max_bytes") {
		t.Fatalf("envelope=%#v", envelope)
	}
}

func writeLargeGitChange(root string) error {
	return os.WriteFile(filepath.Join(root, "README.md"), []byte(strings.Repeat("large change\n", 20000)), 0o600)
}
