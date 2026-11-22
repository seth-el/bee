package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const (
	DefaultGemmaMaxAudioBytes    int64 = 32 << 20
	DefaultGemmaMaxRequestBytes  int64 = 48 << 20
	DefaultGemmaMaxResponseBytes int64 = 1 << 20
	DefaultGemmaMaxTokens              = 900
	DefaultOmniMaxTextBytes            = 8 << 10
	DefaultOmniMaxRequestBytes   int64 = 16 << 20
	DefaultOmniMaxWAVBytes       int64 = 64 << 20
	DefaultOmniMaxOutputBytes    int64 = 32 << 20
	DefaultTTSEndpoint                 = "http://127.0.0.1:8881/v1/audio/speech"
	DefaultTTSInstruction              = "Speak naturally and clearly."
	DefaultTTSCfgScale                 = 2.5
	DefaultTTSSeed                     = int64(42)
	DefaultTTSLockWait                 = 20 * time.Minute
	DefaultTTSLockStale                = 15 * time.Minute
	DefaultTTSRetries                  = 2
	DefaultTTSRetryBase                = time.Second
	DefaultTTSMaxTextBytes             = 8 << 10
	DefaultTTSMaxRequestBytes    int64 = 16 << 20
	DefaultTTSMaxPCMBytes        int64 = 64 << 20
	DefaultTTSMaxOutputBytes     int64 = 32 << 20
	MaxProviderEndpointBytes           = 1024
	MaxProviderModelBytes              = 512
	MaxProviderPromptBytes             = 4096
	MaxProviderRefTextBytes            = 4096
	MaxProviderAPIKeyBytes             = 1024
)

// CommandRunner is an injectable, context-aware process seam. Implementations
// must not include source paths or provider responses in returned errors.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, []byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	stdout, err := command.Output()
	if err == nil {
		return stdout, nil, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout, exitErr.Stderr, err
	}
	return stdout, nil, err
}

func providerEndpoint(name, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || len(endpoint) > MaxProviderEndpointBytes || !utf8.ValidString(endpoint) {
		return fmt.Errorf("%s endpoint is required and bounded", name)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s endpoint must be an absolute http(s) URL without credentials or query", name)
	}
	return nil
}

func validateProviderText(name, value string, max int) error {
	if len(value) == 0 || len(value) > max || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s is required and bounded", name)
	}
	return nil
}

// GemmaArtifactRecognizerConfig configures the local OpenAI-compatible audio
// endpoint. APIKey is optional for local endpoints and never appears in errors.
type GemmaArtifactRecognizerConfig struct {
	Endpoint         string
	Model            string
	APIKey           string
	Prompt           string
	HTTPClient       *http.Client
	CommandRunner    CommandRunner
	MaxAudioBytes    int64
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxTokens        int
}

// GemmaArtifactRecognizer normalizes an adapter-owned voice artifact to WAV,
// sends base64 input_audio JSON, and returns only bounded transcript text.
type GemmaArtifactRecognizer struct {
	endpoint         string
	model            string
	apiKey           string
	prompt           string
	httpClient       *http.Client
	commandRunner    CommandRunner
	maxAudioBytes    int64
	maxRequestBytes  int64
	maxResponseBytes int64
	maxTokens        int
}

