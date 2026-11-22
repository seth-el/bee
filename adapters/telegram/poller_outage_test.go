package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

const outageTestToken = "123:outage-secret-token"

// flakyTelegram serves getMe normally and answers getUpdates according to
// script: "drop" closes the connection without a response, "hang" never
// answers, "401" returns an auth error, anything else is an update batch.
type flakyTelegram struct {
	mu     sync.Mutex
	script []string
	polls  int
	server *httptest.Server
}

func newFlakyTelegram(t *testing.T, ctx context.Context, script ...string) *flakyTelegram {
	t.Helper()
	fake := &flakyTelegram{script: script}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"bee_test_bot"}}`)
			return
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":9,"chat":{"id":42},"date":1,"text":"x"}}`)
			return
		}
		fake.mu.Lock()
		fake.polls++
		step := ""
		if len(fake.script) > 0 {
			step = fake.script[0]
			fake.script = fake.script[1:]
		}
		fake.mu.Unlock()
		switch step {
		case "drop":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		case "hang":
			select {
			case <-ctx.Done():
			case <-r.Context().Done():
			}
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		case "":
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":[`+step+`]}`)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestPollerSurvivesDroppedConnectionsWithoutCancellingReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFlakyTelegram(t, ctx, textUpdate(1, "long question"), "drop", "drop", "drop", textUpdate(2, "next"))
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	client, _ := NewClient(fake.server.URL, outageTestToken, fake.server.Client())
	release := make(chan struct{})
	var mu sync.Mutex
	var handled []string
	handler := bee.ChannelHandlerFunc(func(turnCtx context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		if message.Text == "long question" {
			select {
			case <-release:
			case <-turnCtx.Done():
				return bee.ChannelRouteResult{}, turnCtx.Err()
			}
		}
		mu.Lock()
		handled = append(handled, message.Text)
		mu.Unlock()
		return bee.ChannelRouteResult{}, nil
	})
	poller, err := NewPollerWithOptions(client, testConfig(t), checkpoint, handler, PollerOptions{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	poller.Logf = logs.logf
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	waitFor(t, "polling past the dropped connections", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.polls >= 5
	})
	select {
	case err := <-done:
		t.Fatalf("poller exited during the outage: %v", err)
	default:
	}
	close(release)
	waitFor(t, "both messages handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(handled, "|") == "long question|next"
	})
	waitFor(t, "recovery logged", func() bool { return strings.Contains(logs.joined(), "recovered") })
	got := logs.joined()
	if !strings.Contains(got, "telegram poll failed (") || !strings.Contains(got, "retrying in 1s") {
		t.Fatalf("logs = %q", got)
	}
	if strings.Count(got, "poll failed") != 1 {
		t.Fatalf("each outage should log its first failure once: %q", got)
	}
	if strings.Contains(got, "outage-secret-token") || strings.Contains(got, fake.server.URL) {
		t.Fatalf("logs leak the request URL or token: %q", got)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("run = %v", err)
	}
}

func TestPollerStillExitsOnAuthFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFlakyTelegram(t, ctx, "401")
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	client, _ := NewClient(fake.server.URL, outageTestToken, fake.server.Client())
	handler := bee.ChannelHandlerFunc(func(context.Context, bee.InboundMessage) (bee.ChannelRouteResult, error) {
		return bee.ChannelRouteResult{}, nil
	})
	poller, _ := NewPollerWithOptions(client, testConfig(t), checkpoint, handler, PollerOptions{Sleep: noSleep})
	select {
	case err := <-runAsync(poller, ctx):
		var apiErr *APIError
		if err == nil || !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("run = %v, want the 401", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller kept running on an auth failure")
	}
}

func runAsync(poller *Poller, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()
	return done
}

func TestGetUpdatesTimesOutSilentConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := getUpdatesGrace
	getUpdatesGrace = 50 * time.Millisecond
	defer func() { getUpdatesGrace = previous }()
	fake := newFlakyTelegram(t, ctx, "hang")
	client, _ := NewClient(fake.server.URL, outageTestToken, fake.server.Client())
	start := time.Now()
	_, err := client.GetUpdates(ctx, 0, 1, 10)
	var transport *TransportError
	if !errors.As(err, &transport) || transport.Reason != "timeout" {
		t.Fatalf("err = %v, want a timeout TransportError", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	// A cancelled caller is still reported as cancellation, not an outage.
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := client.GetUpdates(cancelled, 0, 1, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
}

func TestPollOutageBackoffAndWarning(t *testing.T) {
	logs := &logLines{}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	outage := &pollOutage{logf: logs.logf, now: func() time.Time { return now }}
	var delays []time.Duration
	for i := 0; i < 8; i++ {
		delay, retry := outage.failed(&TransportError{Reason: "connection reset"})
		if !retry {
			t.Fatal("transport failure must be retried")
		}
		delays = append(delays, delay)
		now = now.Add(delay)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, delay := range delays {
		if delay != want[i]*time.Second {
			t.Fatalf("delays = %v", delays)
		}
	}
	if strings.Contains(logs.joined(), "still retrying") {
		t.Fatal("warned before five minutes")
	}
	now = now.Add(5 * time.Minute)
	outage.failed(&APIError{HTTPStatus: http.StatusBadGateway})
	outage.failed(&APIError{HTTPStatus: http.StatusBadGateway})
	if got := logs.joined(); strings.Count(got, "still retrying") != 1 || !strings.Contains(got, "last: HTTP 502") {
		t.Fatalf("logs = %q", got)
	}
	outage.recovered()
	if delay, _ := outage.failed(&APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: 45 * time.Second}); delay != 45*time.Second {
		t.Fatalf("retry_after delay = %s", delay)
	}
	if _, retry := outage.failed(&APIError{HTTPStatus: http.StatusUnauthorized}); retry {
		t.Fatal("auth failure must not be retried")
	}
	if _, retry := outage.failed(errors.New("decode telegram API response")); retry {
		t.Fatal("unknown errors must not be retried")
	}
}

func TestNewHTTPClientUsesHTTP1EvenWhenServerOffersHTTP2(t *testing.T) {
	var mu sync.Mutex
	var protos, remotes []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		remotes = append(remotes, r.RemoteAddr)
		mu.Unlock()
		if r.URL.Query().Get("hang") == "1" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := NewHTTPClient()
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	get := func(query string, timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/?"+query, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if err := get("", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := get("hang=1", 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang err = %v", err)
	}
	if err := get("", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, proto := range protos {
		if proto != "HTTP/1.1" {
			t.Fatalf("protos = %v, want HTTP/1.1 only", protos)
		}
	}
	// The timed-out request's connection must be closed, not reused.
	if len(remotes) != 3 || remotes[2] == remotes[1] {
		t.Fatalf("remotes = %v: the request after a timeout reused the timed-out connection", remotes)
	}
}
