package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSpecialistTimeoutSeconds            = 3
	DefaultSpecialistMaxInputBytes             = 16 << 10
	DefaultSpecialistMaxOutputBytes            = 8 << 10
	DefaultSpecialistMaxTokens                 = 1024
	DefaultSpecialistMaxConcurrency            = 2
	DefaultSpecialistObservationThreshold      = 8 << 10
	DefaultSpecialistObservationMaxBytes       = 4 << 10
	DefaultSpecialistCompressionTimeoutSeconds = 10
	DefaultSpecialistFailureThreshold          = 3
	DefaultSpecialistFailureWindowSeconds      = 60
	DefaultSpecialistDigestMaxBytes            = 1024
	DefaultSpecialistImageMaxBytes             = 104857600
	DefaultSpecialistImageMaxDimension         = 1536
	DefaultSpecialistImageMaxPixels            = 1_500_000
	DefaultSpecialistTranscriptMaxBytes        = 16 << 10
	MaxSpecialistTimeoutSeconds                = 30
	MaxSpecialistCompressionTimeoutSeconds     = 60
	MaxSpecialistInputBytes                    = 64 << 10
	MaxSpecialistOutputBytes                   = 64 << 10
	MaxSpecialistTokens                        = 4096
	MaxSpecialistConcurrency                   = 8
	MaxSpecialistObservationMaxBytes           = 32 << 10
	MaxSpecialistDigestMaxBytes                = 8 << 10
	MaxSpecialistImageMaxBytes                 = 104857600
	MaxSpecialistImageMaxDimension             = 1536
	MaxSpecialistImageMaxPixels                = 4_000_000
	MaxSpecialistTranscriptMaxBytes            = 64 << 10
	DefaultSpecialistEvidenceMaxInputBytes     = 16 << 10
	DefaultSpecialistEvidenceMaxOutputBytes    = 8 << 10
	DefaultSpecialistEvidenceMaxItems          = 16
	DefaultSpecialistEvidenceTimeoutSeconds    = 10
	MaxSpecialistEvidenceMaxInputBytes         = 64 << 10
	MaxSpecialistEvidenceMaxOutputBytes        = 32 << 10
	MaxSpecialistEvidenceMaxItems              = 64
	MaxSpecialistEvidenceTimeoutSeconds        = 60
	MaxSpecialistFailureStates                 = 256
)

// SpecialistConfig controls optional parent-owned helper calls. An empty
// Profile resolves to the configured child role (or the legacy local provider).
type SpecialistConfig struct {
	Enabled                              bool   `json:"enabled"`
	Profile                              string `json:"profile,omitempty"`
	ImageSidecarEnabled                  bool   `json:"image_sidecar_enabled"`
	TimeoutSeconds                       int    `json:"timeout_seconds"`
	MaxInputBytes                        int    `json:"max_input_bytes"`
	MaxOutputBytes                       int    `json:"max_output_bytes"`
	MaxTokens                            int    `json:"max_tokens"`
	MaxConcurrency                       int    `json:"max_concurrency"`
	ObservationThresholdBytes            int    `json:"observation_threshold_bytes"`
	ObservationMaxBytes                  int    `json:"observation_max_bytes"`
	ObservationCompressionTimeoutSeconds int    `json:"observation_compression_timeout_seconds"`
	FailureThreshold                     int    `json:"failure_threshold"`
	FailureWindowSeconds                 int    `json:"failure_window_seconds"`
	DigestMaxBytes                       int    `json:"digest_max_bytes"`
	SessionDigestEnabled                 bool   `json:"session_digest_enabled"`
	ImageMaxBytes                        int    `json:"image_max_bytes"`
	ImageMaxDimension                    int    `json:"image_max_dimension"`
	ImageMaxPixels                       int    `json:"image_max_pixels"`
	TranscriptMaxBytes                   int    `json:"transcript_max_bytes"`
	EvidenceEnabled                      bool   `json:"evidence_enabled"`
	EvidenceMaxInputBytes                int    `json:"evidence_max_input_bytes"`
	EvidenceMaxOutputBytes               int    `json:"evidence_max_output_bytes"`
	EvidenceMaxItems                     int    `json:"evidence_max_items"`
	EvidenceTimeoutSeconds               int    `json:"evidence_timeout_seconds"`
	// ObservationCompression turns the deterministic trimming of large tool
	// observations in the provider-facing request on or off (default on).
	ObservationCompression *bool `json:"observation_compression,omitempty"`
	// ImageFallback lets the child describe image attachments when the main
	// provider has no image input (default on). It needs a child profile
	// with image input; image_sidecar_enabled is the separate forced path.
	ImageFallback *bool `json:"image_fallback,omitempty"`
}

