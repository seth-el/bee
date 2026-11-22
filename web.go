package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultWebSearchEndpoint        = "https://example.com/search"
	DefaultWebTimeout               = 15 * time.Second
	DefaultWebTimeZone              = "UTC"
	DefaultWebMaxResults            = 10
	DefaultGatewayMaxConcurrentRuns = 4
	MaxGatewayConcurrentRuns        = 64
	MaxWebQueryBytes                = 2048
	MaxWebURLBytes                  = 4096
	MaxWebResponseBytes             = 4 << 20
	DefaultWebFetchChars            = 64 << 10
	MaxWebFetchChars                = 128 << 10
	MaxWebFetchOffset               = MaxWebResponseBytes
	MaxWebToolOutputBytes           = 1 << 20
	MaxWebErrorBytes                = 8 << 10
	MaxWebResultCount               = 50
	MaxWebRedirects                 = 5
	MaxGitLabMetadataBytes          = 256 << 10
	MaxGitLabTreeBytes              = 1 << 20
	MaxGitLabTreeEntries            = 100
	MaxGitLabProjectPathBytes       = 2048
	MaxGitLabRepositoryPathBytes    = 4096
)

// BrowserWebConfig opts Bee into the explicitly configured browser control
// service. Omitted/disabled browser configuration is fail-closed.
type BrowserWebConfig struct {
	Enabled        bool   `json:"enabled,omitempty"`
	CamofoxURL     string `json:"camofox_url,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	AccessKeyFile  string `json:"access_key_file,omitempty"`
}

func (c BrowserWebConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	camofoxURL := c.CamofoxURL
	if strings.TrimSpace(camofoxURL) == "" {
		camofoxURL = DefaultCamofoxURL
	}
	if _, err := validateCamofoxControlURL(camofoxURL); err != nil {
		return fmt.Errorf("web browser camofox_url: %w", err)
	}
	if strings.TrimSpace(c.AccessKeyFile) != "" {
		if err := validatePrivateSecretFile(c.AccessKeyFile); err != nil {
			return fmt.Errorf("web browser access_key_file: %w", err)
		}
		parsed, _ := validateCamofoxControlURL(camofoxURL)
		if parsed != nil && !isLoopbackHost(parsed.Hostname()) && parsed.Scheme != "https" {
			return errors.New("web browser access_key_file requires HTTPS off loopback")
		}
	} else if parsed, _ := validateCamofoxControlURL(camofoxURL); parsed != nil && !isLoopbackHost(parsed.Hostname()) {
		return errors.New("web browser camofox_url must be loopback when access_key_file is not configured")
	}
	timeout := c.TimeoutSeconds
	if timeout == 0 {
		timeout = int(DefaultWebTimeout / time.Second)
	}
	if timeout < 1 {
		return errors.New("web browser timeout_seconds must be at least 1")
	}
	return nil
}

type WebConfig struct {
	Enabled            bool   `json:"enabled,omitempty"`
	MediaSearchEnabled bool   `json:"media_search_enabled,omitempty"`
	SearchEndpoint     string `json:"search_endpoint,omitempty"`
	TimeoutSeconds     int    `json:"timeout_seconds,omitempty"`
	MaxResults         int    `json:"max_results,omitempty"`
	// UserAvatarFile and BeeAvatarFile are optional local image files loaded by
	// the web gateway at startup and exposed only through fixed avatar routes.
	// Empty values keep the safe built-in fallback (user initials / Bee SVG).
	UserAvatarFile string `json:"user_avatar_file,omitempty"`
	BeeAvatarFile  string `json:"bee_avatar_file,omitempty"`
	// TimeZone is an IANA time zone identifier used for web message timestamps.
	// A named zone such as Europe/Berlin follows its daylight-saving rules.
	TimeZone string `json:"time_zone,omitempty"`
	// MaxConcurrentRuns bounds active web-gateway turns across all sessions.
	// Zero selects the conservative default rather than disabling the bound.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// MaxChars is retained for configuration/source compatibility only. It is
	// Model-facing fetches use a finite default window and a hard maximum.
	MaxChars int `json:"max_chars,omitempty"`
	// AllowPrivateTargets is tri-state so omitted configuration preserves the
	// network capability parity of the operator's configured web client: private
	// and link-local destinations are allowed by default, while an explicit
	// false disables them.
	AllowPrivateTargets *bool                  `json:"allow_private_targets,omitempty"`
	Browser             BrowserWebConfig       `json:"browser,omitempty"`
	Scout               WebResearchScoutConfig `json:"scout,omitempty"`
}

func DefaultWebConfig() WebConfig {
	return WebConfig{SearchEndpoint: DefaultWebSearchEndpoint, TimeoutSeconds: int(DefaultWebTimeout / time.Second), MaxResults: DefaultWebMaxResults, TimeZone: DefaultWebTimeZone, MaxConcurrentRuns: DefaultGatewayMaxConcurrentRuns, Scout: DefaultWebResearchScoutConfig()}
}

func (c WebConfig) EffectiveMaxConcurrentRuns() int {
	if c.MaxConcurrentRuns == 0 {
		return DefaultGatewayMaxConcurrentRuns
	}
	return c.MaxConcurrentRuns
}

// EffectiveTimeZone returns the configured IANA zone, or UTC when it is
// omitted or blank. Configure a named zone rather than a fixed offset so
// daylight-saving transitions are rendered correctly.
func (c WebConfig) EffectiveTimeZone() string {
	zone := strings.TrimSpace(c.TimeZone)
	if zone == "" {
		return DefaultWebTimeZone
	}
	return zone
}

func (c WebConfig) Validate() error {
	if maxConcurrent := c.EffectiveMaxConcurrentRuns(); maxConcurrent < 1 || maxConcurrent > MaxGatewayConcurrentRuns {
		return fmt.Errorf("web max_concurrent_runs must be between 1 and %d", MaxGatewayConcurrentRuns)
	}
	zone := strings.TrimSpace(c.TimeZone)
	if len(zone) > 128 {
		return errors.New("web time_zone must be 128 characters or fewer")
	}
	if strings.IndexFunc(zone, unicode.IsControl) >= 0 {
		return errors.New("web time_zone contains a control character")
	}
	if _, err := time.LoadLocation(c.EffectiveTimeZone()); err != nil {
		return fmt.Errorf("web time_zone: %w", err)
	}
	if err := validateUIAvatarFile("user_avatar_file", c.UserAvatarFile); err != nil {
		return err
	}
	if err := validateUIAvatarFile("bee_avatar_file", c.BeeAvatarFile); err != nil {
		return err
	}
	if c.Browser.Enabled && !c.Enabled {
		return errors.New("web browser requires web.enabled")
	}
	if err := c.Browser.Validate(); err != nil {
		return err
	}
	if err := c.Scout.Validate(); err != nil {
		return err
	}
	if c.Scout.Enabled && !c.Enabled {
		return errors.New("web scout requires web.enabled")
	}
	if !c.Enabled && !c.MediaSearchEnabled {
		return nil
	}
	endpoint := c.SearchEndpoint
	if endpoint == "" {
		endpoint = DefaultWebSearchEndpoint
	}
	if _, err := validateWebURL(endpoint, true); err != nil {
		return fmt.Errorf("web search endpoint: %w", err)
	}
	timeout := c.TimeoutSeconds
	if timeout == 0 {
		timeout = int(DefaultWebTimeout / time.Second)
	}
	if timeout < 1 || timeout > 120 {
		return errors.New("web timeout_seconds must be between 1 and 120")
	}
	maxResults := c.MaxResults
	if maxResults == 0 {
		maxResults = DefaultWebMaxResults
	}
	if maxResults < 1 || maxResults > MaxWebResultCount {
		return fmt.Errorf("web max_results must be between 1 and %d", MaxWebResultCount)
	}
	return nil
}

func validateUIAvatarFile(name, file string) error {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}
	if strings.ContainsAny(file, "\x00\r\n") {
		return fmt.Errorf("web %s contains a control character", name)
	}
	return nil
}

type WebSearchRequest struct {
	Query    string
	Count    int
	Language string
}
type WebSearchHit struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet,omitempty"`
	Untrusted bool   `json:"untrusted"`
}
type WebSearchResult struct {
	Query               string         `json:"query"`
	Results             []WebSearchHit `json:"results"`
	Degraded            bool           `json:"degraded"`
	Retryable           bool           `json:"retryable"`
	Note                string         `json:"note,omitempty"`
	UnresponsiveEngines []string       `json:"unresponsive_engines,omitempty"`
	Untrusted           bool           `json:"untrusted"`
	Truncated           bool           `json:"truncated,omitempty"`
}
type WebFetchRequest struct {
	URL string `json:"url"`
	// ExtractMode is accepted for legacy callers; Bee currently returns text.
	ExtractMode string `json:"extract_mode,omitempty"`
	Offset      int    `json:"offset,omitempty"`
	MaxChars    int    `json:"max_chars,omitempty"`
}
type WebFetchResult struct {
	RequestedURL string `json:"requested_url"`
	FinalURL     string `json:"final_url"`
	Title        string `json:"title,omitempty"`
	Content      string `json:"content"`
	ContentType  string `json:"content_type"`
	Offset       int    `json:"offset"`
	TotalChars   int    `json:"total_chars"`
	HasMore      bool   `json:"has_more"`
	NextOffset   *int   `json:"next_offset,omitempty"`
	Untrusted    bool   `json:"untrusted"`
	Truncated    bool   `json:"truncated,omitempty"`
	Note         string `json:"note,omitempty"`
}

