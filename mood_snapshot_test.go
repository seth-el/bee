package bee

import (
	"strings"
	"testing"
	"time"
)

func TestMoodSnapshotShowsOnlyRecentEntries(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var entries []moodEntry
	for index := 0; index < 30; index++ {
		entries = append(entries, moodEntry{ID: string(rune('a' + index)), At: base.Add(time.Duration(index) * time.Hour), Decay: MoodDecayLong, Text: "movement " + strings.Repeat("x", 300) + " #" + string(rune('A'+index))})
	}
	shown, older := recentMoodEntries(entries)
	if len(shown)+older != len(entries) || len(shown) > MaxMoodSnapshotEntries || len(shown) < 2 {
		t.Fatalf("shown %d older %d", len(shown), older)
	}
	if shown[len(shown)-1].ID != entries[len(entries)-1].ID || shown[0].At.After(shown[len(shown)-1].At) {
		t.Fatal("snapshot does not end with the newest entry in oldest-first order")
	}
	snapshot := renderMoodSnapshot("", entries)
	if len(snapshot) > MaxMoodSnapshotBytes+256 {
		t.Fatalf("snapshot is %d bytes", len(snapshot))
	}
	if strings.Contains(snapshot, "#A") || !strings.Contains(snapshot, "older movements are not shown") {
		t.Fatalf("snapshot kept the oldest entry or lacks the note:\n%s", snapshot)
	}
	// One oversized entry is still shown, and a short list is shown whole.
	if shown, older := recentMoodEntries([]moodEntry{{Text: strings.Repeat("y", 2*MaxMoodSnapshotBytes)}}); len(shown) != 1 || older != 0 {
		t.Fatalf("single large entry: shown %d older %d", len(shown), older)
	}
	if snapshot := renderMoodSnapshot("", entries[:3]); strings.Contains(snapshot, "not shown") {
		t.Fatal("short snapshot claims omitted entries")
	}
	// The stored document keeps every entry.
	document, err := renderMoodDocument("", moodState{Version: 1, Entries: entries})
	if err != nil || !strings.Contains(string(document), "#A") {
		t.Fatalf("document dropped an entry: %v", err)
	}
}

func TestMoodLongEntriesBeyondBudgetDecayAsMedium(t *testing.T) {
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	var entries []moodEntry
	// Twelve long entries, all 30 days old: past the medium lifetime (28 days),
	// well inside the long half-life (60 days).
	for index := 0; index < 12; index++ {
		entries = append(entries, moodEntry{ID: string(rune('a' + index)), At: now.Add(-30*24*time.Hour + time.Duration(index)*time.Minute), Decay: MoodDecayLong, Text: "m"})
	}
	active, softened, dropped := applyMoodDecay(entries, now)
	if len(active) != MaxMoodLongEntries || dropped != 12-MaxMoodLongEntries || softened != 0 {
		t.Fatalf("active %d softened %d dropped %d", len(active), softened, dropped)
	}
	if active[0].ID != entries[12-MaxMoodLongEntries].ID {
		t.Fatalf("kept %q, want the newest long entries", active[0].ID)
	}
	// Within the budget nothing changes.
	if active, _, dropped := applyMoodDecay(entries[:MaxMoodLongEntries], now); len(active) != MaxMoodLongEntries || dropped != 0 {
		t.Fatalf("budgeted long entries decayed: %d kept, %d dropped", len(active), dropped)
	}
}
