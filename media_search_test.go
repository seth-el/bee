package bee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type mediaSearchHTTPClient struct {
	body []byte
	err  error
}

func (client mediaSearchHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if client.err != nil {
		return nil, client.err
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(client.body)), ContentLength: int64(len(client.body)), Request: request}, nil
}

func mediaSearchTestClient(t *testing.T, payload any) *WebClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("categories") == "" {
			t.Fatalf("unexpected search query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 1024})
	if err != nil {
		t.Fatal(err)
	}
	client.SearchEndpoint = server.URL
	client.AllowPrivateTargets = true
	return client
}

func TestMediaSearchParsesImageAndRejectsPrivateRetrieval(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "bee good", "img_src": "https://example.com/good.jpg", "url": "https://example.com/page", "content": strings.Repeat("bee ", 600), "source": "example", "resolution": "640x480"},
		{"title": "private", "img_src": "http://127.0.0.1/private.jpg", "url": "https://example.com/private"},
	}})
	items, truncated, err := client.SearchMedia(context.Background(), MediaSearchRequest{Kind: "image", Query: "bee", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(items) != 1 || items[0].URL != "https://example.com/good.jpg" || items[0].SourceURL != "https://example.com/page" {
		t.Fatalf("items=%#v truncated=%v", items, truncated)
	}
	service := NewMediaSearchService(client)
	result, err := service.Search(context.Background(), SessionID("session-a"), MediaSearchRequest{Kind: "image", Query: "bee"})
	if err != nil || len(result.Candidates) != 1 || len(result.Candidates[0].Snippet) != MaxMediaSearchSnippetBytes {
		t.Fatalf("bounded candidates=%d snippet_len=%d err=%v", len(result.Candidates), func() int {
			if len(result.Candidates) == 0 {
				return 0
			}
			return len(result.Candidates[0].Snippet)
		}(), err)
	}
}

func TestMediaSearchFiltersOverDurationAndPreservesUnderLimitAndUnknown(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "too long", "url": "https://example.com/long", "content": "bee video", "source": "example", "length": "3:00:01"},
		{"title": "short", "url": "https://example.com/short", "content": "bee video", "source": "example", "length": "19:59"},
		{"title": "unknown", "url": "https://example.com/unknown", "content": "bee video", "source": "example"},
		{"title": "fallback", "url": "https://example.com/fallback", "content": "bee video", "source": "example", "duration": "1:00"},
	}})
	items, truncated, err := client.SearchMedia(context.Background(), MediaSearchRequest{Kind: "video", Query: "bee", Count: 10})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(items) != 3 {
		t.Fatalf("items=%#v truncated=%v", items, truncated)
	}
	for _, item := range items {
		if item.Title == "too long" {
			t.Fatal("over-duration candidate was retained")
		}
	}
	if items[0].Duration != "19:59" || items[1].Duration != "" || items[2].Duration != "1:00" {
		t.Fatalf("duration fields were not normalized/fallback-preserved: %#v", items)
	}
}

func TestParseMediaSearchDuration(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		known bool
	}{
		{value: "40:17", want: 40*time.Minute + 17*time.Second, known: true},
		{value: "1:02:03", want: time.Hour + 2*time.Minute + 3*time.Second, known: true},
		{value: "20m", want: 20 * time.Minute, known: true},
		{value: "3h", want: 3 * time.Hour, known: true},
		{value: "2:59:59", want: 2*time.Hour + 59*time.Minute + 59*time.Second, known: true},
		{value: "not supplied", known: false},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, known := parseMediaSearchDuration(test.value)
			if got != test.want || known != test.known {
				t.Fatalf("parseMediaSearchDuration(%q)=(%s,%t), want (%s,%t)", test.value, got, known, test.want, test.known)
			}
		})
	}
}

