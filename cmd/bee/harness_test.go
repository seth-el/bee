package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seth-el/bee"
)

func TestRunHarnessInspectJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	configPath := filepath.Join(home, "config.json")
	data, err := json.Marshal(bee.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	scope := t.TempDir()
	var output, errorOutput bytes.Buffer
	if err := runHarness([]string{"inspect", "--config", configPath, "--scope-root", scope, "--write-root", scope, "--model", "snapshot-model", "--plan", "--json"}, &output, &errorOutput); err != nil {
		t.Fatalf("run harness: %v; stderr=%s", err, errorOutput.String())
	}
	var snapshot bee.HarnessSnapshot
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, output.String())
	}
	if snapshot.Fingerprint == "" || snapshot.CachePrefixFingerprint == "" || snapshot.Provider.Model != "snapshot-model" || !snapshot.RequirePlan || snapshot.Lint.Errors != 0 || len(snapshot.Tools) == 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
