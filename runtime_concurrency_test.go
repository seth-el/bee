package bee

import (
	"context"
	"errors"
	"testing"
	"time"
)

type turnLeaseProvider struct {
	started chan struct{}
	release <-chan struct{}
}

func (provider *turnLeaseProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.Stream(ctx, request, nil)
}

func (provider *turnLeaseProvider) Stream(ctx context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	select {
	case provider.started <- struct{}{}:
	default:
	}
	select {
	case <-provider.release:
		if onDelta != nil {
			_ = onDelta("ok")
		}
		return Response{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

func TestApplicationSerializesDurableSessionTurnsAcrossStoreInstances(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "serialized"})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	provider := &turnLeaseProvider{started: make(chan struct{}, 2), release: release}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	otherStore, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	otherApp, err := NewApplication(DefaultConfig(), provider, otherStore)
	if err != nil {
		t.Fatal(err)
	}

	events, err := app.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not start")
	}
	if _, err := otherApp.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "second"}); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("concurrent turn error = %v, want ErrSessionBusy", err)
	}
	if err := otherStore.Delete(session.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("delete during active turn error = %v, want ErrSessionBusy", err)
	}

	close(release)
	for range events {
	}

	events, err = otherApp.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "after release"})
	if err != nil {
		t.Fatalf("turn after release: %v", err)
	}
	for range events {
	}
}
