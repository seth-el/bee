package bee

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxRecurringJobs              = 32
	maxRecurringTools             = 64
	maxRecurringWorkspacePaths    = 16
	maxRecurringCatchUp           = 32
	defaultRecurringCatchUp       = 1
	defaultRecurringCatchUpWindow = 24 * time.Hour
	maxRecurringCatchUpWindow     = 31 * 24 * time.Hour
	minRecurringInterval          = time.Minute
	maxRecurringInterval          = 366 * 24 * time.Hour
	RecurringConcurrencyForbid    = "forbid"
	RecurringOutputSession        = "session"
	RecurringNotificationInbox    = "inbox"
)

// RecurringJobsConfig is immutable operator configuration. Model-facing job
// tools deliberately have no create/update path for these definitions.
type RecurringJobsConfig struct {
	Jobs []RecurringJobConfig `json:"jobs,omitempty"`
}

type RecurringJobConfig struct {
	ID                string                  `json:"id"`
	Enabled           bool                    `json:"enabled"`
	Schedule          RecurringScheduleConfig `json:"schedule"`
	Prompt            string                  `json:"prompt"`
	Contract          JobContract             `json:"contract"`
	MaxCatchUp        int                     `json:"max_catch_up,omitempty"`
	CatchUpWindowSecs int                     `json:"catch_up_window_seconds,omitempty"`
	ConcurrencyPolicy string                  `json:"concurrency_policy,omitempty"`
}

type RecurringScheduleConfig struct {
	StartAt         time.Time `json:"start_at"`
	IntervalSeconds int       `json:"interval_seconds,omitempty"`
	Cron            string    `json:"cron,omitempty"`
	TimeZone        string    `json:"time_zone,omitempty"`
}

// JobContract is persisted with each materialized firing so later config
// edits cannot widen a run that is already queued.
type JobContract struct {
	AllowedTools   []string `json:"allowed_tools"`
	WorkspacePaths []string `json:"workspace_paths"`
	// ReadOnlyPaths are readable scope roots that are never write roots. They
	// let a run consult ground truth without being able to modify it. A
	// read-only path may contain workspace_paths (read all of memory/, write
	// only memory/topics/), but may not equal or sit inside one, since it
	// would then be writable through that workspace path.
	ReadOnlyPaths       []string `json:"read_only_paths,omitempty"`
	MaxWallClockSeconds int      `json:"max_wall_clock_seconds"`
	MaxToolCalls        int      `json:"max_tool_calls"`
	OutputTarget        string   `json:"output_target"`
	NotificationTarget  string   `json:"notification_target"`
}

func cloneRecurringJobsConfig(config RecurringJobsConfig) RecurringJobsConfig {
	result := RecurringJobsConfig{Jobs: make([]RecurringJobConfig, len(config.Jobs))}
	for index, job := range config.Jobs {
		result.Jobs[index] = job
		result.Jobs[index].Contract.AllowedTools = append([]string(nil), job.Contract.AllowedTools...)
		result.Jobs[index].Contract.WorkspacePaths = append([]string(nil), job.Contract.WorkspacePaths...)
		result.Jobs[index].Contract.ReadOnlyPaths = append([]string(nil), job.Contract.ReadOnlyPaths...)
	}
	return result
}

func cloneJobContract(contract JobContract) JobContract {
	contract.AllowedTools = append([]string(nil), contract.AllowedTools...)
	contract.WorkspacePaths = append([]string(nil), contract.WorkspacePaths...)
	contract.ReadOnlyPaths = append([]string(nil), contract.ReadOnlyPaths...)
	return contract
}

func (config RecurringJobsConfig) Validate() error {
	if len(config.Jobs) > maxRecurringJobs {
		return fmt.Errorf("recurring_jobs must not contain more than %d jobs", maxRecurringJobs)
	}
	seen := make(map[string]struct{}, len(config.Jobs))
	for index, job := range config.Jobs {
		if _, ok := seen[job.ID]; ok {
			return fmt.Errorf("recurring_jobs repeats id %q", job.ID)
		}
		seen[job.ID] = struct{}{}
		if err := job.Validate(); err != nil {
			return fmt.Errorf("recurring_jobs jobs[%d]: %w", index, err)
		}
	}
	return nil
}

