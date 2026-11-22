package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type testProvider struct{}

func (testProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, errors.New("complete not used")
}

func (testProvider) Stream(_ context.Context, _ bee.Request, onDelta bee.StreamFunc) (bee.Response, error) {
	if err := onDelta("hello"); err != nil {
		return bee.Response{}, err
	}
	return bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: "hello"}}, nil
}

type blockingProvider struct{}

func TestExplicitMediaAutoRetrieveIntent(t *testing.T) {
	tests := []struct {
		prompt string
		want   bool
	}{
		{prompt: "show me a bee image", want: true},
		{prompt: "display a short video clip", want: true},
		{prompt: "attach the photo", want: true},
		{prompt: "send me a media picture", want: true},
		{prompt: "retrieve a video", want: true},
		{prompt: "fetch an image", want: true},
		{prompt: "grab a good video", want: true},
		{prompt: "download a short clip", want: true},
		{prompt: "find a bee image", want: false},
		{prompt: "search for a bee photo", want: false},
		{prompt: "show me image options", want: false},
		{prompt: "list video clips", want: false},
		{prompt: "what is a bee image", want: false},
	}
	for _, test := range tests {
		t.Run(test.prompt, func(t *testing.T) {
			if got := explicitMediaAutoRetrieveIntent(test.prompt); got != test.want {
				t.Fatalf("explicitMediaAutoRetrieveIntent(%q)=%t, want %t", test.prompt, got, test.want)
			}
		})
	}
}

type mediaHTTPClient struct{ body []byte }

func (client mediaHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(client.body)), ContentLength: int64(len(client.body)), Request: request}, nil
}

func (blockingProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, errors.New("complete not used")
}

type commandProvider struct {
	called int
}

func (provider *commandProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	provider.called++
	return bee.Response{}, errors.New("provider should not be called")
}

func (provider *commandProvider) Stream(context.Context, bee.Request, bee.StreamFunc) (bee.Response, error) {
	provider.called++
	return bee.Response{}, errors.New("provider should not be called")
}

func (blockingProvider) Stream(ctx context.Context, _ bee.Request, _ bee.StreamFunc) (bee.Response, error) {
	<-ctx.Done()
	return bee.Response{}, ctx.Err()
}

func newTestService(t *testing.T, provider bee.Provider) *Service {
	t.Helper()
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	config.Provider.Model = "test-model"
	app, err := bee.NewApplication(config, provider, store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewForApplication(app, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNewWebGatewayDoesNotStartTelegram(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	memoryRoot := t.TempDir()
	if err := os.Chmod(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config.Assistant.MemoryRoot = memoryRoot
	config.Assistant.JobStorePath = filepath.Join(t.TempDir(), "jobs.json")
	config.Provider.Model = "web-only-test"

	service, err := New(Config{
		BeeConfig:    config,
		SessionStore: store,
		ListenAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.App() == nil || service.App().Store != store {
		t.Fatal("web gateway did not compose the supplied application and session store")
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())

	ready := httptest.NewRecorder()
	service.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("web gateway readiness = %d", ready.Code)
	}
	if err := service.ServeError(); err != nil {
		t.Fatalf("web gateway reported a non-web runtime error: %v", err)
	}
}

func TestGatewaySessionsReadinessAndStaticPage(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	handler := service.Handler()

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready before start = %d", ready.Code)
	}

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	page := root.Body.String()
	if root.Code != http.StatusOK || !strings.Contains(page, `src="/assets/bee-mascot.svg?v=20260919-perf1"`) {
		t.Fatalf("root response = %d %q", root.Code, page)
	}
	if strings.Contains(page, "🐝") {
		t.Fatal("static page still contains the old Bee emoji empty state")
	}
	for _, marker := range []string{
		"Shift+Enter",
		"isComposing",
		"assistant_delta",
		"artifact-card",
		"/api/artifacts/",
		"X-Bee-Artifact-Truncated",
		"New conversation",
		"id=\"sidebar-toggle\"",
		"id=\"mobile-sidebar-toggle\"",
		"id=\"mobile-sidebar-close\"",
		"id=\"sidebar-backdrop\"",
		"sidebar-open",
		"setMobileSidebarOpen",
		"closeMobileSidebar",
		"matchMedia('(max-width: 760px)')",
		"transform: translateX(-105%)",
		"sidebar-backdrop[hidden]",
		"textarea { font-size: 16px; }",
		".composer-wrap { border-top: 1px solid var(--line); background: rgba(16, 13, 13, .94); padding: 22px clamp(18px, 7vw, 110px); }",
		".composer-wrap { padding: 16px 14px; }",
		`data-autogrow="bounded"`,
		`data-max-height="220"`,
		"#prompt { resize: none; overflow-y: hidden; }",
		`#prompt[data-overflowing="true"] { overflow-y: auto; }`,
		"function resizePrompt()",
		"prompt.style.height = 'auto'",
		"prompt.dataset.overflowing",
		"prompt.addEventListener('input', resizePrompt)",
		"prompt.addEventListener('paste'",
		"resizePrompt();",
		"aria-expanded",
		"localStorage",
		"100dvh",
		"overflow: hidden",
		"overflow-y: auto",
		"scrollbar-width: none",
		"-ms-overflow-style: none",
		".scrollbar-hidden::-webkit-scrollbar",
		`class="sidebar scrollbar-hidden"`,
		`class="project-list scrollbar-hidden"`,
		`class="session-list scrollbar-hidden"`,
		`class="messages scrollbar-hidden"`,
		".chat-panel { flex: 1 1 auto; min-width: 0; min-height: 0; display: flex; flex-direction: column; }",
		".chat-header { flex: 0 0 auto; min-height: 70px;",
		".chat-header { align-items: stretch; flex-direction: column; gap: 10px; }",
		".chat-header-leading { width: 100%; display: grid; grid-template-columns: auto minmax(0, 1fr);",
		".chat-header-leading .session-project-picker { grid-column: 1 / -1; grid-row: 2; width: 100%;",
		".chat-header > #session-details { width: 100%; padding-top: 2px; border-top: 1px solid var(--line); }",
		".messages { flex: 1 1 auto; min-height: 0; overflow-y: auto;",
		".empty-state { display: flex; align-items: center; justify-content: center; width: 100%; min-height: 100%;",
		".empty-state-copy { max-width: 34rem; }",
		`<img class="brand-avatar" src="/assets/bee-mascot.svg?v=20260919-perf1" alt=""><span class="brand-mark">BEE</span>`,
		"message-avatar",
		"createMessageAvatar",
		"avatarSource",
		"Start a conversation",
		"Choose a conversation or send a message to Bee.",
		"window.__BEE_UI_CONFIG__",
		".workspace { display: flex; flex-direction: column; min-height: 0; }",
		".chat-panel { flex: 1 1 auto; }",
		"tool-event",
		".tool-event { display: none;",
		"dataset.toolEvent",
		"<details",
		"toolPayloadText",
		"createElement('pre')",
		"streamInactivityWatchdog",
		"readRunStatus",
		"statusCrossesReplayGap",
		"streamError.nextCursor = nextCursor",
		"error.nextCursor",
		"Live stream interrupted; recovering…",
		"Live replay gap detected; Bee is still working…",
		"reconcileConversation",
		"history.timeline",
		"function renderHistory(history, artifacts = [], attachments = [], timeline = [])",
		"item.type === 'artifact'",
		"?from=",
		"/history",
		"DELETE",
		"deleteSession",
		"session-menu-trigger",
		"session-menu-item",
		"aria-haspopup",
		"aria-controls",
		"role', 'menuitem",
		"closeSessionMenus",
		"ArrowDown",
		"Delete conversation",
		"permanently removed",
		"list[0]",
		"/subagents",
		"/tasks",
		"audio.controls = true",
		"mediaType.startsWith('audio/')",
		"mediaType.startsWith('image/')",
		"mediaType.startsWith('video/')",
		"media-search-card",
		"auto_retrieved",
		"if (result && (result.auto_retrieved || result.no_additional_retrieval)) return;",
		"case 'media_search'",
		"/media/select",
		"candidate_id",
		"source_url",
		"createElement('video')",
		"artifact-card-image",
		"media_progress",
		"id=\"continue-checkpoint\"",
		"case 'turn_checkpoint'",
		"checkpoint_id: state.continueCheckpointID || undefined",
		"function continueFromCheckpoint()",
		"/api/artifacts/' + encodeURIComponent(artifact.id) + '/download",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing UI marker %q", marker)
		}
	}
	if strings.Contains(page, "session-delete") {
		t.Fatal("static page still exposes the old always-visible session delete button")
	}

	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready = httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready after start = %d", ready.Code)
	}

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"web"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", create.Code, create.Body.String())
	}
	var metadata bee.SessionMetadata
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ID == "" || metadata.Title != "web" {
		t.Fatalf("metadata = %#v", metadata)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), string(metadata.ID)) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}

	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+string(metadata.ID), nil))
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+string(metadata.ID), nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d %s", missing.Code, missing.Body.String())
	}
	unsafe := httptest.NewRecorder()
	handler.ServeHTTP(unsafe, httptest.NewRequest(http.MethodDelete, "/api/sessions/..", nil))
	if unsafe.Code != http.StatusBadRequest {
		t.Fatalf("delete unsafe id = %d %s", unsafe.Code, unsafe.Body.String())
	}
}

