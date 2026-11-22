package bee

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	MaxMediaSearchCount               = 12
	MaxMediaSearchCandidates          = 12
	MaxMediaChooserFallbackCandidates = 3
	MaxMediaAutoRetrieveAttempts      = 3
	MaxMediaSearchTitleBytes          = 512
	MaxMediaSearchSnippetBytes        = 2048
	MaxMediaSearchSourceBytes         = 256
	MaxMediaSearchSourceURLBytes      = MaxWebURLBytes
	MaxMediaSearchDetailBytes         = 128
	MaxMediaSearchBindings            = 512
	DefaultMediaCandidateTTL          = 10 * time.Minute
)

type MediaSearchRequest struct {
	Kind  string `json:"kind"`
	Query string `json:"query"`
	Count int    `json:"count,omitempty"`
}

type MediaSearchCandidate struct {
	CandidateID string `json:"candidate_id"`
	Token       string `json:"token"`
	Kind        string `json:"kind"`
	Title       string `json:"title,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	Source      string `json:"source,omitempty"`
	SourceURL   string `json:"source_url"`
	Duration    string `json:"duration,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
}

type MediaSearchResult struct {
	Query                 string                 `json:"query"`
	Kind                  string                 `json:"kind"`
	Candidates            []MediaSearchCandidate `json:"candidates"`
	Untrusted             bool                   `json:"untrusted"`
	Truncated             bool                   `json:"truncated,omitempty"`
	Attached              bool                   `json:"-"`
	AttachmentCount       int                    `json:"-"`
	AutoRetrieveFailed    bool                   `json:"-"`
	ConsumedAttempts      int                    `json:"-"`
	AlreadyAttached       bool                   `json:"-"`
	NoAdditionalRetrieval bool                   `json:"-"`
}

