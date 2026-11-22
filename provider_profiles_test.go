package bee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigUsesNamedProviderRoles(t *testing.T) {
	config := DefaultConfig()
	if config.Provider.BaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("default main URL = %q", config.Provider.BaseURL)
	}
	if config.Routing.MainProfile != "local-main-8081" || config.Routing.ChildProfile != "local-child-8082" || config.Routing.VoiceProfile != "local-main-8081" {
		t.Fatalf("routing = %#v", config.Routing)
	}
	if len(config.Profiles) != 2 {
		t.Fatalf("profiles = %#v", config.Profiles)
	}
	if len(config.Routing.ManualProfiles) != 0 {
		t.Fatalf("default manual profiles = %#v, want none", config.Routing.ManualProfiles)
	}
}

func TestProgrammaticDefaultConfigRetainsLegacyProviderOverride(t *testing.T) {
	config := DefaultConfig()
	config.Provider.BaseURL = "http://127.0.0.1:18081"
	config.Provider.Model = "programmatic-main"
	app, err := NewApplicationForProfile(config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.ProviderProfile != "legacy-main-override" || app.Config.Provider.BaseURL != config.Provider.BaseURL {
		t.Fatalf("programmatic provider override profile=%q provider=%#v", app.ProviderProfile, app.Config.Provider)
	}
}

func TestVoiceProviderProfileIsDedicatedFromChildProfile(t *testing.T) {
	config := DefaultConfig()
	config.Profiles = append(config.Profiles, ProviderProfile{Name: "voice-test", Role: ProviderRoleMain, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:18081", Model: "gemma-31b", TimeoutSeconds: 1}})
	config.Routing.VoiceProfile = "voice-test"
	app, err := NewApplicationForProfile(config, "local-main-8081", nil)
	if err != nil {
		t.Fatal(err)
	}
	if app.ChildProvider != nil {
		t.Fatal("child provider should remain lazy")
	}
	if app.ChildProviderProfile != "local-child-8082" || app.VoiceProviderProfile != "voice-test" {
		t.Fatalf("profiles child=%q voice=%q", app.ChildProviderProfile, app.VoiceProviderProfile)
	}
	if got := app.ChildConfigOrMain().Provider.BaseURL; got != "http://127.0.0.1:8082" {
		t.Fatalf("child endpoint = %q", got)
	}
	if got := app.VoiceConfigOrMain().Provider.BaseURL; got != "http://127.0.0.1:18081" {
		t.Fatalf("voice endpoint = %q", got)
	}
}

func TestVoiceProfileRejectsChildRole(t *testing.T) {
	config := DefaultConfig()
	config.Routing.VoiceProfile = "local-child-8082"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "voice_profile") {
		t.Fatalf("voice child profile validation error = %v", err)
	}
}

func TestLegacyConfigRemainsSingleProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider":{"base_url":"http://127.0.0.1:9999","model":"test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Profiles) != 0 || config.Provider.BaseURL != "http://127.0.0.1:9999" {
		t.Fatalf("legacy config = %#v", config)
	}
}

func TestProviderProfileSelectionIsExplicitAndMetadataIsVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"model\":\"cloud-model\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	config := DefaultConfig()
	config.Profiles = append(config.Profiles, ProviderProfile{
		Name:     "manual-test",
		Role:     ProviderRoleManual,
		Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "manual-model"},
	})
	config.Routing.ManualProfiles = []string{"manual-test"}
	app, err := NewApplicationForProfile(config, "manual-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var sawStart bool
	for event := range events {
		if event.Type == RunStart {
			sawStart = true
			if event.Profile != "manual-test" || event.Role != ProviderRoleManual || event.Model != "manual-model" {
				t.Fatalf("start metadata = %#v", event)
			}
		}
	}
	if !sawStart {
		t.Fatal("missing run_start")
	}
}

func TestChildPoolHonorsBoundedProfileCapacity(t *testing.T) {
	app := &Application{Provider: &fakeProvider{}, ChildMax: 3}
	if _, err := NewChildRunnerPool(app, 4); err == nil || !strings.Contains(err.Error(), "maximum 3") {
		t.Fatalf("capacity error = %v", err)
	}
}

func TestChildRunnerUsesDedicatedChildProvider(t *testing.T) {
	mainProvider := &childTestProvider{}
	childProvider := &childTestProvider{}
	config := DefaultConfig()
	config.Provider.Model = "main"
	app, err := NewApplication(config, mainProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.ChildProvider = childProvider
	app.ChildConfig = config
	app.ChildConfig.Provider.Model = "child"
	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), childTask(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if len(mainProvider.observed) != 0 || len(childProvider.observed) != 1 || childProvider.observed[0].Model != "child" {
		t.Fatalf("main=%#v child=%#v", mainProvider.observed, childProvider.observed)
	}
}

func TestAPIKeyFileMustBePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (ProviderConfig{APIKeyFile: path}).APIKey(); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("key permissions error = %v", err)
	}
}

func TestProviderSelectionMetadataDoesNotContainKey(t *testing.T) {
	selection := ProviderSelection{Profile: "cloud", Role: ProviderRoleManual, Model: "model", BaseURL: "https://example.invalid"}
	data, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("provider selection leaked secret-shaped data")
	}
}
