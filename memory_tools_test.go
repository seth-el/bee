package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type captureMemorySearchService struct {
	MemoryService
	request SearchMemoryRequest
	result  MemorySearchResult
}

func (service *captureMemorySearchService) Search(_ context.Context, request SearchMemoryRequest) (MemorySearchResult, error) {
	service.request = request
	return service.result, nil
}

type captureMemoryRememberService struct {
	MemoryService
	searchResult    MemorySearchResult
	searchErr       error
	rememberErr     error
	searchCalls     int
	rememberCalls   int
	rememberRequest RememberRequest
}

func (service *captureMemoryRememberService) Search(_ context.Context, request SearchMemoryRequest) (MemorySearchResult, error) {
	service.searchCalls++
	return service.searchResult, service.searchErr
}

func (service *captureMemoryRememberService) Remember(_ context.Context, request RememberRequest) (MemoryRecord, error) {
	service.rememberCalls++
	service.rememberRequest = request
	if service.rememberErr != nil {
		return MemoryRecord{}, service.rememberErr
	}
	request.Record.Version = 1
	request.Record.CreatedBy = request.Actor
	return request.Record, nil
}

type captureMemoryEmbedder struct{}

func (captureMemoryEmbedder) Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	return MemoryEmbedding{Values: []float64{1}}, nil
}

type captureMemoryVectorIndex struct{}

func (captureMemoryVectorIndex) Search(context.Context, MemoryEmbedding, int) ([]MemoryVectorHit, error) {
	return nil, nil
}

func TestMemorySearchToolProjectsBoundedCompactHits(t *testing.T) {
	hits := make([]MemoryDisplayHit, 12)
	for index := range hits {
		hits[index] = MemoryDisplayHit{
			Record: MemoryDisplayRecord{Record: MemoryRecord{
				ID:         "memory-" + string(rune('a'+index)),
				Namespace:  MemoryNamespaceDurableMemory,
				Key:        "key",
				Content:    strings.Repeat("x", MaxMemoryTextLength),
				Summary:    "summary",
				Source:     "source",
				Provenance: &MemoryProvenance{RelativePath: "should-not-escape"},
			}},
			Score: MemoryScore{Total: .75, Value: .75, Lexical: .5},
		}
	}
	service := &captureMemorySearchService{result: MemorySearchResult{
		Hits:  hits,
		Total: len(hits),
		Diagnostics: MemoryRetrievalDiagnostics{
			Candidates: len(hits),
			Returned:   len(hits),
			Semantic:   true,
		},
	}}
	tool := MemorySearchTool{Service: service, Actor: testMemoryParent(), Policy: DefaultMemoryPolicy()}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"memory","limit":99,"max_bytes":999999}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(output) > maxMemorySearchToolBytes {
		t.Fatalf("compact output exceeded bound: %d", len(output))
	}
	if strings.Contains(output, `"content"`) || strings.Contains(output, `"provenance"`) || strings.Contains(output, "should-not-escape") {
		t.Fatalf("full record fields leaked: %s", output)
	}
	var view struct {
		Hits []struct {
			ID        string          `json:"id"`
			Namespace MemoryNamespace `json:"namespace"`
			Key       string          `json:"key"`
			Summary   string          `json:"summary"`
			Excerpt   string          `json:"excerpt"`
			Score     MemoryScore     `json:"score"`
			Source    string          `json:"source"`
		} `json:"hits"`
		Total         int  `json:"total"`
		MoreAvailable bool `json:"more_available"`
		Diagnostics   struct {
			Semantic bool `json:"semantic"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(output), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Hits) == 0 || len(view.Hits) > maxMemorySearchToolHits {
		t.Fatalf("unexpected projected hit count: %d", len(view.Hits))
	}
	if !view.MoreAvailable || view.Total != len(hits) || !view.Diagnostics.Semantic {
		t.Fatalf("projection metadata=%+v", view)
	}
	for _, hit := range view.Hits {
		if len(hit.Excerpt) > maxMemorySearchToolExcerpt {
			t.Fatalf("excerpt exceeded bound: %d", len(hit.Excerpt))
		}
		if hit.ID == "" || hit.Namespace == "" || hit.Key == "" || hit.Summary != "summary" || hit.Source != "source" || hit.Score.Total != .75 {
			t.Fatalf("incomplete projected hit=%+v", hit)
		}
	}
}

func TestMemorySearchToolPreservesSemanticOptionsAndCapsLimits(t *testing.T) {
	embedder := captureMemoryEmbedder{}
	index := captureMemoryVectorIndex{}
	service := &captureMemorySearchService{result: MemorySearchResult{Total: 1}}
	tool := MemorySearchTool{
		Service: service,
		Actor:   testMemoryParent(),
		Policy:  DefaultMemoryPolicy(),
		Retrieval: MemoryRetrievalOptions{
			SemanticEnabled: true,
			Embedder:        embedder,
			VectorIndex:     index,
			SemanticTimeout: 3 * time.Second,
		},
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"semantic","limit":999,"max_bytes":999999}`)); err != nil {
		t.Fatal(err)
	}
	if !service.request.Options.SemanticEnabled || service.request.Options.Embedder != embedder || service.request.Options.VectorIndex != index || service.request.Options.SemanticTimeout != 3*time.Second {
		t.Fatalf("semantic retrieval options were not forwarded: %+v", service.request.Options)
	}
	if service.request.Query.Limit != maxMemorySearchToolHits || service.request.Options.MaxResults != maxMemorySearchToolHits || service.request.Bounds.MaxRecords != maxMemorySearchToolHits {
		t.Fatalf("limits were not safely capped: query=%+v options=%+v bounds=%+v", service.request.Query, service.request.Options, service.request.Bounds)
	}
	if service.request.Query.MaxBytes > maxMemorySearchToolBytes || service.request.Bounds.MaxBytes > maxMemorySearchToolBytes {
		t.Fatalf("byte limits were not safely capped: query=%+v bounds=%+v", service.request.Query, service.request.Bounds)
	}
	if !service.request.Query.CurrentOnly {
		t.Fatal("model-facing search did not default to current-only memory")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"semantic","current_only":false}`)); err != nil {
		t.Fatal(err)
	}
	if service.request.Query.CurrentOnly {
		t.Fatal("explicit historical search opt-in was ignored")
	}
}

func TestMemorySearchToolDoesNotChangeRichServiceSearch(t *testing.T) {
	root := privateTempDir(t)
	service, err := NewLocalMemoryService(root, DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := MemoryRecord{
		ID:          "rich-memory",
		Namespace:   MemoryNamespaceDurableMemory,
		Key:         "rich",
		Content:     "full record body",
		Summary:     "rich summary",
		Source:      "imported",
		Sensitivity: MemorySensitivityPublic,
		Confidence:  .9,
		CreatedAt:   now,
		UpdatedAt:   now,
		Provenance: &MemoryProvenance{
			InventoryID:   "inventory-1",
			CandidateID:   "candidate-1",
			DecisionID:    "decision-1",
			RootLabel:     "root",
			RelativePath:  "notes/memory.md",
			FileSHA256:    "file-sha",
			ContentSHA256: "content-sha",
			LineStart:     1,
			LineEnd:       1,
		},
	}
	actor := testMemoryParent()
	if _, err := service.Remember(context.Background(), RememberRequest{Actor: actor, Record: record, AuditID: "remember-rich", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	rich, err := service.Search(context.Background(), SearchMemoryRequest{Query: MemoryQuery{Actor: actor, Text: "full record"}, Bounds: MemoryDisplayBounds{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rich.Hits) != 1 || rich.Hits[0].Record.Record.Content != record.Content || rich.Hits[0].Record.Record.Provenance == nil {
		t.Fatalf("rich service search changed: %+v", rich)
	}
	compact, err := (MemorySearchTool{Service: service, Actor: actor, Policy: DefaultMemoryPolicy()}).Execute(context.Background(), json.RawMessage(`{"query":"full record"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compact, `"content"`) || strings.Contains(compact, `"provenance"`) || strings.Contains(compact, "inventory-1") {
		t.Fatalf("compact tool output exposed rich fields: %s", compact)
	}
	if !strings.Contains(compact, `"excerpt":"full record body"`) || !strings.Contains(compact, `"summary":"rich summary"`) || !strings.Contains(compact, `"source":"imported"`) {
		t.Fatalf("compact fields missing: %s", compact)
	}
}

