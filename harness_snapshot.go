package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	HarnessSnapshotSchemaVersion = 1
	HarnessPromptWarningBytes    = 32 << 10
)

type HarnessLintSeverity string

const (
	HarnessLintWarning HarnessLintSeverity = "warning"
	HarnessLintError   HarnessLintSeverity = "error"
)

// HarnessLintIssue is a content-free composition diagnostic. It identifies a
// structural problem without copying prompt, skill, or tool-description text.
type HarnessLintIssue struct {
	Severity  HarnessLintSeverity `json:"severity"`
	Code      string              `json:"code"`
	Component string              `json:"component,omitempty"`
	Detail    string              `json:"detail"`
}

type HarnessLintReport struct {
	Status   string             `json:"status"`
	Warnings int                `json:"warnings"`
	Errors   int                `json:"errors"`
	Issues   []HarnessLintIssue `json:"issues,omitempty"`
}

type HarnessPromptComponent struct {
	Kind         string `json:"kind"`
	Source       string `json:"source,omitempty"`
	Present      bool   `json:"present"`
	Bytes        int    `json:"bytes"`
	ApproxTokens int    `json:"approx_tokens"`
	Fingerprint  string `json:"fingerprint,omitempty"`
}

type HarnessPromptSnapshot struct {
	Mode         string                   `json:"mode"`
	Bytes        int                      `json:"bytes"`
	ApproxTokens int                      `json:"approx_tokens"`
	Fingerprint  string                   `json:"fingerprint"`
	Components   []HarnessPromptComponent `json:"components,omitempty"`
}

type HarnessProviderSnapshot struct {
	Profile             string       `json:"profile,omitempty"`
	Role                ProviderRole `json:"role,omitempty"`
	Type                string       `json:"type"`
	Model               string       `json:"model"`
	EndpointHost        string       `json:"endpoint_host,omitempty"`
	ContextWindowTokens int          `json:"context_window_tokens,omitempty"`
	Fingerprint         string       `json:"fingerprint"`
}

type HarnessToolSnapshot struct {
	Name             string         `json:"name"`
	Risk             RiskLevel      `json:"risk,omitempty"`
	Mutability       ToolMutability `json:"mutability,omitempty"`
	PlanExempt       bool           `json:"plan_exempt,omitempty"`
	DescriptionBytes int            `json:"description_bytes"`
	SchemaBytes      int            `json:"schema_bytes"`
	Fingerprint      string         `json:"fingerprint"`
}

type HarnessSkillSnapshot struct {
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	Checksum         string   `json:"checksum"`
	InstructionBytes int      `json:"instruction_bytes"`
	InstructionHash  string   `json:"instruction_fingerprint,omitempty"`
	RequiredTools    []string `json:"required_tools,omitempty"`
	OptionalTools    []string `json:"optional_tools,omitempty"`
	Executable       bool     `json:"executable"`
}

type HarnessPluginSnapshot struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Checksum   string   `json:"checksum"`
	Tools      []string `json:"tools,omitempty"`
	Executable bool     `json:"executable"`
}

type HarnessExtensionSnapshot struct {
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Started     bool   `json:"started"`
	Fingerprint string `json:"fingerprint"`
}

type HarnessContextSnapshot struct {
	WindowTokens        int `json:"window_tokens,omitempty"`
	ReservedTokens      int `json:"reserved_tokens"`
	OutputReserveTokens int `json:"output_reserve_tokens"`
	PromptTokens        int `json:"prompt_tokens"`
	ToolSchemaTokens    int `json:"tool_schema_tokens"`
	InitialTokens       int `json:"initial_tokens"`
}

type HarnessAgentSnapshot struct {
	MaxToolSteps               int  `json:"max_tool_steps"`
	UnlimitedToolSteps         bool `json:"unlimited_tool_steps,omitempty"`
	ToolLandingSteps           int  `json:"tool_landing_steps,omitempty"`
	TurnTimeoutSeconds         int  `json:"turn_timeout_seconds,omitempty"`
	DeadlineWarningSeconds     int  `json:"deadline_warning_seconds,omitempty"`
	FinalizationTimeoutSeconds int  `json:"finalization_timeout_seconds,omitempty"`
	FinalizationMaxTokens      int  `json:"finalization_max_tokens,omitempty"`
	NoProgressLimit            int  `json:"no_progress_limit,omitempty"`
	MaxAutoContinuations       int  `json:"max_auto_continuations,omitempty"`
	AutoCompactBetweenSegments bool `json:"auto_compact_between_segments,omitempty"`
}