func NewGemmaArtifactRecognizer(config GemmaArtifactRecognizerConfig) (*GemmaArtifactRecognizer, error) {
	if err := providerEndpoint("Gemma", config.Endpoint); err != nil {
		return nil, err
	}
	if err := validateProviderText("Gemma model", config.Model, MaxProviderModelBytes); err != nil {
		return nil, err
	}
	if config.APIKey != "" && (len(config.APIKey) > MaxProviderAPIKeyBytes || !utf8.ValidString(config.APIKey) || strings.ContainsAny(config.APIKey, "\r\n")) {
		return nil, errors.New("Gemma API key is invalid or too large")
	}
	if config.Prompt == "" {
		config.Prompt = "Transcribe the spoken words in this audio accurately. Ignore background noise. Do not summarize. Respond with only the transcript text."
	}
	if err := validateProviderText("Gemma prompt", config.Prompt, MaxProviderPromptBytes); err != nil {
		return nil, err
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 75 * time.Second}
	}
	if config.CommandRunner == nil {
		config.CommandRunner = execCommandRunner{}
	}
	if config.MaxAudioBytes <= 0 || config.MaxAudioBytes > MaxTelegramVoiceFileSize {
		config.MaxAudioBytes = DefaultGemmaMaxAudioBytes
	}
	if config.MaxRequestBytes <= 0 || config.MaxRequestBytes > MaxTelegramVoiceFileSize {
		config.MaxRequestBytes = DefaultGemmaMaxRequestBytes
	}
	if config.MaxResponseBytes <= 0 || config.MaxResponseBytes > MaxResponseBytes {
		config.MaxResponseBytes = DefaultGemmaMaxResponseBytes
	}
	if config.MaxTokens <= 0 || config.MaxTokens > 4096 {
		config.MaxTokens = DefaultGemmaMaxTokens
	}
	return &GemmaArtifactRecognizer{endpoint: strings.TrimSpace(config.Endpoint), model: config.Model, apiKey: config.APIKey, prompt: config.Prompt, httpClient: config.HTTPClient, commandRunner: config.CommandRunner, maxAudioBytes: config.MaxAudioBytes, maxRequestBytes: config.MaxRequestBytes, maxResponseBytes: config.MaxResponseBytes, maxTokens: config.MaxTokens}, nil
}

func (recognizer *GemmaArtifactRecognizer) RecognizeArtifact(ctx context.Context, artifact *VoiceArtifact) (string, error) {
	if recognizer == nil || recognizer.httpClient == nil || recognizer.commandRunner == nil {
		return "", ErrSpeechRecognitionUnavailable
	}
	if ctx == nil {
		return "", errors.New("Gemma recognition context is nil")
	}
	if artifact == nil {
		return "", errors.New("Gemma voice artifact is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	input, err := artifact.Input(), error(nil)
	if err = input.Validate(); err != nil {
		return "", errors.New("Gemma voice artifact metadata is invalid")
	}
	file, err := artifact.Open()
	if err != nil {
		return "", errors.New("open Gemma voice artifact")
	}
	sourcePath := file.Name()
	if err := file.Close(); err != nil {
		return "", errors.New("close Gemma voice artifact")
	}
	tmpDir, err := os.MkdirTemp("", ".bee-gemma-stt-")
	if err != nil {
		return "", errors.New("create Gemma staging directory")
	}
	defer os.RemoveAll(tmpDir)
	wavPath := filepath.Join(tmpDir, "input.wav")
	_, _, runErr := recognizer.commandRunner.Run(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", sourcePath, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wavPath)
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("normalize voice artifact for Gemma")
	}
	data, err := readFileBounded(wavPath, recognizer.maxAudioBytes)
	if err != nil {
		return "", errors.New("read normalized Gemma audio")
	}
	requestBody, err := json.Marshal(map[string]any{
		"model": recognizer.model,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]string{"type": "text", "text": recognizer.prompt},
			map[string]any{"type": "input_audio", "input_audio": map[string]string{"data": base64.StdEncoding.EncodeToString(data), "format": "wav"}},
		}}},
		"temperature":          0,
		"max_tokens":           recognizer.maxTokens,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	if err != nil || int64(len(requestBody)) > recognizer.maxRequestBytes {
		return "", errors.New("Gemma request exceeds bounded size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, recognizer.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", errors.New("create Gemma request")
	}
	req.Header.Set("Content-Type", "application/json")
	if recognizer.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+recognizer.apiKey)
	}
	resp, err := recognizer.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("Gemma HTTP request failed")
	}
	defer resp.Body.Close()
	responseData, err := providerReadBounded(resp.Body, recognizer.maxResponseBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("read Gemma response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Gemma endpoint returned HTTP status %d", resp.StatusCode)
	}
	transcript, err := parseGemmaTranscript(responseData)
	if err != nil {
		return "", err
	}
	return NormalizeTranscript(transcript)
}

