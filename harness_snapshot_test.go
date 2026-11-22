package bee

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildHarnessSnapshotIsStableContentFreeAndSensitiveToComposition(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "snapshot-model"
	config.ProviderProfile = "main-test"
	config.ProviderRole = ProviderRoleMain
	config.Agent.SystemPrompt = "private prompt phrase that must not be serialized"
	config.Agent.systemPromptExplicit = true
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{
		Name: "snapshot_tool", Description: "private tool description", Risk: RiskLow,
		Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object"}`),
	}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	app := &Application{
		Config: config, ProviderProfile: config.ProviderProfile, ProviderRole: ProviderRoleMain,
		Executor: &Executor{Registry: registry}, RequirePlan: true,
		Capabilities: AssistantCapabilities{Policy: AssistantPolicyTrusted, ScopeRoots: []string{root}, WriteRoots: []string{root}},
	}
	first, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprints are not stable: %q %q", first.Fingerprint, second.Fingerprint)
	}
	if first.CachePrefixFingerprint == "" || first.CachePrefixFingerprint != second.CachePrefixFingerprint {
		t.Fatalf("cache-prefix fingerprints are not stable: %q %q", first.CachePrefixFingerprint, second.CachePrefixFingerprint)
	}
	if len(first.Tools) != 5 { // registry tool plus plan and context virtual tools
		t.Fatalf("tools = %#v", first.Tools)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private prompt phrase", "private tool description"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, encoded)
		}
	}
	app.Config.Agent.SystemPrompt += " changed"
	changed, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Fingerprint == first.Fingerprint {
		t.Fatal("prompt change did not change the harness fingerprint")
	}
}

func TestHarnessCachePrefixIsStableAcrossPlanMode(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "snapshot-model"
	registry := mustTestRegistry(t)
	app := &Application{
		Config: config, ProviderRole: ProviderRoleMain,
		Executor:     &Executor{Registry: registry},
		Capabilities: AssistantCapabilities{Policy: AssistantPolicyTrusted},
	}
	off, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	app.RequirePlan = true
	on, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if off.Fingerprint == on.Fingerprint {
		t.Fatal("plan enforcement change did not change provenance fingerprint")
	}
	if off.CachePrefixFingerprint != on.CachePrefixFingerprint {
		t.Fatalf("plan mode changed cache-prefix fingerprint: off=%q on=%q", off.CachePrefixFingerprint, on.CachePrefixFingerprint)
	}
	if len(off.Tools) != len(on.Tools) {
		t.Fatalf("plan mode changed stable tool schema count: off=%d on=%d", len(off.Tools), len(on.Tools))
	}
}

func TestHarnessCachePrefixIncludesRenderSettings(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "snapshot-model"
	app := &Application{Config: config, ProviderRole: ProviderRoleMain, Capabilities: AssistantCapabilities{Policy: AssistantPolicyTrusted}}
	low, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	app.Config.Generation.ReasoningEffort = "high"
	high, err := BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if low.CachePrefixFingerprint == high.CachePrefixFingerprint {
		t.Fatal("reasoning render setting did not change cache-prefix fingerprint")
	}
}

func TestHarnessCompositionLinterFindsStructuralProblems(t *testing.T) {
	snapshot := HarnessSnapshot{
		Prompt:     HarnessPromptSnapshot{Bytes: HarnessPromptWarningBytes + 1},
		Agent:      HarnessAgentSnapshot{UnlimitedToolSteps: true},
		Skills:     []HarnessSkillSnapshot{{Name: "review", RequiredTools: []string{"missing"}}},
		Extensions: []HarnessExtensionSnapshot{{Name: "helper", Enabled: true}},
	}
	report := lintHarnessSnapshot(snapshot, []string{"duplicate"})
	if report.Status != "fail" || report.Errors < 2 || report.Warnings < 2 {
		t.Fatalf("report = %#v", report)
	}
	codes := make(map[string]bool)
	for _, issue := range report.Issues {
		codes[issue.Code] = true
	}
	for _, want := range []string{"large_always_on_prompt", "duplicate_tool", "missing_required_tool", "extension_not_started", "unlimited_tool_steps"} {
		if !codes[want] {
			t.Fatalf("missing lint code %q in %#v", want, report.Issues)
		}
	}
}