func TestGatewayBoundsConcurrentRuns(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(bee.SessionMetadata{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(bee.SessionMetadata{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	config.Web.MaxConcurrentRuns = 1
	app, err := bee.NewApplication(config, blockingProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	app.Diagnostics = bee.NewWriterDiagnosticSink(&diagnostics)
	service, err := NewForApplication(app, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())

	runID, err := service.startRun(bee.RunRequest{SessionID: first.ID, Prompt: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.startRun(bee.RunRequest{SessionID: second.ID, Prompt: "reject"}); !errors.Is(err, ErrRunCapacity) {
		t.Fatalf("second run error = %v, want ErrRunCapacity", err)
	}
	if got := app.Metrics.Snapshot().AdmissionRejected; got != 1 {
		t.Fatalf("admission rejected metric = %d, want 1", got)
	}
	if logged := diagnostics.String(); !strings.Contains(logged, `"error_code":"gateway_capacity"`) || strings.Contains(logged, "reject") {
		t.Fatalf("capacity diagnostic = %q", logged)
	}
	record, ok := service.run(runID)
	if !ok {
		t.Fatal("active run record missing")
	}
	record.cancelRun()
	deadline := time.Now().Add(2 * time.Second)
	for !record.isDone() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !record.isDone() {
		t.Fatal("cancelled run did not finish")
	}
	if got := app.Metrics.Snapshot().RunsCanceled; got != 1 {
		t.Fatalf("canceled run metric = %d, want 1", got)
	}
}

func TestGatewayExposesBoundedRuntimeMetrics(t *testing.T) {
	service := newTestService(t, testProvider{})
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	var snapshot bee.RuntimeMetricsSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveRuns != 0 || snapshot.RunsStarted != 0 {
		t.Fatalf("initial metrics = %#v", snapshot)
	}
}

func TestRunStartErrorsUseAdmissionStatuses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "session busy", err: bee.ErrSessionBusy, want: http.StatusConflict},
		{name: "capacity", err: ErrRunCapacity, want: http.StatusTooManyRequests},
		{name: "shutdown", err: context.Canceled, want: http.StatusServiceUnavailable},
		{name: "internal", err: errors.New("failed"), want: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeRunStartError(response, test.err)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestBoundedRunEventsReason(t *testing.T) {
	for _, reason := range []string{
		runEventsReasonRunCompleted,
		runEventsReasonClientDisconnect,
		runEventsReasonServiceShutdown,
		runEventsReasonMaxDuration,
		runEventsReasonWriteError,
		runEventsReasonSetup,
		runEventsReasonInvalidRequest,
	} {
		if got := boundedRunEventsReason(reason); got != reason {
			t.Fatalf("boundedRunEventsReason(%q) = %q", reason, got)
		}
	}
	if got := boundedRunEventsReason("prompt=do-not-log"); got != runEventsReasonSetup {
		t.Fatalf("unknown reason normalized to %q, want %q", got, runEventsReasonSetup)
	}
}

func TestGatewayStaticPagePreservesVideoPlaybackDuringReconcile(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, marker := range []string{
		"const maxArtifactPlaybackStates = 32;",
		"function captureArtifactVideoPlayback()",
		"function restoreArtifactVideoPlayback(states)",
		".artifact-card[data-artifact-id] video",
		"states.set(artifactID, {",
		"loadedmetadata",
		"recovery never autoplays new media",
		"const playbackStates = captureArtifactVideoPlayback();",
		"restoreArtifactVideoPlayback(playbackStates);",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("static page missing video playback reconciliation marker %q", marker)
		}
	}
	reconcileStart := strings.Index(body, "async function reconcileConversation(sessionID, promptText)")
	if reconcileStart < 0 {
		t.Fatal("static page missing reconcileConversation implementation")
	}
	reconcile := body[reconcileStart:]
	captureIndex := strings.Index(reconcile, "const playbackStates = captureArtifactVideoPlayback();")
	renderIndex := strings.Index(reconcile, "renderHistory(historyMessages,")
	restoreIndex := strings.Index(reconcile, "restoreArtifactVideoPlayback(playbackStates);")
	if captureIndex < 0 || renderIndex < 0 || restoreIndex < 0 || captureIndex > renderIndex || renderIndex > restoreIndex {
		t.Fatalf("video playback recovery ordering invalid: capture=%d render=%d restore=%d", captureIndex, renderIndex, restoreIndex)
	}
	selectStart := strings.Index(body, "async function selectSession(session)")
	if selectStart < 0 || selectStart >= reconcileStart {
		t.Fatal("static page missing selectSession implementation")
	}
	if strings.Contains(body[selectStart:reconcileStart], "captureArtifactVideoPlayback") || strings.Contains(body[selectStart:reconcileStart], "restoreArtifactVideoPlayback") {
		t.Fatal("ordinary selectSession must not use reconciliation playback preservation")
	}
}

func TestGatewayStaticPageRemovesEmptyThinkingBubble(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		"function removeEmptyAssistantBubble()",
		"assistantText.trim()",
		"visibleText !== '' && visibleText !== 'thinking'",
		"assistantBubble.closest('.message-row')",
		"assistantBubble = null;",
		"function finishAssistant()",
		"if (removeEmptyAssistantBubble()) return;",
		"case 'artifact': if (event.artifact) addArtifactCard(event.artifact); break;",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing empty thinking bubble marker %q", marker)
		}
	}
}

func TestGatewayStaticPageDeduplicatesTerminalRunErrors(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		"terminalErrorShown: false",
		"function addRunErrorNotice(error)",
		"if (state.terminalErrorShown) return;",
		"state.terminalErrorShown = false;",
		"case 'run_failure': addRunErrorNotice(event.error); break;",
		"if (event.error) addRunErrorNotice(event.error);",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing terminal error de-duplication marker %q", marker)
		}
	}
}

func TestGatewayStaticPageHomeIdleView(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		"homeMode: true",
		"function enterHomeMode()",
		"state.homeMode = true",
		"setActiveSession(null);",
		"event.preventDefault(); enterHomeMode();",
		"if (!state.homeMode && !state.session && list.length)",
		"async function createSessionForCurrentProject()",
		"state.homeMode = false;",
		"if (!state.selectedID && state.homeMode)",
		"/api/sessions/' + encodeURIComponent(sessionIDValue) + '/messages",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing home idle marker %q", marker)
		}
	}
	refreshStart := strings.Index(page, "async function refresh()")
	if refreshStart < 0 {
		t.Fatal("static page missing refresh/search function boundary")
	}
	refreshEnd := strings.Index(page[refreshStart:], "async function searchSessions()")
	if refreshEnd < 0 {
		t.Fatal("static page missing refresh/search function boundary")
	}
	refreshBody := page[refreshStart : refreshStart+refreshEnd]
	if strings.Contains(refreshBody, "if (!state.session && list.length)") {
		t.Fatal("refresh still auto-selects the newest session while home mode is active")
	}
	sendStart := strings.Index(page, "async function send()")
	if sendStart < 0 {
		t.Fatal("static page missing send/stop function boundary")
	}
	sendEnd := strings.Index(page[sendStart:], "async function stop()")
	if sendEnd < 0 {
		t.Fatal("static page missing send/stop function boundary")
	}
	sendBody := page[sendStart : sendStart+sendEnd]
	createIndex := strings.Index(sendBody, "if (!state.selectedID && state.homeMode)")
	messageIndex := strings.Index(sendBody, "/api/sessions/' + encodeURIComponent(sessionIDValue) + '/messages")
	if createIndex < 0 || messageIndex < 0 || createIndex > messageIndex {
		t.Fatal("send does not create a durable session before posting the first idle-home prompt")
	}
}