type WebClient struct {
	SearchEndpoint string
	HTTPClient     *http.Client
	Timeout        time.Duration
	MaxResults     int
	// MaxChars is retained for source compatibility only.
	MaxChars int
	// AllowPrivateTargets defaults to true for clients created by
	// NewWebClient. Callers may set it to false for a narrower client policy.
	AllowPrivateTargets bool
	// LookupIPAddr is an injectable resolver used by runtime URL validation and
	// the pinned direct dialer. Nil uses net.DefaultResolver.
	LookupIPAddr    func(context.Context, string) ([]net.IPAddr, error)
	Browser         BrowserBackend
	searchCircuitMu sync.Mutex
	searchCircuit   map[string]searchCircuitEntry
	searchInFlight  map[string]chan struct{}
	// searchNow is an injectable clock for the search pause; nil uses time.Now.
	searchNow func() time.Time
}

func NewWebClient(config WebConfig) (*WebClient, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.SearchEndpoint == "" {
		config.SearchEndpoint = DefaultWebSearchEndpoint
	}
	if config.TimeoutSeconds == 0 {
		config.TimeoutSeconds = int(DefaultWebTimeout / time.Second)
	}
	if config.MaxResults == 0 {
		config.MaxResults = DefaultWebMaxResults
	}
	timeout := time.Duration(config.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultWebTimeout
	}
	allowPrivateTargets := true
	if config.AllowPrivateTargets != nil {
		allowPrivateTargets = *config.AllowPrivateTargets
	}
	client := &WebClient{SearchEndpoint: strings.TrimRight(config.SearchEndpoint, "/"), HTTPClient: &http.Client{Timeout: timeout}, Timeout: timeout, MaxResults: config.MaxResults, AllowPrivateTargets: allowPrivateTargets}
	if config.Browser.Enabled {
		baseURL := strings.TrimSpace(config.Browser.CamofoxURL)
		if baseURL == "" {
			baseURL = DefaultCamofoxURL
		}
		browserTimeout := time.Duration(config.Browser.TimeoutSeconds) * time.Second
		if browserTimeout <= 0 {
			browserTimeout = DefaultWebTimeout
		}
		// The browser control plane has its own transport and timeout. Never
		// reuse the general web client's transport, whose timeout and redirect
		// policy belong to public web observations.
		browser, err := NewBrowserCamofoxClientWithAccessKeyFile(baseURL, nil, browserTimeout, config.Browser.AccessKeyFile)
		if err != nil {
			return nil, err
		}
		client.Browser = browser
	}
	return client, nil
}
func (c *WebClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: c.Timeout}
}