func TestMediaSearchCandidateBindingSessionExpiryAndSingleUse(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{{"title": "bee video", "url": "https://example.com/video.mp4", "content": "bee clip", "source": "example", "duration": "1m"}}})
	now := time.Unix(100, 0).UTC()
	service := NewMediaSearchService(client)
	service.TTL = time.Minute
	service.Now = func() time.Time { return now }
	result, err := service.Search(context.Background(), SessionID("session-a"), MediaSearchRequest{Kind: "video", Query: "bee"})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	candidate := result.Candidates[0]
	if _, err := service.Resolve(SessionID("session-b"), candidate.CandidateID, candidate.Token); err == nil {
		t.Fatal("wrong session was accepted")
	}
	if selection, err := service.Resolve(SessionID("session-a"), candidate.CandidateID, candidate.Token); err != nil || selection.URL != "https://example.com/video.mp4" {
		t.Fatalf("selection=%#v err=%v", selection, err)
	}
	if _, err := service.Resolve(SessionID("session-a"), candidate.CandidateID, candidate.Token); err == nil {
		t.Fatal("candidate was reusable")
	}
	result, err = service.Search(context.Background(), SessionID("session-a"), MediaSearchRequest{Kind: "video", Query: "bee"})
	if err != nil {
		t.Fatal(err)
	}
	candidate = result.Candidates[0]
	now = now.Add(2 * time.Minute)
	if _, err := service.Resolve(SessionID("session-a"), candidate.CandidateID, candidate.Token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry error=%v", err)
	}
}

func TestMediaSearchCandidateBindingsStayBounded(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{{"title": "image", "img_src": "https://example.com/image.png", "url": "https://example.com/source"}}})
	service := NewMediaSearchService(client)
	for index := 0; index < MaxMediaSearchBindings+1; index++ {
		if _, err := service.Search(context.Background(), SessionID("bounded"), MediaSearchRequest{Kind: "image", Query: "bee"}); err != nil {
			t.Fatalf("search %d: %v", index, err)
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.byToken) > MaxMediaSearchBindings {
		t.Fatalf("candidate binding store grew to %d entries", len(service.byToken))
	}
}

func TestMediaSearchToolRequiresDurableSession(t *testing.T) {
	tool := MediaSearchTool{Service: NewMediaSearchService(mediaSearchTestClient(t, map[string]any{"results": nil}))}
	args := json.RawMessage(`{"kind":"image","query":"bee"}`)
	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "durable session") {
		t.Fatalf("error=%v", err)
	}
	if _, err := tool.Execute(withMediaSearchSession(context.Background(), SessionID("s")), args); err != nil {
		t.Fatalf("durable session request failed: %v", err)
	}
}

