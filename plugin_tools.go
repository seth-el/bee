package bee

import (
	"context"
	"encoding/json"
	"errors"
)

const PluginListToolName = "plugin_list"

type PluginCatalogEntry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Checksum    string   `json:"checksum"`
	Tools       []string `json:"tools"`
}

type PluginListResult struct {
	Plugins []PluginCatalogEntry `json:"plugins"`
}

type PluginListTool struct{ Plugins *PluginRegistry }

func (PluginListTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        PluginListToolName,
		Description: "List enabled, versioned Bee external-process plugins and their exact declared tool allowlists.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
	}
}

func (tool PluginListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Plugins == nil {
		return "", errors.New("plugin registry is unavailable")
	}
	if err := decodeSkillToolArgs(args, &struct{}{}); err != nil {
		return "", err
	}
	packages, err := tool.Plugins.Enabled(ctx)
	if err != nil {
		return "", err
	}
	result := PluginListResult{Plugins: make([]PluginCatalogEntry, 0, len(packages))}
	for _, pkg := range packages {
		result.Plugins = append(result.Plugins, PluginCatalogEntry{
			Name: pkg.Manifest.Name, Version: pkg.Manifest.Version,
			Description: pkg.Manifest.Description, Checksum: pkg.Checksum,
			Tools: append([]string(nil), pkg.Manifest.Extension.Tools...),
		})
	}
	data, err := json.Marshal(result)
	return string(data), err
}
