package bee

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	SkillCreateToolName  = "skill_create"
	SkillTestToolName    = "skill_test"
	SkillEnableToolName  = "skill_enable"
	PluginCreateToolName = "plugin_create"
	PluginTestToolName   = "plugin_test"
	PluginEnableToolName = "plugin_enable"
)

type PackageBuildResult struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Status  string `json:"status"`
}

type PackageEnableResult struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	Checksum        string `json:"checksum"`
	Enabled         bool   `json:"enabled"`
	RestartRequired bool   `json:"restart_required"`
}

type SkillCreateTool struct{ Scope ExecutionScope }

func (tool SkillCreateTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        SkillCreateToolName,
		Description: "Create a new inert Bee skill source package under .bee-builds in the scoped workspace. This does not install or enable it.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"version":{"type":"string"},"description":{"type":"string"},"instructions":{"type":"string"},"required_tools":{"type":"array","items":{"type":"string"}},"optional_tools":{"type":"array","items":{"type":"string"}},"extension":{"type":"object"}},"required":["name","version","description","instructions"],"additionalProperties":false}`),
	}
}

func (tool SkillCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Name          string          `json:"name"`
		Version       string          `json:"version"`
		Description   string          `json:"description"`
		Instructions  string          `json:"instructions"`
		RequiredTools []string        `json:"required_tools"`
		OptionalTools []string        `json:"optional_tools"`
		Extension     *SkillExtension `json:"extension"`
	}
	if err := decodeSkillToolArgs(args, &input); err != nil {
		return "", err
	}
	manifest := SkillManifest{SchemaVersion: SkillSchemaVersion, Name: input.Name, Version: input.Version, Description: input.Description, Instructions: DefaultSkillInstructions, RequiredTools: append([]string(nil), input.RequiredTools...), OptionalTools: append([]string(nil), input.OptionalTools...), Extension: input.Extension}
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	if err := validateBuilderManifestSize(manifest); err != nil {
		return "", err
	}
	if strings.TrimSpace(input.Instructions) == "" || len(input.Instructions) > SkillMaxInstructionsBytes {
		return "", fmt.Errorf("skill instructions must contain 1-%d bytes", SkillMaxInstructionsBytes)
	}
	for _, value := range input.Instructions {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return "", errors.New("skill instructions contain unsupported control characters")
		}
	}
	destination, err := builderPackagePath(tool.Scope, "skills", manifest.Name, manifest.Version)
	if err != nil {
		return "", err
	}
	err = createBuilderPackage(ctx, destination, func(stage string) error {
		if err := atomicWritePrivateJSON(filepath.Join(stage, SkillManifestFilename), manifest); err != nil {
			return err
		}
		if err := atomicWriteFile(filepath.Join(stage, DefaultSkillInstructions), []byte(input.Instructions), 0o600); err != nil {
			return err
		}
		if manifest.Extension != nil {
			return writeExtensionPlaceholder(stage, manifest.Extension.Command, "skill")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	result := PackageBuildResult{Kind: "skill", Name: manifest.Name, Version: manifest.Version, Path: destination, Status: "created; inert until tested and human-approved"}
	data, err := json.Marshal(result)
	return string(data), err
}

type PluginCreateTool struct{ Scope ExecutionScope }

func (tool PluginCreateTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        PluginCreateToolName,
		Description: "Create a new inert Bee external-process plugin source package under .bee-builds in the scoped workspace. The generated executable is a nonfunctional placeholder that must be implemented before testing.",
		Risk:        RiskHigh,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"version":{"type":"string"},"description":{"type":"string"},"extension":{"type":"object"}},"required":["name","version","description","extension"],"additionalProperties":false}`),
	}
}

func (tool PluginCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Name        string         `json:"name"`
		Version     string         `json:"version"`
		Description string         `json:"description"`
		Extension   SkillExtension `json:"extension"`
	}
	if err := decodeSkillToolArgs(args, &input); err != nil {
		return "", err
	}
	manifest := PluginManifest{SchemaVersion: PluginSchemaVersion, Name: input.Name, Version: input.Version, Description: input.Description, Extension: &input.Extension}
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	if err := validateBuilderManifestSize(manifest); err != nil {
		return "", err
	}
	destination, err := builderPackagePath(tool.Scope, "plugins", manifest.Name, manifest.Version)
	if err != nil {
		return "", err
	}
	err = createBuilderPackage(ctx, destination, func(stage string) error {
		if err := atomicWritePrivateJSON(filepath.Join(stage, PluginManifestFilename), manifest); err != nil {
			return err
		}
		return writeExtensionPlaceholder(stage, manifest.Extension.Command, "plugin")
	})
	if err != nil {
		return "", err
	}
	result := PackageBuildResult{Kind: "plugin", Name: manifest.Name, Version: manifest.Version, Path: destination, Status: "created with placeholder; inert until implemented, tested, and human-approved"}
	data, err := json.Marshal(result)
	return string(data), err
}

func validateBuilderManifestSize(manifest any) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(data) > SkillMaxManifestBytes {
		return fmt.Errorf("package manifest exceeds %d bytes", SkillMaxManifestBytes)
	}
	return nil
}

func builderPackagePath(scope ExecutionScope, kind, name, version string) (string, error) {
	root, err := scope.rootPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".bee-builds", kind, name, version), nil
}

func createBuilderPackage(ctx context.Context, destination string, populate func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("build package already exists at %s; use a new version", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".build-")
	if err != nil {
		return err
	}
	live := true
	defer func() {
		if live {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	if err := populate(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	live = false
	return nil
}

func writeExtensionPlaceholder(root, command, kind string) error {
	relative, err := validateSkillRelativePath(kind+" extension command", command)
	if err != nil {
		return err
	}
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content := "#!/bin/sh\nprintf '%s\\n' 'Bee " + kind + " placeholder: implement bee.extension.v1 before testing' >&2\nexit 1\n"
	return atomicWriteFile(path, []byte(content), 0o700)
}

type SkillTestTool struct{ Scope ExecutionScope }

func (tool SkillTestTool) Definition() ToolDefinition {
	return ToolDefinition{Name: SkillTestToolName, Description: "Validate a Bee skill source package and, when executable, start it long enough to verify its exact extension handshake. Model-authored code execution always requires explicit human approval. Returns the checksum required by skill_enable.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: sourceToolSchema()}
}

func (SkillTestTool) RequiresExplicitApproval() bool { return true }
func (SkillTestTool) RequiresManualApproval() bool   { return true }

func (tool SkillTestTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	source, err := decodeBuilderSource(tool.Scope, args)
	if err != nil {
		return "", err
	}
	report, err := TestSkillSource(ctx, source, false)
	if err != nil {
		return "", err
	}
	if report.Package.Manifest.Extension != nil {
		report, err = TestSkillSource(ctx, source, true)
		if err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(report)
	return string(data), err
}

type PluginTestTool struct{ Scope ExecutionScope }

func (tool PluginTestTool) Definition() ToolDefinition {
	return ToolDefinition{Name: PluginTestToolName, Description: "Validate a Bee plugin source package and start it long enough to verify its exact extension handshake. Model-authored code execution always requires explicit human approval. Returns the checksum required by plugin_enable.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: sourceToolSchema()}
}

func (PluginTestTool) RequiresExplicitApproval() bool { return true }
func (PluginTestTool) RequiresManualApproval() bool   { return true }

func (tool PluginTestTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	source, err := decodeBuilderSource(tool.Scope, args)
	if err != nil {
		return "", err
	}
	report, err := TestPluginSource(ctx, source, true)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(report)
	return string(data), err
}

type SkillEnableTool struct {
	Scope  ExecutionScope
	Skills *SkillRegistry
}

func (tool SkillEnableTool) Definition() ToolDefinition {
	return ToolDefinition{Name: SkillEnableToolName, Description: "Install and enable the exact tested Bee skill source revision. This action always requires explicit human approval and the checksum returned by skill_test.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: enableToolSchema()}
}

func (SkillEnableTool) RequiresExplicitApproval() bool { return true }
func (SkillEnableTool) RequiresManualApproval() bool   { return true }

func (tool SkillEnableTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	registryWasUnavailable := tool.Skills == nil
	skills := tool.Skills
	if skills == nil {
		var err error
		skills, err = NewDefaultSkillRegistry()
		if err != nil {
			return "", fmt.Errorf("initialize skill registry: %w", err)
		}
	}
	source, checksum, err := decodeBuilderEnable(tool.Scope, args)
	if err != nil {
		return "", err
	}
	report, err := TestSkillSource(ctx, source, false)
	if err != nil {
		return "", err
	}
	if report.Package.Manifest.Extension != nil {
		report, err = TestSkillSource(ctx, source, true)
		if err != nil {
			return "", err
		}
	}
	if report.Package.Checksum != checksum {
		return "", fmt.Errorf("skill checksum changed after review: got %s, want %s", report.Package.Checksum, checksum)
	}
	pkg, err := installOrReuseSkill(ctx, skills, report.Package, checksum)
	if err != nil {
		return "", err
	}
	pkg, err = skills.enable(ctx, pkg.Manifest.Name, pkg.Manifest.Version, checksum)
	if err != nil {
		return "", err
	}
	result := PackageEnableResult{Kind: "skill", Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Checksum: pkg.Checksum, Enabled: true, RestartRequired: registryWasUnavailable || pkg.Manifest.Extension != nil}
	data, err := json.Marshal(result)
	return string(data), err
}

type PluginEnableTool struct {
	Scope   ExecutionScope
	Plugins *PluginRegistry
}

func (tool PluginEnableTool) Definition() ToolDefinition {
	return ToolDefinition{Name: PluginEnableToolName, Description: "Install and enable the exact tested Bee plugin source revision. This action always requires explicit human approval and the checksum returned by plugin_test; its tools load on the next composition.", Risk: RiskCritical, Mutability: ToolMutabilityMutating, Parameters: enableToolSchema()}
}

func (PluginEnableTool) RequiresExplicitApproval() bool { return true }
func (PluginEnableTool) RequiresManualApproval() bool   { return true }

func (tool PluginEnableTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	plugins := tool.Plugins
	if plugins == nil {
		var err error
		plugins, err = NewDefaultPluginRegistry()
		if err != nil {
			return "", fmt.Errorf("initialize plugin registry: %w", err)
		}
	}
	source, checksum, err := decodeBuilderEnable(tool.Scope, args)
	if err != nil {
		return "", err
	}
	report, err := TestPluginSource(ctx, source, true)
	if err != nil {
		return "", err
	}
	if report.Package.Checksum != checksum {
		return "", fmt.Errorf("plugin checksum changed after review: got %s, want %s", report.Package.Checksum, checksum)
	}
	pkg, err := installOrReusePlugin(ctx, plugins, report.Package, checksum)
	if err != nil {
		return "", err
	}
	pkg, err = plugins.enable(ctx, pkg.Manifest.Name, pkg.Manifest.Version, checksum)
	if err != nil {
		return "", err
	}
	result := PackageEnableResult{Kind: "plugin", Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Checksum: pkg.Checksum, Enabled: true, RestartRequired: true}
	data, err := json.Marshal(result)
	return string(data), err
}

func (SkillTestTool) ExecutionTimeout() time.Duration    { return 2 * time.Minute }
func (PluginTestTool) ExecutionTimeout() time.Duration   { return 2 * time.Minute }
func (SkillEnableTool) ExecutionTimeout() time.Duration  { return 2 * time.Minute }
func (PluginEnableTool) ExecutionTimeout() time.Duration { return 2 * time.Minute }

func sourceToolSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"}},"required":["source"],"additionalProperties":false}`)
}

func enableToolSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"source":{"type":"string"},"expected_checksum":{"type":"string","pattern":"^[0-9a-f]{64}$"}},"required":["source","expected_checksum"],"additionalProperties":false}`)
}

func decodeBuilderSource(scope ExecutionScope, args json.RawMessage) (string, error) {
	var input struct {
		Source string `json:"source"`
	}
	if err := decodeSkillToolArgs(args, &input); err != nil {
		return "", err
	}
	return securePath(scope, input.Source, false)
}

func decodeBuilderEnable(scope ExecutionScope, args json.RawMessage) (string, string, error) {
	var input struct {
		Source           string `json:"source"`
		ExpectedChecksum string `json:"expected_checksum"`
	}
	if err := decodeSkillToolArgs(args, &input); err != nil {
		return "", "", err
	}
	if len(input.ExpectedChecksum) != 64 {
		return "", "", errors.New("expected_checksum must be a lowercase SHA-256 digest")
	}
	decoded, err := hex.DecodeString(input.ExpectedChecksum)
	if err != nil || len(decoded) != 32 || strings.ToLower(input.ExpectedChecksum) != input.ExpectedChecksum {
		return "", "", errors.New("expected_checksum must be a lowercase SHA-256 digest")
	}
	path, err := securePath(scope, input.Source, false)
	return path, input.ExpectedChecksum, err
}

func installOrReuseSkill(ctx context.Context, registry *SkillRegistry, source SkillPackage, expectedChecksum string) (SkillPackage, error) {
	packages, err := registry.List(ctx)
	if err != nil {
		return SkillPackage{}, err
	}
	for _, installed := range packages {
		if installed.Manifest.Name == source.Manifest.Name && installed.Manifest.Version == source.Manifest.Version {
			if installed.Checksum != expectedChecksum {
				return SkillPackage{}, fmt.Errorf("installed skill %s@%s has a different checksum", source.Manifest.Name, source.Manifest.Version)
			}
			return installed, nil
		}
	}
	installed, err := registry.Install(ctx, source.Path)
	if err != nil {
		return SkillPackage{}, err
	}
	if installed.Checksum != expectedChecksum {
		cleanupErr := registry.Remove(ctx, installed.Manifest.Name, installed.Manifest.Version)
		mismatch := fmt.Errorf("installed skill checksum %s does not match approved checksum %s", installed.Checksum, expectedChecksum)
		if cleanupErr != nil {
			return SkillPackage{}, errors.Join(mismatch, fmt.Errorf("remove mismatched skill: %w", cleanupErr))
		}
		return SkillPackage{}, mismatch
	}
	return installed, nil
}

func installOrReusePlugin(ctx context.Context, registry *PluginRegistry, source PluginPackage, expectedChecksum string) (PluginPackage, error) {
	packages, err := registry.List(ctx)
	if err != nil {
		return PluginPackage{}, err
	}
	for _, installed := range packages {
		if installed.Manifest.Name == source.Manifest.Name && installed.Manifest.Version == source.Manifest.Version {
			if installed.Checksum != expectedChecksum {
				return PluginPackage{}, fmt.Errorf("installed plugin %s@%s has a different checksum", source.Manifest.Name, source.Manifest.Version)
			}
			return installed, nil
		}
	}
	installed, err := registry.Install(ctx, source.Path)
	if err != nil {
		return PluginPackage{}, err
	}
	if installed.Checksum != expectedChecksum {
		cleanupErr := registry.Remove(ctx, installed.Manifest.Name, installed.Manifest.Version)
		mismatch := fmt.Errorf("installed plugin checksum %s does not match approved checksum %s", installed.Checksum, expectedChecksum)
		if cleanupErr != nil {
			return PluginPackage{}, errors.Join(mismatch, fmt.Errorf("remove mismatched plugin: %w", cleanupErr))
		}
		return PluginPackage{}, mismatch
	}
	return installed, nil
}