func (recognizer *GemmaArtifactRecognizer) Recognize(context.Context, bee.VoiceInput) (string, error) {
	return "", ErrSpeechRecognitionUnavailable
}

func parseGemmaTranscript(data []byte) (string, error) {
	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Choices) == 0 {
		return "", errors.New("decode Gemma transcript response")
	}
	content := payload.Choices[0].Message.Content
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var parts []string
		for _, part := range value {
			if object, ok := part.(map[string]any); ok {
				if text, ok := object["text"].(string); ok {
					parts = append(parts, text)
				} else if text, ok := object["content"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", errors.New("Gemma transcript response has no text")
	}
}

// OmniVoiceSynthesizerConfig configures the local multipart TTS endpoint.
type OmniVoiceSynthesizerConfig struct {
	Endpoint        string
	RefAudioPath    string
	RefText         string
	HTTPClient      *http.Client
	CommandRunner   CommandRunner
	ArtifactStore   *VoiceOutputArtifactStore
	MaxTextBytes    int
	MaxRequestBytes int64
	MaxWAVBytes     int64
	MaxOutputBytes  int64
}

// OmniVoiceSynthesizer calls OmniVoice with bounded text and stages a private
// Telegram-compatible OGG/Opus artifact behind an opaque VoiceOutput ref.
type OmniVoiceSynthesizer struct {
	endpoint        string
	refAudioPath    string
	refText         string
	httpClient      *http.Client
	commandRunner   CommandRunner
	artifactStore   *VoiceOutputArtifactStore
	maxTextBytes    int
	maxRequestBytes int64
	maxWAVBytes     int64
	maxOutputBytes  int64
}

func NewOmniVoiceSynthesizer(config OmniVoiceSynthesizerConfig) (*OmniVoiceSynthesizer, error) {
	if err := providerEndpoint("OmniVoice", config.Endpoint); err != nil {
		return nil, err
	}
	if err := validatePrivateAudioPath(config.RefAudioPath, MaxTelegramVoiceFileSize); err != nil {
		return nil, err
	}
	if err := validateProviderText("OmniVoice reference text", config.RefText, MaxProviderRefTextBytes); err != nil {
		return nil, err
	}
	if config.ArtifactStore == nil {
		return nil, errors.New("OmniVoice artifact store is required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 75 * time.Second}
	}
	if config.CommandRunner == nil {
		config.CommandRunner = execCommandRunner{}
	}
	if config.MaxTextBytes <= 0 || config.MaxTextBytes > MaxTelegramSynthesisBytes {
		config.MaxTextBytes = DefaultOmniMaxTextBytes
	}
	if config.MaxRequestBytes <= 0 || config.MaxRequestBytes > MaxTelegramVoiceFileSize {
		config.MaxRequestBytes = DefaultOmniMaxRequestBytes
	}
	if config.MaxWAVBytes <= 0 || config.MaxWAVBytes > MaxTelegramVoiceFileSize {
		config.MaxWAVBytes = DefaultOmniMaxWAVBytes
	}
	if config.MaxOutputBytes <= 0 || config.MaxOutputBytes > MaxTelegramVoiceFileSize {
		config.MaxOutputBytes = DefaultOmniMaxOutputBytes
	}
	return &OmniVoiceSynthesizer{endpoint: strings.TrimSpace(config.Endpoint), refAudioPath: config.RefAudioPath, refText: config.RefText, httpClient: config.HTTPClient, commandRunner: config.CommandRunner, artifactStore: config.ArtifactStore, maxTextBytes: config.MaxTextBytes, maxRequestBytes: config.MaxRequestBytes, maxWAVBytes: config.MaxWAVBytes, maxOutputBytes: config.MaxOutputBytes}, nil
}

func (synthesizer *OmniVoiceSynthesizer) Synthesize(ctx context.Context, text string) (bee.VoiceOutput, error) {
	if synthesizer == nil || synthesizer.httpClient == nil || synthesizer.commandRunner == nil || synthesizer.artifactStore == nil {
		return bee.VoiceOutput{}, ErrSpeechSynthesisUnavailable
	}
	if ctx == nil {
		return bee.VoiceOutput{}, errors.New("OmniVoice synthesis context is nil")
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	text, err := NormalizeSynthesisInput(text)
	if err != nil {
		return bee.VoiceOutput{}, err
	}
	if len(text) > synthesizer.maxTextBytes {
		return bee.VoiceOutput{}, errors.New("OmniVoice text exceeds bounded synthesis chunk")
	}
	refAudio, err := os.Open(synthesizer.refAudioPath)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("open OmniVoice reference audio")
	}
	defer refAudio.Close()
	info, err := refAudio.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxTelegramVoiceFileSize {
		return bee.VoiceOutput{}, errors.New("OmniVoice reference audio is invalid")
	}
	refBytes, err := providerReadBounded(refAudio, MaxTelegramVoiceFileSize)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("read OmniVoice reference audio")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", text); err != nil {
		return bee.VoiceOutput{}, errors.New("encode OmniVoice text")
	}
	if err := writer.WriteField("ref_text", synthesizer.refText); err != nil {
		return bee.VoiceOutput{}, errors.New("encode OmniVoice reference text")
	}
	if err := writer.WriteField("response_format", "wav"); err != nil {
		return bee.VoiceOutput{}, errors.New("encode OmniVoice response format")
	}
	part, err := writer.CreateFormFile("ref_audio", filepath.Base(synthesizer.refAudioPath))
	if err != nil {
		return bee.VoiceOutput{}, errors.New("encode OmniVoice reference audio")
	}
	if _, err := part.Write(refBytes); err != nil {
		return bee.VoiceOutput{}, errors.New("write OmniVoice reference audio")
	}
	if err := writer.Close(); err != nil || int64(body.Len()) > synthesizer.maxRequestBytes {
		return bee.VoiceOutput{}, errors.New("OmniVoice request exceeds bounded size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, synthesizer.endpoint, &body)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("create OmniVoice request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := synthesizer.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, errors.New("OmniVoice HTTP request failed")
	}
	defer resp.Body.Close()
	wav, err := providerReadBounded(resp.Body, synthesizer.maxWAVBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, errors.New("read OmniVoice WAV response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bee.VoiceOutput{}, fmt.Errorf("OmniVoice endpoint returned HTTP status %d", resp.StatusCode)
	}
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return bee.VoiceOutput{}, errors.New("OmniVoice response is not a WAV file")
	}
	tmpDir, err := os.MkdirTemp("", ".bee-omnivoice-")
	if err != nil {
		return bee.VoiceOutput{}, errors.New("create OmniVoice staging directory")
	}
	defer os.RemoveAll(tmpDir)
	wavPath := filepath.Join(tmpDir, "input.wav")
	oggPath := filepath.Join(tmpDir, "output.ogg")
	if err := os.WriteFile(wavPath, wav, 0o600); err != nil {
		return bee.VoiceOutput{}, errors.New("stage OmniVoice WAV")
	}
	if _, _, runErr := synthesizer.commandRunner.Run(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", wavPath, "-c:a", "libopus", "-b:a", "32k", "-vn", "-f", "ogg", oggPath); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, errors.New("convert OmniVoice WAV to OGG")
	}
	ogg, err := readFileBounded(oggPath, synthesizer.maxOutputBytes)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("read OmniVoice OGG output")
	}
	if len(ogg) < 4 || string(ogg[:4]) != "OggS" {
		return bee.VoiceOutput{}, errors.New("OmniVoice conversion did not produce OGG")
	}
	return synthesizer.artifactStore.Stage(ctx, ogg, "audio/ogg", 0)
}

// TTSSynthesizerConfig configures the local TTS TTS 2 multipart PCM
// endpoint. TTS returns raw 24 kHz mono signed-16 PCM; the synthesizer
// converts it to WAV and OGG/Opus before staging the provider-neutral output.
// It is intentionally a composition-time config: the default Telegram
// constructor remains text-only and never contacts TTS by itself.
type TTSSynthesizerConfig struct {
	Endpoint        string
	RefAudioPath    string
	RefText         string
	Instruction     string
	CfgScale        float64
	Seed            int64
	HTTPClient      *http.Client
	CommandRunner   CommandRunner
	ArtifactStore   *VoiceOutputArtifactStore
	LockPath        string
	LockWait        time.Duration
	LockStale       time.Duration
	MaxRetries      int
	RetryBase       time.Duration
	MaxTextBytes    int
	MaxRequestBytes int64
	MaxPCMBytes     int64
	MaxOutputBytes  int64
}

// TTSSynthesizer calls TTS TTS 2 with bounded multipart form data and
// stages a private Telegram-compatible OGG/Opus artifact behind an opaque
// VoiceOutput reference. The lock is shared with other local voice clients so
// concurrent callers do not overload the service.
type TTSSynthesizer struct {
	endpoint        string
	refAudioPath    string
	refText         string
	instruction     string
	cfgScale        float64
	seed            int64
	httpClient      *http.Client
	commandRunner   CommandRunner
	artifactStore   *VoiceOutputArtifactStore
	lockPath        string
	lockWait        time.Duration
	lockStale       time.Duration
	maxRetries      int
	retryBase       time.Duration
	maxTextBytes    int
	maxRequestBytes int64
	maxPCMBytes     int64
	maxOutputBytes  int64
}

func NewTTSSynthesizer(config TTSSynthesizerConfig) (*TTSSynthesizer, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		config.Endpoint = DefaultTTSEndpoint
	}
	if err := providerEndpoint("TTS", config.Endpoint); err != nil {
		return nil, err
	}
	if err := validatePrivateAudioPath(config.RefAudioPath, MaxTelegramVoiceFileSize); err != nil {
		return nil, err
	}
	if err := validateProviderText("TTS reference text", config.RefText, MaxProviderRefTextBytes); err != nil {
		return nil, err
	}
	if config.Instruction == "" {
		config.Instruction = DefaultTTSInstruction
	}
	if err := validateProviderText("TTS instruction", config.Instruction, MaxProviderPromptBytes); err != nil {
		return nil, err
	}
	if config.CfgScale == 0 {
		config.CfgScale = DefaultTTSCfgScale
	}
	if config.CfgScale < 0 || config.CfgScale > 100 {
		return nil, errors.New("TTS cfg_scale is invalid or unbounded")
	}
	if config.Seed == 0 {
		config.Seed = DefaultTTSSeed
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 75 * time.Second}
	}
	if config.CommandRunner == nil {
		config.CommandRunner = execCommandRunner{}
	}
	if config.ArtifactStore == nil {
		return nil, errors.New("TTS artifact store is required")
	}
	if config.LockPath == "" {
		stateRoot, err := bee.DefaultBeeStateRoot()
		if err != nil {
			return nil, fmt.Errorf("resolve TTS lock path: %w", err)
		}
		config.LockPath = filepath.Join(stateRoot, "runtime", "tts.lock")
	}
	if err := validateTTSLockPath(config.LockPath); err != nil {
		return nil, err
	}
	if config.LockWait <= 0 || config.LockWait > 30*time.Minute {
		config.LockWait = DefaultTTSLockWait
	}
	if config.LockStale <= 0 || config.LockStale > 30*time.Minute {
		config.LockStale = DefaultTTSLockStale
	}
	if config.LockStale < time.Second {
		return nil, errors.New("TTS lock stale duration is too short")
	}
	if config.MaxRetries < 0 || config.MaxRetries > 5 {
		config.MaxRetries = DefaultTTSRetries
	}
	if config.RetryBase <= 0 || config.RetryBase > time.Minute {
		config.RetryBase = DefaultTTSRetryBase
	}
	if config.MaxTextBytes <= 0 || config.MaxTextBytes > MaxTelegramSynthesisBytes {
		config.MaxTextBytes = DefaultTTSMaxTextBytes
	}
	if config.MaxRequestBytes <= 0 || config.MaxRequestBytes > MaxTelegramVoiceFileSize {
		config.MaxRequestBytes = DefaultTTSMaxRequestBytes
	}
	if config.MaxPCMBytes <= 0 || config.MaxPCMBytes > MaxTelegramVoiceFileSize {
		config.MaxPCMBytes = DefaultTTSMaxPCMBytes
	}
	if config.MaxOutputBytes <= 0 || config.MaxOutputBytes > MaxTelegramVoiceFileSize {
		config.MaxOutputBytes = DefaultTTSMaxOutputBytes
	}
	return &TTSSynthesizer{
		endpoint: config.Endpoint, refAudioPath: config.RefAudioPath, refText: config.RefText,
		instruction: config.Instruction, cfgScale: config.CfgScale, seed: config.Seed,
		httpClient: config.HTTPClient, commandRunner: config.CommandRunner,
		artifactStore: config.ArtifactStore, lockPath: config.LockPath,
		lockWait: config.LockWait, lockStale: config.LockStale,
		maxRetries: config.MaxRetries, retryBase: config.RetryBase,
		maxTextBytes: config.MaxTextBytes, maxRequestBytes: config.MaxRequestBytes,
		maxPCMBytes: config.MaxPCMBytes, maxOutputBytes: config.MaxOutputBytes,
	}, nil
}

