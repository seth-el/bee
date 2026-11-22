package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seth-el/bee"
)

func runJobs(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 {
		return errors.New("jobs subcommand is required")
	}
	switch args[0] {
	case "create":
		return jobsCreate(args[1:], output, errorOutput)
	case "list":
		return jobsList(args[1:], output)
	case "show":
		return jobsShow(args[1:], output)
	case "approve":
		return jobsApprove(args[1:], output)
	case "cancel":
		return jobsCancel(args[1:], output)
	case "recover":
		return jobsRecover(args[1:], output)
	case "retry":
		return jobsRetry(args[1:], output, errorOutput)
	case "dismiss":
		return jobsDismiss(args[1:], output, errorOutput)
	case "run-due":
		return jobsRunDue(args[1:], output, errorOutput)
	default:
		return fmt.Errorf("unknown jobs subcommand %q", args[0])
	}
}

func jobsFlags(name string, args []string, output io.Writer) (*flag.FlagSet, *string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	store := flags.String("store", "", "private job store path (default: the configured or default job store)")
	if err := flags.Parse(args); err != nil {
		return nil, nil, err
	}
	return flags, store, nil
}
func requireStore(path string) (*bee.JobStore, error) {
	return resolveJobStore(path, "")
}

// resolveJobStore opens the job store named by --store or, when it is empty,
// the store the composed assistant uses: assistant.job_store_path from the
// config, else the default under the Bee state directory. The config is read
// only for that one path, so a config that fails to load is reported rather
// than silently falling back to a different store.
func resolveJobStore(path, configPath string) (*bee.JobStore, error) {
	if strings.TrimSpace(path) != "" {
		return bee.NewJobStore(filepath.Clean(path))
	}
	if configPath == "" {
		var err error
		if configPath, err = bee.DefaultConfigPath(); err != nil {
			return nil, err
		}
	}
	if _, statErr := os.Stat(configPath); statErr == nil {
		config, err := bee.LoadConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("find the job store: %w (or pass --store)", err)
		}
		if configured := strings.TrimSpace(config.Assistant.JobStorePath); configured != "" {
			return bee.NewJobStore(configured)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("find the job store: %w (or pass --store)", statErr)
	}
	defaultPath, err := bee.DefaultBeeJobStorePath()
	if err != nil {
		return nil, err
	}
	return bee.NewJobStore(defaultPath)
}
func jobsCreate(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee jobs create", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	storePath := flags.String("store", "", "private job store path (default: the configured or default job store)")
	id := flags.String("id", "", "job ID")
	name := flags.String("name", "", "job name")
	owner := flags.String("owner", "", "job owner")
	runAt := flags.String("run-at", "", "RFC3339 run time")
	idem := flags.String("idempotency-key", "", "idempotency key")
	prompt := flags.String("prompt", "", "Bee prompt")
	max := flags.Int("max-attempts", 1, "maximum execution attempts")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := requireStore(*storePath)
	if err != nil {
		return err
	}
	when, err := time.Parse(time.RFC3339, *runAt)
	if err != nil {
		return errors.New("--run-at must be RFC3339")
	}
	job := bee.Job{ID: *id, Name: *name, Owner: *owner, IdempotencyKey: *idem, Request: bee.JobRequest{Prompt: *prompt, Title: *name}, Schedule: bee.Schedule{RunAt: when}, MaxAttempts: *max}
	if err := store.Create(job, time.Now().UTC()); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(job)
}
func jobsList(args []string, output io.Writer) error {
	flags, path, err := jobsFlags("bee jobs list", args, output)
	_ = flags
	if err != nil {
		return err
	}
	store, err := requireStore(*path)
	if err != nil {
		return err
	}
	jobs, err := store.List()
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(jobs)
}
func jobsShow(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("bee jobs show", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	id := flags.String("id", "", "job ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := requireStore(*path)
	if err != nil {
		return err
	}
	job, err := store.Get(*id)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(job)
}
func jobsApprove(args []string, output io.Writer) error {
	return jobsTransition(args, "approve", output, func(s *bee.JobStore, id string, now time.Time) error { return s.Approve(id, now) })
}
func jobsCancel(args []string, output io.Writer) error {
	return jobsTransition(args, "cancel", output, func(s *bee.JobStore, id string, now time.Time) error { return s.Cancel(id, now) })
}
func jobsTransition(args []string, name string, output io.Writer, transition func(*bee.JobStore, string, time.Time) error) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	id := flags.String("id", "", "job ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := requireStore(*path)
	if err != nil {
		return err
	}
	if err := transition(store, *id, time.Now().UTC()); err != nil {
		return err
	}
	job, err := store.Get(*id)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(job)
}
func jobsRecover(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("bee jobs recover", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	id := flags.String("id", "", "job ID")
	action := flags.String("action", "", "retry or fail")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := requireStore(*path)
	if err != nil {
		return err
	}
	if err := store.Recover(*id, *action, time.Now().UTC()); err != nil {
		return err
	}
	job, err := store.Get(*id)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(job)
}

func jobsRunDue(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee jobs run-due", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	configPath := flags.String("config", "", "Bee config path")
	profile := flags.String("profile", "", "explicit provider profile; never auto-selects cloud")
	nowFlag := flags.String("now", "", "RFC3339 clock override")
	jsonMode := flags.Bool("json", false, "emit the finished jobs as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := resolveJobStore(*path, *configPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if *nowFlag != "" {
		now, err = time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			return errors.New("--now must be RFC3339")
		}
	}
	config, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, line := range bee.UnknownConfigFieldWarnings(config) {
		fmt.Fprintln(errorOutput, line)
	}
	effectiveConfigPath := *configPath
	if effectiveConfigPath == "" {
		effectiveConfigPath, err = bee.DefaultConfigPath()
		if err != nil {
			return err
		}
	}
	sessions, err := bee.NewDefaultSessionStore()
	if err != nil {
		return err
	}
	app, err := bee.NewApplicationForProfile(config, *profile, sessions)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close(context.Background()) }()
	// Jobs never fall back to another model (G10): they fail and notify.
	app.DisableMainFallback()
	notificationPath := bee.NotificationStorePathForJobStore(store.Path())
	notifications, err := bee.NewNotificationInboxStore(notificationPath)
	if err != nil {
		return err
	}
	scheduler, err := bee.NewRecurringScheduler(config.RecurringJobs, store)
	if err != nil {
		return err
	}
	// Non-fatal: a cron wall time skipped or repeated by daylight saving is
	// the silent-failure class recurring contracts exist to prevent.
	for _, warning := range config.RecurringJobs.ScheduleWarnings(now) {
		fmt.Fprintf(errorOutput, "warning: %s\n", warning)
	}
	results, err := scheduler.Tick(context.Background(), now, func(ctx context.Context, job bee.Job) (bee.JobExecution, error) {
		if job.Contract == nil {
			return executeScheduledApplication(ctx, app, job)
		}
		return executeRecurringJob(ctx, config, effectiveConfigPath, *profile, sessions, store, notifications, job)
	}, bee.NewJobNotificationNotifier(notifications))
	if err != nil {
		return err
	}
	if *jsonMode {
		return json.NewEncoder(output).Encode(results)
	}
	writeRunDueSummary(output, results)
	return nil
}

// writeRunDueSummary prints one line per job that ran: id, occurrence, final
// state, duration, and for anything not successful the bounded error and the
// commands to inspect or recover it. Nothing due prints one line too, so a
// timer's journal shows the tick happened.
func writeRunDueSummary(output io.Writer, results []bee.Job) {
	if len(results) == 0 {
		fmt.Fprintln(output, "run-due: nothing due")
		return
	}
	for _, job := range results {
		line := fmt.Sprintf("%s  %s", job.ID, job.State)
		if !job.Occurrence.IsZero() {
			line += "  occurrence " + job.Occurrence.UTC().Format(time.RFC3339)
		}
		if n := len(job.AttemptsLog); n > 0 && job.AttemptsLog[n-1].FinishedAt != nil {
			last := job.AttemptsLog[n-1]
			line += "  " + last.FinishedAt.Sub(last.StartedAt).Round(time.Second).String()
			if last.SessionID != "" {
				line += "  session " + string(last.SessionID)
			}
		}
		fmt.Fprintln(output, line)
		if job.State != bee.JobSucceeded {
			if job.LastError != "" {
				fmt.Fprintf(output, "    error: %s\n", job.LastError)
			}
			if job.RecurringID != "" && (job.State == bee.JobPaused || job.State == bee.JobFailed) {
				fmt.Fprintf(output, "    next: bee jobs retry --id %s [--refresh-from-config] | bee jobs dismiss --id %s\n", job.ID, job.ID)
			}
		}
	}
}

func jobsRetry(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee jobs retry", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	configPath := flags.String("config", "", "Bee config path")
	id := flags.String("id", "", "job ID of the paused or failed occurrence")
	refresh := flags.Bool("refresh-from-config", false, "replace the occurrence's prompt and contract with its current definition in config")
	jsonMode := flags.Bool("json", false, "emit the job as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" || flags.NArg() != 0 {
		return errors.New("usage: bee jobs retry --id JOB_ID [--refresh-from-config] [--store PATH]")
	}
	store, err := resolveJobStore(*path, *configPath)
	if err != nil {
		return err
	}
	options := bee.JobRetryOptions{}
	if *refresh {
		current, getErr := store.Get(*id)
		if getErr != nil {
			return jobLookupError(*id, getErr)
		}
		config, loadErr := bee.LoadConfig(*configPath)
		if loadErr != nil {
			return loadErr
		}
		for index := range config.RecurringJobs.Jobs {
			if config.RecurringJobs.Jobs[index].ID == current.RecurringID {
				definition := config.RecurringJobs.Jobs[index]
				options.Refresh = &definition
				break
			}
		}
		if options.Refresh == nil {
			return fmt.Errorf("recurring job %q is no longer in the config; dismiss the occurrence instead", current.RecurringID)
		}
	}
	job, err := store.RetryRecurring(*id, options, time.Now().UTC())
	if err != nil {
		return jobLookupError(*id, err)
	}
	acknowledgeJobNotifications(store, job.ID, errorOutput)
	if *jsonMode {
		return json.NewEncoder(output).Encode(job)
	}
	detail := ""
	if n := len(job.OwnerActions); n > 0 {
		detail = " (" + job.OwnerActions[n-1].Detail + ")"
	}
	fmt.Fprintf(output, "%s queued to run again at the next `bee jobs run-due`%s\n", job.ID, detail)
	return nil
}

func jobsDismiss(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee jobs dismiss", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	path := flags.String("store", "", "private job store path (default: the configured or default job store)")
	configPath := flags.String("config", "", "Bee config path")
	id := flags.String("id", "", "job ID of the paused or failed occurrence")
	reason := flags.String("reason", "", "one-line reason recorded on the job")
	jsonMode := flags.Bool("json", false, "emit the job as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" || flags.NArg() != 0 {
		return errors.New("usage: bee jobs dismiss --id JOB_ID [--reason TEXT] [--store PATH]")
	}
	store, err := resolveJobStore(*path, *configPath)
	if err != nil {
		return err
	}
	job, err := store.DismissRecurring(*id, *reason, time.Now().UTC())
	if err != nil {
		return jobLookupError(*id, err)
	}
	acknowledgeJobNotifications(store, job.ID, errorOutput)
	if *jsonMode {
		return json.NewEncoder(output).Encode(job)
	}
	fmt.Fprintf(output, "%s dismissed; it will not run again\n", job.ID)
	return nil
}

func jobLookupError(id string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no job %q (ids are listed by `bee jobs list` and `bee inbox`)", id)
	}
	return err
}

// acknowledgeJobNotifications marks the job's inbox items read once the owner
// has acted on it. The job transition already committed, so a failure here
// is reported but not fatal.
func acknowledgeJobNotifications(store *bee.JobStore, jobID string, errorOutput io.Writer) {
	inbox, err := bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(store.Path()))
	if err == nil {
		_, err = inbox.MarkJobRead(context.Background(), jobID)
	}
	if err != nil {
		fmt.Fprintf(errorOutput, "warning: could not acknowledge the job's inbox notifications: %v\n", err)
	}
}

func executeScheduledApplication(ctx context.Context, app *bee.Application, job bee.Job) (bee.JobExecution, error) {
	events, err := app.Run(ctx, bee.RunRequest{Prompt: job.Request.Prompt, SessionID: job.Request.SessionID, Title: job.Request.Title, ProjectRoot: job.Request.ProjectRoot, DisableCommands: true})
	if err != nil {
		return bee.JobExecution{}, err
	}
	execution := bee.JobExecution{Summary: job.Request.Title}
	failed := false
	unreachableText := ""
	for event := range events {
		if execution.SessionID == "" && event.SessionID != "" {
			execution.SessionID = event.SessionID
		}
		if event.Type == bee.RunFailure {
			failed = true
			if event.ErrorKind == bee.ErrorKindModelUnreachable && unreachableText == "" {
				unreachableText = bee.RunFailureText(event)
			}
		}
	}
	if failed && unreachableText != "" {
		return execution, errors.New("scheduled run failed: " + unreachableText)
	}
	if failed {
		return execution, errors.New("scheduled run failed")
	}
	return execution, nil
}

func executeRecurringJob(parent context.Context, config bee.Config, configPath, profile string, sessions *bee.SessionStore, jobs *bee.JobStore, notifications *bee.NotificationInboxStore, job bee.Job) (bee.JobExecution, error) {
	contract := job.Contract
	if contract == nil {
		return bee.JobExecution{}, errors.New("recurring job contract is required")
	}
	if err := contract.Validate(); err != nil {
		return bee.JobExecution{}, fmt.Errorf("recurring job contract: %w", err)
	}
	config.Agent.MaxToolSteps = contract.MaxToolCalls
	config.Agent.TurnTimeoutSeconds = contract.MaxWallClockSeconds
	allow := make(map[string]struct{}, len(contract.AllowedTools))
	for _, name := range contract.AllowedTools {
		allow[name] = struct{}{}
	}
	if _, ok := allow["terminal"]; !ok {
		config.Terminal.Enabled = false
	}
	usesLSP := false
	for name := range allow {
		if strings.HasPrefix(name, "lsp_") {
			usesLSP = true
			break
		}
	}
	if !usesLSP {
		config.LSP = bee.LSPConfig{}
	}
	policy := config.Assistant.Policy
	if policy == "" {
		policy = bee.AssistantPolicyTrusted
	}
	if policy == bee.AssistantPolicyLocked && len(contract.AllowedTools) > 0 {
		return bee.JobExecution{}, errors.New("recurring job tools are unavailable under locked policy")
	}
	scopeRoots, writeRoots, err := recurringContractRoots(*contract)
	if err != nil {
		return bee.JobExecution{}, err
	}
	options := bee.AssistantCompositionOptions{
		Store: sessions, JobStore: jobs, NotificationStore: notifications,
		ScopeRoots: scopeRoots,
		WriteRoots: writeRoots,
		Policy:     policy, ApprovalMode: bee.ApprovalModeManual,
	}
	app, err := bee.ComposeGeneralAssistant(config, profile, options)
	if err != nil {
		return bee.JobExecution{}, err
	}
	defer func() { _ = app.Close(context.Background()) }()
	// Memory work runs on the configured model or fails and notifies.
	app.DisableMainFallback()
	if app.Executor != nil {
		app.Executor.Unattended = true
		if err := app.RestrictTools(contract.AllowedTools); err != nil {
			return bee.JobExecution{}, err
		}
		if recurringScopeContains(configPath, contract.WorkspacePaths) {
			for _, definition := range app.Executor.Registry.Definitions() {
				if definition.Mutability != bee.ToolMutabilityReadOnly {
					return bee.JobExecution{}, errors.New("recurring job contract cannot mutate a workspace containing the primary config")
				}
			}
		}
	} else if len(contract.AllowedTools) > 0 {
		return bee.JobExecution{}, errors.New("recurring job tool contract cannot be composed")
	}
	runCtx, cancel := context.WithTimeout(parent, time.Duration(contract.MaxWallClockSeconds)*time.Second)
	defer cancel()
	events, err := app.Run(runCtx, bee.RunRequest{Prompt: job.Request.Prompt, Title: job.Request.Title, ProjectRoot: contract.WorkspacePaths[0], DisableCommands: true})
	if err != nil {
		return bee.JobExecution{}, err
	}
	execution := bee.JobExecution{Summary: job.Request.Title}
	failed := false
	approvalRequired := false
	approvalReason := ""
	unreachableText := ""
	contractViolation := false
	for event := range events {
		if execution.SessionID == "" && event.SessionID != "" {
			execution.SessionID = event.SessionID
		}
		if event.Type == bee.RunApproval && event.Approval != nil && event.Approval.EffectiveStatus() != bee.ApprovalStatusApproved {
			approvalRequired = true
			if approvalReason == "" {
				approvalReason = strings.TrimSpace(event.Approval.Reason)
			}
			cancel()
		}
		if event.Type == bee.RunToolCall && event.ToolCall != nil {
			if _, ok := allow[event.ToolCall.Name]; !ok {
				contractViolation = true
				cancel()
			}
		}
		if event.Type == bee.RunFailure {
			failed = true
			if event.ErrorKind == bee.ErrorKindModelUnreachable && unreachableText == "" {
				unreachableText = bee.RunFailureText(event)
			}
		}
	}
	if approvalRequired {
		if approvalReason != "" {
			return execution, fmt.Errorf("%w: %s", bee.ErrJobApprovalRequired, approvalReason)
		}
		return execution, bee.ErrJobApprovalRequired
	}
	if contractViolation {
		return execution, errors.New("recurring job attempted a tool outside its contract")
	}
	if failed || runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return execution, errors.New("recurring job exceeded its wall-clock budget")
		}
		if unreachableText != "" {
			// Jobs never fall back to another model: memory work runs on the
			// configured one or not at all.
			return execution, errors.New("recurring job run failed: " + unreachableText)
		}
		return execution, errors.New("recurring job run failed")
	}
	return execution, nil
}