func (config RecurringJobConfig) Validate() error {
	if err := validateJobIdentifier("recurring job id", config.ID, true); err != nil {
		return err
	}
	if !config.Enabled {
		return nil
	}
	if strings.TrimSpace(config.Prompt) == "" || strings.ContainsRune(config.Prompt, '\x00') || len(config.Prompt) > maxJobToolPromptBytes {
		return fmt.Errorf("prompt must be between 1 and %d bytes", maxJobToolPromptBytes)
	}
	if err := config.Schedule.Validate(); err != nil {
		return err
	}
	if err := config.Contract.Validate(); err != nil {
		return err
	}
	if config.MaxCatchUp < 0 || config.MaxCatchUp > maxRecurringCatchUp {
		return fmt.Errorf("max_catch_up must be between 0 and %d", maxRecurringCatchUp)
	}
	if config.CatchUpWindowSecs < 0 || time.Duration(config.CatchUpWindowSecs)*time.Second > maxRecurringCatchUpWindow {
		return fmt.Errorf("catch_up_window_seconds must be between 0 and %d", int(maxRecurringCatchUpWindow/time.Second))
	}
	if policy := config.effectiveConcurrencyPolicy(); policy != RecurringConcurrencyForbid {
		return fmt.Errorf("concurrency_policy %q is unsupported", policy)
	}
	return nil
}

func (config RecurringScheduleConfig) Validate() error {
	if config.StartAt.IsZero() {
		return errors.New("schedule start_at is required")
	}
	if config.StartAt.Location() != time.UTC {
		return errors.New("schedule start_at must use UTC (Z)")
	}
	hasInterval := config.IntervalSeconds != 0
	hasCron := strings.TrimSpace(config.Cron) != ""
	if hasInterval == hasCron {
		return errors.New("schedule must set exactly one of interval_seconds or cron")
	}
	if hasInterval {
		interval := time.Duration(config.IntervalSeconds) * time.Second
		if interval < minRecurringInterval || interval > maxRecurringInterval {
			return fmt.Errorf("interval_seconds must be between %d and %d", int(minRecurringInterval/time.Second), int(maxRecurringInterval/time.Second))
		}
	}
	if hasCron {
		if _, err := parseRecurringCron(config.Cron); err != nil {
			return fmt.Errorf("cron: %w", err)
		}
	}
	zone := strings.TrimSpace(config.TimeZone)
	if zone == "" {
		zone = "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return errors.New("schedule time_zone is invalid")
	}
	return nil
}

func (contract JobContract) Validate() error {
	if len(contract.AllowedTools) > maxRecurringTools {
		return fmt.Errorf("contract allowed_tools must not contain more than %d entries", maxRecurringTools)
	}
	seenTools := make(map[string]struct{}, len(contract.AllowedTools))
	for _, name := range contract.AllowedTools {
		if !validMCPName(name) {
			return fmt.Errorf("contract allowed_tools contains invalid tool %q", name)
		}
		if _, ok := seenTools[name]; ok {
			return fmt.Errorf("contract allowed_tools repeats %q", name)
		}
		seenTools[name] = struct{}{}
	}
	if len(contract.WorkspacePaths) == 0 || len(contract.WorkspacePaths) > maxRecurringWorkspacePaths {
		return fmt.Errorf("contract workspace_paths must contain between 1 and %d entries", maxRecurringWorkspacePaths)
	}
	seenPaths := make(map[string]struct{}, len(contract.WorkspacePaths))
	for index, path := range contract.WorkspacePaths {
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("contract workspace_paths[%d] must be an absolute non-root path", index)
		}
		clean := filepath.Clean(path)
		if _, ok := seenPaths[clean]; ok {
			return fmt.Errorf("contract workspace_paths repeats %q", clean)
		}
		seenPaths[clean] = struct{}{}
	}
	if len(contract.ReadOnlyPaths) > maxRecurringWorkspacePaths {
		return fmt.Errorf("contract read_only_paths must not contain more than %d entries", maxRecurringWorkspacePaths)
	}
	for index, path := range contract.ReadOnlyPaths {
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("contract read_only_paths[%d] must be an absolute non-root path", index)
		}
		clean := filepath.Clean(path)
		if _, ok := seenPaths[clean]; ok {
			return fmt.Errorf("contract read_only_paths repeats %q", clean)
		}
		// A read-only root equal to or inside a write root would be writable
		// through that write root. The reverse nesting is coherent: writes stay
		// confined to the inner workspace path.
		for _, writable := range contract.WorkspacePaths {
			if lexicalPathWithin(filepath.Clean(writable), clean) {
				return fmt.Errorf("contract read_only_paths[%d] is inside workspace_paths and would be writable", index)
			}
		}
		seenPaths[clean] = struct{}{}
	}
	if contract.MaxWallClockSeconds < 1 || contract.MaxWallClockSeconds > 24*60*60 {
		return errors.New("contract max_wall_clock_seconds must be between 1 and 86400")
	}
	if contract.MaxToolCalls < 1 || contract.MaxToolCalls > 128 {
		return errors.New("contract max_tool_calls must be between 1 and 128")
	}
	if contract.OutputTarget != RecurringOutputSession {
		return fmt.Errorf("contract output_target must be %q", RecurringOutputSession)
	}
	if contract.NotificationTarget != RecurringNotificationInbox {
		return fmt.Errorf("contract notification_target must be %q", RecurringNotificationInbox)
	}
	return nil
}

