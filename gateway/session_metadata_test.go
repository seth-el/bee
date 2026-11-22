package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestGatewaySessionRenamePinAndPinnedFirstSort(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	older, err := service.store.Create(bee.SessionMetadata{Title: "older"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	newer, err := service.store.Create(bee.SessionMetadata{Title: "newer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := older.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "shared search term"}); err != nil {
		t.Fatal(err)
	}
	if err := newer.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "shared search term"}); err != nil {
		t.Fatal(err)
	}

	patch := httptest.NewRecorder()
	service.Handler().ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+string(older.ID), strings.NewReader(`{"title":"Pinned notes","pinned":true}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", patch.Code, patch.Body.String())
	}
	var updated bee.SessionMetadata
	if err := json.Unmarshal(patch.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Pinned notes" || !updated.Pinned {
		t.Fatalf("updated metadata = %#v", updated)
	}

	list := httptest.NewRecorder()
	service.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	var summaries []bee.SessionSummary
	if err := json.Unmarshal(list.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 || summaries[0].ID != older.ID || !summaries[0].Pinned || summaries[1].ID != newer.ID {
		t.Fatalf("sorted summaries = %#v", summaries)
	}
	search := httptest.NewRecorder()
	service.Handler().ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/api/sessions/search?q=shared+search+term", nil))
	if search.Code != http.StatusOK {
		t.Fatalf("search = %d %s", search.Code, search.Body.String())
	}
	var results []bee.SessionSearchResult
	if err := json.Unmarshal(search.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != older.ID || !results[0].Pinned {
		t.Fatalf("sorted search results = %#v", results)
	}

	unpin := httptest.NewRecorder()
	service.Handler().ServeHTTP(unpin, httptest.NewRequest(http.MethodPut, "/api/sessions/"+string(older.ID), strings.NewReader(`{"pinned":false}`)))
	if unpin.Code != http.StatusOK || strings.Contains(unpin.Body.String(), `"pinned":true`) {
		t.Fatalf("unpin = %d %s", unpin.Code, unpin.Body.String())
	}
	loaded, err := service.store.Load(older.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.Title != "Pinned notes" || loaded.Metadata.Pinned {
		t.Fatalf("durable metadata = %#v", loaded.Metadata)
	}
}

func TestGatewaySessionPatchValidationAndCompatibility(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	session, err := service.store.Create(bee.SessionMetadata{Title: "keep"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"title":""}`, `{"pinned":"yes"}`} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+string(session.ID), strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid patch %s = %d %s", body, response.Code, response.Body.String())
		}
	}
	legacy := httptest.NewRecorder()
	service.Handler().ServeHTTP(legacy, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+string(session.ID), strings.NewReader(`{"project_id":null}`)))
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy project patch = %d %s", legacy.Code, legacy.Body.String())
	}
}

func TestGatewayStaticPageHasSafeMarkdownAndSessionActions(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{"renderMarkdown", "markdown-code-copy", "Rename conversation", "Pin conversation", "Unpin conversation", "target = '_blank'"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing marker %q", marker)
		}
	}
	if strings.Contains(page, "innerHTML") {
		t.Fatal("static page must not use innerHTML for model rendering")
	}
}

func TestGatewayStaticPageHasBoundedMarkdownTables(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		"MAX_MARKDOWN_TABLE_COLUMNS",
		"MAX_MARKDOWN_TABLE_ROWS",
		"MAX_MARKDOWN_TABLE_CELL_CHARS",
		"MAX_MARKDOWN_TABLE_CHARS",
		"scanMarkdownTableRow",
		"splitMarkdownTableRow",
		"parseMarkdownTableDelimiter",
		"parseMarkdownTable",
		"renderMarkdownTable",
		"wrapper.setAttribute('role', 'region')",
		"wrapper.setAttribute('tabindex', '0')",
		"wrapper.setAttribute('aria-label', 'Scrollable Markdown table')",
		"cell.scope = 'col'",
		"unescapeMarkdownTablePipes",
		"markdown-table-frame",
		"markdown-table-scroll-hint",
		"↔ scroll",
		"markdown-table-wrapper",
		"overflow-x: auto",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing bounded markdown table marker %q", marker)
		}
	}
	for _, forbidden := range []string{"innerHTML", "rowspan", "colspan", "cdnjs.cloudflare.com", "unpkg.com", "jsdelivr.net"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("static page contains forbidden markdown table/runtime marker %q", forbidden)
		}
	}
}

