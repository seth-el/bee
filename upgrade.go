package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const ReleaseManifestVersion = 1

type ReleaseManifest struct {
	FormatVersion     int            `json:"format_version"`
	BeeCommit         string         `json:"bee_commit,omitempty"`
	OpsConfigVersion  int            `json:"ops_config_version"`
	StateSchemas      map[string]int `json:"state_schemas"`
	CreatedAt         time.Time      `json:"created_at"`
	AppliedMigrations []string       `json:"applied_migrations,omitempty"`
}
type Migration struct {
	ID          string
	Domain      string
	FromVersion int
	ToVersion   int
	Description string
	Plan        func(context.Context, OpsConfig) (string, error)
	Apply       func(context.Context, OpsConfig) error
}
type MigrationReport struct {
	DryRun   bool            `json:"dry_run"`
	Current  ReleaseManifest `json:"current"`
	Target   ReleaseManifest `json:"target"`
	Required []string        `json:"required,omitempty"`
	Errors   []string        `json:"errors,omitempty"`
	Applied  bool            `json:"applied"`
}

func DefaultReleaseManifest(now time.Time) ReleaseManifest {
	return ReleaseManifest{FormatVersion: ReleaseManifestVersion, OpsConfigVersion: OpsConfigVersion, StateSchemas: map[string]int{"sessions": 1, "memory": 1, "jobs": 1, "artifacts": 1, "diagnostics": 1}, CreatedAt: now.UTC()}
}
func LoadReleaseManifest(path string) (ReleaseManifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultReleaseManifest(time.Now()), nil
	}
	if err != nil {
		return ReleaseManifest{}, errors.New("read release manifest")
	}
	var m ReleaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return ReleaseManifest{}, errors.New("decode release manifest")
	}
	if m.FormatVersion != ReleaseManifestVersion {
		return ReleaseManifest{}, errors.New("unsupported release manifest format")
	}
	return m, nil
}
func SaveReleaseManifest(path string, m ReleaseManifest) error {
	if path == "" {
		return errors.New("release manifest path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func PlanMigrations(ctx context.Context, cfg OpsConfig, manifestPath string, migrations []Migration) (MigrationReport, error) {
	return migrate(ctx, cfg, manifestPath, migrations, true)
}
func ApplyMigrations(ctx context.Context, cfg OpsConfig, manifestPath string, migrations []Migration) (MigrationReport, error) {
	return migrate(ctx, cfg, manifestPath, migrations, false)
}
func migrate(ctx context.Context, cfg OpsConfig, path string, migrations []Migration, dry bool) (MigrationReport, error) {
	report := MigrationReport{DryRun: dry}
	if ctx == nil {
		return report, errors.New("migration context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return report, err
	}
	lock, err := acquireSchedulerLock(filepath.Join(cfg.StateRoot, "upgrade.lock"))
	if err != nil {
		return report, err
	}
	defer lock.Close()
	current, err := LoadReleaseManifest(path)
	if err != nil {
		return report, err
	}
	target := DefaultReleaseManifest(time.Now())
	report.Current = current
	report.Target = target
	for _, migration := range migrations {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		version := current.StateSchemas[migration.Domain]
		if version == migration.FromVersion {
			report.Required = append(report.Required, migration.ID)
			if migration.Plan != nil {
				if _, err := migration.Plan(ctx, cfg); err != nil {
					return report, err
				}
			}
			target.StateSchemas[migration.Domain] = migration.ToVersion
			target.AppliedMigrations = append(target.AppliedMigrations, current.AppliedMigrations...)
			target.AppliedMigrations = append(target.AppliedMigrations, migration.ID)
		}
	}
	report.Target = target
	if dry || len(report.Required) == 0 {
		return report, nil
	}
	for _, migration := range migrations {
		for _, id := range report.Required {
			if id == migration.ID {
				if migration.Apply == nil {
					return report, fmt.Errorf("migration %s has no apply function", id)
				}
				if err := migration.Apply(ctx, cfg); err != nil {
					return report, err
				}
			}
		}
	}
	if err := SaveReleaseManifest(path, target); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}
