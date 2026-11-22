package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func TestGatewayProjectsAndSessionFiltering(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	handler := service.Handler()

	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"Gateway Project","description":"test","root_path":"`+root+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create project = %d %s", create.Code, create.Body.String())
	}
	var project bee.Project
	if err := json.Unmarshal(create.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.ID != "gateway-project" || project.RootPath != root {
		t.Fatalf("project = %#v", project)
	}

	assigned := httptest.NewRecorder()
	handler.ServeHTTP(assigned, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"assigned","project_id":"`+string(project.ID)+`"}`)))
	if assigned.Code != http.StatusCreated {
		t.Fatalf("create assigned session = %d %s", assigned.Code, assigned.Body.String())
	}
	var assignedMeta bee.SessionMetadata
	if err := json.Unmarshal(assigned.Body.Bytes(), &assignedMeta); err != nil {
		t.Fatal(err)
	}
	if assignedMeta.ProjectID != project.ID || assignedMeta.ProjectRoot != root {
		t.Fatalf("assigned metadata = %#v", assignedMeta)
	}
	unassigned := httptest.NewRecorder()
	handler.ServeHTTP(unassigned, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"title":"unassigned"}`)))
	if unassigned.Code != http.StatusCreated {
		t.Fatalf("create unassigned session = %d %s", unassigned.Code, unassigned.Body.String())
	}
	var unassignedMeta bee.SessionMetadata
	if err := json.Unmarshal(unassigned.Body.Bytes(), &unassignedMeta); err != nil {
		t.Fatal(err)
	}

	filtered := httptest.NewRecorder()
	handler.ServeHTTP(filtered, httptest.NewRequest(http.MethodGet, "/api/sessions?project_id="+string(project.ID), nil))
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), string(assignedMeta.ID)) || strings.Contains(filtered.Body.String(), "unassigned") {
		t.Fatalf("project filter = %d %s", filtered.Code, filtered.Body.String())
	}
	unassignedList := httptest.NewRecorder()
	handler.ServeHTTP(unassignedList, httptest.NewRequest(http.MethodGet, "/api/sessions?unassigned=true", nil))
	if unassignedList.Code != http.StatusOK || !strings.Contains(unassignedList.Body.String(), "unassigned") || strings.Contains(unassignedList.Body.String(), string(assignedMeta.ID)) {
		t.Fatalf("unassigned filter = %d %s", unassignedList.Code, unassignedList.Body.String())
	}
	assign := httptest.NewRecorder()
	handler.ServeHTTP(assign, httptest.NewRequest(http.MethodPut, "/api/sessions/"+string(unassignedMeta.ID), strings.NewReader(`{"project_id":"`+string(project.ID)+`"}`)))
	if assign.Code != http.StatusOK || !strings.Contains(assign.Body.String(), `"project_id":"`+string(project.ID)+`"`) {
		t.Fatalf("assign session = %d %s", assign.Code, assign.Body.String())
	}

	move := httptest.NewRecorder()
	handler.ServeHTTP(move, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+string(assignedMeta.ID), strings.NewReader(`{"project_id":null}`)))
	if move.Code != http.StatusOK || !strings.Contains(move.Body.String(), `"id":"`+string(assignedMeta.ID)+`"`) {
		t.Fatalf("unassign session = %d %s", move.Code, move.Body.String())
	}
	unassign := httptest.NewRecorder()
	handler.ServeHTTP(unassign, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+string(unassignedMeta.ID), strings.NewReader(`{"project_id":null}`)))
	if unassign.Code != http.StatusOK {
		t.Fatalf("unassign moved session = %d %s", unassign.Code, unassign.Body.String())
	}
	delete := httptest.NewRecorder()
	handler.ServeHTTP(delete, httptest.NewRequest(http.MethodDelete, "/api/projects/"+string(project.ID), nil))
	if delete.Code != http.StatusOK || !strings.Contains(delete.Body.String(), `"deleted":true`) {
		t.Fatalf("delete project = %d %s", delete.Code, delete.Body.String())
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("project deletion removed root: %v", err)
	}
}

