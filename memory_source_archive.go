package bee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MemorySourceArchiveFormatVersion  = 1
	DefaultSourceArchiveMaxFiles      = DefaultInventoryMaxFiles
	DefaultSourceArchiveMaxFileBytes  = DefaultInventoryMaxFileBytes
	DefaultSourceArchiveMaxTotalBytes = DefaultInventoryMaxTotalBytes
)

var defaultSourceArchiveExcludes = []string{
	".dreams",
	"dreaming",
}

// MemorySourceArchiveOptions controls one explicit, bounded source-corpus
// archive. Roots are read-only inputs; the archive is Bee-owned output.
type MemorySourceArchiveOptions struct {
	Roots         []MemoryInventoryRoot
	Excludes      []string
	Name          string
	Destination   string
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
	DryRun        bool
	Overwrite     bool
}

type MemorySourceArchiveRoot struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type MemorySourceArchiveFile struct {
	RootLabel    string    `json:"root_label"`
	RelativePath string    `json:"relative_path"`
	ArchivePath  string    `json:"archive_path"`
	SourceType   string    `json:"source_type"`
	SizeBytes    int64     `json:"size_bytes"`
	LineCount    int       `json:"line_count,omitempty"`
	ModifiedAt   time.Time `json:"modified_at"`
	SHA256       string    `json:"sha256"`
}

type MemorySourceArchiveExcludedFile struct {
	RootLabel    string    `json:"root_label"`
	RelativePath string    `json:"relative_path"`
	SizeBytes    int64     `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at,omitempty"`
	Reason       string    `json:"reason"`
}

type MemorySourceArchiveManifest struct {
	FormatVersion int                               `json:"format_version"`
	ArchiveID     string                            `json:"archive_id"`
	Name          string                            `json:"name"`
	CreatedAt     time.Time                         `json:"created_at"`
	Destination   string                            `json:"destination"`
	Roots         []MemorySourceArchiveRoot         `json:"roots"`
	Excludes      []string                          `json:"excludes"`
	Files         []MemorySourceArchiveFile         `json:"files"`
	ExcludedFiles []MemorySourceArchiveExcludedFile `json:"excluded_files,omitempty"`
	Included      int                               `json:"included"`
	Excluded      int                               `json:"excluded"`
	TotalBytes    int64                             `json:"total_bytes"`
	SkippedBytes  int64                             `json:"skipped_bytes"`
}

// MemorySourceArchiveReport is the bounded result of a source archive
// operation. Dry runs return the same manifest shape without writing output.
type MemorySourceArchiveReport struct {
	Manifest     MemorySourceArchiveManifest `json:"manifest"`
	Destination  string                      `json:"destination"`
	ManifestPath string                      `json:"manifest_path"`
	DryRun       bool                        `json:"dry_run"`
	Applied      bool                        `json:"applied"`
}

type sourceArchiveFile struct {
	Archive MemorySourceArchiveFile
	Data    []byte
}

// DefaultBeeSourceMemoryRoot returns Bee's private, non-structured source
// archive area. It is intentionally separate from the active memory store.
func DefaultBeeSourceMemoryRoot() (string, error) {
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(workspace, "source-memory"), nil
}