func TestGatewayEmbeddedHTMLUsesNoStoreCache(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	for _, path := range []string{"/", "/index.html"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s response status = %d", path, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Fatalf("%s cache control = %q", path, got)
		}
		if !strings.Contains(response.Body.String(), "auto_retrieved") {
			t.Fatalf("%s response did not serve embedded gateway HTML", path)
		}
	}
}

func TestGatewayMediaSelectionResolvesOpaqueCandidateAndStartsRun(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	app, err := bee.NewApplication(config, testProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := bee.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), store)
	if err != nil {
		t.Fatal(err)
	}
	app.ArtifactStore = artifacts
	app.Media = bee.NewMediaService(artifacts)
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	app.Media.HTTPClient = mediaHTTPClient{body: imageData.Bytes()}

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"Bee","img_src":"https://example.com/bee.png","url":"https://example.com/source"}]}`)
	}))
	defer searchServer.Close()
	webClient, err := bee.NewWebClient(bee.WebConfig{Enabled: true, SearchEndpoint: searchServer.URL, TimeoutSeconds: 2, MaxResults: 2, MaxChars: 256})
	if err != nil {
		t.Fatal(err)
	}
	webClient.AllowPrivateTargets = true
	app.MediaSearch = bee.NewMediaSearchService(webClient)
	session, err := store.Create(bee.SessionMetadata{Title: "media"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.MediaSearch.Search(context.Background(), session.ID, bee.MediaSearchRequest{Kind: "image", Query: "bee"})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("search result=%#v err=%v", result, err)
	}
	service, err := NewForApplication(app, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())
	payload, _ := json.Marshal(map[string]string{"candidate_id": result.Candidates[0].CandidateID, "token": result.Candidates[0].Token})
	record := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(session.ID)+"/media/select", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	service.Handler().ServeHTTP(record, request)
	if record.Code != http.StatusAccepted {
		t.Fatalf("selection status=%d body=%s", record.Code, record.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(record.Body.Bytes(), &started); err != nil || started["run_id"] == "" {
		t.Fatalf("selection response=%s err=%v", record.Body.String(), err)
	}
	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(session.ID)+"/media/select", bytes.NewReader(payload))
	secondRequest.Header.Set("Content-Type", "application/json")
	service.Handler().ServeHTTP(second, secondRequest)
	if second.Code != http.StatusBadRequest {
		t.Fatalf("reused candidate status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestGatewayArtifactMetadataDownloadPreviewAndSessionList(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	store, err := bee.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), service.App().Store)
	if err != nil {
		t.Fatal(err)
	}
	service.App().ArtifactStore = store
	created, err := service.App().Store.Create(bee.SessionMetadata{Title: "artifact api"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Create(context.Background(), created.ID, "run-api", bee.ArtifactInput{Filename: "note.txt", MediaType: "text/plain", Content: []byte("preview me")})
	if err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(created.ID)+"/artifacts", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), artifact.ID) {
		t.Fatalf("artifact list = %d %s", list.Code, list.Body.String())
	}
	meta := httptest.NewRecorder()
	handler.ServeHTTP(meta, httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID, nil))
	if meta.Code != http.StatusOK || strings.Contains(meta.Body.String(), store.Root()) {
		t.Fatalf("artifact metadata = %d %s", meta.Code, meta.Body.String())
	}
	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID+"/preview", nil))
	if preview.Code != http.StatusOK || preview.Body.String() != "preview me" || preview.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(preview.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("artifact preview = %d headers=%v body=%q", preview.Code, preview.Header(), preview.Body.String())
	}
	download := httptest.NewRecorder()
	handler.ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID+"/download", nil))
	if download.Code != http.StatusOK || download.Body.String() != "preview me" || download.Header().Get("Accept-Ranges") != "bytes" || download.Header().Get("Content-Length") != "10" || download.Header().Get("Content-Type") != "text/plain" || download.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(download.Header().Get("Content-Disposition"), "note.txt") {
		t.Fatalf("artifact download = %d headers=%v body=%q", download.Code, download.Header(), download.Body.String())
	}

	tests := []struct {
		name       string
		rangeValue string
		status     int
		content    string
		contentRng string
		length     string
	}{
		{name: "prefix", rangeValue: "bytes=0-6", status: http.StatusPartialContent, content: "preview", contentRng: "bytes 0-6/10", length: "7"},
		{name: "suffix", rangeValue: "bytes=-2", status: http.StatusPartialContent, content: "me", contentRng: "bytes 8-9/10", length: "2"},
		{name: "explicit", rangeValue: "bytes=2-5", status: http.StatusPartialContent, content: "evie", contentRng: "bytes 2-5/10", length: "4"},
		{name: "open ended", rangeValue: "bytes=8-", status: http.StatusPartialContent, content: "me", contentRng: "bytes 8-9/10", length: "2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID+"/download", nil)
			request.Header.Set("Range", test.rangeValue)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Body.String() != test.content || response.Header().Get("Accept-Ranges") != "bytes" || response.Header().Get("Content-Range") != test.contentRng || response.Header().Get("Content-Length") != test.length || response.Header().Get("Content-Type") != "text/plain" || response.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment; filename=\"note.txt\"") {
				t.Fatalf("range %q = %d headers=%v body=%q", test.rangeValue, response.Code, response.Header(), response.Body.String())
			}
		})
	}

	for _, rangeValue := range []string{"bytes=10-", "bytes=4-3", "bytes=0-1,2-3", "items=0-1", "bytes=-0", "bytes=abc"} {
		t.Run("reject "+rangeValue, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID+"/download", nil)
			request.Header.Set("Range", rangeValue)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusRequestedRangeNotSatisfiable || response.Header().Get("Content-Range") != "bytes */10" || response.Header().Get("Accept-Ranges") != "bytes" || strings.Contains(response.Body.String(), store.Root()) || strings.Contains(response.Header().Get("Content-Disposition"), store.Root()) {
				t.Fatalf("rejected range %q = %d headers=%v body=%q", rangeValue, response.Code, response.Header(), response.Body.String())
			}
		})
	}
	multiple := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+artifact.ID+"/download", nil)
	multiple.Header.Add("Range", "bytes=0-1")
	multiple.Header.Add("Range", "bytes=2-3")
	multipleResponse := httptest.NewRecorder()
	handler.ServeHTTP(multipleResponse, multiple)
	if multipleResponse.Code != http.StatusRequestedRangeNotSatisfiable || multipleResponse.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("multiple ranges = %d headers=%v body=%q", multipleResponse.Code, multipleResponse.Header(), multipleResponse.Body.String())
	}
}

func TestGatewayStaticPageIncludesMascotFavicon(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("gateway page status = %d", response.Code)
	}
	for _, favicon := range []string{
		`<link rel="icon" type="image/svg+xml" href="/assets/bee-mascot.svg?v=20260919-perf1">`,
		`<link rel="icon" type="image/png" sizes="512x512" href="/assets/bee-mascot-home.png?v=20260919-perf1">`,
		`<link rel="apple-touch-icon" sizes="512x512" href="/assets/bee-mascot-home.png?v=20260919-perf1">`,
	} {
		faviconPos := strings.Index(page, favicon)
		headEnd := strings.Index(page, "</head>")
		if faviconPos < 0 || headEnd < 0 || faviconPos > headEnd {
			t.Fatalf("mascot favicon marker is not in the embedded page head: %q", favicon)
		}
	}
}

func TestGatewayStaticPageTopbarKeepsOnlyBeeBrand(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("gateway page status = %d", response.Code)
	}
	for _, marker := range []string{
		`--topbar-height: 56px;`,
		`.topbar {`,
		`flex: 0 0 var(--topbar-height); min-height: var(--topbar-height); padding: 0 24px;`,
		`.sidebar-backdrop { display: block; position: fixed; inset: var(--topbar-height) 0 0;`,
		`inset: var(--topbar-height) auto 0 0;`,
		`height: calc(100dvh - var(--topbar-height));`,
		`.brand { display: inline-flex; align-items: center; gap: 9px;`,
		`.brand-avatar { display: block; width: 30px; height: 30px;`,
		`@media (max-width: 760px) {`,
		`.topbar { padding: 0 15px; }`,
		`<a class="brand" href="/" aria-label="Bee home"><img class="brand-avatar" src="/assets/bee-mascot.svg?v=20260919-perf1" alt=""><span class="brand-mark">BEE</span></a>`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing topbar marker %q", marker)
		}
	}
	for _, stale := range []string{
		`flex: 0 0 64px; min-height: 64px;`,
		`inset: 64px 0 0;`,
		`inset: 64px auto 0 0;`,
		`height: calc(100dvh - 64px);`,
	} {
		if strings.Contains(page, stale) {
			t.Fatalf("static page still contains stale 64px topbar dependency %q", stale)
		}
	}
	for _, removed := range []string{
		"brand-sub",
		"gateway-state",
		"local gateway / durable sessions",
		"loopback only",
		"● loopback only",
	} {
		if strings.Contains(page, removed) {
			t.Fatalf("static page still contains removed topbar content %q", removed)
		}
	}
}

func TestRenderIndexPageEscapesUIConfigAndIncludesTimeZone(t *testing.T) {
	page := string(renderIndexPage(`"><script>alert(1)</script>`, `"><img src=x>`, "America/Los_Angeles"))
	if !strings.Contains(page, `"timeZone":"America/Los_Angeles"`) {
		t.Fatalf("rendered page omitted time zone: %s", page[:min(len(page), 512)])
	}
	if strings.Contains(page, `"><script>alert(1)</script>`) || strings.Contains(page, `"><img src=x>`) {
		t.Fatal("rendered UI config leaked raw HTML strings")
	}
	if !strings.Contains(page, `\u003cscript\u003e`) {
		t.Fatal("rendered UI config did not JSON-escape HTML-sensitive input")
	}
}

func TestGatewayStaticPageConfiguresMessageAvatars(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userAvatarPath := filepath.Join(t.TempDir(), "user.png")
	beeAvatarPath := filepath.Join(t.TempDir(), "bee.png")
	if err := os.WriteFile(userAvatarPath, beeMascotAsset, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beeAvatarPath, beeMascotAsset, 0o600); err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	config.Provider.Model = "avatar-test"
	config.Web.UserAvatarFile = userAvatarPath
	config.Web.BeeAvatarFile = beeAvatarPath
	service, err := New(Config{BeeConfig: config, SessionStore: store, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("gateway page status = %d", response.Code)
	}
	if !strings.Contains(page, `window.__BEE_UI_CONFIG__ = {"userAvatar":"/assets/user-avatar","beeAvatar":"/assets/bee-avatar","timeZone":"UTC"};`) {
		t.Fatalf("gateway page did not include configured avatar sources: %q", page)
	}
	if strings.Contains(page, `class="empty-state-mascot"`) || strings.Contains(page, `alt="Bee mascot"`) {
		t.Fatal("central mascot empty-state markup was not removed")
	}
	for _, endpoint := range []string{"/assets/user-avatar", "/assets/bee-avatar"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("configured avatar %s = %d content-type=%q", endpoint, response.Code, response.Header().Get("Content-Type"))
		}
		if !bytes.Equal(response.Body.Bytes(), beeMascotAsset) {
			t.Fatalf("configured avatar %s did not serve the loaded file", endpoint)
		}
	}
	missingEndpoint := httptest.NewRecorder()
	service.Handler().ServeHTTP(missingEndpoint, httptest.NewRequest(http.MethodGet, "/assets/user-avatar.png", nil))
	if missingEndpoint.Code != http.StatusNotFound {
		t.Fatalf("arbitrary avatar endpoint = %d, want 404", missingEndpoint.Code)
	}
}

func TestGatewayRejectsInvalidConfiguredAvatarFilesAtStartup(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T) string
	}{
		{name: "missing", make: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.png") }},
		{name: "oversized", make: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "oversized.png")
			if err := os.WriteFile(path, make([]byte, MaxConfiguredAvatarBytes+1), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "unsupported", make: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "unsupported.txt")
			if err := os.WriteFile(path, []byte("not an image"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := bee.NewSessionStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			config := bee.DefaultConfig()
			config.Provider.Model = "avatar-invalid-test"
			config.Web.UserAvatarFile = test.make(t)
			if service, err := New(Config{BeeConfig: config, SessionStore: store, ListenAddr: "127.0.0.1:0"}); err == nil {
				service.Stop(context.Background())
				t.Fatal("invalid configured avatar unexpectedly started gateway")
			}
		})
	}
}

func TestGatewayUnconfiguredAvatarEndpointsUseSafeFallbacks(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	for _, endpoint := range []string{"/assets/user-avatar", "/assets/bee-avatar"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unconfigured avatar %s = %d, want 404", endpoint, response.Code)
		}
	}
}

func TestGatewayServesEmbeddedMascotSVGAsset(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/bee-mascot.svg", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("mascot response status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "image/svg+xml; charset=utf-8" {
		t.Fatalf("mascot content type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("mascot cache control = %q", got)
	}
	if got := response.Header().Get("Content-Security-Policy"); got != "default-src 'none'; style-src 'unsafe-inline'" {
		t.Fatalf("mascot content security policy = %q", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("mascot nosniff header = %q", got)
	}
	body := response.Body.String()
	var document struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal(response.Body.Bytes(), &document); err != nil || document.XMLName.Local != "svg" {
		t.Fatalf("mascot response is not well-formed SVG: %v", err)
	}
	if !strings.Contains(body, `<svg xmlns="http://www.w3.org/2000/svg"`) || !strings.Contains(body, `viewBox="0 0 2160 2160"`) {
		t.Fatal("mascot response is not the sanitized SVG asset")
	}
	for _, marker := range []string{"<metadata", "<script", "<foreignObject", "<image", "href=", "xlink:href", "javascript:", "url("} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(marker)) {
			t.Fatalf("mascot response contains disallowed SVG marker %q", marker)
		}
	}
	if len(response.Body.Bytes()) < 1024 {
		t.Fatalf("mascot response unexpectedly small: %d bytes", len(response.Body.Bytes()))
	}
}

func TestGatewayKeepsPNGMascotRollbackAsset(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/bee-mascot.png", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("rollback mascot response = %d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	if !bytes.HasPrefix(response.Body.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("rollback mascot response is not a PNG")
	}
}

func TestGatewayServesSquareHomeScreenMascotPNG(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/bee-mascot-home.png?v=20260919-perf1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("home-screen mascot response status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("home-screen mascot content type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("home-screen mascot cache control = %q", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("home-screen mascot nosniff header = %q", got)
	}
	decoded, err := png.Decode(response.Body)
	if err != nil {
		t.Fatalf("decode home-screen mascot PNG: %v", err)
	}
	if got := decoded.Bounds().Size(); got.X != 512 || got.Y != 512 {
		t.Fatalf("home-screen mascot dimensions = %v, want 512x512", got)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatalf("home-screen mascot corner pixel alpha = %d, want transparent", alpha)
	}
}

func TestGatewayKeepsPreviousSVGMascotRollbackAsset(t *testing.T) {
	asset, err := os.ReadFile(filepath.Join("assets", "bee-mascot-rollback.svg"))
	if err != nil {
		t.Fatalf("read SVG rollback asset: %v", err)
	}
	var document struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal(asset, &document); err != nil || document.XMLName.Local != "svg" {
		t.Fatalf("SVG rollback asset is not well-formed: %v", err)
	}
	if !strings.Contains(string(asset), `viewBox="0 0 2160 2160"`) {
		t.Fatal("SVG rollback asset has unexpected dimensions")
	}
	if len(asset) < 1024 {
		t.Fatalf("SVG rollback asset unexpectedly small: %d bytes", len(asset))
	}
}

func TestGatewayRejectsMalformedAndOversizedJSON(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "malformed", body: `{`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"unexpected":true}`, want: http.StatusBadRequest},
		{name: "oversized", body: `{"title":"` + strings.Repeat("x", MaxJSONBodyBytes) + `"}`, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			service.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(tc.body)))
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGatewayMessageEventsAndCancellation(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	create := httptest.NewRecorder()
	service.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	var metadata bee.SessionMetadata
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != DefaultSessionTitle {
		t.Fatalf("default session title = %q, want %q", metadata.Title, DefaultSessionTitle)
	}

	message := httptest.NewRecorder()
	path := "/api/sessions/" + string(metadata.ID) + "/messages"
	service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prompt":"hi"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("message = %d %s", message.Code, message.Body.String())
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.RunID == "" {
		t.Fatal("missing run id")
	}

	stream := httptest.NewRecorder()
	service.Handler().ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), `"type":"run_end"`) {
		t.Fatalf("events = %d %s", stream.Code, stream.Body.String())
	}

	blocking := newTestService(t, blockingProvider{})
	defer blocking.Stop(context.Background())
	create = httptest.NewRecorder()
	blocking.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	if create.Code != http.StatusCreated {
		t.Fatal(create.Code)
	}
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	message = httptest.NewRecorder()
	blocking.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"wait"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("blocking message = %d %s", message.Code, message.Body.String())
	}
	if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	cancel := httptest.NewRecorder()
	blocking.Handler().ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, "/api/runs/"+started.RunID+"/cancel", nil))
	if cancel.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", cancel.Code, cancel.Body.String())
	}
}

