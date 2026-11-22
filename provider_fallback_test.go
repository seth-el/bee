package bee

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type scriptedFallbackTarget struct {
	err      error
	content  string
	requests []Request
}

func (provider *scriptedFallbackTarget) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.Stream(ctx, request, nil)
}

func (provider *scriptedFallbackTarget) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	if provider.err != nil {
		return Response{}, provider.err
	}
	if onDelta != nil {
		_ = onDelta(provider.content)
	}
	return Response{Message: Message{Role: RoleAssistant, Content: provider.content}}, nil
}

func unreachableErr() error {
	return &providerRequestError{cause: &ModelUnreachableError{Endpoint: "203.0.113.37:8081", Reason: "connection refused"}}
}

func newTestFallback(primary, fallback Provider, clock *steppedClock) *fallbackProvider {
	return &fallbackProvider{primary: primary, primaryName: "local-main-8081", fallbackName: "local-child-8083", fallbackModel: "gemma",
		newFallback: func() (Provider, error) { return fallback, nil }, now: clock.Now}
}

func TestFallbackAnswersWhenMainIsUnreachableAndSticks(t *testing.T) {
	clock := &steppedClock{now: time.Unix(1000, 0)}
	primary := &scriptedFallbackTarget{err: unreachableErr()}
	fallback := &scriptedFallbackTarget{content: "from gemma"}
	provider := newTestFallback(primary, fallback, clock)
	var notices []ProviderFallbackEvent
	ctx := withProviderFallbackNotifier(context.Background(), func(event ProviderFallbackEvent) { notices = append(notices, event) })

	response, err := provider.Stream(ctx, Request{Model: "local"}, nil)
	if err != nil || response.Message.Content != "from gemma" || fallback.requests[0].Model != "gemma" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if len(notices) != 1 || notices[0].From != "local-main-8081" || notices[0].To != "local-child-8083" || !strings.Contains(notices[0].Reason, "connection refused") {
		t.Fatalf("notices = %#v", notices)
	}
	// Within the sticky window the dead main server is not tried again.
	clock.advance(time.Minute)
	if _, err := provider.Stream(ctx, Request{Model: "local"}, nil); err != nil || len(primary.requests) != 1 || len(fallback.requests) != 2 {
		t.Fatalf("sticky call: err=%v primary=%d fallback=%d", err, len(primary.requests), len(fallback.requests))
	}
	// After it, the main server gets another chance.
	clock.advance(ProviderFallbackSticky)
	primary.err, primary.content = nil, "from glm"
	if response, err := provider.Stream(ctx, Request{Model: "local"}, nil); err != nil || response.Message.Content != "from glm" || primary.requests[1].Model != "local" {
		t.Fatalf("recovered call = %#v, %v", response, err)
	}
}

func TestFallbackSkippedForImagesTheFallbackCannotRead(t *testing.T) {
	fallback := &scriptedFallbackTarget{content: "blind"}
	provider := newTestFallback(&scriptedFallbackTarget{err: unreachableErr()}, fallback, &steppedClock{now: time.Unix(0, 0)})
	request := Request{Messages: []Message{{Role: RoleUser, Content: "what is this", Attachments: []AttachmentRef{{ID: "a1"}}}}}
	_, err := provider.Stream(context.Background(), request, nil)
	if _, ok := AsModelUnreachable(err); !ok || !strings.Contains(err.Error(), "can't read the attached images") || len(fallback.requests) != 0 {
		t.Fatalf("err = %v, fallback calls = %d", err, len(fallback.requests))
	}
}

func TestFallbackOnlyForUnreachableBeforeReply(t *testing.T) {
	clock := &steppedClock{now: time.Unix(1000, 0)}
	for name, primaryErr := range map[string]error{
		"other error":           errors.New("HTTP 400: context too long"),
		"unreachable mid-reply": fmt.Errorf("provider stream: %w", &ModelUnreachableError{Endpoint: "x", Reason: "stopped responding"}),
	} {
		fallback := &scriptedFallbackTarget{content: "should not run"}
		provider := newTestFallback(&scriptedFallbackTarget{err: primaryErr}, fallback, clock)
		if _, err := provider.Stream(context.Background(), Request{}, nil); err == nil || len(fallback.requests) != 0 {
			t.Fatalf("%s: err=%v fallback calls=%d", name, err, len(fallback.requests))
		}
	}
}

