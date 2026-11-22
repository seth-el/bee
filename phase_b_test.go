package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func newDelegateReconTestService(t *testing.T, root string, provider *specialistTestProvider) *DelegateReconService {
	t.Helper()
	specialist, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", SpecialistConfig{EvidenceEnabled: true, MaxInputBytes: 16 << 10, MaxOutputBytes: 8 << 10, MaxTokens: 512, EvidenceMaxInputBytes: 16 << 10, EvidenceMaxOutputBytes: 8 << 10, EvidenceMaxItems: 16, EvidenceTimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewDelegateReconService(&Application{Specialist: specialist}, ExecutionScope{Root: root, TimeoutSeconds: MaxDelegateReconTimeoutSeconds, MaxOutputBytes: MaxDelegateReconSourceBytes})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestDelegateReconEnforcesScopeAndUsesOneToolFreeEvidenceCall(t *testing.T) {
	root := t.TempDir()
	line := `child endpoint is http://127.0.0.1:8082`
	if err := os.WriteFile(root+"/provider_profiles.go", []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	anchor := "provider_profiles.go:1:" + line
	provider := &specialistTestProvider{content: `{"items":[{"claim":"child endpoint uses 8082","anchor":"` + anchor + `","category":"fact","confidence":1}],"uncertainties":[],"errors":[],"suggested_verification_targets":[]}`}
	service := newDelegateReconTestService(t, root, provider)
	if _, err := service.Execute(context.Background(), DelegateReconRequest{Objective: "find endpoint", Scope: "../escape", Queries: []DelegateReconQuery{{Query: "8082"}}}); err == nil {
		t.Fatal("scope escape accepted")
	}
	result, err := service.Execute(context.Background(), DelegateReconRequest{Objective: "find endpoint", Queries: []DelegateReconQuery{{Query: "8082"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AdvisoryOnly || len(result.Evidence.Items) != 1 || !strings.Contains(result.SourcePreview, anchor) {
		t.Fatalf("result=%#v", result)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 || len(provider.requests[0].Tools) != 0 {
		t.Fatalf("requests=%#v", provider.requests)
	}
}

func TestDelegateReconRejectsSymlinkOutsideScope(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := root + "/outside"
	if err := symlinkForTest(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	service := newDelegateReconTestService(t, root, &specialistTestProvider{content: `{"items":[],"uncertainties":[],"errors":[],"suggested_verification_targets":[]}`})
	if _, err := service.Execute(context.Background(), DelegateReconRequest{Objective: "x", Scope: "outside", Queries: []DelegateReconQuery{{Query: "x"}}}); err == nil {
		t.Fatal("outside symlink accepted")
	}
}

func TestDelegateReconCancellationPropagates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/file.txt", []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newDelegateReconTestService(t, root, &specialistTestProvider{content: `{}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Execute(ctx, DelegateReconRequest{Objective: "x", Queries: []DelegateReconQuery{{Query: "needle"}}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func symlinkForTest(target, link string) error { return os.Symlink(target, link) }

func TestVerifyClaimsUsesOneToolFreeHighPriorityChildCall(t *testing.T) {
	provider := &specialistTestProvider{content: `{"supported_claims":["service on port 8082"],"unsupported_claims":[],"contradictions":[],"stale_identifiers_paths_endpoints":[],"required_deterministic_checks":[],"advisory_only":true}`}
	service, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", SpecialistConfig{MaxInputBytes: 16 << 10, MaxOutputBytes: 8 << 10, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 4})
	if err != nil {
		t.Fatal(err)
	}
	service.SetCapacityBroker(broker)
	verification := NewClaimVerificationService(service)
	input := VerifyClaimsRequest{Draft: "service on port 8082", SourceText: "service on port 8082", Evidence: []EvidenceItem{{Claim: "service on port 8082", Anchor: "service on port 8082", Category: "fact", Confidence: .9}}}
	result, err := verification.Verify(context.Background(), input)
	if err != nil || !result.AdvisoryOnly || len(result.SupportedClaims) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 || len(provider.requests[0].Tools) != 0 {
		t.Fatalf("requests=%#v", provider.requests)
	}
	snapshot := broker.Snapshot()
	if snapshot.Acquisitions != 1 || snapshot.Completed != 1 || snapshot.ByPurpose[LocalChildPurposeClaimVerification] != 0 || snapshot.ByPriority[LocalChildPriorityHigh] != 1 {
		t.Fatalf("broker snapshot=%#v", snapshot)
	}
}

func TestVerifyClaimsRejectsInvalidAnchorBeforeProviderCall(t *testing.T) {
	provider := &specialistTestProvider{content: `{}`}
	service, err := NewSpecialistServiceWithProvider(provider, "local", SpecialistConfig{})
	if err != nil {
		t.Fatal(err)
	}
	verification := NewClaimVerificationService(service)
	_, err = verification.Verify(context.Background(), VerifyClaimsRequest{Draft: "draft", SourceText: "source", Evidence: []EvidenceItem{{Claim: "draft", Anchor: "missing", Category: "fact", Confidence: 1}}})
	if err == nil {
		t.Fatal("invalid anchor accepted")
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 0 {
		t.Fatal("provider called for invalid anchor")
	}
}

func TestVerifyClaimsToolStrictArguments(t *testing.T) {
	service := NewClaimVerificationService()
	tool := VerifyClaimsTool{Service: service}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"draft":"x","source_text":"x","evidence":[{"claim":"x","anchor":"missing","category":"fact","confidence":1}]}`)); err == nil {
		t.Fatal("missing anchor accepted")
	}
}

func TestVerifyClaimsRequiresAnchoredFindingsAndNonNullArrays(t *testing.T) {
	provider := &specialistTestProvider{content: `{"supported_claims":[],"unsupported_claims":[],"contradictions":[{"claim":"service is stale","anchor":"service is stale"}],"stale_identifiers_paths_endpoints":[],"required_deterministic_checks":[],"advisory_only":true}`}
	service, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", SpecialistConfig{MaxInputBytes: 16 << 10, MaxOutputBytes: 8 << 10, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewClaimVerificationService(service).Verify(context.Background(), VerifyClaimsRequest{Draft: "service is stale", SourceText: "service is stale", Evidence: []EvidenceItem{}})
	if err != nil || len(result.Contradictions) != 1 || result.Contradictions[0].Anchor != "service is stale" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	provider.content = `{"supported_claims":[],"unsupported_claims":[],"contradictions":null,"stale_identifiers_paths_endpoints":[],"required_deterministic_checks":[],"advisory_only":true}`
	if _, err := NewClaimVerificationService(service).Verify(context.Background(), VerifyClaimsRequest{Draft: "service is stale", SourceText: "service is stale", Evidence: []EvidenceItem{}}); err == nil {
		t.Fatal("null contradiction array accepted")
	}
}

func TestMemoryRerankerOfflineDefaultAndSpecialistPath(t *testing.T) {
	r := MemoryReranker{}
	result, err := r.Rerank(context.Background(), "alpha", []MemoryRerankCandidate{{ID: "a", Excerpt: "alpha"}})
	if err != nil || result.Enabled || len(result.Ordered) != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	provider := &specialistTestProvider{content: `{"ordered":[{"id":"b","reason":"second"},{"id":"a","reason":"best match"}]}`}
	service, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", SpecialistConfig{MaxInputBytes: 16 << 10, MaxOutputBytes: 8 << 10, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	service.SetCapacityBroker(broker)
	r = MemoryReranker{Enabled: true, Specialist: service}
	result, err = r.Rerank(context.Background(), "alpha", []MemoryRerankCandidate{{ID: "a", Excerpt: "alpha"}, {ID: "b", Excerpt: "beta"}})
	if err != nil || len(result.Ordered) != 2 || result.Ordered[0].ID != "b" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 || len(provider.requests[0].Tools) != 0 || strings.Contains(string(mustJSONPhaseB(provider.requests[0])), "memory_store") {
		t.Fatalf("unsafe reranker request=%#v", provider.requests)
	}
	if snapshot := broker.Snapshot(); snapshot.Acquisitions != 1 || snapshot.ByPurpose[LocalChildPurposeMemoryRerank] != 0 || snapshot.ByPriority[LocalChildPriorityBackground] != 1 {
		t.Fatalf("broker snapshot=%#v", snapshot)
	}
}

func TestMemoryRerankerReordersAuthorizedSearchResults(t *testing.T) {
	provider := &specialistTestProvider{content: `{"ordered":[{"id":"b","reason":"closer"},{"id":"a","reason":"secondary"}]}`}
	service, err := newConfiguredChildSpecialistService(provider, "local", "test-child", "http://127.0.0.1:8082", SpecialistConfig{MaxInputBytes: 16 << 10, MaxOutputBytes: 8 << 10, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	reranker := &MemoryReranker{Enabled: true, Specialist: service, MaxCandidates: 2, MaxExcerptBytes: 256}
	result := MemorySearchResult{Hits: []MemoryDisplayHit{
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "a", Key: "a", Content: "alpha"}}},
		{Record: MemoryDisplayRecord{Record: MemoryRecord{ID: "b", Key: "b", Content: "beta"}}},
	}}
	result = RerankMemorySearchResult(context.Background(), reranker, "beta", result)
	if len(result.Hits) != 2 || result.Hits[0].Record.Record.ID != "b" || result.Diagnostics.Degraded {
		t.Fatalf("result=%#v", result)
	}
}

func TestPhaseBOffloadsFailClosedWithoutConfiguredChildProvenance(t *testing.T) {
	provider := &specialistTestProvider{content: `{}`}
	service, err := NewSpecialistServiceWithProvider(provider, "local", DefaultSpecialistConfig())
	if err != nil {
		t.Fatal(err)
	}
	config := service.Config()
	config.EvidenceEnabled = true
	service.config = config
	if _, err := NewEvidenceExtractor(service); err == nil {
		t.Fatal("evidence extractor accepted unconfigured specialist")
	}
	if _, err := NewClaimVerificationService(service).Verify(context.Background(), VerifyClaimsRequest{Draft: "claim", SourceText: "claim"}); err == nil {
		t.Fatal("claim verification accepted unconfigured specialist")
	}
	if _, err := (MemoryReranker{Enabled: true, Specialist: service}).Rerank(context.Background(), "claim", []MemoryRerankCandidate{{ID: "candidate", Excerpt: "claim"}}); err == nil {
		t.Fatal("memory reranker accepted unconfigured specialist")
	}
}

func mustJSONPhaseB(value any) []byte { data, _ := json.Marshal(value); return data }
