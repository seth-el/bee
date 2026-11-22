package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	CurrentTimeToolName   = "current_time"
	MaxCurrentTimeZoneLen = 128
)

// CurrentTimeTool is a read-only local clock adapter. It never performs
// network I/O; the clock and default zone are injectable for deterministic
// tests and composition-specific user settings.
type CurrentTimeTool struct {
	Clock           func() time.Time
	DefaultTimeZone string
}

type currentTimeArgs struct {
	TimeZone string `json:"timezone,omitempty"`
}

type CurrentTimeResult struct {
	UTC         string `json:"utc"`
	Local       string `json:"local"`
	TimeZone    string `json:"timezone"`
	Date        string `json:"date"`
	DayOfWeek   string `json:"day_of_week"`
	UnixSeconds int64  `json:"unix_seconds"`
}

func (tool CurrentTimeTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        CurrentTimeToolName,
		Description: "Read the current UTC and local time without network access. Optionally provide an IANA timezone; otherwise the configured user timezone is used.",
		Risk:        RiskLow,
		Mutability:  ToolMutabilityReadOnly,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","maxLength":128}},"additionalProperties":false}`),
	}
}

func (tool CurrentTimeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var input currentTimeArgs
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("current_time arguments: %w", err)
	}
	zone := strings.TrimSpace(input.TimeZone)
	if zone == "" {
		zone = strings.TrimSpace(tool.DefaultTimeZone)
	}
	if zone == "" {
		zone = DefaultWebTimeZone
	}
	if err := validateCurrentTimeZone(zone); err != nil {
		return "", err
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return "", fmt.Errorf("current_time timezone: %w", err)
	}
	now := time.Now
	if tool.Clock != nil {
		now = tool.Clock
	}
	utc := now().UTC()
	local := utc.In(location)
	result := CurrentTimeResult{
		UTC:         utc.Format(time.RFC3339Nano),
		Local:       local.Format(time.RFC3339Nano),
		TimeZone:    zone,
		Date:        local.Format("2006-01-02"),
		DayOfWeek:   local.Weekday().String(),
		UnixSeconds: utc.Unix(),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode current_time: %w", err)
	}
	return string(encoded), nil
}

func validateCurrentTimeZone(zone string) error {
	if strings.TrimSpace(zone) == "" {
		return errors.New("current_time timezone is required")
	}
	if len(zone) > MaxCurrentTimeZoneLen {
		return fmt.Errorf("current_time timezone must be at most %d bytes", MaxCurrentTimeZoneLen)
	}
	if strings.IndexByte(zone, 0) >= 0 || strings.IndexFunc(zone, unicode.IsControl) >= 0 {
		return errors.New("current_time timezone contains a control character")
	}
	if strings.HasPrefix(zone, "/") || strings.HasPrefix(zone, "\\") || strings.Contains(zone, "..") || strings.Contains(zone, "\\") {
		return errors.New("current_time timezone must be a safe IANA timezone name")
	}
	for _, r := range zone {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/_+-", r) {
			continue
		}
		return errors.New("current_time timezone must be a safe IANA timezone name")
	}
	return nil
}

var _ Tool = CurrentTimeTool{}
