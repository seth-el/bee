package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const (
	// Telegram file identifiers are opaque provider tokens. Keep the raw wire
	// value bounded even though it is never copied into a Bee reference.
	MaxTelegramVoiceFileIDBytes = 512
	MaxTelegramVoiceDurationSec = bee.MaxChannelVoiceDurationMS / 1000
	MaxTelegramVoiceFileSize    = bee.MaxChannelAttachmentBytes
	MaxTelegramTranscriptBytes  = bee.MaxChannelTextBytes
	MaxTelegramSynthesisBytes   = bee.MaxChannelTextBytes
)

var (
	ErrSpeechRecognitionUnavailable = errors.New("telegram speech recognition provider is not configured")
	ErrSpeechSynthesisUnavailable   = errors.New("telegram speech synthesis provider is not configured")
	ErrTelegramVoiceDownload        = errors.New("telegram voice download is not implemented")
	ErrEmptyTranscript              = errors.New("telegram speech recognizer returned an empty transcript")
)

// Voice is the bounded Telegram wire representation. FileID remains adapter
// owned: it is validated here and only a one-way digest crosses into Bee.
type Voice struct {
	FileID   string `json:"file_id"`
	Duration int64  `json:"duration"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// TelegramVoice is an explicit alias for callers that prefer the provider
// name when declaring raw update fixtures.
type TelegramVoice = Voice

func (voice Voice) Validate() error {
	if strings.TrimSpace(voice.FileID) == "" {
		return errors.New("telegram voice file_id is required")
	}
	if len(voice.FileID) > MaxTelegramVoiceFileIDBytes || !utf8.ValidString(voice.FileID) || strings.TrimSpace(voice.FileID) != voice.FileID || containsUnsafeVoiceToken(voice.FileID) {
		return errors.New("telegram voice file_id is invalid or too large")
	}
	if voice.Duration <= 0 || voice.Duration > MaxTelegramVoiceDurationSec {
		return fmt.Errorf("telegram voice duration must be between 1 and %d seconds", MaxTelegramVoiceDurationSec)
	}
	if voice.MimeType != "" {
		if len(voice.MimeType) > bee.MaxChannelContentTypeBytes || !utf8.ValidString(voice.MimeType) || !strings.HasPrefix(strings.ToLower(voice.MimeType), "audio/") {
			return errors.New("telegram voice mime_type must be a bounded audio media type")
		}
		if err := (bee.VoiceInput{Reference: "telegram-voice-validation", ContentType: voice.MimeType}).Validate(); err != nil {
			return fmt.Errorf("telegram voice mime_type is invalid: %w", err)
		}
	}
	if voice.FileSize < 0 || voice.FileSize > MaxTelegramVoiceFileSize {
		return fmt.Errorf("telegram voice file_size must be between 0 and %d bytes", MaxTelegramVoiceFileSize)
	}
	return nil
}

func containsUnsafeVoiceToken(value string) bool {
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return true
	}
	return false
}

// VoiceInputFromRaw deterministically maps bounded Telegram metadata to a
// provider-neutral Bee reference. The raw Telegram file token is hashed and
// never exposed as a path, metadata value, or Bee reference.
func VoiceInputFromRaw(raw Voice) (bee.VoiceInput, error) {
	if err := raw.Validate(); err != nil {
		return bee.VoiceInput{}, err
	}
	canonical := "telegram-voice-v1\x00" + raw.FileID + "\x00" + strconv.FormatInt(raw.Duration, 10) + "\x00" + raw.MimeType + "\x00" + strconv.FormatInt(raw.FileSize, 10)
	digest := sha256.Sum256([]byte(canonical))
	voice := bee.VoiceInput{
		Reference:   "tg-voice-v1-" + hex.EncodeToString(digest[:]),
		ContentType: raw.MimeType,
		DurationMS:  int64(raw.Duration) * 1000,
	}
	if err := voice.Validate(); err != nil {
		return bee.VoiceInput{}, fmt.Errorf("validate normalized telegram voice: %w", err)
	}
	return voice, nil
}

// NormalizeVoiceInput is a descriptive alias for VoiceInputFromRaw.
func NormalizeVoiceInput(raw Voice) (bee.VoiceInput, error) {
	return VoiceInputFromRaw(raw)
}

// VoiceInputFromMessage maps a message's raw voice field without changing the
// text-only poller. A missing voice field is an explicit malformed input.
func VoiceInputFromMessage(message Message) (bee.VoiceInput, error) {
	if message.Voice != nil {
		return VoiceInputFromRaw(*message.Voice)
	}
	if message.Audio != nil {
		return VoiceInputFromRaw(Voice{FileID: message.Audio.FileID, Duration: message.Audio.Duration, MimeType: message.Audio.MimeType, FileSize: message.Audio.FileSize})
	}
	return bee.VoiceInput{}, errors.New("telegram message voice/audio is missing")
}

// SpeechRecognizer is the provider-neutral seam for an explicitly composed
// speech-to-text implementation. It receives only a Bee voice reference; the
// adapter does not download Telegram media or expose a local path.
type SpeechRecognizer interface {
	Recognize(context.Context, bee.VoiceInput) (string, error)
}

// SpeechSynthesizer is the provider-neutral seam for an explicitly composed
// text-to-speech implementation. Providers return a Bee voice reference, not
// audio bytes or a local filesystem path.
type SpeechSynthesizer interface {
	Synthesize(context.Context, string) (bee.VoiceOutput, error)
}

// Recognize validates the input and bounds provider output while preserving
// provider errors and context cancellation. A nil recognizer is intentionally
// an unavailable-provider error; no download or network fallback is attempted.
func Recognize(ctx context.Context, recognizer SpeechRecognizer, input bee.VoiceInput) (string, error) {
	if ctx == nil {
		return "", errors.New("telegram speech recognition context is nil")
	}
	if err := input.Validate(); err != nil {
		return "", fmt.Errorf("telegram speech recognition input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if recognizer == nil {
		return "", ErrSpeechRecognitionUnavailable
	}
	transcript, err := recognizer.Recognize(ctx, input)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return NormalizeTranscript(transcript)
}

// RecognizeVoice is a descriptive alias for Recognize.
func RecognizeVoice(ctx context.Context, recognizer SpeechRecognizer, input bee.VoiceInput) (string, error) {
	return Recognize(ctx, recognizer, input)
}

// NormalizeTranscript trims surrounding whitespace and rejects empty, invalid,
// NUL-containing, or oversized provider output before it enters Bee text.
func NormalizeTranscript(transcript string) (string, error) {
	if len(transcript) > MaxTelegramTranscriptBytes || !utf8.ValidString(transcript) || strings.IndexByte(transcript, 0) >= 0 {
		return "", errors.New("telegram transcript is invalid UTF-8 or exceeds bounded size")
	}
	normalized := strings.TrimSpace(transcript)
	if normalized == "" {
		return "", ErrEmptyTranscript
	}
	return normalized, nil
}

// Synthesize validates bounded input and provider output while preserving
// cancellation/errors. A nil synthesizer is an explicit no-provider result.
func Synthesize(ctx context.Context, synthesizer SpeechSynthesizer, text string) (bee.VoiceOutput, error) {
	if ctx == nil {
		return bee.VoiceOutput{}, errors.New("telegram speech synthesis context is nil")
	}
	normalized, err := NormalizeSynthesisInput(text)
	if err != nil {
		return bee.VoiceOutput{}, err
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	if synthesizer == nil {
		return bee.VoiceOutput{}, ErrSpeechSynthesisUnavailable
	}
	output, err := synthesizer.Synthesize(ctx, normalized)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return bee.VoiceOutput{}, ctxErr
		}
		return bee.VoiceOutput{}, err
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	if err := output.Validate(); err != nil {
		return bee.VoiceOutput{}, fmt.Errorf("telegram speech synthesis output: %w", err)
	}
	return output, nil
}

// SynthesizeVoice is a descriptive alias for Synthesize.
func SynthesizeVoice(ctx context.Context, synthesizer SpeechSynthesizer, text string) (bee.VoiceOutput, error) {
	return Synthesize(ctx, synthesizer, text)
}

func NormalizeSynthesisInput(text string) (string, error) {
	if len(text) > MaxTelegramSynthesisBytes || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", errors.New("telegram speech synthesis input is invalid UTF-8 or exceeds bounded size")
	}
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return "", errors.New("telegram speech synthesis input is empty")
	}
	return normalized, nil
}

// UnavailableSpeechRecognizer makes the no-provider behavior explicit when a
// composition root wants a concrete implementation instead of a nil seam.
type UnavailableSpeechRecognizer struct{}

func (UnavailableSpeechRecognizer) Recognize(context.Context, bee.VoiceInput) (string, error) {
	return "", ErrSpeechRecognitionUnavailable
}

// UnavailableSpeechSynthesizer makes the no-provider behavior explicit when a
// composition root wants a concrete implementation instead of a nil seam.
type UnavailableSpeechSynthesizer struct{}

func (UnavailableSpeechSynthesizer) Synthesize(context.Context, string) (bee.VoiceOutput, error) {
	return bee.VoiceOutput{}, ErrSpeechSynthesisUnavailable
}

// DownloadVoice is retained as a compatibility guard for the old reference-
// only seam. Callers that need media must explicitly provide a private store
// through DownloadVoiceToStore; this function never chooses a filesystem path.
func DownloadVoice(context.Context, *Client, Voice) error {
	return ErrTelegramVoiceDownload
}

// DownloadVoiceToStore stages one Telegram voice below the caller-rooted store.
func DownloadVoiceToStore(ctx context.Context, client *Client, store *VoiceArtifactStore, voice Voice) (*VoiceArtifact, error) {
	if store == nil {
		return nil, errors.New("telegram voice artifact store is required")
	}
	return store.Download(ctx, client, voice)
}
