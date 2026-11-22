package bee

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecurringMissedOccurrencesDetectsAStoppedScheduler(t *testing.T) {
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	dream := RecurringJobConfig{ID: "dreaming", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: start, Cron: "0 3 * * *", TimeZone: "America/New_York"}}
	config := RecurringJobsConfig{Jobs: []RecurringJobConfig{dream, {ID: "off", Enabled: false, Schedule: dream.Schedule}}}
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC) // 10:00 EST
	nov14 := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	missed := config.MissedOccurrences([]Job{{RecurringID: "dreaming", Occurrence: nov14.Add(-24 * time.Hour)}}, now, time.Hour)
	if len(missed) != 1 || missed[0].JobID != "dreaming" || !missed[0].Occurrence.Equal(nov14) || !strings.Contains(missed[0].String(), "run-due") {
		t.Fatalf("missed = %#v", missed)
	}
	if got := config.MissedOccurrences([]Job{{RecurringID: "dreaming", Occurrence: nov14}}, now, time.Hour); len(got) != 0 {
		t.Fatalf("materialized occurrence still reported: %#v", got)
	}
	// Inside the grace period the newest occurrence is not reported yet.
	if got := config.MissedOccurrences(nil, nov14.Add(30*time.Minute), time.Hour); len(got) != 1 || !got[0].Occurrence.Equal(nov14.Add(-24*time.Hour)) {
		t.Fatalf("grace handling = %#v", got)
	}
	// Before the first occurrence there is nothing to miss.
	if got := config.MissedOccurrences(nil, start.Add(time.Hour), time.Hour); len(got) != 0 {
		t.Fatalf("before first occurrence = %#v", got)
	}
	interval := RecurringJobsConfig{Jobs: []RecurringJobConfig{{ID: "tick", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: start, IntervalSeconds: 600}}}}
	got := interval.MissedOccurrences(nil, start.Add(95*time.Minute), time.Hour)
	if len(got) != 1 || !got[0].Occurrence.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("interval missed = %#v", got)
	}
}

func TestDoctorRecurringSchedulerCheck(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	check := func(config RecurringJobsConfig) DoctorCheck {
		t.Helper()
		var checks []DoctorCheck
		addRecurringSchedulerCheck(config, storePath, now, func(name string, status CheckStatus, detail string) {
			checks = append(checks, DoctorCheck{Name: name, Status: status, Detail: detail})
		})
		if len(checks) != 1 || checks[0].Name != "recurring_scheduler" {
			t.Fatalf("checks = %#v", checks)
		}
		return checks[0]
	}
	if got := check(RecurringJobsConfig{}); got.Status != CheckPass {
		t.Fatalf("no jobs = %#v", got)
	}
	dream := RecurringJobConfig{ID: "dreaming", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: now.Add(-72 * time.Hour), Cron: "0 3 * * *", TimeZone: "America/New_York"}}
	if got := check(RecurringJobsConfig{Jobs: []RecurringJobConfig{dream}}); got.Status != CheckWarn || !strings.Contains(got.Detail, `"dreaming"`) {
		t.Fatalf("missed = %#v", got)
	}
	store, err := NewJobStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveLocked([]Job{{ID: "x", RecurringID: "dreaming", Occurrence: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)}}); err != nil {
		t.Fatal(err)
	}
	if got := check(RecurringJobsConfig{Jobs: []RecurringJobConfig{dream}}); got.Status != CheckPass {
		t.Fatalf("ran = %#v", got)
	}
}
