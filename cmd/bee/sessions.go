package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/seth-el/bee"
)

func runSessions(args []string, output, errorOutput io.Writer) error {
	if len(args) > 0 && args[0] == "migrate" {
		return runSessionsMigrate(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "export-history" {
		return runSessionsExportHistory(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "redact-file" {
		return runRedact(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "title-backfill" {
		return runSessionsTitleBackfill(args[1:], output, errorOutput)
	}
	if len(args) == 0 || args[0] != "repair" {
		return errors.New("sessions subcommand must be repair, migrate, export-history, redact-file or title-backfill")
	}
	flags := flag.NewFlagSet("bee sessions repair", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	sessionID := flags.String("session", "", "durable session ID (required)")
	root := flags.String("root", "", "session store root (defaults to Bee's private session root)")
	apply := flags.Bool("apply", false, "apply repair; default is dry-run")
	dryRun := flags.Bool("dry-run", false, "report repair without writing (default)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *sessionID == "" || flags.NArg() != 0 {
		return errors.New("sessions repair requires --session ID and no positional arguments")
	}
	if *apply && *dryRun {
		return errors.New("--apply and --dry-run cannot be combined")
	}
	var store *bee.SessionStore
	var err error
	if *root == "" {
		store, err = bee.NewDefaultSessionStore()
	} else {
		store, err = bee.NewSessionStore(*root)
	}
	if err != nil {
		return err
	}
	report, err := store.RepairSession(bee.SessionID(*sessionID), *apply && !*dryRun)
	if err != nil {
		return err
	}
	return encodeValue(output, report)
}

// runSessionsMigrate moves the legacy XDG session store to the configured
// root (~/.bee/state/sessions). Dry-run unless --apply.
func runSessionsMigrate(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee sessions migrate", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	apply := flags.Bool("apply", false, "move the store; default only reports")
	jsonMode := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("sessions migrate takes no positional arguments")
	}
	report, err := bee.MigrateLegacySessionStore(*apply)
	if *jsonMode {
		if encodeErr := encodeValue(output, report); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	switch report.Status {
	case "nothing_to_migrate":
		fmt.Fprintln(output, "no legacy session store to migrate")
	case "already_migrated":
		fmt.Fprintf(output, "sessions are already in %s\n", report.To)
	case "ready", "moved":
		verb := "would move"
		if report.Applied {
			verb = "moved"
		}
		fmt.Fprintf(output, "%s %d sessions (%d files, %.1f MB)\n  from %s\n  to   %s\n", verb, report.Sessions, report.Files, float64(report.Bytes)/(1<<20), report.From, report.To)
		if !report.Applied && err == nil {
			fmt.Fprintln(output, "run again with --apply to move them")
		}
		if report.Applied {
			fmt.Fprintln(output, "update any adapter that names the old path (e.g. bee-telegram session_root and its unit's ReadWritePaths)")
		}
	}
	return err
}

const historyReadme = `# Bee session history

A redacted copy of Bee's session transcripts, written by
` + "`bee sessions export-history`" + `. The raw transcripts in the session store
are never committed: they keep tool arguments and output verbatim. This copy
replaces every known secret value (files under ~/.bee/secrets and token/key
files named by ~/.bee/*.json) and secret-shaped strings. A secret that is in
neither, such as a password typed into chat, can still appear here.

Everything here is derived and can be regenerated from the session store.
See docs/operations/session-store.md in the bee repo.
`

// runSessionsExportHistory writes the redacted transcript copy and, with
// --commit, commits it to the history repo (created on first use).
func runSessionsExportHistory(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee sessions export-history", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	out := flags.String("out", "", "history directory (default ~/.bee/history)")
	commit := flags.Bool("commit", false, "commit the export to the history git repo, creating it if needed")
	jsonMode := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("sessions export-history takes no positional arguments")
	}
	root := *out
	if root == "" {
		var err error
		if root, err = bee.DefaultBeeHistoryRoot(); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(root) {
		return errors.New("--out must be an absolute path")
	}
	source, err := bee.DefaultBeeSessionRoot()
	if err != nil {
		return err
	}
	report, err := bee.ExportSessionHistory(source, root)
	if err != nil {
		return err
	}
	committed := ""
	if *commit {
		if committed, err = commitHistory(root, report); err != nil {
			return err
		}
	}
	if *jsonMode {
		return encodeValue(output, struct {
			bee.SessionHistoryReport
			Commit string `json:"commit,omitempty"`
		}{report, committed})
	}
	fmt.Fprintf(output, "exported %d sessions to %s: %d written, %d unchanged, %d removed; %d redactions (%d known secret values)\n", report.Sessions, filepath.Join(root, "sessions"), report.Written, report.Unchanged, report.Removed, report.Redactions, report.KnownSecrets)
	switch {
	case !*commit:
	case committed == "":
		fmt.Fprintln(output, "nothing to commit")
	default:
		fmt.Fprintf(output, "committed %s\n", committed)
	}
	return nil
}

func commitHistory(root string, report bee.SessionHistoryReport) (string, error) {
	run := func(args ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		stdout, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(stdout)), nil
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := run("init", "-q"); err != nil {
			return "", err
		}
	}
	readme := filepath.Join(root, "README.md")
	if _, err := os.Stat(readme); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(readme, []byte(historyReadme), 0o600); err != nil {
			return "", err
		}
	}
	if _, err := run("add", "-A", "--", "README.md", "sessions"); err != nil {
		return "", err
	}
	if status, err := run("status", "--porcelain", "--", "README.md", "sessions"); err != nil || status == "" {
		return "", err
	}
	message := fmt.Sprintf("Session history: %d sessions (%d updated, %d removed)", report.Sessions, report.Written, report.Removed)
	if _, err := run("commit", "-q", "-m", message, "--", "README.md", "sessions"); err != nil {
		return "", err
	}
	return run("rev-parse", "--short", "HEAD")
}

// runRedact writes a redacted copy of a file (bee sessions redact-file IN
// OUT) with the
// session-history redaction layers, for committing exports such as a chat
// archive.
func runRedact(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee sessions redact-file", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: bee sessions redact-file INPUT OUTPUT")
	}
	count, err := bee.RedactFileForHistory(flags.Arg(0), flags.Arg(1))
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s: %d redactions\n", flags.Arg(1), count)
	return nil
}

// runSessionsTitleBackfill titles older untitled sessions (and Telegram
// placeholders) with the child model. Dry run unless --apply.
func runSessionsTitleBackfill(args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("bee sessions title-backfill", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "Bee config path")
	apply := flags.Bool("apply", false, "make the model calls and save titles; default only lists candidates")
	limit := flags.Int("limit", 0, "title at most this many sessions (0 = all)")
	jsonMode := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("sessions title-backfill takes no positional arguments")
	}
	config, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, line := range bee.UnknownConfigFieldWarnings(config) {
		fmt.Fprintln(errorOutput, line)
	}
	store, err := bee.NewDefaultSessionStore()
	if err != nil {
		return err
	}
	app, err := bee.NewApplicationForProfile(config, "", store)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close(context.Background()) }()
	var progress func(bee.TitleBackfillItem)
	if !*jsonMode {
		progress = func(item bee.TitleBackfillItem) {
			if item.Title != "" {
				fmt.Fprintf(output, "%s  %s  %s\n", item.ID, item.Outcome, item.Title)
			} else {
				fmt.Fprintf(output, "%s  %s\n", item.ID, item.Outcome)
			}
		}
	}
	report, err := app.BackfillSessionTitles(context.Background(), *apply, *limit, progress)
	if *jsonMode {
		if encodeErr := encodeValue(output, report); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if err != nil {
		return err
	}
	if !*apply {
		fmt.Fprintf(output, "%d untitled session(s) would get a title; run with --apply to title them (one child-model call each)\n", report.Candidates)
		return nil
	}
	counts := map[string]int{}
	for _, item := range report.Items {
		counts[item.Outcome]++
	}
	fmt.Fprintf(output, "titled %d, failed %d, busy %d\n", counts["titled"], counts["failed"], counts["busy"])
	return nil
}
