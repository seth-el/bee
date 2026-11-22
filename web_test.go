package bee

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWebSearchNormalizesSearXNGAndForwardsNoCredentials(t *testing.T) {
	var gotAuth, gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		if r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") != "bee" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{{"title": "Bee docs", "url": "https://example.com/bee", "content": "useful result"}}})
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 1000})
	if err != nil {
		t.Fatal(err)
	}
	client.SearchEndpoint = server.URL
	client.AllowPrivateTargets = true
	result, err := client.Search(context.Background(), WebSearchRequest{Query: "bee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || !result.Untrusted || result.Results[0].URL != "https://example.com/bee" {
		t.Fatalf("result=%#v", result)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Fatalf("credential headers auth=%q cookie=%q", gotAuth, gotCookie)
	}
}

func TestWebFetchExtractsHTMLAndIgnoresLegacyContentLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Title</title><style>bad</style></head><body><nav>nav</nav><p>Hello</p><script>bad</script><p>world</p></body></html>`))
	}))
	defer server.Close()
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 256})
	if err != nil {
		t.Fatal(err)
	}
	client.AllowPrivateTargets = true
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, MaxChars: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "Title" || result.Content != "Hello" || !result.Truncated || result.NextOffset == nil || !result.Untrusted {
		t.Fatalf("result=%#v", result)
	}
}

func TestWebFetchReturnsContentBeyondLegacyCeilings(t *testing.T) {
	want := strings.Repeat("x", 300<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(want))
	}))
	defer server.Close()

	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL, MaxChars: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != DefaultWebFetchChars || !result.Truncated || result.TotalChars != len(want) {
		t.Fatalf("web fetch window: len=%d want=%d total=%d truncated=%v", len(result.Content), DefaultWebFetchChars, result.TotalChars, result.Truncated)
	}
}

func TestWebFetchGitLabProjectShellUsesBoundedAPIAndREADME(t *testing.T) {
	var gotAuth, gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		switch r.URL.EscapedPath() {
		case "/group/project":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>GitLab</title></head><body><div id="app">GitLab</div></body></html>`))
		case "/api/v4/projects/group%2Fproject":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"name":"project","path_with_namespace":"group/project","default_branch":"main"}`))
		case "/api/v4/projects/42/repository/tree":
			if r.URL.Query().Get("ref") != "main" || r.URL.Query().Get("recursive") != "true" || r.URL.Query().Get("per_page") != "100" {
				t.Fatalf("tree query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"path":"README.md","type":"blob"},{"path":"cmd/bee/main.go","type":"blob"},{"path":"docs","type":"tree"}]`))
		case "/api/v4/projects/42/repository/files/README.md/raw":
			if r.URL.Query().Get("ref") != "main" {
				t.Fatalf("README query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("# Project README\nUseful repository content."))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 2048})
	if err != nil {
		t.Fatal(err)
	}
	client.AllowPrivateTargets = true
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/group/project", MaxChars: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project: project", "Default branch: main", "README.md (blob)", "cmd/bee/main.go", "# Project README"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content missing %q: %s", want, result.Content)
		}
	}
	if result.ContentType != "text/plain; charset=utf-8" || !result.Untrusted {
		t.Fatalf("result=%#v", result)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Fatalf("credential headers auth=%q cookie=%q", gotAuth, gotCookie)
	}

	full, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/group/project", MaxChars: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Content, "# Project README") || full.Truncated {
		t.Fatalf("full result=%#v", full)
	}
}

