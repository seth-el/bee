package bee

import (
	"fmt"
	"strings"
	"time"
)

// The fullscreen TUI masthead: a text-only header showing the Bee wordmark,
// the active model and profile, the session, the workspace, and the current
// run state.
const (
	tuiMastheadArtRows     = 5
	tuiMastheadDetailRows  = tuiMastheadArtRows + 3
	tuiMastheadRows        = tuiMastheadDetailRows + 2
	tuiMastheadMinWidth    = 45
	tuiMastheadMinHeight   = tuiMastheadRows + 6
	tuiMastheadWideArtFrom = 49
	tuiActivityMoment      = 1800 * time.Millisecond
)

type tuiActivityState uint8

const (
	tuiActivityIdle tuiActivityState = iota
	tuiActivityThinking
	tuiActivityResponding
	tuiActivityWorking
	tuiActivityApproval
	tuiActivityError
	tuiActivitySuccess
)

type tuiActivityOutcome uint8

const (
	tuiActivityOutcomeNone tuiActivityOutcome = iota
	tuiActivityOutcomeFailed
	tuiActivityOutcomeCancelled
)

// tuiActivity tracks the run state shown in the masthead. Error and success
// states are held briefly, then return to idle.
type tuiActivity struct {
	state     tuiActivityState
	holdUntil time.Time
	outcome   tuiActivityOutcome
}

func newTUIActivity() tuiActivity {
	return tuiActivity{state: tuiActivityIdle}
}

func (activity *tuiActivity) beginRun(now time.Time) {
	activity.outcome = tuiActivityOutcomeNone
	activity.setState(tuiActivityWorking, now, 0)
}

func (activity *tuiActivity) failRun(now time.Time) {
	activity.outcome = tuiActivityOutcomeFailed
	activity.showError(now, tuiActivityMoment)
}

func (activity *tuiActivity) cancelRun(now time.Time) {
	activity.outcome = tuiActivityOutcomeCancelled
	activity.setState(tuiActivityIdle, now, 0)
}

func (activity *tuiActivity) finishRun(now time.Time) {
	switch activity.outcome {
	case tuiActivityOutcomeFailed:
		activity.showError(now, tuiActivityMoment)
	case tuiActivityOutcomeCancelled:
		activity.setState(tuiActivityIdle, now, 0)
	default:
		activity.setState(tuiActivitySuccess, now, tuiActivityMoment)
	}
	activity.outcome = tuiActivityOutcomeNone
}

func (activity *tuiActivity) beginTool(now time.Time) {
	activity.setState(tuiActivityWorking, now, 0)
}

func (activity *tuiActivity) showError(now time.Time, hold time.Duration) {
	activity.setState(tuiActivityError, now, hold)
}

func (activity *tuiActivity) setState(state tuiActivityState, now time.Time, hold time.Duration) {
	activity.state = state
	activity.holdUntil = time.Time{}
	if hold > 0 {
		activity.holdUntil = now.Add(hold)
	}
}

// advance expires a held state. It reports whether the label changed.
func (activity *tuiActivity) advance(now time.Time) bool {
	if !activity.holdUntil.IsZero() && !now.Before(activity.holdUntil) {
		activity.setState(tuiActivityIdle, now, 0)
		return true
	}
	return false
}

func (activity tuiActivity) label() string {
	switch activity.state {
	case tuiActivityThinking:
		return "thinking"
	case tuiActivityResponding:
		return "responding"
	case tuiActivityWorking:
		return "working"
	case tuiActivityApproval:
		return "needs approval"
	case tuiActivityError:
		return "error"
	case tuiActivitySuccess:
		return "done"
	default:
		return "ready"
	}
}

type tuiMastheadInfo struct {
	model   string
	profile string
	session string
	scope   string
}

type tuiMastheadLine struct {
	text     string
	rendered string
}

func tuiMastheadVisible(width, paneHeight int) bool {
	return width >= tuiMastheadMinWidth && paneHeight >= tuiMastheadMinHeight
}

