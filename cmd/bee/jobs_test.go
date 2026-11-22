package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestJobsCLIRejectsUnknownCommand(t *testing.T) {
	if err := runJobs([]string{"wat"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown jobs command accepted")
	}
}

// Without --store, jobs commands use the assistant's job store:
// assistant.job_store_path from config, else BEE_HOME/state/jobs.json.
func TestJobsCLIDefaultStoreFollowsAssistant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	missingConfig := filepath.Join(home, "absent.json")
	store, err := resolveJobStore("", missingConfig)
	if err != nil || store.Path() != filepath.Join(home, "state", "jobs.json") {
		t.Fatalf("no-config store = %v, %v", store, err)
	}
	custom := filepath.Join(t.TempDir(), "custom-jobs.json")
	config := writeTestConfig(t, "http://127.0.0.1:1", "test-model")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	withStore := strings.Replace(string(data), "{", `{"assistant":{"job_store_path":`+strconv.Quote(custom)+`},`, 1)
	if err := os.WriteFile(config, []byte(withStore), 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err = resolveJobStore("", config); err != nil || store.Path() != custom {
		t.Fatalf("configured store = %v, %v", store, err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	if store, err = resolveJobStore(explicit, config); err != nil || store.Path() != explicit {
		t.Fatalf("explicit store = %v, %v", store, err)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte(`{"asistant":{"require_plan":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJobStore("", broken); err == nil || !strings.Contains(err.Error(), "--store") {
		t.Fatalf("broken config err = %v", err)
	}
}

func cliPausedRecurringJob(t *testing.T) (string, string, time.Time) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "jobs.json")
	store, err := bee.NewJobStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	config := bee.RecurringJobsConfig{Jobs: []bee.RecurringJobConfig{{
		ID: "nightly", Enabled: true, Prompt: "consolidate",
		Schedule: bee.RecurringScheduleConfig{StartAt: start, IntervalSeconds: 86400},
		Contract: bee.JobContract{AllowedTools: []string{"read_file"}, WorkspacePaths: []string{t.TempDir()}, MaxWallClockSeconds: 30, MaxToolCalls: 4, OutputTarget: bee.RecurringOutputSession, NotificationTarget: bee.RecurringNotificationInbox},
	}}}
	scheduler, err := bee.NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(storePath))
	if err != nil {
		t.Fatal(err)
	}
	results, err := scheduler.Tick(context.Background(), start, func(context.Context, bee.Job) (bee.JobExecution, error) {
		return bee.JobExecution{SessionID: "s1"}, bee.ErrJobApprovalRequired
	}, bee.NewJobNotificationNotifier(inbox))
	if err != nil || len(results) != 1 || results[0].State != bee.JobPaused {
		t.Fatalf("paused tick = %#v, %v", results, err)
	}
	return storePath, results[0].ID, start
}

func TestInboxCLIListsReasonAndAcknowledges(t *testing.T) {
	storePath, jobID, _ := cliPausedRecurringJob(t)
	var output bytes.Buffer
	if err := runInbox([]string{"--store", storePath}, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"inbox: 1 unread", "paused", jobID, "reason: job paused for owner approval", "bee jobs retry --id " + jobID, "id: notification-"} {
		if !strings.Contains(text, want) {
			t.Fatalf("inbox output missing %q:\n%s", want, text)
		}
	}
	output.Reset()
	if err := runInbox([]string{"list", "--store", storePath, "--json"}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), `"unread_count":1`) || strings.Contains(output.String(), "delivery_key") {
		t.Fatalf("json = %s, %v", output.String(), err)
	}
	if err := runInbox([]string{"ack", "--store", storePath}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("ack without id or --all accepted")
	}
	if err := runInbox([]string{"ack", "notification-missing", "--store", storePath}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "bee inbox") {
		t.Fatalf("missing ack err = %v", err)
	}
	output.Reset()
	if err := runInbox([]string{"ack", "--all", "--store", storePath}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), "acknowledged 1") {
		t.Fatalf("ack all = %s, %v", output.String(), err)
	}
	output.Reset()
	if err := runInbox([]string{"--store", storePath}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), "nothing unread") {
		t.Fatalf("after ack = %s, %v", output.String(), err)
	}
	output.Reset()
	if err := runInbox([]string{"--all", "--store", storePath}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), jobID) {
		t.Fatalf("--all = %s, %v", output.String(), err)
	}
	if err := runInbox([]string{"purge"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown inbox subcommand accepted")
	}
}

func TestJobsRetryAndDismissCLI(t *testing.T) {
	storePath, jobID, _ := cliPausedRecurringJob(t)
	var output bytes.Buffer
	if err := runJobs([]string{"retry", "--store", storePath}, &output, &bytes.Buffer{}); err == nil {
		t.Fatal("retry without --id accepted")
	}
	if err := runJobs([]string{"retry", "--store", storePath, "--id", "nope"}, &output, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "bee jobs list") {
		t.Fatalf("missing job err = %v", err)
	}
	if err := runJobs([]string{"retry", "--store", storePath, "--id", jobID}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), "queued to run again") || !strings.Contains(output.String(), "persisted prompt and contract") {
		t.Fatalf("retry = %s, %v", output.String(), err)
	}
	inbox, err := bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(storePath))
	if err != nil {
		t.Fatal(err)
	}
	if list, err := inbox.List(context.Background(), 10); err != nil || list.UnreadCount != 0 {
		t.Fatalf("retry left notification unread: %#v, %v", list, err)
	}
	if err := runJobs([]string{"dismiss", "--store", storePath, "--id", jobID}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("dismiss of a queued retry err = %v", err)
	}

	storePath, jobID, _ = cliPausedRecurringJob(t)
	output.Reset()
	if err := runJobs([]string{"dismiss", "--store", storePath, "--id", jobID, "--reason", "wrong prompt"}, &output, &bytes.Buffer{}); err != nil || !strings.Contains(output.String(), "dismissed") {
		t.Fatalf("dismiss = %s, %v", output.String(), err)
	}
	store, _ := bee.NewJobStore(storePath)
	job, err := store.Get(jobID)
	if err != nil || job.State != bee.JobCanceled || job.LastError != "dismissed by owner: wrong prompt" {
		t.Fatalf("dismissed job = %#v, %v", job, err)
	}
}

func TestJobsRetryRefreshFromConfigUsesCurrentDefinition(t *testing.T) {
	storePath, jobID, start := cliPausedRecurringJob(t)
	config := writeTestConfig(t, "http://127.0.0.1:1", "test-model")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	recurring := `"recurring_jobs":{"jobs":[{"id":"nightly","enabled":true,"prompt":"fixed prompt","schedule":{"start_at":"` + start.Format(time.RFC3339) + `","interval_seconds":86400},"contract":{"allowed_tools":["read_file"],"workspace_paths":[` + strconv.Quote(workspace) + `],"max_wall_clock_seconds":30,"max_tool_calls":6,"output_target":"session","notification_target":"inbox"}}]},`
	if err := os.WriteFile(config, []byte(strings.Replace(string(data), "{", "{"+recurring, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runJobs([]string{"retry", "--store", storePath, "--config", config, "--id", jobID, "--refresh-from-config"}, &output, &bytes.Buffer{}); err != nil {
		t.Fatalf("refresh retry: %v", err)
	}
	store, _ := bee.NewJobStore(storePath)
	job, _ := store.Get(jobID)
	if job.Request.Prompt != "fixed prompt" || job.Contract.MaxToolCalls != 6 || job.Contract.WorkspacePaths[0] != workspace {
		t.Fatalf("refreshed = %#v", job)
	}

	storePath, jobID, _ = cliPausedRecurringJob(t)
	plain := writeTestConfig(t, "http://127.0.0.1:1", "test-model")
	if err := runJobs([]string{"retry", "--store", storePath, "--config", plain, "--id", jobID, "--refresh-from-config"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no longer in the config") {
		t.Fatalf("removed definition err = %v", err)
	}
}

func TestRunDueSummaryIsOneLinePerJob(t *testing.T) {
	var output bytes.Buffer
	writeRunDueSummary(&output, nil)
	if output.String() != "run-due: nothing due\n" {
		t.Fatalf("empty = %q", output.String())
	}
	started := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	finished := started.Add(212 * time.Second)
	output.Reset()
	writeRunDueSummary(&output, []bee.Job{
		{ID: "recurring-dreaming-1", State: bee.JobSucceeded, RecurringID: "dreaming", Occurrence: started, AttemptsLog: []bee.JobAttempt{{StartedAt: started, FinishedAt: &finished, SessionID: "abc"}}},
		{ID: "recurring-dreaming-2", State: bee.JobPaused, RecurringID: "dreaming", Occurrence: started, LastError: "job paused for owner approval", AttemptsLog: []bee.JobAttempt{{StartedAt: started, FinishedAt: &finished}}},
	})
	text := output.String()
	for _, want := range []string{"recurring-dreaming-1  succeeded  occurrence 2026-11-14T08:00:00Z  3m32s  session abc", "recurring-dreaming-2  paused", "    error: job paused for owner approval", "    next: bee jobs retry --id recurring-dreaming-2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "next:") != 1 {
		t.Fatalf("succeeded job got recovery hints:\n%s", text)
	}
}

func TestRecurringScopeContainsPrimaryConfigIncludingSymlinks(t *testing.T) {
	workspace := t.TempDir()
	config := filepath.Join(workspace, "config.json")
	if err := os.WriteFile(config, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !recurringScopeContains(config, []string{workspace}) {
		t.Fatal("workspace did not contain config")
	}
	outside := t.TempDir()
	if recurringScopeContains(config, []string{outside}) {
		t.Fatal("unrelated workspace contained config")
	}
	link := filepath.Join(outside, "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if !recurringScopeContains(config, []string{link}) {
		t.Fatal("symlinked workspace did not contain config")
	}
}

func TestRecurringContractRootsKeepReadOnlyPathsOutOfWriteRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	memory := filepath.Join(home, "workspace", "memory")
	sessions := filepath.Join(home, "state", "sessions")
	for _, dir := range []string{memory, sessions, filepath.Join(home, "secrets")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reference := filepath.Join(home, "workspace", "projects")
	if err := os.MkdirAll(reference, 0o700); err != nil {
		t.Fatal(err)
	}
	scope, write, err := recurringContractRoots(bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{reference}})
	if err != nil {
		t.Fatal(err)
	}
	if len(write) != 1 || write[0] != memory {
		t.Fatalf("write roots = %v", write)
	}
	if len(scope) != 2 || scope[0] != memory || scope[1] != reference {
		t.Fatalf("scope roots = %v", scope)
	}

	// Lexically disjoint but symlinked into the write root: still overlap.
	link := filepath.Join(t.TempDir(), "memory-link")
	if err := os.Symlink(memory, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recurringContractRoots(bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{link}}); err == nil {
		t.Fatal("symlinked read-only path overlapping the write root was accepted")
	}
	inner := filepath.Join(memory, "topics")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	innerLink := filepath.Join(t.TempDir(), "topics-link")
	if err := os.Symlink(inner, innerLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recurringContractRoots(bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{innerLink}}); err == nil {
		t.Fatal("read-only path resolving inside the write root was accepted")
	}
	// The reverse nesting (write root inside a read-only root) is allowed.
	scope, write, err = recurringContractRoots(bee.JobContract{WorkspacePaths: []string{inner}, ReadOnlyPaths: []string{memory}})
	if err != nil || len(write) != 1 || write[0] != inner || len(scope) != 2 || scope[1] != memory {
		t.Fatalf("nested roots scope=%v write=%v err=%v", scope, write, err)
	}
}

// Raw transcripts keep tool arguments/outputs verbatim and read_file does
// not redact, so no contract root may reach the session store or secrets in
// either direction; jobs read history through conversation_search instead.
func TestRecurringContractRootsRejectSecretsAndTranscriptStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	memory := filepath.Join(home, "workspace", "memory")
	sessions := filepath.Join(home, "state", "sessions")
	secrets := filepath.Join(home, "secrets")
	for _, dir := range []string{memory, sessions, secrets} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "sessions-link")
	if err := os.Symlink(sessions, link); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		contract bee.JobContract
		want     string
	}{
		"read-only sessions":         {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{sessions}}, "transcript"},
		"read-only inside sessions":  {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{filepath.Join(sessions, "one")}}, "transcript"},
		"read-only containing state": {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{filepath.Join(home, "state")}}, "transcript"},
		"read-only symlink":          {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{link}}, "transcript"},
		"workspace sessions":         {bee.JobContract{WorkspacePaths: []string{sessions}}, "transcript"},
		"read-only secrets":          {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{secrets}}, "secrets"},
		"read-only inside secrets":   {bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{filepath.Join(secrets, "keys")}}, "secrets"},
		"workspace containing home":  {bee.JobContract{WorkspacePaths: []string{home}}, "secrets"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := recurringContractRoots(test.contract); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
}

// During and after a G6 migration both the legacy XDG store and the new
// ~/.bee/state/sessions stay off-limits to job contracts.
func TestRecurringContractRootsRejectLegacyAndNewSessionStores(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", "")
	t.Setenv("BEE_SESSION_ROOT", "")
	t.Setenv("XDG_DATA_HOME", data)
	legacy := filepath.Join(data, "bee", "sessions")
	configured := filepath.Join(home, ".bee", "state", "sessions")
	memory := filepath.Join(home, ".bee", "workspace", "memory")
	for _, dir := range []string{legacy, configured, memory} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacy, "a.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{legacy, configured} {
		contract := bee.JobContract{WorkspacePaths: []string{memory}, ReadOnlyPaths: []string{root}}
		if _, _, err := recurringContractRoots(contract); err == nil || !strings.Contains(err.Error(), "transcript") {
			t.Fatalf("%s not refused: %v", root, err)
		}
	}
}