func (config RecurringJobConfig) effectiveMaxCatchUp() int {
	if config.MaxCatchUp <= 0 {
		return defaultRecurringCatchUp
	}
	return config.MaxCatchUp
}

func (config RecurringJobConfig) effectiveCatchUpWindow() time.Duration {
	if config.CatchUpWindowSecs <= 0 {
		return defaultRecurringCatchUpWindow
	}
	return time.Duration(config.CatchUpWindowSecs) * time.Second
}

func (config RecurringJobConfig) effectiveConcurrencyPolicy() string {
	if strings.TrimSpace(config.ConcurrencyPolicy) == "" {
		return RecurringConcurrencyForbid
	}
	return strings.ToLower(strings.TrimSpace(config.ConcurrencyPolicy))
}

type recurringCronField struct {
	values   map[int]struct{}
	wildcard bool
}

type recurringCron struct {
	minute, hour, day, month, weekday recurringCronField
}

func parseRecurringCron(expression string) (recurringCron, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return recurringCron{}, errors.New("expected five fields: minute hour day-of-month month day-of-week")
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	parsed := make([]recurringCronField, 5)
	for index := range fields {
		field, err := parseRecurringCronField(fields[index], bounds[index][0], bounds[index][1])
		if err != nil {
			return recurringCron{}, fmt.Errorf("field %d: %w", index+1, err)
		}
		parsed[index] = field
	}
	return recurringCron{minute: parsed[0], hour: parsed[1], day: parsed[2], month: parsed[3], weekday: parsed[4]}, nil
}

func parseRecurringCronField(input string, minimum, maximum int) (recurringCronField, error) {
	field := recurringCronField{values: make(map[int]struct{}), wildcard: input == "*"}
	for _, part := range strings.Split(input, ",") {
		if part == "" {
			return recurringCronField{}, errors.New("empty list item")
		}
		base := part
		step := 1
		if left, right, ok := strings.Cut(part, "/"); ok {
			base = left
			parsed, err := strconv.Atoi(right)
			if err != nil || parsed < 1 || parsed > maximum-minimum+1 {
				return recurringCronField{}, errors.New("invalid step")
			}
			step = parsed
		}
		start, end := minimum, maximum
		switch {
		case base == "*":
			if len(strings.Split(input, ",")) == 1 {
				field.wildcard = true
			}
		case strings.Contains(base, "-"):
			left, right, _ := strings.Cut(base, "-")
			var err error
			start, err = strconv.Atoi(left)
			if err != nil {
				return recurringCronField{}, errors.New("invalid range")
			}
			end, err = strconv.Atoi(right)
			if err != nil || start > end {
				return recurringCronField{}, errors.New("invalid range")
			}
		default:
			value, err := strconv.Atoi(base)
			if err != nil {
				return recurringCronField{}, errors.New("invalid value")
			}
			start, end = value, value
		}
		if start < minimum || end > maximum {
			return recurringCronField{}, fmt.Errorf("value must be between %d and %d", minimum, maximum)
		}
		for value := start; value <= end; value += step {
			field.values[value] = struct{}{}
		}
	}
	return field, nil
}

func (cron recurringCron) matches(at time.Time) bool {
	contains := func(field recurringCronField, value int) bool {
		_, ok := field.values[value]
		return ok
	}
	if !contains(cron.minute, at.Minute()) || !contains(cron.hour, at.Hour()) || !contains(cron.month, int(at.Month())) {
		return false
	}
	day := contains(cron.day, at.Day())
	weekday := contains(cron.weekday, int(at.Weekday()))
	if !cron.day.wildcard && !cron.weekday.wildcard {
		return day || weekday
	}
	return day && weekday
}

