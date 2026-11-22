package bee

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// MaxCommandArtifactOutputBytes is the hard raw-output retention ceiling.
	// Each stream receives half so a noisy stream cannot starve the other from
	// the deterministic stdout/stderr representation.
	MaxCommandArtifactOutputBytes int64 = 4 << 20
	maxCommandArtifactStreamBytes       = MaxCommandArtifactOutputBytes / 2
	MaxArtifactReadPageBytes      int64 = 64 << 10
	commandArtifactFilename             = "command-output.json"
)

const (
	commandArtifactPurpose = "command_output"
	commandArtifactNext    = "Call artifact_read with artifact_id, stream stdout or stderr, and a bounded offset/limit; continue from next_offset and do not rerun the command."
)

// CommandOutputContinuation is intentionally small and path-free. It is
// embedded in the ordinary command result only when a truncated foreground
// command has a durable session/artifact context.
type CommandOutputContinuation struct {
	Status              string `json:"status"`
	ArtifactID          string `json:"artifact_id,omitempty"`
	RetainedSize        int64  `json:"retained_size,omitempty"`
	RetainedOutputBytes int64  `json:"retained_output_bytes,omitempty"`
	StdoutRetainedBytes int64  `json:"stdout_retained_bytes,omitempty"`
	StderrRetainedBytes int64  `json:"stderr_retained_bytes,omitempty"`
	RetainedCapped      bool   `json:"retained_capped,omitempty"`
	OriginalTruncated   bool   `json:"original_truncated,omitempty"`
	NextAction          string `json:"next_action"`
}

type commandOutputEnvelope struct {
	Stdout       string                     `json:"stdout,omitempty"`
	Stderr       string                     `json:"stderr,omitempty"`
	Output       string                     `json:"output,omitempty"`
	Truncated    bool                       `json:"truncated"`
	Continuation *CommandOutputContinuation `json:"continuation,omitempty"`
}

type commandArtifactPayload struct {
	Version             int    `json:"version"`
	Encoding            string `json:"encoding"`
	Stdout              string `json:"stdout_base64"`
	Stderr              string `json:"stderr_base64"`
	StdoutOriginalBytes int64  `json:"stdout_original_bytes"`
	StderrOriginalBytes int64  `json:"stderr_original_bytes"`
	StdoutRetainedBytes int64  `json:"stdout_retained_bytes"`
	StderrRetainedBytes int64  `json:"stderr_retained_bytes"`
	StdoutCapped        bool   `json:"stdout_retention_capped"`
	StderrCapped        bool   `json:"stderr_retention_capped"`
	OriginalBytes       int64  `json:"original_bytes"`
	RetainedBytes       int64  `json:"retained_bytes"`
	OriginalTruncated   bool   `json:"original_truncated"`
	RetentionCapped     bool   `json:"retention_capped"`
}

// retainedCommandBuffer captures a bounded prefix while counting the complete
// stream size. It never returns a short write to os/exec, so command execution
// continues after the retention ceiling is reached.
type retainedCommandBuffer struct {
	data      bytes.Buffer
	limit     int64
	original  int64
	truncated bool
}

func (buffer *retainedCommandBuffer) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if int64(len(data)) > int64(^uint64(0)>>1)-buffer.original {
		buffer.original = int64(^uint64(0) >> 1)
		buffer.truncated = true
		return len(data), nil
	}
	buffer.original += int64(len(data))
	remaining := buffer.limit - int64(buffer.data.Len())
	if remaining <= 0 {
		buffer.truncated = true
		return len(data), nil
	}
	if int64(len(data)) > remaining {
		_, _ = buffer.data.Write(data[:remaining])
		buffer.truncated = true
		return len(data), nil
	}
	_, _ = buffer.data.Write(data)
	return len(data), nil
}

func (buffer *retainedCommandBuffer) Bytes() []byte { return buffer.data.Bytes() }
func (buffer *retainedCommandBuffer) Len() int64    { return int64(buffer.data.Len()) }