// HarnessSnapshot is a content-free description of one effective Bee
// composition. Fingerprint excludes CapturedAt and lint prose; it is stable
// for the same prompt bytes, provider, tools, packages, policy, and scopes.
type HarnessSnapshot struct {
	SchemaVersion          int                        `json:"schema_version"`
	CapturedAt             time.Time                  `json:"captured_at"`
	Fingerprint            string                     `json:"fingerprint"`
	CachePrefixFingerprint string                     `json:"cache_prefix_fingerprint"`
	Provider               HarnessProviderSnapshot    `json:"provider"`
	Prompt                 HarnessPromptSnapshot      `json:"prompt"`
	Context                HarnessContextSnapshot     `json:"context"`
	Agent                  HarnessAgentSnapshot       `json:"agent"`
	Policy                 AssistantPolicyMode        `json:"policy"`
	ScopeRoots             []string                   `json:"scope_roots,omitempty"`
	WriteRoots             []string                   `json:"write_roots,omitempty"`
	FullMachine            bool                       `json:"full_machine,omitempty"`
	RequirePlan            bool                       `json:"require_plan"`
	Tools                  []HarnessToolSnapshot      `json:"tools,omitempty"`
	Skills                 []HarnessSkillSnapshot     `json:"skills,omitempty"`
	Plugins                []HarnessPluginSnapshot    `json:"plugins,omitempty"`
	Extensions             []HarnessExtensionSnapshot `json:"extensions,omitempty"`
	Lint                   HarnessLintReport          `json:"lint"`
	DynamicExcluded        []string                   `json:"dynamic_excluded,omitempty"`
}

type harnessSnapshotIdentity struct {
	SchemaVersion int
	Provider      ProviderConfig
	Profile       string
	Role          ProviderRole
	Generation    GenerationConfig
	Agent         struct {
		PromptFingerprint string
		MaxToolSteps      int
		ToolLandingSteps  int
		TurnTimeout       int
		DeadlineWarning   int
		Finalization      int
		FinalizationMax   int
		NoProgress        int
		AutoContinuations int
		AutoCompact       bool
	}
	Context         ContextKernelConfig
	Policy          AssistantPolicyMode
	ScopeRoots      []string
	WriteRoots      []string
	FullMachine     bool
	RequirePlan     bool
	Tools           []ToolDefinition
	Skills          []HarnessSkillSnapshot
	Plugins         []HarnessPluginSnapshot
	Extensions      []ExtensionConfig
	ExtensionStates []HarnessExtensionSnapshot
}