func TestGatewayProjectErrorsAndStaticMarkers(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	handler := service.Handler()
	duplicate := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"Same"}`)))
		return recorder
	}
	if got := duplicate(); got.Code != http.StatusCreated {
		t.Fatalf("first project = %d %s", got.Code, got.Body.String())
	}
	if got := duplicate(); got.Code != http.StatusConflict {
		t.Fatalf("duplicate project = %d %s", got.Code, got.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/projects/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing project = %d %s", missing.Code, missing.Body.String())
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, marker := range []string{"/api/projects", "project-filter", "project-list", "new-project", "createProject", "activeProjectID", "unassigned=true", "project_id", "root_path", "create_directory", "Create a matching workspace directory?", "session-project", "Mark complete", "Delete project"} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("static page missing project marker %q", marker)
		}
	}
	for _, marker := range []string{"project-picker-menu", "role=\"listbox\"", "aria-haspopup=\"listbox\"", "wireProjectPicker", "closeProjectPickers", "project-dialog", "project-dialog-form", "project-dialog-directory", "project-dialog-error", "openCreateProjectDialog", "projectDialogReturnFocus"} {
		if !strings.Contains(page.Body.String(), marker) {
			t.Fatalf("static page missing custom project control marker %q", marker)
		}
	}
	if strings.Contains(page.Body.String(), "<select") || strings.Contains(page.Body.String(), "window.prompt") {
		t.Fatal("project controls and create-project flow must not use native selects or prompt dialogs")
	}
}

func TestGatewayProjectMenuEscapesScrollableList(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	page := httptest.NewRecorder()
	service.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	body := page.Body.String()
	for _, marker := range []string{
		".project-menu { position: fixed",
		"project-menu-portal",
		"positionProjectMenu",
		"getBoundingClientRect",
		"data-project-menu",
		"dataset.placement",
		"document.addEventListener('scroll'",
		"Mark complete",
		"Reopen project",
		"Delete project",
		"closeProjectMenus(true)",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("static page missing project menu accessibility marker %q", marker)
		}
	}
	if strings.Contains(body, ".project-menu { position: absolute") {
		t.Fatal("project action menu must not be positioned inside the scroll container")
	}
}

func TestGatewayProjectDirectoryCreationOption(t *testing.T) {
	t.Setenv("BEE_HOME", filepath.Join(t.TempDir(), "bee-home"))
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	handler := service.Handler()

	metadataOnly := httptest.NewRecorder()
	handler.ServeHTTP(metadataOnly, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"Web Metadata Only"}`)))
	if metadataOnly.Code != http.StatusCreated {
		t.Fatalf("metadata-only create = %d %s", metadataOnly.Code, metadataOnly.Body.String())
	}
	var metadataProject bee.Project
	if err := json.Unmarshal(metadataOnly.Body.Bytes(), &metadataProject); err != nil {
		t.Fatal(err)
	}
	if metadataProject.RootPath != "" {
		t.Fatalf("metadata-only root path = %q", metadataProject.RootPath)
	}

	created := httptest.NewRecorder()
	handler.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"Web Directory Project","create_directory":true}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("directory create = %d %s", created.Code, created.Body.String())
	}
	var project bee.Project
	if err := json.Unmarshal(created.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	projectsRoot, err := bee.DefaultBeeProjectsRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(projectsRoot, "web-directory-project")
	if project.RootPath != want {
		t.Fatalf("directory project root path = %q, want %q", project.RootPath, want)
	}
	if info, err := os.Stat(project.RootPath); err != nil || !info.IsDir() {
		t.Fatalf("directory project root stat err=%v info=%v", err, info)
	}

	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/projects/"+string(project.ID), nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete directory project = %d %s", deleted.Code, deleted.Body.String())
	}
	if _, err := os.Stat(project.RootPath); err != nil {
		t.Fatalf("project deletion removed workspace directory: %v", err)
	}
}

func TestGatewayProjectCompletionAndCompletedCreationRefusal(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	handler := service.Handler()

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"name":"Archive Project"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create project = %d %s", create.Code, create.Body.String())
	}
	var project bee.Project
	if err := json.Unmarshal(create.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	complete := httptest.NewRecorder()
	handler.ServeHTTP(complete, httptest.NewRequest(http.MethodPatch, "/api/projects/"+string(project.ID), strings.NewReader(`{"completed":true}`)))
	if complete.Code != http.StatusOK || !strings.Contains(complete.Body.String(), `"completed":true`) || !strings.Contains(complete.Body.String(), `"completed_at"`) {
		t.Fatalf("complete project = %d %s", complete.Code, complete.Body.String())
	}
	newSession := httptest.NewRecorder()
	handler.ServeHTTP(newSession, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"project_id":"`+string(project.ID)+`"}`)))
	if newSession.Code != http.StatusConflict || !strings.Contains(newSession.Body.String(), "reopen") {
		t.Fatalf("completed project session creation = %d %s", newSession.Code, newSession.Body.String())
	}
	reopen := httptest.NewRecorder()
	handler.ServeHTTP(reopen, httptest.NewRequest(http.MethodPut, "/api/projects/"+string(project.ID), strings.NewReader(`{"completed":false}`)))
	if reopen.Code != http.StatusOK || strings.Contains(reopen.Body.String(), `"completed":true`) || strings.Contains(reopen.Body.String(), `"completed_at"`) {
		t.Fatalf("reopen project = %d %s", reopen.Code, reopen.Body.String())
	}
}
