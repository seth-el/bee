package bee

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type InitReport struct {
	Home           string `json:"home"`
	ConfigPath     string `json:"config_path"`
	ConfigCreated  bool   `json:"config_created"`
	ContextCreated bool   `json:"context_created"`
}

// InitializeBee creates the private Bee home layout and an example config.
// Existing files are preserved; running it repeatedly is safe.
func InitializeBee(home string) (InitReport, error) {
	if strings.TrimSpace(home) == "" {
		var err error
		home, err = DefaultBeeHome()
		if err != nil {
			return InitReport{}, err
		}
	} else {
		resolved, err := filepath.Abs(home)
		if err != nil {
			return InitReport{}, fmt.Errorf("resolve Bee home: %w", err)
		}
		home = filepath.Clean(resolved)
	}
	if err := ValidateBeePath(home); err != nil {
		return InitReport{}, err
	}
	beforeContext := filepath.Join(home, "workspace", "BEE.md")
	_, contextErr := os.Lstat(beforeContext)
	if _, err := ensureBeeLayoutAt(home); err != nil {
		return InitReport{}, err
	}
	configPath := filepath.Join(home, "config.json")
	_, err := os.Lstat(configPath)
	created := false
	switch {
	case err == nil:
		// Preserve any existing config, including one that doctor may later flag.
	case errors.Is(err, os.ErrNotExist):
		if err := WriteExampleConfig(configPath); err != nil {
			return InitReport{}, err
		}
		created = true
	default:
		return InitReport{}, fmt.Errorf("inspect Bee config: %w", err)
	}
	return InitReport{Home: home, ConfigPath: configPath, ConfigCreated: created, ContextCreated: errors.Is(contextErr, os.ErrNotExist)}, nil
}

type DoctorCheck struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
}

type DoctorReport struct {
	Kind       string        `json:"kind"`
	Status     CheckStatus   `json:"status"`
	CheckedAt  time.Time     `json:"checked_at"`
	Home       string        `json:"home"`
	ConfigPath string        `json:"config_path"`
	Checks     []DoctorCheck `json:"checks"`
}

// RunDoctor performs bounded local diagnostics only. It never contacts a
// provider, reads API-key contents, or changes Bee state.
func RunDoctor(home, configPath string) DoctorReport {
	report := DoctorReport{Kind: "doctor", CheckedAt: time.Now().UTC(), Checks: []DoctorCheck{}}
	if strings.TrimSpace(home) == "" {
		home, _ = DefaultBeeHome()
	} else if resolved, err := filepath.Abs(home); err == nil {
		home = filepath.Clean(resolved)
	}
	report.Home = home
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(home, "config.json")
	}
	report.ConfigPath = configPath
	add := func(name string, status CheckStatus, detail string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail})
	}
	if runtime.GOOS != "linux" {
		add("platform", CheckFail, "Bee foreground releases support Linux only")
	} else if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		add("platform", CheckWarn, fmt.Sprintf("Linux/%s is not a documented release target", runtime.GOARCH))
	} else {
		add("platform", CheckPass, fmt.Sprintf("Linux/%s", runtime.GOARCH))
	}
	if err := ValidateBeePath(home); err != nil {
		add("bee_home", CheckFail, "Bee home is invalid")
	} else if err := rejectBeeSymlinkAncestors(home); err != nil {
		add("bee_home", CheckFail, "Bee home path uses a symlink")
	} else {
		status, detail := statusForPath(home, true)
		add("bee_home", status, detail)
	}
	for _, item := range []struct {
		name string
		path string
		dir  bool
	}{
		{"workspace", filepath.Join(home, "workspace"), true},
		{"skills", filepath.Join(home, "skills"), true},
		{"plugins", filepath.Join(home, "plugins"), true},
		{"workspace_memory", filepath.Join(home, "workspace", "memory"), true},
		{"workspace_projects", filepath.Join(home, "workspace", "projects"), true},
		{"workspace_exports", filepath.Join(home, "workspace", "exports"), true},
		{"workspace_marker", filepath.Join(home, "workspace", ".bee-workspace"), false},
		{"workspace_context", filepath.Join(home, "workspace", "BEE.md"), false},
		{"state", filepath.Join(home, "state"), true},
		{"state_sessions", filepath.Join(home, "state", "sessions"), true},
		{"state_runtime", filepath.Join(home, "state", "runtime"), true},
		{"secrets", filepath.Join(home, "secrets"), true},
	} {
		status, detail := statusForPath(item.path, item.dir)
		add(item.name, status, detail)
	}
	addSessionStoreCheck(add)
	addHistoryRemotesCheck(home, add)
	// SOUL.md is optional, but when present it must be owner-only like BEE.md:
	// otherwise every session fails to compose its system prompt.
	soul := filepath.Join(home, DefaultSystemPromptFile)
	if _, err := os.Lstat(soul); errors.Is(err, os.ErrNotExist) {
		add("system_prompt_file", CheckPass, "absent (optional)")
	} else {
		status, detail := statusForPath(soul, false)
		add("system_prompt_file", status, detail)
	}
	status, detail := statusForPath(configPath, false)
	add("config_permissions", status, detail)
	if config, err := LoadConfig(configPath); err != nil {
		// Show the cause (e.g. `unknown field "requre_plan"`): "invalid" alone
		// sends the user to `bee config validate` to learn what doctor knew.
		// Redacted and bounded so a secret-like value can never be echoed.
		add("config_validity", CheckFail, "config is missing or invalid: "+truncateUTF8(redactString(err.Error()), 512))
	} else {
		add("config_validity", CheckPass, "config parses and validates")
		if ignored := config.UnknownFields(); len(ignored) > 0 {
			add("config_unknown_fields", CheckWarn, "ignored by this build: "+truncateUTF8(strings.Join(ignored, ", "), 512)+" (newer config, or a typo far from any known field)")
		}
		checkProviderKeyFiles(configPath, add)
		addProviderReachabilityCheck(config, add)
		addRecurringScheduleCheck(config.RecurringJobs, report.CheckedAt, add)
		jobStorePath := strings.TrimSpace(config.Assistant.JobStorePath)
		if jobStorePath == "" {
			jobStorePath = filepath.Join(home, "state", "jobs.json")
		}
		addRecurringSchedulerCheck(config.RecurringJobs, jobStorePath, report.CheckedAt, add)
	}
	if report.Status == "" {
		report.Status = CheckPass
	}
	for _, check := range report.Checks {
		if check.Status == CheckFail {
			report.Status = CheckFail
			break
		}
		if check.Status == CheckWarn && report.Status != CheckFail {
			report.Status = CheckWarn
		}
	}
	return report
}

