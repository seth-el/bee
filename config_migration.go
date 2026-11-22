package bee

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type ConfigMigrationReport struct {
	Path        string `json:"path"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	Changed     bool   `json:"changed"`
	Applied     bool   `json:"applied"`
}

// MigrateConfigFile adds the current schema marker to a validated legacy
// document. It never changes an unsupported or otherwise invalid config.
// Dry-run returns the exact deterministic document that apply would write.
func MigrateConfigFile(path string, apply bool) (ConfigMigrationReport, []byte, error) {
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return ConfigMigrationReport{}, nil, err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("resolve config path: %w", err)
	}
	path = filepath.Clean(abs)
	info, err := os.Lstat(path)
	if err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("inspect config file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ConfigMigrationReport{}, nil, errors.New("config file must be a regular non-symlink file")
	}
	if info.Size() > MaxConfigBytes {
		return ConfigMigrationReport{}, nil, fmt.Errorf("config file exceeds %d bytes", MaxConfigBytes)
	}
	if _, err := LoadConfig(path); err != nil {
		return ConfigMigrationReport{}, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("read config file: %w", err)
	}
	var document map[string]json.RawMessage
	if err := decodeConfigJSON(data, &document); err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("parse config file: %w", err)
	}
	from := 0
	if raw, ok := document["schema_version"]; ok {
		if err := json.Unmarshal(raw, &from); err != nil {
			return ConfigMigrationReport{}, nil, errors.New("config schema_version must be an integer")
		}
	}
	report := ConfigMigrationReport{Path: path, FromVersion: from, ToVersion: ConfigSchemaVersion}
	versionJSON, err := json.Marshal(ConfigSchemaVersion)
	if err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("encode config schema version: %w", err)
	}
	document["schema_version"] = versionJSON
	migrated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return ConfigMigrationReport{}, nil, fmt.Errorf("encode migrated config: %w", err)
	}
	migrated = append(migrated, '\n')
	report.Changed = !bytes.Equal(data, migrated)
	report.Applied = apply && report.Changed
	if report.Applied {
		if err := writeMigratedConfig(path, migrated); err != nil {
			return ConfigMigrationReport{}, nil, err
		}
	}
	return report, migrated, nil
}

func writeMigratedConfig(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".bee-config-migrate-*")
	if err != nil {
		return fmt.Errorf("create migrated config: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect migrated config: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write migrated config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync migrated config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close migrated config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	keep = true
	return nil
}

// EffectiveConfigJSON renders validated effective settings while replacing
// secret-file locations with a content-free marker.
func EffectiveConfigJSON(config Config) ([]byte, error) {
	copy := config
	redact := func(value string) string {
		if value == "" {
			return ""
		}
		return "<configured>"
	}
	copy.Provider.APIKeyFile = redact(copy.Provider.APIKeyFile)
	copy.Profiles = append([]ProviderProfile(nil), config.Profiles...)
	for index := range copy.Profiles {
		copy.Profiles[index].Provider.APIKeyFile = redact(copy.Profiles[index].Provider.APIKeyFile)
	}
	copy.Serve.TokenFile = redact(copy.Serve.TokenFile)
	copy.Semantic.APIKeyFile = redact(copy.Semantic.APIKeyFile)
	copy.Web.Browser.AccessKeyFile = redact(copy.Web.Browser.AccessKeyFile)
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(copy); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
