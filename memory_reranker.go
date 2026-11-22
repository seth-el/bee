package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MemoryRerankCandidate is parent-filtered, transient memory data. The
// reranker cannot retrieve, write, delete, or widen this set.
type MemoryRerankCandidate struct {
	ID      string `json:"id"`
	Excerpt string `json:"excerpt"`
}

type MemoryRerankDecision struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type MemoryRerankResult struct {
	Ordered []MemoryRerankDecision `json:"ordered"`
	Enabled bool                   `json:"enabled"`
}

type MemoryReranker struct {
	Enabled         bool
	Profile         ProviderRole
	ProviderProfile string
	Specialist      *SpecialistService
	MaxCandidates   int
	MaxExcerptBytes int
}

// Rerank invokes the optional local specialist only when explicitly enabled.
// Ordinary turns remain unchanged because the zero value is disabled.
func (reranker MemoryReranker) Rerank(ctx context.Context, query string, candidates []MemoryRerankCandidate) (MemoryRerankResult, error) {
	if !reranker.Enabled {
		return MemoryRerankResult{Ordered: []MemoryRerankDecision{}, Enabled: false}, nil
	}
	if reranker.Specialist == nil {
		return MemoryRerankResult{}, errors.New("memory reranker specialist is unavailable")
	}
	if err := reranker.Specialist.requireConfiguredLocalChild("memory reranking"); err != nil {
		return MemoryRerankResult{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2048 {
		return MemoryRerankResult{}, errors.New("memory rerank query is empty or exceeds bounds")
	}
	if evidenceSecretLike.MatchString(query) || looksLikeSecret(query) {
		return MemoryRerankResult{}, errors.New("memory rerank query contains secret-like output")
	}
	maxCandidates := reranker.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = MaxMemoryAutomationCandidates
	}
	if len(candidates) > maxCandidates {
		return MemoryRerankResult{}, errors.New("memory rerank candidate set exceeds bounds")
	}
	maxExcerpt := reranker.MaxExcerptBytes
	if maxExcerpt <= 0 {
		maxExcerpt = DefaultMemoryRerankConfig().MaxExcerptBytes
	}
	seen := make(map[string]struct{}, len(candidates))
	for index := range candidates {
		candidate := &candidates[index]
		candidate.ID = strings.TrimSpace(candidate.ID)
		candidate.Excerpt = strings.TrimSpace(candidate.Excerpt)
		if candidate.Excerpt == "" || len(candidate.Excerpt) > maxExcerpt {
			return MemoryRerankResult{}, errors.New("memory rerank candidate is invalid")
		}
		if err := validateOpaqueMemoryID(candidate.ID, 256); err != nil {
			return MemoryRerankResult{}, err
		}
		if _, ok := seen[candidate.ID]; ok {
			return MemoryRerankResult{}, fmt.Errorf("memory rerank duplicate id %q", candidate.ID)
		}
		seen[candidate.ID] = struct{}{}
		if err := validateSensitiveEvidenceTokens(candidate.Excerpt, "", false); err != nil {
			return MemoryRerankResult{}, fmt.Errorf("memory rerank excerpt: %w", err)
		}
	}
	payload, err := json.Marshal(struct {
		Query      string                  `json:"query"`
		Candidates []MemoryRerankCandidate `json:"candidates"`
	}{Query: query, Candidates: candidates})
	if err != nil {
		return MemoryRerankResult{}, fmt.Errorf("encode memory rerank input: %w", err)
	}
	system := `You are Bee's optional local memory reranker. Return ONLY one JSON object matching {"ordered":[{"id":"existing candidate id","reason":"short bounded reason"}]}. Order every existing candidate exactly once; do not add, remove, retrieve, write, delete, or invent IDs. Use only the supplied query and sanitized excerpts. Never use tools, browse, access memory stores, expose secrets, or grant authority.`
	if maxInput := reranker.Specialist.Config().MaxInputBytes; maxInput > 0 && len(system)+len(payload) > maxInput {
		return MemoryRerankResult{}, fmt.Errorf("memory rerank input exceeds specialist request bound %d", maxInput)
	}
	content, err := reranker.Specialist.callWithPriority(ctx, LocalChildPurposeMemoryRerank, LocalChildPriorityBackground, system, []Message{{Role: RoleUser, Content: string(payload)}}, reranker.Specialist.Config().EvidenceTimeoutSeconds)
	if err != nil {
		return MemoryRerankResult{}, err
	}
	ordered, err := parseMemoryRerankResponse(content, seen)
	if err != nil {
		return MemoryRerankResult{}, err
	}
	return MemoryRerankResult{Ordered: ordered, Enabled: true}, nil
}

func parseMemoryRerankResponse(content string, known map[string]struct{}) ([]MemoryRerankDecision, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 8<<10 {
		return nil, errors.New("memory rerank response is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(content)))
	decoder.DisallowUnknownFields()
	var envelope struct {
		Ordered []MemoryRerankDecision `json:"ordered"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("parse memory rerank JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if envelope.Ordered == nil || len(envelope.Ordered) != len(known) {
		return nil, errors.New("memory rerank response must contain every existing candidate exactly once")
	}
	seen := make(map[string]struct{}, len(envelope.Ordered))
	for _, decision := range envelope.Ordered {
		if _, ok := known[decision.ID]; !ok {
			return nil, fmt.Errorf("memory rerank returned unknown id %q", decision.ID)
		}
		if _, ok := seen[decision.ID]; ok {
			return nil, fmt.Errorf("memory rerank returned duplicate id %q", decision.ID)
		}
		seen[decision.ID] = struct{}{}
		if strings.TrimSpace(decision.Reason) == "" || len(decision.Reason) > 256 {
			return nil, errors.New("memory rerank reason is invalid, ungrounded, or secret-like")
		}
		if err := validateSensitiveEvidenceTokens(decision.Reason, "", false); err != nil {
			return nil, errors.New("memory rerank reason is invalid, ungrounded, or secret-like")
		}
	}
	return envelope.Ordered, nil
}

// DeterministicMemoryRerankBaseline is retained for offline comparison only;
// it is not used by the enabled reranker path.
func DeterministicMemoryRerankBaseline(query string, candidates []MemoryRerankCandidate) []MemoryRerankDecision {
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	result := make([]MemoryRerankDecision, 0, len(candidates))
	for _, candidate := range candidates {
		score := 0
		lower := strings.ToLower(candidate.Excerpt)
		for _, token := range tokens {
			if strings.Contains(lower, token) {
				score++
			}
		}
		reason := "no query overlap; retained as parent-approved candidate"
		if score > 0 {
			reason = "bounded lexical baseline overlap"
		}
		result = append(result, MemoryRerankDecision{ID: candidate.ID, Reason: reason})
	}
	return result
}

// RerankMemorySearchResult applies the optional local reranker only to the
// already-authorized, bounded result set. Failure preserves deterministic
// retrieval order and is surfaced as degraded diagnostics.
func RerankMemorySearchResult(ctx context.Context, reranker *MemoryReranker, query string, result MemorySearchResult) MemorySearchResult {
	if reranker == nil || !reranker.Enabled || len(result.Hits) < 2 {
		return result
	}
	limit := reranker.MaxCandidates
	if limit <= 0 {
		limit = DefaultMemoryRerankConfig().MaxCandidates
	}
	if limit > len(result.Hits) {
		limit = len(result.Hits)
	}
	maxExcerpt := reranker.MaxExcerptBytes
	if maxExcerpt <= 0 {
		maxExcerpt = DefaultMemoryRerankConfig().MaxExcerptBytes
	}
	candidates := make([]MemoryRerankCandidate, 0, limit)
	byID := make(map[string]MemoryDisplayHit, limit)
	for _, hit := range result.Hits[:limit] {
		record := hit.Record.Record
		excerpt := strings.TrimSpace(record.Summary + "\n" + record.Content)
		if excerpt == "" {
			excerpt = record.Key
		}
		excerpt = truncateText(excerpt, maxExcerpt)
		candidates = append(candidates, MemoryRerankCandidate{ID: record.ID, Excerpt: excerpt})
		byID[record.ID] = hit
	}
	reranked, err := reranker.Rerank(ctx, query, candidates)
	if err != nil || !reranked.Enabled {
		result.Diagnostics.Degraded = true
		result.Diagnostics.DegradedReason = "memory reranker unavailable; deterministic ranking retained"
		return result
	}
	ordered := make([]MemoryDisplayHit, 0, len(result.Hits))
	for _, decision := range reranked.Ordered {
		ordered = append(ordered, byID[decision.ID])
	}
	ordered = append(ordered, result.Hits[limit:]...)
	result.Hits = ordered
	return result
}
