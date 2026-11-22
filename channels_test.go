package bee

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRouteKeyIsDeterministicAndThreadScoped(t *testing.T) {
	first, err := NewSessionRouteKey("telegram", "bot-a", "chat-1", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSessionRouteKey("telegram", "bot-a", "chat-1", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Validate() != nil {
		t.Fatalf("route keys are not stable: %q %q", first, second)
	}
	otherThread, err := NewSessionRouteKey("telegram", "bot-a", "chat-1", "thread-2")
	if err != nil {
		t.Fatal(err)
	}
	if first == otherThread {
		t.Fatal("thread identity was not included in route key")
	}
	if _, err := NewSessionRouteKey("telegram/bad", "bot-a", "chat-1", ""); err == nil {
		t.Fatal("unsafe route component accepted")
	}
}

func TestChannelContractsRejectSecretsPathsAndUnboundedMetadata(t *testing.T) {
	if err := (AttachmentRef{Reference: "/tmp/secret.wav"}).Validate(); err == nil {
		t.Fatal("arbitrary attachment path accepted")
	}
	if err := (AttachmentRef{Reference: "provider-ref", Filename: "../secret.txt"}).Validate(); err == nil {
		t.Fatal("unsafe filename accepted")
	}
	if err := (AttachmentRef{Reference: "provider-ref", ContentType: "text/plain; secret=x"}).Validate(); err == nil {
		t.Fatal("invalid content type accepted")
	}
	if err := (InboundMessage{ChannelID: "x", AccountID: "a", ConversationID: "c", MessageID: "m", Text: "hello", Metadata: ChannelMetadata{"api_key": "secret"}}).Validate(); err == nil {
		t.Fatal("secret metadata accepted")
	}
	if err := (InboundMessage{ChannelID: "x", AccountID: "a", ConversationID: "c", MessageID: "m", Text: strings.Repeat("x", MaxChannelTextBytes+1)}).Validate(); err == nil {
		t.Fatal("oversized message accepted")
	}
	if err := (AttachmentRef{Reference: `provider\ref`}).Validate(); err == nil {
		t.Fatal("attachment reference containing a backslash accepted")
	}
}

func TestSessionRouteKeyValidatesDecodedCanonicalComponents(t *testing.T) {
	valid, err := NewSessionRouteKey("telegram", "bot-a", "chat-1", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid.String(), ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte("../bad"))
	if err := SessionRouteKey(strings.Join(parts, ".")).Validate(); err == nil {
		t.Fatal("unsafe decoded channel component accepted")
	}
	parts = strings.Split(valid.String(), ".")
	parts[1] = "YR" // decodes to "a" but is not its canonical raw URL encoding
	if err := SessionRouteKey(strings.Join(parts, ".")).Validate(); err == nil {
		t.Fatal("noncanonical route component accepted")
	}
}

func TestLocalRouteBindingStoreRoundTripConflictAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "route-bindings.json")
	store, err := NewLocalRouteBindingStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewSessionRouteKey("fake", "account", "conversation", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(context.Background(), key); !errors.Is(err, ErrRouteBindingNotFound) {
		t.Fatalf("missing binding error=%v", err)
	}
	if err := store.Bind(context.Background(), key, "session-a"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Lookup(context.Background(), key); err != nil || got != "session-a" {
		t.Fatalf("binding=%q err=%v", got, err)
	}
	if err := store.Bind(context.Background(), key, "session-a"); err != nil {
		t.Fatalf("idempotent bind failed: %v", err)
	}
	if err := store.Bind(context.Background(), key, "session-b"); !errors.Is(err, ErrRouteBindingConflict) {
		t.Fatalf("conflicting bind error=%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions=%o, want 600", info.Mode().Perm())
	}
	if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".route-bindings-*.tmp")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files=%v err=%v", leftovers, err)
	}

	otherKey, err := NewSessionRouteKey("fake", "account", "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind(context.Background(), otherKey, "session-c"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded) != 2 {
		t.Fatalf("state=%s err=%v", data, err)
	}
	if err := store.Bind(context.Background(), SessionRouteKey("unsafe"), "session-d"); err == nil {
		t.Fatal("unsafe route key accepted")
	}
	if err := store.Bind(context.Background(), otherKey, SessionID(strings.Repeat("x", MaxRouteBindingSessionIDBytes+1))); err == nil {
		t.Fatal("oversized session id accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Lookup(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error=%v", err)
	}
}

