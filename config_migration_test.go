package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSchemaVersionLegacyCompatibilityAndRejection(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"provider":{"model":"legacy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != ConfigSchemaVersion {
		t.Fatalf("legacy effective schema = %d", config.SchemaVersion)
	}
	unsupported := filepath.Join(t.TempDir(), "unsupported.json")
	if err := os.WriteFile(unsupported, []byte(`{"schema_version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(unsupported); err == nil || !strings.Contains(err.Error(), "unsupported config schema_version 99") {
		t.Fatalf("unsupported schema error = %v", err)
	}
	nullVersion := filepath.Join(t.TempDir(), "null-version.json")
	if err := os.WriteFile(nullVersion, []byte(`{"schema_version":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(nullVersion); err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("null schema error = %v", err)
	}
}

func TestLoadConfigRejectsOversizedDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	body := `{"provider":{"model":"` + strings.Repeat("x", MaxConfigBytes) + `"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized config error = %v", err)
	}
}

func TestMigrateConfigFileDryRunAndApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"provider":{"model":"legacy"}}`)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	report, migrated, err := MigrateConfigFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.Applied || !strings.Contains(string(migrated), `"schema_version": 1`) {
		t.Fatalf("dry-run report=%#v data=%s", report, migrated)
	}
	stillOriginal, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stillOriginal) != string(original) {
		t.Fatal("dry-run changed the source config")
	}
	report, _, err = MigrateConfigFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied {
		t.Fatalf("apply report=%#v", report)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("migrated config permissions = %o", info.Mode().Perm())
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	report, migrated, err = MigrateConfigFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed || report.Applied {
		t.Fatalf("second migration report=%#v", report)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(migrated) {
		t.Fatal("idempotent migration preview differs from stored config")
	}
}

func TestEffectiveConfigJSONRedactsSecretFileLocations(t *testing.T) {
	config := DefaultConfig()
	config.Provider.APIKeyFile = "/private/provider.key"
	config.Profiles[0].Provider.APIKeyFile = "/private/profile.key"
	config.Serve.TokenFile = "/private/serve.token"
	config.Semantic.APIKeyFile = "/private/semantic.key"
	config.Web.Browser.AccessKeyFile = "/private/browser.key"
	data, err := EffectiveConfigJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "/private/") || strings.Count(text, "<configured>") < 5 {
		t.Fatalf("effective config was not redacted: %s", text)
	}
}
