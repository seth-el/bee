package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func recurringFixture(t *testing.T, start time.Time) (RecurringJobsConfig, *JobStore) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := RecurringJobsConfig{Jobs: []RecurringJobConfig{{
		ID: "maintenance", Enabled: true, Prompt: "inspect the workspace",
		Schedule: RecurringScheduleConfig{StartAt: start.UTC(), IntervalSeconds: 60, TimeZone: "UTC"},
		Contract: JobContract{
			AllowedTools: []string{"read_file"}, WorkspacePaths: []string{workspace},
			MaxWallClockSeconds: 30, MaxToolCalls: 4,
			OutputTarget: RecurringOutputSession, NotificationTarget: RecurringNotificationInbox,
		},
		MaxCatchUp: 3, CatchUpWindowSecs: 3600, ConcurrencyPolicy: RecurringConcurrencyForbid,
	}}}
	return config, store
}

func TestRecurringSchedulerMaterializesBoundedCatchUpIdempotently(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	now := start.Add(5 * time.Minute)
	created, err := scheduler.Materialize(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 3 || !created[0].Occurrence.Equal(start) || !created[2].Occurrence.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("created occurrences = %#v", created)
	}
	if repeated, err := scheduler.Materialize(now); err != nil || len(repeated) != 0 {
		t.Fatalf("repeated materialization = %#v, %v", repeated, err)
	}
	sessions := []SessionID{"session-a", "session-b", "session-c"}
	index := 0
	results, err := scheduler.Tick(context.Background(), now, func(_ context.Context, job Job) (JobExecution, error) {
		if job.Request.SessionID != "" {
			t.Fatalf("recurring firing reused session %q", job.Request.SessionID)
		}
		result := JobExecution{SessionID: sessions[index], Summary: "journaled"}
		index++
		return result, nil
	}, nil)
	if err != nil || len(results) != 3 {
		t.Fatalf("tick results = %#v, %v", results, err)
	}
	for position, result := range results {
		if result.State != JobSucceeded || len(result.AttemptsLog) != 1 || result.AttemptsLog[0].SessionID != sessions[position] {
			t.Fatalf("result[%d] = %#v", position, result)
		}
	}
	next, err := scheduler.Materialize(now)
	if err != nil || len(next) != 3 || !next[0].Occurrence.Equal(start.Add(3*time.Minute)) || !next[2].Occurrence.Equal(now) {
		t.Fatalf("next catch-up = %#v, %v", next, err)
	}
}

func TestRecurringSchedulerForbidsOverlappingSameDefinition(t *testing.T) {
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].MaxCatchUp = 2
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	now := start.Add(time.Minute)
	if _, err := scheduler.Materialize(now); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var concurrent atomic.Int32
	var peak atomic.Int32
	execute := func(context.Context, Job) (JobExecution, error) {
		call := calls.Add(1)
		active := concurrent.Add(1)
		if active > peak.Load() {
			peak.Store(active)
		}
		if call == 1 {
			close(started)
			<-release
		}
		concurrent.Add(-1)
		return JobExecution{}, nil
	}
	var firstErr error
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_, firstErr = store.RunDueDetailedWithNotifier(context.Background(), now, execute, nil)
	}()
	<-started
	second, secondErr := store.RunDueDetailedWithNotifier(context.Background(), now, execute, nil)
	if secondErr != nil || len(second) != 0 {
		t.Fatalf("overlapping scheduler results = %#v, %v", second, secondErr)
	}
	close(release)
	wait.Wait()
	if firstErr != nil || calls.Load() != 2 || peak.Load() != 1 {
		t.Fatalf("firstErr=%v calls=%d peak=%d", firstErr, calls.Load(), peak.Load())
	}
}

func TestRecurringApprovalPauseCreatesDurableNotification(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].MaxCatchUp = 1
	notifications, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := scheduler.Tick(context.Background(), start, func(context.Context, Job) (JobExecution, error) {
		return JobExecution{SessionID: "approval-session"}, ErrJobApprovalRequired
	}, NewJobNotificationNotifier(notifications))
	if err != nil || len(results) != 1 || results[0].State != JobPaused {
		t.Fatalf("paused results = %#v, %v", results, err)
	}
	if results[0].AttemptsLog[0].SessionID != "approval-session" || results[0].LastError != ErrJobApprovalRequired.Error() {
		t.Fatalf("paused attempt = %#v", results[0])
	}
	inbox, err := notifications.List(context.Background(), 10)
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].State != NotificationStatePaused {
		t.Fatalf("notification = %#v, %v", inbox, err)
	}
}