// BuildHarnessSnapshot inspects an already composed Application. It never
// starts extensions or invokes a provider.
func BuildHarnessSnapshot(ctx context.Context, app *Application) (HarnessSnapshot, error) {
	var snapshot HarnessSnapshot
	if app == nil {
		return snapshot, errors.New("application is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	prompt, err := effectiveSystemPrompt(app.Config.Agent, app.ProviderRole != ProviderRoleChild)
	if err != nil {
		return snapshot, fmt.Errorf("resolve effective harness prompt: %w", err)
	}
	definitions, duplicateTools := effectiveApplicationToolDefinitions(app)
	provider := harnessProviderSnapshot(app)
	promptMessages := []string{prompt}
	promptComponents := harnessPromptComponents(app.Config.Agent, app.ProviderRole != ProviderRoleChild)
	if app.Dashboard != nil && app.ProviderRole != ProviderRoleChild {
		// The dashboard is composed per session activation (like mood), so it is
		// reported as a component but excluded from the static prompt bytes.
		dashboard := app.Dashboard.Snapshot()
		component := HarnessPromptComponent{Kind: "memory_dashboard", Source: app.Dashboard.Path(), Present: dashboard != ""}
		if component.Present {
			component.Bytes = len(dashboard)
			component.ApproxTokens = approximateTextTokens(dashboard)
			component.Fingerprint = fingerprintBytes([]byte(dashboard))
		}
		promptComponents = append(promptComponents, component)
	}
	if runtimeContext := app.runtimeContext(""); runtimeContext != "" && app.ProviderRole != ProviderRoleChild {
		promptComponents = append(promptComponents, HarnessPromptComponent{
			Kind: "runtime_facts", Source: "built-in", Present: true,
			Bytes: len(runtimeContext), ApproxTokens: approximateTextTokens(runtimeContext),
			Fingerprint: fingerprintBytes([]byte(runtimeContext)),
		})
	}
	if app.supportsPlanMode() && app.ProviderRole != ProviderRoleChild {
		promptMessages = append([]string{planProtocolSystemPrompt}, promptMessages...)
		promptComponents = append([]HarnessPromptComponent{{
			Kind: "plan_protocol", Source: "built-in", Present: true,
			Bytes: len(planProtocolSystemPrompt), ApproxTokens: approximateTextTokens(planProtocolSystemPrompt),
			Fingerprint: fingerprintBytes([]byte(planProtocolSystemPrompt)),
		}}, promptComponents...)
	}
	promptBytes := 0
	promptTokens := 0
	for _, message := range promptMessages {
		promptBytes += len(message)
		promptTokens += approximateTextTokens(message)
	}
	promptFingerprint := fingerprintValue("bee-harness-prompt-v2", promptMessages)
	snapshot = HarnessSnapshot{
		SchemaVersion: HarnessSnapshotSchemaVersion,
		CapturedAt:    time.Now().UTC(),
		Provider:      provider,
		Prompt: HarnessPromptSnapshot{
			Mode:         harnessPromptMode(app.Config.Agent, prompt),
			Bytes:        promptBytes,
			ApproxTokens: promptTokens,
			Fingerprint:  promptFingerprint,
			Components:   promptComponents,
		},
		Policy:          app.Capabilities.Policy,
		ScopeRoots:      uniqueOrderedStrings(app.Capabilities.ScopeRoots),
		WriteRoots:      uniqueOrderedStrings(app.Capabilities.WriteRoots),
		FullMachine:     app.Capabilities.FullMachine,
		RequirePlan:     app.RequirePlan,
		DynamicExcluded: []string{"session history", "project instructions", "retrieved memory", "attachments", "per-turn remote grants"},
	}
	if len(snapshot.ScopeRoots) == 0 && app.Capabilities.ScopeRoot != "" {
		snapshot.ScopeRoots = []string{app.Capabilities.ScopeRoot}
	}
	if len(snapshot.WriteRoots) == 0 && app.Capabilities.WriteRoot != "" {
		snapshot.WriteRoots = []string{app.Capabilities.WriteRoot}
	}
	for _, definition := range definitions {
		snapshot.Tools = append(snapshot.Tools, HarnessToolSnapshot{
			Name: definition.Name, Risk: definition.Risk, Mutability: definition.Mutability,
			PlanExempt: definition.PlanExempt, DescriptionBytes: len(definition.Description),
			SchemaBytes: len(definition.Parameters), Fingerprint: fingerprintValue("bee-tool-v1", definition),
		})
	}
	if app.SkillRegistry != nil {
		packages, listErr := app.SkillRegistry.Enabled(ctx)
		if listErr != nil {
			return snapshot, fmt.Errorf("list enabled harness skills: %w", listErr)
		}
		for _, pkg := range packages {
			loaded, instructions, loadErr := app.SkillRegistry.LoadEnabled(ctx, pkg.Manifest.Name)
			if loadErr != nil {
				return snapshot, fmt.Errorf("load enabled harness skill %q: %w", pkg.Manifest.Name, loadErr)
			}
			snapshot.Skills = append(snapshot.Skills, HarnessSkillSnapshot{
				Name: loaded.Manifest.Name, Version: loaded.Manifest.Version, Checksum: loaded.Checksum,
				InstructionBytes: len(instructions), InstructionHash: fingerprintBytes([]byte(instructions)),
				RequiredTools: normalizedStrings(loaded.Manifest.RequiredTools), OptionalTools: normalizedStrings(loaded.Manifest.OptionalTools),
				Executable: loaded.Manifest.Extension != nil,
			})
		}
	}
	if app.PluginRegistry != nil {
		packages, listErr := app.PluginRegistry.Enabled(ctx)
		if listErr != nil {
			return snapshot, fmt.Errorf("list enabled harness plugins: %w", listErr)
		}
		for _, pkg := range packages {
			var tools []string
			if pkg.Manifest.Extension != nil {
				tools = normalizedStrings(pkg.Manifest.Extension.Tools)
			}
			snapshot.Plugins = append(snapshot.Plugins, HarnessPluginSnapshot{Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Checksum: pkg.Checksum, Tools: tools, Executable: pkg.Manifest.Extension != nil})
		}
	}
	started := make(map[string]bool, len(app.ExtensionHosts))
	for _, host := range app.ExtensionHosts {
		if host != nil {
			started[host.Config().Name] = true
		}
	}
	for _, extension := range app.Config.Extensions {
		snapshot.Extensions = append(snapshot.Extensions, HarnessExtensionSnapshot{Name: extension.Name, Enabled: extension.Enabled, Started: started[extension.Name], Fingerprint: fingerprintValue("bee-extension-v1", extension)})
	}
	sort.Slice(snapshot.Skills, func(i, j int) bool { return snapshot.Skills[i].Name < snapshot.Skills[j].Name })
	sort.Slice(snapshot.Plugins, func(i, j int) bool { return snapshot.Plugins[i].Name < snapshot.Plugins[j].Name })
	sort.Slice(snapshot.Extensions, func(i, j int) bool { return snapshot.Extensions[i].Name < snapshot.Extensions[j].Name })
	snapshot.Context = HarnessContextSnapshot{
		WindowTokens: app.Config.Provider.ContextWindowTokens, ReservedTokens: app.Config.Context.ReserveTokens,
		OutputReserveTokens: effectiveHarnessOutputReserve(app.Config), PromptTokens: snapshot.Prompt.ApproxTokens,
		ToolSchemaTokens: estimateToolDefinitions(definitions),
	}
	snapshot.Agent = HarnessAgentSnapshot{
		MaxToolSteps:       configuredMaxToolSteps(app.Config, 0),
		UnlimitedToolSteps: app.Config.Agent.maxToolStepsExplicit && app.Config.Agent.MaxToolSteps == 0,
		ToolLandingSteps:   app.Config.Agent.ToolLandingSteps, TurnTimeoutSeconds: app.Config.Agent.TurnTimeoutSeconds,
		DeadlineWarningSeconds: app.Config.Agent.DeadlineWarningSeconds, FinalizationTimeoutSeconds: app.Config.Agent.FinalizationTimeoutSeconds,
		FinalizationMaxTokens: app.Config.Agent.FinalizationMaxTokens, NoProgressLimit: app.Config.Agent.NoProgressLimit,
		MaxAutoContinuations: app.Config.Agent.MaxAutoContinuations, AutoCompactBetweenSegments: app.Config.Agent.AutoCompactBetweenSegments,
	}
	snapshot.Context.InitialTokens = snapshot.Context.PromptTokens + snapshot.Context.ToolSchemaTokens
	snapshot.Lint = lintHarnessSnapshot(snapshot, duplicateTools)
	identity := harnessSnapshotIdentity{
		SchemaVersion: HarnessSnapshotSchemaVersion, Provider: sanitizedProviderConfig(app.Config.Provider), Profile: app.ProviderProfile,
		Role: app.ProviderRole, Generation: app.Config.Generation, Context: app.Config.Context, Policy: snapshot.Policy,
		ScopeRoots: snapshot.ScopeRoots, WriteRoots: snapshot.WriteRoots, FullMachine: snapshot.FullMachine,
		RequirePlan: snapshot.RequirePlan, Tools: definitions, Skills: snapshot.Skills, Plugins: snapshot.Plugins,
		Extensions: append([]ExtensionConfig(nil), app.Config.Extensions...), ExtensionStates: append([]HarnessExtensionSnapshot(nil), snapshot.Extensions...),
	}
	identity.Agent.PromptFingerprint = snapshot.Prompt.Fingerprint
	identity.Agent.MaxToolSteps = configuredMaxToolSteps(app.Config, 0)
	identity.Agent.ToolLandingSteps = app.Config.Agent.ToolLandingSteps
	identity.Agent.TurnTimeout = app.Config.Agent.TurnTimeoutSeconds
	identity.Agent.DeadlineWarning = app.Config.Agent.DeadlineWarningSeconds
	identity.Agent.Finalization = app.Config.Agent.FinalizationTimeoutSeconds
	identity.Agent.FinalizationMax = app.Config.Agent.FinalizationMaxTokens
	identity.Agent.NoProgress = app.Config.Agent.NoProgressLimit
	identity.Agent.AutoContinuations = app.Config.Agent.MaxAutoContinuations
	identity.Agent.AutoCompact = app.Config.Agent.AutoCompactBetweenSegments
	sort.Slice(identity.Extensions, func(i, j int) bool { return identity.Extensions[i].Name < identity.Extensions[j].Name })
	snapshot.Fingerprint = fingerprintValue("bee-harness-snapshot-v1", identity)
	snapshot.CachePrefixFingerprint = fingerprintValue("bee-cache-prefix-v2", struct {
		ProviderType       string
		Model              string
		ChatTemplateKwargs map[string]any
		PromptFingerprint  string
		Tools              []openAITool
	}{
		ProviderType:       app.Config.Provider.Type,
		Model:              app.Config.Provider.Model,
		ChatTemplateKwargs: chatTemplateKwargs(Request{ReasoningEffort: app.Config.Generation.ReasoningEffort}),
		PromptFingerprint:  promptFingerprint,
		Tools:              requestTools(definitions),
	})
	return snapshot, nil
}

func effectiveApplicationToolDefinitions(app *Application) ([]ToolDefinition, []string) {
	var definitions []ToolDefinition
	if app.Executor != nil && app.Executor.Registry != nil {
		definitions = append(definitions, app.Executor.Registry.Definitions()...)
	}
	web := app.ForegroundWebResearch
	if web == nil {
		web = app.WebResearch
	}
	if web != nil && web.Registry != nil {
		definitions = append(definitions, web.Registry.Definitions()...)
	}
	if app.supportsPlanMode() && app.ProviderRole != ProviderRoleChild {
		definitions = append(definitions, planToolDefinition(), planStatusToolDefinition(), planUpdateToolDefinition())
	}
	if app.Executor != nil && app.ProviderRole != ProviderRoleChild {
		definitions = append(definitions, contextStatusToolDefinition())
	}
	return normalizeToolDefinitions(definitions)
}

func normalizeToolDefinitions(definitions []ToolDefinition) ([]ToolDefinition, []string) {
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	seen := make(map[string]struct{}, len(definitions))
	duplicates := make([]string, 0)
	out := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if _, exists := seen[definition.Name]; exists {
			duplicates = append(duplicates, definition.Name)
			continue
		}
		seen[definition.Name] = struct{}{}
		out = append(out, definition)
	}
	return out, normalizedStrings(duplicates)
}

func harnessProviderSnapshot(app *Application) HarnessProviderSnapshot {
	config := sanitizedProviderConfig(app.Config.Provider)
	host := ""
	if parsed, err := url.Parse(config.BaseURL); err == nil {
		host = parsed.Hostname()
	}
	identity := struct {
		Profile string
		Role    ProviderRole
		Config  ProviderConfig
	}{app.ProviderProfile, app.ProviderRole, config}
	return HarnessProviderSnapshot{Profile: app.ProviderProfile, Role: app.ProviderRole, Type: config.Type, Model: config.Model, EndpointHost: host, ContextWindowTokens: config.ContextWindowTokens, Fingerprint: fingerprintValue("bee-provider-v1", identity)}
}

func sanitizedProviderConfig(config ProviderConfig) ProviderConfig {
	config.APIKeyFile = ""
	return config
}

func harnessPromptMode(config AgentConfig, prompt string) string {
	if prompt == "" {
		return "disabled"
	}
	if config.systemPromptExplicit || explicitInlineSystemPrompt(config.SystemPrompt) {
		return "inline"
	}
	return "composed"
}

func harnessPromptComponents(config AgentConfig, includeWorkspace bool) []HarnessPromptComponent {
	components := make([]HarnessPromptComponent, 0, 4)
	add := func(kind, source, value string, present bool) {
		component := HarnessPromptComponent{Kind: kind, Source: source, Present: present}
		if present {
			component.Bytes = len(value)
			component.ApproxTokens = approximateTextTokens(value)
			component.Fingerprint = fingerprintBytes([]byte(value))
		}
		components = append(components, component)
	}
	if config.systemPromptExplicit || explicitInlineSystemPrompt(config.SystemPrompt) {
		add("inline_system_prompt", "config", config.SystemPrompt, config.SystemPrompt != "")
	} else {
		add("built_in_system_prompt", "built-in", DefaultSystemPrompt, true)
		path := strings.TrimSpace(config.SystemPromptFile)
		if path == "" {
			path = DefaultSystemPromptFile
		}
		if resolved, err := resolveSystemPromptFile(path); err == nil {
			contents, present, loadErr := loadSystemPromptFile(resolved)
			if loadErr == nil {
				add("system_prompt_file", resolved, strings.TrimSpace(contents), present && strings.TrimSpace(contents) != "")
			}
		}
	}
	if includeWorkspace {
		if resolved, err := resolveSystemPromptFile(DefaultWorkspaceContextFile); err == nil {
			contents, present, loadErr := loadPrivatePromptFile(resolved, "Bee workspace context file", MaxWorkspaceContextFileBytes)
			if loadErr == nil {
				contents = strings.TrimSpace(contents)
				add("workspace_context", resolved, contents, present && contents != "")
			}
		}
	}
	return components
}

func lintHarnessSnapshot(snapshot HarnessSnapshot, duplicateTools []string) HarnessLintReport {
	issues := make([]HarnessLintIssue, 0)
	add := func(severity HarnessLintSeverity, code, component, detail string) {
		issues = append(issues, HarnessLintIssue{Severity: severity, Code: code, Component: component, Detail: detail})
	}
	if snapshot.Prompt.Bytes == 0 {
		add(HarnessLintWarning, "empty_system_prompt", "prompt", "the effective system prompt is empty")
	}
	if snapshot.Prompt.Bytes > HarnessPromptWarningBytes {
		add(HarnessLintWarning, "large_always_on_prompt", "prompt", fmt.Sprintf("effective always-on prompt is %d bytes", snapshot.Prompt.Bytes))
	}
	if snapshot.Context.WindowTokens > 0 {
		available := snapshot.Context.WindowTokens - snapshot.Context.ReservedTokens - snapshot.Context.OutputReserveTokens
		if available < 0 || snapshot.Context.InitialTokens > available {
			add(HarnessLintError, "initial_context_exceeds_budget", "context", fmt.Sprintf("initial prompt and tool schemas use about %d tokens; %d remain after reserves", snapshot.Context.InitialTokens, maxInt(available, 0)))
		} else if snapshot.Context.InitialTokens*4 > snapshot.Context.WindowTokens {
			add(HarnessLintWarning, "high_initial_context_pressure", "context", fmt.Sprintf("initial prompt and tool schemas use about %d of %d context tokens", snapshot.Context.InitialTokens, snapshot.Context.WindowTokens))
		}
	}
	for _, name := range duplicateTools {
		add(HarnessLintError, "duplicate_tool", "tool:"+name, "the effective composition exposes the same tool name more than once")
	}
	tools := make(map[string]struct{}, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		tools[tool.Name] = struct{}{}
	}
	for _, skill := range snapshot.Skills {
		if skill.Checksum == "" {
			add(HarnessLintError, "missing_skill_checksum", "skill:"+skill.Name, "enabled skill has no deterministic package checksum")
		}
		for _, required := range skill.RequiredTools {
			if _, ok := tools[required]; !ok {
				add(HarnessLintError, "missing_required_tool", "skill:"+skill.Name, fmt.Sprintf("required tool %q is not in the effective composition", required))
			}
		}
	}
	for _, plugin := range snapshot.Plugins {
		if plugin.Checksum == "" {
			add(HarnessLintError, "missing_plugin_checksum", "plugin:"+plugin.Name, "enabled plugin has no deterministic package checksum")
		}
		for _, declared := range plugin.Tools {
			if _, ok := tools[declared]; !ok {
				add(HarnessLintError, "missing_plugin_tool", "plugin:"+plugin.Name, fmt.Sprintf("declared tool %q is not in the effective composition", declared))
			}
		}
	}
	for _, extension := range snapshot.Extensions {
		if extension.Enabled && !extension.Started {
			add(HarnessLintWarning, "extension_not_started", "extension:"+extension.Name, "enabled extension is not part of the active tool composition")
		}
	}
	if snapshot.Agent.UnlimitedToolSteps {
		add(HarnessLintWarning, "unlimited_tool_steps", "agent", "max_tool_steps was explicitly set to zero")
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity == HarnessLintError
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Component < issues[j].Component
	})
	report := HarnessLintReport{Status: "ok", Issues: issues}
	for _, issue := range issues {
		if issue.Severity == HarnessLintError {
			report.Errors++
		} else {
			report.Warnings++
		}
	}
	if report.Errors > 0 {
		report.Status = "fail"
	} else if report.Warnings > 0 {
		report.Status = "warn"
	}
	return report
}

