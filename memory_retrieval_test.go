package bee

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func rankedRecord(id, namespace, key, content string, confidence float64, updated time.Time) MemoryRecord {
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	record.ID = id
	record.Namespace = MemoryNamespace(namespace)
	record.Key = key
	record.Content = content
	record.Summary = content
	record.Confidence = confidence
	record.CreatedAt = updated.Add(-time.Hour)
	record.UpdatedAt = updated
	record.Version = 1
	return record
}

func appendRankedRecords(t *testing.T, store *LocalMemoryStore, records ...MemoryRecord) {
	t.Helper()
	for index, record := range records {
		if err := store.Append(context.Background(), localEvent("ranked-create-"+record.ID+"-"+string(rune('a'+index)), record)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalRankedRetrievalIsDeterministicAndFieldWeighted(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)
	keyMatch := rankedRecord("memory-key", "durable_memory", "preference.theme", "The user likes concise answers.", 0.5, asOf)
	contentMatch := rankedRecord("memory-content", "durable_memory", "preference.other", "The user mentioned preference.theme in a document.", 0.5, asOf)
	appendRankedRecords(t, store, keyMatch, contentMatch)
	query := MemoryQuery{Actor: testMemoryParent(), Text: "preference.theme", AsOf: asOf, Limit: 2}
	first, err := store.RetrieveRanked(context.Background(), query, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RetrieveRanked(context.Background(), query, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Hits) != 2 || first.Hits[0].Record.ID != "memory-key" {
		t.Fatalf("field weighting result = %#v", first)
	}
	if first.Hits[0].Score.Lexical <= first.Hits[1].Score.Lexical {
		t.Fatalf("key field should outrank content: %#v", first.Hits)
	}
	if first.Hits[0].Score.Phrase <= first.Hits[1].Score.Phrase {
		t.Fatalf("key phrase should outrank content phrase: %#v", first.Hits)
	}
	if first.Hits[0].Score.Validate() != nil {
		t.Fatal(first.Hits[0].Score.Validate())
	}
	firstBytes, _ := CanonicalJSON(first)
	secondBytes, _ := CanonicalJSON(second)
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("repeated ranked retrieval changed ordering or diagnostics:\n%s\n%s", firstBytes, secondBytes)
	}
}

func TestRankedRetrievalFiltersBeforeScoringAndProtectsChildren(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	policy.AllowSensitiveMemory = true
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)
	visible := rankedRecord("visible", "durable_memory/project", "answer.style", "Concise answer style.", 0.2, asOf)
	visible.SubjectID = "user-1"
	expired := rankedRecord("expired", "durable_memory/project", "answer.style", "Concise expired style.", 1, asOf.Add(-time.Hour))
	expired.ExpiresAt = asOf
	sensitive := visible
	sensitive.ID = "sensitive"
	sensitive.Sensitivity = MemorySensitivitySensitive
	otherSubject := visible
	otherSubject.ID = "other-subject"
	otherSubject.SubjectID = "user-2"
	appendRankedRecords(t, store, visible, expired, sensitive, otherSubject)
	childQuery := MemoryQuery{Actor: testMemoryChild(), Namespace: MemoryNamespace("durable_memory/project"), SubjectID: "user-1", Text: "concise", AsOf: asOf}
	result, err := store.RetrieveRanked(context.Background(), childQuery, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Record.ID != "visible" {
		t.Fatalf("filtered child result = %#v", result)
	}
	if result.Diagnostics.Filtered != 0 {
		t.Fatalf("child filtered diagnostics leaked inaccessible count: %d", result.Diagnostics.Filtered)
	}
	for _, hit := range result.Hits {
		if hit.Record.Sensitivity == MemorySensitivitySensitive {
			t.Fatal("sensitive record leaked to child")
		}
	}
	parentQuery := childQuery
	parentQuery.Actor = testMemoryParent()
	parentQuery.IncludeSensitive = true
	parent, err := store.RetrieveRanked(context.Background(), parentQuery, MemoryRetrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Hits) != 2 {
		t.Fatalf("parent filtered result = %#v", parent)
	}
}

func TestRankedRetrievalConfidenceRecencyAndBounds(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)
	oldHigh := rankedRecord("old-high", "durable_memory/a", "preference.one", "common phrase", 1, asOf.Add(-24*time.Hour))
	newLow := rankedRecord("new-low", "durable_memory/b", "preference.two", "common phrase", 0.1, asOf.Add(-time.Hour))
	third := rankedRecord("third", "durable_memory/c", "preference.three", "common phrase", 0.5, asOf)
	appendRankedRecords(t, store, oldHigh, newLow, third)
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "common phrase", AsOf: asOf}, MemoryRetrievalOptions{MaxCandidates: 2, MaxResults: 1, MaxExplanationBytes: 24, MaxReasonBytes: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Diagnostics.Candidates != 2 || result.Total != 3 || !result.Truncated {
		t.Fatalf("bounded ranked result = %#v", result)
	}
	if len(result.Hits[0].Explanation) > 24 || len(result.Hits[0].Reason) > 12 {
		t.Fatalf("explanation/reason bounds exceeded: %#v", result.Hits[0])
	}
	if result.Hits[0].Score.Confidence == 0 || result.Hits[0].Score.Recency == 0 {
		t.Fatalf("confidence/recency components missing: %#v", result.Hits[0].Score)
	}
}

func TestRankedRetrievalAppliesCandidateLimitAfterRelevance(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	appendRankedRecords(t, store, rankedRecord("old-match", "durable_memory", "rare", "rare exact phrase", .9, now.Add(-30*24*time.Hour)))
	for index := 0; index < 5; index++ {
		appendRankedRecords(t, store, rankedRecord("new-"+string(rune('a'+index)), "durable_memory", "recent", "unrelated recent memory", .9, now.Add(time.Duration(index)*time.Minute)))
	}
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "rare exact phrase", AsOf: now}, MemoryRetrievalOptions{MaxCandidates: 1, MaxResults: 1})
	if err != nil || len(result.Hits) != 1 || result.Hits[0].Record.ID != "old-match" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRankedRetrievalDiversityAndTransientProjection(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 16, 22, 0, 0, 0, time.UTC)
	appendRankedRecords(t, store,
		rankedRecord("a1", "durable_memory/a", "preference.a1", "shared phrase", .9, at),
		rankedRecord("a2", "durable_memory/a", "preference.a2", "shared phrase", .8, at),
		rankedRecord("b1", "durable_memory/b", "preference.b1", "shared phrase", .7, at),
	)
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "shared phrase", AsOf: at}, MemoryRetrievalOptions{MaxResults: 3, Diversity: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 || result.Hits[0].Record.Namespace == result.Hits[1].Record.Namespace || !result.Diagnostics.DiversityLimited {
		t.Fatalf("diversity result = %#v", result)
	}
	packet, err := store.RetrieveRankedContext(context.Background(), MemoryQuery{Actor: testMemoryChild(), Text: "shared phrase", AsOf: at}, MemoryRetrievalOptions{MaxResults: 1}, "session-ranked")
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 1 || packet.Items[0].Score <= 0 || packet.SessionID != "session-ranked" {
		t.Fatalf("ranked context = %#v", packet)
	}
}