func (synthesizer *TTSSynthesizer) Synthesize(ctx context.Context, text string) (bee.VoiceOutput, error) {
	if synthesizer == nil || synthesizer.httpClient == nil || synthesizer.commandRunner == nil || synthesizer.artifactStore == nil {
		return bee.VoiceOutput{}, ErrSpeechSynthesisUnavailable
	}
	if ctx == nil {
		return bee.VoiceOutput{}, errors.New("TTS synthesis context is nil")
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	text, err := NormalizeSynthesisInput(text)
	if err != nil {
		return bee.VoiceOutput{}, err
	}
	if len(text) > synthesizer.maxTextBytes {
		return bee.VoiceOutput{}, errors.New("TTS text exceeds bounded synthesis chunk")
	}
	refAudio, err := os.Open(synthesizer.refAudioPath)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("open TTS reference audio")
	}
	info, statErr := refAudio.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxTelegramVoiceFileSize {
		_ = refAudio.Close()
		return bee.VoiceOutput{}, errors.New("TTS reference audio is invalid")
	}
	refBytes, err := providerReadBounded(refAudio, MaxTelegramVoiceFileSize)
	closeErr := refAudio.Close()
	if err != nil || closeErr != nil {
		return bee.VoiceOutput{}, errors.New("read TTS reference audio")
	}

	pcm, err := synthesizer.requestPCM(ctx, text, refBytes)
	if err != nil {
		return bee.VoiceOutput{}, err
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return bee.VoiceOutput{}, errors.New("TTS response is not bounded signed-16 PCM")
	}
	durationMS := int64(len(pcm) / 2 * 1000 / 24000)
	if durationMS <= 0 || durationMS > bee.MaxChannelVoiceDurationMS {
		return bee.VoiceOutput{}, errors.New("TTS response duration is invalid")
	}

	tmpDir, err := os.MkdirTemp("", ".bee-tts-")
	if err != nil {
		return bee.VoiceOutput{}, errors.New("create TTS staging directory")
	}
	defer os.RemoveAll(tmpDir)
	pcmPath := filepath.Join(tmpDir, "input.pcm")
	wavPath := filepath.Join(tmpDir, "output.wav")
	oggPath := filepath.Join(tmpDir, "output.ogg")
	if err := os.WriteFile(pcmPath, pcm, 0o600); err != nil {
		return bee.VoiceOutput{}, errors.New("stage TTS PCM")
	}
	if _, _, runErr := synthesizer.commandRunner.Run(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "s16le", "-ar", "24000", "-ac", "1", "-i", pcmPath, "-c:a", "pcm_s16le", wavPath); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, errors.New("convert TTS PCM to WAV")
	}
	wav, err := readFileBounded(wavPath, synthesizer.maxPCMBytes)
	if err != nil || len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return bee.VoiceOutput{}, errors.New("TTS WAV conversion is invalid")
	}
	if _, _, runErr := synthesizer.commandRunner.Run(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", wavPath, "-c:a", "libopus", "-b:a", "32k", "-vn", "-f", "ogg", oggPath); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, errors.New("convert TTS WAV to OGG")
	}
	ogg, err := readFileBounded(oggPath, synthesizer.maxOutputBytes)
	if err != nil {
		return bee.VoiceOutput{}, errors.New("read TTS OGG output")
	}
	if len(ogg) < 4 || string(ogg[:4]) != "OggS" {
		return bee.VoiceOutput{}, errors.New("TTS conversion did not produce OGG")
	}
	return synthesizer.artifactStore.Stage(ctx, ogg, "audio/ogg", durationMS)
}

