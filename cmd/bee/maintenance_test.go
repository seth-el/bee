package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	bee "github.com/seth-el/bee"
)

func TestMaintenanceCLIBackupRestore(t *testing.T) {
	root := t.TempDir()
	source := writeMaintenanceTestOpsConfig(t, filepath.Join(root, "source"))
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), "sessions", "session.jsonl"), []byte("accepted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "bee-backup.tar.gz")

	var output, errorOutput bytes.Buffer
	if err := runMaintenance([]string{"backup", "--ops-config", source, "--destination", archive}, &output, &errorOutput); err != nil {
		t.Fatalf("backup: %v stderr=%s", err, errorOutput.String())
	}
	var manifest bee.BackupManifest
	if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
		t.Fatalf("decode backup manifest: %v output=%q", err, output.String())
	}
	if manifest.FormatVersion != bee.BackupFormatVersion || manifest.TotalBytes == 0 {
		t.Fatalf("manifest=%#v", manifest)
	}

	target := writeMaintenanceTestOpsConfig(t, filepath.Join(root, "target"))
	output.Reset()
	errorOutput.Reset()
	if err := runMaintenance([]string{"restore", "--ops-config", target, "--source", archive}, &output, &errorOutput); err != nil {
		t.Fatalf("restore dry-run: %v stderr=%s", err, errorOutput.String())
	}
	var preview bee.RestoreReport
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		t.Fatalf("decode restore preview: %v output=%q", err, output.String())
	}
	if preview.Applied || preview.WouldWrite == 0 {
		t.Fatalf("preview=%#v", preview)
	}

	output.Reset()
	errorOutput.Reset()
	if err := runMaintenance([]string{"restore", "--ops-config", target, "--source", archive, "--apply"}, &output, &errorOutput); err != nil {
		t.Fatalf("restore apply: %v stderr=%s", err, errorOutput.String())
	}
	var applied bee.RestoreReport
	if err := json.Unmarshal(output.Bytes(), &applied); err != nil {
		t.Fatalf("decode restore result: %v output=%q", err, output.String())
	}
	if !applied.Applied {
		t.Fatalf("applied=%#v", applied)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "sessions", "session.jsonl")); err != nil {
		t.Fatalf("restored session: %v", err)
	}
}

func writeMaintenanceTestOpsConfig(t *testing.T, root string) string {
	t.Helper()
	state := filepath.Join(root, "state")
	sessions := filepath.Join(root, "sessions")
	memory := filepath.Join(root, "memory")
	artifacts := filepath.Join(root, "artifacts")
	diagnostics := filepath.Join(root, "diagnostics")
	for _, path := range []string{state, sessions, memory, artifacts, diagnostics} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	jobStore := filepath.Join(state, "jobs.json")
	if err := os.WriteFile(jobStore, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := bee.OpsConfig{
		Version:         bee.OpsConfigVersion,
		StateRoot:       state,
		SessionRoot:     sessions,
		MemoryRoot:      memory,
		JobStore:        jobStore,
		ArtifactRoot:    artifacts,
		DiagnosticsRoot: diagnostics,
		Retention:       bee.OpsRetention{DiagnosticsDays: 30},
		Health:          bee.OpsHealth{TimeoutMS: 1000},
		Maintenance:     bee.OpsMaintenance{DryRunDefault: true, MaxItems: 100, MaxBytes: 1 << 20},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "ops.json")
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
