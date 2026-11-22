package bee

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type channelCommandFixture struct {
	app      *Application
	channel  *InMemoryChannel
	router   *ChannelRouter
	bindings *LocalRouteBindingStore
	next     int
}

func newChannelCommandFixture(t *testing.T, replies int, options ChannelRouterOptions) *channelCommandFixture {
	t.Helper()
	root := t.TempDir()
	responses := make([]Response, replies)
	for i := range responses {
		responses[i] = Response{Message: Message{Role: RoleAssistant, Content: fmt.Sprintf("reply %d", i+1)}}
	}
	store, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &scriptedToolProvider{responses: responses}, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := NewLocalRouteBindingStore(filepath.Join(root, "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	options.RouteBindings = bindings
	router, err := NewChannelRouter(app, []Channel{channel}, options)
	if err != nil {
		t.Fatal(err)
	}
	return &channelCommandFixture{app: app, channel: channel, router: router, bindings: bindings}
}

func (f *channelCommandFixture) send(t *testing.T, text string) (ChannelRouteResult, string) {
	t.Helper()
	f.next++
	result, err := f.router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "chat", MessageID: ChannelMessageID(fmt.Sprintf("m%d", f.next)), Text: text})
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	if result.Outbound == nil {
		return result, ""
	}
	return result, result.Outbound.Text
}

func (f *channelCommandFixture) bound(t *testing.T) SessionID {
	t.Helper()
	key, err := NewSessionRouteKey("fake", "account", "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.bindings.Lookup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChannelLocalCommandsReplyAndSwitchSessions(t *testing.T) {
	f := newChannelCommandFixture(t, 3, ChannelRouterOptions{SessionTitle: func(InboundMessage) string { return "Fake chat" }})
	first, text := f.send(t, "hello")
	if text != "reply 1" || first.SessionID == "" {
		t.Fatalf("first turn = %q, %q", text, first.SessionID)
	}
	if session, err := f.app.Resume(first.SessionID); err != nil || session.Metadata.Title != "Fake chat" {
		t.Fatalf("session title = %v, %v", session, err)
	}
	// A command replies with its text and does not change the route.
	if _, text := f.send(t, "/status"); !strings.Contains(text, string(first.SessionID)) {
		t.Fatalf("/status reply = %q", text)
	}
	// Unknown commands reply instead of failing the message.
	if _, text := f.send(t, "/nope"); !strings.Contains(text, "unknown command") {
		t.Fatalf("/nope reply = %q", text)
	}
	// /new rebinds the route to a fresh session titled like channel sessions.
	created, text := f.send(t, "/new")
	if !strings.Contains(text, "started new session") || created.SessionID == first.SessionID || f.bound(t) != created.SessionID {
		t.Fatalf("/new = %q, session %q, bound %q", text, created.SessionID, f.bound(t))
	}
	if session, err := f.app.Resume(created.SessionID); err != nil || session.Metadata.Title != "Fake chat" {
		t.Fatalf("/new title = %v, %v", session, err)
	}
	next, text := f.send(t, "again")
	if text != "reply 2" || next.SessionID != created.SessionID {
		t.Fatalf("turn after /new = %q in %q", text, next.SessionID)
	}
	// /resume lists sessions; /resume <prefix>@bot switches back.
	if _, text := f.send(t, "/resume"); !strings.Contains(text, string(first.SessionID)[:8]) {
		t.Fatalf("/resume list = %q", text)
	}
	if _, text := f.send(t, "/resume@bee_test_bot "+string(first.SessionID)[:12]); !strings.Contains(text, "resumed session") || f.bound(t) != first.SessionID {
		t.Fatalf("/resume id = %q, bound %q", text, f.bound(t))
	}
	back, text := f.send(t, "and back")
	if text != "reply 3" || back.SessionID != first.SessionID {
		t.Fatalf("turn after /resume = %q in %q", text, back.SessionID)
	}
}

func TestChannelCommandOptionsHelpUnsupportedAndFailureReplies(t *testing.T) {
	f := newChannelCommandFixture(t, 0, ChannelRouterOptions{
		UnsupportedCommands: []CommandName{CommandSteer},
		CommandHelp:         "only /status here",
		ReplyOnFailure:      true,
	})
	if _, text := f.send(t, "/help"); text != "only /status here" {
		t.Fatalf("/help = %q", text)
	}
	if _, text := f.send(t, "/steer go left"); !strings.Contains(text, "/steer isn't available") {
		t.Fatalf("/steer = %q", text)
	}
	// The scripted provider has no replies: the run fails and is answered.
	if _, text := f.send(t, "hello"); !strings.Contains(text, "Bee couldn't answer that") {
		t.Fatalf("failure reply = %q", text)
	}
}

func TestChannelRouterRepliesWhenSessionIsBusy(t *testing.T) {
	f := newChannelCommandFixture(t, 1, ChannelRouterOptions{ReplyOnFailure: true})
	first, _ := f.send(t, "hello")
	lease, err := f.app.Store.acquireTurnLease(first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, text := f.send(t, "while busy"); !strings.Contains(text, "busy in another Bee client") {
		t.Fatalf("busy reply = %q", text)
	}
}

func TestChannelCommandNameParsing(t *testing.T) {
	for input, want := range map[string]CommandName{"/status": "status", "/Status@bot x": "status", " /resume abc": "resume"} {
		if got, ok := channelCommandName(input); !ok || got != want {
			t.Fatalf("%q = %q, %v", input, got, ok)
		}
	}
	if _, ok := channelCommandName("hello /status"); ok {
		t.Fatal("non-leading slash parsed as a command")
	}
	if got := stripChannelCommandMention("/resume@bot  abc"); got != "/resume abc" {
		t.Fatalf("strip = %q", got)
	}
}