func TestGatewayMarkdownTableNodeBehavior(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is unavailable")
	}
	command := exec.Command("node", "markdown_table_test.js")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("markdown table Node regression: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "markdown table parser/render tests: PASS") {
		t.Fatalf("unexpected markdown table Node output: %s", output)
	}
}

func TestGatewayStaticPageHasSafeLocalCodeHighlighting(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		"MAX_HIGHLIGHT_CHARS",
		"CODE_LANGUAGE_ALIASES",
		"elisp: 'emacs-lisp'",
		"'emacs lisp': 'emacs-lisp'",
		"lisp: 'common-lisp'",
		"commonlisp: 'common-lisp'",
		"'emacs-lisp': new Set",
		"'common-lisp': new Set",
		"CODE_LANGUAGE_LITERALS_BY_LANGUAGE",
		"CODE_LANGUAGE_TYPES_BY_LANGUAGE",
		"normalizeCodeLanguage",
		"appendHighlightedCode",
		"LISP_LANGUAGES",
		"scanLispBlockComment",
		"source.startsWith('#|', index)",
		"current === ';'",
		"syntax-keyword",
		"syntax-string",
		"syntax-comment",
		"syntax-number",
		"syntax-property",
		"label.dataset.language = language",
		"code.dataset.language = language",
		"code.className = 'language-' + language",
		"parent.textContent = source",
		"codeLines.join('\\n')",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing local syntax-highlighting marker %q", marker)
		}
	}
	for _, forbidden := range []string{"cdnjs.cloudflare.com", "unpkg.com", "jsdelivr.net", "<script src="} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("static page unexpectedly depends on runtime CDN marker %q", forbidden)
		}
	}
}

func TestGatewayStaticPageKeepsMobileHeaderSizedAndOrderedListsGrouped(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{
		".chat-header { flex: 0 0 auto; min-height: 70px;",
		"const itemPattern = ordered ? /^\\s*\\d+[.)]\\s+(.+)$/ : /^\\s*[-*+]\\s+(.+)$/;",
		"const continuationPattern = /^\\s{2,}\\S/;",
		"if (ordered) {",
		"while (lookahead < lines.length && !lines[lookahead].trim()) lookahead += 1;",
		"if (lookahead < lines.length && itemPattern.test(lines[lookahead])) {",
		"if (lookahead < lines.length && continuationPattern.test(lines[lookahead])) {",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing mobile header/list regression marker %q", marker)
		}
	}
}