// renderedMasthead returns the masthead rows, or nil when the terminal is too
// small to show it.
func (activity tuiActivity) renderedMasthead(width int, info tuiMastheadInfo) []string {
	if width < tuiMastheadMinWidth {
		return nil
	}
	contentWidth := width - 4
	topLabel := strings.TrimSpace(strings.Trim(strings.TrimSpace(info.model+" · "+info.profile), "·"))
	if topLabel != "" {
		topLabel = " " + topLabel + " "
	}
	details := make([]tuiMastheadLine, 0, tuiMastheadDetailRows)
	for _, text := range tuiPixelWordForWidth("BEE", contentWidth) {
		details = append(details, tuiMastheadArtLine(text, tuiBold+tuiCyan, contentWidth))
	}
	details = append(details, tuiMastheadLine{})
	details = append(details, tuiMastheadMetadataLine("session", info.session, contentWidth))
	details = append(details, tuiMastheadMetadataLine("workspace", info.scope, contentWidth))
	lines := make([]string, 0, tuiMastheadRows)
	lines = append(lines, tuiCyan+tuiBoxBorder('╭', '╮', topLabel, width)+tuiReset)
	for _, detail := range details {
		padding := contentWidth - tuiDisplayWidth(detail.text)
		if padding < 0 {
			padding = 0
		}
		lines = append(lines, tuiCyan+"│"+tuiReset+" "+detail.rendered+strings.Repeat(" ", padding)+" "+tuiCyan+"│"+tuiReset)
	}
	status := " ● " + activity.label() + " "
	lines = append(lines, tuiCyan+tuiBold+tuiBoxBorder('╰', '╯', status, width)+tuiReset)
	return lines
}

func formatTUIContextUsage(used, window int, known bool) string {
	total := formatTUITokenCount(window)
	value := "— / " + total
	if window <= 0 {
		value = "— / unknown"
	}
	if known {
		value = "~" + formatTUITokenCount(used) + " / " + total
		if window > 0 {
			percent := float64(used) * 100 / float64(window)
			value += fmt.Sprintf(" · %.1f%%", percent)
		}
	}
	return "context " + value
}

func formatTUITokenCount(tokens int) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func tuiPixelWordForWidth(word string, width int) []string {
	if width < tuiMastheadWideArtFrom {
		return tuiPixelWordScaled(word, 2)
	}
	return tuiPixelWordScaled(word, 3)
}

func tuiPixelWordScaled(word string, pixelWidth int) []string {
	glyphs := map[rune][tuiMastheadArtRows]string{
		'B': {"11110", "10001", "11110", "10001", "11110"},
		'E': {"11111", "10000", "11110", "10000", "11111"},
	}
	if pixelWidth < 1 {
		pixelWidth = 1
	}
	pixel := strings.Repeat("█", pixelWidth)
	blank := strings.Repeat(" ", pixelWidth)
	lines := make([]string, tuiMastheadArtRows)
	for row := range lines {
		var line strings.Builder
		for index, letter := range word {
			if index > 0 {
				line.WriteString("  ")
			}
			for _, value := range glyphs[letter][row] {
				if value == '1' {
					line.WriteString(pixel)
				} else {
					line.WriteString(blank)
				}
			}
		}
		lines[row] = strings.TrimRight(line.String(), " ")
	}
	return lines
}

func tuiMastheadArtLine(value, style string, width int) tuiMastheadLine {
	value = clipRunes(value, width)
	return tuiMastheadLine{text: value, rendered: style + value + tuiReset}
}

func tuiMastheadMetadataLine(label, value string, width int) tuiMastheadLine {
	label += strings.Repeat(" ", 10-len(label))
	value = strings.TrimSpace(value)
	if value == "" {
		value = "—"
	}
	value = tuiCompactText(value, width-tuiDisplayWidth(label))
	return tuiMastheadLine{text: label + value, rendered: tuiCyan + label + tuiReset + value}
}