func TestRankedRetrievalUpdateDeleteAndRebuildConsistency(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	record := rankedRecord("mutable", "durable_memory", "preference.old", "old phrase", .5, time.Date(2026, 8, 16, 21, 0, 0, 0, time.UTC))
	if err := store.Append(context.Background(), localEvent("mutable-create", record)); err != nil {
		t.Fatal(err)
	}
	updated := record
	updated.Key = "preference.new"
	updated.Content = "new phrase"
	if err := store.Append(context.Background(), localUpdate("mutable-update", updated, 1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.RebuildMemoryIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "new phrase"}, MemoryRetrievalOptions{})
	if err != nil || len(result.Hits) != 1 || result.Hits[0].Record.Key != "preference.new" {
		t.Fatalf("updated ranked result = %#v, err=%v", result, err)
	}
	if err := store.Append(context.Background(), MemoryEvent{ID: "mutable-delete", Type: MemoryEventDeleted, At: updated.UpdatedAt.Add(time.Minute), Actor: testMemoryParent(), RecordID: updated.ID, Explicit: true, ExpectedVersion: 2}); err != nil {
		t.Fatal(err)
	}
	result, err = store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "new phrase"}, MemoryRetrievalOptions{})
	if err != nil || len(result.Hits) != 0 {
		t.Fatalf("deleted ranked result = %#v, err=%v", result, err)
	}
}

type failingEmbedder struct{ err error }

func (embedder failingEmbedder) Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	return MemoryEmbedding{}, embedder.err
}

type fixedEmbedder struct{ embedding MemoryEmbedding }

func (embedder fixedEmbedder) Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	return embedder.embedding, nil
}

