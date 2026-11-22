package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type fakeTelegram struct {
	mu      sync.Mutex
	batches []string
	sent    []string
	server  *httptest.Server
}

func newFakeTelegram(t *testing.T, ctx context.Context, batches ...string) *fakeTelegram {
	t.Helper()
	fake := &fakeTelegram{batches: batches}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"bee_test_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			body, _ := io.ReadAll(r.Body)
			var request SendMessageRequest
			_ = json.Unmarshal(body, &request)
			fake.mu.Lock()
			fake.sent = append(fake.sent, request.Text)
			fake.mu.Unlock()
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":9,"chat":{"id":%d},"date":1,"text":"x"}}`, request.ChatID)
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			fake.mu.Lock()
			if len(fake.batches) > 0 {
				batch := fake.batches[0]
				fake.batches = fake.batches[1:]
				fake.mu.Unlock()
				fmt.Fprint(w, `{"ok":true,"result":[`+batch+`]}`)
				return
			}
			fake.mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeTelegram) sentTexts() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.sent...)
}

func textUpdate(id int64, text string) string {
	return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"from":{"id":7},"chat":{"id":42},"text":%q}}`, id, id, text)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestPollerStopInterruptsRunningReplyAndKeepsPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeTelegram(t, ctx, textUpdate(1, "long question"), textUpdate(2, "/stop"), textUpdate(3, "next"))
	config := testConfig(t)
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	client, _ := NewClient(fake.server.URL, "123:secret-token", fake.server.Client())
	var mu sync.Mutex
	var handled []string
	cancelled := make(chan struct{}, 1)
	handler := bee.ChannelHandlerFunc(func(turnCtx context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		mu.Lock()
		handled = append(handled, message.Text)
		mu.Unlock()
		if message.Text == "long question" {
			<-turnCtx.Done()
			cancelled <- struct{}{}
			return bee.ChannelRouteResult{}, turnCtx.Err()
		}
		return bee.ChannelRouteResult{}, nil
	})
	poller, err := NewPoller(client, config, checkpoint, handler)
	if err != nil {
		t.Fatal(err)
	}
	poller.ShowTyping = true
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("/stop did not cancel the running reply")
	}
	waitFor(t, "checkpoint past all three updates", func() bool {
		offset, err := checkpoint.Load(context.Background())
		return err == nil && offset == 4
	})
	mu.Lock()
	got := strings.Join(handled, "|")
	mu.Unlock()
	if got != "long question|next" {
		t.Fatalf("handled = %q (the /stop must not reach the router)", got)
	}
	if sent := fake.sentTexts(); len(sent) != 1 || sent[0] != "Stopped." {
		t.Fatalf("sent = %q", sent)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("run = %v", err)
	}
}

func TestPollerSteerReplacesRunningReplyWithCorrection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeTelegram(t, ctx, textUpdate(1, "write a poem"), textUpdate(2, "/steer@bee_test_bot make it a haiku"))
	config := testConfig(t)
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	client, _ := NewClient(fake.server.URL, "123:secret-token", fake.server.Client())
	texts := make(chan string, 4)
	handler := bee.ChannelHandlerFunc(func(turnCtx context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		texts <- message.Text
		if message.Text == "write a poem" {
			<-turnCtx.Done()
			return bee.ChannelRouteResult{}, turnCtx.Err()
		}
		return bee.ChannelRouteResult{}, nil
	})
	poller, _ := NewPoller(client, config, checkpoint, handler)
	go func() { _ = poller.Run(ctx) }()
	for _, want := range []string{"write a poem", "make it a haiku"} {
		select {
		case got := <-texts:
			if got != want {
				t.Fatalf("handled %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	waitFor(t, "steering reply", func() bool {
		sent := fake.sentTexts()
		return len(sent) == 1 && sent[0] == "Steering…"
	})
}

func TestPollerStopWhenIdleAndFromUnknownChat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unknown := `{"update_id":2,"message":{"message_id":2,"from":{"id":666},"chat":{"id":666},"text":"/stop"}}`
	fake := newFakeTelegram(t, ctx, textUpdate(1, "/stop"), unknown)
	config := testConfig(t)
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	client, _ := NewClient(fake.server.URL, "123:secret-token", fake.server.Client())
	poller, _ := NewPoller(client, config, checkpoint, bee.ChannelHandlerFunc(func(context.Context, bee.InboundMessage) (bee.ChannelRouteResult, error) {
		return bee.ChannelRouteResult{}, nil
	}))
	go func() { _ = poller.Run(ctx) }()
	waitFor(t, "checkpoint", func() bool {
		offset, err := checkpoint.Load(context.Background())
		return err == nil && offset == 3
	})
	if sent := fake.sentTexts(); len(sent) != 1 || sent[0] != "Nothing is running." {
		t.Fatalf("sent = %q (unknown chats must get nothing)", sent)
	}
}

func TestCheckpointTrackerAdvancesOnlyPastContiguousDoneUpdates(t *testing.T) {
	store, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	tracker := newCheckpointTracker(store, 5)
	tracker.dispatch(5)
	tracker.dispatch(6)
	tracker.dispatch(8)
	if err := tracker.done(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	if offset, _ := store.Load(context.Background()); offset != 0 {
		t.Fatalf("advanced past running update 5: %d", offset)
	}
	_ = tracker.done(context.Background(), 5)
	if offset, _ := store.Load(context.Background()); offset != 8 {
		t.Fatalf("offset = %d, want 8 (update 8 still pending)", offset)
	}
	_ = tracker.done(context.Background(), 8)
	if offset, _ := store.Load(context.Background()); offset != 9 {
		t.Fatalf("offset = %d, want 9", offset)
	}
}

func TestInterruptCommandParsing(t *testing.T) {
	for input, want := range map[string][2]string{
		"/stop":                {"stop", ""},
		"/KILL@bot":            {"kill", ""},
		"/steer  go left\nnow": {"steer", "go left\nnow"},
		"/status":              {"", ""},
		"stop":                 {"", ""},
	} {
		name, args := interruptCommand(input)
		if name != want[0] || args != want[1] {
			t.Fatalf("%q = %q %q", input, name, args)
		}
	}
}