func statusForPath(path string, directory bool) (CheckStatus, string) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CheckFail, "path is missing; run `bee init`"
		}
		return CheckFail, "path cannot be inspected"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return CheckFail, "path is a symlink"
	}
	if directory {
		if !info.IsDir() {
			return CheckFail, "path is not a directory"
		}
		if info.Mode().Perm() != 0o700 {
			return CheckFail, "directory must be private 0700 (fix: chmod 700 " + shellQuotePath(path) + ")"
		}
	} else {
		if !info.Mode().IsRegular() {
			return CheckFail, "path is not a regular file"
		}
		if info.Mode().Perm()&0o077 != 0 {
			return CheckFail, "file must be owner-only (fix: chmod 600 " + shellQuotePath(path) + ")"
		}
	}
	return CheckPass, "private"
}

func checkProviderKeyFiles(configPath string, add func(string, CheckStatus, string)) {
	config, err := LoadConfig(configPath)
	if err != nil {
		return
	}
	files := map[string]string{}
	if strings.TrimSpace(config.Provider.APIKeyFile) != "" {
		files["provider"] = config.Provider.APIKeyFile
	}
	for _, profile := range config.Profiles {
		if strings.TrimSpace(profile.Provider.APIKeyFile) != "" {
			files["profile "+profile.Name] = profile.Provider.APIKeyFile
		}
	}
	if len(files) == 0 {
		add("provider_api_keys", CheckPass, "no API-key files configured")
		return
	}
	keys := make([]string, 0, len(files))
	for name := range files {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		status, detail := statusForPath(files[name], false)
		if status == CheckPass {
			detail = "owner-only API-key file"
		}
		add("provider_api_key_"+strings.ReplaceAll(name, " ", "_"), status, detail)
	}
}

func (report DoctorReport) Human() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", report.Kind, report.Status)
	fmt.Fprintf(&b, "home: %s\n", report.Home)
	for _, check := range report.Checks {
		fmt.Fprintf(&b, "- %-24s %-4s %s\n", check.Name, strings.ToUpper(string(check.Status)), check.Detail)
	}
	return b.String()
}

