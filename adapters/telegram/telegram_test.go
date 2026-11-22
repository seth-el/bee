package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, []byte("123:secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{ChannelID: "telegram", AccountID: "test-account", ExpectedBotID: 99, ExpectedBotUsername: "bee_test_bot", TokenFile: tokenPath, APIBaseURL: "http://127.0.0.1", PollTimeoutSeconds: 1, PollLimit: 10, CheckpointPath: filepath.Join(root, "checkpoint.json"), SessionRoot: filepath.Join(root, "sessions"), AllowedChats: []int64{-100123, 42}, AllowedSenders: []int64{7, 8}}
}

func TestConfigTokenPermissionAndRedaction(t *testing.T) {
	config := testConfig(t)
	token, err := config.Token()
	if err != nil || token != "123:secret-token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	if err := os.Chmod(config.TokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Token(); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("permission error leaked or missing: %v", err)
	}
}

func TestConfigRejectsUnknownAndUnsafeFields(t *testing.T) {
	config := testConfig(t)
	data, _ := json.Marshal(config)
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	config.AllowedChats = nil
	if err := config.Validate(); err == nil {
		t.Fatal("expected empty allowlist rejection")
	}
}

func TestClientBotAPIRequestsAndErrors(t *testing.T) {
	const token = "123:secret-token"
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		if !strings.Contains(r.URL.Path, token) {
			t.Errorf("request path missing token")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Bee"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":12}]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":22,"chat":{"id":42}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if user, err := client.GetMe(context.Background()); err != nil || user.ID != 99 {
		t.Fatalf("getMe = %#v, %v", user, err)
	}
	updates, err := client.GetUpdates(context.Background(), 11, 3, 4)
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 12 {
		t.Fatalf("updates = %#v, %v", updates, err)
	}
	if _, err := client.SendMessage(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(paths, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "offset=11") || !strings.Contains(joined, "timeout=3") || !strings.Contains(joined, "limit=4") {
		t.Fatalf("query missing: %s", joined)
	}
}

func TestSendMethodsRejectMalformedSuccessfulResponses(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "sendMessage missing result", path: "/sendMessage", body: `{"ok":true}`},
		{name: "sendMessage zero message id", path: "/sendMessage", body: `{"ok":true,"result":{"message_id":0}}`},
		{name: "sendVoice missing result", path: "/sendVoice", body: `{"ok":true}`},
		{name: "sendVoice zero message id", path: "/sendVoice", body: `{"ok":true,"result":{"message_id":0}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, testCase.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "123:secret-token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var sendErr error
			if testCase.path == "/sendMessage" {
				_, sendErr = client.SendMessage(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"})
			} else {
				_, sendErr = client.SendVoice(context.Background(), SendVoiceRequest{ChatID: 42, Voice: strings.NewReader("OggS"), SizeBytes: 4, Filename: "voice.ogg"})
			}
			if sendErr == nil || !strings.Contains(sendErr.Error(), "valid message ID") || strings.Contains(sendErr.Error(), "secret-token") {
				t.Fatalf("error = %v", sendErr)
			}
		})
	}
}

func TestClientErrorRedactionAndBounds(t *testing.T) {
	const token = "123:secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"ok":false,"error_code":502,"description":"%s"}`, token)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, token, server.Client())
	_, err := client.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %v", err)
	}
	client.MaxResponseBytes = 8
	if _, err := client.GetMe(context.Background()); err == nil {
		t.Fatal("expected oversized response failure")
	}
}

func TestNormalizeNegativeChatThreadReplyAndIgnores(t *testing.T) {
	config := testConfig(t)
	config.APIBaseURL = "http://127.0.0.1"
	update := Update{UpdateID: 77, Message: &Message{MessageID: 5, From: &User{ID: 7}, Chat: Chat{ID: -100123, Type: "supergroup"}, Date: 1700000000, Text: "hello", MessageThreadID: 9, ReplyToMessage: &Message{MessageID: 4}}}
	normalized, err := NormalizeUpdate(config, update)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Ignored {
		t.Fatal(normalized.Reason)
	}
	message := normalized.Message
	if message.ConversationID != "tg-chat-n100123" || message.SenderID != "tg-user-p7" || message.ThreadID != "tg-thread-p9" || message.Reply.ReplyToMessageID != "tg-msg-p4" {
		t.Fatalf("normalized ids: %#v", message)
	}
	if message.ReceivedAt.Unix() != 1700000000 {
		t.Fatalf("timestamp: %v", message.ReceivedAt)
	}
	ignored, err := NormalizeUpdate(config, Update{UpdateID: 78, CallbackQuery: &struct{}{}})
	if err != nil || !ignored.Ignored {
		t.Fatalf("unsupported = %#v, %v", ignored, err)
	}
	unauthorized, err := NormalizeUpdate(config, Update{UpdateID: 79, Message: &Message{MessageID: 1, From: &User{ID: 999}, Chat: Chat{ID: -100123}, Text: "no"}})
	if err != nil || !unauthorized.Ignored || unauthorized.Reason != "unauthorized sender" {
		t.Fatalf("unauthorized = %#v, %v", unauthorized, err)
	}
}

