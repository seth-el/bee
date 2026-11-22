package bee

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ProviderRole identifies the only routing roles Bee supports. Routing is
// explicit: main runs use the main profile, bounded children use the child
// profile, and manual profiles are selected by the operator. There is no
// automatic escalation or fallback between roles.
type ProviderRole string

const (
	ProviderRoleMain   ProviderRole = "main"
	ProviderRoleChild  ProviderRole = "child"
	ProviderRoleManual ProviderRole = "manual"
)

const MaxBoundedChildInstances = 3

// ProviderProfile is a named OpenAI-compatible provider configuration. The
// profile name is safe metadata; credentials remain in the referenced secret
// file and are loaded only when that profile is selected.
type ProviderProfile struct {
	Name     string         `json:"name"`
	Role     ProviderRole   `json:"role"`
	Provider ProviderConfig `json:"provider"`
}

// ProviderRoutingConfig declares which profiles own the main and child roles.
// ManualProfiles is documentary/validation metadata; selecting a manual
// profile still requires an explicit operator --profile choice.
type ProviderRoutingConfig struct {
	MainProfile    string   `json:"main_profile,omitempty"`
	ChildProfile   string   `json:"child_profile,omitempty"`
	VoiceProfile   string   `json:"voice_profile,omitempty"`
	ManualProfiles []string `json:"manual_profiles,omitempty"`
	MaxChildren    int      `json:"max_children,omitempty"`
	// FallbackMainProfile answers interactive turns when the main profile's
	// server is unreachable before any reply (G10). Never used by jobs.
	FallbackMainProfile string `json:"fallback_main_profile,omitempty"`
}

// ProviderSelection is bounded, non-secret metadata about a chosen profile.
type ProviderSelection struct {
	Profile string       `json:"profile"`
	Role    ProviderRole `json:"role"`
	Model   string       `json:"model"`
	BaseURL string       `json:"base_url"`
}

func (profile ProviderProfile) Validate() error {
	if err := validateProfileName(profile.Name); err != nil {
		return err
	}
	if profile.Role != ProviderRoleMain && profile.Role != ProviderRoleChild && profile.Role != ProviderRoleManual {
		return fmt.Errorf("provider profile %q has invalid role %q", profile.Name, profile.Role)
	}
	if err := validateProviderConfig(profile.Provider); err != nil {
		return fmt.Errorf("provider profile %q: %w", profile.Name, err)
	}
	return nil
}

func (routing ProviderRoutingConfig) Validate(profiles []ProviderProfile) error {
	if routing.MaxChildren < 0 || routing.MaxChildren > MaxBoundedChildInstances {
		return fmt.Errorf("provider routing max_children must be between 0 and %d", MaxBoundedChildInstances)
	}
	byName := make(map[string]ProviderProfile, len(profiles))
	for _, profile := range profiles {
		if _, exists := byName[profile.Name]; exists {
			return fmt.Errorf("duplicate provider profile %q", profile.Name)
		}
		byName[profile.Name] = profile
	}
	if routing.MainProfile != "" {
		profile, ok := byName[routing.MainProfile]
		if !ok {
			return fmt.Errorf("provider routing main_profile %q does not exist", routing.MainProfile)
		}
		if profile.Role != ProviderRoleMain {
			return fmt.Errorf("provider routing main_profile %q must have role %q", routing.MainProfile, ProviderRoleMain)
		}
	}
	if routing.ChildProfile != "" {
		profile, ok := byName[routing.ChildProfile]
		if !ok {
			return fmt.Errorf("provider routing child_profile %q does not exist", routing.ChildProfile)
		}
		if profile.Role != ProviderRoleChild {
			return fmt.Errorf("provider routing child_profile %q must have role %q", routing.ChildProfile, ProviderRoleChild)
		}
	}
	if routing.FallbackMainProfile != "" {
		profile, ok := byName[routing.FallbackMainProfile]
		if !ok {
			return fmt.Errorf("provider routing fallback_main_profile %q does not exist", routing.FallbackMainProfile)
		}
		if routing.FallbackMainProfile == routing.MainProfile {
			return fmt.Errorf("provider routing fallback_main_profile %q must differ from main_profile", routing.FallbackMainProfile)
		}
		if profile.Role == ProviderRoleManual {
			return fmt.Errorf("provider routing fallback_main_profile %q must not be a manual profile", routing.FallbackMainProfile)
		}
	}
	if routing.VoiceProfile != "" {
		profile, ok := byName[routing.VoiceProfile]
		if !ok {
			return fmt.Errorf("provider routing voice_profile %q does not exist", routing.VoiceProfile)
		}
		if profile.Role == ProviderRoleChild {
			return fmt.Errorf("provider routing voice_profile %q must not have role %q", routing.VoiceProfile, ProviderRoleChild)
		}
	}
	seenManual := make(map[string]struct{}, len(routing.ManualProfiles))
	for _, name := range routing.ManualProfiles {
		if _, ok := seenManual[name]; !ok {
			profile, exists := byName[name]
			if !exists {
				return fmt.Errorf("provider routing manual profile %q does not exist", name)
			}
			if profile.Role != ProviderRoleManual {
				return fmt.Errorf("provider routing manual profile %q must have role %q", name, ProviderRoleManual)
			}
			seenManual[name] = struct{}{}
		}
	}
	return nil
}

