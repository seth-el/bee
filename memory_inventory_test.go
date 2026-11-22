package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryMemorySourcesIsReadOnlyBoundedAndClassified(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "MEMORY.md"), "# Long term\nBee decision\n")
	mustWriteInventoryFile(t, filepath.Join(root, "topics", "projects.md"), "# Project\nFact\n")
	mustWriteInventoryFile(t, filepath.Join(root, "2026-08-19.md"), "daily note\n")
	mustWriteInventoryFile(t, filepath.Join(root, ".dreams", "ignored.md"), "dream\n")
	mustWriteInventoryFile(t, filepath.Join(root, "secret.txt"), "not actually secret\n")
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: root}}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Included != 3 || manifest.Excluded != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	if len(manifest.Files) != 4 {
		t.Fatalf("files = %#v", manifest.Files)
	}
	found := map[string]MemoryInventoryFile{}
	for _, file := range manifest.Files {
		found[file.RelativePath] = file
	}
	if found["MEMORY.md"].SourceType != "curated_long_term" || found["topics/projects.md"].SourceType != "curated_topic" || found["2026-08-19.md"].SourceType != "daily_note" {
		t.Fatalf("classification = %#v", found)
	}
	if found["MEMORY.md"].SHA256 == "" || found["MEMORY.md"].LineCount != 2 {
		t.Fatalf("metadata = %#v", found["MEMORY.md"])
	}
	if _, ok := found[".dreams/ignored.md"]; ok {
		t.Fatal("default excluded directory entered manifest")
	}
}

func TestInventoryDetectsDuplicatesWithoutSourceText(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	content := "same memory\n"
	mustWriteInventoryFile(t, filepath.Join(first, "a.md"), content)
	mustWriteInventoryFile(t, filepath.Join(second, "b.md"), content)
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "one", Path: first}, {Label: "two", Path: second}}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Duplicates != 1 {
		t.Fatalf("duplicates = %d files=%#v", manifest.Duplicates, manifest.Files)
	}
	data, _ := json.Marshal(manifest)
	if strings.Contains(string(data), content) {
		t.Fatal("manifest contains source text")
	}
}

func TestInventoryRejectsSecretShapedContentAndSensitivePaths(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "key.pem"), "private key\n")
	mustWriteInventoryFile(t, filepath.Join(root, "notes.md"), "api_key = sk-abcdefghijklmnopqrstuvwxyz123456\n")
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: root}}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Excluded != 2 || manifest.Included != 0 {
		t.Fatalf("manifest = %#v", manifest)
	}
	for _, file := range manifest.Files {
		if file.Status != "excluded" || file.SHA256 != "" {
			t.Fatalf("file = %#v", file)
		}
	}
}

func TestInventoryBoundsAndSymlinkFailClosed(t *testing.T) {
	root := t.TempDir()
	mustWriteInventoryFile(t, filepath.Join(root, "large.md"), strings.Repeat("x", 32))
	manifest, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "memory", Path: root}}, MaxFileBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Excluded != 1 || manifest.Files[0].Reason != "file exceeds max_file_bytes" {
		t.Fatalf("manifest = %#v", manifest)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InventoryMemorySources(MemoryInventoryOptions{Roots: []MemoryInventoryRoot{{Label: "link", Path: link}}}); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestWriteMemoryInventoryRefusesOverwriteAndUses0600(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "manifest.json")
	manifest := MemoryInventoryManifest{FormatVersion: MemoryInventoryFormatVersion, InventoryID: "test"}
	if err := WriteMemoryInventory(path, manifest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := WriteMemoryInventory(path, manifest); err == nil {
		t.Fatal("overwrite accepted")
	}
}

func mustWriteInventoryFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