// commandCapture is shared by exec and legacy run_command so both paths have
// the same conservative retention and stdout/stderr semantics.
type commandCapture struct {
	stdout retainedCommandBuffer
	stderr retainedCommandBuffer
}

func newCommandCapture(maxOutput int, retain bool) commandCapture {
	limit := maxOutput / 2
	if limit < 1 {
		limit = maxOutput
	}
	if retain {
		limit = int(maxCommandArtifactStreamBytes)
	}
	return commandCapture{
		stdout: retainedCommandBuffer{limit: int64(limit)},
		stderr: retainedCommandBuffer{limit: int64(limit)},
	}
}

func (capture *commandCapture) originalBytes() int64 {
	if capture == nil {
		return 0
	}
	if capture.stdout.original > int64(^uint64(0)>>1)-capture.stderr.original {
		return int64(^uint64(0) >> 1)
	}
	return capture.stdout.original + capture.stderr.original
}

func (capture *commandCapture) retainedBytes() int64 {
	if capture == nil {
		return 0
	}
	return capture.stdout.Len() + capture.stderr.Len()
}

func (capture *commandCapture) retentionCapped() bool {
	return capture != nil && (capture.stdout.truncated || capture.stderr.truncated)
}

func (capture *commandCapture) originalTruncated(visibleLimit int) bool {
	if capture == nil || visibleLimit <= 0 {
		return false
	}
	streamLimit := visibleLimit / 2
	if streamLimit < 1 {
		streamLimit = visibleLimit
	}
	return capture.stdout.original > int64(streamLimit) || capture.stderr.original > int64(streamLimit) || capture.originalBytes() > int64(visibleLimit)
}

func combineCommandOutput(stdout, stderr []byte) string {
	output := string(stdout)
	if len(stderr) > 0 {
		if output != "" {
			output += "\n"
		}
		output += string(stderr)
	}
	return output
}

func (capture *commandCapture) visibleOutput(limit int) (string, bool) {
	if capture == nil {
		return "", false
	}
	output := combineCommandOutput(capture.stdout.Bytes(), capture.stderr.Bytes())
	truncated := capture.originalTruncated(limit) || capture.retentionCapped()
	if truncated {
		output += "\n...[command output exceeded the bounded capture]"
	}
	output, envelopeTruncated := truncateOutput(output, limit)
	return output, truncated || envelopeTruncated
}

func (capture *commandCapture) artifactPayload() ([]byte, error) {
	if capture == nil {
		return nil, errors.New("command output capture is nil")
	}
	payload := commandArtifactPayload{
		Version:             1,
		Encoding:            "base64",
		Stdout:              base64.StdEncoding.EncodeToString(capture.stdout.Bytes()),
		Stderr:              base64.StdEncoding.EncodeToString(capture.stderr.Bytes()),
		StdoutOriginalBytes: capture.stdout.original,
		StderrOriginalBytes: capture.stderr.original,
		StdoutRetainedBytes: capture.stdout.Len(),
		StderrRetainedBytes: capture.stderr.Len(),
		StdoutCapped:        capture.stdout.truncated,
		StderrCapped:        capture.stderr.truncated,
		OriginalBytes:       capture.originalBytes(),
		RetainedBytes:       capture.retainedBytes(),
		OriginalTruncated:   true,
		RetentionCapped:     capture.retentionCapped(),
	}
	return json.Marshal(payload)
}

func commandArtifactContextAvailable(ctx context.Context, store *ArtifactStore) bool {
	if store == nil || ctx == nil {
		return false
	}
	sessionID, sessionOK := SessionIDFromContext(ctx)
	runID, runOK := RunIDFromContext(ctx)
	return sessionOK && runOK && sessionID != "" && strings.TrimSpace(runID) != ""
}

