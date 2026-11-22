package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLoadConfigPreservesExplicitScoutDisableAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"web":{"enabled":true,"scout":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Web.Scout.Enabled || config.Web.Scout.MaxSteps != DefaultWebResearchScoutMaxSteps || !config.Web.Scout.DisableThinking {
		t.Fatalf("scout config=%#v", config.Web.Scout)
	}
}

func TestWebResearchScoutConfigDefaultsAndBounds(t *testing.T) {
	defaults := DefaultWebResearchScoutConfig()
	if defaults.TimeoutSeconds != 240 || defaults.MaxSteps != 10 || defaults.MaxOutputBytes != 12288 || defaults.MaxTokens != 2048 || defaults.MaxSources != 10 || defaults.MaxConcurrency != 2 || !defaults.DisableThinking {
		t.Fatalf("defaults = %#v", defaults)
	}
	if err := (WebResearchScoutConfig{MaxSteps: 17}).Validate(); err == nil {
		t.Fatal("expected max_steps bound")
	}
	if err := (WebResearchScoutConfig{MaxOutputBytes: 32769}).Validate(); err == nil {
		t.Fatal("expected output bound")
	}
	for name, config := range map[string]WebResearchScoutConfig{
		"timeout": {TimeoutSeconds: 1},
		"steps":   {MaxSteps: 1},
		"output":  {MaxOutputBytes: 1024},
		"tokens":  {MaxTokens: 128},
		"sources": {MaxSources: -1},
		"queue":   {MaxConcurrency: -1},
	} {
		if err := config.Validate(); err == nil {
			t.Fatalf("%s minimum should be rejected", name)
		}
	}
	if err := (WebResearchScoutConfig{MaxSources: 21}).Validate(); err == nil {
		t.Fatal("expected source bound")
	}
	if err := (WebResearchScoutConfig{MaxConcurrency: 4}).Validate(); err == nil {
		t.Fatal("expected concurrency bound")
	}
}

func TestComposeGeneralAssistantScoutHidesRawForegroundTools(t *testing.T) {
	config := DefaultConfig()
	config.Web.Enabled = true
	config.Web.Scout.Enabled = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if app.ForegroundWebResearch == nil || app.WebResearch == nil {
		t.Fatalf("foreground=%#v raw=%#v", app.ForegroundWebResearch, app.WebResearch)
	}
	options := app.agentOptions(nil, nil, "")
	for _, name := range []string{"web_search", BrowserToolName, BrowserActToolName} {
		if _, ok := options.WebResearch.Registry.Lookup(name); ok {
			t.Fatalf("foreground exposed raw tool %q", name)
		}
	}
	if _, ok := options.WebResearch.Registry.Lookup(WebResearchToolName); !ok {
		t.Fatal("foreground omitted web_research")
	}
	if _, ok := options.WebResearch.Registry.Lookup("web_fetch"); !ok {
		t.Fatal("foreground omitted bounded web_fetch")
	}
	if _, ok := app.WebResearch.Registry.Lookup("web_search"); !ok {
		t.Fatal("raw executor lost web_search")
	}
	for _, name := range []string{"web_search", BrowserToolName, BrowserActToolName} {
		if containsString(app.Capabilities.ToolNames, name) {
			t.Fatalf("capabilities exposed raw tool %q", name)
		}
	}
	if !containsString(app.Capabilities.ToolNames, "web_fetch") {
		t.Fatal("capabilities omitted bounded web_fetch")
	}
	if !containsString(app.Capabilities.ToolNames, WebResearchToolName) {
		t.Fatal("capabilities omitted web_research")
	}
}

func TestWebResearchScoutLockedModeExposesNothing(t *testing.T) {
	config := DefaultConfig()
	config.Web.Enabled = true
	config.Web.Scout.Enabled = true
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir(), Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if app.Executor != nil || app.ForegroundWebResearch != nil || containsString(app.Capabilities.ToolNames, WebResearchToolName) {
		t.Fatalf("locked scout exposure: executor=%#v foreground=%#v capabilities=%#v", app.Executor, app.ForegroundWebResearch, app.Capabilities)
	}
}

func TestObservedWebSourcesOnlyAcceptsObservedHTTPURLs(t *testing.T) {
	observed := newObservedWebSources(2)
	observed.observe("web_search", `{"results":[{"title":"One","url":"https://example.com/one"},{"title":"Invented","url":"javascript:alert(1)"},{"title":"Two","url":"https://example.com/two"}]}`, "")
	observed.observe("web_fetch", `{"title":"ignored duplicate","final_url":"https://example.com/one"}`, "")
	observed.observe("web_browser", `{"url":"https://example.com/three","title":"over max"}`, "")
	got := observed.list()
	if len(got) != 2 || got[0].URL != "https://example.com/one" || got[1].URL != "https://example.com/two" {
		t.Fatalf("sources=%#v", got)
	}
}