// ObservationCompressionEnabled reports whether large tool observations are
// trimmed in the provider-facing request.
func (config SpecialistConfig) ObservationCompressionEnabled() bool {
	return config.ObservationCompression == nil || *config.ObservationCompression
}

// ImageFallbackEnabled reports whether the child describes images for a main
// provider that can't read them.
func (config SpecialistConfig) ImageFallbackEnabled() bool {
	return config.ImageFallback == nil || *config.ImageFallback
}

func DefaultSpecialistConfig() SpecialistConfig {
	return SpecialistConfig{Enabled: true, TimeoutSeconds: DefaultSpecialistTimeoutSeconds, MaxInputBytes: DefaultSpecialistMaxInputBytes, MaxOutputBytes: DefaultSpecialistMaxOutputBytes, MaxTokens: DefaultSpecialistMaxTokens, MaxConcurrency: DefaultSpecialistMaxConcurrency, ObservationThresholdBytes: DefaultSpecialistObservationThreshold, ObservationMaxBytes: DefaultSpecialistObservationMaxBytes, ObservationCompressionTimeoutSeconds: DefaultSpecialistCompressionTimeoutSeconds, FailureThreshold: DefaultSpecialistFailureThreshold, FailureWindowSeconds: DefaultSpecialistFailureWindowSeconds, DigestMaxBytes: DefaultSpecialistDigestMaxBytes, ImageMaxBytes: DefaultSpecialistImageMaxBytes, ImageMaxDimension: DefaultSpecialistImageMaxDimension, ImageMaxPixels: DefaultSpecialistImageMaxPixels, TranscriptMaxBytes: DefaultSpecialistTranscriptMaxBytes, EvidenceMaxInputBytes: DefaultSpecialistEvidenceMaxInputBytes, EvidenceMaxOutputBytes: DefaultSpecialistEvidenceMaxOutputBytes, EvidenceMaxItems: DefaultSpecialistEvidenceMaxItems, EvidenceTimeoutSeconds: DefaultSpecialistEvidenceTimeoutSeconds}
}

func (config SpecialistConfig) effective() SpecialistConfig {
	defaults := DefaultSpecialistConfig()
	if config.TimeoutSeconds <= 0 {
		config.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = defaults.MaxInputBytes
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = defaults.MaxOutputBytes
	}
	if config.MaxTokens <= 0 {
		config.MaxTokens = defaults.MaxTokens
	}
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = defaults.MaxConcurrency
	}
	if config.ObservationThresholdBytes <= 0 {
		config.ObservationThresholdBytes = defaults.ObservationThresholdBytes
	}
	if config.ObservationMaxBytes <= 0 {
		config.ObservationMaxBytes = defaults.ObservationMaxBytes
	}
	if config.ObservationCompressionTimeoutSeconds <= 0 {
		config.ObservationCompressionTimeoutSeconds = defaults.ObservationCompressionTimeoutSeconds
	}
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = defaults.FailureThreshold
	}
	if config.FailureWindowSeconds <= 0 {
		config.FailureWindowSeconds = defaults.FailureWindowSeconds
	}
	if config.DigestMaxBytes <= 0 {
		config.DigestMaxBytes = defaults.DigestMaxBytes
	}
	if config.ImageMaxBytes <= 0 {
		config.ImageMaxBytes = defaults.ImageMaxBytes
	}
	if config.ImageMaxDimension <= 0 {
		config.ImageMaxDimension = defaults.ImageMaxDimension
	}
	if config.ImageMaxPixels <= 0 {
		config.ImageMaxPixels = defaults.ImageMaxPixels
	}
	if config.TranscriptMaxBytes <= 0 {
		config.TranscriptMaxBytes = defaults.TranscriptMaxBytes
	}
	if config.EvidenceMaxInputBytes <= 0 {
		config.EvidenceMaxInputBytes = defaults.EvidenceMaxInputBytes
	}
	if config.EvidenceMaxOutputBytes <= 0 {
		config.EvidenceMaxOutputBytes = defaults.EvidenceMaxOutputBytes
	}
	if config.EvidenceMaxItems <= 0 {
		config.EvidenceMaxItems = defaults.EvidenceMaxItems
	}
	if config.EvidenceTimeoutSeconds <= 0 {
		config.EvidenceTimeoutSeconds = defaults.EvidenceTimeoutSeconds
	}
	return config
}