// ArchiveMemorySources copies explicit source roots into a Bee-owned private
// archive. It never writes to source roots and refuses existing destinations
// unless Overwrite is explicitly enabled.
func ArchiveMemorySources(options MemorySourceArchiveOptions) (MemorySourceArchiveReport, error) {
	var report MemorySourceArchiveReport
	options, err := normalizeSourceArchiveOptions(options)
	if err != nil {
		return report, err
	}
	roots, err := validateSourceArchiveRoots(options.Roots)
	if err != nil {
		return report, err
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		if len(roots) != 1 {
			return report, errors.New("source archive name is required when archiving multiple roots")
		}
		name = roots[0].Label
	}
	if err := validateSafeID("source archive name", name, 128); err != nil {
		return report, err
	}
	destination, err := sourceArchiveDestination(options.Destination, name)
	if err != nil {
		return report, err
	}
	if err := validateSourceArchiveDestination(destination, roots, options.Overwrite); err != nil {
		return report, err
	}

	files, excludedFiles, err := collectSourceArchiveFiles(roots, destination, options)
	if err != nil {
		return report, err
	}
	manifest := MemorySourceArchiveManifest{
		FormatVersion: MemorySourceArchiveFormatVersion,
		ArchiveID:     fmt.Sprintf("memory-source-archive-%d", time.Now().UTC().UnixNano()),
		Name:          name,
		CreatedAt:     time.Now().UTC(),
		Destination:   destination,
		Roots:         make([]MemorySourceArchiveRoot, 0, len(roots)),
		Excludes:      options.Excludes,
		Files:         make([]MemorySourceArchiveFile, 0, len(files)),
		ExcludedFiles: excludedFiles,
		Included:      len(files),
		Excluded:      len(excludedFiles),
	}
	for _, root := range roots {
		manifest.Roots = append(manifest.Roots, MemorySourceArchiveRoot{Label: root.Label, Path: root.Path})
	}
	for _, file := range files {
		manifest.Files = append(manifest.Files, file.Archive)
		manifest.TotalBytes += file.Archive.SizeBytes
	}
	for _, file := range excludedFiles {
		manifest.SkippedBytes += file.SizeBytes
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		if manifest.Files[i].RootLabel != manifest.Files[j].RootLabel {
			return manifest.Files[i].RootLabel < manifest.Files[j].RootLabel
		}
		return manifest.Files[i].RelativePath < manifest.Files[j].RelativePath
	})
	report = MemorySourceArchiveReport{Manifest: manifest, Destination: destination, ManifestPath: filepath.Join(destination, "manifest.json"), DryRun: options.DryRun}
	if options.DryRun {
		return report, nil
	}
	archiveRoot, err := DefaultBeeSourceMemoryRoot()
	if err != nil {
		return report, err
	}
	if err := ensurePrivateDir(archiveRoot, 0o700); err != nil {
		return report, fmt.Errorf("protect source archive root: %w", err)
	}
	if err := writeSourceArchive(archiveRoot, destination, files, manifest, options.Overwrite); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}

func normalizeSourceArchiveOptions(options MemorySourceArchiveOptions) (MemorySourceArchiveOptions, error) {
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultSourceArchiveMaxFiles
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = DefaultSourceArchiveMaxFileBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = DefaultSourceArchiveMaxTotalBytes
	}
	if options.MaxFiles < 1 || options.MaxFiles > MaxInventoryFiles {
		return MemorySourceArchiveOptions{}, fmt.Errorf("source archive max files must be between 1 and %d", MaxInventoryFiles)
	}
	if options.MaxFileBytes < 1 || options.MaxFileBytes > MaxInventoryFileBytes {
		return MemorySourceArchiveOptions{}, fmt.Errorf("source archive max file bytes must be between 1 and %d", MaxInventoryFileBytes)
	}
	if options.MaxTotalBytes < 1 || options.MaxTotalBytes > MaxInventoryTotalBytes {
		return MemorySourceArchiveOptions{}, fmt.Errorf("source archive max total bytes must be between 1 and %d", MaxInventoryTotalBytes)
	}
	options.Excludes = normalizeSourceArchiveExcludes(options.Excludes)
	return options, nil
}

