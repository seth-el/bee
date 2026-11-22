package bee

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

type delayedMediaRunner struct {
	delegate MediaCommandRunner
	delay    time.Duration
}

func (runner delayedMediaRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	timer := time.NewTimer(runner.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return runner.delegate.Run(ctx, dir, name, args...)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestMediaSearchResolveRankUsesLatestSessionBatchAndSingleUse(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "old one latest", "url": "https://example.com/old-one", "content": "old latest", "source": "example"},
		{"title": "old two latest", "url": "https://example.com/old-two", "content": "old latest", "source": "example"},
	}})
	service := NewMediaSearchService(client)
	session := SessionID("latest-session")
	if _, err := service.Search(context.Background(), session, MediaSearchRequest{Kind: "video", Query: "old"}); err != nil {
		t.Fatal(err)
	}
	latest, err := service.Search(context.Background(), session, MediaSearchRequest{Kind: "video", Query: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := service.ResolveRank(session, 2)
	if err != nil || selection.URL != latest.Candidates[1].SourceURL {
		t.Fatalf("selection=%#v latest=%#v err=%v", selection, latest, err)
	}
	if _, err := service.ResolveRank(session, 2); err == nil {
		t.Fatal("rank was reusable after single-use resolution")
	}
	if _, err := service.ResolveRank(SessionID("other-session"), 1); err == nil {
		t.Fatal("wrong session resolved a prior batch")
	}
}

func TestMediaSearchResolveRankSkipsConsumedAutoCandidates(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "one", "url": "https://example.com/one.mp4", "content": "slow", "source": "example"},
		{"title": "two", "url": "https://example.com/two.mp4", "content": "slow", "source": "example"},
		{"title": "three", "url": "https://example.com/three.mp4", "content": "slow", "source": "example"},
	}})
	service := NewMediaSearchService(client)
	sessionID := SessionID("consumed-ranks")
	if _, err := service.Search(context.Background(), sessionID, MediaSearchRequest{Kind: "video", Query: "slow"}); err != nil {
		t.Fatal(err)
	}
	service.ConsumeAttempts(sessionID, 2)
	selection, err := service.ResolveRank(sessionID, 1)
	if err != nil || selection.URL != "https://example.com/three.mp4" {
		t.Fatalf("selection=%#v err=%v", selection, err)
	}
}

func TestMediaSearchResolveRankEnforcesExpiry(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "expiring", "url": "https://example.com/expiring", "content": "expiring", "source": "example"},
	}})
	now := time.Unix(100, 0).UTC()
	service := NewMediaSearchService(client)
	service.TTL = time.Minute
	service.Now = func() time.Time { return now }
	if _, err := service.Search(context.Background(), SessionID("expiry"), MediaSearchRequest{Kind: "video", Query: "expiring"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := service.ResolveRank(SessionID("expiry"), 1); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry error=%v", err)
	}
}

func TestMediaRetrieveToolAttachesArtifactAndEmitsSafeProgress(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "Blackstone Island one", "url": "https://example.com/one", "content": "Blackstone Island", "source": "example"},
		{"title": "Blackstone Island two", "url": "https://example.com/two", "content": "Blackstone Island", "source": "example"},
		{"title": "Blackstone Island three", "url": "https://example.com/three", "content": "Blackstone Island", "source": "example"},
	}})
	search := NewMediaSearchService(client)
	_, artifacts, sessionID := newMediaTestStore(t)
	media := NewMediaService(artifacts)
	media.CommandRun = &fakeMediaRunner{}
	media.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	if _, err := search.Search(context.Background(), sessionID, MediaSearchRequest{Kind: "video", Query: "Blackstone Island"}); err != nil {
		t.Fatal(err)
	}
	var progress []MediaJobEvent
	var emitted []Artifact
	ctx := withMediaSearchSession(context.Background(), sessionID)
	ctx = withArtifactContext(ctx, sessionID, "run-followup")
	ctx = withMediaProgressEmitter(ctx, func(event MediaJobEvent) { progress = append(progress, event) })
	ctx = withArtifactRunEmitter(ctx, func(artifact Artifact) { emitted = append(emitted, artifact) })
	output, err := (MediaRetrieveTool{Search: search, Media: media}).Execute(ctx, json.RawMessage(`{"rank":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var result MediaRetrieveModelResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "attached" || result.Rank != 3 || result.Kind != "video" || result.ArtifactCount != 1 || len(result.Artifacts) != 1 || result.Artifacts[0].Filename != "video.mp4" {
		t.Fatalf("retrieve result=%#v", result)
	}
	if len(progress) == 0 || len(emitted) != 1 || emitted[0].MediaType != "video/mp4" {
		t.Fatalf("progress=%d artifacts=%#v", len(progress), emitted)
	}
	for _, forbidden := range []string{"https://", "http://", "\"t_", "\"c_", "source_url", "/tmp/", "source HTML"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("model output leaked %q: %s", forbidden, output)
		}
	}
	if _, err := artifacts.Get(emitted[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRetrieveToolRejectsURLTokenAndUnconfiguredContexts(t *testing.T) {
	tool := MediaRetrieveTool{Search: NewMediaSearchService(nil), Media: NewMediaService(nil)}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"rank":1,"url":"https://example.com"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unexpected URL argument result: %v", err)
	}
}

func TestMediaRetrieveExecutorUsesBoundedMediaTimeoutAndParentCancellation(t *testing.T) {
	client := mediaSearchTestClient(t, map[string]any{"results": []map[string]any{
		{"title": "slow video", "url": "https://example.com/slow", "content": "slow video", "source": "example"},
	}})
	search := NewMediaSearchService(client)
	sessions, artifacts, sessionID := newMediaTestStore(t)
	media := NewMediaService(artifacts)
	media.Timeout = 2 * time.Second
	media.CommandRun = delayedMediaRunner{delegate: &fakeMediaRunner{}, delay: 300 * time.Millisecond}
	media.ValidateURL = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	if got := (MediaRetrieveTool{Media: media}).ExecutionTimeout(); got != media.Timeout {
		t.Fatalf("media execution timeout=%s, want %s", got, media.Timeout)
	}
	if _, err := search.Search(context.Background(), sessionID, MediaSearchRequest{Kind: "video", Query: "slow"}); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(MediaRetrieveTool{Search: search, Media: media})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, ExecutionScope{Root: t.TempDir(), TimeoutSeconds: 1})
	ctx := withMediaSearchSession(context.Background(), sessionID)
	ctx = withArtifactContext(ctx, sessionID, "run-slow")
	result, err := executor.Execute(ctx, MediaRetrieveToolName, json.RawMessage(`{"rank":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var attached MediaRetrieveModelResult
	if err := json.Unmarshal([]byte(result.Output), &attached); err != nil {
		t.Fatal(err)
	}
	if attached.Status != "attached" {
		t.Fatalf("slow retrieval result=%#v", attached)
	}

	second, err := sessions.Create(SessionMetadata{Title: "cancelled media"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := search.Search(context.Background(), second.ID, MediaSearchRequest{Kind: "video", Query: "slow"}); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cancelCtx = withMediaSearchSession(cancelCtx, second.ID)
	cancelCtx = withArtifactContext(cancelCtx, second.ID, "run-cancelled")
	cancelled, err := executor.Execute(cancelCtx, MediaRetrieveToolName, json.RawMessage(`{"rank":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var failed MediaRetrieveModelResult
	if err := json.Unmarshal([]byte(cancelled.Output), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" {
		t.Fatalf("cancelled retrieval result=%#v", failed)
	}
}
