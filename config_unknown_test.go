package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnknownFarConfigFieldsLoadWithWarning(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	config, err := LoadConfig(writeTestConfig(t, `{
		"future_feature": {"enabled": true},
		"agent": {"max_tool_steps": 7, "speculative_decoding": true},
		"recurring_jobs": {"jobs": [{"id": "x", "enabled": false, "prompt": "p",
			"schedule": {"interval_seconds": 60, "time_zone": "UTC", "start_at": "2026-01-01T00:00:00Z"},
			"contract": {"allowed_tools": ["read_file"], "workspace_paths": ["/tmp"], "max_wall_clock_seconds": 30, "max_tool_calls": 2,
				"output_target": "session", "notification_target": "inbox", "lease_policy": "budget"}}]}
	}`))
	if err != nil {
		t.Fatalf("newer config rejected: %v", err)
	}
	if config.Agent.MaxToolSteps != 7 {
		t.Fatalf("known field next to an unknown one not applied: %d", config.Agent.MaxToolSteps)
	}
	got := strings.Join(config.UnknownFields(), ",")
	want := "agent.speculative_decoding,future_feature,recurring_jobs.jobs[0].contract.lease_policy"
	if got != want {
		t.Fatalf("unknown fields = %s, want %s", got, want)
	}
	warnings := UnknownConfigFieldWarnings(config)
	if len(warnings) != 3 || !strings.Contains(warnings[0], `ignoring unknown field "agent.speculative_decoding"`) {
		t.Fatalf("warnings = %q", warnings)
	}
}

func TestNearMissConfigFieldsStillFail(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	for body, want := range map[string]string{
		`{"assistant":{"requre_plan":true}}`:                    `unknown field "requre_plan" in assistant (did you mean "require_plan"?)`,
		`{"assistant":{"requirePlan":true}}`:                    `(did you mean "require_plan"?)`,
		`{"agent":{"max_tool_setps":3}}`:                        `(did you mean "max_tool_steps"?)`,
		`{"agnet":{"max_tool_steps":3}}`:                        `unknown field "agnet" (did you mean "agent"?)`,
		`{"recurring_jobs":{"jobs":[{"id":"x","promtp":"p"}]}}`: `in recurring_jobs.jobs[0] (did you mean "prompt"?)`,
	} {
		if _, err := LoadConfig(writeTestConfig(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
	}
}

func TestConfigFieldMatchingFollowsEncodingJSON(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	// encoding/json matches keys case-insensitively, so this is not unknown.
	config, err := LoadConfig(writeTestConfig(t, `{"Agent":{"MAX_TOOL_STEPS":4}}`))
	if err != nil || len(config.UnknownFields()) != 0 || config.Agent.MaxToolSteps != 4 {
		t.Fatalf("case-insensitive keys: %v %v %d", err, config.UnknownFields(), config.Agent.MaxToolSteps)
	}
}

func TestStrictConfigRejectsAnyUnknownField(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	if _, err := LoadConfig(writeTestConfig(t, `{"strict_config":true,"future_feature":1}`)); err == nil || !strings.Contains(err.Error(), `unknown field "future_feature"`) {
		t.Fatalf("strict_config: %v", err)
	}
	t.Setenv("BEE_STRICT_CONFIG", "1")
	if _, err := LoadConfig(writeTestConfig(t, `{"future_feature":1}`)); err == nil || !strings.Contains(err.Error(), `unknown field "future_feature"`) {
		t.Fatalf("BEE_STRICT_CONFIG: %v", err)
	}
	if _, err := LoadConfig(writeTestConfig(t, `{"strict_config":true,"agent":{"max_tool_steps":3}}`)); err != nil {
		t.Fatalf("strict config with only known fields: %v", err)
	}
}

func TestIgnoredConfigFieldNamesAreRedacted(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	config, err := LoadConfig(writeTestConfig(t, `{"token=sk-live-secret-value":1,"agent":{"password: hunter2hunter2":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(append(config.UnknownFields(), UnknownConfigFieldWarnings(config)...), "\n")
	if strings.Contains(all, "sk-live-secret-value") || strings.Contains(all, "hunter2") {
		t.Fatalf("secret-like key echoed: %s", all)
	}
	t.Setenv("BEE_STRICT_CONFIG", "1")
	if _, err := LoadConfig(writeTestConfig(t, `{"token=sk-live-secret-value":1}`)); err == nil || strings.Contains(err.Error(), "sk-live-secret-value") {
		t.Fatalf("strict error echoed key: %v", err)
	}
}

func TestDoctorWarnsAboutIgnoredConfigFields(t *testing.T) {
	t.Setenv("BEE_STRICT_CONFIG", "")
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	if _, err := EnsureBeeLayout(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(`{"future_feature":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report := RunDoctor(home, path)
	found := false
	for _, check := range report.Checks {
		if check.Name == "config_unknown_fields" {
			found = check.Status == CheckWarn && strings.Contains(check.Detail, "future_feature")
		}
		if check.Name == "config_validity" && check.Status != CheckPass {
			t.Fatalf("config_validity = %v %s", check.Status, check.Detail)
		}
	}
	if !found {
		t.Fatalf("no config_unknown_fields warning: %#v", report.Checks)
	}
}
