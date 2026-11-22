package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMoodSnapshotIncludesPrivateFileAndMissingIsAbsent(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got != "" {
		t.Fatalf("missing snapshot=%q", got)
	}
	if err := os.WriteFile(filepath.Join(root, DefaultMoodFileName), []byte("still warm after a kind exchange\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got != "still warm after a kind exchange" {
		t.Fatalf("snapshot=%q", got)
	}
}

func TestMoodSnapshotUnsafeOrInvalidFileIsAbsent(t *testing.T) {
	for _, test := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "permissions", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid utf8", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, DefaultMoodFileName)
			test.make(t, path)
			manager, err := NewMoodManager(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := manager.Snapshot(); got != "" {
				t.Fatalf("snapshot=%q", got)
			}
		})
	}
}

func TestAgentComposesParentMoodAndExcludesItFromChildren(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	const mood = "quietly encouraged"
	parent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{MoodContext: mood})
	if err != nil {
		t.Fatal(err)
	}
	if messages := parent.Messages(); len(messages) != 1 || !strings.Contains(messages[0].Content, "<bee-mood-context>\n"+mood+"\n</bee-mood-context>") {
		t.Fatalf("parent messages=%#v", messages)
	}

	childConfig := DefaultConfig()
	childConfig.ProviderRole = ProviderRoleChild
	child, err := NewAgentWithOptions(childConfig, &fakeProvider{}, AgentOptions{MoodContext: mood})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range child.Messages() {
		if strings.Contains(message.Content, mood) {
			t.Fatalf("child received mood context: %#v", child.Messages())
		}
	}
}

