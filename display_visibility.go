package bee

import "strings"

// ToolDisplayMode controls the human-facing projection of tool activity. It
// never changes execution, durable session records, or provider replay.
type ToolDisplayMode string

const (
	ToolDisplayOff     ToolDisplayMode = "off"
	ToolDisplayCompact ToolDisplayMode = "compact"
	ToolDisplayFull    ToolDisplayMode = "full"
)

func normalizeToolDisplayMode(mode ToolDisplayMode) ToolDisplayMode {
	switch ToolDisplayMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case ToolDisplayOff:
		return ToolDisplayOff
	case ToolDisplayFull:
		return ToolDisplayFull
	default:
		return ToolDisplayCompact
	}
}

// DisplayVisibility controls which diagnostic details owner-facing adapters
// show. It never changes execution, durable session data, or provider replay.
type DisplayVisibility struct {
	// ToolCalls is retained for API compatibility. ToolMode is authoritative
	// when present; ToolCalls=false always means off.
	ToolCalls bool            `json:"tool_calls"`
	ToolMode  ToolDisplayMode `json:"tool_mode"`
	Thinking  bool            `json:"thinking"`
}

func defaultDisplayVisibility() DisplayVisibility {
	return DisplayVisibility{ToolCalls: true, ToolMode: ToolDisplayCompact, Thinking: true}
}

func (visibility DisplayVisibility) toolDisplayMode() ToolDisplayMode {
	if !visibility.ToolCalls {
		return ToolDisplayOff
	}
	if visibility.ToolMode == "" {
		// A caller constructing the legacy boolean-only shape explicitly asked
		// for the former full-detail behavior.
		return ToolDisplayFull
	}
	return normalizeToolDisplayMode(visibility.ToolMode)
}

func (visibility DisplayVisibility) EffectiveToolMode() ToolDisplayMode {
	return visibility.toolDisplayMode()
}

// DisplayVisibility returns the current process-local owner display policy.
// New applications initialize these preferences from AssistantConfig, while a
// zero-value Application used by tests and embedders keeps compact tool
// activity and thinking visible.
func (app *Application) DisplayVisibility() DisplayVisibility {
	if app == nil {
		return defaultDisplayVisibility()
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	mode := normalizeToolDisplayMode(app.toolDisplayMode)
	if app.toolCallsHidden {
		mode = ToolDisplayOff
	}
	return DisplayVisibility{
		ToolCalls: mode != ToolDisplayOff,
		ToolMode:  mode,
		Thinking:  !app.thinkingHidden,
	}
}

func (app *Application) SetToolCallsVisible(visible bool) DisplayVisibility {
	if app == nil {
		return defaultDisplayVisibility()
	}
	app.mu.Lock()
	mode := ToolDisplayOff
	if visible {
		mode = ToolDisplayFull
	}
	app.toolCallsHidden = mode == ToolDisplayOff
	app.toolDisplayMode = mode
	visibility := DisplayVisibility{ToolCalls: visible, ToolMode: mode, Thinking: !app.thinkingHidden}
	app.mu.Unlock()
	return visibility
}

func (app *Application) SetToolDisplayMode(mode ToolDisplayMode) DisplayVisibility {
	if app == nil {
		return defaultDisplayVisibility()
	}
	mode = normalizeToolDisplayMode(mode)
	app.mu.Lock()
	app.toolCallsHidden = mode == ToolDisplayOff
	app.toolDisplayMode = mode
	visibility := DisplayVisibility{ToolCalls: mode != ToolDisplayOff, ToolMode: mode, Thinking: !app.thinkingHidden}
	app.mu.Unlock()
	return visibility
}

func (app *Application) SetThinkingVisible(visible bool) DisplayVisibility {
	if app == nil {
		return defaultDisplayVisibility()
	}
	app.mu.Lock()
	app.thinkingHidden = !visible
	mode := normalizeToolDisplayMode(app.toolDisplayMode)
	if app.toolCallsHidden {
		mode = ToolDisplayOff
	}
	visibility := DisplayVisibility{ToolCalls: mode != ToolDisplayOff, ToolMode: mode, Thinking: !app.thinkingHidden}
	app.mu.Unlock()
	return visibility
}
