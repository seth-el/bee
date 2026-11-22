package bee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebSearchCircuitTripsOnTransportAndDecodeFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`secret upstream body with target-like data`))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withArtifactContext(context.Background(), "s", "transport-run")
	if _, err = client.Search(ctx, WebSearchRequest{Query: "one"}); err == nil {
		t.Fatal("first transport failure was not reported")
	}
	second, err := client.Search(ctx, WebSearchRequest{Query: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || second.Query != "" || !second.Degraded || second.Retryable || strings.Contains(second.Note, "secret") {
		t.Fatalf("second=%#v calls=%d", second, calls.Load())
	}
}

func TestWebFetchRunePaginationAndLegacyExtractMode(t *testing.T) {
	body := strings.Repeat("界<&>\"", 40000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, MaxChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(first.Content)) != 1000 || first.TotalChars != len([]rune(body)) || first.NextOffset == nil || *first.NextOffset != 1000 {
		t.Fatalf("first=%#v", first)
	}
	second, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, Offset: *first.NextOffset, MaxChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if second.Offset != 1000 || len([]rune(second.Content)) != 1000 {
		t.Fatalf("second=%#v", second)
	}
	third, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, Offset: 150000, MaxChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if third.Offset != 150000 || len([]rune(third.Content)) != 1000 {
		t.Fatalf("third=%#v", third)
	}
	if _, err = client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, ExtractMode: "markdown"}); err == nil {
		t.Fatal("markdown legacy mode accepted")
	}
}

func TestWebFetchToolOutputIsValidBoundedJSON(t *testing.T) {
	body := strings.Repeat(`<>&"界`, 40000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	output, err := (WebFetchTool{Client: client}).Execute(context.Background(), json.RawMessage(`{"url":"`+server.URL+`","max_chars":131072}`))
	if err != nil {
		t.Fatal(err)
	}
	var result WebFetchResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(output) > MaxWebToolOutputBytes || !result.Truncated || !result.HasMore || result.NextOffset == nil {
		t.Fatalf("output=%d result=%#v", len(output), result)
	}
}