func TestLocalRouteBindingStoreRejectsMalformedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route-bindings.json")
	if err := os.WriteFile(path, []byte(`{"not-a-route":"session"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalRouteBindingStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewSessionRouteKey("fake", "account", "conversation", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(context.Background(), key); err == nil || strings.Contains(err.Error(), ErrRouteBindingNotFound.Error()) {
		t.Fatalf("malformed state lookup error=%v", err)
	}
	if err := store.Bind(context.Background(), key, "session"); err == nil {
		t.Fatal("bind replaced malformed state")
	}
}

func TestLocalRouteBindingStoreRejectsUnsafeOrOversizedState(t *testing.T) {
	key, err := NewSessionRouteKey("fake", "account", "conversation", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mode     os.FileMode
		contents string
		want     string
	}{
		"permissions": {mode: 0o644, contents: `{}`, want: "permissions"},
		"size":        {mode: 0o600, contents: strings.Repeat("x", MaxRouteBindingStateBytes+1), want: "exceeds"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "route-bindings.json")
			if err := os.WriteFile(path, []byte(tc.contents), tc.mode); err != nil {
				t.Fatal(err)
			}
			store, err := NewLocalRouteBindingStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Lookup(context.Background(), key); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe state error=%v", err)
			}
		})
	}
}

func TestChannelRouterRecoversDurableRouteBindingAfterRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "route-bindings.json")
	sessionRoot := filepath.Join(root, "sessions")
	provider1 := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "first"}}}}
	store1, err := NewSessionStore(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	app1, err := NewApplication(DefaultConfig(), provider1, store1)
	if err != nil {
		t.Fatal(err)
	}
	channel1, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	routeStore1, err := NewLocalRouteBindingStore(path)
	if err != nil {
		t.Fatal(err)
	}
	router1, err := NewChannelRouter(app1, []Channel{channel1}, ChannelRouterOptions{RouteBindings: routeStore1})
	if err != nil {
		t.Fatal(err)
	}
	inbound := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message-1", Text: "hello"}
	first, err := router1.Handle(context.Background(), inbound)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == "" {
		t.Fatal("first route did not create a session")
	}

	provider2 := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "second"}}}}
	store2, err := NewSessionStore(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	app2, err := NewApplication(DefaultConfig(), provider2, store2)
	if err != nil {
		t.Fatal(err)
	}
	channel2, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	routeStore2, err := NewLocalRouteBindingStore(path)
	if err != nil {
		t.Fatal(err)
	}
	router2, err := NewChannelRouter(app2, []Channel{channel2}, ChannelRouterOptions{RouteBindings: routeStore2})
	if err != nil {
		t.Fatal(err)
	}
	inbound.MessageID = "message-2"
	second, err := router2.Handle(context.Background(), inbound)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("restart did not recover session: first=%q second=%q", first.SessionID, second.SessionID)
	}
}

func TestChannelRouterNilRouteBindingStoreRemainsProcessLocal(t *testing.T) {
	root := t.TempDir()
	provider1 := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "first"}}}}
	store1, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	app1, err := NewApplication(DefaultConfig(), provider1, store1)
	if err != nil {
		t.Fatal(err)
	}
	channel1, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	router1, err := NewChannelRouter(app1, []Channel{channel1}, ChannelRouterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inbound := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message-1", Text: "hello"}
	first, err := router1.Handle(context.Background(), inbound)
	if err != nil {
		t.Fatal(err)
	}

	provider2 := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "second"}}}}
	store2, err := NewSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	app2, err := NewApplication(DefaultConfig(), provider2, store2)
	if err != nil {
		t.Fatal(err)
	}
	channel2, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	router2, err := NewChannelRouter(app2, []Channel{channel2}, ChannelRouterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inbound.MessageID = "message-2"
	second, err := router2.Handle(context.Background(), inbound)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == "" || second.SessionID == "" || first.SessionID == second.SessionID {
		t.Fatalf("nil route store unexpectedly recovered binding: first=%q second=%q", first.SessionID, second.SessionID)
	}
}

func TestChannelRouterReleasesClaimWhenRouteBindingPersistenceFails(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "reply"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{Deduplicator: deduplicator, RouteBindings: failingRouteBindingStore{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "persist route binding") {
		t.Fatalf("persistence error=%v", err)
	}
	if deduplicator.claims != 1 || deduplicator.releases != 1 {
		t.Fatalf("claim lifecycle=%#v", deduplicator)
	}
}

func TestChannelRouterIsolatesRoutesDeduplicatesAndPreservesReply(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, Content: "first reply"}},
		{Message: Message{Role: RoleAssistant, Content: "second reply"}},
	}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{MaxOutputTextBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	base := InboundMessage{ChannelID: "fake", AccountID: "account", SenderID: "sender", MessageID: "message-1", Text: "hello"}
	base.ConversationID = "conversation-a"
	first, err := router.Handle(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Duplicate || first.SessionID == "" || first.Outbound == nil {
		t.Fatalf("first route result=%#v", first)
	}
	duplicate, err := router.Handle(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Outbound != nil || len(provider.requests) != 1 {
		t.Fatalf("duplicate was processed: result=%#v provider_calls=%d", duplicate, len(provider.requests))
	}
	other := base
	other.ConversationID = "conversation-b"
	other.MessageID = "message-2"
	second, err := router.Handle(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID == "" || second.SessionID == first.SessionID {
		t.Fatalf("conversation sessions were reused: first=%q second=%q", first.SessionID, second.SessionID)
	}
	messages := channel.Messages()
	if len(messages) != 2 {
		t.Fatalf("sent messages=%#v", messages)
	}
	if messages[0].ConversationID != "conversation-a" || messages[0].ReplyTo != "message-1" || messages[0].Text != "first reply" {
		t.Fatalf("reply metadata not preserved: %#v", messages[0])
	}
	if messages[1].ConversationID != "conversation-b" || messages[1].ReplyTo != "message-2" {
		t.Fatalf("second reply metadata not preserved: %#v", messages[1])
	}
	if len(messages[0].Metadata["bee.route_key"]) == 0 || len(messages[0].Metadata) > MaxChannelMetadataEntries {
		t.Fatalf("bounded output metadata missing: %#v", messages[0].Metadata)
	}
}

func TestChannelRouterRejectsUnauthorizedSenderBeforeDedupOrRun(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "should not run"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{Authorizer: ChannelPolicy{AllowedSenders: []SenderID{"allowed"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", SenderID: "blocked", MessageID: "message", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "authorize") {
		t.Fatalf("unauthorized sender error=%v", err)
	}
	if len(provider.requests) != 0 || len(channel.Messages()) != 0 {
		t.Fatal("unauthorized sender reached model or channel")
	}
}

func TestMemoryMessageDeduplicatorUsesUpdateIDWhenPresent(t *testing.T) {
	deduplicator := NewMemoryMessageDeduplicator(3)
	message := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", UpdateID: "update", Text: "hello"}
	first, err := deduplicator.Claim(context.Background(), message)
	if err != nil || !first {
		t.Fatalf("first claim=%v err=%v", first, err)
	}
	message.MessageID = "different-message-id"
	second, err := deduplicator.Claim(context.Background(), message)
	if err != nil || second {
		t.Fatalf("duplicate update claim=%v err=%v", second, err)
	}
	otherConversation := message
	otherConversation.ConversationID = "other-conversation"
	third, err := deduplicator.Claim(context.Background(), otherConversation)
	if err != nil || !third {
		t.Fatalf("update identity collided across conversations: claim=%v err=%v", third, err)
	}
	fourth, err := deduplicator.Claim(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "third", Text: "hello"})
	if err != nil || !fourth {
		t.Fatalf("fourth claim=%v err=%v", fourth, err)
	}
	if _, err := deduplicator.Claim(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "fourth", Text: "hello"}); err == nil {
		t.Fatal("deduplicator accepted beyond bounded capacity")
	}
}

func TestChannelRouterBoundsRouteState(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "reply"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{MaxRoutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "first", MessageID: "message-1", Text: "hello"}
	if _, err := router.Handle(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ConversationID = "second"
	second.MessageID = "message-2"
	if _, err := router.Handle(context.Background(), second); err == nil || !strings.Contains(err.Error(), "route capacity") {
		t.Fatalf("route capacity was not enforced: %v", err)
	}
}

type attachmentTestHandler struct {
	calls  int
	err    error
	result ChannelRouteResult
}

func (handler *attachmentTestHandler) Handle(context.Context, InboundMessage) (ChannelRouteResult, error) {
	return ChannelRouteResult{}, errors.New("text handler should not be called")
}
func (handler *attachmentTestHandler) HandleAttachment(_ context.Context, _ InboundMessage) (ChannelRouteResult, error) {
	handler.calls++
	if handler.err != nil {
		return ChannelRouteResult{}, handler.err
	}
	return handler.result, nil
}

func TestChannelRouterDeduplicatesAttachmentOnlyMessages(t *testing.T) {
	provider := &scriptedToolProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	handler := &attachmentTestHandler{result: ChannelRouteResult{SessionID: "attachment-session"}}
	deduplicator := NewMemoryMessageDeduplicator(16)
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{AttachmentHandler: handler, Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	message := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Attachments: []AttachmentRef{{Reference: "provider-ref"}}}
	first, err := router.Handle(context.Background(), message)
	if err != nil || first.Duplicate || handler.calls != 1 {
		t.Fatalf("first=%#v err=%v calls=%d", first, err, handler.calls)
	}
	second, err := router.Handle(context.Background(), message)
	if err != nil || !second.Duplicate || handler.calls != 1 {
		t.Fatalf("second=%#v err=%v calls=%d", second, err, handler.calls)
	}
}

func TestChannelRouterReleasesFailedAttachmentClaimForRetry(t *testing.T) {
	provider := &scriptedToolProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	handler := &attachmentTestHandler{err: errors.New("attachment processing failed")}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{AttachmentHandler: handler, Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	message := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Attachments: []AttachmentRef{{Reference: "provider-ref"}}}
	if _, err := router.Handle(context.Background(), message); err == nil || !strings.Contains(err.Error(), "attachment processing failed") {
		t.Fatalf("first error=%v", err)
	}
	if handler.calls != 1 || deduplicator.releases != 1 {
		t.Fatalf("after failure calls=%d releases=%d", handler.calls, deduplicator.releases)
	}
	handler.err = nil
	if _, err := router.Handle(context.Background(), message); err != nil || handler.calls != 2 {
		t.Fatalf("retry err=%v calls=%d", err, handler.calls)
	}
}

type blockingAttachmentHandler struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (handler *blockingAttachmentHandler) Handle(context.Context, InboundMessage) (ChannelRouteResult, error) {
	return ChannelRouteResult{}, errors.New("text handler should not be called")
}
func (handler *blockingAttachmentHandler) HandleAttachment(ctx context.Context, _ InboundMessage) (ChannelRouteResult, error) {
	handler.calls.Add(1)
	select {
	case handler.started <- struct{}{}:
	default:
	}
	select {
	case <-handler.release:
		return ChannelRouteResult{}, nil
	case <-ctx.Done():
		return ChannelRouteResult{}, ctx.Err()
	}
}

func TestChannelRouterSuppressesConcurrentAttachmentDuplicate(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	handler := &blockingAttachmentHandler{started: make(chan struct{}, 1), release: make(chan struct{})}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{AttachmentHandler: handler})
	if err != nil {
		t.Fatal(err)
	}
	message := InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Attachments: []AttachmentRef{{Reference: "provider-ref"}}}
	results := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		_, err := router.Handle(context.Background(), message)
		results <- err
	}()
	select {
	case <-handler.started:
	case <-time.After(time.Second):
		t.Fatal("attachment handler did not start")
	}
	second, err := router.Handle(context.Background(), message)
	if err != nil || !second.Duplicate {
		t.Fatalf("concurrent duplicate=%#v err=%v", second, err)
	}
	close(handler.release)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := handler.calls.Load(); got != 1 {
		t.Fatalf("attachment calls=%d", got)
	}
}

func TestChannelRouterRejectsMediaOnlyBeforeClaimAndRouteAllocation(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "unused"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Handle(context.Background(), InboundMessage{
		ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message",
		Attachments: []AttachmentRef{{Reference: "provider-ref"}},
	})
	if err == nil || !strings.Contains(err.Error(), "normalized text") {
		t.Fatalf("media-only error=%v", err)
	}
	if deduplicator.claims != 0 || len(router.routes) != 0 {
		t.Fatalf("media-only message allocated work: claims=%d routes=%d", deduplicator.claims, len(router.routes))
	}
}

func TestChannelRouterSurfacesRunErrorsAndReleasesClaim(t *testing.T) {
	provider := &scriptedToolProvider{}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "scripted provider exhausted") {
		t.Fatalf("run error=%v", err)
	}
	if deduplicator.claims != 1 || deduplicator.releases != 1 {
		t.Fatalf("claim lifecycle=%#v", deduplicator)
	}
}

func TestChannelRouterReleasesClaimWhenSendFails(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "reply"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{rejectingChannel{}}, ChannelRouterOptions{Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "send channel response") {
		t.Fatalf("send error=%v", err)
	}
	if deduplicator.claims != 1 || deduplicator.releases != 1 {
		t.Fatalf("claim lifecycle=%#v", deduplicator)
	}
}

func TestChannelRouterReleasesClaimWhenContextCancels(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "reply"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	deduplicator := &recordingDeduplicator{}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{Deduplicator: deduplicator})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = router.Handle(ctx, InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "conversation", MessageID: "message", Text: "hello"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context error=%v", err)
	}
	if deduplicator.claims != 1 || deduplicator.releases != 1 {
		t.Fatalf("claim lifecycle=%#v", deduplicator)
	}
}

func TestInMemoryChannelDeepCopiesMessages(t *testing.T) {
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	message := OutboundMessage{
		ChannelID: "fake", AccountID: "account", ConversationID: "conversation", Text: "reply",
		Attachments: []AttachmentRef{{Reference: "provider-ref", Metadata: ChannelMetadata{"nested": "original"}}},
		Voice:       &VoiceOutput{Reference: "voice-ref", ContentType: "audio/ogg", Metadata: ChannelMetadata{"voice": "original"}},
		Metadata:    ChannelMetadata{"top": "original"},
	}
	if err := channel.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	message.Attachments[0].Metadata["nested"] = "mutated"
	message.Voice.Metadata["voice"] = "mutated"
	message.Metadata["top"] = "mutated"
	messages := channel.Messages()
	if messages[0].Attachments[0].Metadata["nested"] != "original" || messages[0].Voice.Metadata["voice"] != "original" || messages[0].Metadata["top"] != "original" {
		t.Fatalf("stored message was aliased: %#v", messages[0])
	}
	messages[0].Attachments[0].Metadata["nested"] = "returned mutation"
	messages[0].Voice.Metadata["voice"] = "returned mutation"
	messages[0].Metadata["top"] = "returned mutation"
	again := channel.Messages()
	if again[0].Attachments[0].Metadata["nested"] != "original" || again[0].Voice.Metadata["voice"] != "original" || again[0].Metadata["top"] != "original" {
		t.Fatalf("returned message was aliased: %#v", again[0])
	}
}

func TestChannelHandlerFuncAndNilChannelFailClearly(t *testing.T) {
	handler := ChannelHandlerFunc(func(_ context.Context, message InboundMessage) (ChannelRouteResult, error) {
		return ChannelRouteResult{SessionID: SessionID(message.ConversationID)}, nil
	})
	result, err := handler.Handle(context.Background(), InboundMessage{ConversationID: "conversation"})
	if err != nil || result.SessionID != "conversation" {
		t.Fatalf("handler result=%#v err=%v", result, err)
	}
	if _, err := NewChannelRouter(nil, []Channel{nil}, ChannelRouterOptions{}); err == nil {
		t.Fatal("nil application accepted")
	}
}

type recordingDeduplicator struct {
	claims   int
	releases int
}

type failingRouteBindingStore struct{}

func (failingRouteBindingStore) Lookup(context.Context, SessionRouteKey) (SessionID, error) {
	return "", ErrRouteBindingNotFound
}

func (failingRouteBindingStore) Bind(context.Context, SessionRouteKey, SessionID) error {
	return errors.New("route binding persistence unavailable")
}

func (deduplicator *recordingDeduplicator) Claim(context.Context, InboundMessage) (bool, error) {
	deduplicator.claims++
	return true, nil
}

func (deduplicator *recordingDeduplicator) Release(context.Context, InboundMessage) error {
	deduplicator.releases++
	return nil
}

type rejectingChannel struct{}

func (rejectingChannel) ID() ChannelID { return "fake" }

func (rejectingChannel) Send(context.Context, OutboundMessage) error {
	return errors.New("send failed")
}
