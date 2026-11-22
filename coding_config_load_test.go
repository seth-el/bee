package bee

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigPreservesCodingDefaultsWhenBackendBlockOmitsRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"coding":{"enabled":true,"backends":[{"name":"local-child","kind":"local_child","enabled":true}]}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Coding.Enabled || config.Coding.DefaultImplementer != CodingBackendNameLocalChild || config.Coding.DefaultReviewer != CodingBackendNameLocalChild || config.Coding.DefaultScout != CodingBackendNameLocalChild {
		t.Fatalf("coding config = %#v", config.Coding)
	}
}