func normalizeSourceArchiveExcludes(input []string) []string {
	all := append(append([]string(nil), defaultSourceArchiveExcludes...), input...)
	seen := make(map[string]struct{}, len(all))
	result := make([]string, 0, len(all))
	for _, value := range all {
		value = filepath.ToSlash(strings.Trim(strings.TrimSpace(value), "/"))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateSourceArchiveRoots(input []MemoryInventoryRoot) ([]MemoryInventoryRoot, error) {
	roots, err := validateInventoryRoots(input)
	if err != nil {
		return nil, err
	}
	for index := range roots {
		path := roots[index].Path
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve source archive root %q: %w", path, err)
		}
		if filepath.Clean(canonical) != filepath.Clean(path) {
			return nil, fmt.Errorf("source archive root %q must not use symlink ancestors", path)
		}
	}
	return roots, nil
}

func sourceArchiveDestination(requested, name string) (string, error) {
	archiveRoot, err := DefaultBeeSourceMemoryRoot()
	if err != nil {
		return "", err
	}
	archiveRoot, err = filepath.Abs(archiveRoot)
	if err != nil {
		return "", err
	}
	if requested == "" {
		return filepath.Join(archiveRoot, name), nil
	}
	if !filepath.IsAbs(requested) {
		return "", errors.New("source archive destination must be absolute")
	}
	destination := filepath.Clean(requested)
	if filepath.Dir(destination) != filepath.Clean(archiveRoot) {
		return "", errors.New("source archive destination must be a direct child of Bee source-memory")
	}
	if filepath.Base(destination) != name {
		return "", errors.New("source archive destination name does not match archive name")
	}
	return destination, nil
}

func validateSourceArchiveDestination(destination string, roots []MemoryInventoryRoot, overwrite bool) error {
	archiveRoot, err := DefaultBeeSourceMemoryRoot()
	if err != nil {
		return err
	}
	archiveRoot, err = filepath.Abs(archiveRoot)
	if err != nil {
		return err
	}
	if filepath.Dir(destination) != filepath.Clean(archiveRoot) || !scopeWithin(archiveRoot, destination) {
		return errors.New("source archive destination escaped Bee source-memory")
	}
	if err := rejectBeeSymlinkAncestors(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("inspect source archive destination: %w", err)
	}
	for _, root := range roots {
		if sourceArchivePathsOverlap(root.Path, destination) {
			return errors.New("source archive destination overlaps a source root")
		}
	}
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect source archive destination: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source archive destination must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("source archive destination must be a directory")
	}
	if !overwrite {
		return errors.New("refusing to overwrite existing source archive destination")
	}
	return nil
}

func sourceArchivePathsOverlap(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return true
	}
	leftAbs, rightAbs = filepath.Clean(leftAbs), filepath.Clean(rightAbs)
	return leftAbs == rightAbs || scopeWithin(leftAbs, rightAbs) || scopeWithin(rightAbs, leftAbs)
}

func collectSourceArchiveFiles(roots []MemoryInventoryRoot, destination string, options MemorySourceArchiveOptions) ([]sourceArchiveFile, []MemorySourceArchiveExcludedFile, error) {
	files := make([]sourceArchiveFile, 0)
	excludedFiles := make([]MemorySourceArchiveExcludedFile, 0)
	var totalBytes int64
	for _, root := range roots {
		entries, err := collectSourceArchiveRootFiles(root, destination)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			if inventoryPathExcluded(entry.relative, options.Excludes) {
				excludedFiles = append(excludedFiles, MemorySourceArchiveExcludedFile{RootLabel: root.Label, RelativePath: filepath.ToSlash(entry.relative), SizeBytes: entry.info.Size(), ModifiedAt: entry.info.ModTime().UTC(), Reason: "excluded path"})
				continue
			}
			if len(files) >= options.MaxFiles {
				return nil, nil, fmt.Errorf("source archive exceeds max files %d", options.MaxFiles)
			}
			data, info, err := readSourceArchiveFile(entry.path, options.MaxFileBytes)
			if err != nil {
				return nil, nil, fmt.Errorf("archive %s:%s: %w", root.Label, entry.relative, err)
			}
			if inventorySensitivePath(entry.relative) {
				return nil, nil, fmt.Errorf("archive %s:%s: sensitive path is not allowed", root.Label, entry.relative)
			}
			if inventoryLooksSecret(data) {
				return nil, nil, fmt.Errorf("archive %s:%s: secret-shaped content is not allowed", root.Label, entry.relative)
			}
			if info.Size() > options.MaxFileBytes {
				return nil, nil, fmt.Errorf("archive %s:%s: file exceeds max_file_bytes", root.Label, entry.relative)
			}
			if info.Size() > options.MaxTotalBytes-totalBytes {
				return nil, nil, fmt.Errorf("archive exceeds max_total_bytes %d", options.MaxTotalBytes)
			}
			hash := sha256.Sum256(data)
			archivePath := filepath.ToSlash(filepath.Join(root.Label, filepath.FromSlash(entry.relative)))
			files = append(files, sourceArchiveFile{Data: data, Archive: MemorySourceArchiveFile{RootLabel: root.Label, RelativePath: filepath.ToSlash(entry.relative), ArchivePath: archivePath, SourceType: classifyInventorySource(root.Label, entry.relative), SizeBytes: int64(len(data)), LineCount: countInventoryLines(data), ModifiedAt: info.ModTime().UTC(), SHA256: hex.EncodeToString(hash[:])}})
			totalBytes += int64(len(data))
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Archive.RootLabel != files[j].Archive.RootLabel {
			return files[i].Archive.RootLabel < files[j].Archive.RootLabel
		}
		return files[i].Archive.RelativePath < files[j].Archive.RelativePath
	})
	sort.Slice(excludedFiles, func(i, j int) bool {
		if excludedFiles[i].RootLabel != excludedFiles[j].RootLabel {
			return excludedFiles[i].RootLabel < excludedFiles[j].RootLabel
		}
		return excludedFiles[i].RelativePath < excludedFiles[j].RelativePath
	})
	return files, excludedFiles, nil
}

