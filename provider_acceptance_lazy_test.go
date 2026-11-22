package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildProviderAPIKeyIsLazy(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "missing-child-key")
	config := DefaultConfig()
	for index := range config.Profiles {
		if config.Profiles[index].Name == "local-child-8082" {
			config.Profiles[index].Provider.APIKeyFile = keyPath
		}
	}
	app, err := NewApplicationForProfile(config, "local-main-8081", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("test key unexpectedly exists")
	}
	// Main composition succeeds without touching the child key. The child
	// provider is only loaded when a child execution requests it.
	if app.ChildProvider != nil {
		t.Fatal("child provider was eagerly instantiated")
	}
	if _, err := app.childProvider(); err == nil || !strings.Contains(err.Error(), "missing-child-key") {
		t.Fatalf("lazy child key error = %v", err)
	}
}
