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
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	MemoryInventoryFormatVersion  = 1
	DefaultInventoryMaxFiles      = 10_000
	DefaultInventoryMaxFileBytes  = 4 << 20
	DefaultInventoryMaxTotalBytes = 128 << 20
	MaxInventoryFiles             = 100_000
	MaxInventoryFileBytes         = 64 << 20
	MaxInventoryTotalBytes        = 1 << 30
)

type MemoryInventoryRoot struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

type MemoryInventoryOptions struct {
	Roots         []MemoryInventoryRoot
	Excludes      []string
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

type MemoryInventoryFile struct {
	RootLabel       string    `json:"root_label"`
	RelativePath    string    `json:"relative_path"`
	SourceType      string    `json:"source_type"`
	Status          string    `json:"status"`
	Reason          string    `json:"reason,omitempty"`
	SizeBytes       int64     `json:"size_bytes"`
	LineCount       int       `json:"line_count,omitempty"`
	ModifiedAt      time.Time `json:"modified_at,omitempty"`
	SHA256          string    `json:"sha256,omitempty"`
	DuplicateOf     string    `json:"duplicate_of,omitempty"`
	SecretSuspected bool      `json:"secret_suspected,omitempty"`
}

type MemoryInventoryManifest struct {
	FormatVersion int                   `json:"format_version"`
	InventoryID   string                `json:"inventory_id"`
	CreatedAt     time.Time             `json:"created_at"`
	Roots         []MemoryInventoryRoot `json:"roots"`
	Excludes      []string              `json:"excludes,omitempty"`
	Files         []MemoryInventoryFile `json:"files"`
	Included      int                   `json:"included"`
	Excluded      int                   `json:"excluded"`
	Errors        int                   `json:"errors"`
	Duplicates    int                   `json:"duplicates"`
	TotalBytes    int64                 `json:"total_bytes"`
	SkippedBytes  int64                 `json:"skipped_bytes"`
	Truncated     bool                  `json:"truncated"`
	Diagnostics   []string              `json:"diagnostics,omitempty"`
}

type inventoryFileInfo struct {
	path       string
	root       MemoryInventoryRoot
	relative   string
	info       os.FileInfo
	sourceType string
}

var inventorySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~-]{20,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|password)\s*[:=]\s*["']?[A-Za-z0-9_./+=:-]{20,}`),
}

// InventoryMemorySources scans only explicitly supplied roots. It reads
// bounded file bytes to hash/count/classify them, never stores source text in
// the manifest, never writes to the roots, and never creates memory records.
func InventoryMemorySources(options MemoryInventoryOptions) (MemoryInventoryManifest, error) {
	options, err := normalizeInventoryOptions(options)
	if err != nil {
		return MemoryInventoryManifest{}, err
	}
	if len(options.Roots) == 0 {
		return MemoryInventoryManifest{}, errors.New("at least one memory inventory source root is required")
	}
	roots, err := validateInventoryRoots(options.Roots)
	if err != nil {
		return MemoryInventoryManifest{}, err
	}
	options.Excludes = normalizeInventoryExcludes(options.Excludes)
	manifest := MemoryInventoryManifest{
		FormatVersion: MemoryInventoryFormatVersion,
		InventoryID:   fmt.Sprintf("memory-inventory-%d", time.Now().UTC().UnixNano()),
		CreatedAt:     time.Now().UTC(),
		Roots:         roots,
		Excludes:      options.Excludes,
		Files:         make([]MemoryInventoryFile, 0),
	}
	seenPaths := make(map[string]struct{})
	seenHashes := make(map[string]string)
	for _, root := range roots {
		entries, collectErr := collectInventoryFiles(root, options.Excludes)
		if collectErr != nil {
			manifest.Errors++
			manifest.Diagnostics = appendBoundedDiagnostic(manifest.Diagnostics, root.Label+": "+collectErr.Error())
			continue
		}
		for _, entry := range entries {
			if len(manifest.Files) >= options.MaxFiles {
				manifest.Truncated = true
				manifest.Diagnostics = appendBoundedDiagnostic(manifest.Diagnostics, "maximum inventory file count reached")
				break
			}
			canonical, canonicalErr := filepath.EvalSymlinks(entry.path)
			if canonicalErr != nil {
				manifest.Errors++
				manifest.Files = append(manifest.Files, inventoryErrorFile(entry, "cannot resolve source path"))
				continue
			}
			if _, exists := seenPaths[canonical]; exists {
				continue
			}
			seenPaths[canonical] = struct{}{}
			result := inventoryOneFile(entry, options, manifest.TotalBytes, seenHashes)
			manifest.Files = append(manifest.Files, result)
			switch result.Status {
			case "included":
				manifest.Included++
				manifest.TotalBytes += result.SizeBytes
				if result.DuplicateOf != "" {
					manifest.Duplicates++
				}
			case "excluded":
				manifest.Excluded++
				manifest.SkippedBytes += result.SizeBytes
			case "error":
				manifest.Errors++
			}
		}
		if manifest.Truncated {
			break
		}
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		if manifest.Files[i].RootLabel != manifest.Files[j].RootLabel {
			return manifest.Files[i].RootLabel < manifest.Files[j].RootLabel
		}
		return manifest.Files[i].RelativePath < manifest.Files[j].RelativePath
	})
	manifest.Diagnostics = uniqueStrings(manifest.Diagnostics)
	return manifest, nil
}

