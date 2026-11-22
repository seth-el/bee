package bee

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleEmbedderUsesEmbeddingsEndpointAndBoundsResponse(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		var body embeddingsRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "test-embedding" || body.Input != "hello" {
			t.Errorf("request=%#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(embeddingsResponse{Data: []struct {
			Embedding []float64 `json:"embedding"`
		}{{Embedding: []float64{1, 0}}}})
	}))
	defer server.Close()
	embedder, err := NewOpenAICompatibleEmbedder(server.URL, "test-embedding", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	embedding, err := embedder.Embed(context.Background(), MemoryEmbeddingRequest{QueryText: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/embeddings" || len(embedding.Values) != 2 {
		t.Fatalf("path=%q embedding=%#v", gotPath, embedding)
	}
}

func TestFileMemoryVectorIndexSearchesAndRejectsStaleMemory(t *testing.T) {
	memoryRoot := privateTempDir(t)
	indexRoot := filepath.Join(t.TempDir(), "index")
	store, err := NewLocalMemoryStore(memoryRoot, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	record := rankedRecord("vector-record", "durable_memory", "semantic", "cats and mountains", .9, time.Now().UTC())
	appendRankedRecords(t, store, record)
	embedder := fixedSemanticEmbedder{vectors: map[string]MemoryEmbedding{record.ID: {Values: []float64{1, 0}}}}
	manifest, err := BuildSemanticIndex(context.Background(), memoryRoot, indexRoot, "test-model", embedder)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RecordCount != 1 || manifest.Dimensions != 2 {
		t.Fatalf("manifest=%#v", manifest)
	}
	index, err := LoadSemanticIndex(indexRoot)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := index.Search(context.Background(), MemoryEmbedding{Values: []float64{1, 0}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RecordID != record.ID || hits[0].Score != 1 {
		t.Fatalf("hits=%#v", hits)
	}
	if _, err := VerifySemanticIndex(memoryRoot, indexRoot); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), MemoryEvent{ID: "new-event", Type: MemoryEventCreated, At: time.Now().UTC(), Actor: testMemoryParent(), Record: rankedRecord("new-record", "durable_memory", "new", "new", .8, time.Now().UTC()), Explicit: true, Mode: MemoryWriteExplicit, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySemanticIndex(memoryRoot, indexRoot); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale verify error=%v", err)
	}
}

func TestM6SemanticRetrievalFindsNonLexicalMatchAndFallsBack(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	record := rankedRecord("semantic-match", "durable_memory", "travel", "Smoky Mountains trip", .9, at)
	appendRankedRecords(t, store, record)
	options := MemoryRetrievalOptions{SemanticEnabled: true, Embedder: fixedSemanticEmbedder{vectors: map[string]MemoryEmbedding{"query": {Values: []float64{1, 0}}, record.ID: {Values: []float64{1, 0}}}}, VectorIndex: fixedVectorIndex{hits: []MemoryVectorHit{{RecordID: record.ID, Score: 1}}}}
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "Appalachian vacation", AsOf: at}, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Record.ID != record.ID || !result.Diagnostics.Semantic {
		t.Fatalf("semantic result=%#v", result)
	}
	fallback, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "Appalachian vacation", AsOf: at}, MemoryRetrievalOptions{SemanticEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Diagnostics.Semantic || !fallback.Diagnostics.Degraded || len(fallback.Hits) != 0 {
		t.Fatalf("fallback=%#v", fallback)
	}
}

func TestSemanticConfigWiresHybridModelFacingSearch(t *testing.T) {
	root := privateTempDir(t)
	indexRoot := filepath.Join(t.TempDir(), "index")
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	store, ok := service.Memory.(*LocalMemoryStore)
	if !ok {
		t.Fatal("local service does not expose local memory store")
	}
	record := rankedRecord("config-semantic", "durable_memory", "travel", "Smoky Mountains trip", .9, time.Now().UTC())
	appendRankedRecords(t, store, record)
	if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", fixedSemanticEmbedder{vectors: map[string]MemoryEmbedding{record.ID: {Values: []float64{1, 0}}}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(embeddingsResponse{Data: []struct {
			Embedding []float64 `json:"embedding"`
		}{{Embedding: []float64{1, 0}}}})
	}))
	defer server.Close()
	options := (SemanticConfig{Enabled: true, Endpoint: server.URL, Model: "test-model", IndexRoot: indexRoot}).RetrievalOptions(root)
	if !options.SemanticEnabled || options.Embedder == nil || options.VectorIndex == nil {
		t.Fatalf("semantic options=%#v", options)
	}
	tool := MemorySearchTool{Service: service, Actor: testMemoryParent(), Policy: DefaultMemoryPolicy(), Retrieval: options}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"Appalachian vacation"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"semantic":true`) || !strings.Contains(output, record.ID) {
		t.Fatalf("hybrid model-facing search output=%s", output)
	}
}

func TestSemanticRetrievalDisabledKeepsLexicalPath(t *testing.T) {
	root := privateTempDir(t)
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	store := service.Memory.(*LocalMemoryStore)
	record := rankedRecord("lexical-only", "durable_memory", "travel", "Smoky Mountains trip", .9, time.Now().UTC())
	appendRankedRecords(t, store, record)
	tool := MemorySearchTool{Service: service, Actor: testMemoryParent(), Policy: DefaultMemoryPolicy(), Retrieval: (SemanticConfig{}).RetrievalOptions(root)}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"Smoky Mountains"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, `"semantic":true`) || !strings.Contains(output, record.ID) {
		t.Fatalf("lexical-only model-facing search output=%s", output)
	}
}

func TestSemanticConfigMissingOrStaleIndexFallsBackBoundedly(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale bool
	}{
		{name: "missing"},
		{name: "stale", stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := privateTempDir(t)
			indexRoot := filepath.Join(t.TempDir(), "index")
			service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
			if err != nil {
				t.Fatal(err)
			}
			store := service.Memory.(*LocalMemoryStore)
			record := rankedRecord("fallback-"+test.name, "durable_memory", "travel", "Smoky Mountains trip", .9, time.Now().UTC())
			appendRankedRecords(t, store, record)
			if test.stale {
				if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", fixedSemanticEmbedder{vectors: map[string]MemoryEmbedding{record.ID: {Values: []float64{1, 0}}}}); err != nil {
					t.Fatal(err)
				}
				if err := store.Append(context.Background(), MemoryEvent{ID: "stale-event", Type: MemoryEventCreated, At: time.Now().UTC(), Actor: testMemoryParent(), Record: rankedRecord("stale-new", "durable_memory", "new", "new", .8, time.Now().UTC()), Explicit: true, Mode: MemoryWriteExplicit, Reason: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			options := (SemanticConfig{Enabled: true, Endpoint: "http://127.0.0.1:1", Model: "test-model", IndexRoot: indexRoot}).RetrievalOptions(root)
			result, err := service.Search(context.Background(), SearchMemoryRequest{Query: MemoryQuery{Actor: testMemoryParent(), Text: "Appalachian vacation"}, Options: options, Bounds: MemoryDisplayBounds{}})
			if err != nil {
				t.Fatal(err)
			}
			if result.Diagnostics.Semantic || !result.Diagnostics.Degraded || result.Diagnostics.DegradedReason == "" {
				t.Fatalf("fallback diagnostics=%#v", result.Diagnostics)
			}
			if len(result.Hits) != 0 {
				t.Fatalf("non-lexical fallback returned hits=%#v", result.Hits)
			}
		})
	}
}

func TestM6SemanticIndexDoesNotPersistSecretsOrUseExternalProvider(t *testing.T) {
	root := privateTempDir(t)
	indexRoot := filepath.Join(t.TempDir(), "index")
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	record := rankedRecord("secret", "durable_memory", "secret", "safe content", .9, time.Now().UTC())
	record.Sensitivity = MemorySensitivityPersonal
	appendRankedRecords(t, store, record)
	if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "model", fixedSemanticEmbedder{vectors: map[string]MemoryEmbedding{record.ID: {Values: []float64{1}}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(indexRoot, SemanticIndexVectorsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), record.Content) || strings.Contains(string(data), record.Summary) {
		t.Fatal("semantic vector index persisted memory text")
	}
}

type fixedSemanticEmbedder struct{ vectors map[string]MemoryEmbedding }

func (embedder fixedSemanticEmbedder) Embed(_ context.Context, request MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	if vector, ok := embedder.vectors[request.QueryText]; ok {
		return vector, nil
	}
	for _, vector := range embedder.vectors {
		return vector, nil
	}
	return MemoryEmbedding{}, errors.New("no fixed vector")
}

var _ = os.ErrNotExist

type countingSemanticEmbedder struct{ calls int }

func (embedder *countingSemanticEmbedder) Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	embedder.calls++
	return MemoryEmbedding{Values: []float64{1, 0}}, nil
}

func TestSemanticIndexRebuildReusesUnchangedVectors(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	appendRankedRecords(t, store,
		rankedRecord("one", "durable_memory", "one", "first memory", .9, now),
		rankedRecord("two", "durable_memory", "two", "second memory", .9, now),
	)
	indexRoot := filepath.Join(t.TempDir(), "semantic-index")
	embedder := &countingSemanticEmbedder{}
	first, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", embedder)
	if err != nil || first.EmbeddedRecords != 2 || first.ReusedRecords != 0 || embedder.calls != 2 {
		t.Fatalf("first=%#v calls=%d err=%v", first, embedder.calls, err)
	}
	appendRankedRecords(t, store, rankedRecord("three", "durable_memory", "three", "third memory", .9, now))
	second, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", embedder)
	if err != nil || second.EmbeddedRecords != 1 || second.ReusedRecords != 2 || embedder.calls != 3 {
		t.Fatalf("second=%#v calls=%d err=%v", second, embedder.calls, err)
	}
}

func TestLiveSemanticIndexObservesPublishedGenerationAndFiltersCandidates(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	appendRankedRecords(t, store,
		rankedRecord("allowed", "durable_memory", "allowed", "allowed memory", .9, now),
		rankedRecord("blocked", "durable_memory", "blocked", "blocked memory", .9, now),
	)
	indexRoot := filepath.Join(t.TempDir(), "semantic-index")
	live := &LiveMemoryVectorIndex{MemoryRoot: root, IndexRoot: indexRoot, Model: "test-model"}
	if code := live.runtimeHealth(context.Background()); code != SemanticHealthMissing {
		t.Fatalf("health before build=%q", code)
	}
	if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", &countingSemanticEmbedder{}); err != nil {
		t.Fatal(err)
	}
	if code := live.runtimeHealth(context.Background()); code != SemanticHealthReady {
		t.Fatalf("health after build=%q", code)
	}
	hits, err := live.SearchFiltered(context.Background(), MemoryEmbedding{Values: []float64{1, 0}}, map[string]struct{}{"allowed": {}}, 10)
	if err != nil || len(hits) != 1 || hits[0].RecordID != "allowed" {
		t.Fatalf("hits=%#v err=%v", hits, err)
	}
}
