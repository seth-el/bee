package bee

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type failingTitleProvider struct{ calls int }

func (provider *failingTitleProvider) Complete(context.Context, Request) (Response, error) {
	provider.calls++
	return Response{}, errors.New("child unavailable")
}

func (provider *failingTitleProvider) Stream(ctx context.Context, request Request, _ StreamFunc) (Response, error) {
	return provider.Complete(ctx, request)
}

func autoTitleApp(t *testing.T, child Provider) (*Application, *SessionStore) {
	t.Helper()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Specialist.Enabled = false
	app, err := NewApplication(config, &digestRuntimeProvider{specialistTestProvider{content: "main"}}, store)
	if err != nil {
		t.Fatal(err)
	}
	app.ChildProvider = child
	app.ChildProviderProfile = "test-child"
	app.ChildConfig = config
	app.ChildConfig.Provider.BaseURL = "http://127.0.0.1:18083"
	return app, store
}

func runTurn(t *testing.T, app *Application, request RunRequest) SessionID {
	t.Helper()
	events, err := app.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var id SessionID
	for event := range events {
		if event.SessionID != "" {
			id = event.SessionID
		}
		if event.Type == RunFailure {
			t.Fatalf("run failed: %s", event.Error)
		}
	}
	return id
}

func TestAutoTitleOncePerUntitledSession(t *testing.T) {
	child := &specialistTestProvider{content: "\"Title: Fixing the Telegram bot.\"\nextra line"}
	app, store := autoTitleApp(t, child)
	id := runTurn(t, app, RunRequest{Prompt: "my telegram bot stopped answering"})
	session, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.Metadata.Title != "Fixing the Telegram bot" || session.Metadata.TitleSource != TitleSourceAuto {
		t.Fatalf("metadata = %#v", session.Metadata)
	}
	if len(child.requests) != 1 || child.requests[0].Tools != nil || !child.requests[0].DisableThinking {
		t.Fatalf("title requests = %#v", child.requests)
	}
	runTurn(t, app, RunRequest{Prompt: "and another thing", SessionID: id})
	if len(child.requests) != 1 {
		t.Fatalf("title generated again: %d calls", len(child.requests))
	}
	summaries, err := store.List()
	if err != nil || len(summaries) != 1 || summaries[0].Title != "Fixing the Telegram bot" {
		t.Fatalf("summaries = %#v, %v", summaries, err)
	}
}

func TestAutoTitleNeverReplacesAnExistingTitle(t *testing.T) {
	child := &specialistTestProvider{content: "Something else"}
	app, store := autoTitleApp(t, child)
	id := runTurn(t, app, RunRequest{Prompt: "hello", Title: "Telegram · Nov 14"})
	session, _ := store.Load(id)
	if session.Metadata.Title != "Telegram · Nov 14" || session.Metadata.TitleSource != "" || len(child.requests) != 0 {
		t.Fatalf("metadata = %#v, calls = %d", session.Metadata, len(child.requests))
	}
}

func TestAutoTitleFailureLeavesSessionUntitledForGood(t *testing.T) {
	child := &failingTitleProvider{}
	app, store := autoTitleApp(t, child)
	id := runTurn(t, app, RunRequest{Prompt: "hello there"})
	session, _ := store.Load(id)
	if session.Metadata.Title != "" || session.Metadata.TitleSource != TitleSourceAutoFailed || child.calls != 1 {
		t.Fatalf("metadata = %#v, calls = %d", session.Metadata, child.calls)
	}
	runTurn(t, app, RunRequest{Prompt: "again", SessionID: id})
	if child.calls != 1 {
		t.Fatalf("failed title retried: %d calls", child.calls)
	}
}

func TestAutoTitleNeedsAChildProfile(t *testing.T) {
	child := &specialistTestProvider{content: "x"}
	app, store := autoTitleApp(t, child)
	app.ChildProviderProfile = ""
	id := runTurn(t, app, RunRequest{Prompt: "hello"})
	session, _ := store.Load(id)
	if session.Metadata.TitleSource != "" || len(child.requests) != 0 {
		t.Fatalf("titled without a child profile: %#v, calls = %d", session.Metadata, len(child.requests))
	}
}

func TestAutoTitleOffByConfig(t *testing.T) {
	child := &specialistTestProvider{content: "x"}
	app, store := autoTitleApp(t, child)
	off := false
	app.Config.Assistant.AutoTitleSessions = &off
	id := runTurn(t, app, RunRequest{Prompt: "hello"})
	session, _ := store.Load(id)
	if session.Metadata.Title != "" || session.Metadata.TitleSource != "" || len(child.requests) != 0 {
		t.Fatalf("metadata = %#v, calls = %d", session.Metadata, len(child.requests))
	}
}