func validateProfileName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider profile name is required")
	}
	if len(name) > 64 {
		return errors.New("provider profile name exceeds 64 bytes")
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == ':' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return fmt.Errorf("provider profile name %q contains an unsafe character", name)
		}
	}
	return nil
}

func validateProviderConfig(provider ProviderConfig) error {
	if provider.Type != DefaultProviderType {
		return fmt.Errorf("unsupported provider type %q", provider.Type)
	}
	if strings.TrimSpace(provider.BaseURL) == "" {
		return errors.New("provider base_url is required")
	}
	parsed, err := url.Parse(provider.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("provider base_url must be an absolute http(s) URL, got %q", provider.BaseURL)
	}
	if strings.TrimSpace(provider.Model) == "" {
		return errors.New("provider model is required")
	}
	if err := ValidateModelName(provider.Model); err != nil {
		return fmt.Errorf("provider model: %w", err)
	}
	if provider.TimeoutSeconds < 0 {
		return errors.New("provider timeout_seconds must not be negative")
	}
	if provider.ContextWindowTokens < 0 || provider.ContextWindowTokens > MaxContextWindowTokens {
		return fmt.Errorf("provider context_window_tokens must be between 0 and %d", MaxContextWindowTokens)
	}
	return nil
}

func defaultProviderProfiles() []ProviderProfile {
	return []ProviderProfile{
		{Name: "local-main-8081", Role: ProviderRoleMain, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:8081", Model: "local", TimeoutSeconds: DefaultTimeout, ImageInput: true}},
		{Name: "local-child-8082", Role: ProviderRoleChild, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:8082", Model: "local", TimeoutSeconds: DefaultTimeout}},
	}
}

func defaultProviderRouting() ProviderRoutingConfig {
	return ProviderRoutingConfig{MainProfile: "local-main-8081", ChildProfile: "local-child-8082", VoiceProfile: "local-main-8081", MaxChildren: MaxBoundedChildInstances}
}

// ResolveProviderProfile chooses a profile for the main application. An empty
// name uses the configured main profile, or the legacy provider field when no
// profile document is present. Child profiles cannot be manually selected as
// the main provider.
func (config Config) ResolveProviderProfile(name string) (ProviderProfile, ProviderSelection, error) {
	if len(config.Profiles) == 0 {
		if strings.TrimSpace(name) != "" {
			return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider profile %q is not configured", name)
		}
		return ProviderProfile{Name: "legacy-main", Role: ProviderRoleMain, Provider: config.Provider}, ProviderSelection{Profile: "legacy-main", Role: ProviderRoleMain, Model: config.Provider.Model, BaseURL: config.Provider.BaseURL}, nil
	}
	if name == "" {
		name = config.Routing.MainProfile
	}
	if name == "" {
		return ProviderProfile{}, ProviderSelection{}, errors.New("provider profile is required when profiles are configured")
	}
	for _, profile := range config.Profiles {
		if profile.Name != name {
			continue
		}
		if profile.Role == ProviderRoleChild {
			return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("child provider profile %q is not a manually selectable main profile", name)
		}
		return profile, ProviderSelection{Profile: profile.Name, Role: profile.Role, Model: profile.Provider.Model, BaseURL: profile.Provider.BaseURL}, nil
	}
	return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider profile %q is not configured", name)
}

func (config Config) resolveChildProfile() (ProviderProfile, ProviderSelection, error) {
	if len(config.Profiles) == 0 || config.Routing.ChildProfile == "" {
		return ProviderProfile{Name: "legacy-child", Role: ProviderRoleChild, Provider: config.Provider}, ProviderSelection{Profile: "legacy-child", Role: ProviderRoleChild, Model: config.Provider.Model, BaseURL: config.Provider.BaseURL}, nil
	}
	for _, profile := range config.Profiles {
		if profile.Name == config.Routing.ChildProfile {
			return profile, ProviderSelection{Profile: profile.Name, Role: profile.Role, Model: profile.Provider.Model, BaseURL: profile.Provider.BaseURL}, nil
		}
	}
	return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider child profile %q is not configured", config.Routing.ChildProfile)
}

func (config Config) resolveVoiceProfile() (ProviderProfile, ProviderSelection, error) {
	if len(config.Profiles) == 0 {
		return ProviderProfile{Name: "legacy-main", Role: ProviderRoleMain, Provider: config.Provider}, ProviderSelection{Profile: "legacy-main", Role: ProviderRoleMain, Model: config.Provider.Model, BaseURL: config.Provider.BaseURL}, nil
	}
	profileName := config.Routing.VoiceProfile
	if profileName == "" {
		profileName = config.Routing.MainProfile
	}
	if profileName == "" {
		return ProviderProfile{Name: "legacy-main", Role: ProviderRoleMain, Provider: config.Provider}, ProviderSelection{Profile: "legacy-main", Role: ProviderRoleMain, Model: config.Provider.Model, BaseURL: config.Provider.BaseURL}, nil
	}
	for _, profile := range config.Profiles {
		if profile.Name == profileName {
			if profile.Role == ProviderRoleChild {
				return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider voice profile %q is a child profile", profile.Name)
			}
			return profile, ProviderSelection{Profile: profile.Name, Role: profile.Role, Model: profile.Provider.Model, BaseURL: profile.Provider.BaseURL}, nil
		}
	}
	return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider voice profile %q is not configured", profileName)
}

// NewProviderForProfile constructs the selected provider and returns the
// effective config that must be passed to an Application.
func NewProviderForProfile(config Config, profileName string) (Config, ProviderSelection, Provider, error) {
	profile, selection, err := config.ResolveProviderProfile(profileName)
	if err != nil {
		return Config{}, ProviderSelection{}, nil, err
	}
	effective := config
	effective.Provider = profile.Provider
	effective.ProviderProfile = selection.Profile
	effective.ProviderRole = selection.Role
	provider, err := NewProvider(effective.Provider)
	if err != nil {
		return Config{}, ProviderSelection{}, nil, err
	}
	return effective, selection, provider, nil
}

// NewApplicationForProfile composes main and child roles. The child provider
// is kept separate from the main provider and can only be reached through a
// bounded ChildRunner/ChildRunnerPool.
func NewApplicationForProfile(config Config, profileName string, store *SessionStore) (*Application, error) {
	var effective Config
	var mainSelection ProviderSelection
	var mainProvider Provider
	var err error
	// Preserve the programmatic pre-profile provider override used by direct
	// library callers. LoadConfig accepts top-level provider only for legacy
	// single-provider documents; profile-based files use routing.main_profile as
	// their one authoritative main-provider source.
	mainProfile, _, _ := config.ResolveProviderProfile("")
	if profileName == "" && config.providerExplicit && len(config.Profiles) > 0 && !sameProviderConfig(config.Provider, mainProfile.Provider) {
		effective = config
		effective.ProviderProfile = "legacy-main-override"
		effective.ProviderRole = ProviderRoleMain
		mainSelection = ProviderSelection{Profile: effective.ProviderProfile, Role: ProviderRoleMain, Model: effective.Provider.Model, BaseURL: effective.Provider.BaseURL}
		mainProvider, err = NewProvider(effective.Provider)
	} else {
		effective, mainSelection, mainProvider, err = NewProviderForProfile(config, profileName)
	}
	if err != nil {
		return nil, err
	}
	app, err := NewApplication(effective, mainProvider, store)
	if err != nil {
		return nil, err
	}
	if profileName == "" && config.Routing.FallbackMainProfile != "" {
		for _, profile := range config.Profiles {
			if profile.Name != config.Routing.FallbackMainProfile {
				continue
			}
			fallbackConfig := profile.Provider
			app.Provider = &fallbackProvider{
				primary: mainProvider, primaryName: mainSelection.Profile,
				fallbackName: profile.Name, fallbackModel: fallbackConfig.Model,
				newFallback: func() (Provider, error) { return NewProvider(fallbackConfig) },
			}
			app.configureProviderAttachmentResolver()
		}
	}
	app.ProviderProfile = mainSelection.Profile
	app.ProviderRole = mainSelection.Role
	childProfile, childSelection, childProviderErr := config.resolveChildProfile()
	if childProviderErr != nil {
		return nil, childProviderErr
	}
	childConfig := config
	childConfig.Provider = childProfile.Provider
	childConfig.ProviderProfile = childSelection.Profile
	childConfig.ProviderRole = childSelection.Role
	// Keep child credentials lazy. Config/profile shape is already validated;
	// the API key file is not read until a child is actually started.
	app.ChildProvider = nil
	app.ChildConfig = childConfig
	if childProfile.Name == mainSelection.Profile && sameProviderConfig(childConfig.Provider, effective.Provider) {
		app.ChildProvider = mainProvider
		app.ChildConfig = effective
	}
	app.ChildProviderProfile = childSelection.Profile
	voiceProfile, voiceSelection, voiceProviderErr := config.resolveVoiceProfile()
	if voiceProviderErr != nil {
		return nil, voiceProviderErr
	}
	voiceConfig := config
	voiceConfig.Provider = voiceProfile.Provider
	voiceConfig.ProviderProfile = voiceSelection.Profile
	voiceConfig.ProviderRole = voiceSelection.Role
	app.VoiceProvider = nil
	app.VoiceConfig = voiceConfig
	app.VoiceProviderProfile = voiceSelection.Profile
	if voiceProfile.Name == mainSelection.Profile && sameProviderConfig(voiceConfig.Provider, effective.Provider) {
		app.VoiceProvider = mainProvider
		app.VoiceConfig = effective
	}
	if config.Web.Enabled {
		webExecutor, webErr := NewWebResearchExecutor(config.Web)
		if webErr != nil {
			return nil, webErr
		}
		app.WebResearch = webExecutor
	}
	app.ChildMax = config.Routing.MaxChildren
	if app.ChildMax == 0 {
		app.ChildMax = MaxBoundedChildInstances
	}
	return app, nil
}

func sameProviderConfig(left, right ProviderConfig) bool {
	return left.Type == right.Type && left.BaseURL == right.BaseURL && left.Model == right.Model && left.APIKeyFile == right.APIKeyFile && left.TimeoutSeconds == right.TimeoutSeconds && left.ImageInput == right.ImageInput && left.Local == right.Local && left.ContextWindowTokens == right.ContextWindowTokens
}

func localMultimodalProviderURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return (host == "127.0.0.1" || host == "localhost" || host == "::1") && parsed.Port() == "8081"
}

func sortedProfileNames(profiles []ProviderProfile) []string {
	result := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		result = append(result, profile.Name)
	}
	sort.Strings(result)
	return result
}
