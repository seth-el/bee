package bee

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMemoryAutomationMergesSafeDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"memory":{"automation":{"enabled":true,"provider_role":"child","provider_profile":"local-child-8082"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	automation := config.Memory.Automation
	if !automation.Enabled || automation.AutoSaveSafe || automation.TimeoutSeconds != DefaultMemoryAutomationTimeoutSeconds || automation.ProviderRole != ProviderRoleChild || automation.ProviderProfile != "local-child-8082" {
		t.Fatalf("automation config = %#v", automation)
	}
}
