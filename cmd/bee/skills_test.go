package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func writeCLISkillPackage(t *testing.T, parent, version string) string {
	t.Helper()
	root := filepath.Join(parent, "review-"+strings.ReplaceAll(version, ".", "-"))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := bee.SkillManifest{SchemaVersion: bee.SkillSchemaVersion, Name: "review", Version: version, Description: "Review changes", Instructions: bee.DefaultSkillInstructions, RequiredTools: []string{"read_file"}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bee.SkillManifestFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bee.DefaultSkillInstructions), []byte("Review carefully.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSkillCLILifecycleDoesNotRequireProviderConfig(t *testing.T) {
	registryRoot := filepath.Join(t.TempDir(), "skills")
	sources := t.TempDir()
	v1 := writeCLISkillPackage(t, sources, "1.0.0")
	v2 := writeCLISkillPackage(t, sources, "1.1.0")
	var output bytes.Buffer
	var errors bytes.Buffer

	if err := run([]string{"skill", "test", "--source", v1}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "valid review@1.0.0") {
		t.Fatalf("test output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"skill", "install", "--root", registryRoot, "--enable", v1}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "installed and enabled review@1.0.0") {
		t.Fatalf("install output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"skill", "install", "--root", registryRoot, v2}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"skill", "enable", "--root", registryRoot, "--version", "1.1.0", "review"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"skill", "rollback", "--root", registryRoot, "review"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "rolled back review to 1.0.0") {
		t.Fatalf("rollback output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"skill", "list", "--root", registryRoot}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "review@1.0.0  enabled") || !strings.Contains(output.String(), "review@1.1.0  disabled") {
		t.Fatalf("list output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"skill", "remove", "--root", registryRoot, "--version", "1.1.0", "review"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "would remove review@1.1.0") {
		t.Fatalf("dry-run output=%q", output.String())
	}
	if _, err := os.Stat(filepath.Join(registryRoot, "review", "1.1.0", bee.SkillManifestFilename)); err != nil {
		t.Fatalf("dry-run removed package: %v", err)
	}
	output.Reset()
	if err := run([]string{"skill", "remove", "--root", registryRoot, "--version", "1.1.0", "--apply", "review"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(registryRoot, "review", "1.1.0")); !os.IsNotExist(err) {
		t.Fatalf("applied removal left package: %v", err)
	}
}
