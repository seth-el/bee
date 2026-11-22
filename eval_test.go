package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenScenarioFixturesLoadAndNonMCPScenariosMatch(t *testing.T) {
	paths, err := GoldenFixturePaths(filepath.Join("testdata", "eval"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 7 {
		t.Fatalf("fixture count = %d, want 7", len(paths))
	}
	for _, path := range paths {
		scenario, err := LoadGoldenScenario(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if scenario.MCP != nil {
			// The real bee-eval subprocess exercises this fixture in CI. A Go test
			// binary does not implement bee-eval's hidden MCP stub subcommand.
			continue
		}
		actual, err := RunGoldenScenario(context.Background(), scenario, "")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := CompareGoldenTrace(scenario.Expected, actual); err != nil {
			t.Fatalf("%s:\n%v", path, err)
		}
	}
}

func TestGoldenScenarioLoaderIsStrictAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"name":"bad","turns":[{"prompt":"x","responses":[{"content":"done"}]}],"expected":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGoldenScenario(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict loader error = %v", err)
	}
}

func TestUnifiedGoldenDiffShowsOnlyChangedWindow(t *testing.T) {
	diff := UnifiedGoldenDiff("[\n  one,\n  two,\n  three\n]", "[\n  one,\n  changed,\n  three\n]")
	for _, marker := range []string{"--- expected", "+++ actual", "-  two,", "+  changed,"} {
		if !strings.Contains(diff, marker) {
			t.Fatalf("diff missing %q:\n%s", marker, diff)
		}
	}
}

func TestGoldenRecurringFollowUpReportsAbsentDashboardWhenJobWritesNone(t *testing.T) {
	scenario, err := LoadGoldenScenario(filepath.Join("testdata", "eval", "06-recurring-job.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario.Recurring.FollowUp = &GoldenTurn{Prompt: "good morning", Responses: []GoldenProviderResponse{{Content: "morning"}}}
	actual, err := RunGoldenScenario(context.Background(), scenario, "")
	if err != nil {
		t.Fatal(err)
	}
	last := actual[len(actual)-1]
	if last.Kind != "next_session_prefix" || last.Outcome != "absent" {
		t.Fatalf("follow-up trace=%#v", actual)
	}
	scenario.Recurring.FollowUp = &GoldenTurn{Prompt: "good morning"}
	if err := scenario.Validate(); err == nil {
		t.Fatal("follow-up without responses accepted")
	}
}
