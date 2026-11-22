package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	DelegateReconToolName              = "delegate_recon"
	MaxDelegateReconObjectiveBytes     = 4096
	MaxDelegateReconPromptBytes        = 8192
	MaxDelegateReconQueries            = 8
	MaxDelegateReconPatterns           = 8
	MaxDelegateReconSeedFiles          = 8
	MaxDelegateReconSourceBytes        = 16 << 10
	MaxDelegateReconSourcePreviewBytes = 4 << 10
	MaxDelegateReconTimeoutSeconds     = 120
)

type DelegateReconQuery struct {
	Path       string `json:"path,omitempty"`
	Query      string `json:"query"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
}

type DelegateReconRequest struct {
	Objective    string               `json:"objective"`
	Prompt       string               `json:"prompt,omitempty"`
	Scope        string               `json:"scope,omitempty"`
	Queries      []DelegateReconQuery `json:"queries,omitempty"`
	FilePatterns []string             `json:"file_patterns,omitempty"`
	SeedFiles    []string             `json:"seed_files,omitempty"`
}

type DelegateReconResult struct {
	Objective     string         `json:"objective"`
	Evidence      EvidencePacket `json:"evidence"`
	SourcePreview string         `json:"source_preview,omitempty"`
	SourceBytes   int            `json:"source_bytes"`
	Truncated     bool           `json:"truncated,omitempty"`
	AdvisoryOnly  bool           `json:"advisory_only"`
}

// DelegateReconService performs deterministic, parent-owned read-only
// collection and gives the bounded resulting corpus to one tool-free local
// child evidence call. The local child never chooses or executes tools.
type DelegateReconService struct {
	Application *Application
	Scope       ExecutionScope
	Extractor   *EvidenceExtractor
}

func NewDelegateReconService(application *Application, scope ExecutionScope) (*DelegateReconService, error) {
	if application == nil || application.Specialist == nil {
		return nil, errors.New("delegate recon application and specialist are required")
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("delegate recon parent scope: %w", err)
	}
	extractor, err := NewEvidenceExtractor(application.Specialist)
	if err != nil {
		return nil, fmt.Errorf("delegate recon evidence extractor: %w", err)
	}
	return &DelegateReconService{Application: application, Scope: scope, Extractor: extractor}, nil
}

func (service *DelegateReconService) Execute(ctx context.Context, input DelegateReconRequest) (DelegateReconResult, error) {
	if service == nil || service.Application == nil || service.Extractor == nil {
		return DelegateReconResult{}, errors.New("delegate recon is unavailable")
	}
	input.Objective = strings.TrimSpace(input.Objective)
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Objective == "" || len(input.Objective) > MaxDelegateReconObjectiveBytes || len(input.Prompt) > MaxDelegateReconPromptBytes {
		return DelegateReconResult{}, errors.New("delegate recon objective or prompt is invalid")
	}
	if len(input.Queries) > MaxDelegateReconQueries || len(input.FilePatterns) > MaxDelegateReconPatterns || len(input.SeedFiles) > MaxDelegateReconSeedFiles {
		return DelegateReconResult{}, errors.New("delegate recon request exceeds collection bounds")
	}
	if len(input.Queries)+len(input.FilePatterns)+len(input.SeedFiles) == 0 {
		return DelegateReconResult{}, errors.New("delegate recon requires at least one query, file pattern, or seed file")
	}

	scope, err := service.resolveScope(input.Scope)
	if err != nil {
		return DelegateReconResult{}, err
	}
	readTool := ReconReadFileTool{Scope: scope}
	findTool := ReconFindFilesTool{Scope: scope}
	grepTool := ReconGrepTool{Scope: scope}

	var source strings.Builder
	appendSection := func(kind, selector, output string) bool {
		section := fmt.Sprintf("[%s %q]\n%s\n", kind, selector, strings.TrimSpace(output))
		remaining := MaxDelegateReconSourceBytes - source.Len()
		if remaining <= 0 {
			return false
		}
		if len(section) > remaining {
			section = truncateUTF8(section, remaining)
			source.WriteString(section)
			return false
		}
		source.WriteString(section)
		return true
	}

	truncated := false
	for _, path := range input.SeedFiles {
		path = strings.TrimSpace(path)
		if err := validateReconRelativeSelector(path); err != nil {
			return DelegateReconResult{}, fmt.Errorf("delegate recon seed file: %w", err)
		}
		args, _ := json.Marshal(map[string]string{"path": path})
		output, readErr := readTool.Execute(ctx, args)
		if readErr != nil {
			return DelegateReconResult{}, fmt.Errorf("delegate recon read %q: %w", path, readErr)
		}
		if !appendSection("file", path, output) {
			truncated = true
			break
		}
	}
	if !truncated {
		for _, pattern := range input.FilePatterns {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" || len(pattern) > 256 || strings.ContainsAny(pattern, `/\\`) {
				return DelegateReconResult{}, errors.New("delegate recon file pattern is invalid")
			}
			args, _ := json.Marshal(map[string]string{"path": ".", "pattern": pattern})
			output, findErr := findTool.Execute(ctx, args)
			if findErr != nil {
				return DelegateReconResult{}, fmt.Errorf("delegate recon find %q: %w", pattern, findErr)
			}
			if !appendSection("find", pattern, output) {
				truncated = true
				break
			}
		}
	}
	if !truncated {
		for _, query := range input.Queries {
			query.Query = strings.TrimSpace(query.Query)
			query.Path = strings.TrimSpace(query.Path)
			if query.Query == "" || len(query.Query) > 512 {
				return DelegateReconResult{}, errors.New("delegate recon query is invalid")
			}
			if query.Path == "" {
				query.Path = "."
			}
			if err := validateReconRelativeSelector(query.Path); err != nil {
				return DelegateReconResult{}, fmt.Errorf("delegate recon query path: %w", err)
			}
			args, _ := json.Marshal(query)
			output, grepErr := grepTool.Execute(ctx, args)
			if grepErr != nil {
				return DelegateReconResult{}, fmt.Errorf("delegate recon grep %q: %w", query.Query, grepErr)
			}
			selector := query.Query + " @ " + query.Path
			if !appendSection("grep", selector, output) {
				truncated = true
				break
			}
		}
	}

	corpus := strings.TrimSpace(strings.Join([]string{
		"Objective: " + input.Objective,
		"Parent guidance: " + input.Prompt,
		"Collected read-only evidence:\n" + source.String(),
	}, "\n\n"))
	if len(corpus) > MaxDelegateReconSourceBytes {
		corpus = truncateUTF8(corpus, MaxDelegateReconSourceBytes)
		truncated = true
	}
	packet, err := service.Extractor.Extract(ctx, corpus)
	if err != nil {
		return DelegateReconResult{}, fmt.Errorf("delegate recon evidence extraction: %w", err)
	}
	return DelegateReconResult{
		Objective:     input.Objective,
		Evidence:      packet,
		SourcePreview: truncateUTF8(source.String(), MaxDelegateReconSourcePreviewBytes),
		SourceBytes:   len(corpus),
		Truncated:     truncated,
		AdvisoryOnly:  true,
	}, nil
}

func (service *DelegateReconService) resolveScope(raw string) (ExecutionScope, error) {
	parentRoot, err := service.Scope.rootPath()
	if err != nil {
		return ExecutionScope{}, err
	}
	relative := strings.TrimSpace(raw)
	if relative == "" {
		relative = "."
	}
	if err := validateReconRelativeSelector(relative); err != nil {
		return ExecutionScope{}, err
	}
	resolved, err := securePath(service.Scope, relative, false)
	if err != nil {
		return ExecutionScope{}, fmt.Errorf("delegate recon scope: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return ExecutionScope{}, fmt.Errorf("resolve delegate recon scope symlinks: %w", err)
	}
	if !scopeWithin(parentRoot, resolved) {
		return ExecutionScope{}, errors.New("delegate recon scope escapes assistant root")
	}
	return ExecutionScope{Root: resolved, TimeoutSeconds: MaxDelegateReconTimeoutSeconds, MaxOutputBytes: MaxDelegateReconSourceBytes}, nil
}

func validateReconRelativeSelector(value string) error {
	if value == "" || len(value) > 1024 || filepath.IsAbs(value) {
		return errors.New("path must be a bounded relative selector")
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("path escapes delegated scope")
	}
	return nil
}

type DelegateReconTool struct{ Service *DelegateReconService }

func (tool DelegateReconTool) Definition() ToolDefinition {
	return ToolDefinition{Name: DelegateReconToolName, Description: "Collect bounded read-only file/grep evidence selected by the parent, then ask the local child for one strict anchored evidence packet. The child receives no tools and cannot browse, mutate, write memory, or delegate.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"objective":{"type":"string","maxLength":4096},"prompt":{"type":"string","maxLength":8192},"scope":{"type":"string","maxLength":1024},"queries":{"type":"array","maxItems":8,"items":{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","maxLength":1024},"query":{"type":"string","maxLength":512},"ignore_case":{"type":"boolean"}},"required":["query"]}},"file_patterns":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":256}},"seed_files":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":1024}}},"required":["objective"],"anyOf":[{"required":["queries"]},{"required":["file_patterns"]},{"required":["seed_files"]}]}`)}
}

func (tool DelegateReconTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Service == nil {
		return "", errors.New("delegate_recon service is unavailable")
	}
	var input DelegateReconRequest
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("delegate_recon arguments: %w", err)
	}
	result, err := tool.Service.Execute(ctx, input)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func (service *DelegateReconService) ExecutionTimeout() time.Duration {
	return time.Duration(MaxDelegateReconTimeoutSeconds) * time.Second
}
