package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/memory-dreaming/recurring-job.json is generated from the canonical
// prompt recorded in AGENTS.md. This keeps the two from drifting and keeps
// the shipped entry loadable under the strict config decoder.
func TestMemoryDreamingConfigMatchesCanonicalPrompt(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("docs", "memory-dreaming", "recurring-job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		RecurringJobs RecurringJobsConfig `json:"recurring_jobs"`
	}
	if err := decodeConfigJSON(data, &document); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if err := document.RecurringJobs.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(document.RecurringJobs.Jobs) != 1 {
		t.Fatalf("jobs=%d", len(document.RecurringJobs.Jobs))
	}
	job := document.RecurringJobs.Jobs[0]
	if job.ID != "dreaming" || !job.Enabled {
		t.Fatalf("job=%#v", job)
	}
	if warnings := document.RecurringJobs.ScheduleWarnings(job.Schedule.StartAt); len(warnings) != 0 {
		t.Fatalf("schedule warnings: %v", warnings)
	}
	contract := job.Contract
	if strings.Join(contract.WorkspacePaths, ",") != "/home/user/.bee/workspace/memory/topics,/home/user/.bee/workspace/memory/dashboard" ||
		strings.Join(contract.ReadOnlyPaths, ",") != "/home/user/.bee/workspace/memory" {
		t.Fatalf("contract roots=%v / %v", contract.WorkspacePaths, contract.ReadOnlyPaths)
	}
	if contract.MaxWallClockSeconds > 15*60 {
		t.Fatalf("wall clock %ds exceeds the fixed 15 minute attempt lease", contract.MaxWallClockSeconds)
	}

	agents, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(agents)
	marker := strings.Index(text, "**Final prompt**")
	if marker < 0 {
		t.Fatal("AGENTS.md lacks the canonical final prompt")
	}
	open := strings.Index(text[marker:], "```text\n")
	if open < 0 {
		t.Fatal("final prompt fence not found")
	}
	body := text[marker+open+len("```text\n"):]
	end := strings.Index(body, "\n```")
	if end < 0 {
		t.Fatal("final prompt fence not closed")
	}
	if canonical := body[:end]; canonical != job.Prompt {
		t.Fatalf("recurring-job.json prompt drifted from AGENTS.md (json %d bytes, canonical %d bytes)", len(job.Prompt), len(canonical))
	}
	for _, required := range []string{"You are the assistant agent", "conversation_search", "/home/user/.bee/workspace/memory/dashboard/MEMORY.md", "128 tool calls and\n15 minutes"} {
		if !strings.Contains(job.Prompt, required) {
			t.Fatalf("prompt lacks %q", required)
		}
	}
}