func (config SpecialistConfig) Validate() error {
	if config.TimeoutSeconds < 0 || config.TimeoutSeconds > MaxSpecialistTimeoutSeconds {
		return fmt.Errorf("specialist timeout_seconds must be between 0 and %d", MaxSpecialistTimeoutSeconds)
	}
	if config.MaxInputBytes < 0 || config.MaxInputBytes > MaxSpecialistInputBytes {
		return fmt.Errorf("specialist max_input_bytes must be between 0 and %d", MaxSpecialistInputBytes)
	}
	if config.MaxOutputBytes < 0 || config.MaxOutputBytes > MaxSpecialistOutputBytes {
		return fmt.Errorf("specialist max_output_bytes must be between 0 and %d", MaxSpecialistOutputBytes)
	}
	if config.MaxTokens < 0 || config.MaxTokens > MaxSpecialistTokens {
		return fmt.Errorf("specialist max_tokens must be between 0 and %d", MaxSpecialistTokens)
	}
	if config.MaxConcurrency < 0 || config.MaxConcurrency > MaxSpecialistConcurrency {
		return fmt.Errorf("specialist max_concurrency must be between 0 and %d", MaxSpecialistConcurrency)
	}
	if config.ObservationThresholdBytes < 0 || config.ObservationThresholdBytes > MaxSpecialistObservationMaxBytes {
		return fmt.Errorf("specialist observation_threshold_bytes is invalid")
	}
	if config.ObservationMaxBytes < 0 || config.ObservationMaxBytes > MaxSpecialistObservationMaxBytes {
		return fmt.Errorf("specialist observation_max_bytes is invalid")
	}
	if config.ObservationCompressionTimeoutSeconds < 0 || config.ObservationCompressionTimeoutSeconds > MaxSpecialistCompressionTimeoutSeconds {
		return fmt.Errorf("specialist observation_compression_timeout_seconds must be between 0 and %d", MaxSpecialistCompressionTimeoutSeconds)
	}
	if config.FailureThreshold < 0 || config.FailureThreshold > 32 {
		return fmt.Errorf("specialist failure_threshold is invalid")
	}
	if config.FailureWindowSeconds < 0 || config.FailureWindowSeconds > 3600 {
		return fmt.Errorf("specialist failure_window_seconds is invalid")
	}
	if config.DigestMaxBytes < 0 || config.DigestMaxBytes > MaxSpecialistDigestMaxBytes {
		return fmt.Errorf("specialist digest_max_bytes is invalid")
	}
	if config.ImageMaxBytes < 0 || config.ImageMaxBytes > MaxSpecialistImageMaxBytes {
		return fmt.Errorf("specialist image_max_bytes is invalid")
	}
	if config.ImageMaxDimension < 0 || config.ImageMaxDimension > MaxSpecialistImageMaxDimension {
		return fmt.Errorf("specialist image_max_dimension must be between 0 and %d", MaxSpecialistImageMaxDimension)
	}
	if config.ImageMaxPixels < 0 || config.ImageMaxPixels > MaxSpecialistImageMaxPixels {
		return fmt.Errorf("specialist image_max_pixels must be between 0 and %d", MaxSpecialistImageMaxPixels)
	}
	if config.TranscriptMaxBytes < 0 || config.TranscriptMaxBytes > MaxSpecialistTranscriptMaxBytes {
		return fmt.Errorf("specialist transcript_max_bytes is invalid")
	}
	if config.EvidenceMaxInputBytes < 0 || config.EvidenceMaxInputBytes > MaxSpecialistEvidenceMaxInputBytes {
		return fmt.Errorf("specialist evidence_max_input_bytes is invalid")
	}
	if config.EvidenceMaxOutputBytes < 0 || config.EvidenceMaxOutputBytes > MaxSpecialistEvidenceMaxOutputBytes {
		return fmt.Errorf("specialist evidence_max_output_bytes is invalid")
	}
	if config.EvidenceMaxItems < 0 || config.EvidenceMaxItems > MaxSpecialistEvidenceMaxItems {
		return fmt.Errorf("specialist evidence_max_items is invalid")
	}
	if config.EvidenceTimeoutSeconds < 0 || config.EvidenceTimeoutSeconds > MaxSpecialistEvidenceTimeoutSeconds {
		return fmt.Errorf("specialist evidence_timeout_seconds is invalid")
	}
	if config.Profile != "" {
		if err := validateProfileName(config.Profile); err != nil {
			return fmt.Errorf("specialist profile: %w", err)
		}
	}
	return nil
}

