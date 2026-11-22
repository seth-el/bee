package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthReportChecksPrivateOpsRoot(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{}
	for _, name := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	job := filepath.Join(paths["state"], "jobs.json")
	if err := os.WriteFile(job, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := OpsConfig{Version: 1, StateRoot: paths["state"], SessionRoot: paths["sessions"], MemoryRoot: paths["memory"], JobStore: job, ArtifactRoot: paths["artifacts"], DiagnosticsRoot: paths["diagnostics"], Health: OpsHealth{TimeoutMS: 1000}}
	report := CheckHealth(nil, cfg)
	if report.Status != CheckPass {
		data, _ := json.Marshal(report)
		t.Fatalf("report=%s", data)
	}
}