func TestGatewayRunEventsResumeFromOffset(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	create := httptest.NewRecorder()
	service.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	var metadata bee.SessionMetadata
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	message := httptest.NewRecorder()
	service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"resume"}`)))
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	full := httptest.NewRecorder()
	service.Handler().ServeHTTP(full, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), "id: 3\n") {
		t.Fatalf("full events = %d %s", full.Code, full.Body.String())
	}
	resumed := httptest.NewRecorder()
	service.Handler().ServeHTTP(resumed, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events?from=3", nil))
	body := resumed.Body.String()
	if resumed.Code != http.StatusOK || !strings.HasPrefix(body, ": connected\n\n") || strings.Contains(body, "id: 0\n") || strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") || !strings.Contains(body, "id: 3\n") || !strings.Contains(body, `"type":"run_end"`) {
		t.Fatalf("resumed events = %d %s", resumed.Code, body)
	}
	invalid := httptest.NewRecorder()
	service.Handler().ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events?from=-1", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid offset status = %d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestGatewayRunStatusAndCursorContract(t *testing.T) {
	record := newRunRecord("run-status", context.Background(), func() {})
	record.publish(bee.RunEvent{Type: bee.RunStart, RunID: record.id})
	record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id})
	service := &Service{ctx: context.Background(), runs: map[string]*runRecord{record.id: record}}

	status := httptest.NewRecorder()
	service.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id, nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"done"`) || !strings.Contains(status.Body.String(), `"terminal_observed":true`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	resumed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events?after=0", nil)
	service.Handler().ServeHTTP(resumed, request)
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), "id: 1\n") || strings.Contains(resumed.Body.String(), "id: 0\n") {
		t.Fatalf("after replay = %d %s", resumed.Code, resumed.Body.String())
	}
	invalid := httptest.NewRecorder()
	service.Handler().ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events?after=+1", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestGatewayRunEventsExplicitGapAndCompactTerminal(t *testing.T) {
	gapRecord := newRunRecord("run-gap", context.Background(), func() {})
	for i := 0; i < MaxRunEvents+8; i++ {
		gapRecord.publish(bee.RunEvent{Type: bee.RunAssistantDelta, RunID: gapRecord.id, Delta: "late"})
	}
	gapRecord.publish(bee.RunEvent{Type: bee.RunEnd, RunID: gapRecord.id})
	service := &Service{ctx: context.Background(), runs: map[string]*runRecord{gapRecord.id: gapRecord}}
	status := httptest.NewRecorder()
	service.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/runs/"+gapRecord.id, nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"replay_gap":true`) || !strings.Contains(status.Body.String(), `"gap_start_cursor":1024`) || !strings.Contains(status.Body.String(), `"gap_end_cursor":1031`) {
		t.Fatalf("gap status = %d %s", status.Code, status.Body.String())
	}
	for _, path := range []string{
		"/api/runs/" + gapRecord.id + "/events",
		"/api/runs/" + gapRecord.id + "/events?after=" + strconv.Itoa(MaxRunEvents-1),
	} {
		gap := httptest.NewRecorder()
		service.Handler().ServeHTTP(gap, httptest.NewRequest(http.MethodGet, path, nil))
		if gap.Code != http.StatusConflict || !strings.Contains(gap.Body.String(), `"gap":true`) {
			t.Fatalf("gap replay %s = %d %s", path, gap.Code, gap.Body.String())
		}
	}
	resumed := httptest.NewRecorder()
	service.Handler().ServeHTTP(resumed, httptest.NewRequest(http.MethodGet, "/api/runs/"+gapRecord.id+"/events?after=1031", nil))
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), `"type":"run_end"`) {
		t.Fatalf("post-gap terminal replay = %d %s", resumed.Code, resumed.Body.String())
	}

	compactRecord := newRunRecord("run-compact-terminal", context.Background(), func() {})
	compactRecord.publish(bee.RunEvent{Type: bee.RunStart, RunID: compactRecord.id})
	compactRecord.publish(bee.RunEvent{Type: bee.RunEnd, RunID: compactRecord.id, Response: &bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: strings.Repeat("x", MaxRunEventBytes)}}})
	compactEvents, _, compactDone := compactRecord.after(0)
	if !compactDone || len(compactEvents) != 2 || compactEvents[1].Response != nil {
		t.Fatalf("stored compact terminal = done=%t events=%#v", compactDone, compactEvents)
	}
	compactService := &Service{ctx: context.Background(), runs: map[string]*runRecord{compactRecord.id: compactRecord}}
	compact := httptest.NewRecorder()
	compactService.Handler().ServeHTTP(compact, httptest.NewRequest(http.MethodGet, "/api/runs/"+compactRecord.id+"/events", nil))
	if compact.Code != http.StatusOK || !strings.Contains(compact.Body.String(), `"type":"run_end"`) || strings.Contains(compact.Body.String(), strings.Repeat("x", 1024)) || compact.Body.Len() > 4096 {
		t.Fatalf("compact terminal = %d bytes=%d body=%s", compact.Code, compact.Body.Len(), compact.Body.String())
	}
}