func TestAgentComposesMoodAppendProtocolWithoutBaseline(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{MoodEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) != 1 || !strings.Contains(messages[0].Content, moodDeltaPrefix) || strings.Contains(messages[0].Content, "<bee-mood-context>") {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestMoodSnapshotIsStableForOneSessionActivation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, DefaultMoodFileName)
	if err := os.WriteFile(path, []byte("first register"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Mood: manager}
	first := app.activateMoodSession(context.Background(), "session-a")
	if err := os.WriteFile(path, []byte("second register"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := app.activateMoodSession(context.Background(), "session-a"); got != first {
		t.Fatalf("same activation changed from %q to %q", first, got)
	}
	if got := app.activateMoodSession(context.Background(), "session-b"); got != "second register" {
		t.Fatalf("new activation snapshot=%q", got)
	}
	app.beginMoodSession("session-b")
	if err := os.WriteFile(path, []byte("third register"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := app.activateMoodSession(context.Background(), "session-b"); got != "third register" {
		t.Fatalf("explicit resume snapshot=%q", got)
	}
}

func TestMoodConsolidationOrdersOldestFirstAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	older := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	newer := older.Add(4 * time.Hour)
	contents := moodDeltaBlock(newer, MoodDecayMedium, "newer movement") + "\n" + moodDeltaBlock(older, MoodDecayShort, "older movement")
	if err := os.WriteFile(filepath.Join(root, "2026-10-28.md"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Consolidate(context.Background(), newer.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Merged != 2 || !report.Updated || report.ActiveEntries != 2 {
		t.Fatalf("report=%+v", report)
	}
	snapshot := manager.Snapshot()
	if olderIndex, newerIndex := strings.Index(snapshot, "older movement"), strings.Index(snapshot, "newer movement"); olderIndex < 0 || newerIndex < 0 || olderIndex >= newerIndex {
		t.Fatalf("snapshot order=%q", snapshot)
	}
	before, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Consolidate(context.Background(), newer.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	if second.Merged != 0 || second.Updated || !stringSlicesEqual(before, after) {
		t.Fatalf("second=%+v changed=%v", second, !stringSlicesEqual(before, after))
	}
}

func TestMoodActivationLeavesMidSessionDeltaInTailUntilNextSession(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	source := filepath.Join(root, "2026-10-29.md")
	if err := os.WriteFile(source, []byte(moodDeltaBlock(now.Add(-time.Hour), MoodDecayMedium, "first movement")), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &Application{Mood: manager, MoodClock: func() time.Time { return now.Add(time.Hour) }}
	first := app.activateMoodSession(context.Background(), "session-a")
	baseline, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + moodDeltaBlock(now, MoodDecayShort, "second movement")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if same := app.activateMoodSession(context.Background(), "session-a"); same != first {
		t.Fatalf("same session snapshot changed: %q", same)
	}
	unchanged, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !stringSlicesEqual(baseline, unchanged) {
		t.Fatal("mid-session append rewrote mood.md")
	}
	second := app.activateMoodSession(context.Background(), "session-b")
	if !strings.Contains(second, "first movement") || !strings.Contains(second, "second movement") {
		t.Fatalf("next session snapshot=%q", second)
	}
	mergedBaseline, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	app.beginMoodSession("session-c")
	third := app.activateMoodSession(context.Background(), "session-c")
	if third != second {
		t.Fatalf("idempotent activation changed snapshot: second=%q third=%q", second, third)
	}
	idempotentBaseline, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !stringSlicesEqual(mergedBaseline, idempotentBaseline) {
		t.Fatal("idempotent activation rewrote mood.md")
	}
}

func TestMoodActivationConsumesEveryNewSameSourceDeltaOldestFirst(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	source := filepath.Join(root, "2026-10-29.md")
	if err := os.WriteFile(source, []byte(moodDeltaBlock(now.Add(-3*time.Hour), MoodDecayMedium, "baseline movement")), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &Application{Mood: manager, MoodClock: func() time.Time { return now.Add(time.Hour) }}
	_ = app.activateMoodSession(context.Background(), "session-a")
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Append order and emotional time order deliberately differ. Consolidation
	// must consume both new blocks and sort by their timestamps, not cursor order.
	appended := "\n" + moodDeltaBlock(now.Add(30*time.Minute), MoodDecayMedium, "later movement") + "\n" + moodDeltaBlock(now.Add(-30*time.Minute), MoodDecayMedium, "earlier movement")
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := app.activateMoodSession(context.Background(), "session-b")
	baselineIndex := strings.Index(snapshot, "baseline movement")
	earlierIndex := strings.Index(snapshot, "earlier movement")
	laterIndex := strings.Index(snapshot, "later movement")
	if baselineIndex < 0 || earlierIndex < 0 || laterIndex < 0 || !(baselineIndex < earlierIndex && earlierIndex < laterIndex) {
		t.Fatalf("snapshot order=%q", snapshot)
	}
}

func TestCorruptManagedMoodComposesAbsent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, DefaultMoodFileName)
	if err := os.WriteFile(path, []byte("# Mood\n\n"+moodManagedStart+"\nnot-state\n"+moodManagedEnd+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot(); got != "" {
		t.Fatalf("corrupt snapshot=%q", got)
	}
}

func TestMoodSweepRetriesIncompleteDeltaAfterItIsCompleted(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(root, "mood-deltas.md")
	partial := `<!-- bee:mood-delta at="` + now.Format(time.RFC3339) + `" decay="short" -->` + "\nunfinished but recoverable"
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Consolidate(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Merged != 0 || first.Updated {
		t.Fatalf("partial report=%+v", first)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + moodDeltaClose + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Consolidate(context.Background(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Merged != 1 || !second.Updated || !strings.Contains(manager.Snapshot(), "unfinished but recoverable") {
		t.Fatalf("completed report=%+v snapshot=%q", second, manager.Snapshot())
	}
}

func TestMoodConsolidationAppliesNaturalDecay(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewMoodManager(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	deltas := strings.Join([]string{
		moodDeltaBlock(now.Add(-5*time.Hour), MoodDecayFleeting, "fresh fleeting movement"),
		moodDeltaBlock(now.Add(-49*time.Hour), MoodDecayShort, "softened short movement"),
		moodDeltaBlock(now.Add(-29*24*time.Hour), MoodDecayMedium, "expired medium movement"),
		moodDeltaBlock(now.Add(-61*24*time.Hour), MoodDecayLong, "softened long movement"),
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "mood-deltas.md"), []byte(deltas), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Consolidate(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Merged != 4 || report.Softened != 2 || report.Dropped != 1 || report.ActiveEntries != 3 || !report.Updated {
		t.Fatalf("report=%+v", report)
	}
	snapshot := manager.Snapshot()
	if !strings.Contains(snapshot, "- fresh fleeting movement") || strings.Contains(snapshot, "Fading, but still present: fresh fleeting movement") {
		t.Fatalf("fresh movement rendered incorrectly: %q", snapshot)
	}
	for _, text := range []string{"softened short movement", "softened long movement"} {
		if !strings.Contains(snapshot, "Fading, but still present: "+text) {
			t.Fatalf("softened movement %q missing from %q", text, snapshot)
		}
	}
	if strings.Contains(snapshot, "expired medium movement") {
		t.Fatalf("expired movement remained in %q", snapshot)
	}
	second, err := manager.Consolidate(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Merged != 0 || second.Dropped != 0 || second.Softened != 2 || second.Updated {
		t.Fatalf("idempotent decay report=%+v", second)
	}
}

func moodDeltaBlock(at time.Time, decay MoodDecay, text string) string {
	return `<!-- bee:mood-delta at="` + at.UTC().Format(time.RFC3339) + `" decay="` + string(decay) + `" -->` + "\n" + text + "\n" + moodDeltaClose
}

func stringSlicesEqual(left, right []byte) bool {
	return string(left) == string(right)
}
