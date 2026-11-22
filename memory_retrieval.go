package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	MaxMemoryQueryTextLength     = MaxMemorySummaryLength
	MaxMemoryExplanationLength   = 2 << 10
	MaxMemoryReasonLength        = 2 << 10
	MaxMemoryEmbeddingDimensions = 4096
	MaxMemorySemanticHits        = 1024
)

// MemoryScore is a bounded, provider-neutral score breakdown. Every component
// and Total is finite and in [0,1]. Value mirrors Total for callers that want
// a scalar score without knowing the breakdown.
type MemoryScore struct {
	Total      float64 `json:"total"`
	Value      float64 `json:"value,omitempty"`
	Lexical    float64 `json:"lexical,omitempty"`
	Phrase     float64 `json:"phrase,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Recency    float64 `json:"recency,omitempty"`
	Semantic   float64 `json:"semantic,omitempty"`
}

func (score MemoryScore) Validate() error {
	components := []struct {
		name  string
		value float64
	}{
		{"total", score.Total}, {"value", score.Value}, {"lexical", score.Lexical},
		{"phrase", score.Phrase}, {"confidence", score.Confidence},
		{"recency", score.Recency}, {"semantic", score.Semantic},
	}
	for _, component := range components {
		if err := validateBoundedScore(component.name, component.value); err != nil {
			return err
		}
	}
	if score.Value != score.Total {
		return errors.New("memory score value must equal total")
	}
	return nil
}

func validateBoundedScore(label string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return fmt.Errorf("memory score %s must be a finite number between 0 and 1", label)
	}
	return nil
}

// RankedMemoryHit is a validated durable record plus a transient ranking
// explanation. The explanation and reason are bounded and are never persisted
// as part of the durable MemoryRecord.
type RankedMemoryHit struct {
	Record      MemoryRecord `json:"record"`
	Score       MemoryScore  `json:"score"`
	Explanation string       `json:"explanation,omitempty"`
	Reason      string       `json:"reason,omitempty"`
}

func (hit RankedMemoryHit) Validate(policy MemoryPolicy, bounds MemoryRetrievalOptions) error {
	if err := hit.Record.Validate(policy); err != nil {
		return fmt.Errorf("ranked memory hit record: %w", err)
	}
	if err := hit.Score.Validate(); err != nil {
		return err
	}
	if len(hit.Explanation) > bounds.maxExplanation(policy) {
		return fmt.Errorf("ranked memory explanation exceeds %d bytes", bounds.maxExplanation(policy))
	}
	if len(hit.Reason) > bounds.maxReason(policy) {
		return fmt.Errorf("ranked memory reason exceeds %d bytes", bounds.maxReason(policy))
	}
	return nil
}

// MemoryRetrievalDiagnostics describes bounded retrieval work and degraded
// optional-provider state. It is descriptive only and contains no memory text.
type MemoryRetrievalDiagnostics struct {
	Candidates       int    `json:"candidates"`
	Filtered         int    `json:"filtered"`
	Returned         int    `json:"returned"`
	Semantic         bool   `json:"semantic"`
	Degraded         bool   `json:"degraded"`
	DegradedReason   string `json:"degraded_reason,omitempty"`
	SemanticHealth   string `json:"semantic_health,omitempty"`
	DegradedCode     string `json:"degraded_code,omitempty"`
	DiversityLimited bool   `json:"diversity_limited,omitempty"`
}

func (diagnostics MemoryRetrievalDiagnostics) Validate() error {
	if diagnostics.Candidates < 0 || diagnostics.Filtered < 0 || diagnostics.Returned < 0 {
		return errors.New("memory retrieval diagnostics counts must not be negative")
	}
	if diagnostics.Returned > diagnostics.Candidates {
		return errors.New("memory retrieval diagnostics returned exceeds candidates")
	}
	if len(diagnostics.DegradedReason) > MaxMemoryReasonLength {
		return fmt.Errorf("memory retrieval degraded reason exceeds %d bytes", MaxMemoryReasonLength)
	}
	if !SemanticHealthCode(diagnostics.SemanticHealth).valid() {
		return fmt.Errorf("memory retrieval semantic health code %q is invalid", diagnostics.SemanticHealth)
	}
	if !SemanticHealthCode(diagnostics.DegradedCode).valid() {
		return fmt.Errorf("memory retrieval degraded code %q is invalid", diagnostics.DegradedCode)
	}
	return nil
}

// RankedMemoryResult is the scored counterpart to MemoryRetrievalResult. Hits
// remain durable records wrapped in a transient result; no result is written to
// the store.
type RankedMemoryResult struct {
	Hits        []RankedMemoryHit          `json:"hits,omitempty"`
	Total       int                        `json:"total"`
	Bytes       int                        `json:"bytes"`
	Truncated   bool                       `json:"truncated,omitempty"`
	Diagnostics MemoryRetrievalDiagnostics `json:"diagnostics"`
}

func (result RankedMemoryResult) Validate(policy MemoryPolicy, bounds MemoryRetrievalOptions) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if err := bounds.validate(policy, MemoryQuery{}); err != nil {
		return err
	}
	if result.Total < 0 || result.Total < len(result.Hits) {
		return errors.New("ranked memory result total is inconsistent")
	}
	if result.Bytes < 0 || result.Bytes > bounds.maxBytes(policy) {
		return fmt.Errorf("ranked memory result bytes must be between 0 and %d", bounds.maxBytes(policy))
	}
	if len(result.Hits) > bounds.maxResults(policy) {
		return fmt.Errorf("ranked memory result exceeds %d hits", bounds.maxResults(policy))
	}
	actualBytes := 0
	for index, hit := range result.Hits {
		if err := hit.Validate(policy, bounds); err != nil {
			return fmt.Errorf("ranked memory hit %d: %w", index, err)
		}
		encoded, err := json.Marshal(hit)
		if err != nil {
			return fmt.Errorf("encode ranked memory hit %d: %w", index, err)
		}
		actualBytes += len(encoded)
	}
	if actualBytes > bounds.maxBytes(policy) {
		return fmt.Errorf("ranked memory hits exceed %d bytes", bounds.maxBytes(policy))
	}
	if result.Diagnostics.Returned != len(result.Hits) {
		return errors.New("ranked memory diagnostics returned is inconsistent")
	}
	return result.Diagnostics.Validate()
}

// MemoryRetrievalOptions contains exact per-request bounds. Zero values use
// the corresponding policy/query bound. The alias fields make the contract
// convenient for callers that prefer candidate/result terminology.
type MemoryRetrievalOptions struct {
	MaxCandidates       int
	CandidateLimit      int
	MaxResults          int
	ResultLimit         int
	MaxBytes            int
	MaxQueryTextBytes   int
	MaxQueryText        int
	MaxExplanationBytes int
	MaxReasonBytes      int
	MaxPerNamespace     int
	Diversity           bool
	Embedder            MemoryEmbedder
	VectorIndex         MemoryVectorIndex
	SemanticTimeout     time.Duration
	AllowMemoryText     bool
	SemanticEnabled     bool
	SemanticHealth      SemanticHealthCode
}

// MemoryRetrievalBounds is a descriptive compatibility alias.
type MemoryRetrievalBounds = MemoryRetrievalOptions

// Validate checks per-request retrieval bounds against the supplied policy.
// LocalMemoryStore calls this after applying query-level defaults.
func (options MemoryRetrievalOptions) Validate(policy MemoryPolicy, query MemoryQuery) error {
	return options.validate(policy, query)
}

func (options MemoryRetrievalOptions) validate(policy MemoryPolicy, query MemoryQuery) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	candidateLimit := options.maxCandidates(policy)
	resultLimit := options.maxResults(policy)
	maxBytes := options.maxBytes(policy)
	queryTextLimit := options.maxQueryText()
	if candidateLimit <= 0 || candidateLimit > policy.MaxRecords || candidateLimit > MaxMemoryRecords {
		return fmt.Errorf("memory retrieval max candidates must be between 1 and %d", minInt(policy.MaxRecords, MaxMemoryRecords))
	}
	if resultLimit <= 0 || resultLimit > policy.MaxQueryRecords {
		return fmt.Errorf("memory retrieval max results must be between 1 and %d", policy.MaxQueryRecords)
	}
	if maxBytes <= 0 || maxBytes > policy.MaxQueryBytes {
		return fmt.Errorf("memory retrieval max bytes must be between 1 and %d", policy.MaxQueryBytes)
	}
	if queryTextLimit <= 0 || queryTextLimit > MaxMemoryQueryTextLength {
		return fmt.Errorf("memory retrieval max query text must be between 1 and %d", MaxMemoryQueryTextLength)
	}
	if len(query.Text) > queryTextLimit {
		return fmt.Errorf("memory retrieval query text exceeds %d bytes", queryTextLimit)
	}
	if options.maxExplanation(policy) <= 0 || options.maxExplanation(policy) > MaxMemoryExplanationLength {
		return fmt.Errorf("memory retrieval max explanation must be between 1 and %d", MaxMemoryExplanationLength)
	}
	if options.maxReason(policy) <= 0 || options.maxReason(policy) > MaxMemoryReasonLength {
		return fmt.Errorf("memory retrieval max reason must be between 1 and %d", MaxMemoryReasonLength)
	}
	if options.MaxPerNamespace < 0 || options.MaxPerNamespace > resultLimit {
		return fmt.Errorf("memory retrieval max per namespace must be between 0 and %d", resultLimit)
	}
	if options.SemanticTimeout < 0 {
		return errors.New("memory retrieval semantic timeout must not be negative")
	}
	if !options.SemanticHealth.valid() {
		return fmt.Errorf("memory retrieval semantic health code %q is invalid", options.SemanticHealth)
	}
	return nil
}

func (options MemoryRetrievalOptions) maxCandidates(policy MemoryPolicy) int {
	if options.MaxCandidates > 0 {
		return options.MaxCandidates
	}
	if options.CandidateLimit > 0 {
		return options.CandidateLimit
	}
	return minInt(policy.MaxQueryRecords, policy.MaxRecords)
}

func (options MemoryRetrievalOptions) maxResults(policy MemoryPolicy) int {
	if options.MaxResults > 0 {
		return options.MaxResults
	}
	if options.ResultLimit > 0 {
		return options.ResultLimit
	}
	return policy.MaxQueryRecords
}

func (options MemoryRetrievalOptions) maxBytes(policy MemoryPolicy) int {
	if options.MaxBytes > 0 {
		return options.MaxBytes
	}
	return policy.MaxQueryBytes
}

func (options MemoryRetrievalOptions) maxQueryText() int {
	if options.MaxQueryTextBytes > 0 {
		return options.MaxQueryTextBytes
	}
	if options.MaxQueryText > 0 {
		return options.MaxQueryText
	}
	return MaxMemoryQueryTextLength
}

func (options MemoryRetrievalOptions) maxExplanation(policy MemoryPolicy) int {
	if options.MaxExplanationBytes > 0 {
		return options.MaxExplanationBytes
	}
	return minInt(MaxMemoryExplanationLength, policy.MaxQueryBytes)
}

func (options MemoryRetrievalOptions) maxReason(policy MemoryPolicy) int {
	if options.MaxReasonBytes > 0 {
		return options.MaxReasonBytes
	}
	return minInt(MaxMemoryReasonLength, policy.MaxQueryBytes)
}

// RankedMemoryRetriever is an additive interface; MemoryRetriever and
// MemoryStore remain source-compatible with Phase 7A/7B callers.
type RankedMemoryRetriever interface {
	RetrieveRanked(context.Context, MemoryQuery, MemoryRetrievalOptions) (RankedMemoryResult, error)
}

// MemoryRankingRequest is the explicit boundary for a custom ranker. Callers
// must opt into AllowMemoryText before a provider receives durable records.
type MemoryRankingRequest struct {
	Query           MemoryQuery
	Candidates      []MemoryRecord
	AsOf            time.Time
	AllowMemoryText bool
}

// MemoryRanker is optional. LocalMemoryStore uses deterministic lexical
// ranking by default and does not require an implementation of this seam.
type MemoryRanker interface {
	Rank(context.Context, MemoryRankingRequest) ([]RankedMemoryHit, error)
}

// MemoryEmbeddingRequest carries only query data by default. Durable memory
// text is never sent to an embedder by the local core.
type MemoryEmbeddingRequest struct {
	QueryText       string
	AllowMemoryText bool
}

// MemoryEmbedding is an optional semantic vector. Values must be finite,
// non-empty, and bounded in dimension.
type MemoryEmbedding struct {
	Values []float64 `json:"values"`
}

func (embedding MemoryEmbedding) Validate() error {
	if len(embedding.Values) == 0 || len(embedding.Values) > MaxMemoryEmbeddingDimensions {
		return fmt.Errorf("memory embedding dimensions must be between 1 and %d", MaxMemoryEmbeddingDimensions)
	}
	norm := 0.0
	for _, value := range embedding.Values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("memory embedding values must be finite")
		}
		norm += value * value
	}
	if math.IsNaN(norm) || math.IsInf(norm, 0) {
		return errors.New("memory embedding norm must be finite")
	}
	if norm == 0 {
		return errors.New("memory embedding must not be the zero vector")
	}
	return nil
}

// MemoryEmbedder and MemoryVectorIndex are optional seams. They are not used
// unless a caller explicitly supplies both through MemoryRetrievalOptions.
type MemoryEmbedder interface {
	Embed(context.Context, MemoryEmbeddingRequest) (MemoryEmbedding, error)
}

type MemoryVectorHit struct {
	RecordID string  `json:"record_id"`
	Score    float64 `json:"score"`
}

func (hit MemoryVectorHit) Validate() error {
	if err := validateSafeID("memory vector hit record_id", hit.RecordID, MaxMemoryIDLength); err != nil {
		return err
	}
	return validateBoundedScore("vector hit", hit.Score)
}

type MemoryVectorIndex interface {
	Search(context.Context, MemoryEmbedding, int) ([]MemoryVectorHit, error)
}

// FilteredMemoryVectorIndex lets the store apply authorization and temporal
// filtering before vector ranking without first narrowing candidates by
// recency. Implementations must search only the supplied record IDs.
type FilteredMemoryVectorIndex interface {
	SearchFiltered(context.Context, MemoryEmbedding, map[string]struct{}, int) ([]MemoryVectorHit, error)
}

// RetrieveRanked performs deterministic local lexical retrieval and optionally
// adds semantic scores. Filters are applied before scoring. Provider failures,
// invalid vectors, and invalid vector hits degrade to lexical results.
func (store *LocalMemoryStore) RetrieveRanked(ctx context.Context, query MemoryQuery, options MemoryRetrievalOptions) (RankedMemoryResult, error) {
	if store == nil {
		return RankedMemoryResult{}, errors.New("memory store is nil")
	}
	if err := query.Validate(store.policy); err != nil {
		return RankedMemoryResult{}, err
	}
	effectiveOptions := options
	if effectiveOptions.MaxResults == 0 && effectiveOptions.ResultLimit == 0 && query.Limit > 0 {
		effectiveOptions.MaxResults = query.Limit
	}
	if effectiveOptions.MaxBytes == 0 && query.MaxBytes > 0 {
		effectiveOptions.MaxBytes = query.MaxBytes
	}
	if err := effectiveOptions.validate(store.policy, query); err != nil {
		return RankedMemoryResult{}, err
	}
	if err := contextErr(ctx); err != nil {
		return RankedMemoryResult{}, err
	}
	if runtimeCode := semanticRuntimeHealth(ctx, effectiveOptions); runtimeCode != "" {
		effectiveOptions.SemanticHealth = runtimeCode
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return RankedMemoryResult{}, err
	}
	asOf := query.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	eligible := make([]MemoryRecord, 0, len(store.state))
	filtered := 0
	for _, state := range store.state {
		if state.deleted || isMemoryExpired(state.record, store.policy, asOf) || !memoryMatchesTemporal(state.record, query, asOf) {
			filtered++
			continue
		}
		if state.record.Sensitivity == MemorySensitivitySensitive && !query.IncludeSensitive {
			filtered++
			continue
		}
		if query.Namespace != "" && state.record.Namespace != query.Namespace {
			filtered++
			continue
		}
		if query.Key != "" && state.record.Key != query.Key {
			filtered++
			continue
		}
		if query.SubjectID != "" && state.record.SubjectID != query.SubjectID {
			filtered++
			continue
		}
		if strings.TrimSpace(query.Text) != "" {
			_, _, matched := lexicalMemoryComponents(state.record, query.Text)
			semanticRequested := effectiveOptions.SemanticEnabled || effectiveOptions.Embedder != nil || effectiveOptions.VectorIndex != nil
			if !matched && !semanticRequested {
				filtered++
				continue
			}
		}
		eligible = append(eligible, state.record)
	}
	total := len(eligible)
	candidateLimit := effectiveOptions.maxCandidates(store.policy)
	semanticScores, semantic, degradedReason, degradedCode := retrieveSemanticScores(ctx, query, eligible, candidateLimit, effectiveOptions)
	if degradedReason != "" && strings.TrimSpace(query.Text) != "" {
		// Lexical fallback considers every authorized record before applying the
		// candidate bound, so an older exact match cannot be displaced merely by
		// newer records.
		lexicalEligible := eligible[:0]
		for _, record := range eligible {
			_, _, matched := lexicalMemoryComponents(record, query.Text)
			if matched {
				lexicalEligible = append(lexicalEligible, record)
			}
		}
		eligible = lexicalEligible
		total = len(eligible)
		semanticScores = map[string]float64{}
		semantic = false
	}
	hits := make([]RankedMemoryHit, 0, len(eligible))
	for _, record := range eligible {
		if err := contextErr(ctx); err != nil {
			return RankedMemoryResult{}, err
		}
		semanticScore, semanticMatched := semanticScores[record.ID]
		_, _, lexicalMatched := lexicalMemoryComponents(record, query.Text)
		if semantic && strings.TrimSpace(query.Text) != "" && !lexicalMatched && !semanticMatched {
			continue
		}
		hit := lexicalMemoryHit(record, query.Text, asOf, store.policy, semanticScore, semantic, effectiveOptions)
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score.Total != hits[j].Score.Total {
			return hits[i].Score.Total > hits[j].Score.Total
		}
		if !hits[i].Record.UpdatedAt.Equal(hits[j].Record.UpdatedAt) {
			return hits[i].Record.UpdatedAt.After(hits[j].Record.UpdatedAt)
		}
		return hits[i].Record.ID < hits[j].Record.ID
	})
	if len(hits) > candidateLimit {
		hits = hits[:candidateLimit]
	}
	result := RankedMemoryResult{Total: total}
	if query.Actor.Kind == MemoryActorChild {
		// Filter counts can reveal the existence of sensitive, expired, or
		// otherwise inaccessible records. Child diagnostics expose only the
		// records that survived authorization.
		filtered = 0
	}
	result.Diagnostics = MemoryRetrievalDiagnostics{Candidates: len(hits), Filtered: filtered, Semantic: semantic, Degraded: degradedReason != "", DegradedReason: degradedReason, SemanticHealth: string(effectiveOptions.SemanticHealth), DegradedCode: degradedCode}
	maxResults := effectiveOptions.maxResults(store.policy)
	perNamespace := options.MaxPerNamespace
	if effectiveOptions.Diversity && perNamespace == 0 {
		perNamespace = 1
	}
	namespaceCounts := make(map[MemoryNamespace]int)
	for _, hit := range hits {
		if len(result.Hits) >= maxResults {
			result.Truncated = true
			break
		}
		if perNamespace > 0 && namespaceCounts[hit.Record.Namespace] >= perNamespace {
			result.Truncated = true
			result.Diagnostics.DiversityLimited = true
			continue
		}
		encoded, err := json.Marshal(hit)
		if err != nil {
			return RankedMemoryResult{}, fmt.Errorf("encode ranked memory hit: %w", err)
		}
		if result.Bytes+len(encoded) > effectiveOptions.maxBytes(store.policy) {
			result.Truncated = true
			break
		}
		result.Hits = append(result.Hits, hit)
		result.Bytes += len(encoded)
		namespaceCounts[hit.Record.Namespace]++
	}
	if len(result.Hits) < total {
		result.Truncated = true
	}
	result.Diagnostics.Returned = len(result.Hits)
	if err := result.Validate(store.policy, effectiveOptions); err != nil {
		return RankedMemoryResult{}, fmt.Errorf("validate ranked retrieval result: %w", err)
	}
	return result, nil
}

// RetrieveScored is a descriptive compatibility alias for RetrieveRanked.
func (store *LocalMemoryStore) RetrieveScored(ctx context.Context, query MemoryQuery, options MemoryRetrievalOptions) (RankedMemoryResult, error) {
	return store.RetrieveRanked(ctx, query, options)
}

// RetrieveRankedContext projects ranked hits into the existing transient
// context contract. Durable records and ranking explanations remain separate
// from the packet, and only the bounded score is copied into each item.
func (store *LocalMemoryStore) RetrieveRankedContext(ctx context.Context, query MemoryQuery, options MemoryRetrievalOptions, sessionID string) (MemoryContextPacket, error) {
	result, err := store.RetrieveRanked(ctx, query, options)
	if err != nil {
		return MemoryContextPacket{}, err
	}
	if err := result.Validate(store.policy, effectiveRetrievalOptions(query, options)); err != nil {
		return MemoryContextPacket{}, err
	}
	packet := MemoryContextPacket{SessionID: sessionID}
	for _, hit := range result.Hits {
		item := MemoryContextItem{RecordID: hit.Record.ID, Namespace: hit.Record.Namespace, Key: hit.Record.Key, Content: hit.Record.Content, Summary: hit.Record.Summary, Sensitivity: hit.Record.Sensitivity, Score: hit.Score.Total}
		item.Updated, item.Status = memoryContextProvenance(hit.Record)
		encoded, err := json.Marshal(item)
		if err != nil {
			return MemoryContextPacket{}, fmt.Errorf("encode ranked memory context item: %w", err)
		}
		if len(packet.Items) >= store.policy.MaxContextRecords || packet.UsedBytes+len(encoded) > store.policy.MaxContextBytes {
			packet.Truncated = true
			break
		}
		packet.Items = append(packet.Items, item)
		packet.UsedBytes += len(encoded)
	}
	if len(packet.Items) < len(result.Hits) || result.Truncated {
		packet.Truncated = true
	}
	if err := packet.Validate(store.policy, query.Actor); err != nil {
		return MemoryContextPacket{}, err
	}
	return packet, nil
}

func effectiveRetrievalOptions(query MemoryQuery, options MemoryRetrievalOptions) MemoryRetrievalOptions {
	if options.MaxResults == 0 && options.ResultLimit == 0 && query.Limit > 0 {
		options.MaxResults = query.Limit
	}
	if options.MaxBytes == 0 && query.MaxBytes > 0 {
		options.MaxBytes = query.MaxBytes
	}
	return options
}

// RebuildMemoryIndex explicitly replays the durable log. Local retrieval does
// not maintain a second index, so this operation is deterministic and safe:
// every retrieve observes replacements and tombstones from the source log.
func (store *LocalMemoryStore) RebuildMemoryIndex(ctx context.Context) error {
	if store == nil {
		return errors.New("memory store is nil")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	return store.replayLocked()
}

// Reindex is a short compatibility alias for RebuildMemoryIndex.
func (store *LocalMemoryStore) Reindex(ctx context.Context) error {
	return store.RebuildMemoryIndex(ctx)
}

func retrieveSemanticScores(ctx context.Context, query MemoryQuery, records []MemoryRecord, candidateLimit int, options MemoryRetrievalOptions) (map[string]float64, bool, string, string) {
	scores := make(map[string]float64)
	if !options.SemanticEnabled && options.Embedder == nil && options.VectorIndex == nil {
		return scores, false, "", ""
	}
	if strings.TrimSpace(query.Text) == "" {
		return scores, false, "", ""
	}
	if options.SemanticHealth != "" && options.SemanticHealth != SemanticHealthReady && options.SemanticHealth != SemanticHealthEmpty {
		return scores, false, semanticHealthReason(options.SemanticHealth), string(options.SemanticHealth)
	}
	if options.Embedder == nil || options.VectorIndex == nil {
		return scores, false, "semantic provider unavailable", string(SemanticHealthProviderUnavailable)
	}
	if len(records) == 0 {
		return scores, true, "", ""
	}
	providerCtx := ctx
	cancel := func() {}
	if options.SemanticTimeout > 0 {
		providerCtx, cancel = context.WithTimeout(ctx, options.SemanticTimeout)
	}
	defer cancel()
	embedding, err := callMemoryEmbedder(providerCtx, options.Embedder, MemoryEmbeddingRequest{QueryText: query.Text, AllowMemoryText: options.AllowMemoryText})
	if err != nil {
		return scores, false, "semantic provider failed", string(SemanticHealthProviderUnavailable)
	}
	if err := embedding.Validate(); err != nil {
		return scores, false, "semantic provider returned invalid vector", string(SemanticHealthInvalid)
	}
	limit := minInt(len(records), minInt(candidateLimit, MaxMemorySemanticHits))
	var vectorHits []MemoryVectorHit
	if filtered, ok := options.VectorIndex.(FilteredMemoryVectorIndex); ok {
		allowed := make(map[string]struct{}, len(records))
		for _, record := range records {
			allowed[record.ID] = struct{}{}
		}
		vectorHits, err = filtered.SearchFiltered(providerCtx, embedding, allowed, limit)
	} else {
		vectorHits, err = callMemoryVectorIndex(providerCtx, options.VectorIndex, embedding, limit)
	}
	if err != nil {
		return scores, false, "semantic index failed", string(SemanticHealthProviderUnavailable)
	}
	if len(vectorHits) > MaxMemorySemanticHits {
		return scores, false, "semantic index returned too many hits", string(SemanticHealthInvalid)
	}
	allowed := make(map[string]struct{}, len(records))
	for _, record := range records {
		allowed[record.ID] = struct{}{}
	}
	for _, vectorHit := range vectorHits {
		if err := vectorHit.Validate(); err != nil {
			return map[string]float64{}, false, "semantic index returned invalid hit", string(SemanticHealthInvalid)
		}
		if _, ok := allowed[vectorHit.RecordID]; !ok {
			return map[string]float64{}, false, "semantic index returned unknown record", string(SemanticHealthInvalid)
		}
		if _, duplicate := scores[vectorHit.RecordID]; duplicate {
			return map[string]float64{}, false, "semantic index returned duplicate record", string(SemanticHealthInvalid)
		}
		scores[vectorHit.RecordID] = vectorHit.Score
	}
	return scores, true, "", ""
}

func semanticHealthReason(code SemanticHealthCode) string {
	switch code {
	case SemanticHealthMissing:
		return "semantic index is missing"
	case SemanticHealthStale:
		return "semantic index is stale"
	case SemanticHealthInvalid:
		return "semantic index is invalid"
	case SemanticHealthMisconfigured:
		return "semantic configuration is invalid"
	case SemanticHealthProviderUnavailable:
		return "semantic provider unavailable"
	case SemanticHealthModelMismatch:
		return "semantic index model mismatch"
	case SemanticHealthRootMismatch:
		return "semantic index memory root mismatch"
	default:
		return "semantic retrieval is degraded"
	}
}

func semanticRuntimeHealth(ctx context.Context, options MemoryRetrievalOptions) SemanticHealthCode {
	index, ok := options.VectorIndex.(interface {
		runtimeHealth(context.Context) SemanticHealthCode
	})
	if !ok || index == nil {
		return ""
	}
	return index.runtimeHealth(ctx)
}

func callMemoryEmbedder(ctx context.Context, embedder MemoryEmbedder, request MemoryEmbeddingRequest) (MemoryEmbedding, error) {
	type response struct {
		embedding MemoryEmbedding
		err       error
	}
	responses := make(chan response, 1)
	go func() {
		embedding, err := embedder.Embed(ctx, request)
		responses <- response{embedding: embedding, err: err}
	}()
	select {
	case result := <-responses:
		return result.embedding, result.err
	case <-ctx.Done():
		return MemoryEmbedding{}, ctx.Err()
	}
}

func callMemoryVectorIndex(ctx context.Context, index MemoryVectorIndex, embedding MemoryEmbedding, limit int) ([]MemoryVectorHit, error) {
	type response struct {
		hits []MemoryVectorHit
		err  error
	}
	responses := make(chan response, 1)
	go func() {
		hits, err := index.Search(ctx, embedding, limit)
		responses <- response{hits: hits, err: err}
	}()
	select {
	case result := <-responses:
		return result.hits, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func lexicalMemoryHit(record MemoryRecord, queryText string, asOf time.Time, policy MemoryPolicy, semantic float64, semanticAvailable bool, options MemoryRetrievalOptions) RankedMemoryHit {
	lexical, phrase, matched := lexicalMemoryComponents(record, queryText)
	if strings.TrimSpace(queryText) != "" && !matched {
		// This path is intentionally unreachable for normal local retrieval, but
		// keeps the standalone scoring helper bounded and unsurprising.
		lexical, phrase = 0, 0
	}
	confidence := record.Confidence
	recency := memoryRecencyScore(record, policy, asOf)
	if !semanticAvailable {
		semantic = 0
	}
	semanticWeight := 0.0
	if semanticAvailable {
		semanticWeight = 0.10
	}
	lexicalWeight := 0.65
	if semanticAvailable {
		lexicalWeight = 0.55
	}
	total := lexicalWeight*lexical + 0.10*phrase + 0.15*confidence + 0.10*recency + semanticWeight*semantic
	if total < 0 {
		total = 0
	}
	if total > 1 {
		total = 1
	}
	score := MemoryScore{Total: total, Value: total, Lexical: lexical, Phrase: phrase, Confidence: confidence, Recency: recency, Semantic: semantic}
	explanation := fmt.Sprintf("lexical %.3f; phrase %.3f; confidence %.3f; recency %.3f", lexical, phrase, confidence, recency)
	if semanticAvailable {
		explanation += fmt.Sprintf("; semantic %.3f", semantic)
	}
	if len(explanation) > options.maxExplanation(policy) {
		explanation = explanation[:options.maxExplanation(policy)]
	}
	reason := "matched bounded memory fields"
	if strings.TrimSpace(queryText) == "" {
		reason = "filtered memory ranked by confidence and recency"
	}
	if len(reason) > options.maxReason(policy) {
		reason = reason[:options.maxReason(policy)]
	}
	return RankedMemoryHit{Record: record, Score: score, Explanation: explanation, Reason: reason}
}

func lexicalMemoryComponents(record MemoryRecord, query string) (float64, float64, bool) {
	queryTokens := memoryTokens(query)
	if len(queryTokens) == 0 {
		return 0, 0, true
	}
	type field struct {
		value  string
		weight float64
	}
	fields := []field{
		{record.Key, 1.00},
		{sortedTagText(record.Tags), 0.85},
		{record.Summary, 0.80},
		{record.Content, 0.60},
		{record.Source, 0.35},
	}
	matchedAny := false
	maxToken := 0.0
	maxPhrase := 0.0
	weightedCoverage := 0.0
	weightTotal := 0.0
	phrase := strings.ToLower(strings.TrimSpace(query))
	for _, current := range fields {
		if strings.TrimSpace(current.value) == "" {
			continue
		}
		fieldTokens := memoryTokens(current.value)
		present := make(map[string]struct{}, len(fieldTokens))
		for _, token := range fieldTokens {
			present[token] = struct{}{}
		}
		matched := 0
		for _, token := range queryTokens {
			if _, ok := present[token]; ok {
				matched++
			}
		}
		coverage := float64(matched) / float64(len(queryTokens))
		if matched > 0 {
			matchedAny = true
		}
		weightedCoverage += current.weight * coverage
		weightTotal += current.weight
		if current.weight*coverage > maxToken {
			maxToken = current.weight * coverage
		}
		if phrase != "" && strings.Contains(strings.ToLower(current.value), phrase) {
			if current.weight > maxPhrase {
				maxPhrase = current.weight
			}
		}
	}
	if !matchedAny {
		return 0, 0, false
	}
	average := 0.0
	if weightTotal > 0 {
		average = weightedCoverage / weightTotal
	}
	return clampScore(0.8*maxToken + 0.2*average), clampScore(maxPhrase), true
}

func sortedTagText(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		parts = append(parts, key, tags[key])
	}
	return strings.Join(parts, " ")
}

func memoryRecencyScore(record MemoryRecord, policy MemoryPolicy, asOf time.Time) float64 {
	if record.UpdatedAt.After(asOf) {
		return 1
	}
	age := asOf.Sub(record.UpdatedAt)
	window := policy.Retention
	if window <= 0 {
		window = 365 * 24 * time.Hour
	}
	if age <= 0 {
		return 1
	}
	if age >= window {
		return 0
	}
	return clampScore(1 - age.Seconds()/window.Seconds())
}

func clampScore(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

var _ RankedMemoryRetriever = (*LocalMemoryStore)(nil)
var _ MemoryRanker = LexicalMemoryRanker{}

// LexicalMemoryRanker exposes the same deterministic scorer used by the local
// backend for callers that need a provider-neutral ranker seam.
type LexicalMemoryRanker struct{}

func (LexicalMemoryRanker) Rank(ctx context.Context, request MemoryRankingRequest) ([]RankedMemoryHit, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	policy := DefaultMemoryPolicy()
	options := MemoryRetrievalOptions{}
	asOf := request.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	hits := make([]RankedMemoryHit, 0, len(request.Candidates))
	for _, record := range request.Candidates {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		hits = append(hits, lexicalMemoryHit(record, request.Query.Text, asOf, policy, 0, false, options))
	}
	return hits, nil
}