func TestMemoryRememberToolBuildsBoundedParentOwnedExplicitRequest(t *testing.T) {
	service := &captureMemoryRememberService{}
	tool := MemoryRememberTool{Service: service, Actor: testMemoryParent()}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"preference.theme","content":"Prefer concise answers","summary":"Response style","reason":"The user stated this preference"}`))
	if err != nil {
		t.Fatal(err)
	}
	if service.searchCalls != 1 || service.rememberCalls != 1 {
		t.Fatalf("calls search=%d remember=%d", service.searchCalls, service.rememberCalls)
	}
	request := service.rememberRequest
	if request.Actor != testMemoryParent() || request.Mode != MemoryWriteExplicit || request.Reason != "The user stated this preference" {
		t.Fatalf("remember request ownership/mode/reason=%+v", request)
	}
	if request.Record.Namespace != MemoryNamespaceDurableMemory || request.Record.Key != "preference.theme" || request.Record.Content != "Prefer concise answers" || request.Record.Summary != "Response style" || request.Record.Sensitivity != MemorySensitivityPublic || request.Record.Status != MemoryStatusActive || request.Record.Confidence != 1 {
		t.Fatalf("remember record=%+v", request.Record)
	}
	if request.Record.ID != memoryRememberRecordID(MemoryNamespaceDurableMemory, request.Record.Key) || len(request.AuditID) > MaxMemoryAuditIDLength || request.AuditID == "" {
		t.Fatalf("generated identity=%q audit=%q", request.Record.ID, request.AuditID)
	}
	if strings.Contains(output, "Prefer concise answers") || strings.Contains(output, "The user stated") {
		t.Fatalf("output echoed full input: %s", output)
	}
	var result memoryRememberToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved || result.AlreadyPresent || result.ID != request.Record.ID || result.Key != request.Record.Key || result.Namespace != MemoryNamespaceDurableMemory || result.Version != 1 {
		t.Fatalf("ack=%+v", result)
	}
}

func TestMemoryRememberToolRejectsStrictOrUnsafeArguments(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{name: "unknown actor", args: `{"key":"k","content":"c","reason":"r","actor":{"kind":"parent","id":"other"}}`},
		{name: "unknown namespace", args: `{"key":"k","content":"c","reason":"r","namespace":"durable_memory/other"}`},
		{name: "missing reason", args: `{"key":"k","content":"c"}`},
		{name: "unsafe key", args: `{"key":"bad key","content":"c","reason":"r"}`},
		{name: "secret content", args: `{"key":"k","content":"api_key=secret","reason":"r"}`},
		{name: "nul content", args: "{\"key\":\"k\",\"content\":\"c\\u0000\",\"reason\":\"r\"}"},
		{name: "empty content", args: `{"key":"k","content":" ","reason":"r"}`},
		{name: "oversized input", args: `{"key":"k","content":"` + strings.Repeat("x", maxMemoryRememberToolInputBytes) + `","reason":"r"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service := &captureMemoryRememberService{}
			tool := MemoryRememberTool{Service: service, Actor: testMemoryParent()}
			if _, err := tool.Execute(context.Background(), json.RawMessage(test.args)); err == nil {
				t.Fatal("expected validation error")
			}
			if service.rememberCalls != 0 {
				t.Fatalf("invalid input reached Remember: %d", service.rememberCalls)
			}
		})
	}
}

