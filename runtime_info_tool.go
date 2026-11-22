package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RuntimeInfoTool exposes bounded, read-only runtime metadata. It deliberately
// does not read credential files or grant any authority; exec and the legacy
// run_command path remain separate policy/composition-controlled capabilities.
type RuntimeInfoTool struct {
	Application *Application
	Scope       ExecutionScope
}

type SpecialistVisibility struct {
	Enabled           bool   `json:"enabled"`
	Profile           string `json:"profile,omitempty"`
	EvidenceEnabled   bool   `json:"evidence_enabled"`
	EvidenceMaxInput  int    `json:"evidence_max_input_bytes,omitempty"`
	EvidenceMaxOutput int    `json:"evidence_max_output_bytes,omitempty"`
	EvidenceMaxItems  int    `json:"evidence_max_items,omitempty"`
	ClaimVerification bool   `json:"claim_verification"`
	MemoryReranker    bool   `json:"memory_reranker"`
}

type SemanticMemoryVisibility struct {
	Enabled bool               `json:"enabled"`
	Health  SemanticHealthCode `json:"health,omitempty"`
}

type RuntimeInfo struct {
	Hostname         string                    `json:"hostname,omitempty"`
	OS               string                    `json:"os,omitempty"`
	Arch             string                    `json:"arch,omitempty"`
	Kernel           string                    `json:"kernel,omitempty"`
	Runtime          string                    `json:"runtime,omitempty"`
	PID              int                       `json:"pid"`
	WorkingDirectory string                    `json:"working_directory,omitempty"`
	ScopeRoot        string                    `json:"scope_root,omitempty"`
	FullMachine      bool                      `json:"full_machine"`
	AssistantPolicy  AssistantPolicyMode       `json:"assistant_policy,omitempty"`
	Provider         string                    `json:"provider,omitempty"`
	Profile          string                    `json:"profile,omitempty"`
	ProviderRole     ProviderRole              `json:"provider_role,omitempty"`
	Model            string                    `json:"model,omitempty"`
	ActiveToolNames  []string                  `json:"active_tool_names,omitempty"`
	ChildBroker      *LocalChildBrokerSnapshot `json:"child_broker,omitempty"`
	Specialist       SpecialistVisibility      `json:"specialist,omitempty"`
	SemanticMemory   SemanticMemoryVisibility  `json:"semantic_memory"`
	Environment      map[string]string         `json:"environment,omitempty"`
}

func (tool RuntimeInfoTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "runtime_info",
		Description: "Read-only, non-secret host/runtime/application introspection. Reports bounded metadata and a filtered environment view; never reads credential files or exposes keys, tokens, passwords, or private credential contents.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}
}

func (tool RuntimeInfoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var input struct{}
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("runtime_info arguments: %w", err)
	}
	info := tool.snapshot(ctx)
	encoded, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("encode runtime_info: %w", err)
	}
	return string(encoded), nil
}

func (tool RuntimeInfoTool) snapshot(ctx context.Context) RuntimeInfo {
	info := RuntimeInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Runtime: runtime.Version(), PID: os.Getpid(), ScopeRoot: filepath.Clean(tool.Scope.Root), Environment: safeRuntimeEnvironment()}
	info.Kernel = runtimeKernelVersion()
	info.Hostname, _ = os.Hostname()
	info.WorkingDirectory, _ = os.Getwd()
	if tool.Application != nil {
		app := tool.Application
		info.Provider = app.Config.Provider.Type
		info.Profile = app.ProviderProfile
		info.ProviderRole = app.ProviderRole
		info.Model = app.Config.Provider.Model
		info.FullMachine = app.Capabilities.FullMachine
		info.AssistantPolicy = app.Capabilities.Policy
		if app.ChildBroker != nil {
			snapshot := app.ChildBroker.Snapshot()
			info.ChildBroker = &snapshot
		}
		if app.Specialist != nil {
			config := app.Specialist.Config()
			localChild := app.Specialist.isConfiguredLocalChild()
			info.Specialist = SpecialistVisibility{Enabled: config.Enabled, Profile: app.Specialist.Profile(), EvidenceEnabled: config.EvidenceEnabled && localChild, EvidenceMaxInput: config.EvidenceMaxInputBytes, EvidenceMaxOutput: config.EvidenceMaxOutputBytes, EvidenceMaxItems: config.EvidenceMaxItems, ClaimVerification: app.ClaimVerification != nil && app.ClaimVerification.specialist != nil && app.ClaimVerification.specialist.isConfiguredLocalChild(), MemoryReranker: app.MemoryReranker != nil}
		}
		health := app.MemoryRetrieval.SemanticHealth
		if runtimeHealth := semanticRuntimeHealth(ctx, app.MemoryRetrieval); runtimeHealth != "" {
			health = runtimeHealth
		}
		info.SemanticMemory = SemanticMemoryVisibility{Enabled: app.MemoryRetrieval.SemanticEnabled, Health: health}
		if app.Executor != nil && app.Executor.Registry != nil {
			for _, definition := range app.Executor.Registry.Definitions() {
				info.ActiveToolNames = append(info.ActiveToolNames, definition.Name)
			}
		}
		webExecutor := app.ForegroundWebResearch
		if webExecutor == nil {
			webExecutor = app.WebResearch
		}
		if webExecutor != nil && webExecutor.Registry != nil {
			for _, definition := range webExecutor.Registry.Definitions() {
				info.ActiveToolNames = append(info.ActiveToolNames, definition.Name)
			}
		}
	}
	if info.ScopeRoot == "." || info.ScopeRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			info.ScopeRoot = cwd
		}
	}
	if len(info.ActiveToolNames) == 0 && tool.Application == nil {
		info.ActiveToolNames = nil
	}
	return info
}

func runtimeKernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func safeRuntimeEnvironment() map[string]string {
	result := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !safeRuntimeEnvironmentKey(key) {
			continue
		}
		result[key] = value
	}
	return result
}

func safeRuntimeEnvironmentKey(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" || secretEnvironmentKey(upper) {
		return false
	}
	for _, marker := range []string{"AUTH", "PRIVATE", "CREDENTIAL", "COOKIE", "SESSION", "PASS", "SIGNATURE"} {
		if strings.Contains(upper, marker) {
			return false
		}
	}
	return true
}

// RuntimeInfoTool is intentionally read-only and therefore suitable for the
// general assistant registry. Keep this registration provider-neutral and
// parent-owned; extensions cannot widen it or replace its filtering.
func registerRuntimeInfoTool(registry *Registry, scope ExecutionScope) (*RuntimeInfoTool, error) {
	if registry == nil {
		return nil, errors.New("runtime_info registry is nil")
	}
	tool := &RuntimeInfoTool{Scope: scope}
	if err := registry.Register(tool); err != nil {
		return nil, err
	}
	return tool, nil
}
