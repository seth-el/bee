package bee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveMemorySourcesCopiesPrivateManifestAndLeavesSourcesUnchanged(t *testing.T) {
	beeHome := t.TempDir()
	t.Setenv("BEE_HOME", beeHome)
	source := t.TempDir()
	memoryPath := filepath.Join(source, "MEMORY.md")
	topicPath := filepath.Join(source, "topics", "bee.md")
	if err := os.MkdirAll(filepath.Dir(topicPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memoryPath, []byte("# Memory\nprefer concise answers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topicPath, []byte("# Bee\narchive source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeMemory, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTopic, err := os.ReadFile(topicPath)
	if err != nil {
		t.Fatal(err)
	}
	memoryInfo, err := os.Stat(memoryPath)
	if err != nil {
		t.Fatal(err)
	}

	report, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: source}}, Name: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied || report.DryRun || len(report.Manifest.Files) != 2 {
		t.Fatalf("report=%#v", report)
	}
	archiveRoot, err := DefaultBeeSourceMemoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !scopeWithin(archiveRoot, report.Destination) || filepath.Base(report.Destination) != "initial" {
		t.Fatalf("destination escaped archive root: %s", report.Destination)
	}
	if info, err := os.Stat(report.Destination); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("destination info=%#v err=%v", info, err)
	}
	for _, file := range report.Manifest.Files {
		path := filepath.Join(report.Destination, filepath.FromSlash(file.ArchivePath))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("archive file %s mode=%o", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if got := hex.EncodeToString(hash[:]); got != file.SHA256 {
			t.Fatalf("hash for %s=%s want %s", file.ArchivePath, got, file.SHA256)
		}
	}
	manifestInfo, err := os.Stat(report.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode=%o", manifestInfo.Mode().Perm())
	}
	manifestData, err := os.ReadFile(report.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest MemorySourceArchiveManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != MemorySourceArchiveFormatVersion || manifest.TotalBytes != int64(len(beforeMemory)+len(beforeTopic)) {
		t.Fatalf("manifest=%#v", manifest)
	}

	afterMemory, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	afterTopic, err := os.ReadFile(topicPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterMemory) != string(beforeMemory) || string(afterTopic) != string(beforeTopic) || !afterInfo.ModTime().Equal(memoryInfo.ModTime()) {
		t.Fatal("source root was modified")
	}
}

func TestArchiveMemorySourcesDryRunAndNoOverwrite(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	source := t.TempDir()
	path := filepath.Join(source, "note.md")
	if err := os.WriteFile(path, []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "notes", Path: source}}, Name: "preview", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.Applied {
		t.Fatalf("preview=%#v", preview)
	}
	if _, err := os.Stat(preview.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run destination exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(preview.Destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created archive parent: %v", err)
	}

	applied, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "notes", Path: source}}, Name: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(applied.Destination, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "notes", Path: source}}, Name: "preview"}); err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("overwrite error=%v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep me" {
		t.Fatalf("existing archive changed data=%q err=%v", data, err)
	}
}

func TestArchiveMemorySourcesExcludesDreamingAndReportsPaths(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	root := t.TempDir()
	files := map[string][]byte{
		"MEMORY.md":                      []byte("durable memory\n"),
		"2026-08-21.md":                  []byte("daily memory\n"),
		".dreams/short-term-recall.json": []byte(strings.Repeat("dreaming state\n", 1024)),
		"dreaming/light/2026-08-21.md":   []byte("light dream\n"),
		"topics/keep.md":                 []byte("approved topic\n"),
		"notes/extra.txt":                []byte("exclude me by explicit glob\n"),
	}
	for relative, data := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	report, err := ArchiveMemorySources(MemorySourceArchiveOptions{
		Roots:        []MemoryInventoryRoot{{Label: "memory", Path: root}},
		Excludes:     []string{"notes/*.txt"},
		Name:         "dreaming-exclusions",
		MaxFileBytes: 64,
		DryRun:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Applied || report.Manifest.Included != 3 || report.Manifest.Excluded != 3 {
		t.Fatalf("report=%#v", report)
	}
	if report.Manifest.TotalBytes != int64(len(files["MEMORY.md"])+len(files["2026-08-21.md"])+len(files["topics/keep.md"])) {
		t.Fatalf("total bytes=%d", report.Manifest.TotalBytes)
	}
	if report.Manifest.SkippedBytes != int64(len(files[".dreams/short-term-recall.json"])+len(files["dreaming/light/2026-08-21.md"])+len(files["notes/extra.txt"])) {
		t.Fatalf("skipped bytes=%d", report.Manifest.SkippedBytes)
	}
	if len(report.Manifest.Excludes) != 3 {
		t.Fatalf("excludes=%#v", report.Manifest.Excludes)
	}
	excluded := make(map[string]bool, len(report.Manifest.ExcludedFiles))
	for _, file := range report.Manifest.ExcludedFiles {
		excluded[file.RelativePath] = true
		if file.Reason != "excluded path" {
			t.Fatalf("excluded file=%#v", file)
		}
	}
	for _, relative := range []string{".dreams/short-term-recall.json", "dreaming/light/2026-08-21.md", "notes/extra.txt"} {
		if !excluded[relative] {
			t.Fatalf("missing excluded path %q: %#v", relative, report.Manifest.ExcludedFiles)
		}
	}
	for _, file := range report.Manifest.Files {
		if file.RelativePath == ".dreams/short-term-recall.json" || file.RelativePath == "dreaming/light/2026-08-21.md" {
			t.Fatalf("Dreaming file included: %#v", file)
		}
	}
}

func TestArchiveMemorySourcesDoesNotMutateBeeWorkspaceSourceRoot(t *testing.T) {
	beeHome := t.TempDir()
	t.Setenv("BEE_HOME", beeHome)
	source := filepath.Join(beeHome, "workspace", "memory")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "note.md")
	if err := os.WriteFile(path, []byte("workspace source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: source}}, Name: "workspace-source"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("source root mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "workspace source\n" {
		t.Fatalf("source file changed data=%q err=%v", data, err)
	}
}

func TestArchiveMemorySourcesRejectsSymlinkAndSecretSources(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "link.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "root", Path: root}}, Name: "symlink"}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink error=%v", err)
		}
	})
	t.Run("secret", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "token.txt"), []byte("Bearer abcdefghijklmnopqrstuvwxyz123456\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ArchiveMemorySources(MemorySourceArchiveOptions{Roots: []MemoryInventoryRoot{{Label: "root", Path: root}}, Name: "secret"}); err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("secret error=%v", err)
		}
	})
}
