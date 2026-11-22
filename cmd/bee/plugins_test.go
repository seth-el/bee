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

func writeCLIPluginPackage(t *testing.T, parent, version string) string {
	t.Helper()
	root := filepath.Join(parent, "echoer-"+strings.ReplaceAll(version, ".", "-"))
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "extension"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := bee.PluginManifest{SchemaVersion: bee.PluginSchemaVersion, Name: "echoer", Version: version, Description: "Echo plugin", Extension: &bee.SkillExtension{Name: "plugin.echoer", Command: "bin/extension", Tools: []string{"echo"}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bee.PluginManifestFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPluginCLILifecycleDoesNotRequireProviderConfig(t *testing.T) {
	registryRoot := filepath.Join(t.TempDir(), "plugins")
	sources := t.TempDir()
	v1 := writeCLIPluginPackage(t, sources, "1.0.0")
	v2 := writeCLIPluginPackage(t, sources, "1.1.0")
	var output bytes.Buffer
	var errors bytes.Buffer

	if err := run([]string{"plugin", "test", "--source", v1}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "valid echoer@1.0.0") {
		t.Fatalf("test output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"plugin", "install", "--root", registryRoot, "--enable", v1}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"plugin", "install", "--root", registryRoot, v2}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"plugin", "enable", "--root", registryRoot, "--version", "1.1.0", "echoer"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"plugin", "rollback", "--root", registryRoot, "echoer"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "rolled back echoer to 1.0.0") {
		t.Fatalf("rollback output=%q", output.String())
	}
	output.Reset()
	if err := run([]string{"plugin", "remove", "--root", registryRoot, "--version", "1.1.0", "--apply", "echoer"}, strings.NewReader(""), &output, &errors); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(registryRoot, "echoer", "1.1.0")); !os.IsNotExist(err) {
		t.Fatalf("removed plugin remains: %v", err)
	}
}