func normalizeInventoryOptions(options MemoryInventoryOptions) (MemoryInventoryOptions, error) {
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultInventoryMaxFiles
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = DefaultInventoryMaxFileBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = DefaultInventoryMaxTotalBytes
	}
	if options.MaxFiles < 1 || options.MaxFiles > MaxInventoryFiles {
		return MemoryInventoryOptions{}, fmt.Errorf("inventory max files must be between 1 and %d", MaxInventoryFiles)
	}
	if options.MaxFileBytes < 1 || options.MaxFileBytes > MaxInventoryFileBytes {
		return MemoryInventoryOptions{}, fmt.Errorf("inventory max file bytes must be between 1 and %d", MaxInventoryFileBytes)
	}
	if options.MaxTotalBytes < 1 || options.MaxTotalBytes > MaxInventoryTotalBytes {
		return MemoryInventoryOptions{}, fmt.Errorf("inventory max total bytes must be between 1 and %d", MaxInventoryTotalBytes)
	}
	return options, nil
}

func validateInventoryRoots(input []MemoryInventoryRoot) ([]MemoryInventoryRoot, error) {
	roots := make([]MemoryInventoryRoot, 0, len(input))
	seenLabels := make(map[string]struct{}, len(input))
	seenPaths := make(map[string]struct{}, len(input))
	for _, root := range input {
		label := strings.TrimSpace(root.Label)
		path := filepath.Clean(strings.TrimSpace(root.Path))
		if label == "" {
			label = filepath.Base(path)
		}
		if err := validateSafeID("inventory root label", label, 128); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(path) || path == string(filepath.Separator) {
			return nil, fmt.Errorf("inventory root %q must be an absolute non-root path", label)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect inventory root %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("inventory root %q must not be a symlink", path)
		}
		if _, exists := seenLabels[label]; exists {
			return nil, fmt.Errorf("duplicate inventory root label %q", label)
		}
		if _, exists := seenPaths[path]; exists {
			return nil, fmt.Errorf("duplicate inventory root path %q", path)
		}
		seenLabels[label] = struct{}{}
		seenPaths[path] = struct{}{}
		roots = append(roots, MemoryInventoryRoot{Label: label, Path: path})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Label < roots[j].Label })
	return roots, nil
}