func TestMediaSearchToolModelOutputOmitsBindingsAndURLs(t *testing.T) {
	tool := MediaSearchTool{Service: NewMediaSearchService(mediaSearchTestClient(t, map[string]any{"results": []map[string]any{{
		"title": "Bee image", "img_src": "https://example.com/bee.jpg", "url": "https://example.com/source", "content": "bee result", "source": "https://example.com/source",
	}}}))}
	var event MediaSearchEvent
	ctx := withMediaSearchRunEmitter(withMediaSearchSession(context.Background(), SessionID("s")), func(value MediaSearchEvent) { event = value })
	output, err := tool.Execute(ctx, json.RawMessage(`{"kind":"image","query":"bee"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"\"t_", "\"c_", "source_url", "img_src", "https://", "http://"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("model output contains forbidden %q: %s", forbidden, output)
		}
	}
	if !strings.Contains(output, "NOT downloaded") || !strings.Contains(output, "media_retrieve") || !strings.Contains(output, "Do not emit Markdown image links") {
		t.Fatalf("model output lacks safety instructions: %s", output)
	}
	if len(event.Candidates) != 1 || event.Candidates[0].Token == "" || event.Candidates[0].SourceURL == "" {
		t.Fatalf("UI event did not retain selection bindings: %#v", event)
	}
}

func TestMediaSearchRunEventSuppressesCandidatesAfterAutoAttach(t *testing.T) {
	result := MediaSearchResult{
		Query:      "bee",
		Kind:       "image",
		Candidates: []MediaSearchCandidate{{CandidateID: "c_1", Token: "t_1", Kind: "image"}},
		Untrusted:  true,
		Attached:   true,
	}

	event := result.runEvent()
	if !event.AutoRetrieved {
		t.Fatal("auto-attached result did not set auto_retrieved")
	}
	if len(event.Candidates) != 0 {
		t.Fatalf("auto-attached event still exposed candidates: %#v", event.Candidates)
	}
}

func TestMediaSearchAutoRetrieveIsOneShotPerRun(t *testing.T) {
	tool := MediaSearchTool{Service: NewMediaSearchService(mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "Bee image one", "img_src": "https://example.com/bee-one.jpg", "url": "https://example.com/bee-one", "content": "bee image", "source": "example"},
		{"title": "Bee image two", "img_src": "https://example.com/bee-two.jpg", "url": "https://example.com/bee-two", "content": "bee image", "source": "example"},
	}}))}
	calls := 0
	ctx := withMediaSearchSession(context.Background(), SessionID("one-shot"))
	ctx = withMediaSearchAutoRetriever(ctx, func(context.Context, MediaSearchResult) MediaSearchAutoRetrieveResult {
		calls++
		return MediaSearchAutoRetrieveResult{Attached: true, AttachmentCount: 1}
	})
	first, err := tool.Execute(ctx, json.RawMessage(`{"kind":"image","query":"bee"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.Execute(ctx, json.RawMessage(`{"kind":"image","query":"bee again"}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("auto-retriever invoked %d times", calls)
	}
	var firstResult, secondResult MediaSearchModelResult
	if err := json.Unmarshal([]byte(first), &firstResult); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(second), &secondResult); err != nil {
		t.Fatal(err)
	}
	if !firstResult.Attached || firstResult.AttachmentCount != 1 || firstResult.NoAdditionalRetrieval {
		t.Fatalf("first retrieval state=%#v", firstResult)
	}
	if !secondResult.AlreadyAttached || !secondResult.NoAdditionalRetrieval || len(secondResult.Candidates) != 0 {
		t.Fatalf("repeated retrieval state=%#v", secondResult)
	}
}

func TestMediaSearchStripsMarkersAndRanksDeduplicatesRelevantResults(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "blue fox", "img_src": "https://example.com/fox.jpg", "url": "https://example.com/fox", "content": "irrelevant", "source": "wildlife"},
		{"title": "red panda photo\ue000", "img_src": "https://example.com/panda.jpg", "url": "https://example.com/panda", "content": "red panda habitat\ue001", "source": "wildlife"},
		{"title": "red panda photo", "img_src": "https://example.com/panda-duplicate.jpg", "url": "https://example.com/panda-duplicate", "content": "red panda habitat", "source": "wildlife"},
		{"title": "panda portrait", "img_src": "https://example.com/portrait.jpg", "url": "https://example.com/portrait", "content": "panda", "source": "wildlife"},
	}})
	items, truncated, err := client.SearchMedia(context.Background(), MediaSearchRequest{Kind: "image", Query: "red panda", Count: 10})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(items) != 2 {
		t.Fatalf("items=%#v truncated=%v", items, truncated)
	}
	if items[0].Title != "red panda photo" || strings.Contains(items[0].Title+items[0].Snippet+items[0].Source, "\ue000") || strings.Contains(items[0].Title+items[0].Snippet+items[0].Source, "\ue001") {
		t.Fatalf("markers or ranking not normalized: %#v", items)
	}
	if items[1].Title != "panda portrait" {
		t.Fatalf("unexpected deterministic ranking: %#v", items)
	}
}

func TestRuntimeEmitsMediaSearchUIEventSeparatelyFromToolResult(t *testing.T) {
	searchClient := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{{
		"title": "Bee image", "img_src": "https://example.com/bee.jpg", "url": "https://example.com/source", "content": "bee result", "source": "example",
	}}})
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "media-call", Type: "function", Name: "media_search", Arguments: `{"kind":"image","query":"bee"}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "choose one"}},
	}}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	app.MediaSearch = NewMediaSearchService(searchClient)
	registry, err := NewRegistry(MediaSearchTool{Service: app.MediaSearch})
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = NewExecutor(registry, ExecutionScope{Root: t.TempDir()})
	session, err := store.Create(SessionMetadata{Title: "media"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "find a bee image", MediaEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var searchEvent *RunEvent
	var toolResult *RunEvent
	for event := range events {
		copy := event
		if event.Type == RunMediaSearch {
			searchEvent = &copy
		}
		if event.Type == RunToolResult {
			toolResult = &copy
		}
	}
	if searchEvent == nil || searchEvent.MediaSearch == nil || len(searchEvent.MediaSearch.Candidates) != 1 {
		t.Fatalf("missing UI-only media event: %#v", searchEvent)
	}
	if searchEvent.MediaSearch.Candidates[0].Token == "" || searchEvent.MediaSearch.Candidates[0].SourceURL == "" {
		t.Fatalf("UI event lost selection metadata: %#v", searchEvent.MediaSearch)
	}
	if toolResult == nil || toolResult.ToolResult == nil {
		t.Fatal("missing media_search tool result")
	}
	for _, forbidden := range []string{"\"t_", "\"c_", "source_url", "img_src", "https://"} {
		if strings.Contains(toolResult.ToolResult.Output, forbidden) {
			t.Fatalf("tool result contains UI-only %q: %s", forbidden, toolResult.ToolResult.Output)
		}
	}
}

func newAutoMediaSearchApp(t *testing.T, retrievalErr error) (*Application, SessionID) {
	t.Helper()
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Fatalf("unexpected search request: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "Bee image one", "img_src": "https://example.com/bee-one.jpg", "url": "https://example.com/bee-one", "content": "bee image", "source": "example"},
			{"title": "Bee image two", "img_src": "https://example.com/bee-two.jpg", "url": "https://example.com/bee-two", "content": "bee image", "source": "example"},
			{"title": "Bee image three", "img_src": "https://example.com/bee-three.jpg", "url": "https://example.com/bee-three", "content": "bee image", "source": "example"},
			{"title": "Bee image four", "img_src": "https://example.com/bee-four.jpg", "url": "https://example.com/bee-four", "content": "bee image", "source": "example"},
		}})
	}))
	t.Cleanup(searchServer.Close)
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: searchServer.URL, TimeoutSeconds: 2, MaxResults: 5, MaxChars: 1024})
	if err != nil {
		t.Fatal(err)
	}
	store, artifacts, sessionID := newMediaTestStore(t)
	provider := &scriptedToolProvider{responses: []Response{
		{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "media-call", Type: "function", Name: "media_search", Arguments: `{"kind":"image","query":"bee"}`}}}},
		{Message: Message{Role: RoleAssistant, Content: "The image is attached."}},
	}}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	service := NewMediaService(artifacts)
	service.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	service.HTTPClient = mediaSearchHTTPClient{body: mustTinyPNG(t), err: retrievalErr}
	app.ArtifactStore = artifacts
	app.Media = service
	app.MediaSearch = NewMediaSearchService(client)
	registry, err := NewRegistry(MediaSearchTool{Service: app.MediaSearch})
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = NewExecutor(registry, ExecutionScope{Root: t.TempDir()})
	return app, sessionID
}

func mustTinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRuntimeAutoRetrievesTopMediaAndEmitsArtifactProgress(t *testing.T) {
	app, sessionID := newAutoMediaSearchApp(t, nil)
	events, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "show me a bee image", MediaEnabled: true, AutoRetrieveTop: true})
	if err != nil {
		t.Fatal(err)
	}
	var searchEvent *RunEvent
	var artifactEvent *RunEvent
	var output string
	progress := 0
	for event := range events {
		copy := event
		switch event.Type {
		case RunMediaSearch:
			searchEvent = &copy
		case RunMediaProgress:
			progress++
		case RunArtifact:
			artifactEvent = &copy
		case RunToolResult:
			if event.ToolResult != nil && event.ToolResult.ToolName == "media_search" {
				output = event.ToolResult.Output
			}
		case RunFailure:
			t.Fatalf("unexpected run failure: %s", event.Error)
		}
	}
	if searchEvent == nil || searchEvent.MediaSearch == nil || !searchEvent.MediaSearch.AutoRetrieved {
		t.Fatalf("missing successful auto-retrieval event: %#v", searchEvent)
	}
	if len(searchEvent.MediaSearch.Candidates) != 0 {
		t.Fatalf("auto event exposed chooser candidates after attachment: %#v", searchEvent.MediaSearch)
	}
	if artifactEvent == nil || artifactEvent.Artifact == nil || artifactEvent.Artifact.MediaType != "image/png" || progress == 0 {
		t.Fatalf("missing media lifecycle artifact=%#v progress=%d", artifactEvent, progress)
	}
	var modelResult MediaSearchModelResult
	if err := json.Unmarshal([]byte(output), &modelResult); err != nil {
		t.Fatal(err)
	}
	if !modelResult.Attached || modelResult.AttachmentCount != 1 || !strings.Contains(modelResult.Notice, "attached") {
		t.Fatalf("model-safe attachment state=%#v", modelResult)
	}
	for _, forbidden := range []string{"https://", "http://", "\"t_", "\"c_", "source_url", "img_src"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("model output leaked %q: %s", forbidden, output)
		}
	}
	app.MediaSearch.mu.Lock()
	remainingBindings := len(app.MediaSearch.byToken)
	app.MediaSearch.mu.Unlock()
	if remainingBindings != 3 {
		t.Fatalf("top candidate binding was not consumed; remaining bindings=%d", remainingBindings)
	}
}