// validateImageSidecarConfig validates the composition required to keep image
// bytes away from the main provider. It intentionally relies on the declared
// profile capability rather than the 8081 loopback compatibility heuristic.
func validateImageSidecarConfig(config Config) error {
	if !config.Specialist.ImageSidecarEnabled {
		return nil
	}
	if !config.Specialist.Enabled {
		return errors.New("specialist image sidecar requires specialist.enabled=true")
	}
	if len(config.Profiles) == 0 {
		return errors.New("specialist image sidecar requires a configured local child specialist profile")
	}
	profile, err := config.resolveSpecialistProfile(config.Specialist.Profile)
	if err != nil {
		return fmt.Errorf("specialist image sidecar: %w", err)
	}
	if !profile.Provider.ImageInput {
		return fmt.Errorf("specialist image sidecar profile %q requires explicit provider image_input=true", profile.Name)
	}
	return nil
}

// SpecialistService is the provider-neutral, no-tools helper boundary.
type SpecialistService struct {
	provider    Provider
	factory     func() (Provider, error)
	model       string
	profile     string
	role        ProviderRole
	endpoint    string
	local       bool
	configured  bool
	config      SpecialistConfig
	sem         chan struct{}
	once        sync.Once
	providerErr error
	resolver    AttachmentArtifactResolver
	mu          sync.RWMutex
	broker      *LocalChildCapacityBroker
}

func NewSpecialistService(config Config) (*SpecialistService, error) {
	specialistConfig := config.Specialist.effective()
	if !specialistConfig.Enabled {
		return nil, nil
	}
	if err := specialistConfig.Validate(); err != nil {
		return nil, err
	}
	if err := validateImageSidecarConfig(config); err != nil {
		return nil, err
	}
	profile, err := config.resolveSpecialistProfile(specialistConfig.Profile)
	if err != nil {
		return nil, err
	}
	if !providerIsLocal(profile.Provider) {
		return nil, errors.New("specialist provider must use a local endpoint")
	}
	service := &SpecialistService{model: profile.Provider.Model, profile: profile.Name, role: profile.Role, endpoint: profile.Provider.BaseURL, local: providerIsLocal(profile.Provider), configured: len(config.Profiles) > 0, config: specialistConfig, sem: make(chan struct{}, specialistConfig.MaxConcurrency)}
	service.factory = func() (Provider, error) { return NewProvider(profile.Provider) }
	return service, nil
}

func NewSpecialistServiceWithProvider(provider Provider, model string, config SpecialistConfig) (*SpecialistService, error) {
	if provider == nil {
		return nil, errors.New("specialist provider is required")
	}
	if config.ImageSidecarEnabled {
		return nil, errors.New("specialist image sidecar requires a configured local child specialist profile")
	}
	config = config.effective()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("specialist model is required")
	}
	return &SpecialistService{provider: provider, model: model, config: config, sem: make(chan struct{}, config.MaxConcurrency)}, nil
}

// newConfiguredChildSpecialistService is intentionally test/composition-only:
// production callers must obtain provenance from Config profiles, never from
// caller-provided role/name strings.
func newConfiguredChildSpecialistService(provider Provider, model, profile, endpoint string, config SpecialistConfig) (*SpecialistService, error) {
	serviceConfig := config
	serviceConfig.ImageSidecarEnabled = false
	service, err := NewSpecialistServiceWithProvider(provider, model, serviceConfig)
	if err != nil {
		return nil, err
	}
	service.config = config.effective()
	service.profile = profile
	service.role = ProviderRoleChild
	service.endpoint = endpoint
	service.configured = true
	return service, nil
}

func (config Config) resolveSpecialistProfile(name string) (ProviderProfile, error) {
	if len(config.Profiles) == 0 {
		if name != "" {
			return ProviderProfile{}, fmt.Errorf("specialist provider profile %q is not configured", name)
		}
		return ProviderProfile{Name: "legacy-specialist", Role: ProviderRoleMain, Provider: config.Provider}, nil
	}
	if name == "" {
		name = config.Routing.ChildProfile
	}
	if name == "" {
		return ProviderProfile{}, errors.New("specialist provider profile is required")
	}
	for _, profile := range config.Profiles {
		if profile.Name == name {
			if profile.Role != ProviderRoleChild || !providerIsLocal(profile.Provider) {
				return ProviderProfile{}, errors.New("specialist refuses manual/cloud providers; specialist requires a local child provider")
			}
			return profile, nil
		}
	}
	return ProviderProfile{}, fmt.Errorf("specialist provider profile %q is not configured", name)
}