func normalizeInventoryExcludes(input []string) []string {
	defaults := []string{".dreams", ".openclaw-runtime", ".openclaw", "session-corpus", "generated", "archive/dreaming"}
	all := append(defaults, input...)
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

func collectInventoryFiles(root MemoryInventoryRoot, excludes []string) ([]inventoryFileInfo, error) {
	info, err := os.Stat(root.Path)
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return []inventoryFileInfo{{path: root.Path, root: root, relative: filepath.Base(root.Path), info: info, sourceType: classifyInventorySource(root.Label, filepath.Base(root.Path))}}, nil
	}
	if !info.IsDir() {
		return nil, errors.New("inventory root must be a regular file or directory")
	}
	entries := make([]inventoryFileInfo, 0)
	err = filepath.WalkDir(root.Path, func(path string, dirEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root.Path, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relativeSlash := filepath.ToSlash(relative)
		if inventoryPathExcluded(relativeSlash, excludes) {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if dirEntry.Type()&os.ModeSymlink != 0 {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if dirEntry.IsDir() {
			return nil
		}
		fileInfo, err := dirEntry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		entries = append(entries, inventoryFileInfo{path: path, root: root, relative: relativeSlash, info: fileInfo, sourceType: classifyInventorySource(root.Label, relativeSlash)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relative < entries[j].relative })
	return entries, nil
}

func inventoryPathExcluded(relative string, excludes []string) bool {
	relative = filepath.ToSlash(relative)
	parts := strings.Split(relative, "/")
	for _, exclude := range excludes {
		exclude = filepath.ToSlash(exclude)
		if relative == exclude || strings.HasPrefix(relative, exclude+"/") {
			return true
		}
		if matched, _ := filepath.Match(exclude, relative); matched {
			return true
		}
		for _, part := range parts {
			if matched, _ := filepath.Match(exclude, part); matched {
				return true
			}
		}
	}
	return false
}

func inventoryOneFile(entry inventoryFileInfo, options MemoryInventoryOptions, totalBytes int64, seenHashes map[string]string) MemoryInventoryFile {
	result := MemoryInventoryFile{RootLabel: entry.root.Label, RelativePath: entry.relative, SourceType: entry.sourceType, Status: "included", SizeBytes: entry.info.Size(), ModifiedAt: entry.info.ModTime().UTC()}
	if inventoryPathExcluded(entry.relative, options.Excludes) || inventorySensitivePath(entry.relative) {
		result.Status = "excluded"
		result.Reason = "excluded path"
		return result
	}
	if entry.info.Size() > options.MaxFileBytes {
		result.Status = "excluded"
		result.Reason = "file exceeds max_file_bytes"
		return result
	}
	if entry.info.Size() > options.MaxTotalBytes-totalBytes {
		result.Status = "excluded"
		result.Reason = "total byte budget would be exceeded"
		return result
	}
	file, err := os.Open(entry.path)
	if err != nil {
		return inventoryErrorFile(entry, "read source file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, options.MaxFileBytes+1))
	if err != nil {
		return inventoryErrorFile(entry, "read source file")
	}
	if int64(len(data)) > options.MaxFileBytes {
		result.Status = "excluded"
		result.Reason = "file exceeds max_file_bytes"
		return result
	}
	result.LineCount = countInventoryLines(data)
	if inventoryLooksSecret(data) {
		result.Status = "excluded"
		result.Reason = "secret-shaped content"
		result.SecretSuspected = true
		return result
	}
	hash := sha256.Sum256(data)
	result.SHA256 = hex.EncodeToString(hash[:])
	if previous, exists := seenHashes[result.SHA256]; exists {
		result.DuplicateOf = previous
	} else {
		seenHashes[result.SHA256] = entry.root.Label + ":" + entry.relative
	}
	return result
}

func inventoryErrorFile(entry inventoryFileInfo, reason string) MemoryInventoryFile {
	return MemoryInventoryFile{RootLabel: entry.root.Label, RelativePath: entry.relative, SourceType: entry.sourceType, Status: "error", Reason: reason, SizeBytes: entry.info.Size(), ModifiedAt: entry.info.ModTime().UTC()}
}

func countInventoryLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := 1 + strings.Count(string(data), "\n")
	if data[len(data)-1] == '\n' {
		lines--
	}
	return lines
}

func inventoryLooksSecret(data []byte) bool {
	text := string(data)
	for _, pattern := range inventorySecretPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func inventorySensitivePath(relative string) bool {
	base := strings.ToLower(filepath.Base(relative))
	return base == ".env" || strings.Contains(base, "secret") || strings.Contains(base, "credential") || strings.Contains(base, "password") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key")
}

func classifyInventorySource(label, relative string) string {
	lower := strings.ToLower(filepath.ToSlash(relative))
	base := strings.ToLower(filepath.Base(relative))
	switch {
	case base == "memory.md":
		return "curated_long_term"
	case strings.Contains(lower, "/topics/") || strings.HasPrefix(lower, "topics/"):
		return "curated_topic"
	case strings.Contains(lower, "/archive/") || strings.HasPrefix(lower, "archive/"):
		return "archive"
	case dailyMemoryNamePattern.MatchString(base):
		return "daily_note"
	case label != "":
		return "source_file"
	default:
		return "unknown"
	}
}

var dailyMemoryNamePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:-[0-9]{3,4})?\.(?:md|markdown|txt)$`)

func appendBoundedDiagnostic(values []string, value string) []string {
	if len(value) > 512 {
		value = value[:512]
	}
	if len(values) >= 128 {
		return values
	}
	return append(values, value)
}

// WriteMemoryInventory writes a new owner-only JSON manifest and refuses to
// overwrite an existing path. The source roots remain read-only.
func WriteMemoryInventory(path string, manifest MemoryInventoryManifest) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("inventory output path is required")
	}
	if !filepath.IsAbs(path) {
		return errors.New("inventory output path must be absolute")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("refusing to overwrite existing inventory manifest")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect inventory output path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create inventory output directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inventory manifest: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create inventory manifest: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write inventory manifest: %w", err)
	}
	return file.Chmod(0o600)
}
