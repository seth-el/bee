package bee

// ToolCallLimitState is the process-local foreground-agent loop setting.
// Maximum counts provider tool-call batches per segment, matching max_tool_steps
// and context_status. A disabled limit has Maximum zero.
type ToolCallLimitState struct {
	Enabled bool   `json:"enabled"`
	Maximum int    `json:"maximum"`
	Unit    string `json:"unit"`
}

const toolCallLimitUnit = "provider_tool_batches_per_segment"

func defaultToolCallLimitState() ToolCallLimitState {
	return ToolCallLimitState{Enabled: true, Maximum: DefaultMaxToolSteps, Unit: toolCallLimitUnit}
}

// ToolCallLimit reports the current process-local setting. The bounded
// configured behavior is enabled by default; slash-command overrides are not
// persisted into durable sessions or configuration.
func (app *Application) ToolCallLimit() ToolCallLimitState {
	if app == nil {
		return defaultToolCallLimitState()
	}
	maximum := app.effectiveMaxToolSteps()
	return ToolCallLimitState{Enabled: maximum > 0, Maximum: maximum, Unit: toolCallLimitUnit}
}

// SetToolCallLimitEnabled changes the foreground-agent setting for subsequent
// turns. Enabling restores the configured max_tool_steps value.
func (app *Application) SetToolCallLimitEnabled(enabled bool) ToolCallLimitState {
	if app == nil {
		return defaultToolCallLimitState()
	}
	app.toolCallLimitDisabled.Store(!enabled)
	return app.ToolCallLimit()
}

func (app *Application) effectiveMaxToolSteps() int {
	if app == nil {
		return DefaultMaxToolSteps
	}
	if app.toolCallLimitDisabled.Load() {
		return 0
	}
	return configuredMaxToolSteps(app.Config, 0)
}
