package bee

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationsDryRunAndCurrentNoOp(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		os.Mkdir(filepath.Join(root, d), 0700)
	}
	job := filepath.Join(root, "state", "jobs.json")
	os.WriteFile(job, []byte("{}"), 0600)
	cfg := OpsConfig{Version: 1, StateRoot: filepath.Join(root, "state"), SessionRoot: filepath.Join(root, "sessions"), MemoryRoot: filepath.Join(root, "memory"), JobStore: job, ArtifactRoot: filepath.Join(root, "artifacts"), DiagnosticsRoot: filepath.Join(root, "diagnostics"), Health: OpsHealth{TimeoutMS: 1000}}
	manifest := filepath.Join(root, "state", "release.json")
	migration := Migration{ID: "jobs-v1-v2", Domain: "jobs", FromVersion: 2, ToVersion: 3}
	r, err := PlanMigrations(context.Background(), cfg, manifest, []Migration{migration})
	if err != nil || len(r.Required) != 0 || r.Applied {
		t.Fatalf("report=%#v err=%v", r, err)
	}
	_ = time.Now()
}
