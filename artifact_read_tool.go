package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const ArtifactReadToolName = "artifact_read"

type artifactReadArgs struct {
	ArtifactID string `json:"artifact_id"`
	Stream     string `json:"stream"`
	Offset     int64  `json:"offset,omitempty"`
	Limit      int64  `json:"limit"`
}

type artifactReadResult struct {
	ArtifactID           string `json:"artifact_id"`
	Stream               string `json:"stream"`
	Offset               int64  `json:"offset"`
	ReturnedBytes        int    `json:"returned_bytes"`
	NextOffset           int64  `json:"next_offset"`
	EOF                  bool   `json:"eof"`
	ArtifactSize         int64  `json:"artifact_size"`
	StreamOriginalBytes  int64  `json:"stream_original_bytes"`
	StreamRetainedBytes  int64  `json:"stream_retained_bytes"`
	TotalOriginalBytes   int64  `json:"total_original_bytes"`
	TotalRetainedBytes   int64  `json:"total_retained_bytes"`
	StreamRetainedCapped bool   `json:"stream_retained_capped"`
	RetainedCapped       bool   `json:"retained_capped"`
	OriginalTruncated    bool   `json:"original_truncated"`
	Encoding             string `json:"encoding"`
	DataBase64           string `json:"data_base64,omitempty"`
	Text                 string `json:"text,omitempty"`
}

type ArtifactReadTool struct {
	Store *ArtifactStore
}

func (tool ArtifactReadTool) SelfBoundedOutputLimit() int {
	return MaxSelfBoundedOutputBytes
}

func (tool ArtifactReadTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        ArtifactReadToolName,
		Description: "Read a bounded stdout or stderr page from a private retained command-output artifact in the current session. Continue from next_offset; never rerun the command.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":128},"stream":{"type":"string","enum":["stdout","stderr"]},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":65536}},"required":["artifact_id","stream","limit"]}`),
	}
}

func (tool ArtifactReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input artifactReadArgs
	if err := parseArgs(args, &input); err != nil {
		return "", errors.New("invalid artifact_read arguments")
	}
	input.ArtifactID = strings.TrimSpace(input.ArtifactID)
	if len(input.ArtifactID) > 128 || validateArtifactID(input.ArtifactID) != nil {
		return "", errors.New("artifact is unavailable")
	}
	if input.Stream != "stdout" && input.Stream != "stderr" {
		return "", errors.New("stream must be stdout or stderr")
	}
	if input.Offset < 0 {
		return "", errors.New("offset must not be negative")
	}
	if input.Limit <= 0 || input.Limit > MaxArtifactReadPageBytes {
		return "", fmt.Errorf("limit must be between 1 and %d bytes", MaxArtifactReadPageBytes)
	}
	result, err := readArtifactPage(ctx, tool.Store, input)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