const degradedSearchNote = "Search backend returned no results and engine failures. Do not repeatedly reformulate or retry web_search; it is paused for this run for a couple of minutes. Meanwhile use a known-URL web_fetch/browser lookup, or report that search is unavailable."

// After a failed search, web_search pauses for the rest of the run's calls
// until the pause ends; then one attempt goes through. Each further failure
// doubles the pause, up to searchPauseMax; a success clears it. This keeps a
// model from hammering a rate-limited backend (which prolongs the rate limit)
// without costing it search for a whole long run after one blip.
const (
	searchPauseBase = 2 * time.Minute
	searchPauseMax  = 15 * time.Minute
	// searchCircuitIdle drops a run's pause state after this long unused.
	searchCircuitIdle = 30 * time.Minute
)

type searchCircuitEntry struct {
	at    time.Time
	until time.Time
	trips int
}

func searchPause(trips int) time.Duration {
	pause := searchPauseBase
	for i := 1; i < trips && pause < searchPauseMax; i++ {
		pause *= 2
	}
	return min(pause, searchPauseMax)
}

func (c *WebClient) clock() time.Time {
	if c.searchNow != nil {
		return c.searchNow().UTC()
	}
	return time.Now().UTC()
}

func searchPausedNote(remaining time.Duration) string {
	minutes := int((remaining + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("Search backend failed recently; web_search is paused for this run for about %d more %s. Do not retry with changed queries before then; use a known-URL web_fetch/browser lookup or report that search is unavailable.", minutes, unit)
}

func (c *WebClient) beginSearch(runID string, now time.Time) (open bool, remaining time.Duration, wait <-chan struct{}, leader bool) {
	if strings.TrimSpace(runID) == "" {
		return false, 0, nil, true
	}
	c.searchCircuitMu.Lock()
	defer c.searchCircuitMu.Unlock()
	if c.searchCircuit == nil {
		c.searchCircuit = make(map[string]searchCircuitEntry)
	}
	for key, entry := range c.searchCircuit {
		if now.Sub(entry.at) > searchCircuitIdle {
			delete(c.searchCircuit, key)
		}
	}
	if entry, ok := c.searchCircuit[runID]; ok && now.Before(entry.until) {
		return true, entry.until.Sub(now), nil, false
	}
	if c.searchInFlight == nil {
		c.searchInFlight = make(map[string]chan struct{})
	}
	if channel, ok := c.searchInFlight[runID]; ok {
		return false, 0, channel, false
	}
	c.searchInFlight[runID] = make(chan struct{})
	return false, 0, nil, true
}

func (c *WebClient) finishSearch(runID string, degraded bool, now time.Time) {
	if strings.TrimSpace(runID) == "" {
		return
	}
	c.searchCircuitMu.Lock()
	defer c.searchCircuitMu.Unlock()
	if !degraded {
		delete(c.searchCircuit, runID)
	} else {
		if c.searchCircuit == nil {
			c.searchCircuit = make(map[string]searchCircuitEntry)
		}
		trips := c.searchCircuit[runID].trips + 1
		c.searchCircuit[runID] = searchCircuitEntry{at: now, until: now.Add(searchPause(trips)), trips: trips}
		for len(c.searchCircuit) > 256 {
			var oldestKey string
			var oldest time.Time
			for key, entry := range c.searchCircuit {
				if oldestKey == "" || entry.at.Before(oldest) {
					oldestKey, oldest = key, entry.at
				}
			}
			if oldestKey == "" {
				break
			}
			delete(c.searchCircuit, oldestKey)
		}
	}
	if c.searchInFlight != nil {
		if channel, ok := c.searchInFlight[runID]; ok {
			delete(c.searchInFlight, runID)
			close(channel)
		}
	}
}

func parseUnresponsiveEngines(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		var text string
		switch item := value.(type) {
		case string:
			text = item
		case []any:
			parts := make([]string, 0, len(item))
			for _, part := range item {
				if s, ok := part.(string); ok {
					parts = append(parts, s)
				}
			}
			text = strings.Join(parts, ": ")
		}
		text = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, text))
		if text == "" || len(text) > 256 || seen[text] || len(result) >= 32 {
			continue
		}
		seen[text] = true
		result = append(result, text)
	}
	return result
}

