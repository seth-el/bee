package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MediaRetrieveToolName = "media_retrieve"
	MaxMediaRetrieveRank  = MaxMediaSearchCandidates
)

// MediaRetrieveTool is the narrow model-facing bridge from a prior
// media_search result to the existing MediaService/ArtifactStore pipeline.
// It accepts only a bounded rank; candidate URLs and opaque bindings remain
// parent-service state and never cross the provider boundary.
type MediaRetrieveTool struct {
	Search *MediaSearchService
	Media  *MediaService
}

// ExecutionTimeout requests the existing bounded media-job timeout without
// changing the executor's ordinary timeout for other tools.
func (tool MediaRetrieveTool) ExecutionTimeout() time.Duration {
	if tool.Media == nil || tool.Media.Timeout <= 0 {
		return defaultMediaServiceTimeout
	}
	return tool.Media.Timeout
}

type mediaRetrieveArgs struct {
	Rank int `json:"rank"`
}

type MediaRetrieveArtifact struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
}

type MediaRetrieveModelResult struct {
	Status        string                  `json:"status"`
	Rank          int                     `json:"rank"`
	Kind          string                  `json:"kind,omitempty"`
	ArtifactCount int                     `json:"artifact_count"`
	Artifacts     []MediaRetrieveArtifact `json:"artifacts,omitempty"`
	Notice        string                  `json:"notice"`
}

func (tool MediaRetrieveTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        MediaRetrieveToolName,
		Description: "Attach exactly one candidate from the most recent media_search result. Pass only its bounded rank (1-12), never a URL, token, candidate ID, or browser field. Use this after the user names or ranks a prior search result; describe the resulting Bee artifact instead of emitting Markdown or media links.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityMutating,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"rank":{"type":"integer","minimum":1,"maximum":12}},"required":["rank"],"additionalProperties":false}`),
	}
}

func (tool MediaRetrieveTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Search == nil || tool.Media == nil {
		return "", errors.New("media_retrieve is not configured")
	}
	var input mediaRetrieveArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("media_retrieve arguments: %w", err)
	}
	if input.Rank < 1 || input.Rank > MaxMediaRetrieveRank {
		return "", fmt.Errorf("media_retrieve rank must be between 1 and %d", MaxMediaRetrieveRank)
	}
	sessionID, ok := mediaSearchSessionFromContext(ctx)
	if !ok || strings.TrimSpace(string(sessionID)) == "" {
		return "", errors.New("media_retrieve requires a durable session context")
	}
	selection, err := tool.Search.ResolveRank(sessionID, input.Rank)
	if err != nil {
		return marshalMediaRetrieveResult(MediaRetrieveModelResult{
			Status: "unavailable",
			Rank:   input.Rank,
			Notice: "That media candidate is unavailable, expired, already used, or not from this session.",
		})
	}
	runID, _ := RunIDFromContext(ctx)
	artifacts, err := tool.Media.Execute(ctx, sessionID, runID, MediaCommand{Kind: CommandName(selection.Kind), URL: selection.URL}, func(progress MediaJobEvent) {
		emitMediaProgressEvent(ctx, progress)
	})
	if err != nil {
		emitMediaProgressEvent(ctx, MediaJobEvent{Command: CommandName(selection.Kind), Stage: "failed", Status: "failed", Message: "Selected media could not be attached."})
		return marshalMediaRetrieveResult(MediaRetrieveModelResult{
			Status: "failed",
			Rank:   input.Rank,
			Kind:   selection.Kind,
			Notice: "The selected media could not be attached as a Bee artifact.",
		})
	}
	out := MediaRetrieveModelResult{
		Status:        "attached",
		Rank:          input.Rank,
		Kind:          selection.Kind,
		ArtifactCount: boundedMediaAttachmentCount(len(artifacts)),
		Notice:        "The selected media is attached as a Bee artifact. Describe the artifact; do not emit Markdown or source media links.",
		Artifacts:     make([]MediaRetrieveArtifact, 0, len(artifacts)),
	}
	for _, artifact := range artifacts {
		if len(out.Artifacts) >= 4 {
			break
		}
		out.Artifacts = append(out.Artifacts, MediaRetrieveArtifact{ID: artifact.ID, Filename: artifact.Filename, MediaType: artifact.MediaType, Kind: artifact.Kind, Size: artifact.Size})
	}
	return marshalMediaRetrieveResult(out)
}

func marshalMediaRetrieveResult(result MediaRetrieveModelResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