func (config RecurringJobConfig) dueOccurrences(after, now time.Time) ([]time.Time, error) {
	if now.Before(config.Schedule.StartAt) {
		return nil, nil
	}
	limit := config.effectiveMaxCatchUp()
	lower := now.Add(-config.effectiveCatchUpWindow())
	if lower.Before(config.Schedule.StartAt) {
		lower = config.Schedule.StartAt
	}
	if after.After(lower) {
		lower = after.Add(time.Nanosecond)
	}
	occurrences := make([]time.Time, 0, limit)
	if config.Schedule.IntervalSeconds > 0 {
		interval := time.Duration(config.Schedule.IntervalSeconds) * time.Second
		first := config.Schedule.StartAt
		if first.Before(lower) {
			steps := lower.Sub(first) / interval
			first = first.Add(steps * interval)
			if first.Before(lower) {
				first = first.Add(interval)
			}
		}
		for candidate := first; !candidate.After(now) && len(occurrences) < limit; candidate = candidate.Add(interval) {
			if candidate.After(after) {
				occurrences = append(occurrences, candidate.UTC())
			}
		}
		return occurrences, nil
	}
	cron, err := parseRecurringCron(config.Schedule.Cron)
	if err != nil {
		return nil, err
	}
	zone := config.Schedule.TimeZone
	if strings.TrimSpace(zone) == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	candidate := lower.UTC().Truncate(time.Minute)
	if candidate.Before(lower) {
		candidate = candidate.Add(time.Minute)
	}
	for !candidate.After(now) && len(occurrences) < limit {
		if candidate.After(after) && cron.matches(candidate.In(location)) {
			occurrences = append(occurrences, candidate.UTC())
		}
		candidate = candidate.Add(time.Minute)
	}
	return occurrences, nil
}

// RecurringScheduler materializes configured firings into the existing job
// store. Claims, leases, retries, finalization, and notification intents then
// remain the one authoritative scheduler path.
// lexicalPathWithin reports whether path equals root or lies below it, using
// cleaned absolute paths only. Symlink-aware checks happen at composition.
func lexicalPathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

type RecurringScheduler struct {
	Config RecurringJobsConfig
	Store  *JobStore
}

