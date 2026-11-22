package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestMemorySemanticRebuildHonorsConfiguredProviderAndPortableRoots(t *testing.T) {
	home := filepath.Join(t.TempDir(), "vm-user", ".bee")
	t.Setenv("BEE_HOME", home)
	if _, err := bee.InitializeBee(home); err != nil {
		t.Fatal(err)
	}
	memoryRoot := filepath.Join(home, "workspace", "memory")
	service, err := bee.NewLocalMemoryService(memoryRoot, bee.DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := service.Remember(context.Background(), bee.RememberRequest{
		Actor:   bee.MemoryActor{Kind: bee.MemoryActorParent, ID: "semantic-test"},
		Record:  bee.MemoryRecord{ID: "memory-portable", Namespace: bee.MemoryNamespaceDurableMemory, Key: "portable", Content: "portable semantic memory", Sensitivity: bee.MemorySensitivityPublic, CreatedAt: now, UpdatedAt: now, Confidence: 1},
		AuditID: "semantic-portable",
		Reason:  "semantic portability test",
	}); err != nil {
		t.Fatal(err)
	}

	var gotModel, gotInput, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuthorization = request.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		gotModel, gotInput = body.Model, body.Input
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": []any{map[string]any{"embedding": []float64{1, 0, 0}}}})
	}))
	defer server.Close()

	keyPath := filepath.Join(home, "secrets", "embedding.key")
	if err := os.WriteFile(keyPath, []byte("embedding-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := []byte(`{
  "assistant": {"memory_root": "workspace/memory"},
  "semantic": {
    "enabled": true,
    "endpoint": "` + server.URL + `",
    "model": "portable-embedding",
    "api_key_file": "secrets/embedding.key",
    "timeout_seconds": 2,
    "index_root": "state/custom-semantic"
  }
}`)
	if err := os.WriteFile(filepath.Join(home, "config.json"), document, 0o600); err != nil {
		t.Fatal(err)
	}

	var output, errorOutput bytes.Buffer
	if err := memorySemanticRebuild(nil, &output, &errorOutput); err != nil {
		t.Fatalf("rebuild: %v stderr=%q", err, errorOutput.String())
	}
	if gotModel != "portable-embedding" || !strings.Contains(gotInput, "portable semantic memory") || gotAuthorization != "Bearer embedding-secret" {
		t.Fatalf("embedding request model=%q input=%q authorization=%q", gotModel, gotInput, gotAuthorization)
	}
	indexRoot := filepath.Join(home, "state", "custom-semantic")
	manifest, err := bee.SemanticIndexStatus(indexRoot)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Model != "portable-embedding" || manifest.MemoryRoot != memoryRoot || manifest.RecordCount != 1 {
		t.Fatalf("semantic manifest=%#v", manifest)
	}
}
