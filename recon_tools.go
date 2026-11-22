package bee

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxReconReadLines   = 256
	maxReconListEntries = 128
	maxReconFindMatches = 128
	maxReconGrepMatches = 64
	maxReconToolBytes   = 12 << 10
	maxReconLineBytes   = 1024
)

// Recon tools deliberately reuse the ordinary read-only filesystem tools but
// bound their observations before a local child sees them. They never widen
// scope or add command/network authority.
type ReconReadFileTool struct{ Scope ExecutionScope }
type ReconListFilesTool struct{ Scope ExecutionScope }
type ReconFindFilesTool struct{ Scope ExecutionScope }
type ReconGrepTool struct{ Scope ExecutionScope }

func (tool ReconReadFileTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "read_file", Description: "Read a bounded text excerpt from one file within the delegated recon scope. Paths must be relative to that scope.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}
}
func (tool ReconReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := (ReadFileTool{Scope: tool.Scope}).Execute(ctx, args)
	if err != nil {
		return "", err
	}
	return boundReconObservation(out, maxReconReadLines, maxReconToolBytes), nil
}

func (tool ReconListFilesTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "list_files", Description: "List a bounded number of entries in one directory within the delegated recon scope. Use '.' for the scope root; do not use '/'.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
}
func (tool ReconListFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := (ListFilesTool{Scope: tool.Scope}).Execute(ctx, args)
	if err != nil {
		return "", err
	}
	return boundReconObservation(out, maxReconListEntries, maxReconToolBytes), nil
}

func (tool ReconFindFilesTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "find_files", Description: "Find a bounded number of files by basename glob within the delegated recon scope. Prefer a narrow pattern.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string"}},"required":["pattern"]}`)}
}
func (tool ReconFindFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := (FindFilesTool{Scope: tool.Scope}).Execute(ctx, args)
	if err != nil {
		return "", err
	}
	return boundReconObservation(out, maxReconFindMatches, maxReconToolBytes), nil
}

func (tool ReconGrepTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "grep", Description: "Search text files within the delegated recon scope and return a bounded set of path:line:content matches. Use a narrow query and path when possible.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"query":{"type":"string"},"ignore_case":{"type":"boolean"}},"required":["query"]}`)}
}
func (tool ReconGrepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	out, err := (GrepTool{Scope: tool.Scope}).Execute(ctx, args)
	if err != nil {
		return "", err
	}
	return boundReconObservation(out, maxReconGrepMatches, maxReconToolBytes), nil
}

func newReconRegistry(scope ExecutionScope) (*Registry, error) {
	return NewRegistry(
		ReconReadFileTool{Scope: scope},
		ReconListFilesTool{Scope: scope},
		ReconFindFilesTool{Scope: scope},
		ReconGrepTool{Scope: scope},
	)
}

func boundReconObservation(raw string, maxLines, maxBytes int) string {
	if maxLines <= 0 || maxBytes <= 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	kept := make([]string, 0, reconMinInt(len(lines), maxLines))
	omitted := 0
	for _, line := range lines {
		if len(kept) >= maxLines {
			omitted++
			continue
		}
		if strings.ContainsRune(line, '\x00') || !utf8.ValidString(line) {
			omitted++
			continue
		}
		line = truncateUTF8(line, maxReconLineBytes)
		candidate := strings.Join(append(append([]string(nil), kept...), line), "\n")
		if len(candidate) > maxBytes {
			omitted += len(lines) - len(kept)
			break
		}
		kept = append(kept, line)
	}
	if omitted > 0 {
		marker := fmt.Sprintf("[recon output bounded; %d additional or non-text lines omitted]", omitted)
		candidate := strings.Join(append(append([]string(nil), kept...), marker), "\n")
		if len(candidate) <= maxBytes {
			kept = append(kept, marker)
		}
	}
	return truncateUTF8(strings.Join(kept, "\n"), maxBytes)
}

func reconMinInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
