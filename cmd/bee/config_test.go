package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCommandsValidateEffectiveAndMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":{"model":"local"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, errors bytes.Buffer
	if err := runConfig([]string{"validate", "--config", path}, &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "schema_version=1") {
		t.Fatalf("validate output = %q", output.String())
	}
	output.Reset()
	if err := runConfig([]string{"effective", "--config", path}, &output, &errors); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"schema_version": 1`) {
		t.Fatalf("effective output = %q", output.String())
	}
	output.Reset()
	if err := runConfig([]string{"migrate", "--config", path, "--apply"}, &output, &errors); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema_version": 1`) {
		t.Fatalf("migrated config = %s", data)
	}
}