func TestWebFetchGitLabProjectFindsRootREADMEOutsideRecursiveTreePage(t *testing.T) {
	var recursiveTreeAttempts, rootTreeAttempts, readmeAttempts int
	recursiveEntries := make([]gitLabTreeEntry, MaxGitLabTreeEntries)
	for i := range recursiveEntries {
		recursiveEntries[i] = gitLabTreeEntry{Path: "src/file-" + strconv.Itoa(i) + ".go", Type: "blob"}
	}
	recursiveTree, err := json.Marshal(recursiveEntries)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/group/project":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>GitLab</title></head><body><div id="app">GitLab</div></body></html>`))
		case "/api/v4/projects/group%2Fproject":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":43,"name":"project","path_with_namespace":"group/project","default_branch":"main"}`))
		case "/api/v4/projects/43/repository/tree":
			if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("ref") != "main" {
				t.Fatalf("tree query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("recursive") == "true" {
				recursiveTreeAttempts++
				_, _ = w.Write(recursiveTree)
				return
			}
			if r.URL.Query().Get("recursive") != "false" {
				t.Fatalf("root tree must be non-recursive: %s", r.URL.RawQuery)
			}
			rootTreeAttempts++
			_, _ = w.Write([]byte(`[{"path":"README.md","type":"blob"}]`))
		case "/api/v4/projects/43/repository/files/README.md/raw":
			readmeAttempts++
			if r.URL.Query().Get("ref") != "main" {
				t.Fatalf("README query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("# Root README"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/group/project"})
	if err != nil {
		t.Fatal(err)
	}
	if recursiveTreeAttempts != 1 || rootTreeAttempts != 1 || readmeAttempts != 1 {
		t.Fatalf("attempts recursive=%d root=%d README=%d", recursiveTreeAttempts, rootTreeAttempts, readmeAttempts)
	}
	if !strings.Contains(result.Content, "# Root README") {
		t.Fatalf("root README missing from result: %s", result.Content)
	}
}

func TestWebFetchGitLabProjectMetadataSearchFallbackUsesExactProjectID(t *testing.T) {
	const projectPath = "example/sample-project"
	var metadataAttempts, searchAttempts, treeAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatalf("unexpected credentials auth=%q cookie=%q", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		switch r.URL.EscapedPath() {
		case "/example/sample-project":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>GitLab</title></head><body><div id="app">GitLab</div></body></html>`))
		case "/api/v4/projects/example%2Fsample-project":
			metadataAttempts++
			http.Error(w, "encoded project lookup is unsupported", http.StatusBadRequest)
		case "/api/v4/projects":
			searchAttempts++
			if got := r.URL.Query().Get("search"); got != "sample-project" {
				t.Fatalf("search query=%q", got)
			}
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Fatalf("search per_page=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
                {"id":7,"name":"sample-project","path":"sample-project","path_with_namespace":"other/sample-project","default_branch":"main"},
                {"id":129,"name":"sample-project","path":"sample-project","path_with_namespace":"example/sample-project","default_branch":"main"}
            ]`))
		case "/api/v4/projects/129/repository/tree":
			treeAttempts++
			if r.URL.Query().Get("ref") != "main" {
				t.Fatalf("tree query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"path":"README.md","type":"blob"}]`))
		case "/api/v4/projects/129/repository/files/README.md/raw":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("# Fallback README"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/" + projectPath})
	if err != nil {
		t.Fatal(err)
	}
	if metadataAttempts != 1 || searchAttempts != 1 || treeAttempts != 1 {
		t.Fatalf("attempts metadata=%d search=%d tree=%d", metadataAttempts, searchAttempts, treeAttempts)
	}
	for _, want := range []string{"Project: sample-project", "Default branch: main", "# Fallback README"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content missing %q: %s", want, result.Content)
		}
	}
}

func TestSelectGitLabProjectRejectsAmbiguousPathFallback(t *testing.T) {
	project, ok := selectGitLabProject([]gitLabProject{
		{ID: 1, Path: "project"},
		{ID: 2, Path: "project"},
	}, "project")
	if ok || project.ID != 0 {
		t.Fatalf("ambiguous fallback selected project=%#v ok=%v", project, ok)
	}

	project, ok = selectGitLabProject([]gitLabProject{{ID: 3, Path: "project"}}, "project")
	if !ok || project.ID != 3 {
		t.Fatalf("safe path fallback not selected project=%#v ok=%v", project, ok)
	}
}

func TestWebFetchGitLabBlobShellUsesRawFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/group/project/-/blob/main/src/main.go":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><div id="app">GitLab</div></body></html>`))
		case "/group/project/-/raw/main/src/main.go":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("package main\n\nfunc main() {}\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/group/project/-/blob/main/src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "main.go" || result.Content != "package main\n\nfunc main() {}\n" {
		t.Fatalf("result=%#v", result)
	}
}

func TestWebFetchNonGitLabHTMLShellRegression(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><div id="app">Ordinary application content</div></body></html>`))
	}))
	defer server.Close()
	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL + "/app"})
	if err != nil || result.Content != "Ordinary application content" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestWebAllowsPrivateTargetsByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("internal Bee research"))
	}))
	defer server.Close()

	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL})
	if err != nil {
		t.Fatalf("default private fetch failed: %v", err)
	}
	if result.Content != "internal Bee research" || result.FinalURL != server.URL {
		t.Fatalf("result=%#v", result)
	}
}

func TestWebExplicitlyDisablesPrivateTargets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("internal Bee research"))
	}))
	defer server.Close()

	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	client.AllowPrivateTargets = false
	if _, err := client.Fetch(context.Background(), WebFetchRequest{URL: server.URL}); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("client policy accepted private target: %v", err)
	}

	allowPrivateTargets := false
	client, err = NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 256, AllowPrivateTargets: &allowPrivateTargets})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"http://127.0.0.1/", "http://localhost/"} {
		if _, err := client.Fetch(context.Background(), WebFetchRequest{URL: target}); err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatalf("target=%s err=%v", target, err)
		}
	}
}

func TestWebPublicOnlyDialRejectsDNSRebinding(t *testing.T) {
	var lookups int
	dialed := false
	client := &WebClient{
		HTTPClient: &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		}}},
		Timeout:             time.Second,
		AllowPrivateTargets: false,
		LookupIPAddr: func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			if lookups == 1 {
				return []net.IPAddr{{IP: net.ParseIP("198.51.100.10")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
	}
	_, err := client.Fetch(context.Background(), WebFetchRequest{URL: "http://rebind.example/resource"})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("rebinding error=%v", err)
	}
	if dialed {
		t.Fatal("private rebound address reached dialer")
	}
}

func TestWebPublicOnlyClientClearsCustomTLSDialer(t *testing.T) {
	client := &WebClient{
		HTTPClient: &http.Client{Transport: &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("custom TLS dialer must not run")
		}}},
		AllowPrivateTargets: false,
	}
	requestClient, err := client.requestClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := requestClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport=%T", requestClient.Transport)
	}
	if transport.DialTLSContext != nil {
		t.Fatal("public-only client retained a custom TLS dialer")
	}
}

func TestWebRejectsBadSchemesAndUserinfo(t *testing.T) {
	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"file:///tmp/a", "ftp://example.com/a", "https://user:pass@example.com/"} {
		if _, err := client.Fetch(context.Background(), WebFetchRequest{URL: target}); err == nil {
			t.Fatalf("target accepted: %s", target)
		}
	}
}

func TestWebRedirectPolicyFollowsSelectedSetting(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("redirected internal result"))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	allowed, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := allowed.Fetch(context.Background(), WebFetchRequest{URL: source.URL})
	if err != nil || result.Content != "redirected internal result" {
		t.Fatalf("allowed redirect result=%#v err=%v", result, err)
	}

	allowPrivateTargets := false
	blocked, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 2, MaxResults: 5, MaxChars: 256, AllowPrivateTargets: &allowPrivateTargets})
	if err != nil {
		t.Fatal(err)
	}
	sourceURL, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := blocked.do(context.Background(), sourceURL); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("blocked redirect error=%v", err)
	}
}

func TestWebSearchResultFilteringFollowsSelectedSetting(t *testing.T) {
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{
			{"title": "internal", "url": "http://127.0.0.1/internal", "content": "private result"},
			{"title": "public", "url": "http://198.51.100.1/public", "content": "public result"},
		}})
	}))
	defer searchServer.Close()

	allowed, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: searchServer.URL, TimeoutSeconds: 2, MaxResults: 5, MaxChars: 256})
	if err != nil {
		t.Fatal(err)
	}
	result, err := allowed.Search(context.Background(), WebSearchRequest{Query: "bee"})
	if err != nil || len(result.Results) != 2 {
		t.Fatalf("default search result=%#v err=%v", result, err)
	}

	allowPrivateTargets := false
	blocked, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: searchServer.URL, TimeoutSeconds: 2, MaxResults: 5, MaxChars: 256, AllowPrivateTargets: &allowPrivateTargets})
	if err != nil {
		t.Fatal(err)
	}
	result, err = blocked.Search(context.Background(), WebSearchRequest{Query: "bee"})
	if err != nil || len(result.Results) != 1 || result.Results[0].Title != "public" {
		t.Fatalf("blocked search result=%#v err=%v", result, err)
	}
}

func TestWebTimeoutAndToolReadOnlyDefinitions(t *testing.T) {
	client, err := NewWebClient(WebConfig{Enabled: true, SearchEndpoint: "https://example.com/search", TimeoutSeconds: 1, MaxResults: 5, MaxChars: 256})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewWebResearchRegistry(client)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range registry.Definitions() {
		if d.Mutability != ToolMutabilityReadOnly || d.Risk != RiskLow {
			t.Fatalf("definition=%#v", d)
		}
	}
	for _, d := range registry.Definitions() {
		if d.Name == "web_fetch" && !strings.Contains(string(d.Parameters), "max_chars") {
			t.Fatalf("web_fetch schema lost bounded max_chars: %s", d.Parameters)
		}
	}
	_ = time.Second
}

func TestWebResearchExecutorDoesNotTruncateWebFetchOutput(t *testing.T) {
	want := strings.Repeat("y", 300<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(want))
	}))
	defer server.Close()

	executor, err := NewWebResearchExecutor(WebConfig{Enabled: true, SearchEndpoint: server.URL, TimeoutSeconds: 2, MaxResults: 5, MaxChars: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), "web_fetch", json.RawMessage(`{"url":"`+server.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var fetched WebFetchResult
	if err := json.Unmarshal([]byte(result.Output), &fetched); err != nil {
		t.Fatalf("decode tool output: %v", err)
	}
	if len(fetched.Content) != DefaultWebFetchChars || !fetched.Truncated || result.Truncated {
		t.Fatalf("research executor window: content=%d want=%d fetched_truncated=%v result_truncated=%v", len(fetched.Content), DefaultWebFetchChars, fetched.Truncated, result.Truncated)
	}
}

func TestAgentWebRegistryMergeDoesNotMutateBaseExecutor(t *testing.T) {
	client, err := NewWebClient(DefaultWebConfig())
	if err != nil {
		t.Fatal(err)
	}
	webRegistry, err := NewWebResearchRegistry(client)
	if err != nil {
		t.Fatal(err)
	}
	baseRegistry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "base", Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: baseRegistry, Scope: ExecutionScope{Root: t.TempDir()}}
	options := AgentOptions{Executor: executor, WebResearch: &Executor{Registry: webRegistry, Scope: executor.Scope}}
	provider := &fakeProvider{}
	if _, err := NewAgentWithOptions(DefaultConfig(), provider, options); err != nil {
		t.Fatal(err)
	}
	if _, ok := baseRegistry.Lookup("web_fetch"); ok {
		t.Fatal("web merge mutated base registry")
	}
	if _, err := NewAgentWithOptions(DefaultConfig(), provider, options); err != nil {
		t.Fatalf("second web merge failed: %v", err)
	}
}