func TestRunRecordRetainsTerminalEventsBeyondOrdinaryCap(t *testing.T) {
	record := newRunRecord("run-terminal-buffer", context.Background(), func() {})
	for i := 0; i < MaxRunEvents+32; i++ {
		record.publish(bee.RunEvent{Type: bee.RunAssistantDelta, Delta: "ordinary"})
	}
	record.publish(bee.RunEvent{Type: bee.RunFailure, Error: "failed"})
	record.publish(bee.RunEvent{Type: bee.RunFailure, Error: "duplicate failure"})
	record.publish(bee.RunEvent{Type: bee.RunEnd, Error: "failed"})
	record.publish(bee.RunEvent{Type: bee.RunEnd, Error: "duplicate end"})

	events, _, done := record.after(0)
	if !done {
		t.Fatal("run record is not marked done after RunEnd")
	}
	if got, want := len(events), MaxRunEvents+MaxRunTerminalEvents; got != want {
		t.Fatalf("retained event count = %d, want %d", got, want)
	}
	if len(events) > MaxRunRecordEvents {
		t.Fatalf("retained event count = %d exceeds bounded cap %d", len(events), MaxRunRecordEvents)
	}
	if events[MaxRunEvents].Type != bee.RunFailure || events[MaxRunEvents+1].Type != bee.RunEnd {
		t.Fatalf("terminal event order = %#v, want failure then end", events[MaxRunEvents:])
	}

	resumed, _, resumedDone := record.after(MaxRunEvents)
	if !resumedDone || len(resumed) != MaxRunTerminalEvents {
		t.Fatalf("resumed terminal events = len(%d), done=%t; want len(%d), done=true", len(resumed), resumedDone, MaxRunTerminalEvents)
	}
	if resumed[0].Type != bee.RunFailure || resumed[1].Type != bee.RunEnd {
		t.Fatalf("resumed terminal event order = %#v", resumed)
	}
	pastEnd, _, pastEndDone := record.after(len(events))
	if !pastEndDone || len(pastEnd) != 0 {
		t.Fatalf("after terminal events = len(%d), done=%t; want len(0), done=true", len(pastEnd), pastEndDone)
	}
}

