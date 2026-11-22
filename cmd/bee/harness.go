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

	"github.com/seth-el/bee"
)

func runHarness(args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 || args[0] != "inspect" {
		return errors.New("usage: bee harness inspect [--config PATH --profile NAME --model MODEL --json --strict]")
	}
	flags := flag.NewFlagSet("bee harness inspect", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "path to the primary JSON config file")
	profile := flags.String("profile", "", "explicit provider profile")
	model := flags.String("model", "", "explicit model override")
	scopeRoot := flags.String("scope-root", "", "personal assistant readable filesystem scope")
	writeRoot := flags.String("write-root", "", "preferred automatically writable filesystem root")
	codingProjectRoot := flags.String("coding-project-root", "", "explicit coding repository/project root")
	codingManagedRoot := flags.String("coding-managed-root", "", "parent-owned Bee worktree/artifact root")
	codingBaseCommit := flags.String("coding-base-commit", "", "parent-approved Git base commit")
	fullMachine := flags.Bool("full-machine", false, "explicitly allow the full machine filesystem scope")
	policy := flags.String("policy", "", "assistant policy: trusted, prompted, or locked")
	planMode := flags.Bool("plan", false, "enable plan mode in the inspected composition")
	noPlanMode := flags.Bool("no-plan", false, "disable plan mode in the inspected composition")
	jsonMode := flags.Bool("json", false, "emit JSON")
	strict := flags.Bool("strict", false, "treat lint warnings as failures")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("bee harness inspect does not accept positional arguments")
	}
	if *planMode && *noPlanMode {
		return errors.New("--plan and --no-plan cannot be combined")
	}
	config, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	temporaryRoot, err := os.MkdirTemp("", "bee-harness-inspect-")
	if err != nil {
		return fmt.Errorf("create harness inspection state: %w", err)
	}
	defer os.RemoveAll(temporaryRoot)
	store, err := bee.NewSessionStore(filepath.Join(temporaryRoot, "sessions"))
	if err != nil {
		return err
	}
	jobStore, err := bee.NewJobStore(filepath.Join(temporaryRoot, "jobs", "jobs.json"))
	if err != nil {
		return err
	}
	notifications, err := bee.NewNotificationInboxStore(filepath.Join(temporaryRoot, "notifications", "inbox.json"))
	if err != nil {
		return err
	}
	todos, err := bee.NewTodoStore(filepath.Join(temporaryRoot, "todos", "todos.json"))
	if err != nil {
		return err
	}
	app, err := bee.ComposeGeneralAssistant(config, *profile, bee.AssistantCompositionOptions{
		Store: store, JobStore: jobStore, NotificationStore: notifications, TodoStore: todos,
		ScopeRoot: *scopeRoot, WriteRoot: *writeRoot, FullMachine: *fullMachine,
		MemoryRoot: filepath.Join(temporaryRoot, "memory"), Policy: bee.AssistantPolicyMode(*policy),
		CodingProjectRoot: *codingProjectRoot, CodingManagedRoot: *codingManagedRoot, CodingBaseCommit: *codingBaseCommit,
		DisableDefaultDiagnostics: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close(context.Background()) }()
	if *model != "" {
		selectedModel := strings.TrimSpace(*model)
		if selectedModel == "" {
			return errors.New("--model must not be blank")
		}
		app.Config.Provider.Model = selectedModel
	}
	if *planMode || *noPlanMode {
		if *planMode && app.Executor == nil {
			return errors.New("plan mode is unavailable for this assistant composition")
		}
		app.RequirePlan = *planMode
	}
	if app.Executor != nil {
		if err := app.StartExtensions(context.Background()); err != nil {
			return fmt.Errorf("start effective harness extensions: %w", err)
		}
	}
	snapshot, err := bee.BuildHarnessSnapshot(context.Background(), app)
	if err != nil {
		return err
	}
	if *jsonMode {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(snapshot); err != nil {
			return err
		}
	} else if _, err := io.WriteString(output, snapshot.Human()); err != nil {
		return err
	}
	if snapshot.Lint.Errors > 0 || (*strict && snapshot.Lint.Warnings > 0) {
		return fmt.Errorf("effective harness lint reported %s", snapshot.Lint.Status)
	}
	return nil
}
