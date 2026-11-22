package bee

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// G4: an unattended job's out-of-roots write says what it tried and why,
// and the reason reaches the run's approval event.
func TestUnattendedOutOfRootWriteExplainsContract(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	memory := t.TempDir()
	if err := os.Chmod(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	topics := filepath.Join(memory, "topics")
	if err := os.MkdirAll(topics, 0o700); err != nil {
		t.Fatal(err)
	}
	daily := filepath.Join(memory, "2026-11-12.md")
	if err := os.WriteFile(daily, []byte("what happened\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(unattended bool) string {
		app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
			Store: mustAssistantStore(t), ScopeRoots: []string{topics, memory}, WriteRoots: []string{topics},
			MemoryRoot: memory, Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		app.Executor.Unattended = unattended
		args := `{"path":` + strconvQuote(daily) + `,"old_text":"what happened","new_text":"summary"}`
		app.Provider = &goldenScriptedProvider{responses: []GoldenProviderResponse{
			{ToolCalls: []ToolCall{{ID: "e1", Type: "function", Name: "edit_file", Arguments: args}}},
			{Content: "done"},
		}}
		events, err := app.Run(context.Background(), RunRequest{Prompt: "consolidate", DisableCommands: true})
		if err != nil {
			t.Fatal(err)
		}
		reason := ""
		for event := range events {
			if event.Type == RunApproval && event.Approval != nil && event.Approval.EffectiveStatus() != ApprovalStatusApproved && reason == "" {
				reason = event.Approval.Reason
			}
		}
		if data, err := os.ReadFile(daily); err != nil || string(data) != "what happened\n" {
			t.Fatalf("daily changed: %q, %v", data, err)
		}
		return reason
	}
	reason := run(true)
	resolvedTopics, _ := filepath.EvalSymlinks(topics)
	resolvedDaily, _ := filepath.EvalSymlinks(daily)
	for _, want := range []string{"edit_file", strconvQuote(resolvedDaily), "outside this job's write roots", strconvQuote(resolvedTopics), "jobs never auto-approve", "workspace_paths"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("unattended reason missing %q:\n%s", want, reason)
		}
	}
	if strings.Contains(reason, "interactive client") {
		t.Fatalf("unattended reason still suggests an interactive client: %s", reason)
	}
	if interactive := run(false); !strings.Contains(interactive, "requires approval but no approval hook is configured") || !strings.Contains(interactive, "use an interactive client") {
		t.Fatalf("interactive reason changed: %s", interactive)
	}
}

func TestUnattendedNonPathApprovalNamesOwner(t *testing.T) {
	executor := &Executor{Unattended: true, Scope: ExecutionScope{Root: t.TempDir()}}
	err := executor.noApprovalHookError(SkillEnableToolName, SkillEnableToolName, []byte(`{}`))
	if !strings.Contains(err.Error(), "scheduled jobs never auto-approve") || strings.Contains(err.Error(), "interactive client") {
		t.Fatalf("non-path unattended error = %v", err)
	}
}

func TestDescribeWriteTargetHidesSecretsAndBoundsLength(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	secrets, err := DefaultBeeSecretsRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: t.TempDir()}
	if got := describeWriteTarget(scope, filepath.Join(secrets, "telegram-bot-token"), true); strings.Contains(got, "telegram-bot-token") || strings.Contains(got, secrets) {
		t.Fatalf("secret path exposed: %s", got)
	}
	long := "/" + strings.Repeat("a", 600)
	if got := describeWriteTarget(scope, long, true); len(got) > maxDescribedPathBytes+16 {
		t.Fatalf("long path not bounded: %d bytes", len(got))
	}
}

func TestPausedJobKeepsApprovalReasonInErrorAndInbox(t *testing.T) {
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
	reason := `tool "edit_file" authorization denied at approval: edit_file target "/m/2026-11-12.md" is outside this job's write roots ("/m/topics"); recovery: jobs never auto-approve`
	results, err := scheduler.Tick(context.Background(), start, func(context.Context, Job) (JobExecution, error) {
		return JobExecution{SessionID: "s"}, fmt.Errorf("%w: %s", ErrJobApprovalRequired, reason)
	}, NewJobNotificationNotifier(notifications))
	if err != nil || len(results) != 1 || results[0].State != JobPaused {
		t.Fatalf("results = %#v, %v", results, err)
	}
	want := ErrJobApprovalRequired.Error() + ": " + reason
	if results[0].LastError != want {
		t.Fatalf("LastError = %q", results[0].LastError)
	}
	inbox, err := notifications.List(context.Background(), 10)
	if err != nil || len(inbox.Items) != 1 {
		t.Fatalf("inbox = %#v, %v", inbox, err)
	}
	if got, err := notifications.NotificationErrorFor(context.Background(), inbox.Items[0].ID); err != nil || got != want {
		t.Fatalf("inbox reason = %q, %v", got, err)
	}
	if !errors.Is(fmt.Errorf("%w: x", ErrJobApprovalRequired), ErrJobApprovalRequired) {
		t.Fatal("wrapped pause must still be recognized")
	}
}