func TestChannelUnicodeChunkingAndRetry(t *testing.T) {
	config := testConfig(t)
	config.APIBaseURL = "http://127.0.0.1"
	var mu sync.Mutex
	attempts := 0
	var lengths []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var request SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		attempts++
		lengths = append(lengths, len(request.Text))
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"ok":false,"error_code":503}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	channel, err := NewTelegramChannel(client, config, ChannelOptions{RetryDelay: 0, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("界", 3000)
	message := bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-n100123", ThreadID: "tg-thread-p9", ReplyTo: "tg-msg-p5", Text: text}
	if err := channel.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 4 || len(lengths) != 4 || lengths[0] > MaxTelegramTextBytes || lengths[1] > MaxTelegramTextBytes {
		t.Fatalf("attempts/lengths: %d %#v", attempts, lengths)
	}
}

func TestCheckpointAtomicRestartAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkpoint.json")
	store, err := NewCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(context.Background()); err != nil || got != 0 {
		t.Fatalf("initial checkpoint = %d, %v", got, err)
	}
	if err := store.Save(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	mode, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", mode.Mode().Perm())
	}
	restarted, _ := NewCheckpointStore(path)
	if got, err := restarted.Load(context.Background()); err != nil || got != 42 {
		t.Fatalf("restart checkpoint = %d, %v", got, err)
	}
	if err := store.Save(context.Background(), -1); err == nil {
		t.Fatal("expected negative offset rejection")
	}
}

func TestPollerRetriesTransientPollFailures(t *testing.T) {
	config := testConfig(t)
	config.APIBaseURL = "http://127.0.0.1"
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	var updatesCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"bee_test_bot"}}`)
			return
		}
		call := updatesCalls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"ok":false,"error_code":503}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	poller, err := NewPollerWithOptions(client, config, checkpoint, bee.ChannelHandlerFunc(func(context.Context, bee.InboundMessage) (bee.ChannelRouteResult, error) {
		return bee.ChannelRouteResult{}, nil
	}), PollerOptions{MaxAttempts: 2, RetryDelay: 0, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = poller.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || updatesCalls.Load() < 2 {
		t.Fatalf("poll retry error=%v calls=%d", err, updatesCalls.Load())
	}
}

func TestPollerAcknowledgesIgnoredAndLeavesFailureRetryable(t *testing.T) {
	config := testConfig(t)
	config.APIBaseURL = "http://127.0.0.1"
	root := t.TempDir()
	checkpoint, _ := NewCheckpointStore(filepath.Join(root, "checkpoint.json"))
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"bee_test_bot"}}`)
			return
		}
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"callback_query":{}},{"update_id":2,"message":{"message_id":1,"from":{"id":7},"chat":{"id":42},"text":"hello"}}]}`)
			return
		}
		<-ctx.Done()
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	var handled []bee.InboundMessage
	handledSignal := make(chan struct{})
	handler := bee.ChannelHandlerFunc(func(_ context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		handled = append(handled, message)
		close(handledSignal)
		return bee.ChannelRouteResult{}, nil
	})
	poller, err := NewPoller(client, config, checkpoint, handler)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()
	select {
	case <-handledSignal:
	case <-time.After(time.Second):
		t.Fatal("poller did not process the valid update")
	}
	deadline := time.After(time.Second)
	for {
		offset, loadErr := checkpoint.Load(context.Background())
		if loadErr == nil && offset == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("checkpoint did not advance: offset=%d err=%v", offset, loadErr)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poller error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %d", len(handled))
	}
}

func TestPollerDoesNotAdvanceOnHandlerFailure(t *testing.T) {
	config := testConfig(t)
	config.APIBaseURL = "http://127.0.0.1"
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"bee_test_bot"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[{"update_id":3,"message":{"message_id":1,"from":{"id":7},"chat":{"id":42},"text":"hello"}}]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	poller, _ := NewPoller(client, config, checkpoint, bee.ChannelHandlerFunc(func(context.Context, bee.InboundMessage) (bee.ChannelRouteResult, error) {
		return bee.ChannelRouteResult{}, errors.New("model failed")
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := poller.Run(ctx); err == nil || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("error = %v", err)
	}
	if offset, err := checkpoint.Load(context.Background()); err != nil || offset != 0 {
		t.Fatalf("offset advanced: %d, %v", offset, err)
	}
}

func TestChannelCancellationDuringRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"ok":false,"error_code":503}`)
	}))
	defer server.Close()
	config := testConfig(t)
	config.APIBaseURL = server.URL
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	ctx, cancel := context.WithCancel(context.Background())
	channel, err := NewTelegramChannel(client, config, ChannelOptions{RetryDelay: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	err = channel.Send(ctx, bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-n100123", Text: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}
