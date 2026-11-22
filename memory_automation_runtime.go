package bee

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func boundTurnMemoryObservations(observations []TurnMemoryObservation, maxBytes int) []TurnMemoryObservation {
	if maxBytes <= 0 {
		return nil
	}
	result := make([]TurnMemoryObservation, 0, len(observations))
	used := 0
	for _, observation := range observations {
		item := observation
		item.ToolName = truncateText(item.ToolName, 256)
		item.Output = truncateText(item.Output, maxBytes)
		item.Error = truncateText(item.Error, maxBytes)
		encodedLen := len(item.ToolName) + len(item.Output) + len(item.Error) + 32
		if used+encodedLen > maxBytes {
			remaining := maxBytes - used
			if remaining <= 0 {
				break
			}
			item.Output = truncateText(item.Output, remaining)
			item.Error = ""
			encodedLen = len(item.ToolName) + len(item.Output) + 32
			if used+encodedLen > maxBytes {
				break
			}
		}
		result = append(result, item)
		used += encodedLen
	}
	return result
}

// startPostTurnMemory runs post-turn memory extraction after the reply,
// off the run's event stream, so a slow extraction call never holds back
// run_end, the session lease or a channel reply. The result reaches /status
// as a memory_extraction telemetry event. Close waits for pending work; the
// extraction stays bounded by its own timeout.
func (app *Application) startPostTurnMemory(runCtx context.Context, input TurnMemoryInput) {
	if app == nil || !app.Config.Memory.Automation.effective().Enabled {
		return
	}
	ctx := context.WithoutCancel(runCtx)
	app.postTurnWork.Add(1)
	go func() {
		defer app.postTurnWork.Done()
		extraction := app.extractPostTurnMemory(ctx, input)
		if !extraction.Attempted {
			return
		}
		event := MemoryExtractionEvent{Attempted: extraction.Attempted, Saved: append([]string(nil), extraction.Saved...), Unchanged: append([]string(nil), extraction.Unchanged...), Proposed: append([]string(nil), extraction.Proposed...), Rejected: extraction.Rejected, Error: extraction.Error}
		app.observeCommandStatusEvent(RunEvent{Type: RunMemoryExtraction, At: time.Now().UTC(), RunID: input.RunID, SessionID: input.SessionID, Profile: input.Provider, Role: input.Role, Model: input.Model, MemoryExtraction: &event})
	}()
}

// WaitPostTurnWork blocks until post-turn memory extraction has finished.
func (app *Application) WaitPostTurnWork() {
	if app != nil {
		app.postTurnWork.Wait()
	}
}

func (app *Application) extractPostTurnMemory(parentCtx context.Context, input TurnMemoryInput) MemoryExtractionReport {
	if app == nil {
		return MemoryExtractionReport{}
	}
	config := app.Config.Memory.Automation.effective()
	if !config.Enabled {
		return MemoryExtractionReport{}
	}
	if strings.TrimSpace(input.Prompt) == "" || strings.TrimSpace(input.Assistant) == "" {
		return MemoryExtractionReport{Attempted: true, Rejected: 1, Error: "memory extraction requires a non-empty completed turn"}
	}
	extractor := app.MemoryExtractor
	if extractor == nil {
		var err error
		extractor, err = app.newConfiguredMemoryExtractor(config)
		if err != nil {
			return MemoryExtractionReport{Attempted: true, Error: err.Error()}
		}
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()
	result, err := extractor.Extract(ctx, input)
	if err != nil {
		return MemoryExtractionReport{Attempted: true, Error: safeMemoryExtractionError(err)}
	}
	if app.MemoryService == nil {
		return MemoryExtractionReport{Attempted: true, Rejected: len(result.Candidates), Error: "memory service is not configured"}
	}
	return ApplyMemoryExtraction(ctx, app.MemoryService, app.MemoryActor, app.MemoryPolicy, input, result, config)
}

func safeMemoryExtractionError(err error) string {
	if err == nil {
		return ""
	}
	message := truncateText(err.Error(), 1024)
	if looksLikeSecret(message) {
		return "memory extraction provider failed"
	}
	return message
}

func (app *Application) newConfiguredMemoryExtractor(config MemoryAutomationConfig) (PostTurnMemoryExtractor, error) {
	if app == nil {
		return nil, errors.New("application is nil")
	}
	var provider Provider
	model := ""
	var broker *LocalChildCapacityBroker
	if app.Specialist != nil && (config.ProviderProfile == "" || config.ProviderProfile == app.Specialist.Profile()) {
		var err error
		provider, err = app.Specialist.getProvider()
		if err != nil {
			return nil, fmt.Errorf("memory extraction specialist provider: %w", err)
		}
		model = app.Specialist.Model()
		broker = app.ChildBroker
	} else if config.ProviderProfile != "" {
		for _, profile := range app.Config.Profiles {
			if profile.Name != config.ProviderProfile {
				continue
			}
			if profile.Role == ProviderRoleManual {
				return nil, errors.New("memory extraction refuses manual/cloud providers")
			}
			if !providerIsLocal(profile.Provider) {
				return nil, errors.New("memory extraction provider must use a local endpoint")
			}
			var err error
			provider, err = NewProvider(profile.Provider)
			if err != nil {
				return nil, fmt.Errorf("memory extraction provider: %w", err)
			}
			model = profile.Provider.Model
			if profile.Role == ProviderRoleChild {
				broker = app.ChildBroker
			}
			break
		}
		if provider == nil {
			return nil, fmt.Errorf("memory extraction provider profile %q is not configured", config.ProviderProfile)
		}
	} else {
		switch config.ProviderRole {
		case ProviderRoleChild:
			var err error
			provider, err = app.childProvider()
			if err != nil {
				return nil, fmt.Errorf("memory extraction child provider: %w", err)
			}
			model = app.ChildConfigOrMain().Provider.Model
			if !providerIsLocal(app.ChildConfigOrMain().Provider) {
				return nil, errors.New("memory extraction child provider must use a local endpoint")
			}
			broker = app.ChildBroker
		case ProviderRoleMain:
			if app.ProviderRole == ProviderRoleManual || !providerIsLocal(app.Config.Provider) {
				return nil, errors.New("memory extraction refuses non-local main providers")
			}
			provider = app.Provider
			model = app.Config.Provider.Model
		default:
			return nil, errors.New("memory extraction requires child or local main provider role")
		}
	}
	if provider == nil {
		return nil, errors.New("memory extraction provider is unavailable")
	}
	return ProviderPostTurnMemoryExtractor{Provider: provider, Model: model, MaxOutputBytes: config.MaxOutputBytes, MaxCandidates: config.MaxCandidates, MaxPromptBytes: config.MaxPromptBytes, MaxAssistantBytes: config.MaxAssistantBytes, MaxToolObservationBytes: config.MaxToolObservationBytes, CapacityBroker: broker}, nil
}