func TestRecurringBudgetExhaustionFailsAndNotifies(t *testing.T) {
	start := time.Date(2026, 3, 5, 5, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].MaxCatchUp = 1
	notifications, err := NewNotificationInboxStore(filepath.Join(t.TempDir(), "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	results, err := scheduler.Tick(context.Background(), start, func(context.Context, Job) (JobExecution, error) {
		return JobExecution{SessionID: "budget-session"}, errors.New("recurring job exceeded its wall-clock budget")
	}, NewJobNotificationNotifier(notifications))
	if err != nil || len(results) != 1 || results[0].State != JobFailed {
		t.Fatalf("budget results = %#v, %v", results, err)
	}
	if results[0].AttemptsLog[0].SessionID != "budget-session" {
		t.Fatalf("budget attempt = %#v", results[0].AttemptsLog)
	}
	inbox, err := notifications.List(context.Background(), 10)
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].State != NotificationStateFailed {
		t.Fatalf("budget notification = %#v, %v", inbox, err)
	}
}

func TestDetailedJobRetriesPreserveEachAttemptSession(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.retryPolicy = RetryPolicy{Retryable: func(error) bool { return true }, Sleep: func(context.Context, time.Duration) error { return nil }}
	now := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	job := Job{ID: "detailed", Name: "detailed", Owner: "owner", IdempotencyKey: "detailed", Schedule: Schedule{RunAt: now}, Request: JobRequest{Prompt: "run"}, MaxAttempts: 2}
	if err := store.Create(job, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve(job.ID, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	results, err := store.RunDueDetailedWithNotifier(context.Background(), now, func(context.Context, Job) (JobExecution, error) {
		calls++
		if calls == 1 {
			return JobExecution{SessionID: "first-session"}, errors.New("retry")
		}
		return JobExecution{SessionID: "second-session"}, nil
	}, nil)
	if err != nil || len(results) != 1 || len(results[0].AttemptsLog) != 2 {
		t.Fatalf("detailed retry = %#v, %v", results, err)
	}
	if results[0].AttemptsLog[0].SessionID != "first-session" || results[0].AttemptsLog[1].SessionID != "second-session" {
		t.Fatalf("attempt sessions = %#v", results[0].AttemptsLog)
	}
}

func TestRecurringCronAndContractSnapshotAreDeterministic(t *testing.T) {
	start := time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].Schedule = RecurringScheduleConfig{StartAt: start, Cron: "*/15 * * * *", TimeZone: "UTC"}
	config.Jobs[0].MaxCatchUp = 2
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	config.Jobs[0].Contract.AllowedTools[0] = "write_file"
	created, err := scheduler.Materialize(start.Add(50 * time.Minute))
	if err != nil || len(created) != 2 {
		t.Fatalf("cron materialization = %#v, %v", created, err)
	}
	if !created[0].Occurrence.Equal(start) || !created[1].Occurrence.Equal(start.Add(15*time.Minute)) {
		t.Fatalf("cron occurrences = %v, %v", created[0].Occurrence, created[1].Occurrence)
	}
	if created[0].Contract == nil || created[0].Contract.AllowedTools[0] != "read_file" {
		t.Fatalf("contract was not snapshotted: %#v", created[0].Contract)
	}
}

func TestRecurringConfigValidationAndStrictLoading(t *testing.T) {
	start := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)
	config, _ := recurringFixture(t, start)
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := cloneRecurringJobsConfig(config)
	invalid.Jobs[0].Contract.NotificationTarget = "arbitrary-webhook"
	if err := invalid.Validate(); err == nil {
		t.Fatal("accepted unsupported notification target")
	}
	invalid = cloneRecurringJobsConfig(config)
	invalid.Jobs[0].Schedule.Cron = "* * * * *"
	if err := invalid.Validate(); err == nil {
		t.Fatal("accepted interval and cron together")
	}
	root := t.TempDir()
	document := map[string]any{
		"provider":       map[string]any{"type": "openai-compatible", "base_url": "http://127.0.0.1:8081", "model": "local", "timeout_seconds": 0},
		"generation":     map[string]any{"temperature": 0.2, "max_tokens": 1024},
		"recurring_jobs": config,
	}
	data, _ := json.Marshal(document)
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || len(loaded.RecurringJobs.Jobs) != 1 {
		t.Fatalf("loaded recurring jobs = %#v, %v", loaded.RecurringJobs, err)
	}
}