func (c *WebClient) Search(ctx context.Context, req WebSearchRequest) (WebSearchResult, error) {
	q := strings.TrimSpace(req.Query)
	if q == "" || len(q) > MaxWebQueryBytes {
		return WebSearchResult{}, errors.New("web search query is empty or too large")
	}
	runID, _ := RunIDFromContext(ctx)
	for {
		open, remaining, wait, leader := c.beginSearch(runID, c.clock())
		if open {
			return WebSearchResult{Results: []WebSearchHit{}, Degraded: true, Retryable: false, Note: searchPausedNote(remaining), Untrusted: true}, nil
		}
		if leader {
			break
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return WebSearchResult{}, ctx.Err()
		}
	}
	searchDegraded := true
	defer func() { c.finishSearch(runID, searchDegraded, c.clock()) }()
	count := req.Count
	if count <= 0 {
		count = c.MaxResults
	}
	if count <= 0 {
		count = DefaultWebMaxResults
	}
	if count > MaxWebResultCount {
		count = MaxWebResultCount
	}
	// The search endpoint is an explicit operator-configured service and may be
	// private (for example, a local SearXNG ingress). Redirects and result URLs
	// remain subject to the public-target policy.
	base, err := validateWebURL(c.SearchEndpoint, true)
	if err != nil {
		return WebSearchResult{}, err
	}
	u := base
	values := u.Query()
	values.Set("q", q)
	values.Set("format", "json")
	if req.Language != "" {
		values.Set("language", req.Language)
	}
	u.RawQuery = values.Encode()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	data, final, contentType, err := c.do(ctx, u)
	if err != nil {
		return WebSearchResult{}, err
	}
	_ = final
	if !strings.Contains(contentType, "json") {
		return WebSearchResult{}, errors.New("web search endpoint did not return JSON")
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		Unresponsive json.RawMessage `json:"unresponsive_engines"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return WebSearchResult{}, fmt.Errorf("decode web search response: %w", err)
	}
	out := WebSearchResult{Query: q, Results: []WebSearchHit{}, Retryable: true, Untrusted: true, UnresponsiveEngines: parseUnresponsiveEngines(payload.Unresponsive)}
	for _, item := range payload.Results {
		if item.URL == "" {
			continue
		}
		hitURL, err := c.validateURL(ctx, item.URL, c.AllowPrivateTargets)
		if err != nil {
			continue
		}
		snippet := item.Content
		if snippet == "" {
			snippet = item.Snippet
		}
		out.Results = append(out.Results, WebSearchHit{Title: truncateWeb(item.Title, 512), URL: hitURL.String(), Snippet: truncateWeb(snippet, 2048), Untrusted: true})
		if len(out.Results) >= count {
			out.Truncated = len(payload.Results) > count
			break
		}
	}
	if len(out.Results) == 0 && len(out.UnresponsiveEngines) > 0 {
		out.Degraded = true
		out.Retryable = false
		out.Note = degradedSearchNote
		searchDegraded = true
	} else {
		searchDegraded = false
	}
	return out, nil
}
func (c *WebClient) Fetch(ctx context.Context, req WebFetchRequest) (WebFetchResult, error) {
	switch strings.ToLower(strings.TrimSpace(req.ExtractMode)) {
	case "", "text":
	default:
		return WebFetchResult{}, errors.New("web fetch extract_mode supports only text")
	}
	if req.Offset < 0 {
		return WebFetchResult{}, errors.New("web fetch offset must be non-negative")
	}
	if req.Offset > MaxWebFetchOffset {
		return WebFetchResult{}, fmt.Errorf("web fetch offset must be between 0 and %d", MaxWebFetchOffset)
	}
	if req.MaxChars < 0 || req.MaxChars > MaxWebFetchChars {
		return WebFetchResult{}, fmt.Errorf("web fetch max_chars must be between 0 and %d", MaxWebFetchChars)
	}
	u, err := c.validateURL(ctx, req.URL, c.AllowPrivateTargets)
	if err != nil {
		return WebFetchResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	data, final, contentType, err := c.doFetch(ctx, u)
	if err != nil {
		return WebFetchResult{}, err
	}
	title, content := extractWebContent(data, contentType)
	if isGitLabShell(u, final, data, contentType, content) {
		if result, handled, err := c.fetchGitLab(ctx, u, final); handled {
			if err != nil {
				return WebFetchResult{}, err
			}
			return windowWebFetchResult(result, req.Offset, req.MaxChars), nil
		}
	}
	if !strings.HasPrefix(contentType, "text/") && !strings.Contains(contentType, "html") && !strings.Contains(contentType, "json") {
		return WebFetchResult{}, fmt.Errorf("unsupported web content type %q", contentType)
	}
	return windowWebFetchResult(WebFetchResult{RequestedURL: u.String(), FinalURL: final.String(), Title: title, Content: content, ContentType: normalizeWebContentType(contentType), Untrusted: true}, req.Offset, req.MaxChars), nil
}
func (c *WebClient) do(ctx context.Context, u *url.URL) ([]byte, *url.URL, string, error) {
	return c.doLimitedForHost(ctx, u, MaxWebResponseBytes, webURLAuthority(u))
}

func (c *WebClient) doFetch(ctx context.Context, u *url.URL) ([]byte, *url.URL, string, error) {
	return c.doLimited(ctx, u, MaxWebResponseBytes)
}

func normalizeWebContentType(value string) string {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || mediaType == "" {
		return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	}
	mediaType = strings.ToLower(mediaType)
	if charset := strings.TrimSpace(params["charset"]); charset != "" {
		return mediaType + "; charset=" + strings.ToLower(charset)
	}
	return mediaType
}

func windowWebFetchResult(result WebFetchResult, offset, requested int) WebFetchResult {
	runes := []rune(result.Content)
	if offset < 0 {
		offset = 0
	}
	if offset > len(runes) {
		offset = len(runes)
	}
	limit := requested
	if limit <= 0 {
		limit = DefaultWebFetchChars
	}
	if limit > MaxWebFetchChars {
		limit = MaxWebFetchChars
	}
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	fullLength := len(runes)
	result.Offset = offset
	result.TotalChars = fullLength
	result.Content = string(runes[offset:end])
	result.HasMore = end < fullLength
	result.Truncated = result.HasMore
	if result.HasMore {
		next := end
		result.NextOffset = &next
		result.Note = fmt.Sprintf("Content is windowed. Continue with offset=%d and the same url; do not repeat unchanged arguments.", end)
	} else {
		result.NextOffset = nil
		result.Note = "Content is complete within the finite fetch window."
	}
	return result
}

type webHTTPStatusError struct {
	status int
}

func (e *webHTTPStatusError) Error() string {
	return fmt.Sprintf("web request returned HTTP %d", e.status)
}

func (c *WebClient) doLimited(ctx context.Context, u *url.URL, maxBytes int) ([]byte, *url.URL, string, error) {
	return c.doLimitedForHost(ctx, u, maxBytes, "")
}

func (c *WebClient) doLimitedForHost(ctx context.Context, u *url.URL, maxBytes int, allowedPrivateAuthority string) ([]byte, *url.URL, string, error) {
	if maxBytes > MaxWebResponseBytes {
		maxBytes = MaxWebResponseBytes
	}
	client, err := c.requestClient(allowedPrivateAuthority)
	if err != nil {
		return nil, nil, "", err
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxWebRedirects {
			return errors.New("web redirect limit exceeded")
		}
		allowPrivate := c.AllowPrivateTargets || allowedPrivateAuthority != "" && strings.EqualFold(webURLAuthority(req.URL), allowedPrivateAuthority)
		if _, err := c.validateURL(req.Context(), req.URL.String(), allowPrivate); err != nil {
			return err
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, "", err
	}
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9")
	req.Header.Set("User-Agent", "Bee/1.0 (read-only research)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, "", fmt.Errorf("web request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, "", &webHTTPStatusError{status: resp.StatusCode}
	}
	var data []byte
	if maxBytes <= 0 {
		data, err = io.ReadAll(resp.Body)
	} else {
		data, err = io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	}
	if err != nil {
		return nil, nil, "", err
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return nil, nil, "", fmt.Errorf("web response exceeds %d-byte size bound", maxBytes)
	}
	return data, resp.Request.URL, normalizeWebContentType(resp.Header.Get("Content-Type")), nil
}

func (c *WebClient) lookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if c != nil && c.LookupIPAddr != nil {
		return c.LookupIPAddr(ctx, host)
	}
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func (c *WebClient) validateURL(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
	return validateWebURLWithResolver(ctx, raw, allowPrivate, c.lookupIPAddr)
}

// requestClient pins each public-only connection to the same vetted IP that
// was resolved immediately before dialing. Proxies are disabled in this mode
// because a proxy would resolve the target independently and reopen DNS
// rebinding/private-network access.
func (c *WebClient) requestClient(allowedPrivateAuthority string) (*http.Client, error) {
	base := c.client()
	client := *base
	if c.AllowPrivateTargets {
		return &client, nil
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		return nil, errors.New("public-only web policy requires an HTTP transport")
	}
	clone := httpTransport.Clone()
	clone.Proxy = nil
	clone.DialTLSContext = nil
	dial := clone.DialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: c.Timeout}
		dial = dialer.DialContext
	}
	clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split web dial address: %w", err)
		}
		var addresses []net.IPAddr
		if literal := net.ParseIP(host); literal != nil {
			addresses = []net.IPAddr{{IP: literal}}
		} else {
			addresses, err = c.lookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve web host: %w", err)
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("resolve web host: no addresses")
		}
		allowPrivate := allowedPrivateAuthority != "" && strings.EqualFold(net.JoinHostPort(strings.ToLower(host), port), allowedPrivateAuthority)
		for _, address := range addresses {
			if address.IP == nil || !allowPrivate && isPrivateWebIP(address.IP) {
				return nil, errors.New("private/link-local web targets are disabled")
			}
		}
		var lastErr error
		for _, resolved := range addresses {
			connection, err := dial(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if err == nil {
				return connection, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	client.Transport = clone
	return &client, nil
}

func webURLAuthority(u *url.URL) string {
	if u == nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if port == "" || u.Hostname() == "" {
		return ""
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

type gitLabTarget struct {
	kind        string
	projectPath string
	ref         string
	filePath    string
}

type gitLabProject struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	Namespace         struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
}

type gitLabTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

func isGitLabShell(requestURL, finalURL *url.URL, data []byte, contentType, content string) bool {
	if !strings.Contains(strings.ToLower(contentType), "html") {
		return false
	}
	hostLooksGitLab := strings.Contains(strings.ToLower(requestURL.Hostname()), "gitlab") || strings.Contains(strings.ToLower(finalURL.Hostname()), "gitlab")
	lower := strings.ToLower(string(data))
	shellMarkers := strings.Contains(lower, "gitlab") && (strings.Contains(lower, "id=\"app\"") || strings.Contains(lower, "id='app'") || strings.Contains(lower, "data-page") || strings.Contains(lower, "__initial_state__") || strings.Contains(lower, "gitlab-ui"))
	return (hostLooksGitLab || shellMarkers) && len(content) <= 2048
}

func (c *WebClient) fetchGitLab(ctx context.Context, requestURL, finalURL *url.URL) (WebFetchResult, bool, error) {
	target, err := parseGitLabTarget(requestURL)
	if err != nil {
		return WebFetchResult{}, false, nil
	}
	switch target.kind {
	case "blob":
		return c.fetchGitLabBlob(ctx, requestURL, target)
	case "project":
		return c.fetchGitLabProject(ctx, requestURL, finalURL, target)
	default:
		return WebFetchResult{}, false, nil
	}
}

func (c *WebClient) fetchGitLabProject(ctx context.Context, requestURL, finalURL *url.URL, target gitLabTarget) (WebFetchResult, bool, error) {
	project, err := c.resolveGitLabProject(ctx, requestURL, target.projectPath)
	if err != nil {
		return WebFetchResult{}, true, err
	}
	if project.ID <= 0 {
		return WebFetchResult{}, true, errors.New("GitLab project metadata did not include a valid numeric project id")
	}
	branch := project.DefaultBranch
	if err := validateGitLabRef(branch); err != nil {
		return WebFetchResult{}, true, fmt.Errorf("GitLab project default branch: %w", err)
	}

	projectID := strconv.FormatInt(project.ID, 10)
	treeURL := gitLabProjectAPIURL(requestURL, projectID, "repository", "tree")
	values := treeURL.Query()
	values.Set("recursive", "true")
	values.Set("per_page", strconv.Itoa(MaxGitLabTreeEntries))
	values.Set("page", "1")
	values.Set("ref", branch)
	treeURL.RawQuery = values.Encode()
	treeData, _, treeContentType, err := c.doLimited(ctx, treeURL, MaxGitLabTreeBytes)
	if err != nil {
		return WebFetchResult{}, true, fmt.Errorf("GitLab repository tree fetch: %w", err)
	}
	if !strings.Contains(strings.ToLower(treeContentType), "json") {
		return WebFetchResult{}, true, fmt.Errorf("GitLab repository tree returned unsupported content type %q", treeContentType)
	}
	var entries []gitLabTreeEntry
	if err := json.Unmarshal(treeData, &entries); err != nil {
		return WebFetchResult{}, true, fmt.Errorf("decode GitLab repository tree: %w", err)
	}
	readmePath := ""
	validEntries := make([]gitLabTreeEntry, 0, MaxGitLabTreeEntries)
	for _, entry := range entries {
		if entry.Type != "blob" && entry.Type != "tree" {
			continue
		}
		if err := validateGitLabRepositoryPath(entry.Path); err != nil {
			continue
		}
		if len(validEntries) < MaxGitLabTreeEntries {
			validEntries = append(validEntries, entry)
		}
		if entry.Type == "blob" && (strings.EqualFold(path.Base(entry.Path), "README.md") || strings.EqualFold(path.Base(entry.Path), "README")) {
			if readmePath == "" || !strings.Contains(entry.Path, "/") {
				readmePath = entry.Path
			}
		}
	}
	if readmePath == "" || strings.Contains(readmePath, "/") {
		rootReadmePath, rootErr := c.findGitLabRootReadme(ctx, requestURL, projectID, branch)
		if rootErr != nil {
			return WebFetchResult{}, true, fmt.Errorf("GitLab root repository tree fetch: %w", rootErr)
		}
		if rootReadmePath != "" {
			readmePath = rootReadmePath
		}
	}

	readme := ""
	if readmePath != "" {
		readmeURL := gitLabProjectAPIURL(requestURL, projectID, "repository", "files", readmePath, "raw")
		readmeValues := readmeURL.Query()
		readmeValues.Set("ref", branch)
		readmeURL.RawQuery = readmeValues.Encode()
		readmeData, _, readmeContentType, readmeErr := c.doFetch(ctx, readmeURL)
		if readmeErr != nil {
			var statusErr *webHTTPStatusError
			if !errors.As(readmeErr, &statusErr) || statusErr.status != http.StatusNotFound {
				return WebFetchResult{}, true, fmt.Errorf("GitLab README fetch: %w", readmeErr)
			}
		} else if !isGitLabReadableContentType(readmeContentType) {
			return WebFetchResult{}, true, fmt.Errorf("GitLab README returned unsupported content type %q", readmeContentType)
		} else {
			readme = strings.TrimSpace(string(readmeData))
		}
	}

	title := project.Name
	if title == "" {
		title = project.PathWithNamespace
	}
	if title == "" {
		title = target.projectPath
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Project: %s\nDefault branch: %s\nTree:\n", title, branch)
	for _, entry := range validEntries {
		fmt.Fprintf(&out, "- %s (%s)\n", entry.Path, entry.Type)
	}
	if len(entries) > len(validEntries) {
		out.WriteString("- [tree listing bounded]\n")
	}
	if readme != "" {
		out.WriteString("\nREADME:\n")
		out.WriteString(readme)
		out.WriteByte('\n')
	}
	result := WebFetchResult{RequestedURL: requestURL.String(), FinalURL: finalURL.String(), Title: title, Content: out.String(), ContentType: "text/plain; charset=utf-8", Untrusted: true}
	return result, true, nil
}

func (c *WebClient) findGitLabRootReadme(ctx context.Context, requestURL *url.URL, projectID, branch string) (string, error) {
	treeURL := gitLabProjectAPIURL(requestURL, projectID, "repository", "tree")
	values := treeURL.Query()
	values.Set("recursive", "false")
	values.Set("per_page", strconv.Itoa(MaxGitLabTreeEntries))
	values.Set("page", "1")
	values.Set("ref", branch)
	treeURL.RawQuery = values.Encode()
	treeData, _, treeContentType, err := c.doLimited(ctx, treeURL, MaxGitLabTreeBytes)
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(treeContentType), "json") {
		return "", fmt.Errorf("returned unsupported content type %q", treeContentType)
	}
	var entries []gitLabTreeEntry
	if err := json.Unmarshal(treeData, &entries); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	for i, entry := range entries {
		if i >= MaxGitLabTreeEntries {
			break
		}
		if entry.Type != "blob" || strings.Contains(entry.Path, "/") {
			continue
		}
		if err := validateGitLabRepositoryPath(entry.Path); err != nil {
			continue
		}
		if strings.EqualFold(entry.Path, "README.md") || strings.EqualFold(entry.Path, "README") {
			return entry.Path, nil
		}
	}
	return "", nil
}

func (c *WebClient) resolveGitLabProject(ctx context.Context, requestURL *url.URL, projectPath string) (gitLabProject, error) {
	metadataURL := gitLabProjectAPIURL(requestURL, projectPath)
	data, _, contentType, err := c.doLimited(ctx, metadataURL, MaxGitLabMetadataBytes)
	if err == nil {
		if !strings.Contains(strings.ToLower(contentType), "json") {
			return gitLabProject{}, fmt.Errorf("GitLab project metadata returned unsupported content type %q", contentType)
		}
		var project gitLabProject
		if err := json.Unmarshal(data, &project); err != nil {
			return gitLabProject{}, fmt.Errorf("decode GitLab project metadata: %w", err)
		}
		return project, nil
	}

	var statusErr *webHTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.status != http.StatusBadRequest && statusErr.status != http.StatusNotFound {
		return gitLabProject{}, fmt.Errorf("GitLab project metadata fetch: %w", err)
	}

	searchURL := gitLabProjectsAPIURL(requestURL)
	values := searchURL.Query()
	values.Set("search", path.Base(projectPath))
	values.Set("per_page", strconv.Itoa(MaxGitLabTreeEntries))
	values.Set("page", "1")
	searchURL.RawQuery = values.Encode()
	searchData, _, searchContentType, searchErr := c.doLimited(ctx, searchURL, MaxGitLabMetadataBytes)
	if searchErr != nil {
		return gitLabProject{}, fmt.Errorf("GitLab project metadata fetch: %w; metadata search fallback: %w", err, searchErr)
	}
	if !strings.Contains(strings.ToLower(searchContentType), "json") {
		return gitLabProject{}, fmt.Errorf("GitLab project metadata fetch: %w; metadata search fallback returned unsupported content type %q", err, searchContentType)
	}
	var candidates []gitLabProject
	if searchErr := json.Unmarshal(searchData, &candidates); searchErr != nil {
		return gitLabProject{}, fmt.Errorf("GitLab project metadata fetch: %w; decode metadata search fallback: %w", err, searchErr)
	}
	project, ok := selectGitLabProject(candidates, projectPath)
	if !ok {
		return gitLabProject{}, fmt.Errorf("GitLab project metadata fetch: %w; metadata search fallback found no exact project %q", err, projectPath)
	}
	return project, nil
}

func selectGitLabProject(candidates []gitLabProject, projectPath string) (gitLabProject, bool) {
	for i, candidate := range candidates {
		if i >= MaxGitLabTreeEntries {
			break
		}
		if candidate.ID > 0 && candidate.PathWithNamespace == projectPath {
			return candidate, true
		}
	}

	// A path-only response is safe to use only when it identifies the complete
	// requested path and there is no namespace ambiguity. GitLab normally
	// supplies path_with_namespace, so this is intentionally narrow.
	if strings.Contains(projectPath, "/") {
		for i, candidate := range candidates {
			if i >= MaxGitLabTreeEntries {
				break
			}
			if candidate.ID > 0 && candidate.PathWithNamespace == "" && candidate.Namespace.FullPath != "" && candidate.Namespace.FullPath+"/"+candidate.Path == projectPath {
				return candidate, true
			}
		}
		return gitLabProject{}, false
	}

	var fallback gitLabProject
	found := false
	for i, candidate := range candidates {
		if i >= MaxGitLabTreeEntries {
			break
		}
		if candidate.ID <= 0 || candidate.PathWithNamespace != "" || candidate.Path != projectPath {
			continue
		}
		if found {
			return gitLabProject{}, false
		}
		fallback = candidate
		found = true
	}
	return fallback, found
}

func (c *WebClient) fetchGitLabBlob(ctx context.Context, requestURL *url.URL, target gitLabTarget) (WebFetchResult, bool, error) {
	rawURL := gitLabRawURL(requestURL, target)
	data, finalURL, contentType, err := c.doFetch(ctx, rawURL)
	if err != nil {
		return WebFetchResult{}, true, fmt.Errorf("GitLab raw file fetch: %w", err)
	}
	if !isGitLabReadableContentType(contentType) {
		return WebFetchResult{}, true, fmt.Errorf("GitLab raw file returned unsupported content type %q", contentType)
	}
	result := WebFetchResult{RequestedURL: requestURL.String(), FinalURL: finalURL.String(), Title: path.Base(target.filePath), Content: string(data), ContentType: contentType, Untrusted: true}
	return result, true, nil
}

func parseGitLabTarget(u *url.URL) (gitLabTarget, error) {
	escaped := strings.Trim(u.EscapedPath(), "/")
	if escaped == "" {
		return gitLabTarget{}, errors.New("GitLab URL does not identify a project")
	}
	if marker := strings.Index(escaped, "/-/blob/"); marker >= 0 {
		projectPath, err := decodeGitLabPath(escaped[:marker])
		if err != nil {
			return gitLabTarget{}, err
		}
		remainder := escaped[marker+len("/-/blob/"):]
		parts := strings.Split(remainder, "/")
		if len(parts) < 2 {
			return gitLabTarget{}, errors.New("GitLab blob URL must include a ref and file path")
		}
		ref, err := url.PathUnescape(parts[0])
		if err != nil {
			return gitLabTarget{}, fmt.Errorf("decode GitLab blob ref: %w", err)
		}
		filePath, err := decodeGitLabPath(strings.Join(parts[1:], "/"))
		if err != nil {
			return gitLabTarget{}, err
		}
		if err := validateGitLabRef(ref); err != nil {
			return gitLabTarget{}, err
		}
		if err := validateGitLabProjectPath(projectPath); err != nil {
			return gitLabTarget{}, fmt.Errorf("GitLab project path: %w", err)
		}
		if err := validateGitLabRepositoryPath(filePath); err != nil {
			return gitLabTarget{}, fmt.Errorf("GitLab blob file path: %w", err)
		}
		return gitLabTarget{kind: "blob", projectPath: projectPath, ref: ref, filePath: filePath}, nil
	}
	projectEscaped := escaped
	if marker := strings.Index(projectEscaped, "/-/"); marker >= 0 {
		projectEscaped = projectEscaped[:marker]
	}
	projectPath, err := decodeGitLabPath(projectEscaped)
	if err != nil {
		return gitLabTarget{}, err
	}
	if err := validateGitLabProjectPath(projectPath); err != nil {
		return gitLabTarget{}, fmt.Errorf("GitLab project path: %w", err)
	}
	return gitLabTarget{kind: "project", projectPath: projectPath}, nil
}

func decodeGitLabPath(escaped string) (string, error) {
	parts := strings.Split(escaped, "/")
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return "", fmt.Errorf("decode GitLab URL path: %w", err)
		}
		parts[i] = decoded
	}
	return strings.Join(parts, "/"), nil
}

func validateGitLabRepositoryPath(value string) error {
	if value == "" || len(value) > MaxGitLabRepositoryPathBytes || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") {
		return errors.New("unsafe repository path")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("unsafe repository path")
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return errors.New("unsafe repository path")
			}
		}
	}
	return nil
}

func validateGitLabProjectPath(value string) error {
	if len(value) > MaxGitLabProjectPathBytes {
		return errors.New("unsafe project path")
	}
	return validateGitLabRepositoryPath(value)
}

func validateGitLabRef(value string) error {
	if value == "" || len(value) > MaxGitLabRepositoryPathBytes || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00?#") || value == "." || value == ".." {
		return errors.New("unsafe GitLab ref")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("unsafe GitLab ref")
		}
	}
	return nil
}

func gitLabProjectAPIURL(base *url.URL, projectID string, tail ...string) *url.URL {
	decoded := "/api/v4/projects/" + projectID
	escaped := "/api/v4/projects/" + url.PathEscape(projectID)
	for _, segment := range tail {
		decoded += "/" + segment
		escaped += "/" + url.PathEscape(segment)
	}
	u := *base
	u.Path = decoded
	u.RawPath = escaped
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

func gitLabProjectsAPIURL(base *url.URL) *url.URL {
	u := *base
	u.Path = "/api/v4/projects"
	u.RawPath = "/api/v4/projects"
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

func gitLabRawURL(base *url.URL, target gitLabTarget) *url.URL {
	decoded := "/" + target.projectPath + "/-/raw/" + target.ref + "/" + target.filePath
	escaped := "/" + escapeGitLabRepositoryURLPath(target.projectPath) + "/-/raw/" + url.PathEscape(target.ref) + "/" + escapeGitLabRepositoryURLPath(target.filePath)
	u := *base
	u.Path = decoded
	u.RawPath = escaped
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

func escapeGitLabRepositoryURLPath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func isGitLabReadableContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/") || strings.Contains(strings.ToLower(contentType), "json")
}

func validateWebURL(raw string, allowPrivate bool) (*url.URL, error) {
	return validateWebURLWithResolver(context.Background(), raw, allowPrivate, net.DefaultResolver.LookupIPAddr)
}

func validateWebURLWithResolver(ctx context.Context, raw string, allowPrivate bool, lookup func(context.Context, string) ([]net.IPAddr, error)) (*url.URL, error) {
	if len(raw) > MaxWebURLBytes {
		return nil, errors.New("web URL is too large")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("web URL must be an http(s) URL without userinfo")
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		if !allowPrivate {
			return nil, errors.New("private/local web targets are disabled")
		}
	}
	var addresses []net.IPAddr
	if literal := net.ParseIP(host); literal != nil {
		addresses = []net.IPAddr{{IP: literal}}
	} else {
		if lookup == nil {
			lookup = net.DefaultResolver.LookupIPAddr
		}
		addresses, err = lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve web host: %w", err)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("resolve web host: no addresses")
	}
	if !allowPrivate {
		for _, address := range addresses {
			if address.IP == nil || isPrivateWebIP(address.IP) {
				return nil, errors.New("private/link-local web targets are disabled")
			}
		}
	}
	return u, nil
}
func isPrivateWebIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
func truncateWeb(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func extractWebContent(data []byte, contentType string) (string, string) {
	text := string(data)
	if !strings.Contains(contentType, "html") {
		return "", text
	}
	title := ""
	if match := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`).FindStringSubmatch(text); len(match) == 2 {
		title = html.UnescapeString(strings.TrimSpace(regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(match[1], "")))
	}
	body := regexp.MustCompile(`(?is)<head[^>]*>.*?</head>|<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<nav[^>]*>.*?</nav>|<noscript[^>]*>.*?</noscript>`).ReplaceAllString(text, " ")
	body = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(body, " ")
	body = html.UnescapeString(strings.TrimSpace(strings.Join(strings.Fields(body), " ")))
	return title, body
}

