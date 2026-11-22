package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAppliesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":{"model":"test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider.Type != DefaultProviderType || config.Provider.BaseURL != DefaultBaseURL {
		t.Fatalf("provider defaults = %#v", config.Provider)
	}
	if config.Generation.Temperature != DefaultTemperature || config.Generation.MaxTokens != DefaultMaxTokens {
		t.Fatalf("generation defaults = %#v", config.Generation)
	}
	if config.Generation.MainMaxTokens != nil {
		t.Fatalf("main generation override default = %v, want nil", *config.Generation.MainMaxTokens)
	}
	if config.Provider.Model != "test-model" {
		t.Fatalf("model = %q", config.Provider.Model)
	}
	if config.Agent.SystemPrompt != DefaultSystemPrompt {
		t.Fatalf("system prompt default = %q", config.Agent.SystemPrompt)
	}
	if config.Agent.MaxToolSteps != DefaultMaxToolSteps {
		t.Fatalf("agent max_tool_steps default = %d, want %d", config.Agent.MaxToolSteps, DefaultMaxToolSteps)
	}
	if config.Agent.ToolLandingSteps != DefaultToolLandingSteps || config.Agent.TurnTimeoutSeconds != DefaultTurnTimeoutSeconds || config.Agent.FinalizationTimeoutSeconds != DefaultFinalizationTimeoutSeconds || config.Agent.FinalizationMaxTokens != DefaultFinalizationMaxTokens || config.Agent.NoProgressLimit != DefaultNoProgressLimit {
		t.Fatalf("agent turn-control defaults = %#v", config.Agent)
	}
	if config.Agent.maxToolStepsExplicit {
		t.Fatal("omitted agent max_tool_steps must not be marked explicit")
	}
	if DefaultConfig().Memory.RecallMode != MemoryRecallHybrid {
		t.Fatalf("default memory recall mode = %q, want hybrid", DefaultConfig().Memory.RecallMode)
	}
	if config.Assistant.UnsafeDisableProcessSandbox {
		t.Fatal("process sandbox must be enabled by default")
	}
}