func retainCommandOutput(ctx context.Context, store *ArtifactStore, capture *commandCapture) *CommandOutputContinuation {
	if capture == nil || !capture.originalTruncated(1) || !commandArtifactContextAvailable(ctx, store) {
		return nil
	}
	continuation := &CommandOutputContinuation{Status: "unavailable", OriginalTruncated: true, RetainedCapped: capture.retentionCapped(), RetainedOutputBytes: capture.retainedBytes(), NextAction: "Retention is unavailable; use a smaller command or inspect the bounded result without rerunning blindly."}
	continuation.StdoutRetainedBytes = capture.stdout.Len()
	continuation.StderrRetainedBytes = capture.stderr.Len()
	sessionID, _ := SessionIDFromContext(ctx)
	runID, _ := RunIDFromContext(ctx)
	content, err := capture.artifactPayload()
	if err != nil || int64(len(content)) > MaxArtifactBytes {
		return continuation
	}
	// Retention remains useful after a command deadline/cancellation. Context
	// values (session/run/emitter) are preserved while cancellation is detached.
	artifactCtx := context.WithoutCancel(ctx)
	artifact, err := store.Create(artifactCtx, sessionID, runID, ArtifactInput{
		Filename:          commandArtifactFilename,
		MediaType:         "application/json",
		Content:           content,
		Purpose:           commandArtifactPurpose,
		OriginalSize:      capture.originalBytes(),
		RetainedSize:      capture.retainedBytes(),
		OriginalTruncated: true,
		RetentionCapped:   capture.retentionCapped(),
	})
	if err != nil {
		return continuation
	}
	continuation.Status = "available"
	continuation.ArtifactID = artifact.ID
	continuation.RetainedSize = artifact.Size
	continuation.NextAction = commandArtifactNext
	return continuation
}

func marshalCommandOutputEnvelope(capture *commandCapture, visible string, truncated bool, continuation *CommandOutputContinuation, limit int) (string, error) {
	envelope := commandOutputEnvelope{Output: visible, Truncated: truncated, Continuation: continuation}
	if capture != nil {
		streamLimit := limit / 2
		if streamLimit < 1 {
			streamLimit = limit
		}
		envelope.Stdout, _ = truncateOutput(string(capture.stdout.Bytes()), streamLimit)
		envelope.Stderr, _ = truncateOutput(string(capture.stderr.Bytes()), streamLimit)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	if len(encoded) > limit {
		// This is only a compatibility envelope for a truncated result. Keep it
		// bounded and never make a retention failure erase the command output.
		envelope.Stdout = ""
		envelope.Stderr = ""
		envelope.Output, _ = truncateOutput(envelope.Output, limit/4)
		encoded, err = json.Marshal(envelope)
		if err != nil {
			return "", err
		}
		if len(encoded) > limit {
			// Metadata is the continuation contract. Drop the visible prefix only
			// as a last resort so the opaque artifact ID remains usable.
			envelope.Output = ""
			envelope.Continuation = continuation
			encoded, err = json.Marshal(envelope)
			if err != nil {
				return "", err
			}
			if len(encoded) > limit {
				return "", fmt.Errorf("command result envelope exceeds %d-byte bound", limit)
			}
		}
	}
	return string(encoded), nil
}

func readArtifactPage(ctx context.Context, store *ArtifactStore, args artifactReadArgs) (artifactReadResult, error) {
	result := artifactReadResult{ArtifactID: args.ArtifactID, Stream: args.Stream, Offset: args.Offset, Encoding: "base64"}
	if store == nil {
		return result, errors.New("artifact storage is unavailable")
	}
	if ctx == nil {
		return result, errors.New("artifact_read requires trusted session context")
	}
	sessionID, ok := SessionIDFromContext(ctx)
	if !ok || sessionID == "" {
		return result, errors.New("artifact_read requires a durable session")
	}
	artifact, file, err := store.OpenPayload(args.ArtifactID)
	if err != nil {
		return result, errors.New("artifact is unavailable")
	}
	defer file.Close()
	if artifact.SessionID != sessionID || artifact.Purpose != commandArtifactPurpose || artifact.RunID == "" || artifact.Filename != commandArtifactFilename || artifact.MediaType != "application/json" || artifact.Size < 0 || artifact.Size > MaxArtifactBytes {
		return result, errors.New("artifact is unavailable")
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxArtifactBytes+1))
	if err != nil || int64(len(content)) != artifact.Size || int64(len(content)) > MaxArtifactBytes {
		return result, errors.New("artifact is unavailable")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return result, errors.New("artifact is unavailable")
	}
	var payload commandArtifactPayload
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return result, errors.New("artifact is unavailable")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return result, errors.New("artifact is unavailable")
	}
	stdout, stderr, err := validateCommandArtifactPayload(payload, artifact)
	if err != nil {
		return result, errors.New("artifact is unavailable")
	}
	stream := stdout
	streamOriginal := payload.StdoutOriginalBytes
	streamCapped := payload.StdoutCapped
	if args.Stream == "stderr" {
		stream = stderr
		streamOriginal = payload.StderrOriginalBytes
		streamCapped = payload.StderrCapped
	}
	if args.Offset > int64(len(stream)) {
		return result, errors.New("artifact offset is outside the retained stream")
	}
	end := args.Offset + args.Limit
	if end < args.Offset || end > int64(len(stream)) {
		end = int64(len(stream))
	}
	page := stream[args.Offset:end]
	result.DataBase64 = base64.StdEncoding.EncodeToString(page)
	if utf8.Valid(page) {
		result.Text = string(page)
	}
	result.ReturnedBytes = len(page)
	result.NextOffset = end
	result.EOF = end >= int64(len(stream))
	result.ArtifactSize = artifact.Size
	result.StreamOriginalBytes = streamOriginal
	result.StreamRetainedBytes = int64(len(stream))
	result.TotalOriginalBytes = payload.OriginalBytes
	result.TotalRetainedBytes = payload.RetainedBytes
	result.StreamRetainedCapped = streamCapped
	result.RetainedCapped = payload.RetentionCapped
	result.OriginalTruncated = payload.OriginalTruncated
	return result, nil
}