func TestCleanAutoTitle(t *testing.T) {
	for raw, want := range map[string]string{
		"Fixing the Telegram bot":        "Fixing the Telegram bot",
		"**Title:** Home server backups": "Home server backups",
		"\"The memory dreaming pass\".":  "The memory dreaming pass",
		"  SICP study plan 🎓  ":          "SICP study plan",
		"Planning a very long and detailed migration of every single home lab service and more": "",
		"token=abc123secretvalue": "",
		"":                        "",
		"Notes on the history repository and the nightly push job setup": "Notes on the history repository and the nightly push job",
	} {
		got, ok := cleanAutoTitle(raw)
		if want == "" {
			if ok {
				t.Errorf("%q accepted as %q", raw, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q -> %q (%v), want %q", raw, got, ok, want)
		}
	}
}

func TestAutoTitleReplacesAProvisionalTitleKeepingItsLabel(t *testing.T) {
	child := &specialistTestProvider{content: "Disk usage on beta"}
	app, store := autoTitleApp(t, child)
	id := runTurn(t, app, RunRequest{Prompt: "how full is beta", Title: "Telegram · Nov 14", TitleProvisional: true})
	session, _ := store.Load(id)
	if session.Metadata.Title != "Telegram · Disk usage on beta" || session.Metadata.TitleSource != TitleSourceAuto {
		t.Fatalf("metadata = %#v", session.Metadata)
	}
}

func TestProvisionalTitleSurvivesATitleFailure(t *testing.T) {
	child := &failingTitleProvider{}
	app, store := autoTitleApp(t, child)
	id := runTurn(t, app, RunRequest{Prompt: "hello", Title: "Telegram · Nov 14", TitleProvisional: true})
	session, _ := store.Load(id)
	if session.Metadata.Title != "Telegram · Nov 14" || session.Metadata.TitleSource != TitleSourceAutoFailed || child.calls != 1 {
		t.Fatalf("metadata = %#v, calls = %d", session.Metadata, child.calls)
	}
}

func TestTitleBackfill(t *testing.T) {
	child := &specialistTestProvider{content: "Old conversation subject"}
	app, store := autoTitleApp(t, child)
	off := false
	app.Config.Assistant.AutoTitleSessions = &off
	untitled := runTurn(t, app, RunRequest{Prompt: "an old question"})
	owned := runTurn(t, app, RunRequest{Prompt: "named one", Title: "Owner title"})
	busy := runTurn(t, app, RunRequest{Prompt: "a busy one"})
	app.Config.Assistant.AutoTitleSessions = nil
	if len(child.requests) != 0 {
		t.Fatalf("titles generated while off: %d", len(child.requests))
	}
	before := map[SessionID]SessionSummary{}
	summaries, _ := store.List()
	for _, summary := range summaries {
		before[summary.ID] = summary
	}

	dry, err := app.BackfillSessionTitles(context.Background(), false, 0, nil)
	if err != nil || dry.Candidates != 2 || len(child.requests) != 0 {
		t.Fatalf("dry run = %#v, err=%v, calls=%d", dry, err, len(child.requests))
	}

	lease, err := store.acquireTurnLease(busy)
	if err != nil {
		t.Fatal(err)
	}
	report, err := app.BackfillSessionTitles(context.Background(), true, 0, nil)
	_ = lease.Close()
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[SessionID]string{}
	for _, item := range report.Items {
		outcomes[item.ID] = item.Outcome
	}
	if outcomes[untitled] != "titled" || outcomes[busy] != "busy" || outcomes[owned] != "" {
		t.Fatalf("outcomes = %#v", outcomes)
	}
	session, _ := store.Load(untitled)
	if session.Metadata.Title != "Old conversation subject" || session.Metadata.TitleSource != TitleSourceAuto {
		t.Fatalf("metadata = %#v", session.Metadata)
	}
	summaries, _ = store.List()
	for _, summary := range summaries {
		if !summary.UpdatedAt.Equal(before[summary.ID].UpdatedAt) {
			t.Fatalf("backfill moved %s: %v -> %v", summary.ID, before[summary.ID].UpdatedAt, summary.UpdatedAt)
		}
	}
	if owner, _ := store.Load(owned); owner.Metadata.Title != "Owner title" {
		t.Fatalf("owner title changed: %#v", owner.Metadata)
	}
}
