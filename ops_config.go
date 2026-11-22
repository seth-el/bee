package bee

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const OpsConfigVersion = 1

const (
	MaxOpsConfigBytes       = 1 << 20
	MaxDiagnosticsFields    = 64
	MaxDiagnosticValueBytes = 1024
	MaxRetentionDays        = 3650
)

type OpsConfig struct {
	Version         int            `json:"version"`
	StateRoot       string         `json:"state_root"`
	SessionRoot     string         `json:"session_root"`
	MemoryRoot      string         `json:"memory_root"`
	JobStore        string         `json:"job_store"`
	ArtifactRoot    string         `json:"artifact_root"`
	DiagnosticsRoot string         `json:"diagnostics_root"`
	Retention       OpsRetention   `json:"retention"`
	Health          OpsHealth      `json:"health"`
	Maintenance     OpsMaintenance `json:"maintenance"`
}
type OpsRetention struct {
	SessionsDays    int `json:"sessions_days"`
	DiagnosticsDays int `json:"diagnostics_days"`
	ArtifactsDays   int `json:"artifacts_days"`
	JobsDays        int `json:"jobs_days"`
}
type OpsHealth struct {
	TimeoutMS     int  `json:"timeout_ms"`
	CheckProvider bool `json:"check_provider"`
	CheckChannel  bool `json:"check_channel"`
}
type OpsMaintenance struct {
	DryRunDefault bool  `json:"dry_run_default"`
	MaxItems      int   `json:"max_items"`
	MaxBytes      int64 `json:"max_bytes"`
}

func LoadOpsConfig(path string) (OpsConfig, error) {
	if err := validateOpsConfigFile(path); err != nil {
		return OpsConfig{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return OpsConfig{}, errors.New("inspect operations config")
	}
	if info.Size() > MaxOpsConfigBytes {
		return OpsConfig{}, errors.New("operations config is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return OpsConfig{}, errors.New("read operations config")
	}
	var cfg OpsConfig
	if err := strictOpsJSON(data, &cfg); err != nil {
		return OpsConfig{}, fmt.Errorf("parse operations config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return OpsConfig{}, fmt.Errorf("validate operations config: %w", err)
	}
	return cfg, nil
}
func strictOpsJSON(data []byte, value any) error {
	if err := rejectOpsDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("operations config has trailing JSON")
	}
	return nil
}
func rejectOpsDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok {
					return errors.New("operations config key is not a string")
				}
				if seen[s] {
					return fmt.Errorf("duplicate operations config key %q", s)
				}
				seen[s] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	return nil
}
func (cfg OpsConfig) Validate() error {
	if cfg.Version != OpsConfigVersion {
		return fmt.Errorf("unsupported operations config version %d", cfg.Version)
	}
	directories := []struct{ name, path string }{{"state_root", cfg.StateRoot}, {"session_root", cfg.SessionRoot}, {"memory_root", cfg.MemoryRoot}, {"artifact_root", cfg.ArtifactRoot}, {"diagnostics_root", cfg.DiagnosticsRoot}}
	for _, item := range directories {
		if err := validateOpsPrivatePath(item.name, item.path, true); err != nil {
			return err
		}
	}
	if err := validateOpsPrivatePath("job_store", cfg.JobStore, false); err != nil {
		return err
	}
	if cfg.Retention.DiagnosticsDays < 0 || cfg.Retention.DiagnosticsDays > MaxRetentionDays {
		return errors.New("retention diagnostics_days is out of bounds")
	}
	for _, unsupported := range []struct {
		name  string
		value int
	}{{"sessions_days", cfg.Retention.SessionsDays}, {"artifacts_days", cfg.Retention.ArtifactsDays}, {"jobs_days", cfg.Retention.JobsDays}} {
		if unsupported.value != 0 {
			return fmt.Errorf("retention %s is unsupported; only diagnostics_days is implemented", unsupported.name)
		}
	}
	if cfg.Health.TimeoutMS < 100 || cfg.Health.TimeoutMS > int((5*time.Minute)/time.Millisecond) {
		return errors.New("health timeout_ms is out of bounds")
	}
	if cfg.Maintenance.MaxItems < 0 || cfg.Maintenance.MaxItems > 1000000 || cfg.Maintenance.MaxBytes < 0 || cfg.Maintenance.MaxBytes > (1<<40) {
		return errors.New("maintenance bounds are invalid")
	}
	return nil
}
func validateOpsConfigFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect operations config")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("operations config must be owner-only 0600")
	}
	return rejectOpsSymlinkAncestors(path)
}
func validateOpsPrivatePath(name, path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return fmt.Errorf("%s must be an absolute non-root path", name)
	}
	if strings.Contains(strings.ToLower(filepath.Clean(path)), ".openclaw") {
		return fmt.Errorf("%s must not use an OpenClaw path", name)
	}
	if err := rejectOpsSymlinkAncestors(path); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must not be a symlink", name)
		}
		if directory {
			if !info.IsDir() || info.Mode().Perm() != 0700 {
				return fmt.Errorf("%s must be owner-only 0700", name)
			}
		} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return fmt.Errorf("%s must be owner-only 0600", name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s", name)
	}
	return nil
}
func rejectOpsSymlinkAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path uses symlink")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect path")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}