func TestGatewayRunEventsFreshAndResumedRetainTerminalEvents(t *testing.T) {
	record := newRunRecord("run-terminal-sse", context.Background(), func() {})
	for i := 0; i < MaxRunEvents+32; i++ {
		record.publish(bee.RunEvent{Type: bee.RunAssistantDelta, Delta: "ordinary"})
	}
	record.publish(bee.RunEvent{Type: bee.RunFailure, Error: "failed"})
	record.publish(bee.RunEvent{Type: bee.RunEnd, Error: "failed"})
	service := &Service{ctx: context.Background(), runs: map[string]*runRecord{record.id: record}}

	fresh := httptest.NewRecorder()
	service.handleRunEvents(fresh, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events", nil), record.id)
	if fresh.Code != http.StatusConflict || !strings.Contains(fresh.Body.String(), `"gap":true`) {
		t.Fatalf("fresh gap response = %d %s", fresh.Code, fresh.Body.String())
	}

	resumed := httptest.NewRecorder()
	service.handleRunEvents(resumed, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events?after=1055", nil), record.id)
	resumedBody := resumed.Body.String()
	if resumed.Code != http.StatusOK || !strings.Contains(resumedBody, "id: 1056\n") || !strings.Contains(resumedBody, "id: 1057\n") || strings.Count(resumedBody, `"type":"run_failure"`) != 1 || strings.Count(resumedBody, `"type":"run_end"`) != 1 {
		t.Fatalf("resumed terminal SSE = %d %s", resumed.Code, resumedBody)
	}
	if strings.Index(resumedBody, `"type":"run_failure"`) > strings.Index(resumedBody, `"type":"run_end"`) {
		t.Fatalf("resumed terminal SSE reversed failure/end order: %s", resumedBody)
	}
}

func TestGatewayRunEventsRetainRawSeparateReasoningAndIncompleteMarker(t *testing.T) {
	service := newTestService(t, testProvider{})
	record := newRunRecord("reasoning-run", service.ctx, func() {})
	raw := "token=super-secret password=hunter2 (/home/testuser/.bee /opt/bee /mnt/data /root/private) path:`/srv/bee/state` https://example.com/home/bee"
	// Split the first secret marker across events. Retained/SSE delivery must
	// preserve each raw provider reasoning delta exactly.
	record.publish(bee.RunEvent{Type: bee.RunReasoningDelta, RunID: record.id, Reasoning: &bee.ReasoningRunEvent{Delta: "tok", Step: 1}})
	record.publish(bee.RunEvent{Type: bee.RunReasoningDelta, RunID: record.id, Reasoning: &bee.ReasoningRunEvent{Delta: raw[len("tok"):], Step: 1}})
	record.publish(bee.RunEvent{Type: bee.RunReasoningFinal, RunID: record.id, Reasoning: &bee.ReasoningRunEvent{Content: raw, Step: 1, Incomplete: true, ErrorClass: "timeout"}})
	record.publish(bee.RunEvent{Type: bee.RunEnd, RunID: record.id, Error: "timeout"})
	service.mu.Lock()
	service.runs[record.id] = record
	service.mu.Unlock()
	writer := httptest.NewRecorder()
	service.handleRunEvents(writer, httptest.NewRequest(http.MethodGet, "/api/runs/"+record.id+"/events", nil), record.id)
	body := writer.Body.String()
	if !strings.Contains(body, `"type":"reasoning_delta"`) || !strings.Contains(body, `"incomplete":true`) || !strings.Contains(body, `"error_class":"timeout"`) || !strings.Contains(body, "super-secret") || !strings.Contains(body, "hunter2") || !strings.Contains(body, "/home/testuser") || !strings.Contains(body, "/opt/bee") || !strings.Contains(body, "/mnt/data") || !strings.Contains(body, "/root/private") || !strings.Contains(body, "/srv/bee") || !strings.Contains(body, "https://example.com/home/bee") {
		t.Fatalf("reasoning SSE = %s", body)
	}
}

func TestSessionHistoryReasoningTimelineIsPaginatedRawAndSeparate(t *testing.T) {
	service := newTestService(t, testProvider{})
	session, err := service.store.Create(bee.SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "visible"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendReasoning(bee.ReasoningEvent{RunID: "run", Profile: "main", Model: "m", Step: 1, Content: `thinking /opt/bee/state token=secret`, Complete: true, FinishReason: "stop"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=1", nil)
	writer := httptest.NewRecorder()
	service.handleSessionHistory(writer, request, session.ID)
	if writer.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
	}
	body := writer.Body.String()
	if !strings.Contains(body, "/opt/bee/state") || !strings.Contains(body, "token=secret") {
		t.Fatalf("history did not preserve raw reasoning: %s", body)
	}
	if !strings.Contains(body, `"type":"reasoning"`) || !strings.Contains(body, `"run_id":"run"`) {
		t.Fatalf("history omitted reasoning timeline: %s", body)
	}
	var response struct {
		Messages []bee.Message    `json:"messages"`
		Timeline []map[string]any `json:"timeline"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, message := range response.Messages {
		if strings.Contains(message.Content, "thinking") {
			t.Fatal("reasoning leaked into ordinary messages")
		}
	}
}

func TestLegacyHistoryOmitsReasoningAndTimelinePagingStaysBounded(t *testing.T) {
	service := newTestService(t, testProvider{})
	session, err := service.store.Create(bee.SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	pending := make([]bee.Event, 0, 3000)
	for index := 0; index < 3000; index++ {
		event, eventErr := bee.NewReasoningEvent(session.ID, bee.ReasoningEvent{RunID: "large", Profile: "main", Model: "m", Step: index + 1, Content: strings.Repeat("x", 64), Complete: true, FinishReason: "stop"})
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		pending = append(pending, event)
	}
	if _, err := service.store.AppendEvents(session.ID, pending); err != nil {
		t.Fatal(err)
	}
	legacy := httptest.NewRecorder()
	service.handleSessionHistory(legacy, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history", nil), session.ID)
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy status=%d", legacy.Code)
	}
	var legacyPayload struct {
		Timeline  []map[string]any `json:"timeline"`
		Reasoning []any            `json:"reasoning"`
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyPayload); err != nil {
		t.Fatal(err)
	}
	if len(legacyPayload.Reasoning) != 0 || len(legacyPayload.Timeline) > maxSessionTimelineItems || legacy.Body.Len() > 2<<20 {
		t.Fatalf("legacy history unbounded/duplicated: reasoning=%d timeline=%d bytes=%d", len(legacyPayload.Reasoning), len(legacyPayload.Timeline), legacy.Body.Len())
	}
	first := httptest.NewRecorder()
	service.handleSessionHistory(first, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=7", nil), session.ID)
	var page struct {
		Timeline []struct {
			Seq uint64 `json:"seq"`
		} `json:"timeline"`
		NextBefore string `json:"next_before"`
		HasMore    bool   `json:"has_more"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Timeline) != 7 || !page.HasMore || page.NextBefore == "" {
		t.Fatalf("first page=%#v", page)
	}
	second := httptest.NewRecorder()
	service.handleSessionHistory(second, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=7&before="+page.NextBefore, nil), session.ID)
	var previous struct {
		Timeline []struct {
			Seq uint64 `json:"seq"`
		} `json:"timeline"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &previous); err != nil {
		t.Fatal(err)
	}
	for _, item := range previous.Timeline {
		if item.Seq >= parseTestUint(page.NextBefore) {
			t.Fatalf("before pagination leaked seq=%d before=%s", item.Seq, page.NextBefore)
		}
	}
}

func parseTestUint(raw string) uint64 { value, _ := strconv.ParseUint(raw, 10, 64); return value }

func TestGatewaySessionHistoryReplayAndHumanTitle(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	create := httptest.NewRecorder()
	service.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", create.Code, create.Body.String())
	}
	var metadata bee.SessionMetadata
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	message := httptest.NewRecorder()
	service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"remember this title"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("message = %d %s", message.Code, message.Body.String())
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	stream := httptest.NewRecorder()
	service.Handler().ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
	if stream.Code != http.StatusOK {
		t.Fatalf("events = %d %s", stream.Code, stream.Body.String())
	}
	history := httptest.NewRecorder()
	service.Handler().ServeHTTP(history, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(metadata.ID)+"/history", nil))
	if history.Code != http.StatusOK {
		t.Fatalf("history = %d %s", history.Code, history.Body.String())
	}
	var replay struct {
		Metadata bee.SessionMetadata `json:"metadata"`
		Messages []bee.Message       `json:"messages"`
	}
	if err := jsonUnmarshal(history.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Metadata.Title != "remember this title" || len(replay.Messages) != 2 || replay.Messages[0].Content != "remember this title" || replay.Messages[1].Content != "hello" {
		t.Fatalf("replay = %#v", replay)
	}
	legacy := httptest.NewRecorder()
	service.Handler().ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(metadata.ID), nil))
	if legacy.Code != http.StatusOK || !strings.Contains(legacy.Body.String(), "remember this title") {
		t.Fatalf("legacy history = %d %s", legacy.Code, legacy.Body.String())
	}
}

func TestGatewaySessionHistoryTimelineOrdersMessagesAndArtifacts(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	artifacts, err := bee.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), service.App().Store)
	if err != nil {
		t.Fatal(err)
	}
	service.App().ArtifactStore = artifacts
	session, err := service.App().Store.Create(bee.SessionMetadata{Title: "timeline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "before"}); err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.Create(context.Background(), session.ID, "run-timeline", bee.ArtifactInput{Filename: "clip.mp4", MediaType: "video/mp4", Content: []byte("video")})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "after"}); err != nil {
		t.Fatal(err)
	}
	history := httptest.NewRecorder()
	service.Handler().ServeHTTP(history, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history", nil))
	if history.Code != http.StatusOK {
		t.Fatalf("history = %d %s", history.Code, history.Body.String())
	}
	var replay struct {
		Timeline []sessionTimelineItem `json:"timeline"`
	}
	if err := jsonUnmarshal(history.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if len(replay.Timeline) != 3 || replay.Timeline[0].Type != "message" || replay.Timeline[1].Type != "artifact" || replay.Timeline[2].Type != "message" {
		t.Fatalf("timeline order = %#v", replay.Timeline)
	}
	if replay.Timeline[1].Artifact == nil || replay.Timeline[1].Artifact.ID != artifact.ID || replay.Timeline[0].Message.Content != "before" || replay.Timeline[2].Message.Content != "after" {
		t.Fatalf("timeline payload = %#v", replay.Timeline)
	}
	if strings.Contains(history.Body.String(), artifacts.Root()) {
		t.Fatal("history exposed an artifact filesystem path")
	}
}

func TestGatewayHistoryRedactsFaithfulDurableSecretsAtDisplayBoundary(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	session, err := service.App().Store.Create(bee.SessionMetadata{Title: "secret history"})
	if err != nil {
		t.Fatal(err)
	}
	rawToken := "raw-gateway-token-123"
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "token=" + rawToken}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "assistant token=" + rawToken, ToolCalls: []bee.ToolCall{{ID: "call-1", Name: "exec", Arguments: `{"command":"printf 'token=raw-gateway-token-123'","token":"raw-gateway-token-123"}`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleTool, ToolCallID: "call-1", Name: "exec", Content: "result token=" + rawToken}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(service.App().Store.Root(), string(session.ID)+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), rawToken) {
		t.Fatal("durable session did not retain the raw token")
	}
	history := httptest.NewRecorder()
	service.Handler().ServeHTTP(history, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history", nil))
	if history.Code != http.StatusOK {
		t.Fatalf("history = %d %s", history.Code, history.Body.String())
	}
	if strings.Contains(history.Body.String(), rawToken) {
		t.Fatal("history response exposed raw durable token")
	}
	var response struct {
		Messages []bee.Message         `json:"messages"`
		Timeline []sessionTimelineItem `json:"timeline"`
	}
	if err := jsonUnmarshal(history.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 2 || len(response.Messages[1].ToolCalls) != 1 {
		t.Fatalf("display messages = %#v", response.Messages)
	}
	var displayedArguments map[string]any
	if err := json.Unmarshal([]byte(response.Messages[1].ToolCalls[0].Arguments), &displayedArguments); err != nil {
		t.Fatalf("display arguments are not valid JSON: %v", err)
	}
	redactedToken, _ := displayedArguments["token"].(string)
	if !strings.HasPrefix(redactedToken, "[REDACTED") || strings.Contains(response.Messages[0].Content, rawToken) || strings.Contains(response.Messages[1].Content, rawToken) {
		t.Fatalf("display projection did not redact: %#v", response.Messages)
	}
	if len(response.Timeline) != 2 {
		t.Fatalf("timeline length = %#v", response.Timeline)
	}
	for _, item := range response.Timeline {
		if item.Message != nil && strings.Contains(item.Message.Content, rawToken) {
			t.Fatalf("timeline display projection = %#v", response.Timeline)
		}
	}
}

func TestGatewayLocalCommandEventsDoNotCallProvider(t *testing.T) {
	provider := &commandProvider{}
	service := newTestService(t, provider)
	defer service.Stop(context.Background())
	create := httptest.NewRecorder()
	service.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"commands"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", create.Code, create.Body.String())
	}
	var metadata bee.SessionMetadata
	if err := jsonUnmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	message := httptest.NewRecorder()
	service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"  /help  "}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("message = %d %s", message.Code, message.Body.String())
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	stream := httptest.NewRecorder()
	service.Handler().ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
	body := stream.Body.String()
	if stream.Code != http.StatusOK || !strings.Contains(body, `"type":"local_command"`) || !strings.Contains(body, `"name":"help"`) {
		t.Fatalf("local command events = %d %s", stream.Code, body)
	}
	if provider.called != 0 {
		t.Fatalf("local command called provider %d times", provider.called)
	}
	for _, tc := range []struct {
		prompt string
		name   string
	}{
		{prompt: "/subagents", name: "subagents"},
		{prompt: "/tasks", name: "tasks"},
	} {
		message = httptest.NewRecorder()
		service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"`+tc.prompt+`"}`)))
		if message.Code != http.StatusAccepted {
			t.Fatalf("%s message = %d %s", tc.prompt, message.Code, message.Body.String())
		}
		if err := jsonUnmarshal(message.Body.Bytes(), &started); err != nil {
			t.Fatal(err)
		}
		stream = httptest.NewRecorder()
		service.Handler().ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
		body = stream.Body.String()
		if stream.Code != http.StatusOK || !strings.Contains(body, `"type":"local_command"`) || !strings.Contains(body, `"name":"`+tc.name+`"`) {
			t.Fatalf("%s local command events = %d %s", tc.prompt, stream.Code, body)
		}
	}
	if provider.called != 0 {
		t.Fatalf("new local commands called provider %d times", provider.called)
	}
}

func TestGatewayListenAddressScope(t *testing.T) {
	if _, err := NewForApplication(nil, "0.0.0.0:18790"); err == nil {
		t.Fatal("nil app unexpectedly accepted")
	}
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := bee.NewApplication(bee.DefaultConfig(), testProvider{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewForApplication(app, "0.0.0.0:18790"); err != nil {
		t.Fatalf("wildcard listen address rejected: %v", err)
	}
	if _, err := NewForApplication(app, "203.0.113.200:18790"); err == nil {
		t.Fatal("specific non-loopback listen address accepted")
	}
}

func jsonUnmarshal(data []byte, value any) error {
	return json.Unmarshal(data, value)
}