func (service *SpecialistService) Profile() string {
	if service == nil {
		return ""
	}
	return service.profile
}

func (service *SpecialistService) isConfiguredLocalChild() bool {
	if service == nil || !service.configured || service.role != ProviderRoleChild || strings.TrimSpace(service.profile) == "" {
		return false
	}
	return service.local || providerURLIsLocal(service.endpoint)
}

func (service *SpecialistService) requireConfiguredLocalChild(purpose string) error {
	if !service.isConfiguredLocalChild() {
		return fmt.Errorf("%s requires a configured local child specialist profile", purpose)
	}
	return nil
}
func (service *SpecialistService) Model() string {
	if service == nil {
		return ""
	}
	return service.model
}
func (service *SpecialistService) SetCapacityBroker(broker *LocalChildCapacityBroker) {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.broker = broker
	service.mu.Unlock()
}

func (service *SpecialistService) Config() SpecialistConfig {
	if service == nil {
		return SpecialistConfig{}
	}
	return service.config
}
func (service *SpecialistService) SetAttachmentResolver(resolver AttachmentArtifactResolver) {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.resolver = resolver
	provider := service.provider
	service.mu.Unlock()
	if provider != nil {
		if configurable, ok := provider.(interface {
			SetAttachmentResolver(AttachmentArtifactResolver)
		}); ok && resolver != nil {
			configurable.SetAttachmentResolver(boundedSpecialistResolver{resolver: resolver, maxBytes: int64(service.config.ImageMaxBytes), maxDimension: service.config.ImageMaxDimension, maxPixels: int64(service.config.ImageMaxPixels)})
		}
	}
}
func (service *SpecialistService) getProvider() (Provider, error) {
	service.mu.RLock()
	provider, providerErr := service.provider, service.providerErr
	service.mu.RUnlock()
	if provider != nil || providerErr != nil {
		return provider, providerErr
	}
	service.once.Do(func() {
		created, err := service.factory()
		service.mu.Lock()
		service.provider, service.providerErr = created, err
		resolver := service.resolver
		service.mu.Unlock()
		if err == nil && resolver != nil {
			if configurable, ok := created.(interface {
				SetAttachmentResolver(AttachmentArtifactResolver)
			}); ok {
				configurable.SetAttachmentResolver(boundedSpecialistResolver{resolver: resolver, maxBytes: int64(service.config.ImageMaxBytes), maxDimension: service.config.ImageMaxDimension, maxPixels: int64(service.config.ImageMaxPixels)})
			}
		}
	})
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.provider, service.providerErr
}

type boundedSpecialistResolver struct {
	resolver     AttachmentArtifactResolver
	maxBytes     int64
	maxDimension int
	maxPixels    int64
}

func (resolver boundedSpecialistResolver) OpenAttachmentPayload(ctx context.Context, ref AttachmentRef) (Artifact, io.ReadCloser, error) {
	return normalizeSpecialistImagePayload(ctx, resolver.resolver, ref, resolver.maxBytes, resolver.maxDimension, resolver.maxPixels)
}

func (service *SpecialistService) call(ctx context.Context, system string, messages []Message) (string, error) {
	return service.callWithPriority(ctx, LocalChildPurposeSpecialist, LocalChildPriorityNormal, system, messages, service.config.TimeoutSeconds)
}

func (service *SpecialistService) callWithTimeout(ctx context.Context, system string, messages []Message, timeoutSeconds int) (string, error) {
	return service.callWithPriority(ctx, LocalChildPurposeSpecialist, LocalChildPriorityNormal, system, messages, timeoutSeconds)
}