func validateCommandArtifactPayload(payload commandArtifactPayload, artifact Artifact) ([]byte, []byte, error) {
	if payload.Version != 1 || payload.Encoding != "base64" || !payload.OriginalTruncated {
		return nil, nil, errors.New("invalid command artifact payload")
	}
	stdout, err := base64.StdEncoding.DecodeString(payload.Stdout)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := base64.StdEncoding.DecodeString(payload.Stderr)
	if err != nil {
		return nil, nil, err
	}
	if payload.StdoutOriginalBytes < 0 || payload.StderrOriginalBytes < 0 || payload.StdoutRetainedBytes != int64(len(stdout)) || payload.StderrRetainedBytes != int64(len(stderr)) {
		return nil, nil, errors.New("invalid command artifact stream sizes")
	}
	if payload.StdoutRetainedBytes > payload.StdoutOriginalBytes || payload.StderrRetainedBytes > payload.StderrOriginalBytes {
		return nil, nil, errors.New("invalid command artifact retained sizes")
	}
	if payload.StdoutCapped != (payload.StdoutRetainedBytes < payload.StdoutOriginalBytes) || payload.StderrCapped != (payload.StderrRetainedBytes < payload.StderrOriginalBytes) {
		return nil, nil, errors.New("invalid command artifact stream cap state")
	}
	original := payload.StdoutOriginalBytes + payload.StderrOriginalBytes
	retained := payload.StdoutRetainedBytes + payload.StderrRetainedBytes
	if original < payload.StdoutOriginalBytes || retained < payload.StdoutRetainedBytes || payload.OriginalBytes != original || payload.RetainedBytes != retained {
		return nil, nil, errors.New("invalid command artifact totals")
	}
	if payload.RetentionCapped != (payload.StdoutCapped || payload.StderrCapped) || artifact.OriginalSize != original || artifact.RetainedSize != retained || !artifact.OriginalTruncated || artifact.RetentionCapped != payload.RetentionCapped {
		return nil, nil, errors.New("invalid command artifact metadata")
	}
	return stdout, stderr, nil
}