// addRecurringScheduleCheck warns when an enabled cron job's local wall time
// is skipped (job silently does not fire) or repeated (job fires twice) at a
// daylight-saving change within the next year.
func addRecurringScheduleCheck(config RecurringJobsConfig, now time.Time, add func(string, CheckStatus, string)) {
	warnings := config.ScheduleWarnings(now)
	if len(warnings) == 0 {
		add("recurring_schedules", CheckPass, "no cron wall time is skipped or repeated by daylight saving in the next year")
		return
	}
	details := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		details = append(details, warning.String())
	}
	add("recurring_schedules", CheckWarn, strings.Join(details, "; "))
}

// addHistoryRemotesCheck warns when the workspace or history repo has
// commits its origin remote does not, so a nightly push that keeps failing
// is visible. Repos without an origin are local by choice and pass.
func addHistoryRemotesCheck(home string, add func(string, CheckStatus, string)) {
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	var notes, problems []string
	for _, repo := range []string{filepath.Join(home, "workspace"), filepath.Join(home, "history")} {
		if info, err := os.Stat(filepath.Join(repo, ".git")); err != nil || !info.IsDir() {
			continue
		}
		git := func(args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
			return strings.TrimSpace(string(out)), err
		}
		name := filepath.Base(repo)
		if _, err := git("remote", "get-url", "origin"); err != nil {
			notes = append(notes, name+": local only")
			continue
		}
		ahead, err := git("rev-list", "--count", "@{upstream}..HEAD")
		switch {
		case err != nil:
			problems = append(problems, name+": has an origin but no upstream branch (git push -u origin HEAD)")
		case ahead != "0":
			problems = append(problems, name+": "+ahead+" commit(s) not pushed to origin")
		default:
			notes = append(notes, name+": pushed")
		}
	}
	switch {
	case len(problems) > 0:
		add("history_remotes", CheckWarn, strings.Join(append(problems, notes...), "; "))
	case len(notes) > 0:
		add("history_remotes", CheckPass, strings.Join(notes, "; "))
	}
}

// addSessionStoreCheck reports which session store Bee uses, and warns
// while sessions are still in (or left behind in) the legacy XDG store.
func addSessionStoreCheck(add func(string, CheckStatus, string)) {
	root, err := DefaultBeeSessionRoot()
	if err != nil {
		add("session_store", CheckFail, err.Error())
		return
	}
	legacy := LegacyBeeSessionRoot()
	switch {
	case legacy != "" && root == legacy:
		add("session_store", CheckWarn, "sessions are still in the legacy store "+legacy+"; stop bee-serve, bee-telegram and any TUI, then run `bee sessions migrate --apply` (and point other adapters' session_root at the new store)")
	case legacy != "" && SessionRootHasSessions(legacy):
		add("session_store", CheckWarn, "using "+root+", but the legacy store "+legacy+" still holds sessions Bee no longer reads; merge or archive them by hand")
	default:
		add("session_store", CheckPass, "sessions in "+root)
	}
}

// recurringSchedulerGrace is how long after an occurrence comes due doctor
// waits before calling it missed. A timer runs `bee jobs run-due` every few
// minutes, so an hour only trips when nothing is ticking at all.
const recurringSchedulerGrace = time.Hour

// addRecurringSchedulerCheck warns when an enabled recurring job's latest due
// occurrence never reached the job store, i.e. nothing runs `bee jobs
// run-due`. Recurring jobs have no other trigger, so without this the whole
// feature can be silently off.
func addRecurringSchedulerCheck(config RecurringJobsConfig, jobStorePath string, now time.Time, add func(string, CheckStatus, string)) {
	enabled := 0
	for _, job := range config.Jobs {
		if job.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		add("recurring_scheduler", CheckPass, "no enabled recurring jobs")
		return
	}
	store, err := NewJobStore(jobStorePath)
	if err != nil {
		add("recurring_scheduler", CheckWarn, "job store path is invalid")
		return
	}
	jobs, err := store.List()
	if err != nil {
		add("recurring_scheduler", CheckWarn, "job store cannot be read: "+err.Error())
		return
	}
	missed := config.MissedOccurrences(jobs, now, recurringSchedulerGrace)
	if len(missed) == 0 {
		add("recurring_scheduler", CheckPass, "every enabled recurring job has run its latest due occurrence")
		return
	}
	details := make([]string, 0, len(missed))
	for _, item := range missed {
		details = append(details, item.String())
	}
	add("recurring_scheduler", CheckWarn, strings.Join(details, "; "))
}

// shellQuotePath renders a path for a copy-pasteable fix hint.
func shellQuotePath(path string) string {
	if path != "" && strings.IndexFunc(path, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) < 0 {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