func TestLoadConfigRequiresExplicitUnsafeProcessSandboxOptOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"assistant":{"policy":"trusted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.UnsafeDisableProcessSandbox {
		t.Fatal("omitting unsafe_disable_process_sandbox disabled the sandbox")
	}
	if err := os.WriteFile(path, []byte(`{"assistant":{"policy":"trusted","unsafe_disable_process_sandbox":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Assistant.UnsafeDisableProcessSandbox {
		t.Fatal("explicit unsafe process sandbox opt-out was not loaded")
	}
}

func TestLoadConfigApprovalModesDefaultAndFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":{"model":"test-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.ApprovalMode != ApprovalModeManual || config.Serve.ApprovalMode != ApprovalModeManual {
		t.Fatalf("approval defaults: assistant=%q serve=%q", config.Assistant.ApprovalMode, config.Serve.ApprovalMode)
	}

	if err := os.WriteFile(path, []byte(`{"assistant":{"approval_mode":"auto"},"serve":{"approval_mode":"auto"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.ApprovalMode != ApprovalModeAuto || config.Serve.ApprovalMode != ApprovalModeAuto {
		t.Fatalf("approval config: assistant=%q serve=%q", config.Assistant.ApprovalMode, config.Serve.ApprovalMode)
	}

	for _, body := range []string{
		`{"assistant":{"approval_mode":"sometimes"}}`,
		`{"serve":{"approval_mode":"sometimes"}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "approval mode") {
			t.Fatalf("invalid approval mode did not fail closed: %v", err)
		}
	}
}

func TestLoadConfigTurnControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agent":{"tool_landing_steps":4,"turn_timeout_seconds":0,"deadline_warning_seconds":15,"finalization_timeout_seconds":30,"finalization_max_tokens":768,"no_progress_limit":5,"max_auto_continuations":2,"auto_compact_between_segments":true}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	agent := config.Agent
	if agent.ToolLandingSteps != 4 || agent.TurnTimeoutSeconds != 0 || agent.DeadlineWarningSeconds != 15 || agent.FinalizationTimeoutSeconds != 30 || agent.FinalizationMaxTokens != 768 || agent.NoProgressLimit != 5 || agent.MaxAutoContinuations != 2 || !agent.AutoCompactBetweenSegments {
		t.Fatalf("turn controls=%#v", agent)
	}
}

func TestLoadConfigReasoningEffortIsOptionalValidatedAndForwarded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"generation":{"reasoning_effort":"high"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Generation.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort=%q", config.Generation.ReasoningEffort)
	}
	if err := os.WriteFile(path, []byte(`{"generation":{"reasoning_effort":"extreme"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "reasoning_effort") {
		t.Fatalf("invalid reasoning effort error=%v", err)
	}
}

func TestLoadConfigToolStepDefaultAndExplicitOverrides(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		want     int
		explicit bool
	}{
		{name: "omitted uses bounded default", body: `{}`, want: DefaultMaxToolSteps},
		{name: "explicit zero preserves unlimited override", body: `{"agent":{"max_tool_steps":0}}`, want: 0, explicit: true},
		{name: "explicit positive override", body: `{"agent":{"max_tool_steps":7}}`, want: 7, explicit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.Agent.MaxToolSteps != test.want {
				t.Fatalf("agent max_tool_steps = %d, want %d", config.Agent.MaxToolSteps, test.want)
			}
			if config.Agent.maxToolStepsExplicit != test.explicit {
				t.Fatalf("agent max_tool_steps explicit = %v, want %v", config.Agent.maxToolStepsExplicit, test.explicit)
			}
			agent, err := NewAgent(config, &fakeProvider{})
			if err != nil {
				t.Fatal(err)
			}
			if agent.maxToolSteps != test.want {
				t.Fatalf("effective agent max_tool_steps = %d, want %d", agent.maxToolSteps, test.want)
			}
		})
	}
}

func TestLoadConfigMemoryRecallMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"memory":{"recall_mode":"hybrid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Memory.RecallMode != MemoryRecallHybrid {
		t.Fatalf("memory recall mode = %q, want hybrid", config.Memory.RecallMode)
	}

	if err := os.WriteFile(path, []byte(`{"memory":{"recall_mode":"invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "memory recall_mode") {
		t.Fatalf("invalid recall mode error = %v", err)
	}
}

func TestLoadConfigLoadsVoiceProfileRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"routing":{"main_profile":"local-main-8081","child_profile":"local-child-8082","voice_profile":"local-main-8081","max_children":3}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Routing.VoiceProfile != "local-main-8081" {
		t.Fatalf("loaded voice profile = %q", config.Routing.VoiceProfile)
	}
	if len(config.Routing.ManualProfiles) != 0 {
		t.Fatalf("loaded manual profiles = %#v, want none", config.Routing.ManualProfiles)
	}
}

func TestLoadConfigSpecialistDefaultsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"specialist":{"max_concurrency":2,"failure_threshold":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Specialist.Profile != "" || config.Specialist.MaxConcurrency != 2 || config.Specialist.FailureThreshold != 3 {
		t.Fatalf("specialist config=%#v", config.Specialist)
	}
	if config.Specialist.TimeoutSeconds != DefaultSpecialistTimeoutSeconds {
		t.Fatalf("specialist timeout default=%d, want %d", config.Specialist.TimeoutSeconds, DefaultSpecialistTimeoutSeconds)
	}
	if config.Specialist.ObservationThresholdBytes != DefaultSpecialistObservationThreshold {
		t.Fatalf("specialist observation threshold default=%d, want %d", config.Specialist.ObservationThresholdBytes, DefaultSpecialistObservationThreshold)
	}
	if config.Specialist.ObservationCompressionTimeoutSeconds != DefaultSpecialistCompressionTimeoutSeconds {
		t.Fatalf("specialist compression timeout default=%d, want %d", config.Specialist.ObservationCompressionTimeoutSeconds, DefaultSpecialistCompressionTimeoutSeconds)
	}
	if config.Specialist.SessionDigestEnabled {
		t.Fatal("session digest must be opt-in by default")
	}
	if config.Specialist.ImageSidecarEnabled {
		t.Fatal("image sidecar must be disabled by default")
	}
	if config.Specialist.ImageMaxPixels != DefaultSpecialistImageMaxPixels {
		t.Fatalf("specialist image pixel default=%d, want %d", config.Specialist.ImageMaxPixels, DefaultSpecialistImageMaxPixels)
	}
	service, err := NewSpecialistService(config)
	if err != nil || service.Config().TimeoutSeconds != DefaultSpecialistTimeoutSeconds {
		t.Fatalf("effective specialist=%#v err=%v", service, err)
	}
}

func TestLoadConfigSpecialistImageSidecarRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"specialist":{"image_sidecar_enabled":true,"image_max_pixels":1000000},"profiles":[{"name":"local-main-8081","role":"main","provider":{"type":"openai-compatible","base_url":"http://127.0.0.1:8081","model":"local","image_input":true}},{"name":"local-child-8082","role":"child","provider":{"type":"openai-compatible","base_url":"http://127.0.0.1:8082","model":"local","image_input":true}},{"name":"cloud","role":"manual","provider":{"type":"openai-compatible","base_url":"https://example.invalid/v1","model":"cloud-model"}}],"routing":{"main_profile":"local-main-8081","child_profile":"local-child-8082"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Specialist.ImageSidecarEnabled {
		t.Fatal("image sidecar flag was not loaded")
	}
	if config.Specialist.ImageMaxPixels != 1000000 {
		t.Fatalf("image pixel override=%d", config.Specialist.ImageMaxPixels)
	}
	encoded, err := json.Marshal(config.Specialist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"image_sidecar_enabled":true`) {
		t.Fatalf("specialist JSON=%s", encoded)
	}

	for _, mutate := range []struct {
		name string
		fn   func(*Config)
		want string
	}{
		{name: "disabled specialist", fn: func(config *Config) { config.Specialist.Enabled = false }, want: "specialist.enabled=true"},
		{name: "missing profiles", fn: func(config *Config) { config.Profiles = nil; config.Routing = ProviderRoutingConfig{} }, want: "configured local child specialist profile"},
		{name: "non-local specialist", fn: func(config *Config) { config.Profiles[1].Provider.BaseURL = "https://example.invalid/v1" }, want: "local child provider"},
		{name: "child image capability omitted", fn: func(config *Config) { config.Profiles[1].Provider.ImageInput = false }, want: "image_input=true"},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			copy := config
			copy.Profiles = append([]ProviderProfile(nil), config.Profiles...)
			mutate.fn(&copy)
			if err := copy.Validate(); err == nil || !strings.Contains(err.Error(), mutate.want) {
				t.Fatalf("validation error=%v, want %q", err, mutate.want)
			}
		})
	}
}

func TestLoadConfigSpecialistObservationCompressionTimeoutOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"specialist":{"observation_compression_timeout_seconds":17}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Specialist.ObservationCompressionTimeoutSeconds != 17 {
		t.Fatalf("specialist compression timeout=%d, want 17", config.Specialist.ObservationCompressionTimeoutSeconds)
	}
}

func TestLoadConfigSpecialistSessionDigestFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"specialist":{"session_digest_enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Specialist.SessionDigestEnabled {
		t.Fatal("explicit session_digest_enabled=true was lost during default merge")
	}
}

func TestLoadConfigSpecialistExplicitDisableOverridesEnabledDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"specialist":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Specialist.Enabled {
		t.Fatal("explicit specialist.enabled=false was lost during default merge")
	}
	service, err := NewSpecialistService(config)
	if err != nil {
		t.Fatal(err)
	}
	if service != nil {
		t.Fatal("disabled specialist unexpectedly constructed")
	}
}

func TestLoadConfigVoiceProfileOnlyKeepsRoleDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"routing":{"voice_profile":"local-main-8081"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Routing.MainProfile != "local-main-8081" || config.Routing.ChildProfile != "local-child-8082" || config.Routing.VoiceProfile != "local-main-8081" {
		t.Fatalf("routing defaults after voice-only load = %#v", config.Routing)
	}
}

func TestLoadConfigImageInputDefaultsOnlyForLocalMain(t *testing.T) {
	for _, test := range []struct {
		name string
		json string
		want bool
	}{
		{name: "local main", json: `{"provider":{"base_url":"http://127.0.0.1:8081"}}`, want: true},
		{name: "other endpoint", json: `{"provider":{"base_url":"http://127.0.0.1:8082"}}`, want: false},
		{name: "explicit remote opt in", json: `{"provider":{"base_url":"https://example.invalid/v1","image_input":true}}`, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.json), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.Provider.ImageInput != test.want {
				t.Fatalf("image_input = %v, want %v", config.Provider.ImageInput, test.want)
			}
		})
	}
}

