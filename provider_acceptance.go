package bee

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultProviderAcceptanceTimeout = 15 * time.Second
	MaxProviderAcceptanceTimeout     = 60 * time.Second
	ProviderAcceptanceMaxTokens      = 128
)

// ProviderAcceptanceOptions bounds one explicit endpoint probe. Cloud/manual
// profiles are never contacted unless AllowCloudProbe is true.
type ProviderAcceptanceOptions struct {
	AllowCloudProbe bool
	Timeout         time.Duration
	Nonce           string
}

// ProviderAcceptanceResult is a redacted, bounded result packet. It contains
// routing metadata and outcome classes, never prompts, response text, API
// keys, authorization headers, or raw provider errors.
type ProviderAcceptanceResult struct {
	Profile           string       `json:"profile"`
	Role              ProviderRole `json:"role"`
	Model             string       `json:"model"`
	EndpointHost      string       `json:"endpoint_host"`
	ResponseModel     string       `json:"response_model,omitempty"`
	Attempted         bool         `json:"attempted"`
	ResponseReceived  bool         `json:"response_received"`
	SkippedCloudProbe bool         `json:"skipped_cloud_probe,omitempty"`
	FallbackAttempts  int          `json:"fallback_attempts"`
	ErrorClass        string       `json:"error_class,omitempty"`
	DurationMS        int64        `json:"duration_ms"`
}

// ProviderAcceptanceReport is safe to persist under generated/ or show to an
// operator. A skipped manual profile is not a failed local endpoint probe.
type ProviderAcceptanceReport struct {
	Acceptance       string                     `json:"acceptance"`
	StartedAt        time.Time                  `json:"started_at"`
	CompletedAt      time.Time                  `json:"completed_at"`
	Passed           bool                       `json:"passed"`
	FallbackAttempts int                        `json:"fallback_attempts"`
	Results          []ProviderAcceptanceResult `json:"results"`
	SkippedProfiles  []string                   `json:"skipped_profiles,omitempty"`
}