func TestRunAnnouncesAndJournalsFallbackOnce(t *testing.T) {
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Specialist.Enabled = false
	clock := &steppedClock{now: time.Unix(1000, 0)}
	fallback := &scriptedFallbackTarget{content: "answered anyway"}
	app, err := NewApplication(config, newTestFallback(&scriptedFallbackTarget{err: unreachableErr()}, fallback, clock), store)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var id SessionID
	fallbacks, answered := 0, false
	for event := range events {
		if event.SessionID != "" {
			id = event.SessionID
		}
		if event.Type == RunProviderFallback {
			fallbacks++
		}
		if event.Type == RunFailure {
			t.Fatalf("run failed: %s", event.Error)
		}
		if event.Type == RunEnd && event.Response != nil && event.Response.Message.Content == "answered anyway" {
			answered = true
		}
	}
	if fallbacks != 1 || !answered {
		t.Fatalf("fallback events = %d, answered = %v", fallbacks, answered)
	}
	session, _ := store.Load(id)
	journaled := 0
	for _, event := range session.Events {
		if event.Type == EventProviderFallback {
			journaled++
		}
	}
	if journaled != 1 {
		t.Fatalf("journaled %d fallback events", journaled)
	}
	app.DisableMainFallback()
	if _, wrapped := app.Provider.(*fallbackProvider); wrapped {
		t.Fatal("DisableMainFallback left the wrapper in place")
	}
}

func TestFallbackRoutingValidationAndComposition(t *testing.T) {
	base := DefaultConfig()
	base.Profiles = []ProviderProfile{
		{Name: "main", Role: ProviderRoleMain, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:18081", Model: "glm"}},
		{Name: "child", Role: ProviderRoleChild, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:18083", Model: "gemma"}},
		{Name: "cloud", Role: ProviderRoleManual, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "https://example.invalid", Model: "x"}},
	}
	for fallback, want := range map[string]string{"main": "must differ", "cloud": "manual", "nope": "does not exist"} {
		config := base
		config.Routing = ProviderRoutingConfig{MainProfile: "main", ChildProfile: "child", ManualProfiles: []string{"cloud"}, FallbackMainProfile: fallback}
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fallback %q: err = %v", fallback, err)
		}
	}
	config := base
	config.Routing = ProviderRoutingConfig{MainProfile: "main", ChildProfile: "child", ManualProfiles: []string{"cloud"}, FallbackMainProfile: "child"}
	store, _ := NewSessionStore(t.TempDir())
	app, err := NewApplicationForProfile(config, "", store)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := app.Provider.(*fallbackProvider)
	if !ok || wrapped.fallbackName != "child" || wrapped.fallbackModel != "gemma" {
		t.Fatalf("provider = %#v", app.Provider)
	}
	// An explicit --profile never falls back.
	if explicit, err := NewApplicationForProfile(config, "main", store); err != nil {
		t.Fatal(err)
	} else if _, ok := explicit.Provider.(*fallbackProvider); ok {
		t.Fatal("explicit profile got a fallback")
	}
}

func TestLifecycleRecordsFallbackWithoutChangingState(t *testing.T) {
	store, _ := NewSessionStore(t.TempDir())
	app, _ := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	registry := newOpenAIWebRunRegistry(app)
	registry.publish("c", RunEvent{Type: RunStart, RunID: "r"})
	item, ok := registry.publish("c", RunEvent{Type: RunProviderFallback, RunID: "r", ProviderFallback: &ProviderFallbackEvent{From: "local-main-8081", To: "local-child-8083", Reason: "connection refused"}})
	if !ok || item.State != openAIWebRunRunning || item.FallbackProfile != "local-child-8083" || item.Reason != "" {
		t.Fatalf("fallback record = %#v", item)
	}
}
