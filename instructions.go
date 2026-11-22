package bee

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InstructionOptions bounds discovery so loading project guidance never turns
// into an unbounded filesystem scan.
type InstructionOptions struct {
	MaxFiles int
	MaxBytes int
	MaxDepth int
}

func defaultInstructionOptions() InstructionOptions {
	return InstructionOptions{MaxFiles: 32, MaxBytes: 256 << 10, MaxDepth: 16}
}

type InstructionFile struct {
	Path       string
	Directory  string
	Precedence int
	Content    string
}

type ProjectInstructions struct {
	ProjectRoot string
	Files       []InstructionFile
	Content     string
}

// DiscoverProjectInstructions loads only AGENTS.md files. Ordering is explicit:
// filesystem ancestors first, then project-root descendants toward target;
// later files therefore have higher precedence for the target path.
func DiscoverProjectInstructions(projectRoot, targetPath string, options ...InstructionOptions) ([]InstructionFile, error) {
	root, target, opts, err := normalizeInstructionPaths(projectRoot, targetPath, options)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for directory := filepath.Dir(root); ; directory = filepath.Dir(directory) {
		dirs = append(dirs, directory)
		if directory == filepath.Dir(directory) {
			break
		}
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	// Include root and each directory from root to target, but never walk below
	// the target path or outside the project root.
	projectDirs := []string{root}
	targetDir := target
	if info, statErr := os.Stat(target); statErr == nil && !info.IsDir() {
		targetDir = filepath.Dir(target)
	}
	if targetDir != root {
		var tail []string
		for directory := targetDir; isWithin(root, directory) && directory != root; directory = filepath.Dir(directory) {
			tail = append(tail, directory)
		}
		for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
			tail[i], tail[j] = tail[j], tail[i]
		}
		projectDirs = append(projectDirs, tail...)
	}
	dirs = append(dirs, projectDirs...)
	seen := make(map[string]bool)
	var result []InstructionFile
	totalBytes := 0
	for _, directory := range dirs {
		if opts.MaxDepth > 0 && instructionDepth(root, directory) > opts.MaxDepth {
			continue
		}
		path := filepath.Join(directory, "AGENTS.md")
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		if opts.MaxFiles > 0 && len(result) >= opts.MaxFiles {
			break
		}
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read project instruction %q: %w", path, readErr)
		}
		if opts.MaxBytes > 0 && totalBytes+len(data) > opts.MaxBytes {
			return nil, fmt.Errorf("project instructions exceed %d bytes", opts.MaxBytes)
		}
		result = append(result, InstructionFile{Path: path, Directory: directory, Precedence: len(result), Content: string(data)})
		totalBytes += len(data)
	}
	return result, nil
}

func LoadProjectInstructions(projectRoot, targetPath string) (ProjectInstructions, error) {
	root, _, _, err := normalizeInstructionPaths(projectRoot, targetPath, nil)
	if err != nil {
		return ProjectInstructions{}, err
	}
	files, err := DiscoverProjectInstructions(projectRoot, targetPath)
	if err != nil {
		return ProjectInstructions{}, err
	}
	var parts []string
	for _, file := range files {
		parts = append(parts, fmt.Sprintf("# Instructions from %s\n%s", file.Path, strings.TrimSpace(file.Content)))
	}
	return ProjectInstructions{ProjectRoot: root, Files: files, Content: strings.Join(parts, "\n\n")}, nil
}

func normalizeInstructionPaths(projectRoot, targetPath string, options []InstructionOptions) (string, string, InstructionOptions, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", "", InstructionOptions{}, errors.New("project root is required")
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", "", InstructionOptions{}, fmt.Errorf("resolve project root: %w", err)
	}
	root = filepath.Clean(root)
	if info, statErr := os.Stat(root); statErr != nil {
		return "", "", InstructionOptions{}, fmt.Errorf("stat project root: %w", statErr)
	} else if !info.IsDir() {
		return "", "", InstructionOptions{}, errors.New("project root must be a directory")
	}
	target := targetPath
	if target == "" {
		target = root
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", InstructionOptions{}, fmt.Errorf("resolve target path: %w", err)
	}
	target = filepath.Clean(target)
	if !isWithin(root, target) {
		return "", "", InstructionOptions{}, errors.New("target path is outside project root")
	}
	if info, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
		target = filepath.Dir(target)
	} else if statErr == nil && !info.IsDir() {
		target = filepath.Dir(target)
	}
	opts := defaultInstructionOptions()
	if len(options) > 0 {
		opts = options[0]
		if opts.MaxFiles == 0 {
			opts.MaxFiles = 32
		}
		if opts.MaxBytes == 0 {
			opts.MaxBytes = 256 << 10
		}
		if opts.MaxDepth == 0 {
			opts.MaxDepth = 16
		}
		if opts.MaxFiles < 0 || opts.MaxBytes < 0 || opts.MaxDepth < 0 {
			return "", "", InstructionOptions{}, errors.New("instruction bounds must not be negative")
		}
	}
	return root, target, opts, nil
}

func instructionDepth(root, directory string) int {
	rel, err := filepath.Rel(root, directory)
	if err != nil || rel == "." {
		return 0
	}
	depth := 0
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part != "." {
			depth++
		}
	}
	return depth
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// InstructionPrecedence returns a stable path order for diagnostics and tests.
func InstructionPrecedence(files []InstructionFile) []string {
	copyFiles := append([]InstructionFile(nil), files...)
	sort.SliceStable(copyFiles, func(i, j int) bool { return copyFiles[i].Precedence < copyFiles[j].Precedence })
	result := make([]string, len(copyFiles))
	for i, file := range copyFiles {
		result[i] = file.Path
	}
	return result
}