// ConfiguredProviderProfileNames returns deterministic profile names. Legacy
// configs expose one synthetic main profile and have no child/cloud probe.
func ConfiguredProviderProfileNames(config Config) []string {
	if len(config.Profiles) == 0 {
		return []string{"legacy-main"}
	}
	names := make([]string, 0, len(config.Profiles))
	for _, profile := range config.Profiles {
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	return names
}

func resolveAcceptanceProfile(config Config, name string) (ProviderProfile, ProviderSelection, error) {
	if len(config.Profiles) == 0 {
		if name == "" || name == "legacy-main" {
			return ProviderProfile{Name: "legacy-main", Role: ProviderRoleMain, Provider: config.Provider}, ProviderSelection{Profile: "legacy-main", Role: ProviderRoleMain, Model: config.Provider.Model, BaseURL: config.Provider.BaseURL}, nil
		}
		return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider profile %q is not configured", name)
	}
	if name == "" {
		name = config.Routing.MainProfile
	}
	for _, profile := range config.Profiles {
		if profile.Name == name {
			return profile, ProviderSelection{Profile: profile.Name, Role: profile.Role, Model: profile.Provider.Model, BaseURL: profile.Provider.BaseURL}, nil
		}
	}
	return ProviderProfile{}, ProviderSelection{}, fmt.Errorf("provider profile %q is not configured", name)
}

// ProbeProviderProfile performs exactly one bounded OpenAI-compatible request
// to the selected profile. It has no fallback path and no session/memory
// integration. Manual profiles require explicit cloud opt-in.
func ProbeProviderProfile(ctx context.Context, config Config, profileName string, options ProviderAcceptanceOptions) ProviderAcceptanceResult {
	profile, selection, err := resolveAcceptanceProfile(config, profileName)
	result := ProviderAcceptanceResult{Profile: profileName, FallbackAttempts: 0}
	if err != nil {
		result.ErrorClass = "profile_not_configured"
		return result
	}
	result.Profile = selection.Profile
	result.Role = selection.Role
	result.Model = selection.Model
	result.EndpointHost = acceptanceEndpointHost(selection.BaseURL)
	if selection.Role == ProviderRoleManual && !options.AllowCloudProbe {
		result.SkippedCloudProbe = true
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultProviderAcceptanceTimeout
	}
	if timeout > MaxProviderAcceptanceTimeout {
		timeout = MaxProviderAcceptanceTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nonce := options.Nonce
	if strings.TrimSpace(nonce) == "" {
		nonce = "bee-provider-acceptance"
	}
	if err := validateAcceptanceNonce(nonce); err != nil {
		result.ErrorClass = "invalid_nonce"
		return result
	}
	started := time.Now()
	result.Attempted = true
	provider, err := NewProvider(profile.Provider)
	if err == nil {
		maxTokens := config.Generation.MaxTokens
		if maxTokens <= 0 || maxTokens > ProviderAcceptanceMaxTokens {
			maxTokens = ProviderAcceptanceMaxTokens
		}
		request := Request{Model: profile.Provider.Model, Messages: []Message{{Role: RoleUser, Content: "Bee provider acceptance nonce: " + nonce}}, Temperature: config.Generation.Temperature, MaxTokens: maxTokens}
		response, callErr := provider.Stream(probeCtx, request, nil)
		if callErr != nil {
			err = callErr
		} else {
			result.ResponseReceived = true
			result.ResponseModel = safeProviderModel(response.Model)
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.ErrorClass = providerAcceptanceErrorClass(err, probeCtx.Err())
	}
	return result
}

// RunProviderAcceptance probes the requested profiles. When profileNames is
// empty, it probes the main and child roles and reports manual profiles as
// skipped unless cloud opt-in is supplied.
func RunProviderAcceptance(ctx context.Context, config Config, profileNames []string, options ProviderAcceptanceOptions) ProviderAcceptanceReport {
	started := time.Now().UTC()
	report := ProviderAcceptanceReport{Acceptance: "provider-endpoint", StartedAt: started, Passed: true}
	if len(profileNames) == 0 {
		for _, name := range ConfiguredProviderProfileNames(config) {
			profile, _, err := resolveAcceptanceProfile(config, name)
			if err == nil && profile.Role == ProviderRoleManual && !options.AllowCloudProbe {
				report.SkippedProfiles = append(report.SkippedProfiles, name)
				continue
			}
			profileNames = append(profileNames, name)
		}
	}
	for _, name := range profileNames {
		result := ProbeProviderProfile(ctx, config, name, options)
		report.Results = append(report.Results, result)
		report.FallbackAttempts += result.FallbackAttempts
		if !result.Attempted || !result.ResponseReceived || result.ErrorClass != "" {
			report.Passed = false
		}
	}
	if len(report.Results) == 0 {
		report.Passed = false
	}
	report.CompletedAt = time.Now().UTC()
	return report
}

func safeProviderModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	// Provider responses may echo an absolute local model path. Keep only a
	// bounded basename in acceptance evidence; never persist host paths.
	if strings.Contains(model, "/") || strings.Contains(model, "\\") {
		model = filepath.Base(model)
	}
	if len(model) > 128 {
		model = model[:128]
	}
	return model
}

func acceptanceEndpointHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

func validateAcceptanceNonce(nonce string) error {
	if len(nonce) > 128 {
		return errors.New("provider acceptance nonce exceeds 128 bytes")
	}
	if strings.TrimSpace(nonce) == "" {
		return errors.New("provider acceptance nonce is required")
	}
	for _, r := range nonce {
		if r < 0x20 || r == 0x7f {
			return errors.New("provider acceptance nonce contains control characters")
		}
	}
	return nil
}

func providerAcceptanceErrorClass(err, ctxErr error) string {
	if errors.Is(ctxErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(ctxErr, context.Canceled) || errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "http ") || strings.Contains(message, "provider returned") {
		return "http_error"
	}
	if strings.Contains(message, "decode") || strings.Contains(message, "malformed") || strings.Contains(message, "choice") {
		return "invalid_response"
	}
	return "provider_error"
}
