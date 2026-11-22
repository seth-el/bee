package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

var (
	ErrProjectNotFound  = errors.New("project not found")
	ErrProjectConflict  = errors.New("project already exists")
	ErrProjectInUse     = errors.New("project has assigned sessions")
	ErrProjectCompleted = errors.New("project is completed; reopen it before creating new work")
)

// Project is durable Bee metadata. RootPath is informational/runtime context;
// project deletion never removes or mutates that filesystem directory.
type Project struct {
	ID          ProjectID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	RootPath    string     `json:"root_path,omitempty"`
	Completed   bool       `json:"completed,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ProjectPatch updates only supplied fields. A nil pointer preserves the
// existing value; non-nil empty strings clear optional fields.
type ProjectPatch struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	RootPath    *string `json:"root_path,omitempty"`
	Completed   *bool   `json:"completed,omitempty"`
}

// ProjectCreateOptions controls optional filesystem side effects during
// project creation. The default is metadata-only creation.
type ProjectCreateOptions struct {
	CreateDirectory bool
}

const projectStoreFile = "projects.json"

func (store *SessionStore) projectsPath() string {
	return filepath.Join(store.root, projectStoreFile)
}

func validateProjectID(id ProjectID) error {
	value := string(id)
	if value == "" || value == "." || value == ".." || len(value) > 128 || filepath.Base(value) != value {
		return errors.New("project id must be a single path-safe component")
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.') {
			return errors.New("project id must be a single path-safe component")
		}
	}
	return nil
}

func normalizeProject(project Project) (Project, error) {
	project.Name = strings.TrimSpace(project.Name)
	project.Description = strings.TrimSpace(project.Description)
	if project.Name == "" {
		return Project{}, errors.New("project name is required")
	}
	if len(project.Name) > 256 || len(project.Description) > 4096 || len(project.RootPath) > 4096 {
		return Project{}, errors.New("project fields are too large")
	}
	if project.ID != "" {
		if err := validateProjectID(project.ID); err != nil {
			return Project{}, err
		}
	}
	if !project.Completed {
		project.CompletedAt = nil
	}
	if strings.TrimSpace(project.RootPath) != "" {
		root, err := filepath.Abs(strings.TrimSpace(project.RootPath))
		if err != nil {
			return Project{}, fmt.Errorf("resolve project root: %w", err)
		}
		project.RootPath = filepath.Clean(root)
	} else {
		project.RootPath = ""
	}
	return project, nil
}

func projectSlug(name string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if builder.Len() > 0 && !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	value := strings.Trim(builder.String(), "-")
	if value == "" {
		value = "project"
	}
	if len(value) > 96 {
		value = strings.Trim(value[:96], "-")
	}
	return value
}

func readProjects(path string) ([]Project, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Project{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	if len(data) == 0 {
		return []Project{}, nil
	}
	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return projects, nil
}

func writeProjects(path string, projects []Project) error {
	data, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return fmt.Errorf("encode projects: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write projects: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace projects: %w", err)
	}
	return nil
}

func withProjects[T any](store *SessionStore, fn func([]Project) (T, []Project, bool, error)) (T, error) {
	var zero T
	if store == nil {
		return zero, errors.New("session store is nil")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	lock, err := acquireSessionFileLock(store.projectsPath() + ".lock")
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	projects, err := readProjects(store.projectsPath())
	if err != nil {
		return zero, err
	}
	result, updated, changed, err := fn(projects)
	if err != nil {
		return zero, err
	}
	if changed {
		if err := writeProjects(store.projectsPath(), updated); err != nil {
			return zero, err
		}
	}
	return result, nil
}

func (store *SessionStore) ListProjects() ([]Project, error) {
	return withProjects(store, func(projects []Project) ([]Project, []Project, bool, error) {
		result := append([]Project(nil), projects...)
		sort.Slice(result, func(i, j int) bool {
			if result[i].Name == result[j].Name {
				return result[i].ID < result[j].ID
			}
			return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
		})
		return result, projects, false, nil
	})
}

func (store *SessionStore) GetProject(id ProjectID) (Project, error) {
	if err := validateProjectID(id); err != nil {
		return Project{}, err
	}
	return withProjects(store, func(projects []Project) (Project, []Project, bool, error) {
		for _, project := range projects {
			if project.ID == id {
				return project, projects, false, nil
			}
		}
		return Project{}, projects, false, ErrProjectNotFound
	})
}

func (store *SessionStore) CreateProject(project Project) (Project, error) {
	return store.CreateProjectWithOptions(project, ProjectCreateOptions{})
}

// CreateProjectWithOptions creates durable project metadata and, when opted
// into, a private workspace directory under Bee's canonical projects root.
// Existing callers retain metadata-only behavior through CreateProject.
func (store *SessionStore) CreateProjectWithOptions(project Project, options ProjectCreateOptions) (Project, error) {
	project, err := normalizeProject(project)
	if err != nil {
		return Project{}, err
	}
	if options.CreateDirectory && project.RootPath != "" {
		return Project{}, errors.New("root path cannot be supplied when create directory is enabled")
	}
	return withProjects(store, func(projects []Project) (Project, []Project, bool, error) {
		for _, existing := range projects {
			if strings.EqualFold(existing.Name, project.Name) {
				return Project{}, projects, false, ErrProjectConflict
			}
		}
		if project.ID == "" {
			base := projectSlug(project.Name)
			project.ID = ProjectID(base)
			for suffix := 2; ; suffix++ {
				found := false
				for _, existing := range projects {
					if existing.ID == project.ID {
						found = true
						break
					}
				}
				if !found {
					break
				}
				project.ID = ProjectID(fmt.Sprintf("%s-%d", base, suffix))
			}
		}
		for _, existing := range projects {
			if existing.ID == project.ID {
				return Project{}, projects, false, ErrProjectConflict
			}
		}
		if options.CreateDirectory {
			root, err := createProjectWorkspaceDirectory(project.Name)
			if err != nil {
				return Project{}, projects, false, err
			}
			project.RootPath = root
		}
		now := time.Now().UTC()
		if project.Completed && project.CompletedAt == nil {
			project.CompletedAt = &now
		}
		project.CreatedAt, project.UpdatedAt = now, now
		updated := append(projects, project)
		return project, updated, true, nil
	})
}

func createProjectWorkspaceDirectory(name string) (string, error) {
	projectsRoot, err := EnsureBeeProjectsRoot()
	if err != nil {
		return "", fmt.Errorf("ensure Bee project workspace root: %w", err)
	}
	base := projectSlug(name)
	for suffix := 1; suffix <= 10000; suffix++ {
		candidateName := base
		if suffix > 1 {
			candidateName = fmt.Sprintf("%s-%d", base, suffix)
		}
		candidate := filepath.Join(projectsRoot, candidateName)
		if err := os.Mkdir(candidate, 0o700); err == nil {
			return filepath.Clean(candidate), nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create project workspace directory: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique project workspace directory")
}

func (store *SessionStore) UpdateProject(id ProjectID, patch ProjectPatch) (Project, error) {
	if err := validateProjectID(id); err != nil {
		return Project{}, err
	}
	return withProjects(store, func(projects []Project) (Project, []Project, bool, error) {
		index := -1
		for i := range projects {
			if projects[i].ID == id {
				index = i
				break
			}
		}
		if index < 0 {
			return Project{}, projects, false, ErrProjectNotFound
		}
		current := projects[index]
		if patch.Name != nil {
			current.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.Description != nil {
			current.Description = strings.TrimSpace(*patch.Description)
		}
		if patch.RootPath != nil {
			current.RootPath = strings.TrimSpace(*patch.RootPath)
		}
		if patch.Completed != nil {
			current.Completed = *patch.Completed
			if current.Completed {
				now := time.Now().UTC()
				current.CompletedAt = &now
			} else {
				current.CompletedAt = nil
			}
		}
		normalized, err := normalizeProject(current)
		if err != nil {
			return Project{}, projects, false, err
		}
		for i, existing := range projects {
			if i != index && (existing.ID == id || strings.EqualFold(existing.Name, normalized.Name)) {
				return Project{}, projects, false, ErrProjectConflict
			}
		}
		normalized.CreatedAt = current.CreatedAt
		normalized.UpdatedAt = time.Now().UTC()
		projects[index] = normalized
		return normalized, projects, true, nil
	})
}

func (store *SessionStore) DeleteProject(id ProjectID) error {
	if err := validateProjectID(id); err != nil {
		return err
	}
	_, err := withProjects(store, func(projects []Project) (struct{}, []Project, bool, error) {
		index := -1
		for i := range projects {
			if projects[i].ID == id {
				index = i
				break
			}
		}
		if index < 0 {
			return struct{}{}, projects, false, ErrProjectNotFound
		}
		assigned, err := store.projectHasSessionsUnlocked(id)
		if err != nil {
			return struct{}{}, projects, false, err
		}
		if assigned {
			return struct{}{}, projects, false, ErrProjectInUse
		}
		updated := append(projects[:index:index], projects[index+1:]...)
		return struct{}{}, updated, true, nil
	})
	return err
}

func (store *SessionStore) projectHasSessionsUnlocked(id ProjectID) (bool, error) {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return false, fmt.Errorf("list sessions for project: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionID := SessionID(strings.TrimSuffix(entry.Name(), ".jsonl"))
		events, err := replayFile(filepath.Join(store.root, entry.Name()), sessionID)
		if err != nil {
			return false, err
		}
		var currentProject ProjectID
		for _, event := range events {
			if event.Type != EventMetadata {
				continue
			}
			meta, err := decodeEventPayload[MetadataEvent](event, EventMetadata)
			if err != nil {
				return false, err
			}
			currentProject = meta.Metadata.ProjectID
		}
		if currentProject == id {
			return true, nil
		}
	}
	return false, nil
}
