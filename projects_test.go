package bee

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectStorePersistsAssociationsAndNeverDeletesRoot(t *testing.T) {
	storeRoot := t.TempDir()
	filesystemRoot := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filesystemRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(Project{Name: "My Workspace", Description: "durable metadata", RootPath: filesystemRoot})
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != "my-workspace" {
		t.Fatalf("project id = %q", project.ID)
	}
	if project.RootPath != filepath.Clean(filesystemRoot) {
		t.Fatalf("root path = %q", project.RootPath)
	}
	second, err := NewSessionStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := second.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("persisted projects = %#v", projects)
	}
	created, err := second.Create(SessionMetadata{Title: "assigned", ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := second.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ProjectID != project.ID {
		t.Fatalf("session summaries = %#v", list)
	}
	if err := second.DeleteProject(project.ID); !errors.Is(err, ErrProjectInUse) {
		t.Fatalf("delete assigned project error = %v", err)
	}
	loaded, err := second.Load(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.UpdateProjectID(""); err != nil {
		t.Fatal(err)
	}
	if err := second.DeleteProject(project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filesystemRoot); err != nil {
		t.Fatalf("project deletion removed filesystem root: %v", err)
	}
	projects, err = second.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects after deletion = %#v", projects)
	}
}

func TestProjectDirectoryCreationIsOptInAndUsesCanonicalRoot(t *testing.T) {
	beeHome := filepath.Join(t.TempDir(), "bee-home")
	t.Setenv("BEE_HOME", beeHome)
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}

	metadataOnly, err := store.CreateProject(Project{Name: "Metadata Only"})
	if err != nil {
		t.Fatal(err)
	}
	if metadataOnly.RootPath != "" {
		t.Fatalf("metadata-only root path = %q", metadataOnly.RootPath)
	}

	created, err := store.CreateProjectWithOptions(Project{Name: "Workspace Project"}, ProjectCreateOptions{CreateDirectory: true})
	if err != nil {
		t.Fatal(err)
	}
	projectsRoot, err := DefaultBeeProjectsRoot()
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(projectsRoot, "workspace-project")
	if created.RootPath != expected {
		t.Fatalf("created root path = %q, want %q", created.RootPath, expected)
	}
	info, err := os.Stat(created.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created directory mode=%o dir=%t", info.Mode().Perm(), info.IsDir())
	}
}

func TestProjectDirectoryCreationHandlesSlugCollisions(t *testing.T) {
	beeHome := filepath.Join(t.TempDir(), "bee-home")
	t.Setenv("BEE_HOME", beeHome)
	projectsRoot, err := EnsureBeeProjectsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(projectsRoot, "collision-project"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateProjectWithOptions(Project{Name: "Collision Project"}, ProjectCreateOptions{CreateDirectory: true})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(projectsRoot, "collision-project-2")
	if created.RootPath != want {
		t.Fatalf("collision root path = %q, want %q", created.RootPath, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("collision directory stat err=%v info=%v", err, info)
	}
}

func TestProjectDirectoryCreationRejectsExplicitRootPath(t *testing.T) {
	t.Setenv("BEE_HOME", filepath.Join(t.TempDir(), "bee-home"))
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateProjectWithOptions(Project{Name: "Unsafe Combination", RootPath: filepath.Join(t.TempDir(), "root")}, ProjectCreateOptions{CreateDirectory: true})
	if err == nil || !strings.Contains(err.Error(), "root path cannot be supplied") {
		t.Fatalf("explicit root path error = %v", err)
	}
}

func TestProjectIDsArePathSafeAndNamesConflict(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateProject(Project{Name: "Repeated Name"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateProject(Project{Name: "Repeated Name 2"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.ID == "" || second.ID == "" {
		t.Fatalf("generated ids = %q, %q", first.ID, second.ID)
	}
	if err := validateProjectID(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(Project{Name: "repeated name"}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("duplicate name error = %v", err)
	}
	if _, err := store.CreateProject(Project{ID: ProjectID("../unsafe"), Name: "Unsafe"}); err == nil {
		t.Fatal("unsafe project id accepted")
	}
}

func TestProjectCompletionPersistsAndReopens(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(Project{Name: "Archive Me"})
	if err != nil {
		t.Fatal(err)
	}
	completed := true
	project, err = store.UpdateProject(project.ID, ProjectPatch{Completed: &completed})
	if err != nil {
		t.Fatal(err)
	}
	if !project.Completed || project.CompletedAt == nil {
		t.Fatalf("completed project = %#v", project)
	}
	reloaded, err := store.GetProject(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Completed || reloaded.CompletedAt == nil || !reloaded.CompletedAt.Equal(project.CompletedAt.UTC()) {
		t.Fatalf("persisted completion = %#v", reloaded)
	}
	reopened := false
	reloaded, err = store.UpdateProject(project.ID, ProjectPatch{Completed: &reopened})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Completed || reloaded.CompletedAt != nil {
		t.Fatalf("reopened project = %#v", reloaded)
	}
}