func (service *SpecialistService) callWithPriority(ctx context.Context, purpose LocalChildPurpose, priority LocalChildPriority, system string, messages []Message, timeoutSeconds int) (string, error) {
	if service == nil {
		return "", errors.New("specialist service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for index := range messages {
		if len(messages[index].Content) > service.config.MaxInputBytes {
			messages[index].Content = truncateUTF8(messages[index].Content, service.config.MaxInputBytes)
		}
	}
	select {
	case service.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-service.sem }()
	service.mu.RLock()
	broker := service.broker
	service.mu.RUnlock()
	var err error
	callCtx := ctx
	cancel := func() {}
	if timeoutSeconds > 0 {
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	}
	defer cancel()
	provider, err := service.getProvider()
	if err != nil {
		return "", err
	}
	input := make([]Message, 0, len(messages)+1)
	if system != "" {
		input = append(input, Message{Role: RoleSystem, Content: truncateUTF8(system, service.config.MaxInputBytes)})
	}
	input = append(input, messages...)
	request, err := NormalizeRequest(Request{Model: service.model, Messages: input, Temperature: 0, MaxTokens: service.config.MaxTokens, DisableThinking: true})
	if err != nil {
		return "", err
	}
	var lease *LocalChildLease
	if broker != nil {
		lease, err = broker.Acquire(callCtx, purpose, priority)
		if err != nil {
			return "", err
		}
	}
	response, err := provider.Complete(callCtx, request)
	if lease != nil {
		lease.ReleaseError(err)
	}
	if err != nil {
		return "", err
	}
	content := strings.TrimSpace(response.Message.Content)
	if len(content) > service.config.MaxOutputBytes {
		return "", fmt.Errorf("specialist response exceeds %d bytes", service.config.MaxOutputBytes)
	}
	return content, nil
}

func (service *SpecialistService) Complete(ctx context.Context, system, input string) (string, error) {
	input = truncateUTF8(input, service.config.MaxInputBytes)
	return service.call(ctx, system, []Message{{Role: RoleUser, Content: input}})
}

// ImageInterpretation is transient helper output; the original AttachmentRef
// remains the only durable attachment representation.
type ImageInterpretation struct {
	Summary string   `json:"summary"`
	Claims  []string `json:"claims,omitempty"`
}

// specialistJSONPayload accepts either a bare JSON value or one exact JSON
// Markdown fence with no surrounding prose. Some local multimodal models wrap
// otherwise-valid JSON despite an explicit JSON-only instruction.
func specialistJSONPayload(content string) ([]byte, error) {
	value := strings.TrimSpace(content)
	if value == "" {
		return nil, errors.New("specialist returned empty JSON content")
	}
	if strings.HasPrefix(value, "```") {
		lineEnd := strings.IndexByte(value, '\n')
		if lineEnd < 0 {
			return nil, errors.New("specialist JSON fence is incomplete")
		}
		header := strings.TrimSpace(value[:lineEnd])
		if header != "```" && !strings.EqualFold(header, "```json") {
			return nil, errors.New("specialist JSON fence has an unsupported language")
		}
		body := strings.TrimSpace(value[lineEnd+1:])
		if !strings.HasSuffix(body, "```") {
			return nil, errors.New("specialist JSON fence is missing its closing delimiter")
		}
		body = strings.TrimSpace(strings.TrimSuffix(body, "```"))
		if strings.Contains(body, "```") {
			return nil, errors.New("specialist JSON fence contains nested delimiters")
		}
		value = body
	} else if strings.Contains(value, "```") {
		return nil, errors.New("specialist JSON content contains unexpected fence text")
	}
	payload := []byte(value)
	if !json.Valid(payload) {
		return nil, errors.New("specialist response is not valid JSON")
	}
	return payload, nil
}

func (service *SpecialistService) InterpretImage(ctx context.Context, ref AttachmentRef) (ImageInterpretation, error) {
	if service == nil {
		return ImageInterpretation{}, errors.New("specialist service is unavailable")
	}
	service.mu.RLock()
	resolver := service.resolver
	service.mu.RUnlock()
	if resolver == nil {
		return ImageInterpretation{}, errors.New("specialist attachment resolver is not configured")
	}
	capable, ok := service.getProviderSupportsImages()
	if !ok || !capable {
		return ImageInterpretation{}, errors.New("specialist provider has no declared image capability")
	}
	content, err := service.call(ctx, `Return ONLY JSON matching {"summary":"...","claims":["..."]}. Describe the image conservatively. Never mention storage paths, credentials, or hidden metadata.`, []Message{{Role: RoleUser, Content: "Interpret this image attachment.", Attachments: []AttachmentRef{ref}}})
	if err != nil {
		return ImageInterpretation{}, err
	}
	payload, err := specialistJSONPayload(content)
	if err != nil {
		return ImageInterpretation{}, fmt.Errorf("specialist image response is not JSON: %w", err)
	}
	var result ImageInterpretation
	if err := json.Unmarshal(payload, &result); err != nil {
		return ImageInterpretation{}, fmt.Errorf("specialist image response is not JSON: %w", err)
	}
	result.Summary = truncateUTF8(strings.TrimSpace(result.Summary), service.config.MaxOutputBytes)
	for i := range result.Claims {
		result.Claims[i] = truncateUTF8(strings.TrimSpace(result.Claims[i]), 256)
	}
	return result, nil
}

func (service *SpecialistService) getProviderSupportsImages() (bool, bool) {
	provider, err := service.getProvider()
	if err != nil {
		return false, false
	}
	capable, ok := provider.(interface{ SupportsImageInput() bool })
	return ok && capable.SupportsImageInput(), true
}

func (service *SpecialistService) SummarizeTranscript(ctx context.Context, transcript string) (string, error) {
	transcript = truncateUTF8(transcript, service.config.TranscriptMaxBytes)
	return service.Complete(ctx, "Summarize the supplied audio transcript into concise factual text. Do not add facts, paths, keys, or speaker identities not present.", transcript)
}

type ObservationSummary struct {
	Errors  []string `json:"errors,omitempty"`
	Facts   []string `json:"facts,omitempty"`
	Anchors []string `json:"anchors,omitempty"`
}

func (service *SpecialistService) CompressObservation(ctx context.Context, observation string) (string, error) {
	if service == nil {
		return "", errors.New("specialist service is unavailable")
	}
	if len(observation) <= service.config.ObservationThresholdBytes {
		return observation, nil
	}
	preprocessed := DeterministicObservationPreprocess(observation)
	if len(preprocessed) <= service.config.ObservationThresholdBytes {
		return preprocessed, nil
	}
	input := truncateUTF8(preprocessed, service.config.MaxInputBytes)
	if service.config.EvidenceEnabled {
		if extractor, extractorErr := NewEvidenceExtractor(service); extractorErr == nil {
			if packet, evidenceErr := extractor.Extract(ctx, input); evidenceErr == nil {
				if encoded, marshalErr := json.Marshal(packet); marshalErr == nil && len(encoded) <= service.config.ObservationMaxBytes {
					return string(encoded), nil
				}
			}
		}
		// Evidence extraction is an opt-in sidecar. Invalid/failed model output
		// never suppresses the deterministic bounded fallback.
		return DeterministicObservationFallback(input, service.config.ObservationMaxBytes), nil
	}
	content, err := service.callWithTimeout(ctx, `Return ONLY JSON matching {"errors":["..."],"facts":["..."],"anchors":["..."]}. Preserve concrete error text, important facts, identifiers safe to repeat, and actionable anchors. Never include paths or credentials unless already opaque identifiers.`, []Message{{Role: RoleUser, Content: input}}, service.config.ObservationCompressionTimeoutSeconds)
	if err != nil {
		return "", err
	}
	content, err = unwrapObservationJSONFence(content)
	if err != nil {
		return "", err
	}
	var summary ObservationSummary
	decoder := json.NewDecoder(bytes.NewReader([]byte(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&summary); err != nil {
		return "", err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return "", err
	}
	return marshalBoundedObservationSummary(summary, service.config.ObservationMaxBytes), nil
}

func unwrapObservationJSONFence(content string) (string, error) {
	content = strings.TrimSpace(content)
	prefix := ""
	switch {
	case strings.HasPrefix(content, "```json"):
		prefix = "```json"
	case strings.HasPrefix(content, "```"):
		prefix = "```"
	default:
		return content, nil
	}
	rest := strings.TrimLeft(content[len(prefix):], " \t\r\n")
	if !strings.HasSuffix(rest, "```") {
		return "", errors.New("specialist JSON fence is not closed")
	}
	body := strings.TrimSpace(strings.TrimSuffix(rest, "```"))
	if body == "" || strings.Contains(body, "```") {
		return "", errors.New("specialist JSON contains an invalid nested fence")
	}
	return body, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("specialist JSON has trailing data")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func marshalBoundedObservationSummary(summary ObservationSummary, maxBytes int) string {
	if maxBytes <= 1 {
		return ""
	}
	trim := func(values []string, max int) []string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			value = truncateUTF8(strings.TrimSpace(value), max)
			if value != "" {
				result = append(result, value)
			}
		}
		return result
	}
	summary.Errors, summary.Facts, summary.Anchors = trim(summary.Errors, maxBytes), trim(summary.Facts, maxBytes), trim(summary.Anchors, maxBytes)
	for {
		data, _ := json.Marshal(summary)
		if len(data) <= maxBytes {
			return string(data)
		}
		changed := false
		for _, list := range []*[]string{&summary.Facts, &summary.Anchors, &summary.Errors} {
			if len(*list) > 0 {
				*list = (*list)[:len(*list)-1]
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		return "{}"
	}
}

func DeterministicObservationFallback(observation string, maxBytes int) string {
	return DeterministicObservationFallbackPreservingSignals(observation, maxBytes)
}

type failureState struct {
	first, last time.Time
	count       int
	diagnosed   bool
}

type ToolFailureDetector struct {
	service   *SpecialistService
	threshold int
	window    time.Duration
	mu        sync.Mutex
	states    map[string]failureState
}

func NewToolFailureDetector(service *SpecialistService, config SpecialistConfig) *ToolFailureDetector {
	config = config.effective()
	return &ToolFailureDetector{service: service, threshold: config.FailureThreshold, window: time.Duration(config.FailureWindowSeconds) * time.Second, states: make(map[string]failureState)}
}

func (detector *ToolFailureDetector) pruneExpired(now time.Time) {
	for signature, state := range detector.states {
		if state.last.IsZero() || now.Sub(state.last) >= detector.window {
			delete(detector.states, signature)
		}
	}
}

func (detector *ToolFailureDetector) evictOldest() {
	if len(detector.states) < MaxSpecialistFailureStates {
		return
	}
	oldestSignature := ""
	var oldest time.Time
	for signature, state := range detector.states {
		if oldestSignature == "" || state.last.Before(oldest) || (state.last.Equal(oldest) && signature < oldestSignature) {
			oldestSignature, oldest = signature, state.last
		}
	}
	if oldestSignature != "" {
		delete(detector.states, oldestSignature)
	}
}

var dynamicFailurePattern = regexp.MustCompile(`(?i)(0x[0-9a-f]+|[0-9a-f]{8,}|\b\d+\b|/[^ ]+|\\[^ ]+)`)

func NormalizeToolFailureSignature(toolName, failure string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(failure)), " "))
	normalized = dynamicFailurePattern.ReplaceAllString(normalized, "#")
	return strings.ToLower(strings.TrimSpace(toolName)) + ":" + normalized
}

func (detector *ToolFailureDetector) Observe(ctx context.Context, toolName, failure string) string {
	if detector == nil || detector.service == nil || strings.TrimSpace(failure) == "" {
		return ""
	}
	now := time.Now().UTC()
	signature := NormalizeToolFailureSignature(toolName, failure)
	detector.mu.Lock()
	detector.pruneExpired(now)
	if _, exists := detector.states[signature]; !exists {
		detector.evictOldest()
	}
	state := detector.states[signature]
	if state.first.IsZero() || now.Sub(state.first) > detector.window {
		state = failureState{first: now}
	}
	state.last, state.count = now, state.count+1
	shouldDiagnose := state.count >= detector.threshold && !state.diagnosed
	if shouldDiagnose {
		state.diagnosed = true
	}
	detector.states[signature] = state
	detector.mu.Unlock()
	if !shouldDiagnose {
		return ""
	}
	input := fmt.Sprintf("tool=%s\nsignature=%s\nlatest_error=%s\nrepeated_count=%d", truncateUTF8(toolName, 128), truncateUTF8(signature, 512), truncateUTF8(failure, 1024), state.count)
	diagnosis, err := detector.service.Complete(ctx, "Diagnose this repeated tool failure. Return one bounded descriptive sentence only. Do not propose retries, permissions, or authority changes.", input)
	if err != nil {
		return "Repeated tool failure detected for " + truncateUTF8(toolName, 128) + "."
	}
	return truncateUTF8(diagnosis, detector.service.config.MaxOutputBytes)
}

type SessionDigest struct {
	Title  string `json:"title,omitempty"`
	Digest string `json:"digest,omitempty"`
}

func (service *SpecialistService) ExtractSessionDigest(ctx context.Context, prompt, assistant string) (SessionDigest, error) {
	input := `PROMPT:\n` + truncateUTF8(prompt, service.config.MaxInputBytes/2) + `\nASSISTANT:\n` + truncateUTF8(assistant, service.config.MaxInputBytes/2)
	content, err := service.Complete(ctx, `Return ONLY JSON matching {"title":"...","digest":"..."}. Title max 80 bytes; digest max 512 bytes. Summarize only this completed turn.`, input)
	if err != nil {
		return SessionDigest{}, err
	}
	var result SessionDigest
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return SessionDigest{}, err
	}
	result.Title, result.Digest = truncateUTF8(strings.TrimSpace(result.Title), 80), truncateUTF8(strings.TrimSpace(result.Digest), service.config.DigestMaxBytes)
	return result, nil
}