func TestObservedWebSourcesPromotesInspectedResultsAndPreservesOrder(t *testing.T) {
	observed := newObservedWebSources(3)
	observed.observe("web_search", `{"results":[{"title":"one","url":"https://example.com/1"},{"title":"two","url":"https://example.com/2"},{"title":"three","url":"https://example.com/3"},{"title":"four","url":"https://example.com/4"},{"title":"eight","url":"https://example.com/8"}]}`, "")
	observed.observe("web_fetch", `{"title":"Fetched Eight","final_url":"https://example.com/8"}`, "")
	observed.observe("web_browser", `{"ok":true,"title":"Inspected Two","url":"https://example.com/2"}`, "")
	got := observed.list()
	if len(got) != 3 {
		t.Fatalf("sources=%#v", got)
	}
	want := []string{"https://example.com/8", "https://example.com/2", "https://example.com/1"}
	for i, source := range got {
		if source.URL != want[i] {
			t.Fatalf("source[%d]=%#v want %q; all=%#v", i, source, want[i], got)
		}
	}
	if got[0].Title != "Fetched Eight" || got[1].Title != "Inspected Two" {
		t.Fatalf("strong observations did not update titles: %#v", got)
	}
}

func TestObservedWebSourcesRejectsUnsuccessfulResults(t *testing.T) {
	observed := newObservedWebSources(5)
	observed.observe("web_fetch", `{"title":"empty","final_url":""}`, "")
	observed.observe(BrowserToolName, `{"ok":false,"url":"https://example.com/nope","title":"failed"}`, "")
	observed.observe("web_fetch", `{"title":"error","final_url":"https://example.com/error"}`, "transport failed")
	if got := observed.list(); len(got) != 0 {
		t.Fatalf("unsuccessful sources=%#v", got)
	}
}

func TestObservedWebSourcesBoundsCitationMetadata(t *testing.T) {
	observed := newObservedWebSources(5)
	observed.observe("web_search", `{"results":[{"title":"`+strings.Repeat("猫", 400)+`","url":"https://example.com/ok"},{"title":"too long","url":"https://example.com/`+strings.Repeat("a", MaxWebResearchCitationURLBytes)+`"}]}`, "")
	got := observed.list()
	if len(got) != 1 || len(got[0].Title) > MaxWebResearchCitationTitleBytes || got[0].URL != "https://example.com/ok" {
		t.Fatalf("bounded citations=%#v", got)
	}
}

func TestCitationURLCanonicalizationRemovesSensitiveAndTrackingQuery(t *testing.T) {
	raw := "https://www.reddit.com/r/example/comments/abc/?solution=answer&jsc_token=secret&challenge_id=123&session=abc&page=2&sort=top&utm_source=feed#fragment"
	canonical, ok := canonicalizeCitationURL(raw)
	if !ok {
		t.Fatal("challenge URL was rejected instead of safely canonicalized")
	}
	if strings.Contains(strings.ToLower(canonical), "solution") || strings.Contains(strings.ToLower(canonical), "token") || strings.Contains(strings.ToLower(canonical), "challenge") || strings.Contains(strings.ToLower(canonical), "session") || strings.Contains(canonical, "utm_") || strings.Contains(canonical, "#") {
		t.Fatalf("sensitive data survived canonicalization: %q", canonical)
	}
	if !strings.Contains(canonical, "page=2") || !strings.Contains(canonical, "sort=top") {
		t.Fatalf("meaningful query parameters were lost: %q", canonical)
	}
}

func TestObservedSourcesDeduplicateCanonicalChallengeURLsAndSanitizeBrief(t *testing.T) {
	observed := newObservedWebSources(5)
	observed.observe("web_search", `{"results":[{"title":"Reddit challenge","url":"https://www.reddit.com/r/example/comments/abc/?jsc_token=secret&solution=x&page=2"}]}`, "")
	observed.observe("web_fetch", `{"title":"Reddit page","final_url":"https://www.reddit.com/r/example/comments/abc/?page=2#frag"}`, "")
	sources := observed.list()
	if len(sources) != 1 || sources[0].URL != "https://www.reddit.com/r/example/comments/abc/?page=2" {
		t.Fatalf("canonical sources=%#v", sources)
	}
	brief := "See https://www.reddit.com/r/example/comments/abc/?jsc_token=secret&solution=x&page=2 and https://www.reddit.com/r/example/comments/abc/?page=2."
	clean := sanitizeScoutBrief(brief, sources)
	if strings.Contains(clean, "jsc_token") || strings.Contains(clean, "solution=x") || !strings.Contains(clean, "https://www.reddit.com/r/example/comments/abc/?page=2") {
		t.Fatalf("sanitized challenge brief=%q", clean)
	}
}

