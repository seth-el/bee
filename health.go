package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

type HealthCheck struct {
	Name       string      `json:"name"`
	Status     CheckStatus `json:"status"`
	Code       string      `json:"code,omitempty"`
	Detail     string      `json:"detail,omitempty"`
	DurationMS int64       `json:"duration_ms"`
}
type HealthReport struct {
	Kind      string        `json:"kind"`
	Status    CheckStatus   `json:"status"`
	CheckedAt time.Time     `json:"checked_at"`
	Checks    []HealthCheck `json:"checks"`
}
type ReadinessOptions struct {
	CheckProvider bool
	CheckChannel  bool
	Timeout       time.Duration
	ProviderCheck func(context.Context) error
	ChannelCheck  func(context.Context) error
}

func CheckHealth(ctx context.Context, cfg OpsConfig) HealthReport {
	return checkOperations(ctx, cfg, "health", ReadinessOptions{})
}
func CheckReadiness(ctx context.Context, cfg OpsConfig, options ReadinessOptions) HealthReport {
	return checkOperations(ctx, cfg, "readiness", options)
}
func checkOperations(ctx context.Context, cfg OpsConfig, kind string, options ReadinessOptions) HealthReport {
	report := HealthReport{Kind: kind, CheckedAt: time.Now().UTC(), Checks: []HealthCheck{}}
	add := func(name string, fn func() error) {
		start := time.Now()
		err := fn()
		check := HealthCheck{Name: name, DurationMS: time.Since(start).Milliseconds()}
		if err != nil {
			check.Status = CheckFail
			check.Code = healthCode(err)
			check.Detail = err.Error()
		} else {
			check.Status = CheckPass
		}
		report.Checks = append(report.Checks, check)
	}
	add("ops_config", cfg.Validate)
	for _, item := range []struct {
		name, path string
		dir        bool
	}{{"state_root", cfg.StateRoot, true}, {"session_root", cfg.SessionRoot, true}, {"memory_root", cfg.MemoryRoot, true}, {"artifact_root", cfg.ArtifactRoot, true}, {"diagnostics_root", cfg.DiagnosticsRoot, true}, {"job_store", cfg.JobStore, false}} {
		item := item
		add(item.name, func() error { return checkPath(item.path, item.dir) })
	}
	if kind == "readiness" && options.CheckProvider {
		add("provider", func() error { return runBoundedCheck(ctx, options.Timeout, options.ProviderCheck) })
	}
	if kind == "readiness" && options.CheckChannel {
		add("channel", func() error { return runBoundedCheck(ctx, options.Timeout, options.ChannelCheck) })
	}
	report.Status = CheckPass
	for _, check := range report.Checks {
		if check.Status == CheckFail {
			report.Status = CheckFail
		}
	}
	return report
}
func runBoundedCheck(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	if fn == nil {
		return errors.New("check is not configured")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func checkPath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("path is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	if dir {
		if !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("directory is not private 0700")
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("file is not private 0600")
	}
	return nil
}
func healthCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "check_timeout"
	}
	if strings.Contains(err.Error(), "version") {
		return "ops_config_unsupported_version"
	}
	if strings.Contains(err.Error(), "path") {
		return "path_unavailable"
	}
	return "check_failed"
}
func (report HealthReport) MarshalJSON() ([]byte, error) {
	type alias HealthReport
	return json.Marshal(alias(report))
}
func (report HealthReport) Human() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", report.Kind, report.Status)
	for _, check := range report.Checks {
		fmt.Fprintf(&b, "- %-18s %s", check.Name, check.Status)
		if check.Code != "" {
			fmt.Fprintf(&b, " (%s)", check.Code)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func validateHealthOutput(path string) error {
	if filepath.IsAbs(path) && strings.Contains(strings.ToLower(path), ".openclaw") {
		return errors.New("health output must not use OpenClaw path")
	}
	return nil
}
