package bee

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTUIMastheadRendersDetailsAndRunState(t *testing.T) {
	activity := newTUIActivity()
	lines := activity.renderedMasthead(80, tuiMastheadInfo{model: "local-model", profile: "default profile", session: "session-1", scope: "/work/project"})
	if len(lines) != tuiMastheadRows {
		t.Fatalf("rows=%d want %d", len(lines), tuiMastheadRows)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"local-model · default profile", "session-1", "/work/project", "● ready"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("masthead lacks %q:\n%s", want, joined)
		}
	}
	for _, line := range lines {
		if width := tuiDisplayWidth(stripMastheadANSI(line)); width != 80 {
			t.Fatalf("line width=%d: %q", width, stripMastheadANSI(line))
		}
	}
	if activity.renderedMasthead(tuiMastheadMinWidth-1, tuiMastheadInfo{}) != nil {
		t.Fatal("masthead should be hidden on narrow terminals")
	}
}

func TestTUIActivityHoldsOutcomeThenReturnsToReady(t *testing.T) {
	now := time.Unix(100, 0)
	activity := newTUIActivity()
	activity.beginRun(now)
	if activity.label() != "working" {
		t.Fatalf("label=%q", activity.label())
	}
	activity.finishRun(now)
	if activity.label() != "done" || activity.advance(now) {
		t.Fatalf("label=%q", activity.label())
	}
	if !activity.advance(now.Add(tuiActivityMoment)) || activity.label() != "ready" {
		t.Fatalf("label=%q after hold", activity.label())
	}
	activity.beginRun(now)
	activity.failRun(now)
	activity.finishRun(now)
	if activity.label() != "error" {
		t.Fatalf("failed run label=%q", activity.label())
	}
	activity.beginRun(now)
	activity.cancelRun(now)
	activity.finishRun(now)
	if activity.label() != "ready" {
		t.Fatalf("cancelled run label=%q", activity.label())
	}
}

var mastheadANSI = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripMastheadANSI(value string) string {
	return mastheadANSI.ReplaceAllString(value, "")
}
