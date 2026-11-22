package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validOpsConfig(t *testing.T) (OpsConfig, string) {
	t.Helper()
	root := t.TempDir()
	paths := []*string{}
	for _, name := range []string{"state", "sessions", "memory", "artifacts", "diagnostics"} {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, &p)
	}
	jobStore := filepath.Join(*paths[0], "jobs.json")
	if err := os.WriteFile(jobStore, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := OpsConfig{Version: 1, StateRoot: *paths[0], SessionRoot: *paths[1], MemoryRoot: *paths[2], JobStore: jobStore, ArtifactRoot: *paths[3], DiagnosticsRoot: *paths[4], Retention: OpsRetention{DiagnosticsDays: 30}, Health: OpsHealth{TimeoutMS: 1000}}
	return cfg, filepath.Join(root, "ops.json")
}
func TestOpsConfigStrictPrivateValidation(t *testing.T) {
	cfg, path := validOpsConfig(t)
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOpsConfig(path)
	if err != nil || loaded.Version != 1 {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	raw := strings.Replace(string(data), "\"version\":1", "\"version\":1,\"unknown\":true", 1)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOpsConfig(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}
func TestOpsConfigRejectsUnsupportedRetentionDomains(t *testing.T) {
	cfg, path := validOpsConfig(t)
	for _, set := range []func(*OpsRetention){
		func(retention *OpsRetention) { retention.SessionsDays = 1 },
		func(retention *OpsRetention) { retention.ArtifactsDays = 1 },
		func(retention *OpsRetention) { retention.JobsDays = 1 },
	} {
		cfg.Retention = OpsRetention{}
		set(&cfg.Retention)
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOpsConfig(path); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("retention=%#v err=%v", cfg.Retention, err)
		}
	}
}

func TestOpsConfigRejectsUnsafeVersionPermissionAndOpenClaw(t *testing.T) {
	cfg, path := validOpsConfig(t)
	cfg.Version = 2
	data, _ := json.Marshal(cfg)
	os.WriteFile(path, data, 0600)
	if _, err := LoadOpsConfig(path); err == nil {
		t.Fatal("unsupported version accepted")
	}
	cfg.Version = 1
	cfg.StateRoot = "/tmp/.openclaw/state"
	data, _ = json.Marshal(cfg)
	os.WriteFile(path, data, 0600)
	if _, err := LoadOpsConfig(path); err == nil {
		t.Fatal("OpenClaw path accepted")
	}
	cfg.StateRoot = filepath.Dir(path)
	data, _ = json.Marshal(cfg)
	os.WriteFile(path, data, 0644)
	if _, err := LoadOpsConfig(path); err == nil {
		t.Fatal("unsafe config permissions accepted")
	}
}