func TestLoadConfigPreservesOperatorSystemPromptAndExplicitEmpty(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{name: "operator prompt", config: `{"agent":{"system_prompt":"custom instructions"}}`, want: "custom instructions"},
		{name: "explicit empty legacy override", config: `{"agent":{"system_prompt":""}}`, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.Agent.SystemPrompt != test.want {
				t.Fatalf("system prompt = %q, want %q", config.Agent.SystemPrompt, test.want)
			}
		})
	}
}

func TestLoadConfigMissingFileIsActionable(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "--print-example") || !strings.Contains(err.Error(), "--init-config") {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestLoadConfigPlanRequirementDistinguishesOmittedAndExplicitFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"assistant":{"policy":"trusted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.RequirePlan {
		t.Fatal("omitted require_plan did not preserve the disabled default")
	}
	if err := os.WriteFile(path, []byte(`{"assistant":{"policy":"trusted","require_plan":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Assistant.RequirePlan {
		t.Fatal("explicit require_plan=false was ignored")
	}
}

func TestLoadConfigOpenAIServeSettingsAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"serve":{"enabled":true,"addr":"0.0.0.0:19000","token_file":"/tmp/bee-serve-token","policy":"trusted","max_concurrent":3,"heartbeat_seconds":20,"request_timeout_seconds":600}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Serve.Enabled || config.Serve.Addr != "0.0.0.0:19000" || config.Serve.TokenFile != "/tmp/bee-serve-token" || config.Serve.Policy != AssistantPolicyTrusted || config.Serve.MaxConcurrent != 3 || config.Serve.HeartbeatSeconds != 20 || config.Serve.RequestTimeoutSeconds != 600 {
		t.Fatalf("serve config=%#v", config.Serve)
	}

	if err := os.WriteFile(path, []byte(`{"serve":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultOpenAIServeConfig()
	if config.Serve.Addr != defaults.Addr || config.Serve.Policy != AssistantPolicyPrompted || config.Serve.MaxConcurrent != defaults.MaxConcurrent || config.Serve.HeartbeatSeconds != defaults.HeartbeatSeconds || config.Serve.RequestTimeoutSeconds != defaults.RequestTimeoutSeconds {
		t.Fatalf("serve defaults=%#v", config.Serve)
	}

	if err := os.WriteFile(path, []byte(`{"serve":{"max_concurrent":999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("invalid serve max_concurrent error=%v", err)
	}

}

func TestWebGatewayConcurrencyDefaultsAndBounds(t *testing.T) {
	if got := (WebConfig{}).EffectiveMaxConcurrentRuns(); got != DefaultGatewayMaxConcurrentRuns {
		t.Fatalf("default gateway concurrency = %d", got)
	}
	config := DefaultConfig()
	config.Web.MaxConcurrentRuns = MaxGatewayConcurrentRuns + 1
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "max_concurrent_runs") {
		t.Fatalf("invalid gateway concurrency error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"web":{"time_zone":"UTC"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Web.MaxConcurrentRuns != DefaultGatewayMaxConcurrentRuns {
		t.Fatalf("loaded default gateway concurrency = %d", loaded.Web.MaxConcurrentRuns)
	}
}

func TestLoadOpenAIServeTokenRequiresPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := LoadOpenAIServeToken(path); err != nil || token != "secret-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOpenAIServeToken(path); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("public token file error=%v", err)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"generation":{"max_tokens":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("expected max_tokens validation error, got %v", err)
	}
}

func TestConfigValidationKeepsGlobalMaxTokensPositive(t *testing.T) {
	config := DefaultConfig()
	if config.Generation.MaxTokens != DefaultMaxTokens {
		t.Fatalf("default max_tokens = %d, want %d", config.Generation.MaxTokens, DefaultMaxTokens)
	}
	config.Generation.MaxTokens = 0
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "generation max_tokens") {
		t.Fatalf("zero max_tokens validation error = %v", err)
	}
}

func TestLoadConfigMainMaxTokensOverridePreservesNilAndLoadsExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"generation":{"main_max_tokens":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Generation.MainMaxTokens == nil || *config.Generation.MainMaxTokens != 0 {
		t.Fatalf("loaded main_max_tokens = %v, want pointer to zero", config.Generation.MainMaxTokens)
	}
	if config.Generation.MaxTokens != DefaultMaxTokens {
		t.Fatalf("global max_tokens = %d, want %d", config.Generation.MaxTokens, DefaultMaxTokens)
	}

	if err := os.WriteFile(path, []byte(`{"generation":{"main_max_tokens":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "main_max_tokens") {
		t.Fatalf("negative main_max_tokens validation error = %v", err)
	}
}

func TestLoadConfigSemanticDefaultsToDisabledAndLoadsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"semantic":{"enabled":true,"endpoint":"http://127.0.0.1:8090","model":"embedding-model","index_root":"/tmp/bee-semantic","timeout_seconds":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Semantic.Enabled || config.Semantic.Endpoint != "http://127.0.0.1:8090" || config.Semantic.Model != "embedding-model" || config.Semantic.IndexRoot != "/tmp/bee-semantic" || config.Semantic.TimeoutSeconds != 7 {
		t.Fatalf("semantic config=%#v", config.Semantic)
	}
	if DefaultConfig().Semantic.Enabled {
		t.Fatal("semantic retrieval must remain disabled by default")
	}
}

func TestLoadConfigSpeechPathsAreOptionalAndPortable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"speech":{"transcription_script":"tools/transcribe","synthesis_wrapper":"tools/synthesize","synthesis_working_directory":"workspace","synthesis_output_directory":"workspace/generated/voice-replies"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Speech.TranscriptionScript != "tools/transcribe" || config.Speech.SynthesisWrapper != "tools/synthesize" {
		t.Fatalf("speech config=%#v", config.Speech)
	}
	if defaults := DefaultConfig().Speech; defaults.TranscriptionScript != "" || defaults.SynthesisWrapper != "" {
		t.Fatalf("speech executables must be disabled by default: %#v", defaults)
	}
}

func TestAssistantWriteRootConfigurationIsStrict(t *testing.T) {
	config := DefaultConfig()
	config.Assistant.WriteRoot = "/tmp/one"
	config.Assistant.WriteRoots = []string{"/tmp/two"}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "write_root cannot be combined") {
		t.Fatalf("write root conflict err=%v", err)
	}
	config = DefaultConfig()
	config.Assistant.WriteRoots = []string{""}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "write_roots[0]") {
		t.Fatalf("empty write root err=%v", err)
	}
}

func TestConfigRejectsInvalidSemanticSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{name: "endpoint", config: Config{Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: DefaultBaseURL, Model: DefaultModel, TimeoutSeconds: DefaultTimeout}, Generation: GenerationConfig{Temperature: DefaultTemperature, MaxTokens: DefaultMaxTokens}, Agent: AgentConfig{SystemPrompt: DefaultSystemPrompt, MaxToolSteps: DefaultMaxToolSteps}, Semantic: SemanticConfig{Endpoint: "not-an-url"}}, want: "semantic config"},
		{name: "model", config: Config{Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: DefaultBaseURL, Model: DefaultModel, TimeoutSeconds: DefaultTimeout}, Generation: GenerationConfig{Temperature: DefaultTemperature, MaxTokens: DefaultMaxTokens}, Agent: AgentConfig{SystemPrompt: DefaultSystemPrompt, MaxToolSteps: DefaultMaxToolSteps}, Semantic: SemanticConfig{Model: "bad model"}}, want: "semantic model"},
		{name: "index", config: Config{Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: DefaultBaseURL, Model: DefaultModel, TimeoutSeconds: DefaultTimeout}, Generation: GenerationConfig{Temperature: DefaultTemperature, MaxTokens: DefaultMaxTokens}, Agent: AgentConfig{SystemPrompt: DefaultSystemPrompt, MaxToolSteps: DefaultMaxToolSteps}, Semantic: SemanticConfig{IndexRoot: "/"}}, want: "semantic index_root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.config.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestWriteExampleConfigDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := WriteExampleConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteExampleConfig(path); err == nil {
		t.Fatal("expected overwrite refusal")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"approval_mode": "manual"`) {
		t.Fatalf("example config does not make the fail-closed approval default explicit: %s", data)
	}
}

func TestLoadConfigRejectsUnknownDuplicateAndTrailingJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		// A far unknown field now loads with a warning (G7); see
		// config_unknown_test.go. Near misses still fail.
		{name: "near-miss nested field", body: `{"assistant":{"require_plan":true,"requre_plan":false}}`, want: `unknown field "requre_plan" in assistant (did you mean "require_plan"?)`},
		{name: "duplicate nested field", body: `{"agent":{"max_tool_steps":2,"max_tool_steps":3}}`, want: "duplicate config key"},
		{name: "trailing document", body: `{} {}`, want: "trailing JSON"},
		{name: "null document", body: `null`, want: "JSON object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadConfig error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadConfigProviderSourcesAreUnambiguous(t *testing.T) {
	legacyData, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(t.TempDir(), "legacy-generated.json")
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadConfig(legacyPath)
	if err != nil {
		t.Fatalf("matching legacy compatibility provider was rejected: %v", err)
	}
	if legacy.providerExplicit {
		t.Fatal("matching compatibility provider remained authoritative over named routing")
	}

	example, err := ExampleConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	conflictingProvider := `  "provider": {"type":"openai-compatible","base_url":"http://127.0.0.1:9999","model":"legacy-override","timeout_seconds":0},` + "\n"
	conflicting := strings.Replace(string(example), "{\n", "{\n"+conflictingProvider, 1)
	conflictPath := filepath.Join(t.TempDir(), "conflict.json")
	if err := os.WriteFile(conflictPath, []byte(conflicting), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(conflictPath); err == nil || !strings.Contains(err.Error(), "top-level provider conflicts") {
		t.Fatalf("provider conflict error=%v", err)
	}
}

func TestLoadConfigPreservesExplicitContextZeroValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"context":{"reserve_tokens":0,"output_reserve_tokens":0,"tool_result_tokens":0,"summary_tokens":0,"auto_compact":false}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Context != (ContextKernelConfig{}) {
		t.Fatalf("explicit context zeros were replaced: %#v", config.Context)
	}
}