type sourceArchiveEntry struct {
	path     string
	relative string
	info     os.FileInfo
}

func collectSourceArchiveRootFiles(root MemoryInventoryRoot, destination string) ([]sourceArchiveEntry, error) {
	info, err := os.Lstat(root.Path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("source root must not be a symlink")
	}
	if info.Mode().IsRegular() {
		return []sourceArchiveEntry{{path: root.Path, relative: filepath.Base(root.Path), info: info}}, nil
	}
	if !info.IsDir() {
		return nil, errors.New("source root must be a regular file or directory")
	}
	entries := make([]sourceArchiveEntry, 0)
	err = filepath.WalkDir(root.Path, func(path string, dirEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if sourceArchivePathsOverlap(path, destination) {
			return filepath.SkipDir
		}
		if dirEntry.Type()&os.ModeSymlink != 0 {
			return errors.New("source archive refuses symlink entries")
		}
		relative, err := filepath.Rel(root.Path, path)
		if err != nil {
			return err
		}
		if relative == "." || dirEntry.IsDir() {
			return nil
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("source archive accepts regular files only")
		}
		entries = append(entries, sourceArchiveEntry{path: path, relative: filepath.ToSlash(relative), info: info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relative < entries[j].relative })
	return entries, nil
}

func readSourceArchiveFile(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, errors.New("source file must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.Mode().IsRegular() {
		return nil, nil, errors.New("source file changed to a non-regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("file exceeds %d bytes", maxBytes)
	}
	return data, openedInfo, nil
}

func writeSourceArchive(archiveRoot, destination string, files []sourceArchiveFile, manifest MemorySourceArchiveManifest, overwrite bool) error {
	stage, err := os.MkdirTemp(archiveRoot, ".source-archive-")
	if err != nil {
		return fmt.Errorf("create source archive staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		path := filepath.Join(stage, filepath.FromSlash(file.Archive.ArchivePath))
		if err := ensurePrivateDir(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create source archive directory: %w", err)
		}
		if err := os.WriteFile(path, file.Data, 0o600); err != nil {
			return fmt.Errorf("write source archive file: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode source archive manifest: %w", err)
	}
	data = append(data, '\n')
	manifestPath := filepath.Join(stage, "manifest.json")
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		return fmt.Errorf("write source archive manifest: %w", err)
	}
	if err := os.Chmod(manifestPath, 0o600); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		if !overwrite {
			return errors.New("refusing to overwrite existing source archive destination")
		}
		if err := os.RemoveAll(destination); err != nil {
			return fmt.Errorf("replace source archive destination: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect source archive destination before commit: %w", err)
	}
	if err := os.Rename(stage, destination); err != nil {
		return fmt.Errorf("commit source archive: %w", err)
	}
	return nil
}
