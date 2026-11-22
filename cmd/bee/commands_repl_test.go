package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/seth-el/bee"
)

type steeringREPLProvider struct {
	mu       sync.Mutex
	requests []bee.Request
}

func (*steeringREPLProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, nil
}

func (provider *steeringREPLProvider) Stream(ctx context.Context, request bee.Request, onDelta bee.StreamFunc) (bee.Response, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	call := len(provider.requests)
	provider.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
		return bee.Response{}, ctx.Err()
	}
	if onDelta != nil {
		_ = onDelta("corrected")
	}
	return bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: "corrected"}}, nil
}

func (provider *steeringREPLProvider) snapshot() []bee.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]bee.Request(nil), provider.requests...)
}

func TestTerminalREPLUsesSharedCommandsForHelpAndUnknownSlash(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output markedTerminalBuffer
	if err := repl(app, strings.NewReader("/help\n/nope\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"/status", "/help", "unknown command"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output=%q missing %q", got, want)
		}
	}
	if len(provider.requests) != 0 {
		t.Fatalf("local slash commands unexpectedly ran provider: %#v", provider.requests)
	}
}

func TestTerminalREPLKeepsSlashCommandsOutOfBufferedStatusOutput(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := repl(app, strings.NewReader("/status\n/exit\n"), &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "you> you> "; got != want {
		t.Fatalf("buffered status output=%q, want %q", got, want)
	}
}

func TestTerminalREPLSteersAnActiveResponseInTheSameSession(t *testing.T) {
	provider := &steeringREPLProvider{}
	app, err := bee.NewApplication(bee.DefaultConfig(), provider, mustTestSessionStore(t))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	input := strings.NewReader("take the long route\n/steer use the short route\n/exit\n")
	if err := repl(app, input, &output, bee.RunRequest{}); err != nil {
		t.Fatal(err)
	}
	requests := provider.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests=%d, want 2: %#v", len(requests), requests)
	}
	var userPrompts []string
	for _, message := range requests[1].Messages {
		if message.Role == bee.RoleUser {
			userPrompts = append(userPrompts, message.Content)
		}
	}
	if got := strings.Join(userPrompts, " | "); got != "take the long route | use the short route" {
		t.Fatalf("steered provider user prompts=%q", got)
	}
	for _, want := range []string{"steering current response", "corrected"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output=%q missing %q", output.String(), want)
		}
	}
}
