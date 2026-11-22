package bee

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebSearchDegradedCircuitAndReset(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["duckduckgo","timeout"],"bing: error",["bad\u0001engine","oops"]]}`))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withArtifactContext(context.Background(), "session-search", "run-search")
	first, err := client.Search(ctx, WebSearchRequest{Query: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Degraded || first.Retryable || len(first.Results) != 0 || len(first.UnresponsiveEngines) != 3 || !strings.Contains(first.Note, "Do not repeatedly") {
		t.Fatalf("first=%#v", first)
	}
	second, err := client.Search(ctx, WebSearchRequest{Query: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Degraded || second.Retryable || calls.Load() != 1 || strings.Contains(second.Query, "changed") {
		t.Fatalf("second=%#v calls=%d", second, calls.Load())
	}
	third, err := client.Search(withArtifactContext(context.Background(), "session-search", "run-new"), WebSearchRequest{Query: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if !third.Degraded || calls.Load() != 2 {
		t.Fatalf("reset=%#v calls=%d", third, calls.Load())
	}
}

func TestWebFetchWindowAndBoundedTransport(t *testing.T) {
	content := strings.Repeat("x", DefaultWebFetchChars+1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "TEXT/HTML; charset=UTF-8")
		_, _ = w.Write([]byte("<title>A &amp; B</title><p>" + content + "</p>"))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, MaxChars: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1024 || !result.HasMore || result.NextOffset == nil || result.TotalChars <= len(result.Content) || result.ContentType != "text/html; charset=utf-8" || !strings.Contains(result.Title, "A & B") {
		t.Fatalf("result=%#v", result)
	}
	continued, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, Offset: *result.NextOffset, MaxChars: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if continued.Offset != *result.NextOffset || continued.TotalChars != result.TotalChars {
		t.Fatalf("continued=%#v", continued)
	}

	tooLarge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("z", MaxWebResponseBytes+1)))
	}))
	defer tooLarge.Close()
	if _, err := client.Fetch(context.Background(), WebFetchRequest{URL: tooLarge.URL}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestWebToolSchemasAreStrictAndBounded(t *testing.T) {
	search := WebSearchTool{}.Definition().Parameters
	fetch := WebFetchTool{}.Definition().Parameters
	for name, data := range map[string]json.RawMessage{"search": search, "fetch": fetch} {
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s schema not strict: %s", name, data)
		}
	}
	if strings.Contains(string(fetch), "markdown") || strings.Contains(string(fetch), "extract_mode") {
		t.Fatalf("fetch advertises unsupported extraction: %s", fetch)
	}
}

func TestBrowserAccessKeyFileRequiresPrivateRegularFile(t *testing.T) {
	file := t.TempDir() + "/key"
	if err := os.WriteFile(file, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (BrowserWebConfig{Enabled: true, CamofoxURL: "http://127.0.0.1:9377", AccessKeyFile: file}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (BrowserWebConfig{Enabled: true, CamofoxURL: "http://127.0.0.1:9377", AccessKeyFile: file}).Validate(); err == nil {
		t.Fatal("world-readable key accepted")
	}
	_ = time.Second
}

func TestWebSearchPauseExpiresDoublesAndClears(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if healthy.Load() {
			_, _ = w.Write([]byte(`{"results":[{"title":"t","url":"https://example.com/","content":"c"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["brave","too many requests"]]}`))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	client.LookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.215.14")}}, nil
	}
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	client.searchNow = func() time.Time { return now }
	ctx := withArtifactContext(context.Background(), "session-pause", "run-pause")
	search := func() WebSearchResult {
		t.Helper()
		result, err := client.Search(ctx, WebSearchRequest{Query: "q"})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	if first := search(); !first.Degraded || calls.Load() != 1 {
		t.Fatalf("first=%#v calls=%d", first, calls.Load())
	}
	now = now.Add(90 * time.Second)
	if paused := search(); calls.Load() != 1 || !strings.Contains(paused.Note, "about 1 more minute") {
		t.Fatalf("paused=%q calls=%d", paused.Note, calls.Load())
	}
	// The 2-minute pause ends: one attempt reaches the backend, fails, and
	// the pause doubles to 4 minutes.
	now = now.Add(31 * time.Second)
	if second := search(); !second.Degraded || calls.Load() != 2 {
		t.Fatalf("second=%#v calls=%d", second, calls.Load())
	}
	now = now.Add(time.Second)
	if paused := search(); calls.Load() != 2 || !strings.Contains(paused.Note, "about 4 more minutes") {
		t.Fatalf("doubled pause=%q calls=%d", paused.Note, calls.Load())
	}
	// After the pause a success clears the state entirely.
	healthy.Store(true)
	now = now.Add(4 * time.Minute)
	if ok := search(); ok.Degraded || len(ok.Results) != 1 || calls.Load() != 3 {
		t.Fatalf("recovered=%#v calls=%d", ok, calls.Load())
	}
	healthy.Store(false)
	if again := search(); !again.Degraded || calls.Load() != 4 {
		t.Fatalf("after recovery=%#v calls=%d", again, calls.Load())
	}
	now = now.Add(searchPauseBase)
	if fresh := search(); calls.Load() != 5 || !fresh.Degraded {
		t.Fatalf("pause did not restart at the base length: calls=%d", calls.Load())
	}
}

func TestSearchPauseCaps(t *testing.T) {
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, expected := range want {
		if got := searchPause(i + 1); got != expected {
			t.Fatalf("searchPause(%d) = %s, want %s", i+1, got, expected)
		}
	}
}
