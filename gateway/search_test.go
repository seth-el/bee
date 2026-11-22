package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestGatewaySearchAPIIsBoundedCaseInsensitiveAndProjectAware(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	project, err := service.store.CreateProject(bee.Project{Name: "Search API Project"})
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := service.store.Create(bee.SessionMetadata{Title: "Assigned search", ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := assigned.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "Find the durable Honeycomb notes."}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	unassigned, err := service.store.Create(bee.SessionMetadata{Title: "Unassigned search"})
	if err != nil {
		t.Fatal(err)
	}
	if err := unassigned.AppendMessage(bee.Message{Role: bee.RoleTool, Content: "Honeycomb tool output"}); err != nil {
		t.Fatal(err)
	}

	handler := service.Handler()
	search := httptest.NewRecorder()
	handler.ServeHTTP(search, httptest.NewRequest(http.MethodGet, "/api/sessions/search?q=hOnEyCoMb", nil))
	if search.Code != http.StatusOK {
		t.Fatalf("search = %d %s", search.Code, search.Body.String())
	}
	var all []bee.SessionSearchResult
	if err := json.Unmarshal(search.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Matches[0].Snippet == "" {
		t.Fatalf("all search results = %#v", all)
	}

	filtered := httptest.NewRecorder()
	handler.ServeHTTP(filtered, httptest.NewRequest(http.MethodGet, "/api/sessions/search?q=honeycomb&project_id="+string(project.ID), nil))
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), string(assigned.ID)) || strings.Contains(filtered.Body.String(), string(unassigned.ID)) {
		t.Fatalf("project search = %d %s", filtered.Code, filtered.Body.String())
	}

	for _, path := range []string{"/api/sessions/search", "/api/sessions/search?q=" + strings.Repeat("x", bee.MaxSessionSearchQueryBytes+1)} {
		invalid := httptest.NewRecorder()
		handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, path, nil))
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("invalid search %q = %d %s", path, invalid.Code, invalid.Body.String())
		}
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, marker := range []string{"/api/sessions/search", "session-search-form", "session-search-input", "session-search-clear", "searchSessions", "renderSearchResults", "No matching conversations"} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("static page missing search marker %q", marker)
		}
	}
}
