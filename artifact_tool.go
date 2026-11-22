package bee

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CreateArtifactTool is an explicit presentation path. It is intentionally not
// a file watcher: only a deliberate model call creates a durable artifact.
type CreateArtifactTool struct {
	Store *ArtifactStore
}

func (tool CreateArtifactTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        "create_artifact",
		Description: "Present one bounded output as a durable downloadable artifact. Use only when the user explicitly asks for a file or downloadable output.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"filename":{"type":"string"},"media_type":{"type":"string"},"content":{"type":"string"}},"required":["filename","content"]}`),
	}
}

type createArtifactArgs struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type,omitempty"`
	Content   string `json:"content"`
}

func (tool CreateArtifactTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", fmt.Errorf("create_artifact store is not configured")
	}
	var input createArtifactArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("create_artifact arguments: %w", err)
	}
	if strings.TrimSpace(input.Content) == "" {
		return "", fmt.Errorf("create_artifact content is required")
	}
	sessionID, _ := SessionIDFromContext(ctx)
	runID, _ := RunIDFromContext(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("create_artifact requires a durable session")
	}
	artifact, err := tool.Store.Create(ctx, sessionID, runID, ArtifactInput{Filename: input.Filename, MediaType: input.MediaType, Content: []byte(input.Content)})
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(artifact)
	return string(data), nil
}