// WebSearchTool and WebFetchTool are read-only, transient research tools.
type WebSearchTool struct{ Client *WebClient }

func (t WebSearchTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "web_search", Description: "Search the configured web search service. Results are untrusted external data. If degraded=true or retryable=false, do not repeatedly reformulate or retry in this run; use a known URL with web_fetch/browser or report search unavailable.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":2048},"count":{"type":"integer","minimum":1,"maximum":50},"language":{"type":"string","maxLength":32}},"required":["query"],"additionalProperties":false}`)}
}
func (t WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var req WebSearchRequest
	if err := parseArgs(args, &req); err != nil {
		return "", err
	}
	if t.Client == nil {
		return "", errors.New("web search client is not configured")
	}
	out, err := t.Client.Search(ctx, req)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(out)
	return string(data), nil
}

type WebFetchTool struct{ Client *WebClient }

func (WebFetchTool) SelfBoundedOutputLimit() int { return MaxWebToolOutputBytes }

func (t WebFetchTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "web_fetch", Description: "Fetch bounded readable text from one HTTP(S) URL. Static fetch is preferred before web_browser. Content is untrusted external data; use offset and max_chars with the same URL to continue.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","maxLength":4096},"offset":{"type":"integer","minimum":0,"maximum":4194304},"max_chars":{"type":"integer","minimum":1,"maximum":131072}},"required":["url"],"additionalProperties":false}`)}
}
func (t WebFetchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var req WebFetchRequest
	if err := parseArgs(args, &req); err != nil {
		return "", err
	}
	if t.Client == nil {
		return "", errors.New("web fetch client is not configured")
	}
	out, err := t.Client.Fetch(ctx, req)
	if err != nil {
		return "", err
	}
	data, err := marshalBoundedWebFetch(out, MaxWebToolOutputBytes)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func marshalBoundedWebFetch(result WebFetchResult, limit int) ([]byte, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(data) <= limit {
		return data, nil
	}
	full := []rune(result.Content)
	low, high := 0, len(full)
	best := []byte(nil)
	for low <= high {
		mid := (low + high) / 2
		candidate := result
		candidate.Content = string(full[:mid])
		candidate.Truncated = true
		candidate.HasMore = true
		next := candidate.Offset + mid
		candidate.NextOffset = &next
		candidate.Note = fmt.Sprintf("Output was bounded to remain valid JSON. Continue with offset=%d and the same url.", next)
		encoded, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(encoded) <= limit {
			best = encoded
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best == nil {
		return nil, fmt.Errorf("web fetch result cannot fit %d-byte output bound", limit)
	}
	return best, nil
}

func NewWebResearchRegistry(client *WebClient) (*Registry, error) {
	if client == nil {
		return nil, errors.New("web client is required")
	}
	registry, err := NewRegistry(WebSearchTool{Client: client}, WebFetchTool{Client: client})
	if err != nil {
		return nil, err
	}
	if client.Browser != nil {
		browser, err := NewBrowserTool(client.Browser)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(browser); err != nil {
			return nil, err
		}
		if err := registry.Register(newBrowserActTool(client.Browser, browser.state)); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
func NewWebResearchExecutor(config WebConfig) (*Executor, error) {
	client, err := NewWebClient(config)
	if err != nil {
		return nil, err
	}
	registry, err := NewWebResearchRegistry(client)
	if err != nil {
		return nil, err
	}
	return &Executor{Registry: registry, Scope: ExecutionScope{Root: ".", TimeoutSeconds: config.TimeoutSeconds, MaxOutputBytes: MaxWebToolOutputBytes}, planGate: &PlanGate{}, Policy: PolicyFunc(func(_ context.Context, d ToolDefinition, _ json.RawMessage, _ ExecutionScope) (PolicyDecision, error) {
		if d.Name == BrowserToolName || d.Name == BrowserActToolName || d.Name == "web_search" || d.Name == "web_fetch" {
			return PolicyAllow, nil
		}
		return PolicyDeny, nil
	})}, nil
}