type sequenceMediaHTTPClient struct {
	body   []byte
	errors []error
	calls  int
}

func (client *sequenceMediaHTTPClient) Do(request *http.Request) (*http.Response, error) {
	index := client.calls
	client.calls++
	if index < len(client.errors) && client.errors[index] != nil {
		return nil, client.errors[index]
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(client.body)), ContentLength: int64(len(client.body)), Request: request}, nil
}

func TestRuntimeAutoRetrieveRetriesNextCandidatesAndAttachesOne(t *testing.T) {
	app, sessionID := newAutoMediaSearchApp(t, nil)
	client := &sequenceMediaHTTPClient{body: mustTinyPNG(t), errors: []error{errors.New("first failed"), errors.New("second failed")}}
	app.Media.HTTPClient = client
	events, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "show me a bee image", MediaEnabled: true, AutoRetrieveTop: true})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := 0
	searchCandidates := -1
	for event := range events {
		if event.Type == RunArtifact {
			artifacts++
		}
		if event.Type == RunMediaSearch && event.MediaSearch != nil {
			searchCandidates = len(event.MediaSearch.Candidates)
		}
		if event.Type == RunFailure {
			t.Fatalf("unexpected run failure: %s", event.Error)
		}
	}
	if client.calls != 3 || artifacts != 1 || searchCandidates != 0 {
		t.Fatalf("retry behavior calls=%d artifacts=%d chooser_candidates=%d", client.calls, artifacts, searchCandidates)
	}
}

func TestMediaSearchAutoRetrieveFailureBoundsChooserFallback(t *testing.T) {
	result := MediaSearchResult{Kind: "video", AutoRetrieveFailed: true, ConsumedAttempts: 3}
	for index := 0; index < 8; index++ {
		result.Candidates = append(result.Candidates, MediaSearchCandidate{Title: fmt.Sprintf("candidate-%d", index+1)})
	}
	model := result.modelSafe()
	if len(model.Candidates) != MaxMediaChooserFallbackCandidates || model.Candidates[0].Title != "candidate-4" {
		t.Fatalf("model fallback=%#v", model.Candidates)
	}
	event := result.runEvent()
	if len(event.Candidates) != MaxMediaChooserFallbackCandidates || event.Candidates[0].Title != "candidate-4" {
		t.Fatalf("UI fallback=%#v", event.Candidates)
	}
}

func TestRuntimeAutoRetrieveFailureLeavesChooserForOtherCandidates(t *testing.T) {
	app, sessionID := newAutoMediaSearchApp(t, errors.New("retrieval failed"))
	events, err := app.Run(context.Background(), RunRequest{SessionID: sessionID, Prompt: "attach a bee image", MediaEnabled: true, AutoRetrieveTop: true})
	if err != nil {
		t.Fatal(err)
	}
	var searchEvent *RunEvent
	var output string
	failedProgress := false
	for event := range events {
		copy := event
		if event.Type == RunMediaSearch {
			searchEvent = &copy
		}
		if event.Type == RunMediaProgress && event.Media != nil && event.Media.Status == "failed" {
			failedProgress = true
		}
		if event.Type == RunToolResult && event.ToolResult != nil && event.ToolResult.ToolName == "media_search" {
			output = event.ToolResult.Output
		}
		if event.Type == RunFailure {
			t.Fatalf("search turn should remain usable after top retrieval failure: %s", event.Error)
		}
	}
	if searchEvent == nil || searchEvent.MediaSearch == nil || searchEvent.MediaSearch.AutoRetrieved || len(searchEvent.MediaSearch.Candidates) != 1 {
		t.Fatalf("failure should preserve chooser for remaining candidate: %#v", searchEvent)
	}
	if !failedProgress || !strings.Contains(output, "could not be attached") || strings.Contains(output, "https://") || strings.Contains(output, "http://") {
		t.Fatalf("failure status/progress unsafe: progress=%t output=%s", failedProgress, output)
	}
	if strings.Contains(output, "\"t_") || strings.Contains(output, "\"c_") {
		t.Fatalf("failure output leaked binding token: %s", output)
	}
}