func (synthesizer *TTSSynthesizer) requestPCM(ctx context.Context, text string, refBytes []byte) ([]byte, error) {
	var lastConflict bool
	for attempt := 0; attempt <= synthesizer.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var pcm []byte
		err := withTTSRequestLock(ctx, synthesizer.lockPath, synthesizer.lockWait, synthesizer.lockStale, func() error {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("ref_text", synthesizer.refText); err != nil {
				return errors.New("encode TTS reference text")
			}
			if err := writer.WriteField("text", text); err != nil {
				return errors.New("encode TTS text")
			}
			if err := writer.WriteField("instruction", synthesizer.instruction); err != nil {
				return errors.New("encode TTS instruction")
			}
			if err := writer.WriteField("cfg_scale", strconv.FormatFloat(synthesizer.cfgScale, 'f', -1, 64)); err != nil {
				return errors.New("encode TTS cfg_scale")
			}
			if err := writer.WriteField("seed", strconv.FormatInt(synthesizer.seed, 10)); err != nil {
				return errors.New("encode TTS seed")
			}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="ref_audio"; filename=`+strconv.Quote(filepath.Base(synthesizer.refAudioPath)))
			header.Set("Content-Type", "audio/wav")
			part, err := writer.CreatePart(header)
			if err != nil {
				return errors.New("encode TTS reference audio")
			}
			if _, err := part.Write(refBytes); err != nil {
				return errors.New("write TTS reference audio")
			}
			if err := writer.Close(); err != nil || int64(body.Len()) > synthesizer.maxRequestBytes {
				return errors.New("TTS request exceeds bounded size")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, synthesizer.endpoint, &body)
			if err != nil {
				return errors.New("create TTS request")
			}
			req.Header.Set("Content-Type", writer.FormDataContentType())
			resp, err := synthesizer.httpClient.Do(req)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return errors.New("TTS HTTP request failed")
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusConflict {
				lastConflict = true
				_, _ = providerReadBounded(resp.Body, 4<<10)
				return nil
			}
			data, err := providerReadBounded(resp.Body, synthesizer.maxPCMBytes)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return errors.New("read TTS PCM response")
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("TTS endpoint returned HTTP status %d", resp.StatusCode)
			}
			pcm = data
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !lastConflict {
			return pcm, nil
		}
		lastConflict = false
		if attempt >= synthesizer.maxRetries {
			return nil, errors.New("TTS endpoint returned HTTP status 409")
		}
		if err := sleepContext(ctx, synthesizer.retryBase*time.Duration(1<<attempt)); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("TTS endpoint returned HTTP status 409")
}

func validatePrivateAudioPath(path string, maxBytes int64) error {
	if strings.TrimSpace(path) == "" || len(path) > MaxSessionRootPathByte || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return errors.New("reference audio path is required and bounded")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > maxBytes {
		return errors.New("reference audio path must be a private bounded file")
	}
	return nil
}

func readFileBounded(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return providerReadBounded(file, maxBytes)
}

func providerReadBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("bounded read limit is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("bounded read exceeds limit")
	}
	return data, nil
}
