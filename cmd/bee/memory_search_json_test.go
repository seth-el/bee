package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func memorySearchJSONFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "memory")
	policy := bee.DefaultMemoryPolicy()
	policy.AllowSensitiveMemory = true
	service, err := bee.NewLocalMemoryService(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, record := range []bee.MemoryRecord{
		{ID: "pref-public", Namespace: bee.MemoryNamespaceDurableMemory, Key: "owner-preferences", Content: "Owner preferences: " + strings.Repeat("plain answers ", 300), Summary: "The owner prefers plain answers", Tags: map[string]string{"topic": "style", "flag": "yes"}, Source: "daily 2026-11-19", Sensitivity: bee.MemorySensitivityPublic, CreatedAt: updated, UpdatedAt: updated, Confidence: 1},
		{ID: "pref-sensitive", Namespace: bee.MemoryNamespaceDurableMemory, Key: "owner-preferences-private", Content: "Owner preferences private detail", Sensitivity: bee.MemorySensitivitySensitive, CreatedAt: updated, UpdatedAt: updated, Confidence: 1},
	} {
		if _, err := service.Remember(context.Background(), bee.RememberRequest{Actor: bee.MemoryActor{Kind: bee.MemoryActorParent, ID: "fixture"}, Record: record, AuditID: "remember-" + record.ID, Reason: "json fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runMemorySearchJSON(t *testing.T, args ...string) (map[string]any, []byte) {
	t.Helper()
	var output, errorOutput bytes.Buffer
	if err := runMemory(append([]string{"search"}, args...), &output, &errorOutput); err != nil {
		t.Fatalf("search: %v (%s)", err, errorOutput.String())
	}
	var document map[string]any
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("decode %q: %v", output.String(), err)
	}
	return document, output.Bytes()
}

func TestMemorySearchJSONContract(t *testing.T) {
	root := memorySearchJSONFixture(t)
	document, raw := runMemorySearchJSON(t, "--memory-root", root, "--query", "Owner preferences", "--json")
	if len(document) != 1 {
		t.Fatalf("top-level keys: %v", document)
	}
	results, ok := document["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results=%v", document["results"])
	}
	hit := results[0].(map[string]any)
	for _, key := range []string{"path", "score", "snippet", "citation", "corpus", "key", "content", "summary", "tags", "sensitivity", "source", "update_date"} {
		if _, present := hit[key]; !present {
			t.Fatalf("missing %q in %v", key, hit)
		}
	}
	if hit["path"] != "owner-preferences" || hit["snippet"] != "The owner prefers plain answers" || hit["corpus"] != string(bee.MemoryNamespaceDurableMemory) {
		t.Fatalf("hit=%v", hit)
	}
	if score, _ := hit["score"].(float64); score <= 0 {
		t.Fatalf("score=%v", hit["score"])
	}
	if hit["update_date"] != "2026-11-19T12:00:00Z" || hit["citation"] != "daily 2026-11-19" {
		t.Fatalf("hit=%v", hit)
	}
	if tags, _ := hit["tags"].([]any); len(tags) != 2 || tags[0] != "flag=yes" || tags[1] != "topic=style" {
		t.Fatalf("tags=%v", hit["tags"])
	}
	if content, _ := hit["content"].(string); len([]rune(content)) > memorySearchJSONMaxChars {
		t.Fatalf("content not bounded: %d", len(content))
	}
	// Keys come out in the documented order.
	if !strings.HasPrefix(string(raw), `{"results":[{"path":`) || strings.Index(string(raw), `"update_date"`) < strings.Index(string(raw), `"source"`) {
		t.Fatalf("key order: %s", raw)
	}
	if strings.Contains(string(raw), "private detail") || strings.Contains(string(raw), "owner-preferences-private") {
		t.Fatalf("sensitive record leaked: %s", raw)
	}
}

func TestMemorySearchJSONEmptyResults(t *testing.T) {
	root := memorySearchJSONFixture(t)
	_, raw := runMemorySearchJSON(t, "--memory-root", root, "--query", "zzqx nothing matches this", "--json")
	if string(raw) != "{\"results\":[]}\n" {
		t.Fatalf("empty output=%q", raw)
	}
}

func TestMemorySearchJSONIncludeSensitiveMatchesHumanPath(t *testing.T) {
	root := memorySearchJSONFixture(t)
	document, _ := runMemorySearchJSON(t, "--memory-root", root, "--query", "Owner preferences private detail", "--include-sensitive", "--json")
	found := false
	for _, item := range document["results"].([]any) {
		hit := item.(map[string]any)
		if hit["key"] == "owner-preferences-private" {
			found = hit["content"] == "Owner preferences private detail" && hit["sensitivity"] == string(bee.MemorySensitivitySensitive)
		}
	}
	if !found {
		t.Fatalf("include-sensitive should return the sensitive record: %v", document)
	}
}

func TestMemorySearchJSONLeavesDefaultOutputUnchanged(t *testing.T) {
	root := memorySearchJSONFixture(t)
	var output, errorOutput bytes.Buffer
	if err := runMemory([]string{"search", "--memory-root", root, "--query", "Owner preferences"}, &output, &errorOutput); err != nil {
		t.Fatal(err)
	}
	var result bee.MemorySearchResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Hits) == 0 {
		t.Fatalf("default output should stay the full search result: %v %s", err, output.String())
	}
	if strings.Contains(output.String(), `"results"`) {
		t.Fatalf("default output changed: %s", output.String())
	}
}

func TestMemorySearchJSONBoundsAndRedaction(t *testing.T) {
	hits := make([]bee.MemoryDisplayHit, 0, 30)
	hits = append(hits, bee.MemoryDisplayHit{Record: bee.MemoryDisplayRecord{Record: bee.MemoryRecord{Key: "hidden"}, Redacted: true}})
	for index := 0; index < 29; index++ {
		hits = append(hits, bee.MemoryDisplayHit{Record: bee.MemoryDisplayRecord{Record: bee.MemoryRecord{Key: "k", Content: strings.Repeat("é", 3000)}}})
	}
	document := memorySearchJSON(bee.MemorySearchResult{Hits: hits})
	if len(document.Results) != memorySearchJSONMaxResults {
		t.Fatalf("results=%d", len(document.Results))
	}
	for _, result := range document.Results {
		if result.Key == "hidden" {
			t.Fatal("redacted hit emitted")
		}
		if len([]rune(result.Content)) != memorySearchJSONMaxChars || result.Snippet != result.Content || result.Tags == nil {
			t.Fatalf("result=%+v", len([]rune(result.Content)))
		}
	}
}