type sleepingEmbedder struct{}

func (sleepingEmbedder) Embed(ctx context.Context, _ MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	select {
	case <-time.After(200 * time.Millisecond):
		return MemoryEmbedding{Values: []float64{1}}, nil
	case <-ctx.Done():
		return MemoryEmbedding{}, ctx.Err()
	}
}

type fixedVectorIndex struct {
	hits []MemoryVectorHit
	err  error
}

func (index fixedVectorIndex) Search(context.Context, MemoryEmbedding, int) ([]MemoryVectorHit, error) {
	return index.hits, index.err
}

func TestRankedRetrievalOptionalSemanticFailureFallsBackBoundedly(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 16, 22, 0, 0, 0, time.UTC)
	record := rankedRecord("semantic", "durable_memory", "preference.semantic", "semantic phrase", .8, at)
	appendRankedRecords(t, store, record)
	query := MemoryQuery{Actor: testMemoryParent(), Text: "semantic phrase", AsOf: at}
	for _, tc := range []struct {
		name    string
		options MemoryRetrievalOptions
		want    string
	}{
		{"provider-error", MemoryRetrievalOptions{Embedder: failingEmbedder{err: errors.New("offline")}, VectorIndex: fixedVectorIndex{}}, "semantic provider failed"},
		{"invalid-vector", MemoryRetrievalOptions{Embedder: fixedEmbedder{embedding: MemoryEmbedding{Values: []float64{math.NaN()}}}, VectorIndex: fixedVectorIndex{}}, "semantic provider returned invalid vector"},
		{"index-error", MemoryRetrievalOptions{Embedder: fixedEmbedder{embedding: MemoryEmbedding{Values: []float64{1}}}, VectorIndex: fixedVectorIndex{err: errors.New("timeout")}}, "semantic index failed"},
		{"provider-timeout", MemoryRetrievalOptions{Embedder: sleepingEmbedder{}, VectorIndex: fixedVectorIndex{}, SemanticTimeout: time.Millisecond}, "semantic provider failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.RetrieveRanked(context.Background(), query, tc.options)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Diagnostics.Degraded || result.Diagnostics.DegradedReason != tc.want || len(result.Hits) != 1 {
				t.Fatalf("fallback result = %#v", result)
			}
		})
	}
	result, err := store.RetrieveRanked(context.Background(), query, MemoryRetrievalOptions{})
	if err != nil || result.Diagnostics.Degraded || result.Diagnostics.Semantic {
		t.Fatalf("no-provider default = %#v, err=%v", result, err)
	}
}

func TestMemoryScoreAndEmbeddingValidationRejectInvalidValues(t *testing.T) {
	for _, score := range []MemoryScore{{Total: math.NaN()}, {Total: math.Inf(1)}, {Total: 1.1}, {Total: .5, Value: .4}, {Total: .5, Semantic: -0.1}} {
		if err := score.Validate(); err == nil {
			t.Fatalf("invalid score accepted: %#v", score)
		}
	}
	for _, embedding := range []MemoryEmbedding{{}, {Values: []float64{0}}, {Values: []float64{math.Inf(1)}}} {
		if err := embedding.Validate(); err == nil {
			t.Fatalf("invalid embedding accepted: %#v", embedding)
		}
	}
	if err := (MemoryRetrievalOptions{MaxQueryTextBytes: 2}).validate(localMemoryPolicy(), MemoryQuery{Text: "long"}); err == nil || !strings.Contains(err.Error(), "query text") {
		t.Fatalf("query text bound error = %v", err)
	}
}

type observingEmbedder struct{ got MemoryEmbeddingRequest }

func (embedder *observingEmbedder) Embed(_ context.Context, request MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	embedder.got = request
	return MemoryEmbedding{Values: []float64{1}}, nil
}

func TestSemanticProviderReceivesNoMemoryTextByDefault(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 16, 22, 0, 0, 0, time.UTC)
	appendRankedRecords(t, store, rankedRecord("private-text", "durable_memory", "private.key", "private durable content", .8, at))
	embedder := &observingEmbedder{}
	_, err = store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "private durable", AsOf: at}, MemoryRetrievalOptions{Embedder: embedder, VectorIndex: fixedVectorIndex{}})
	if err != nil {
		t.Fatal(err)
	}
	if embedder.got.AllowMemoryText || embedder.got.QueryText != "private durable" {
		t.Fatalf("unexpected semantic provider request: %#v", embedder.got)
	}
}