// MediaSearchModelCandidate is the provider-facing, model-safe projection of
// a media candidate. Selection bindings and retrieval URLs stay UI-only.
type MediaSearchModelCandidate struct {
	Rank       int    `json:"rank"`
	Title      string `json:"title,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Source     string `json:"source,omitempty"`
	Duration   string `json:"duration,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

type MediaSearchModelResult struct {
	Kind                  string                      `json:"kind"`
	Candidates            []MediaSearchModelCandidate `json:"candidates"`
	Attached              bool                        `json:"attached"`
	AttachmentCount       int                         `json:"attachment_count"`
	AlreadyAttached       bool                        `json:"already_attached"`
	NoAdditionalRetrieval bool                        `json:"no_additional_retrieval"`
	Notice                string                      `json:"notice"`
	Instruction           string                      `json:"instruction"`
}

// MediaSearchEvent is a provider-neutral UI-only run payload. It deliberately
// carries the opaque bindings and retrieval URLs that must never reach a
// provider/model.
type MediaSearchEvent struct {
	Query                 string                 `json:"query"`
	Kind                  string                 `json:"kind"`
	Candidates            []MediaSearchCandidate `json:"candidates"`
	Untrusted             bool                   `json:"untrusted"`
	Truncated             bool                   `json:"truncated,omitempty"`
	AutoRetrieved         bool                   `json:"auto_retrieved"`
	AlreadyAttached       bool                   `json:"already_attached"`
	NoAdditionalRetrieval bool                   `json:"no_additional_retrieval"`
}

// MediaSearchAutoRetrieveResult is an internal, bounded handoff from the
// runtime's existing MediaService to the model-facing search tool. Error is
// intentionally a boolean-like status projection; raw retrieval errors never
// reach the model.
type MediaSearchAutoRetrieveResult struct {
	Attached              bool
	AttachmentCount       int
	Failed                bool
	AlreadyAttached       bool
	NoAdditionalRetrieval bool
	ConsumedAttempts      int
}

type MediaSelection struct {
	Kind string
	URL  string
}

type mediaCandidateBinding struct {
	SessionID SessionID
	Candidate MediaSearchCandidate
	URL       string
	ExpiresAt time.Time
}

type mediaSearchBatch struct {
	Candidates []MediaSearchCandidate
	Consumed   int
}

type MediaSearchService struct {
	Client  *WebClient
	TTL     time.Duration
	Now     func() time.Time
	mu      sync.Mutex
	once    sync.Once
	byToken map[string]mediaCandidateBinding
	latest  map[SessionID]mediaSearchBatch
}

func NewMediaSearchService(client *WebClient) *MediaSearchService {
	return &MediaSearchService{Client: client, TTL: DefaultMediaCandidateTTL, Now: func() time.Time { return time.Now().UTC() }, byToken: make(map[string]mediaCandidateBinding), latest: make(map[SessionID]mediaSearchBatch)}
}

func (service *MediaSearchService) init() {
	service.once.Do(func() {
		if service.TTL <= 0 {
			service.TTL = DefaultMediaCandidateTTL
		}
		if service.Now == nil {
			service.Now = func() time.Time { return time.Now().UTC() }
		}
		if service.byToken == nil {
			service.byToken = make(map[string]mediaCandidateBinding)
		}
		if service.latest == nil {
			service.latest = make(map[SessionID]mediaSearchBatch)
		}
	})
}

func (service *MediaSearchService) Search(ctx context.Context, sessionID SessionID, req MediaSearchRequest) (MediaSearchResult, error) {
	if service == nil || service.Client == nil {
		return MediaSearchResult{}, errors.New("media search service is not configured")
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		return MediaSearchResult{}, errors.New("media search requires a durable session")
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind != "image" && kind != "video" {
		return MediaSearchResult{}, errors.New("media search kind must be image or video")
	}
	query := strings.TrimSpace(req.Query)
	if query == "" || len(query) > MaxWebQueryBytes {
		return MediaSearchResult{}, errors.New("media search query is empty or too large")
	}
	count := req.Count
	if count <= 0 {
		count = service.Client.MaxResults
	}
	if count <= 0 {
		count = DefaultWebMaxResults
	}
	if count > MaxMediaSearchCount {
		count = MaxMediaSearchCount
	}
	items, truncated, err := service.Client.SearchMedia(ctx, MediaSearchRequest{Kind: kind, Query: query, Count: count})
	if err != nil {
		return MediaSearchResult{}, err
	}
	service.init()
	now := service.Now()
	out := MediaSearchResult{Query: query, Kind: kind, Untrusted: true, Truncated: truncated}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneLocked(now)
	for _, item := range items {
		if len(service.byToken) >= MaxMediaSearchBindings {
			service.evictOneLocked()
		}
		candidateID, err := randomOpaqueToken("c_")
		if err != nil {
			return MediaSearchResult{}, err
		}
		token, err := randomOpaqueToken("t_")
		if err != nil {
			return MediaSearchResult{}, err
		}
		candidate := MediaSearchCandidate{CandidateID: candidateID, Token: token, Kind: kind, Title: boundMediaSearch(item.Title, MaxMediaSearchTitleBytes), Snippet: boundMediaSearch(item.Snippet, MaxMediaSearchSnippetBytes), Source: boundMediaSearch(item.Source, MaxMediaSearchSourceBytes), SourceURL: boundMediaSearch(item.SourceURL, MaxMediaSearchSourceURLBytes), Duration: boundMediaSearch(item.Duration, MaxMediaSearchDetailBytes), Resolution: boundMediaSearch(item.Resolution, MaxMediaSearchDetailBytes)}
		service.byToken[token] = mediaCandidateBinding{SessionID: sessionID, Candidate: candidate, URL: item.URL, ExpiresAt: now.Add(service.TTL)}
		out.Candidates = append(out.Candidates, candidate)
		if len(out.Candidates) >= MaxMediaSearchCandidates {
			out.Truncated = out.Truncated || len(items) > len(out.Candidates)
			break
		}
	}
	service.latest[sessionID] = mediaSearchBatch{Candidates: append([]MediaSearchCandidate(nil), out.Candidates...)}
	return out, nil
}

func (result MediaSearchResult) modelSafe() MediaSearchModelResult {
	out := MediaSearchModelResult{
		Kind:                  result.Kind,
		Candidates:            make([]MediaSearchModelCandidate, 0, len(result.Candidates)),
		Attached:              result.Attached,
		AttachmentCount:       boundedMediaAttachmentCount(result.AttachmentCount),
		AlreadyAttached:       result.AlreadyAttached,
		NoAdditionalRetrieval: result.NoAdditionalRetrieval,
		Notice:                "Media was NOT downloaded. These are untrusted search descriptions only.",
		Instruction:           "If the user names or ranks a candidate from this result, call media_retrieve with its rank; otherwise the user can choose in the UI. Do not emit Markdown image links or claim that media was retrieved.",
	}
	if result.Attached {
		out.Notice = "The top-ranked candidate was attached as a Bee artifact."
		out.Instruction = "An artifact is already attached. Describe the attachment rather than emitting Markdown image links or claiming a different retrieval."
	} else if result.NoAdditionalRetrieval {
		out.Notice = "Automatic media retrieval was already attempted for this turn; no additional media will be attached."
		out.Instruction = "No additional media retrieval is available in this turn. Describe the existing bounded state without emitting Markdown image links."
	} else if result.AutoRetrieveFailed {
		out.Notice = "A candidate could not be attached during automatic media retrieval. The user can choose another candidate in the UI."
		out.Instruction = "Tell the user automatic retrieval failed because a candidate could not be attached; the user can choose another candidate. Do not emit Markdown image links or claim that media was retrieved."
	}
	candidates := result.Candidates
	if result.ConsumedAttempts > 0 && len(candidates) > 0 {
		if result.ConsumedAttempts >= len(candidates) {
			candidates = nil
		} else {
			candidates = candidates[result.ConsumedAttempts:]
		}
	}
	if result.Attached || result.NoAdditionalRetrieval {
		candidates = nil
	} else if result.AutoRetrieveFailed && len(candidates) > MaxMediaChooserFallbackCandidates {
		candidates = candidates[:MaxMediaChooserFallbackCandidates]
	}
	for index, candidate := range candidates {
		out.Candidates = append(out.Candidates, MediaSearchModelCandidate{
			Rank:       index + 1,
			Title:      sanitizeMediaSearchModelText(candidate.Title, MaxMediaSearchTitleBytes),
			Snippet:    sanitizeMediaSearchModelText(candidate.Snippet, MaxMediaSearchSnippetBytes),
			Source:     sanitizeMediaSearchSource(candidate.Source),
			Duration:   sanitizeMediaSearchModelText(candidate.Duration, MaxMediaSearchDetailBytes),
			Resolution: sanitizeMediaSearchModelText(candidate.Resolution, MaxMediaSearchDetailBytes),
		})
	}
	return out
}

func (result MediaSearchResult) runEvent() MediaSearchEvent {
	if result.Attached || result.NoAdditionalRetrieval {
		return MediaSearchEvent{Query: result.Query, Kind: result.Kind, Untrusted: result.Untrusted, Truncated: result.Truncated, AutoRetrieved: result.Attached, AlreadyAttached: result.AlreadyAttached, NoAdditionalRetrieval: result.NoAdditionalRetrieval}
	}
	candidates := result.Candidates
	if result.ConsumedAttempts > 0 && len(candidates) > 0 {
		if result.ConsumedAttempts >= len(candidates) {
			candidates = nil
		} else {
			candidates = candidates[result.ConsumedAttempts:]
		}
	}
	if result.AutoRetrieveFailed && len(candidates) > MaxMediaChooserFallbackCandidates {
		candidates = candidates[:MaxMediaChooserFallbackCandidates]
	}
	return MediaSearchEvent{Query: result.Query, Kind: result.Kind, Candidates: append([]MediaSearchCandidate(nil), candidates...), Untrusted: result.Untrusted, Truncated: result.Truncated, AutoRetrieved: result.Attached, AlreadyAttached: result.AlreadyAttached, NoAdditionalRetrieval: result.NoAdditionalRetrieval}
}

func boundedMediaAttachmentCount(count int) int {
	if count < 0 {
		return 0
	}
	if count > 4 {
		return 4
	}
	return count
}

func (service *MediaSearchService) Resolve(sessionID SessionID, candidateID, token string) (MediaSelection, error) {
	if service == nil {
		return MediaSelection{}, errors.New("media search service is not configured")
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		return MediaSelection{}, errors.New("media selection requires a durable session")
	}
	candidateID, token = strings.TrimSpace(candidateID), strings.TrimSpace(token)
	if candidateID == "" || token == "" || len(candidateID) > 128 || len(token) > 128 {
		return MediaSelection{}, errors.New("candidate_id and token are required")
	}
	service.init()
	now := service.Now()
	service.mu.Lock()
	binding, ok := service.byToken[token]
	service.pruneLocked(now)
	if !ok {
		service.mu.Unlock()
		return MediaSelection{}, errors.New("media candidate is missing, expired, or already used")
	}
	if binding.SessionID != sessionID || binding.Candidate.CandidateID != candidateID {
		service.mu.Unlock()
		return MediaSelection{}, errors.New("media candidate does not belong to this session")
	}
	if !now.Before(binding.ExpiresAt) {
		delete(service.byToken, token)
		service.mu.Unlock()
		return MediaSelection{}, errors.New("media candidate is expired")
	}
	delete(service.byToken, token)
	service.mu.Unlock()
	validated, err := validatePublicMediaURL(binding.URL)
	if err != nil {
		return MediaSelection{}, fmt.Errorf("media candidate URL is no longer valid: %w", err)
	}
	return MediaSelection{Kind: binding.Candidate.Kind, URL: validated.String()}, nil
}

// ResolveTop consumes only rank 1 from this just-produced search result. The
// binding remains session-bound and single-use through Resolve.
func (service *MediaSearchService) ResolveTop(sessionID SessionID, result MediaSearchResult) (MediaSelection, error) {
	if len(result.Candidates) == 0 {
		return MediaSelection{}, errors.New("media search returned no candidates")
	}
	top := result.Candidates[0]
	return service.Resolve(sessionID, top.CandidateID, top.Token)
}

// ResolveRank resolves one bounded rank from the most recent search result for
// this durable session. The opaque binding remains authoritative: Resolve
// enforces expiry, session ownership, public-URL validation, and single use.
func (service *MediaSearchService) ResolveRank(sessionID SessionID, rank int) (MediaSelection, error) {
	if service == nil {
		return MediaSelection{}, errors.New("media search service is not configured")
	}
	if strings.TrimSpace(string(sessionID)) == "" {
		return MediaSelection{}, errors.New("media selection requires a durable session")
	}
	if rank < 1 || rank > MaxMediaSearchCandidates {
		return MediaSelection{}, fmt.Errorf("media selection rank must be between 1 and %d", MaxMediaSearchCandidates)
	}
	service.init()
	service.mu.Lock()
	batch, ok := service.latest[sessionID]
	index := batch.Consumed + rank - 1
	if !ok || index < batch.Consumed || index >= len(batch.Candidates) {
		service.mu.Unlock()
		return MediaSelection{}, errors.New("media selection rank is unavailable")
	}
	candidate := batch.Candidates[index]
	service.mu.Unlock()
	return service.Resolve(sessionID, candidate.CandidateID, candidate.Token)
}

// ConsumeAttempts advances the rank window after bounded automatic retrieval
// attempts. The opaque candidate bindings remain authoritative; this offset
// only keeps subsequent model/UI ranks aligned with unconsumed candidates.
func (service *MediaSearchService) ConsumeAttempts(sessionID SessionID, count int) {
	if service == nil || count <= 0 {
		return
	}
	service.init()
	service.mu.Lock()
	defer service.mu.Unlock()
	batch, ok := service.latest[sessionID]
	if !ok {
		return
	}
	batch.Consumed += count
	if batch.Consumed > len(batch.Candidates) {
		batch.Consumed = len(batch.Candidates)
	}
	service.latest[sessionID] = batch
}

func (service *MediaSearchService) pruneLocked(now time.Time) {
	for token, binding := range service.byToken {
		if !now.Before(binding.ExpiresAt) {
			delete(service.byToken, token)
		}
	}
}

func (service *MediaSearchService) evictOneLocked() {
	var oldestToken string
	var oldest time.Time
	for token, binding := range service.byToken {
		if oldestToken == "" || binding.ExpiresAt.Before(oldest) {
			oldestToken = token
			oldest = binding.ExpiresAt
		}
	}
	if oldestToken != "" {
		delete(service.byToken, oldestToken)
	}
}

func randomOpaqueToken(prefix string) (string, error) {
	var data [18]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

func boundMediaSearch(value string, max int) string {
	value = stripPrivateUseMarkers(value)
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	if len(value) > max {
		return value[:max]
	}
	return value
}

func stripPrivateUseMarkers(value string) string {
	if value == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if (r >= 0xE000 && r <= 0xF8FF) || (r >= 0xF0000 && r <= 0xFFFFD) || (r >= 0x100000 && r <= 0x10FFFD) {
			return -1
		}
		return r
	}, value)
}

func sanitizeMediaSearchSource(value string) string {
	value = sanitizeMediaSearchModelText(value, MaxMediaSearchSourceBytes)
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		return boundMediaSearch(parsed.Hostname(), MaxMediaSearchSourceBytes)
	}
	if strings.Contains(value, "://") {
		return ""
	}
	return value
}

var mediaSearchURLPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>"']+`)
var mediaSearchBindingPattern = regexp.MustCompile(`(?i)\b(?:t_|c_)[a-z0-9_-]+\b`)

func sanitizeMediaSearchModelText(value string, max int) string {
	value = boundMediaSearch(value, max)
	value = mediaSearchURLPattern.ReplaceAllString(value, "[url omitted]")
	value = mediaSearchBindingPattern.ReplaceAllString(value, "[binding omitted]")
	value = strings.NewReplacer("source_url", "[field omitted]", "img_src", "[field omitted]", "candidate_id", "[field omitted]", "token", "[field omitted]").Replace(value)
	return boundMediaSearch(value, max)
}

type mediaSearchItem struct {
	Title, Snippet, Source, SourceURL, Duration, Resolution, URL string
}

func (client *WebClient) SearchMedia(ctx context.Context, req MediaSearchRequest) ([]mediaSearchItem, bool, error) {
	if client == nil {
		return nil, false, errors.New("web client is not configured")
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind != "image" && kind != "video" {
		return nil, false, errors.New("media search kind must be image or video")
	}
	query := strings.TrimSpace(req.Query)
	if query == "" || len(query) > MaxWebQueryBytes {
		return nil, false, errors.New("media search query is empty or too large")
	}
	count := req.Count
	if count <= 0 {
		count = client.MaxResults
	}
	if count <= 0 {
		count = DefaultWebMaxResults
	}
	if count > MaxMediaSearchCount {
		count = MaxMediaSearchCount
	}
	base, err := validateWebURL(client.SearchEndpoint, true)
	if err != nil {
		return nil, false, err
	}
	u := base
	values := u.Query()
	values.Set("q", query)
	values.Set("format", "json")
	values.Set("categories", kind+"s")
	u.RawQuery = values.Encode()
	ctx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()
	data, _, contentType, err := client.do(ctx, u)
	if err != nil {
		return nil, false, err
	}
	if !strings.Contains(contentType, "json") {
		return nil, false, errors.New("media search endpoint did not return JSON")
	}
	// Use a raw intermediary because SearXNG uses the same result shape with
	// category-specific fields and some deployments omit optional extras.
	var raw struct {
		Results []map[string]any `json:"results"`
		Images  []map[string]any `json:"images"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("decode media search response: %w", err)
	}
	rows := raw.Results
	if kind == "image" && len(raw.Images) > 0 {
		rows = raw.Images
	}
	type scoredMediaItem struct {
		item  mediaSearchItem
		score int
		index int
	}
	queryTerms, queryPhrase := mediaSearchTerms(query)
	meaningfulTerms := len(queryTerms) > 0
	items := make([]scoredMediaItem, 0, len(rows))
	seenURLs := make(map[string]struct{})
	seenEntries := make(map[string]struct{})
	for rowIndex, row := range rows {
		get := func(name string) string {
			switch value := row[name].(type) {
			case string:
				return value
			case float64:
				return strconv.FormatFloat(value, 'f', -1, 64)
			case json.Number:
				return value.String()
			}
			return ""
		}
		retrieval := get("url")
		sourceURL := retrieval
		if kind == "image" {
			retrieval = get("img_src")
			sourceURL = get("url")
		}
		if retrieval == "" || sourceURL == "" {
			continue
		}
		retrievalURL, err := validateWebURL(retrieval, false)
		if err != nil {
			continue
		}
		source, err := validateWebURL(sourceURL, false)
		if err != nil {
			continue
		}
		item := mediaSearchItem{Title: stripPrivateUseMarkers(get("title")), Snippet: stripPrivateUseMarkers(get("content")), Source: stripPrivateUseMarkers(get("source")), SourceURL: source.String(), Duration: stripPrivateUseMarkers(get("duration")), Resolution: stripPrivateUseMarkers(get("resolution")), URL: retrievalURL.String()}
		if kind == "video" {
			// SearXNG video results commonly expose this as `length`, while
			// other deployments use `duration`. Keep the original bounded text
			// for display, but reject only candidates with a known duration over
			// Bee's retrieval ceiling. Unknown/malformed values remain eligible.
			item.Duration = stripPrivateUseMarkers(get("length"))
			if item.Duration == "" {
				item.Duration = stripPrivateUseMarkers(get("duration"))
			}
			if duration, ok := parseMediaSearchDuration(item.Duration); ok && duration > MaxMediaDuration {
				continue
			}
		}
		if item.Snippet == "" {
			item.Snippet = stripPrivateUseMarkers(get("snippet"))
		}
		if item.Source == "" {
			if parsed, parseErr := url.Parse(source.String()); parseErr == nil {
				item.Source = parsed.Hostname()
			}
		}
		itemText := normalizeMediaText(strings.Join([]string{item.Title, item.Snippet, item.Source}, " "))
		titleText := normalizeMediaText(item.Title)
		if meaningfulTerms && !mediaTextOverlaps(itemText, queryTerms) {
			continue
		}
		urlKey := canonicalMediaURL(item.URL)
		entryKey := titleText + "\x00" + normalizeMediaText(item.Source)
		if _, ok := seenURLs[urlKey]; ok {
			continue
		}
		if entryKey != "\x00" {
			if _, ok := seenEntries[entryKey]; ok {
				continue
			}
			seenEntries[entryKey] = struct{}{}
		}
		seenURLs[urlKey] = struct{}{}
		score := mediaRelevanceScore(item, queryTerms, queryPhrase)
		items = append(items, scoredMediaItem{item: item, score: score, index: rowIndex})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].index < items[j].index
	})
	truncated := len(items) > count
	if len(items) > count {
		items = items[:count]
	}
	out := make([]mediaSearchItem, 0, len(items))
	for _, scored := range items {
		out = append(out, scored.item)
	}
	return out, truncated, nil
}

// parseMediaSearchDuration parses the compact duration forms emitted by
// SearXNG (for example "40:17" or "1:02:03") and common unit-suffixed
// fallbacks. It deliberately returns unknown for free-form text so a missing
// provider field does not make an otherwise useful result disappear.
func parseMediaSearchDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, false
	}
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return 0, false
		}
		values := make([]float64, len(parts))
		for index, part := range parts {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil || parsed < 0 {
				return 0, false
			}
			values[index] = parsed
		}
		if (len(values) == 2 && values[1] >= 60) || (len(values) == 3 && (values[1] >= 60 || values[2] >= 60)) {
			return 0, false
		}
		seconds := values[len(values)-1]
		if len(values) == 3 {
			seconds += values[0] * 3600
			seconds += values[1] * 60
		} else {
			seconds += values[0] * 60
		}
		return durationFromSeconds(seconds)
	}

	for _, suffix := range []struct {
		suffix string
		factor float64
	}{
		{"hours", 3600}, {"hour", 3600}, {"h", 3600},
		{"minutes", 60}, {"minute", 60}, {"mins", 60}, {"min", 60}, {"m", 60},
		{"seconds", 1}, {"second", 1}, {"secs", 1}, {"sec", 1}, {"s", 1},
	} {
		if strings.HasSuffix(value, suffix.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(value, suffix.suffix))
			parsed, err := strconv.ParseFloat(number, 64)
			if err != nil || parsed < 0 {
				return 0, false
			}
			return durationFromSeconds(parsed * suffix.factor)
		}
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return durationFromSeconds(parsed)
}

func durationFromSeconds(seconds float64) (time.Duration, bool) {
	if seconds < 0 || seconds > float64((1<<63-1)/int64(time.Second)) {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

var mediaSearchStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "for": {}, "in": {}, "of": {}, "on": {}, "or": {}, "the": {}, "to": {}, "with": {},
}

func normalizeMediaText(value string) string {
	value = strings.ToLower(stripPrivateUseMarkers(value))
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func mediaSearchTerms(query string) ([]string, string) {
	normalized := normalizeMediaText(query)
	terms := make([]string, 0)
	seen := make(map[string]struct{})
	for _, term := range strings.Fields(normalized) {
		if len(term) < 2 {
			continue
		}
		if _, stop := mediaSearchStopWords[term]; stop {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms, normalized
}

func mediaTextOverlaps(text string, terms []string) bool {
	tokens := mediaTextTokens(text)
	for _, term := range terms {
		if _, ok := tokens[term]; ok {
			return true
		}
	}
	return false
}

func mediaRelevanceScore(item mediaSearchItem, terms []string, phrase string) int {
	title := normalizeMediaText(item.Title)
	text := normalizeMediaText(strings.Join([]string{item.Title, item.Snippet, item.Source}, " "))
	titleTokens := mediaTextTokens(title)
	textTokens := mediaTextTokens(text)
	score := 0
	if phrase != "" && strings.Contains(text, phrase) {
		score += 40
	}
	if phrase != "" && strings.Contains(title, phrase) {
		score += 60
	}
	for _, term := range terms {
		if _, ok := titleTokens[term]; ok {
			score += 12
		} else if _, ok := textTokens[term]; ok {
			score += 3
		}
	}
	return score
}

func mediaTextTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range strings.Fields(normalizeMediaText(value)) {
		tokens[token] = struct{}{}
	}
	return tokens
}

func canonicalMediaURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	parsed.Fragment = ""
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

type MediaSearchTool struct{ Service *MediaSearchService }

func (tool MediaSearchTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "media_search", Description: "Search configured image or video sources. Returns bounded, model-safe descriptions only. For explicit show/display/attach/send/retrieve/fetch requests, the web gateway may already attach only the top-ranked result as a bounded Bee artifact; describe that attachment rather than emitting Markdown image links. If the user later names or ranks a candidate from this result, call media_retrieve with only its rank. Never retrieve arbitrary model-supplied URLs.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["image","video"]},"query":{"type":"string"},"count":{"type":"integer","minimum":1,"maximum":12}},"required":["kind","query"]}`)}
}

