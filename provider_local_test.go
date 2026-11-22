package bee

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLANProfilesConfig(t *testing.T, local bool) Config {
	t.Helper()
	localField := ""
	if local {
		localField = `, "local": true`
	}
	data := `{
  "profiles": [
    {"name": "lan-main", "role": "main", "provider": {"type": "openai-compatible", "base_url": "http://203.0.113.37:8081", "model": "glm"` + localField + `}},
    {"name": "lan-child", "role": "child", "provider": {"type": "openai-compatible", "base_url": "http://203.0.113.37:8083", "model": "gemma"` + localField + `}}
  ],
  "routing": {"main_profile": "lan-main", "child_profile": "lan-child", "voice_profile": "lan-main"}
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestLANProviderIsNotLocalUnlessDeclared(t *testing.T) {
	config := writeLANProfilesConfig(t, false)
	if _, err := NewSpecialistService(config); err == nil {
		t.Fatal("specialist accepted an undeclared LAN endpoint")
	}
	for _, profile := range config.Profiles {
		if providerIsLocal(profile.Provider) {
			t.Fatalf("profile %s counted as local without the flag", profile.Name)
		}
	}
}

func TestDeclaredLocalLANProviderEnablesSpecialist(t *testing.T) {
	config := writeLANProfilesConfig(t, true)
	for _, profile := range config.Profiles {
		if !profile.Provider.Local || !providerIsLocal(profile.Provider) {
			t.Fatalf("profile %s did not load as local: %#v", profile.Name, profile.Provider)
		}
	}
	service, err := NewSpecialistService(config)
	if err != nil {
		t.Fatal(err)
	}
	if service == nil || !service.isConfiguredLocalChild() {
		t.Fatalf("specialist is not a configured local child: %#v", service)
	}
	if service.Profile() != "lan-child" {
		t.Fatalf("specialist profile = %q", service.Profile())
	}
}

func TestLoopbackProviderStaysLocalWithoutFlag(t *testing.T) {
	if !providerIsLocal(ProviderConfig{BaseURL: "http://127.0.0.1:8083"}) {
		t.Fatal("loopback endpoint is no longer local")
	}
	if providerIsLocal(ProviderConfig{BaseURL: "https://api.example.com/v1"}) {
		t.Fatal("remote endpoint counted as local")
	}
}
