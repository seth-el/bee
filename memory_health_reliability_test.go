package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type maxDimensionSemanticEmbedder struct{}

func (maxDimensionSemanticEmbedder) Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	values := make([]float64, MaxMemoryEmbeddingDimensions)
	for i := range values {
		values[i] = 1
	}
	return MemoryEmbedding{Values: values}, nil
}

func TestSemanticHealthCodesMatrixAndValidEmptyIndex(t *testing.T) {
	root := privateTempDir(t)
	missingRoot := filepath.Join(t.TempDir(), "missing-index")
	if got := (SemanticConfig{}).Health(root).Code; got != SemanticHealthDisabled {
		t.Fatalf("disabled health=%q", got)
	}
	config := SemanticConfig{Enabled: true, Endpoint: "http://127.0.0.1:8090", Model: "test-model", IndexRoot: missingRoot}
	if got := config.Health(root).Code; got != SemanticHealthMissing {
		t.Fatalf("missing health=%q", got)
	}
	badRoot := filepath.Join(t.TempDir(), "bad-index")
	if err := os.MkdirAll(badRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, SemanticIndexManifestFile), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.IndexRoot = badRoot
	if got := config.Health(root).Code; got != SemanticHealthInvalid {
		t.Fatalf("invalid health=%q", got)
	}
	emptyIndex := filepath.Join(t.TempDir(), "empty-index")
	if _, err := BuildSemanticIndex(context.Background(), root, emptyIndex, "test-model", fixedSemanticEmbedder{}); err != nil {
		t.Fatal(err)
	}
	config.IndexRoot = emptyIndex
	if got := config.Health(root).Code; got != SemanticHealthEmpty {
		t.Fatalf("empty health=%q", got)
	}
}

func TestSemanticFallbackRestoresLexicalMatchesOutsideCandidateWindow(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := rankedRecord("old-lexical", "durable_memory", "old", "rare lexical phrase", .9, now.Add(-time.Hour))
	appendRankedRecords(t, store, old)
	for i := 0; i < 3; i++ {
		appendRankedRecords(t, store, rankedRecord("new-"+string(rune('a'+i)), "durable_memory", "new", "unrelated", .9, now.Add(time.Duration(i)*time.Minute)))
	}
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "rare lexical phrase", AsOf: now}, MemoryRetrievalOptions{SemanticEnabled: true, MaxCandidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Record.ID != old.ID || !result.Diagnostics.Degraded {
		t.Fatalf("fallback=%#v", result)
	}
}

func TestMemorySearchToolProjectsSemanticHealthCodes(t *testing.T) {
	data, err := marshalMemorySearchToolResult(MemorySearchResult{Diagnostics: MemoryRetrievalDiagnostics{Degraded: true, DegradedReason: "semantic index is stale", SemanticHealth: string(SemanticHealthStale), DegradedCode: string(SemanticHealthStale)}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result memorySearchToolResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.SemanticHealth != string(SemanticHealthStale) || result.Diagnostics.DegradedCode != string(SemanticHealthStale) {
		t.Fatalf("diagnostics=%+v", result.Diagnostics)
	}
}

func TestSemanticIndexLoadsMaximumDimensionAndRejectsMixedGeneration(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	appendRankedRecords(t, store, rankedRecord("max-vector", "durable_memory", "vector", "maximum dimension semantic vector", .9, time.Now().UTC()))
	indexRoot := filepath.Join(t.TempDir(), "semantic-index")
	if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", maxDimensionSemanticEmbedder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSemanticIndex(indexRoot); err != nil {
		t.Fatalf("load maximum-dimension index: %v", err)
	}
	vectorPath := filepath.Join(indexRoot, SemanticIndexVectorsFile)
	data, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatal(err)
	}
	modified := bytes.Replace(data, []byte("1,1"), []byte("2,1"), 1)
	if bytes.Equal(modified, data) {
		t.Fatal("failed to modify semantic vector fixture")
	}
	if err := os.WriteFile(vectorPath, modified, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSemanticIndex(indexRoot); err == nil {
		t.Fatal("mixed-generation semantic index unexpectedly loaded")
	}
}

func TestSemanticRuntimeHealthCachesFreshMetadataAndDetectsMutation(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	appendRankedRecords(t, store, rankedRecord("first", "durable_memory", "first", "first semantic memory", .9, now))
	indexRoot := filepath.Join(t.TempDir(), "semantic-index")
	if _, err := BuildSemanticIndex(context.Background(), root, indexRoot, "test-model", maxDimensionSemanticEmbedder{}); err != nil {
		t.Fatal(err)
	}
	config := SemanticConfig{Enabled: true, Endpoint: "http://127.0.0.1:8090", Model: "test-model", IndexRoot: indexRoot}
	options := config.RetrievalOptions(root)
	if options.SemanticHealth != SemanticHealthReady {
		t.Fatalf("initial health=%q", options.SemanticHealth)
	}
	appendRankedRecords(t, store, rankedRecord("second", "durable_memory", "second", "new lexical memory", .9, now.Add(time.Minute)))
	result, err := store.RetrieveRanked(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "new lexical memory"}, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Diagnostics.Degraded || result.Diagnostics.DegradedCode != string(SemanticHealthStale) || len(result.Hits) == 0 || result.Hits[0].Record.ID != "second" {
		t.Fatalf("result=%#v", result)
	}
}