func NewRecurringScheduler(config RecurringJobsConfig, store *JobStore) (*RecurringScheduler, error) {
	if store == nil {
		return nil, errors.New("recurring scheduler job store is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &RecurringScheduler{Config: cloneRecurringJobsConfig(config), Store: store}, nil
}

func (scheduler *RecurringScheduler) Materialize(now time.Time) ([]Job, error) {
	if scheduler == nil || scheduler.Store == nil {
		return nil, errors.New("recurring scheduler is not configured")
	}
	lock, err := scheduler.Store.acquire()
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	scheduler.Store.mu.Lock()
	defer scheduler.Store.mu.Unlock()
	jobs, err := scheduler.Store.loadLocked()
	if err != nil {
		return nil, err
	}
	created := make([]Job, 0)
	for _, definition := range scheduler.Config.Jobs {
		if !definition.Enabled {
			continue
		}
		latest := time.Time{}
		active := false
		for _, existing := range jobs {
			if existing.RecurringID != definition.ID {
				continue
			}
			if existing.Occurrence.After(latest) {
				latest = existing.Occurrence
			}
			if existing.State == JobApproved || existing.State == JobRunning || existing.State == JobPending {
				active = true
			}
		}
		if active {
			continue
		}
		occurrences, dueErr := definition.dueOccurrences(latest, now.UTC())
		if dueErr != nil {
			return nil, dueErr
		}
		for _, occurrence := range occurrences {
			id := fmt.Sprintf("recurring-%s-%d", definition.ID, occurrence.Unix())
			contract := cloneJobContract(definition.Contract)
			job := Job{
				ID: id, Name: definition.ID, Owner: "configured", IdempotencyKey: id,
				Schedule: Schedule{RunAt: occurrence},
				Request:  JobRequest{Prompt: definition.Prompt, Title: definition.ID, ProjectRoot: contract.WorkspacePaths[0]},
				State:    JobApproved, Approval: ApprovalGranted, MaxAttempts: 1,
				CreatedAt: now.UTC(), UpdatedAt: now.UTC(), RecurringID: definition.ID,
				Occurrence: occurrence.UTC(), Contract: &contract,
			}
			jobs = append(jobs, job)
			created = append(created, job)
		}
	}
	if len(created) == 0 {
		return nil, nil
	}
	if err := scheduler.Store.saveLocked(jobs); err != nil {
		return nil, err
	}
	sort.Slice(created, func(i, j int) bool { return created[i].Occurrence.Before(created[j].Occurrence) })
	return created, nil
}

func (scheduler *RecurringScheduler) Tick(ctx context.Context, now time.Time, execute JobDetailedExecutor, notifier JobNotifier) ([]Job, error) {
	if _, err := scheduler.Materialize(now); err != nil {
		return nil, err
	}
	return scheduler.Store.RunDueDetailedWithNotifier(ctx, now, execute, notifier)
}

// RecurringScheduleWarning is a non-fatal schedule lint finding. It names
// only the job id, schedule, zone, and the first affected local date.
type RecurringScheduleWarning struct {
	JobID string `json:"job_id"`
	// Kind is "skipped" (a matching local wall time does not exist that day,
	// so the job silently does not fire) or "repeated" (the wall time occurs
	// twice, so the job fires twice).
	Kind      string `json:"kind"`
	Cron      string `json:"cron"`
	TimeZone  string `json:"time_zone"`
	LocalDate string `json:"local_date"`
	LocalTime string `json:"local_time"`
}

func (warning RecurringScheduleWarning) String() string {
	if warning.Kind == "repeated" {
		return fmt.Sprintf("recurring job %q: cron %q in %s fires twice on %s (%s occurs twice at a daylight-saving change)", warning.JobID, warning.Cron, warning.TimeZone, warning.LocalDate, warning.LocalTime)
	}
	return fmt.Sprintf("recurring job %q: cron %q in %s silently does not fire on %s (%s does not exist at a daylight-saving change)", warning.JobID, warning.Cron, warning.TimeZone, warning.LocalDate, warning.LocalTime)
}

// ScheduleWarnings lints every enabled cron job over the year following
// from. Interval schedules are UTC-relative and never affected.
func (config RecurringJobsConfig) ScheduleWarnings(from time.Time) []RecurringScheduleWarning {
	var warnings []RecurringScheduleWarning
	for _, job := range config.Jobs {
		if job.Enabled {
			warnings = append(warnings, job.ScheduleWarnings(from)...)
		}
	}
	return warnings
}

// ScheduleWarnings reports, at most once per kind, the first local date in
// the year following from (or start_at, if later) on which a cron wall time
// is skipped or repeated by a zone offset change. Materialization matches
// cron fields against local wall time minute by minute, so a nonexistent
// wall time never matches and a repeated one matches twice.
func (config RecurringJobConfig) ScheduleWarnings(from time.Time) []RecurringScheduleWarning {
	if strings.TrimSpace(config.Schedule.Cron) == "" {
		return nil
	}
	cron, err := parseRecurringCron(config.Schedule.Cron)
	if err != nil {
		return nil
	}
	zone := strings.TrimSpace(config.Schedule.TimeZone)
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil
	}
	start := from.UTC()
	if config.Schedule.StartAt.After(start) {
		start = config.Schedule.StartAt.UTC()
	}
	start = start.Truncate(time.Hour)
	end := start.Add(366 * 24 * time.Hour)
	result := make([]RecurringScheduleWarning, 0, 2)
	seen := map[string]bool{}
	add := func(kind string, date civilDate, hour, minute int) {
		if seen[kind] {
			return
		}
		seen[kind] = true
		result = append(result, RecurringScheduleWarning{
			JobID: config.ID, Kind: kind, Cron: config.Schedule.Cron, TimeZone: zone,
			LocalDate: fmt.Sprintf("%04d-%02d-%02d", date.year, date.month, date.day),
			LocalTime: fmt.Sprintf("%02d:%02d", hour, minute),
		})
	}
	_, previousOffset := start.In(location).Zone()
	for at := start.Add(time.Hour); at.Before(end) && len(seen) < 2; at = at.Add(time.Hour) {
		_, offset := at.In(location).Zone()
		if offset == previousOffset {
			continue
		}
		previousOffset = offset
		// Count how often each local wall minute matches within two days of the
		// change, then compare against the wall times cron expects that day.
		counts := map[recurringWallMinute]int{}
		for minute := at.Add(-48 * time.Hour); minute.Before(at.Add(48 * time.Hour)); minute = minute.Add(time.Minute) {
			local := minute.In(location)
			if cron.matches(local) {
				counts[recurringWallMinuteOf(local)]++
			}
		}
		changeDate := civilDateOf(at.In(location))
		for _, date := range []civilDate{changeDate, civilDateOf(at.Add(-time.Hour).In(location))} {
			probe := time.Date(date.year, time.Month(date.month), date.day, 12, 0, 0, 0, location)
			if !cron.matchesDate(probe) {
				continue
			}
			for hour := 0; hour < 24; hour++ {
				if _, ok := cron.hour.values[hour]; !ok {
					continue
				}
				for minute := 0; minute < 60; minute++ {
					if _, ok := cron.minute.values[minute]; !ok {
						continue
					}
					wall := recurringWallMinute{date: date, hour: hour, minute: minute}
					switch counts[wall] {
					case 0:
						add("skipped", date, hour, minute)
					case 1:
					default:
						add("repeated", date, hour, minute)
					}
				}
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].LocalDate < result[j].LocalDate })
	return result
}

type civilDate struct{ year, month, day int }

func civilDateOf(at time.Time) civilDate {
	return civilDate{year: at.Year(), month: int(at.Month()), day: at.Day()}
}

type recurringWallMinute struct {
	date         civilDate
	hour, minute int
}

func recurringWallMinuteOf(at time.Time) recurringWallMinute {
	return recurringWallMinute{date: civilDateOf(at), hour: at.Hour(), minute: at.Minute()}
}

// matchesDate applies only the day-of-month, month, and weekday fields
// (with cron's OR rule when both day fields are restricted).
func (cron recurringCron) matchesDate(at time.Time) bool {
	contains := func(field recurringCronField, value int) bool {
		_, ok := field.values[value]
		return ok
	}
	if !contains(cron.month, int(at.Month())) {
		return false
	}
	day := contains(cron.day, at.Day())
	weekday := contains(cron.weekday, int(at.Weekday()))
	if !cron.day.wildcard && !cron.weekday.wildcard {
		return day || weekday
	}
	return day && weekday
}

// RecurringMissedOccurrence reports an enabled job whose most recent due
// occurrence (older than the grace period) was never materialized into the
// job store: nothing has run `bee jobs run-due` since it came due.
type RecurringMissedOccurrence struct {
	JobID      string    `json:"job_id"`
	Occurrence time.Time `json:"occurrence"`
}

func (missed RecurringMissedOccurrence) String() string {
	return fmt.Sprintf("recurring job %q has not run its %s occurrence; nothing has run `bee jobs run-due` since then (see docs/operations/recurring-jobs-timer.md)", missed.JobID, missed.Occurrence.UTC().Format(time.RFC3339))
}

// maxRecurringMissedLookback bounds the backwards cron search; every cron
// expression the parser accepts fires at least once a month.
const maxRecurringMissedLookback = 32 * 24 * time.Hour

// MissedOccurrences compares each enabled job's latest occurrence at or
// before now-grace with the job store. It is the "is the scheduler running?"
// check: an occurrence that came due over grace ago and was never
// materialized means no tick has happened since.
func (config RecurringJobsConfig) MissedOccurrences(jobs []Job, now time.Time, grace time.Duration) []RecurringMissedOccurrence {
	var missed []RecurringMissedOccurrence
	for _, definition := range config.Jobs {
		if !definition.Enabled {
			continue
		}
		latest, ok := definition.latestOccurrenceAtOrBefore(now.Add(-grace).UTC())
		if !ok {
			continue
		}
		seen := false
		for _, job := range jobs {
			if job.RecurringID == definition.ID && !job.Occurrence.Before(latest) {
				seen = true
				break
			}
		}
		if !seen {
			missed = append(missed, RecurringMissedOccurrence{JobID: definition.ID, Occurrence: latest})
		}
	}
	return missed
}

func (config RecurringJobConfig) latestOccurrenceAtOrBefore(at time.Time) (time.Time, bool) {
	start := config.Schedule.StartAt.UTC()
	if at.Before(start) {
		return time.Time{}, false
	}
	if config.Schedule.IntervalSeconds > 0 {
		interval := time.Duration(config.Schedule.IntervalSeconds) * time.Second
		return start.Add(at.Sub(start) / interval * interval), true
	}
	cron, err := parseRecurringCron(config.Schedule.Cron)
	if err != nil {
		return time.Time{}, false
	}
	zone := strings.TrimSpace(config.Schedule.TimeZone)
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, false
	}
	floor := at.Add(-maxRecurringMissedLookback)
	if floor.Before(start) {
		floor = start
	}
	for candidate := at.Truncate(time.Minute); !candidate.Before(floor); candidate = candidate.Add(-time.Minute) {
		if cron.matches(candidate.In(location)) {
			return candidate, true
		}
	}
	return time.Time{}, false
}