func (tool MediaSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var req MediaSearchRequest
	if err := parseArgs(args, &req); err != nil {
		return "", err
	}
	if tool.Service == nil {
		return "", errors.New("media search service is not configured")
	}
	sessionID, ok := mediaSearchSessionFromContext(ctx)
	if !ok || strings.TrimSpace(string(sessionID)) == "" {
		return "", errors.New("media_search requires a durable session context")
	}
	result, err := tool.Service.Search(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	if retrieve, ok := mediaSearchAutoRetrieverFromContext(ctx); ok && retrieve != nil && len(result.Candidates) > 0 {
		attached := retrieve(ctx, result)
		result.Attached = attached.Attached
		result.AttachmentCount = boundedMediaAttachmentCount(attached.AttachmentCount)
		result.AutoRetrieveFailed = attached.Failed
		result.AlreadyAttached = attached.AlreadyAttached
		result.NoAdditionalRetrieval = attached.NoAdditionalRetrieval
		result.ConsumedAttempts = attached.ConsumedAttempts
	}
	emitMediaSearchRunEvent(ctx, result.runEvent())
	data, err := json.Marshal(result.modelSafe())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type mediaSearchSessionContextKey struct{}

func withMediaSearchSession(ctx context.Context, sessionID SessionID) context.Context {
	return context.WithValue(ctx, mediaSearchSessionContextKey{}, sessionID)
}
func mediaSearchSessionFromContext(ctx context.Context) (SessionID, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(mediaSearchSessionContextKey{}).(SessionID)
	return value, ok
}

type mediaSearchRunEmitterContextKey struct{}

type mediaSearchAutoRetrieverContextKey struct{}

type mediaSearchAutoRetrieverState struct {
	mu        sync.Mutex
	attempted bool
	result    MediaSearchAutoRetrieveResult
}

func withMediaSearchAutoRetriever(ctx context.Context, retrieve func(context.Context, MediaSearchResult) MediaSearchAutoRetrieveResult) context.Context {
	if retrieve == nil {
		return ctx
	}
	state := &mediaSearchAutoRetrieverState{}
	wrapper := func(retrieveCtx context.Context, result MediaSearchResult) MediaSearchAutoRetrieveResult {
		state.mu.Lock()
		if state.attempted {
			first := state.result
			state.mu.Unlock()
			return MediaSearchAutoRetrieveResult{AttachmentCount: first.AttachmentCount, Failed: first.Failed, AlreadyAttached: first.Attached, NoAdditionalRetrieval: true, ConsumedAttempts: first.ConsumedAttempts}
		}
		state.attempted = true
		state.mu.Unlock()
		first := retrieve(retrieveCtx, result)
		state.mu.Lock()
		state.result = first
		state.mu.Unlock()
		return first
	}
	return context.WithValue(ctx, mediaSearchAutoRetrieverContextKey{}, wrapper)
}

func mediaSearchAutoRetrieverFromContext(ctx context.Context) (func(context.Context, MediaSearchResult) MediaSearchAutoRetrieveResult, bool) {
	if ctx == nil {
		return nil, false
	}
	retrieve, ok := ctx.Value(mediaSearchAutoRetrieverContextKey{}).(func(context.Context, MediaSearchResult) MediaSearchAutoRetrieveResult)
	return retrieve, ok
}

func withMediaSearchRunEmitter(ctx context.Context, emit func(MediaSearchEvent)) context.Context {
	return context.WithValue(ctx, mediaSearchRunEmitterContextKey{}, emit)
}

func emitMediaSearchRunEvent(ctx context.Context, event MediaSearchEvent) {
	if ctx == nil {
		return
	}
	if emit, ok := ctx.Value(mediaSearchRunEmitterContextKey{}).(func(MediaSearchEvent)); ok && emit != nil {
		emit(event)
	}
}
