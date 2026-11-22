package bee

import (
	"strings"
	"testing"
)

func TestDeterministicObservationPreprocessPreservesSignalsAndContext(t *testing.T) {
	input := "header id=abc\nprogress: waiting\nprogress: waiting\nERROR exit code: 7\nERROR exit code: 7\ntrailer identifier=xyz"
	got := DeterministicObservationPreprocess(input)
	for _, want := range []string{"header id=abc", "ERROR exit code: 7", "trailer identifier=xyz"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preprocessed output missing %q: %q", want, got)
		}
	}
	if strings.Count(got, "progress: waiting") != 1 {
		t.Fatalf("boilerplate duplicate not compacted: %q", got)
	}
	if strings.Count(got, "ERROR exit code: 7") != 2 {
		t.Fatalf("important error was deduplicated: %q", got)
	}
}

func TestDeterministicObservationFallbackIsBoundedAndStable(t *testing.T) {
	input := strings.Repeat("noise ", 100) + "\nERROR exit code: 9\nidentifier=keep"
	first := DeterministicObservationFallback(input, 256)
	second := DeterministicObservationFallback(input, 256)
	if first != second {
		t.Fatal("fallback is not deterministic")
	}
	if len(first) > 256 {
		t.Fatalf("fallback length=%d", len(first))
	}
	if !strings.Contains(first, "exit code") {
		t.Fatalf("fallback lost error signal: %s", first)
	}
}

func TestDeterministicObservationPreprocessPreservesRepeatedOrdinaryEvents(t *testing.T) {
	input := "record=42\nstep=one\nrecord=42\nstep=two\nstep=three\nstep=four\nstep=five\nstep=six\nstep=seven"
	got := DeterministicObservationPreprocess(input)
	if strings.Count(got, "record=42") != 2 {
		t.Fatalf("ordinary repeated event was dropped: %q", got)
	}
}