func TestMemoryRememberToolRejectsNonParentCompositionActor(t *testing.T) {
	service := &captureMemoryRememberService{}
	tool := MemoryRememberTool{Service: service, Actor: testMemoryChild()}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"k","content":"c","reason":"r"}`)); err == nil || !strings.Contains(err.Error(), "parent actor") {
		t.Fatalf("error=%v", err)
	}
	if service.searchCalls != 0 || service.rememberCalls != 0 {
		t.Fatalf("non-parent actor reached service: search=%d remember=%d", service.searchCalls, service.rememberCalls)
	}
}

func TestMemoryRememberToolCreateOnlyDuplicateDoesNotOverwrite(t *testing.T) {
	service := &captureMemoryRememberService{searchResult: MemorySearchResult{Total: 1, Hits: []MemoryDisplayHit{{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "existing", Namespace: MemoryNamespaceDurableMemory, Key: "k"}}}}}}
	tool := MemoryRememberTool{Service: service, Actor: testMemoryParent()}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"k","content":"new","reason":"r"}`)); err == nil || !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("error=%v", err)
	}
	if service.rememberCalls != 0 {
		t.Fatalf("duplicate reached Remember: %d", service.rememberCalls)
	}
}

func TestMemoryRememberToolPersistsThroughLocalMemoryService(t *testing.T) {
	service, err := NewLocalMemoryService(privateTempDir(t), DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	tool := MemoryRememberTool{Service: service, Actor: testMemoryParent()}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"durable.preference","content":"Concise responses","reason":"The user requested this"}`)); err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), SearchMemoryRequest{Query: MemoryQuery{Actor: testMemoryParent(), Namespace: MemoryNamespaceDurableMemory, Key: "durable.preference", CurrentOnly: true, Limit: 1}, Bounds: MemoryDisplayBounds{MaxRecords: 1, MaxBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Record.Record.Content != "Concise responses" || result.Hits[0].Record.Record.CreatedBy != testMemoryParent() || result.Hits[0].Record.Record.Sensitivity != MemorySensitivityPublic || result.Hits[0].Record.Record.Status != MemoryStatusActive {
		t.Fatalf("persisted result=%+v", result)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"key":"durable.preference","content":"Overwrite","reason":"retry"}`)); err == nil || !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestMemoryRememberToolDefinitionAndGeneralRegistry(t *testing.T) {
	definition := (MemoryRememberTool{}).Definition()
	if definition.Mutability != ToolMutabilityMutating || definition.Risk != RiskMedium || !strings.Contains(string(definition.Parameters), `"reason"`) {
		t.Fatalf("definition=%+v", definition)
	}
	registry, err := NewGeneralAssistantRegistry(ExecutionScope{Root: t.TempDir()}, &captureMemoryRememberService{}, testMemoryParent(), DefaultMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup("memory_remember"); !ok {
		t.Fatal("general registry missing memory_remember")
	}
	for mode, want := range map[AssistantPolicyMode]PolicyDecision{
		AssistantPolicyTrusted:  PolicyAllow,
		AssistantPolicyPrompted: PolicyRequireApproval,
		AssistantPolicyLocked:   PolicyDeny,
	} {
		decision, err := (GeneralAssistantPolicy{Mode: mode}).Evaluate(context.Background(), definition, json.RawMessage(`{"key":"k","content":"c","reason":"r"}`), ExecutionScope{})
		if err != nil || decision != want {
			t.Fatalf("mode=%s decision=%v err=%v want=%v", mode, decision, err, want)
		}
	}
}
