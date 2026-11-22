package bee

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ProviderCallTiming is per-provider-call performance telemetry (G9): how
// long the call took, how long before the first token arrived (prompt
// processing, for a local model), decode speed, and how full the context
// was. All numbers, no content; fields are omitted when unknown.
type ProviderCallTiming struct {
	DurationMs            int64   `json:"duration_ms,omitempty"`
	TimeToFirstTokenMs    int64   `json:"time_to_first_token_ms,omitempty"`
	DecodeTokensPerSecond float64 `json:"decode_tokens_per_second,omitempty"`
	ContextWindowTokens   int     `json:"context_window_tokens,omitempty"`
	ContextUsedTokens     int     `json:"context_used_tokens,omitempty"`
	// ContextUsedEstimated is true when the provider reported no prompt
	// token count and Bee's own estimate was used.
	ContextUsedEstimated bool `json:"context_used_estimated,omitempty"`
}

// minDecodeWindow avoids a meaningless tokens/second from a reply that
// arrived in one chunk.
const minDecodeWindow = 50 * time.Millisecond

func (agent *Agent) providerNow() time.Time {
	if agent.clock != nil {
		return agent.clock()
	}
	return time.Now()
}

// beginProviderCall starts timing one provider request.
func (agent *Agent) beginProviderCall() {
	agent.callStarted = agent.providerNow()
	agent.callFirstToken = time.Time{}
}

// markFirstToken records the first streamed output of the current call:
// content, reasoning or a tool-call fragment.
func (agent *Agent) markFirstToken() {
	if !agent.callStarted.IsZero() && agent.callFirstToken.IsZero() {
		agent.callFirstToken = agent.providerNow()
	}
}

// finishProviderCall computes the timing for the call that produced
// response and resets the clock. It returns nil when no call was timed.
func (agent *Agent) finishProviderCall(response Response) *ProviderCallTiming {
	if agent.callStarted.IsZero() {
		return nil
	}
	end := agent.providerNow()
	timing := &ProviderCallTiming{DurationMs: end.Sub(agent.callStarted).Milliseconds()}
	if !agent.callFirstToken.IsZero() {
		timing.TimeToFirstTokenMs = agent.callFirstToken.Sub(agent.callStarted).Milliseconds()
		if decode := end.Sub(agent.callFirstToken); decode >= minDecodeWindow && response.Usage.CompletionTokens > 0 {
			rate := float64(response.Usage.CompletionTokens) / decode.Seconds()
			timing.DecodeTokensPerSecond = math.Round(rate*10) / 10
		}
	}
	if window := agent.providerContextWindow(); window > 0 {
		timing.ContextWindowTokens = window
	}
	if response.Usage.PromptTokens > 0 {
		timing.ContextUsedTokens = response.Usage.PromptTokens
	} else if agent.lastContextUsage.PromptUsed > 0 {
		timing.ContextUsedTokens = agent.lastContextUsage.PromptUsed
		timing.ContextUsedEstimated = true
	}
	agent.callStarted, agent.callFirstToken = time.Time{}, time.Time{}
	return timing
}

// runPerformance accumulates the timed provider calls of one run for the
// TUI's end-of-run line.
type runPerformance struct {
	timedCalls   int
	modelMs      int64
	firstTokenMs int64
	firstCounted int
	decodeTokens int
	decodeMs     int64
}

func (perf *runPerformance) add(event *UsageEvent) {
	if perf == nil || event == nil || event.Timing == nil {
		return
	}
	timing := event.Timing
	perf.timedCalls++
	perf.modelMs += timing.DurationMs
	if timing.TimeToFirstTokenMs > 0 {
		perf.firstTokenMs += timing.TimeToFirstTokenMs
		perf.firstCounted++
	}
	if timing.DecodeTokensPerSecond > 0 && event.Usage.CompletionTokens > 0 {
		perf.decodeTokens += event.Usage.CompletionTokens
		perf.decodeMs += timing.DurationMs - timing.TimeToFirstTokenMs
	}
}

// summary renders "model 4.2s (first token 1.1s, 38 tok/s) · other 4.9s".
// wall is the run's elapsed time; zero omits the "other" part.
func (perf runPerformance) summary(wall time.Duration) string {
	if perf.timedCalls == 0 {
		return ""
	}
	text := "model " + formatPerfSeconds(perf.modelMs)
	var details []string
	if perf.firstCounted > 0 {
		label := "first token "
		if perf.firstCounted > 1 {
			label = "first token avg "
		}
		details = append(details, label+formatPerfSeconds(perf.firstTokenMs/int64(perf.firstCounted)))
	}
	if perf.decodeMs > 0 && perf.decodeTokens > 0 {
		details = append(details, fmt.Sprintf("%.0f tok/s", float64(perf.decodeTokens)/(float64(perf.decodeMs)/1000)))
	}
	if len(details) > 0 {
		text += " (" + strings.Join(details, ", ") + ")"
	}
	if other := wall.Milliseconds() - perf.modelMs; wall > 0 && other >= 100 {
		text += " · other " + formatPerfSeconds(other)
	}
	return text
}

func formatPerfSeconds(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 10_000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%ds", (ms+500)/1000)
}

// formatProviderCallTiming renders one call for /status:
// "4.2s · first token 1.1s · 38.2 tok/s · context 12,345 / 32,768 (38%)".
func formatProviderCallTiming(timing ProviderCallTiming) string {
	parts := []string{formatPerfSeconds(timing.DurationMs)}
	if timing.TimeToFirstTokenMs > 0 {
		parts = append(parts, "first token "+formatPerfSeconds(timing.TimeToFirstTokenMs))
	}
	if timing.DecodeTokensPerSecond > 0 {
		parts = append(parts, fmt.Sprintf("%.1f tok/s", timing.DecodeTokensPerSecond))
	}
	if timing.ContextUsedTokens > 0 {
		context := "context " + formatStatusInteger(timing.ContextUsedTokens)
		if timing.ContextUsedEstimated {
			context = "context ~" + formatStatusInteger(timing.ContextUsedTokens)
		}
		if timing.ContextWindowTokens > 0 {
			context += fmt.Sprintf(" / %s (%d%%)", formatStatusInteger(timing.ContextWindowTokens), timing.ContextUsedTokens*100/timing.ContextWindowTokens)
		}
		parts = append(parts, context)
	}
	return strings.Join(parts, " · ")
}
