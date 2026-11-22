package bee

import (
	"context"
	"sync"
	"time"
)

// Main-model fallback (G10). With routing.fallback_main_profile set, an
// interactive turn whose main model server is unreachable before any reply
// is answered by the fallback profile instead, and the run says so. After a
// fallback, later calls go straight to the fallback for ProviderFallbackSticky
// before the main server is tried again, so a dead server does not cost a
// connection attempt on every call. Recurring jobs never fall back
// (Application.DisableMainFallback).

// ProviderFallbackSticky is how long Bee keeps using the fallback after the
// main server was unreachable.
const ProviderFallbackSticky = 2 * time.Minute

// EventProviderFallback journals each run that used the fallback.
const EventProviderFallback EventType = "provider_fallback"

// RunProviderFallback tells clients a turn is being answered by the
// fallback profile.
const RunProviderFallback RunEventType = "provider_fallback"

// ProviderFallbackEvent is bounded and content-free.
type ProviderFallbackEvent struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type providerFallbackNotifierKey struct{}

// withProviderFallbackNotifier lets the fallback provider report to the
// run that owns ctx.
func withProviderFallbackNotifier(ctx context.Context, notify func(ProviderFallbackEvent)) context.Context {
	return context.WithValue(ctx, providerFallbackNotifierKey{}, notify)
}

func notifyProviderFallback(ctx context.Context, event ProviderFallbackEvent) {
	if notify, ok := ctx.Value(providerFallbackNotifierKey{}).(func(ProviderFallbackEvent)); ok && notify != nil {
		notify(event)
	}
}

type fallbackProvider struct {
	primary      Provider
	primaryName  string
	fallbackName string
	// fallbackModel replaces the request's model on the fallback server.
	fallbackModel string
	newFallback   func() (Provider, error)

	once        sync.Once
	fallback    Provider
	fallbackErr error

	mu          sync.Mutex
	stickyUntil time.Time
	lastReason  string
	resolver    AttachmentArtifactResolver
	now         func() time.Time
}

func (provider *fallbackProvider) clock() time.Time {
	if provider.now != nil {
		return provider.now()
	}
	return time.Now()
}

func (provider *fallbackProvider) fallbackProvider() (Provider, error) {
	provider.once.Do(func() {
		created, err := provider.newFallback()
		provider.mu.Lock()
		provider.fallback, provider.fallbackErr = created, err
		resolver := provider.resolver
		provider.mu.Unlock()
		if err == nil && resolver != nil {
			if configurable, ok := created.(interface {
				SetAttachmentResolver(AttachmentArtifactResolver)
			}); ok {
				configurable.SetAttachmentResolver(resolver)
			}
		}
	})
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.fallback, provider.fallbackErr
}

// call runs one request on the main provider, or on the fallback when the
// main server is unreachable before replying (or was within the sticky
// window). A failure after output started never falls back.
func (provider *fallbackProvider) call(ctx context.Context, request Request, primary func(Provider, Request) (Response, error)) (Response, error) {
	provider.mu.Lock()
	sticky := provider.clock().Before(provider.stickyUntil)
	reason := provider.lastReason
	provider.mu.Unlock()
	if !sticky {
		response, err := primary(provider.primary, request)
		unreachable, isUnreachable := AsModelUnreachable(err)
		if err == nil || !isUnreachable || !providerRequestFailedBeforeResponse(err) || ctx.Err() != nil {
			return response, err
		}
		reason = unreachable.Error()
		provider.mu.Lock()
		provider.stickyUntil = provider.clock().Add(ProviderFallbackSticky)
		provider.lastReason = reason
		provider.mu.Unlock()
	}
	fallback, err := provider.fallbackProvider()
	if err != nil {
		return Response{}, err
	}
	if requestHasAttachments(request) && !providerSupportsImages(fallback) {
		// The fallback can't see images: report the real problem instead.
		return Response{}, &providerRequestError{cause: &ModelUnreachableError{Endpoint: provider.primaryName, Reason: reason + "; the fallback " + provider.fallbackName + " can't read the attached images"}}
	}
	notifyProviderFallback(ctx, ProviderFallbackEvent{From: provider.primaryName, To: provider.fallbackName, Reason: truncateUTF8(reason, 256)})
	request.Model = provider.fallbackModel
	return primary(fallback, request)
}

func (provider *fallbackProvider) Complete(ctx context.Context, request Request) (Response, error) {
	return provider.call(ctx, request, func(target Provider, request Request) (Response, error) {
		return target.Complete(ctx, request)
	})
}

func (provider *fallbackProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	return provider.call(ctx, request, func(target Provider, request Request) (Response, error) {
		return target.Stream(ctx, request, onDelta)
	})
}

func (provider *fallbackProvider) StreamWithEvents(ctx context.Context, request Request, onEvent StreamEventFunc) (Response, error) {
	return provider.call(ctx, request, func(target Provider, request Request) (Response, error) {
		if rich, ok := target.(RichStreamingProvider); ok {
			return rich.StreamWithEvents(ctx, request, onEvent)
		}
		return target.Stream(ctx, request, func(delta string) error {
			return onEvent(StreamEvent{Kind: StreamContentDelta, Delta: delta})
		})
	})
}

// ContextWindowTokens, SupportsImageInput and SetAttachmentResolver forward
// to the main provider (the resolver also reaches the fallback).
func (provider *fallbackProvider) ContextWindowTokens(model string) (int, bool) {
	if discoverer, ok := provider.primary.(ProviderContextWindow); ok {
		return discoverer.ContextWindowTokens(model)
	}
	return 0, false
}

func (provider *fallbackProvider) SupportsImageInput() bool {
	return providerSupportsImages(provider.primary)
}

func (provider *fallbackProvider) SetAttachmentResolver(resolver AttachmentArtifactResolver) {
	provider.mu.Lock()
	provider.resolver = resolver
	fallback := provider.fallback
	provider.mu.Unlock()
	for _, target := range []Provider{provider.primary, fallback} {
		if configurable, ok := target.(interface {
			SetAttachmentResolver(AttachmentArtifactResolver)
		}); ok {
			configurable.SetAttachmentResolver(resolver)
		}
	}
}

// DisableMainFallback makes this application use only its main provider
// (recurring jobs: memory work runs on the configured model or fails).
func (app *Application) DisableMainFallback() {
	if app == nil {
		return
	}
	if wrapped, ok := app.Provider.(*fallbackProvider); ok {
		app.Provider = wrapped.primary
	}
}

func requestHasAttachments(request Request) bool {
	for _, message := range request.Messages {
		if len(message.Attachments) > 0 {
			return true
		}
	}
	return false
}
