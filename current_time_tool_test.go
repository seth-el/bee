package bee

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCurrentTimeToolFixedClockAndTimezone(t *testing.T) {
	fixed := time.Date(2026, time.March, 8, 6, 30, 0, 123000000, time.UTC)
	tool := CurrentTimeTool{Clock: func() time.Time { return fixed }, DefaultTimeZone: "America/New_York"}
	encoded, err := tool.Execute(context.Background(), []byte(`{"timezone":"America/New_York"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result CurrentTimeResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.UTC != "2026-03-08T06:30:00.123Z" || result.Local != "2026-03-08T01:30:00.123-05:00" || result.Date != "2026-03-08" || result.DayOfWeek != "Sunday" || result.UnixSeconds != fixed.Unix() {
		t.Fatalf("result=%+v", result)
	}
}

func TestCurrentTimeToolDefaultAndInvalidZone(t *testing.T) {
	tool := CurrentTimeTool{Clock: func() time.Time { return time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC) }, DefaultTimeZone: "UTC"}
	if _, err := tool.Execute(context.Background(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"timezone":"Not/AZone"}`)); err == nil {
		t.Fatal("invalid timezone unexpectedly accepted")
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"timezone":"`+strings.Repeat("x", MaxCurrentTimeZoneLen+1)+`"}`)); err == nil {
		t.Fatal("oversized timezone unexpectedly accepted")
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"timezone":"../../etc/passwd"}`)); err == nil {
		t.Fatal("unsafe timezone unexpectedly accepted")
	}
}

func TestCurrentTimeToolDSTTransition(t *testing.T) {
	tool := CurrentTimeTool{Clock: func() time.Time { return time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC) }, DefaultTimeZone: "America/New_York"}
	encoded, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result CurrentTimeResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.Local != "2026-03-08T03:30:00-04:00" {
		t.Fatalf("local=%q", result.Local)
	}
}

func TestCurrentTimeToolRegistryAndSchema(t *testing.T) {
	registry, err := NewRegistry(CurrentTimeTool{})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Lookup(CurrentTimeToolName)
	if !ok || tool.Definition().Mutability != ToolMutabilityReadOnly {
		t.Fatalf("tool=%#v", tool)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Definition().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema=%v", schema)
	}
}

func TestComposeGeneralAssistantWiresConfiguredCurrentTimeZone(t *testing.T) {
	config := DefaultConfig()
	config.Web.TimeZone = "America/Los_Angeles"
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// current_time is pruned from the foreground: every turn carries a clock
	// record. The tool itself (and its time-zone handling) is tested directly.
	if _, ok := app.Executor.Registry.Lookup(CurrentTimeToolName); ok {
		t.Fatal("current_time is still model-facing")
	}
	registered := Tool(&CurrentTimeTool{DefaultTimeZone: config.Web.EffectiveTimeZone()})
	tool, ok := registered.(*CurrentTimeTool)
	if !ok {
		t.Fatalf("current_time tool type=%T", registered)
	}
	tool.Clock = func() time.Time { return time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC) }
	encoded, err := tool.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result CurrentTimeResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.TimeZone != "America/Los_Angeles" || result.Local != "2026-07-01T05:00:00-07:00" {
		t.Fatalf("result=%+v", result)
	}
}