func TestGatewayTimelineHistoryWindowIsStableAndLegacyCompatible(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	session, err := service.store.Create(bee.SessionMetadata{Title: "windowed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "second", ToolCalls: []bee.ToolCall{{ID: "call-1", Name: "lookup", Arguments: `{"q":"bee"}`}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(bee.ToolCall{ID: "call-1", Name: "lookup", Arguments: `{"q":"bee"}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(bee.ToolResultEvent{CallID: "call-1", ToolName: "lookup", Output: "found"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleTool, Name: "lookup", ToolCallID: "call-1", Content: "found"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "last"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "fourth"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleAssistant, Content: "fifth"}); err != nil {
		t.Fatal(err)
	}

	legacy := httptest.NewRecorder()
	service.Handler().ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history", nil))
	if legacy.Code != http.StatusOK || !strings.Contains(legacy.Body.String(), `"messages"`) || !strings.Contains(legacy.Body.String(), `"timeline"`) || strings.Contains(legacy.Body.String(), `"tool_call"`) || strings.Contains(legacy.Body.String(), `"tool_result"`) {
		t.Fatalf("legacy history = %d %s", legacy.Code, legacy.Body.String())
	}

	first := httptest.NewRecorder()
	service.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=3", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first timeline = %d %s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), `"messages"`) || strings.Contains(first.Body.String(), `"artifacts"`) {
		t.Fatalf("optimized timeline duplicated legacy arrays: %s", first.Body.String())
	}
	var page sessionTimelineResponse
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Timeline) != 3 || !page.HasMore || page.NextBefore == "" || page.EventCount != 10 {
		t.Fatalf("first page = %#v", page)
	}
	for _, item := range page.Timeline {
		if item.Seq == 0 || item.ID == "" || item.At.IsZero() {
			t.Fatalf("window timeline item lost event metadata: %#v", item)
		}
	}
	if page.Timeline[0].Message == nil || page.Timeline[0].Message.Content != "last" || page.Timeline[1].Message == nil || page.Timeline[1].Message.Content != "fourth" || page.Timeline[2].Message == nil || page.Timeline[2].Message.Content != "fifth" {
		t.Fatalf("first page ordering = %#v", page.Timeline)
	}

	second := httptest.NewRecorder()
	service.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=3&before="+page.NextBefore, nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second timeline = %d %s", second.Code, second.Body.String())
	}
	var older sessionTimelineResponse
	if err := json.Unmarshal(second.Body.Bytes(), &older); err != nil {
		t.Fatal(err)
	}
	if len(older.Timeline) != 2 || older.Timeline[0].Seq >= page.Timeline[0].Seq || older.Timeline[1].Seq >= page.Timeline[0].Seq {
		t.Fatalf("older page boundary = %#v after %#v", older.Timeline, page.Timeline)
	}
	if older.Timeline[0].Message == nil || older.Timeline[0].Message.Content != "first" || older.Timeline[1].Message == nil || older.Timeline[1].Message.Content != "second" {
		t.Fatalf("older page ordering = %#v", older.Timeline)
	}
	fullRecorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(fullRecorder, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history", nil))
	var full sessionHistoryResponse
	if err := json.Unmarshal(fullRecorder.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	for _, item := range full.Timeline {
		if item.Seq == 0 || item.ID == "" || item.At.IsZero() {
			t.Fatalf("full timeline item lost event metadata: %#v", item)
		}
	}
}

func TestGatewayTimelineHistoryWindowEmptyAndValidation(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	session, err := service.store.Create(bee.SessionMetadata{Title: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?view=timeline&limit=0", "?view=timeline&limit=129", "?view=timeline&before=0", "?view=timeline&before=nope"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %s = %d %s", query, response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=2", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timeline":[]`) || strings.Contains(response.Body.String(), `"has_more":true`) {
		t.Fatalf("empty timeline = %d %s", response.Code, response.Body.String())
	}
}

func TestGatewayTimelineHistoryCursorBeyondNewestReturnsNewestEligibleWindow(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	session, err := service.store.Create(bee.SessionMetadata{Title: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"one", "two", "three"} {
		if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=2&before=999999", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("timeline = %d %s", response.Code, response.Body.String())
	}
	var page sessionTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Timeline) != 2 || page.Timeline[0].Message == nil || page.Timeline[0].Message.Content != "two" || page.Timeline[1].Message == nil || page.Timeline[1].Message.Content != "three" {
		t.Fatalf("cursor beyond newest page = %#v", page.Timeline)
	}
}

func TestGatewayTimelineHistoryWindowIncludesAttachmentMetadata(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	artifacts, err := bee.NewArtifactStore(t.TempDir(), service.App().Store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := bee.NewAttachmentStore(t.TempDir(), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	service.App().ArtifactStore = artifacts
	service.App().AttachmentStore = attachments
	session, err := service.store.Create(bee.SessionMetadata{Title: "attachments"})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := attachments.Create(context.Background(), session.ID, "note.txt", "text/plain", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	older, err := attachments.Create(context.Background(), session.ID, "older.txt", "text/plain", []byte("older"))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "see file", Attachments: []bee.AttachmentRef{{ID: attachment.ID, ArtifactID: attachment.ArtifactID, Filename: attachment.Filename, MediaType: attachment.MediaType, Kind: attachment.Kind}}}); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/history?view=timeline&limit=8", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("timeline = %d %s", response.Code, response.Body.String())
	}
	var page sessionTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Attachments) != 2 || page.Attachments[0].ID != attachment.ID || page.Attachments[1].ID != older.ID || len(page.Timeline) == 0 {
		t.Fatalf("initial timeline must preserve referenced and orphan attachment metadata = %#v", page)
	}
	if page.Timeline[len(page.Timeline)-1].Message == nil || len(page.Timeline[len(page.Timeline)-1].Message.Attachments) != 1 {
		t.Fatalf("attachment reference missing from message = %#v", page.Timeline)
	}

}

func TestGatewayStaticPageHasWindowedHistoryContract(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{"view=timeline", "Load earlier messages", "history-window-controls", "DocumentFragment", "IntersectionObserver", "loading = 'lazy'", "next_before", "has_more", "requestAnimationFrame", "flushAssistantDelta", "assistantPending", "live-streaming", "beforeunload", "message-time", "formatMessageTimestamp", "timeZone", "item.at || ''", "new Date().toISOString()"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing history performance marker %q", marker)
		}
	}
}