func effectiveHarnessOutputReserve(config Config) int {
	if config.Generation.MainMaxTokens != nil && config.ProviderRole == ProviderRoleMain {
		if *config.Generation.MainMaxTokens > 0 {
			return *config.Generation.MainMaxTokens
		}
		if *config.Generation.MainMaxTokens == 0 {
			return config.Context.OutputReserveTokens
		}
	}
	if config.Generation.MaxTokens > 0 {
		return config.Generation.MaxTokens
	}
	return config.Context.OutputReserveTokens
}

func approximateTextTokens(value string) int { return (len(value) + 3) / 4 }

func normalizedStrings(values []string) []string {
	out := uniqueOrderedStrings(values)
	sort.Strings(out)
	return out
}

func uniqueOrderedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func fingerprintBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fingerprintValue(domain string, value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Human returns a compact operator-facing snapshot. It never includes prompt,
// skill, tool-description, or credential contents.
func (snapshot HarnessSnapshot) Human() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bee effective harness %s\n", snapshot.Fingerprint)
	fmt.Fprintf(&b, "cache prefix: %s\n", snapshot.CachePrefixFingerprint)
	fmt.Fprintf(&b, "provider: %s/%s profile=%s role=%s\n", snapshot.Provider.Type, snapshot.Provider.Model, snapshot.Provider.Profile, snapshot.Provider.Role)
	fmt.Fprintf(&b, "always-on prompt: %d bytes (~%d tokens); tool schemas: ~%d tokens\n", snapshot.Prompt.Bytes, snapshot.Prompt.ApproxTokens, snapshot.Context.ToolSchemaTokens)
	fmt.Fprintf(&b, "components: %d tools, %d skills, %d plugins, %d extensions\n", len(snapshot.Tools), len(snapshot.Skills), len(snapshot.Plugins), len(snapshot.Extensions))
	fmt.Fprintf(&b, "lint: %s (%d warnings, %d errors)\n", snapshot.Lint.Status, snapshot.Lint.Warnings, snapshot.Lint.Errors)
	for _, issue := range snapshot.Lint.Issues {
		component := ""
		if issue.Component != "" {
			component = " [" + issue.Component + "]"
		}
		fmt.Fprintf(&b, "- %s %s%s: %s\n", strings.ToUpper(string(issue.Severity)), issue.Code, component, issue.Detail)
	}
	return b.String()
}
