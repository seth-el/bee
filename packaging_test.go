package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializeBeeIsIdempotentAndPreservesConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	first, err := InitializeBee(home)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ConfigCreated || !first.ContextCreated {
		t.Fatalf("first init = %#v", first)
	}
	configData, err := os.ReadFile(first.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.ConfigPath, []byte("{\"provider\":{\"model\":\"operator-model\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := InitializeBee(home)
	if err != nil {
		t.Fatal(err)
	}
	if second.ConfigCreated || second.ContextCreated {
		t.Fatalf("second init = %#v", second)
	}
	updated, err := os.ReadFile(second.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) == string(configData) || !strings.Contains(string(updated), "operator-model") {
		t.Fatalf("existing config was overwritten: %q", updated)
	}
}

func TestInitializeBeeWritesMinimalBehaviorallyStableConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	t.Setenv("BEE_HOME", home)
	report, err := InitializeBee(home)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(report.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"provider", "generation", "web", "semantic", "memory", "coding", "operations", "terminal", "specialist", "child_broker", "context"} {
		if _, present := document[unwanted]; present {
			t.Fatalf("generated config unexpectedly freezes or exposes %q: %s", unwanted, data)
		}
	}
	var agent map[string]json.RawMessage
	if err := json.Unmarshal(document["agent"], &agent); err != nil {
		t.Fatal(err)
	}
	if _, present := agent["system_prompt"]; present {
		t.Fatalf("generated config pins the inline system prompt: %s", data)
	}
	if _, present := agent["system_prompt_file"]; !present {
		t.Fatalf("generated config does not expose system_prompt_file: %s", data)
	}
	if strings.Contains(string(data), "openrouter.ai") {
		t.Fatalf("generated config includes an opt-in cloud profile: %s", data)
	}
	var routing ProviderRoutingConfig
	if err := json.Unmarshal(document["routing"], &routing); err != nil {
		t.Fatal(err)
	}
	if len(routing.ManualProfiles) != 0 {
		t.Fatalf("generated config has default manual profiles: %#v", routing.ManualProfiles)
	}

	soulPath := filepath.Join(home, "workspace", "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("Prefer concise operator-facing answers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(report.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range config.Profiles {
		if profile.Provider.TimeoutSeconds != DefaultTimeout {
			t.Fatalf("generated profile %q timeout=%d, want inherited default %d", profile.Name, profile.Provider.TimeoutSeconds, DefaultTimeout)
		}
	}
	prompt, err := ResolveSystemPrompt(config.Agent)
	if err != nil {
		t.Fatal(err)
	}
	soulIndex := strings.Index(prompt, "Prefer concise operator-facing answers.")
	workspaceIndex := strings.Index(prompt, "# Bee workspace")
	if !strings.HasPrefix(prompt, DefaultSystemPrompt+"\n\n") || soulIndex < 0 || workspaceIndex < 0 || soulIndex >= workspaceIndex || !strings.HasSuffix(prompt, "</bee-workspace-context>") {
		t.Fatalf("generated config did not compose SOUL.md and BEE.md with the built-in prompt: %q", prompt)
	}
	if config.Agent.systemPromptExplicit || !config.Agent.systemPromptFileExplicit {
		t.Fatalf("generated prompt presence metadata is wrong: %#v", config.Agent)
	}
	app, err := NewApplicationForProfile(config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.ProviderProfile != config.Routing.MainProfile || app.Config.Provider.BaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("generated config selected profile=%q provider=%#v", app.ProviderProfile, app.Config.Provider)
	}
}

func TestInitializeBeeRejectsSymlinkHome(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeBee(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestRunDoctorReportsPermissionsAndDoesNotLeakKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	if _, err := InitializeBee(home); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(home, "secrets", "provider-key")
	if err := os.WriteFile(keyPath, []byte("sk-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"api_key_file": ""`, `"api_key_file": "`+keyPath+`"`, 1))
	// The example omits the empty field, so append a minimal valid provider key file config.
	data = []byte(strings.Replace(string(data), `"provider": {`, "\"provider\": {\n    \"api_key_file\": \""+keyPath+"\",", 1))
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	report := RunDoctor(home, "")
	if report.Status != CheckPass {
		t.Fatalf("doctor status=%s checks=%#v", report.Status, report.Checks)
	}
	for _, check := range report.Checks {
		if strings.Contains(check.Detail, "sk-secret-value") {
			t.Fatalf("doctor leaked secret in %#v", check)
		}
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	report = RunDoctor(home, "")
	if report.Status != CheckFail {
		t.Fatalf("unsafe key permissions status=%s checks=%#v", report.Status, report.Checks)
	}
}