// recurringContractRoots maps a contract onto composition scopes. Workspace
// paths are both readable and writable; read-only paths are readable scope
// roots only and are never write roots. Overlap is re-checked through
// symlinks here (validation is lexical).
//
// No contract root may contain, or sit inside, the Bee secrets directory or
// the session transcript store. Session JSONL keeps tool arguments and
// outputs verbatim and read_file does not redact, so a headless job reads
// transcripts only through conversation_search, which projects redacted
// user/assistant excerpts (and is date-filterable with since/before).
func recurringContractRoots(contract bee.JobContract) ([]string, []string, error) {
	if err := recurringRejectProtectedRoots(append(append([]string(nil), contract.WorkspacePaths...), contract.ReadOnlyPaths...)); err != nil {
		return nil, nil, err
	}
	writeRoots := append([]string(nil), contract.WorkspacePaths...)
	scopeRoots := append([]string(nil), contract.WorkspacePaths...)
	for _, readOnly := range contract.ReadOnlyPaths {
		// Re-check through symlinks: a read-only root that resolves into a
		// write root would be writable. A write root nested inside a read-only
		// root is allowed; writes stay confined to the write root.
		if recurringScopeContains(readOnly, contract.WorkspacePaths) {
			return nil, nil, errors.New("recurring job contract read_only_paths resolves inside workspace_paths and would be writable")
		}
		scopeRoots = append(scopeRoots, readOnly)
	}
	return scopeRoots, writeRoots, nil
}

func recurringRejectProtectedRoots(roots []string) error {
	protected := []struct {
		name    string
		resolve func() (string, error)
	}{
		{"the Bee secrets directory", bee.DefaultBeeSecretsRoot},
	}
	for _, root := range bee.SessionStoreRoots() {
		root := root
		protected = append(protected, struct {
			name    string
			resolve func() (string, error)
		}{"the session transcript store (use conversation_search)", func() (string, error) { return root, nil }})
	}
	for _, item := range protected {
		path, err := item.resolve()
		if err != nil {
			continue
		}
		for _, root := range roots {
			if recurringScopeContains(path, []string{root}) || recurringScopeContains(root, []string{path}) {
				return fmt.Errorf("recurring job contract paths must not overlap %s", item.name)
			}
		}
	}
	return nil
}

func recurringScopeContains(path string, roots []string) bool {
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	for _, root := range roots {
		root, err = filepath.Abs(root)
		if err != nil {
			continue
		}
		if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = resolved
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}