func TestScoutEvidencePacketIsBoundedUTF8AndPrioritizesInspectedEvidence(t *testing.T) {
	evidence := newScoutEvidenceCollector()
	evidence.observe("web_search", `{"results":[{"title":"search","url":"https://example.com/search","snippet":"`+strings.Repeat("猫", 1000)+`"},{"title":"tail","url":"https://example.com/tail","snippet":"tail"}]}`, "")
	evidence.observe("web_fetch", `{"title":"inspected","final_url":"https://example.com/inspected","content":"token=secret `+strings.Repeat("x", 10000)+`"}`, "")
	sources := []WebResearchSource{{Title: "inspected", URL: "https://example.com/inspected"}, {Title: "search", URL: "https://example.com/search"}, {Title: "tail", URL: "https://example.com/tail"}}
	packet := evidence.packet(sources)
	if len(packet) > MaxWebResearchEvidenceBytes || !utf8.ValidString(packet) {
		t.Fatalf("evidence packet bound/UTF failure len=%d valid=%v", len(packet), utf8.ValidString(packet))
	}
	if !strings.HasPrefix(packet, "[S1] inspected") || strings.Contains(packet, "secret") {
		t.Fatalf("evidence packet unsafe or unordered: %q", packet[:minInt(len(packet), 512)])
	}
}

func TestBoundedScoutSearchDoesNotExecuteThirdUpstreamRequest(t *testing.T) {
	calls := 0
	inner := ToolFunc{ToolDefinition: ToolDefinition{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) {
		calls++
		return `{"query":"x","results":[],"retryable":true}`, nil
	}}
	tool := boundedScoutSearchTool{inner: inner, gate: &scoutSearchGate{}}
	for i := 0; i < 3; i++ {
		output, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && !strings.Contains(output, "Search request budget expired") {
			t.Fatalf("third search output=%s", output)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2", calls)
	}
}

func TestSanitizeScoutBriefRemovesInventedURLs(t *testing.T) {
	brief := "Observed https://example.com/one, but invented https://evil.example/nope. Markdown: [source](https://example.com/one)."
	got := sanitizeScoutBrief(brief, []WebResearchSource{{Title: "One", URL: "https://example.com/one"}})
	want := "Observed https://example.com/one, but invented [unverified URL omitted]. Markdown: [source](https://example.com/one)."
	if got != want {
		t.Fatalf("sanitized brief=%q want %q", got, want)
	}
}

func TestMarshalBoundedWebResearchResultIsRuneSafe(t *testing.T) {
	result := WebResearchResult{Status: "complete", Brief: strings.Repeat("猫", 5000), Sources: []WebResearchSource{{Title: "One", URL: "https://example.com/one"}}, Limitations: []string{}, ToolCalls: WebResearchToolCounts{}, Untrusted: true}
	data, err := marshalBoundedWebResearchResult(result, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 || !json.Valid(data) {
		t.Fatalf("invalid bounded result len=%d valid=%v", len(data), json.Valid(data))
	}
	var decoded WebResearchResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded.Brief, "猫") {
		t.Fatalf("multibyte brief was lost: %q", decoded.Brief)
	}
}

func TestMarshalBoundedWebResearchResultPreservesLimitationsAndPrioritySources(t *testing.T) {
	sources := make([]WebResearchSource, 0, 20)
	for i := 0; i < 20; i++ {
		sources = append(sources, WebResearchSource{Title: strings.Repeat("quoted \"title\" ", 30), URL: "https://example.com/source-" + strconv.Itoa(i) + "-" + strings.Repeat("a", 1900)})
	}
	result := WebResearchResult{
		Status:       "complete",
		Brief:        strings.Repeat("猫 and escaped \\\"quotes\\\". ", 600),
		Sources:      sources,
		Limitations:  []string{"search request budget expired", "external content is untrusted"},
		ToolCalls:    WebResearchToolCounts{Total: 8, Search: 2, Fetch: 4, Browser: 2},
		ChildProfile: "local-child-8082",
		ChildModel:   "muse-test",
		Untrusted:    true,
	}
	data, err := marshalBoundedWebResearchResult(result, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4096 || !json.Valid(data) {
		t.Fatalf("bounded result len=%d valid=%v", len(data), json.Valid(data))
	}
	var decoded WebResearchResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OutputTruncated || decoded.Brief == "" || len(decoded.Limitations) != 2 || len(decoded.Sources) == 0 {
		t.Fatalf("metadata was lost: %#v", decoded)
	}
	if decoded.Sources[0].URL != sources[0].URL || decoded.Sources[0].Title == "" {
		t.Fatalf("highest-priority source was not retained: %#v", decoded.Sources)
	}
}

func TestWebResearchToolRejectsUnknownAndInvalidURLs(t *testing.T) {
	definition := (WebResearchTool{}).Definition()
	var schema map[string]any
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema=%s", definition.Parameters)
	}
	tool := WebResearchTool{}
	if _, err := tool.Execute(context.Background(), []byte(`{"question":"x","extra":true}`)); err == nil {
		t.Fatal("expected strict argument rejection")
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"question":"x","known_urls":["file:///tmp/x"]}`)); err == nil {
		t.Fatal("expected URL rejection")
	}
	if !strings.Contains(strings.ToLower(definition.Description), "synthesize") {
		t.Fatal("description should explain synthesis boundary")
	}
}