func TestRestrictToolsIsExactAndRejectsPartialInternalGroups(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(ReadFileTool{Scope: ExecutionScope{Root: root}}, WriteFileTool{Scope: ExecutionScope{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Executor: &Executor{Registry: registry}, ProviderRole: ProviderRoleMain}
	if err := app.RestrictTools([]string{"read_file"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup("read_file"); !ok {
		t.Fatal("allowed tool was removed")
	}
	if _, ok := registry.Lookup("write_file"); ok {
		t.Fatal("unlisted tool survived restriction")
	}
	registry, _ = NewRegistry(ReadFileTool{Scope: ExecutionScope{Root: root}})
	app = &Application{Executor: &Executor{Registry: registry}, ProviderRole: ProviderRoleMain}
	if err := app.RestrictTools([]string{PlanToolName}); err == nil {
		t.Fatal("accepted partial planning-tool group")
	}
	requiredRegistry, _ := NewRegistry(ReadFileTool{Scope: ExecutionScope{Root: root}})
	required := &Application{Executor: &Executor{Registry: requiredRegistry}, ProviderRole: ProviderRoleMain, RequirePlan: true}
	if err := required.RestrictTools([]string{"read_file"}); err == nil {
		t.Fatal("required plan mode accepted an allowlist without planning tools")
	}
}

func TestRecurringContractReadOnlyPathsValidation(t *testing.T) {
	start := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)
	config, _ := recurringFixture(t, start)
	workspace := config.Jobs[0].Contract.WorkspacePaths[0]
	transcripts := t.TempDir()

	valid := cloneRecurringJobsConfig(config)
	valid.Jobs[0].Contract.ReadOnlyPaths = []string{transcripts}
	if err := valid.Validate(); err != nil {
		t.Fatalf("disjoint read-only path rejected: %v", err)
	}
	for name, paths := range map[string][]string{
		"relative":         {"sessions"},
		"root":             {"/"},
		"duplicate":        {transcripts, transcripts + "/"},
		"equals workspace": {workspace},
		"inside workspace": {filepath.Join(workspace, "sessions")},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cloneRecurringJobsConfig(config)
			invalid.Jobs[0].Contract.ReadOnlyPaths = paths
			if err := invalid.Validate(); err == nil {
				t.Fatalf("accepted read_only_paths %v", paths)
			}
		})
	}
	// A write root nested inside a read-only root is coherent: read all of
	// memory/, write only memory/topics/.
	nested := cloneRecurringJobsConfig(config)
	nested.Jobs[0].Contract.ReadOnlyPaths = []string{filepath.Dir(workspace)}
	if err := nested.Validate(); err != nil {
		t.Fatalf("read-only root containing the write root rejected: %v", err)
	}
	tooMany := cloneRecurringJobsConfig(config)
	for index := 0; index <= maxRecurringWorkspacePaths; index++ {
		tooMany.Jobs[0].Contract.ReadOnlyPaths = append(tooMany.Jobs[0].Contract.ReadOnlyPaths, filepath.Join(transcripts, string(rune('a'+index))))
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("accepted unbounded read_only_paths")
	}
}

func TestRecurringContractSnapshotsReadOnlyPaths(t *testing.T) {
	start := time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	transcripts := t.TempDir()
	config.Jobs[0].Contract.ReadOnlyPaths = []string{transcripts}
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	config.Jobs[0].Contract.ReadOnlyPaths[0] = "/widened"
	created, err := scheduler.Materialize(start)
	if err != nil || len(created) != 1 {
		t.Fatalf("materialize = %#v, %v", created, err)
	}
	if got := created[0].Contract.ReadOnlyPaths; len(got) != 1 || got[0] != transcripts {
		t.Fatalf("read-only paths were not snapshotted: %#v", got)
	}
	jobs, err := store.List()
	if err != nil || len(jobs) != 1 || jobs[0].Contract == nil || len(jobs[0].Contract.ReadOnlyPaths) != 1 || jobs[0].Contract.ReadOnlyPaths[0] != transcripts {
		t.Fatalf("persisted contract = %#v, %v", jobs, err)
	}
}

// A read-only contract root is a readable scope root but never a write root:
// reads succeed and every write path fails closed without touching the file.
func TestReadOnlyScopeRootIsReadableButNeverWritable(t *testing.T) {
	workspace := t.TempDir()
	transcripts := t.TempDir()
	transcript := filepath.Join(transcripts, "session.jsonl")
	if err := os.WriteFile(transcript, []byte("ground truth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store: mustAssistantStore(t), ScopeRoots: []string{workspace, transcripts}, WriteRoots: []string{workspace},
		MemoryRoot: filepath.Join(workspace, "memory"), Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	read, err := app.Executor.Execute(context.Background(), "read_file", json.RawMessage(`{"path":`+strconvQuote(transcript)+`}`))
	if err != nil || !strings.Contains(read.Output, "ground truth") {
		t.Fatalf("read-only root read = %#v, %v", read, err)
	}
	for _, args := range []string{
		`{"path":` + strconvQuote(transcript) + `,"content":"rewritten","operation":"replace"}`,
		`{"path":` + strconvQuote(transcript) + `,"content":"appended","operation":"append"}`,
		`{"path":` + strconvQuote(filepath.Join(transcripts, "new.md")) + `,"content":"planted","operation":"create"}`,
	} {
		if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(args)); err == nil {
			t.Fatalf("write into read-only root succeeded: %s", args)
		}
	}
	data, err := os.ReadFile(transcript)
	if err != nil || string(data) != "ground truth\n" {
		t.Fatalf("read-only transcript changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(transcripts, "new.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write planted a file in the read-only root: %v", err)
	}
	if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":`+strconvQuote(filepath.Join(workspace, "ok.md"))+`,"content":"fine","operation":"create"}`)); err != nil {
		t.Fatalf("write root write failed: %v", err)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestRecurringScheduleWarningsFlagDaylightSavingGapsAndRepeats(t *testing.T) {
	from := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	job := func(cron, zone string) RecurringJobConfig {
		return RecurringJobConfig{ID: "dream", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: from, Cron: cron, TimeZone: zone}}
	}
	for name, test := range map[string]struct {
		cron, zone string
		want       map[string]string // kind -> local date
	}{
		"2am skipped on spring forward":     {"0 2 * * *", "America/New_York", map[string]string{"skipped": "2027-03-14"}},
		"2:30 skipped":                      {"30 2 * * *", "America/New_York", map[string]string{"skipped": "2027-03-14"}},
		"1:30 repeats on fall back":         {"30 1 * * *", "America/New_York", map[string]string{"repeated": "2027-11-07"}},
		"3am is safe":                       {"0 3 * * *", "America/New_York", nil},
		"utc never shifts":                  {"0 2 * * *", "UTC", nil},
		"empty zone is utc":                 {"0 2 * * *", "", nil},
		"weekday that misses the change":    {"0 2 * * 1", "America/New_York", nil},
		"hourly both":                       {"15 * * * *", "America/New_York", map[string]string{"skipped": "2027-03-14", "repeated": "2027-11-07"}},
		"southern hemisphere":               {"30 2 * * *", "Australia/Sydney", map[string]string{"repeated": "2027-04-04", "skipped": "2027-10-03"}},
		"day-of-month restricted, matching": {"0 2 14 3 *", "America/New_York", map[string]string{"skipped": "2027-03-14"}},
	} {
		t.Run(name, func(t *testing.T) {
			warnings := job(test.cron, test.zone).ScheduleWarnings(from)
			got := map[string]string{}
			for _, warning := range warnings {
				got[warning.Kind] = warning.LocalDate
				if warning.JobID != "dream" || warning.Cron != test.cron || !strings.Contains(warning.String(), "dream") {
					t.Fatalf("warning fields=%#v", warning)
				}
			}
			if len(got) != len(test.want) {
				t.Fatalf("warnings=%v, want %v", warnings, test.want)
			}
			for kind, date := range test.want {
				if got[kind] != date {
					t.Fatalf("%s date=%q, want %q (all=%v)", kind, got[kind], date, warnings)
				}
			}
		})
	}
	interval := RecurringJobConfig{ID: "tick", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: from, IntervalSeconds: 3600, TimeZone: "America/New_York"}}
	if warnings := interval.ScheduleWarnings(from); len(warnings) != 0 {
		t.Fatalf("interval schedule warned: %v", warnings)
	}
	disabled := RecurringJobsConfig{Jobs: []RecurringJobConfig{job("0 2 * * *", "America/New_York")}}
	disabled.Jobs[0].Enabled = false
	if warnings := disabled.ScheduleWarnings(from); len(warnings) != 0 {
		t.Fatalf("disabled job warned: %v", warnings)
	}
}

// The lint must describe real scheduler behavior, not just zone arithmetic.
func TestRecurringScheduleWarningsMatchMaterializedOccurrences(t *testing.T) {
	start := time.Date(2027, 3, 13, 0, 0, 0, 0, time.UTC)
	skipped := RecurringJobConfig{ID: "dream", Enabled: true, MaxCatchUp: 32, CatchUpWindowSecs: 3 * 24 * 3600, Schedule: RecurringScheduleConfig{StartAt: start, Cron: "0 2 * * *", TimeZone: "America/New_York"}}
	occurrences, err := skipped.dueOccurrences(time.Time{}, start.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("America/New_York")
	for _, occurrence := range occurrences {
		if local := occurrence.In(location); local.Day() == 14 {
			t.Fatalf("2am fired on the spring-forward day at %v", local)
		}
	}
	if len(occurrences) != 2 {
		t.Fatalf("occurrences=%v, want the 13th and 15th only", occurrences)
	}
	fallStart := time.Date(2027, 11, 6, 0, 0, 0, 0, time.UTC)
	repeated := skipped
	repeated.Schedule = RecurringScheduleConfig{StartAt: fallStart, Cron: "30 1 7 11 *", TimeZone: "America/New_York"}
	occurrences, err = repeated.dueOccurrences(time.Time{}, fallStart.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(occurrences) != 2 {
		t.Fatalf("fall-back 01:30 occurrences=%v, want two", occurrences)
	}
}

func TestDoctorRecurringScheduleCheckWarnsOnDaylightSavingGap(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	check := func(config RecurringJobsConfig) DoctorCheck {
		t.Helper()
		var checks []DoctorCheck
		addRecurringScheduleCheck(config, now, func(name string, status CheckStatus, detail string) {
			checks = append(checks, DoctorCheck{Name: name, Status: status, Detail: detail})
		})
		if len(checks) != 1 || checks[0].Name != "recurring_schedules" {
			t.Fatalf("checks=%#v", checks)
		}
		return checks[0]
	}
	dream := RecurringJobConfig{ID: "dream", Enabled: true, Schedule: RecurringScheduleConfig{StartAt: now, Cron: "0 2 * * *", TimeZone: "America/New_York"}}
	if got := check(RecurringJobsConfig{Jobs: []RecurringJobConfig{dream}}); got.Status != CheckWarn || !strings.Contains(got.Detail, "2027-03-14") || !strings.Contains(got.Detail, `"dream"`) {
		t.Fatalf("2am check=%#v", got)
	}
	dream.Schedule.Cron = "0 3 * * *"
	if got := check(RecurringJobsConfig{Jobs: []RecurringJobConfig{dream}}); got.Status != CheckPass {
		t.Fatalf("3am check=%#v", got)
	}
	if got := check(RecurringJobsConfig{}); got.Status != CheckPass {
		t.Fatalf("no jobs check=%#v", got)
	}
}

// The dreamer's shape: memory/ is readable, only memory/topics/ and
// memory/dashboard/ are writable, so dated daily notes are structurally
// out of the job's reach.
func TestNestedWriteRootsInsideReadOnlyMemoryRoot(t *testing.T) {
	memory := t.TempDir()
	if err := os.Chmod(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	topics := filepath.Join(memory, "topics")
	dashboard := filepath.Join(memory, DefaultMemoryDashboardDir)
	for _, dir := range []string{topics, dashboard} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	daily := filepath.Join(memory, "2026-11-11.md")
	if err := os.WriteFile(daily, []byte("what happened\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := JobContract{WorkspacePaths: []string{topics, dashboard}, ReadOnlyPaths: []string{memory}, MaxWallClockSeconds: 60, MaxToolCalls: 8, OutputTarget: RecurringOutputSession, NotificationTarget: RecurringNotificationInbox}
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store: mustAssistantStore(t), ScopeRoots: []string{topics, dashboard, memory}, WriteRoots: []string{topics, dashboard},
		MemoryRoot: memory, Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(name, args string) error {
		_, err := app.Executor.Execute(context.Background(), name, json.RawMessage(args))
		return err
	}
	if err := execute("read_file", `{"path":`+strconvQuote(daily)+`}`); err != nil {
		t.Fatalf("daily not readable: %v", err)
	}
	for _, args := range []string{
		`{"path":` + strconvQuote(daily) + `,"content":"rewritten","operation":"replace"}`,
		`{"path":` + strconvQuote(daily) + `,"content":"appended","operation":"append"}`,
		`{"path":` + strconvQuote(filepath.Join(memory, "mood.md")) + `,"content":"planted","operation":"create"}`,
	} {
		if err := execute("write_file", args); err == nil {
			t.Fatalf("write outside topics/dashboard succeeded: %s", args)
		}
	}
	if data, err := os.ReadFile(daily); err != nil || string(data) != "what happened\n" {
		t.Fatalf("daily changed: %q, %v", data, err)
	}
	for _, path := range []string{filepath.Join(topics, "infra.md"), filepath.Join(dashboard, DefaultMemoryDashboardFileName)} {
		if err := execute("write_file", `{"path":`+strconvQuote(path)+`,"content":"curated","operation":"create"}`); err != nil {
			t.Fatalf("write to %s failed: %v", path, err)
		}
	}
}

// Pins the claim the dreaming prompt makes: a write outside the contract's
// write roots is refused AND surfaces as an unapproved RunApproval event,
// which executeRecurringJob turns into ErrJobApprovalRequired (pause +
// inbox notification). If this ever stops emitting, the run would carry on
// silently with only a tool error.
func TestRecurringWriteOutsideWriteRootsSurfacesAsUnapprovedApproval(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	memory := t.TempDir()
	if err := os.Chmod(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	topics := filepath.Join(memory, "topics")
	if err := os.MkdirAll(topics, 0o700); err != nil {
		t.Fatal(err)
	}
	daily := filepath.Join(memory, "2026-11-11.md")
	if err := os.WriteFile(daily, []byte("what happened\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store: mustAssistantStore(t), ScopeRoots: []string{topics, memory}, WriteRoots: []string{topics},
		MemoryRoot: memory, Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := `{"path":` + strconvQuote(daily) + `,"content":"rewritten","operation":"replace"}`
	app.Provider = &goldenScriptedProvider{responses: []GoldenProviderResponse{
		{ToolCalls: []ToolCall{{ID: "w1", Type: "function", Name: "write_file", Arguments: args}}},
		{Content: "done"},
	}}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "consolidate", DisableCommands: true})
	if err != nil {
		t.Fatal(err)
	}
	unapproved := false
	for event := range events {
		if event.Type == RunApproval && event.Approval != nil && event.Approval.EffectiveStatus() != ApprovalStatusApproved {
			unapproved = true
		}
	}
	if !unapproved {
		t.Fatal("out-of-scope write did not surface as an unapproved approval event")
	}
	if data, err := os.ReadFile(daily); err != nil || string(data) != "what happened\n" {
		t.Fatalf("daily changed: %q, %v", data, err)
	}
}

// The documented dreaming allowlist must name real tools exactly, and
// restricting to it must leave nothing else (no exec, terminal, plugins).
func TestDreamingAllowlistIsExactAndExclusive(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	root := t.TempDir()
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"conversation_search", "list_files", "read_file", "grep", "write_file", "edit_file", "memory_search", "memory_show", "memory_edit", "memory_supersede"}
	if err := app.RestrictTools(allowed); err != nil {
		t.Fatalf("dreaming allowlist rejected: %v", err)
	}
	var names []string
	for _, definition := range app.Executor.Registry.Definitions() {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	want := append([]string(nil), allowed...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("restricted tools=%v, want exactly %v", names, want)
	}
}
