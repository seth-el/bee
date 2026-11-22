package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CleanupOptions struct {
	Now      time.Time
	DryRun   bool
	MaxItems int
	MaxBytes int64
}
type CleanupCandidate struct {
	Domain     string    `json:"domain"`
	Identifier string    `json:"identifier"`
	Reason     string    `json:"reason"`
	SizeBytes  int64     `json:"size_bytes"`
	EligibleAt time.Time `json:"eligible_at"`
}
type CleanupReport struct {
	DryRun         bool               `json:"dry_run"`
	Candidates     []CleanupCandidate `json:"candidates"`
	Removed        []CleanupCandidate `json:"removed"`
	Skipped        []CleanupCandidate `json:"skipped"`
	Errors         []string           `json:"errors,omitempty"`
	BytesReclaimed int64              `json:"bytes_reclaimed"`
}

func PlanCleanup(ctx context.Context, cfg OpsConfig, options CleanupOptions) (CleanupReport, error) {
	options.DryRun = true
	return cleanup(ctx, cfg, options)
}
func ApplyCleanup(ctx context.Context, cfg OpsConfig, options CleanupOptions) (CleanupReport, error) {
	options.DryRun = false
	return cleanup(ctx, cfg, options)
}
func cleanup(ctx context.Context, cfg OpsConfig, options CleanupOptions) (CleanupReport, error) {
	report := CleanupReport{DryRun: options.DryRun, Candidates: []CleanupCandidate{}, Removed: []CleanupCandidate{}, Skipped: []CleanupCandidate{}}
	if ctx == nil {
		return report, errors.New("cleanup context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return report, err
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if options.MaxItems <= 0 {
		options.MaxItems = 100
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 128 << 20
	}
	if err := collectDiagnostics(ctx, &report, cfg, options); err != nil {
		return report, err
	}
	sort.Slice(report.Candidates, func(i, j int) bool { return report.Candidates[i].Identifier < report.Candidates[j].Identifier })
	for _, candidate := range report.Candidates {
		if len(report.Removed) >= options.MaxItems || report.BytesReclaimed+candidate.SizeBytes > options.MaxBytes {
			report.Skipped = append(report.Skipped, candidate)
			continue
		}
		if options.DryRun {
			continue
		}
		if err := removeCandidate(candidate, cfg); err != nil {
			report.Errors = append(report.Errors, err.Error())
			continue
		}
		report.Removed = append(report.Removed, candidate)
		report.BytesReclaimed += candidate.SizeBytes
	}
	return report, nil
}
func collectDiagnostics(ctx context.Context, report *CleanupReport, cfg OpsConfig, options CleanupOptions) error {
	entries, err := os.ReadDir(cfg.DiagnosticsRoot)
	if err != nil {
		return err
	}
	cutoff := options.Now.Add(-time.Duration(cfg.Retention.DiagnosticsDays) * 24 * time.Hour)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "diagnostics-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			report.Candidates = append(report.Candidates, CleanupCandidate{Domain: "diagnostics", Identifier: entry.Name(), Reason: "retention expired", SizeBytes: info.Size(), EligibleAt: cutoff})
		}
	}
	return nil
}
func removeCandidate(candidate CleanupCandidate, cfg OpsConfig) error {
	if candidate.Domain != "diagnostics" {
		return errors.New("unsupported cleanup domain")
	}
	path := filepath.Join(cfg.DiagnosticsRoot, candidate.Identifier)
	if !withinCleanupRoot(cfg.DiagnosticsRoot, path) {
		return errors.New("cleanup path escaped diagnostics root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("cleanup candidate is not a private regular file")
	}
	return os.Remove(path)
}
func withinCleanupRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path != root && strings.HasPrefix(path, root+string(os.PathSeparator))
}
