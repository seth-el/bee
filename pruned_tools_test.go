package bee

import (
	"path/filepath"
	"testing"
)

// None of the pruned tools may reach the main assistant's tool list, its
// reported capabilities, or the harness snapshot, while the tools it keeps
// (including the ones chosen to stay despite low use) are still there.
func TestPrunedToolsAreNotModelFacing(t *testing.T) {
	root := t.TempDir()
	jobs, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobs, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range prunedForegroundTools {
		if _, ok := app.Executor.Registry.Lookup(name); ok {
			t.Fatalf("%s is still registered", name)
		}
		if containsString(app.Capabilities.ToolNames, name) {
			t.Fatalf("%s is still in capabilities", name)
		}
	}
	for _, kept := range []string{"exec", "exec_readonly", "edit_file", "write_file", "read_file", "grep", "find_files", "mutation_list", "mutation_undo", "job_create", "job_list", "job_cancel", "todo_complete", "artifact_read"} {
		if _, ok := app.Executor.Registry.Lookup(kept); !ok {
			t.Fatalf("kept tool %s is missing", kept)
		}
	}
	if len(prunedForegroundTools) != 24 {
		t.Fatalf("pruned list changed size: %d", len(prunedForegroundTools))
	}
}

// A LAN child marked local registers verify_claims and delegate_recon;
// neither may reach the main model.
func TestLocalChildToolsArePrunedFromMainAssistant(t *testing.T) {
	config := writeLANProfilesConfig(t, true)
	root := t.TempDir()
	jobs, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: jobs, ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	if app.Specialist == nil || !app.Specialist.isConfiguredLocalChild() {
		t.Fatal("expected a local child specialist")
	}
	for _, name := range []string{VerifyClaimsToolName, DelegateReconToolName} {
		if _, ok := app.Executor.Registry.Lookup(name); ok {
			t.Fatalf("%s reached the main assistant", name)
		}
	}
}
