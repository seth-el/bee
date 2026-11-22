package bee

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type observabilityProvider struct{}

func (observabilityProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return observabilityProvider{}.Stream(ctx, request, nil)
}

func (observabilityProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	if onDelta != nil {
		_ = onDelta("observed")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "observed"}}, nil
}

func TestApplicationEmitsContentFreeRunDiagnosticsAndMetrics(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "metrics"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), observabilityProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	app.Diagnostics = NewWriterDiagnosticSink(&output)
	events, err := app.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "private prompt must not be logged"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	snapshot := app.Metrics.Snapshot()
	if snapshot.RunsStarted != 1 || snapshot.RunsSucceeded != 1 || snapshot.ActiveRuns != 0 || snapshot.RunsFailed != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
	logged := output.String()
	if !strings.Contains(logged, `"status":"started"`) || !strings.Contains(logged, `"status":"succeeded"`) {
		t.Fatalf("diagnostics = %q", logged)
	}
	if strings.Contains(logged, "private prompt") || strings.Contains(logged, "observed") {
		t.Fatalf("diagnostics leaked content: %q", logged)
	}
}

func TestGeneralAssistantOwnsPrivateDefaultDiagnosticSink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	store, err := NewSessionStore(filepath.Join(home, "state", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: store, ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if app.Diagnostics == nil || !app.ownsDiagnostics {
		t.Fatal("general assistant did not own its default diagnostic sink")
	}
	path, err := DefaultBeeRuntimeDiagnosticsPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostic file mode = %v", info.Mode())
	}
	if err := app.CloseExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !app.ownsDiagnostics {
		t.Fatal("closing extensions closed the application diagnostic sink")
	}
	if err := app.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
