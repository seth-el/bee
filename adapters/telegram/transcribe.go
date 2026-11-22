package telegram

import (
	"context"
	"errors"
	"fmt"
)

// ArtifactSpeechRecognizer is the explicit adapter seam for a provider that
// needs staged media. The artifact path and bytes remain outside Bee.
type ArtifactSpeechRecognizer interface {
	RecognizeArtifact(context.Context, *VoiceArtifact) (string, error)
}

type ArtifactSpeechRecognizerFunc func(context.Context, *VoiceArtifact) (string, error)

func (fn ArtifactSpeechRecognizerFunc) RecognizeArtifact(ctx context.Context, artifact *VoiceArtifact) (string, error) {
	return fn(ctx, artifact)
}

// TranscribeVoice downloads one bounded Telegram voice into an owner-only
// temporary artifact, invokes the explicitly composed recognizer, normalizes
// its output, and removes every artifact on every exit path.
func TranscribeVoice(ctx context.Context, client *Client, store *VoiceArtifactStore, recognizer ArtifactSpeechRecognizer, raw Voice) (transcript string, err error) {
	if ctx == nil {
		return "", errors.New("telegram voice transcription context is nil")
	}
	if client == nil {
		return "", errors.New("telegram voice transcription client is required")
	}
	if store == nil {
		return "", errors.New("telegram voice transcription store is required")
	}
	if recognizer == nil {
		return "", ErrSpeechRecognitionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	artifact, err := store.Download(ctx, client, raw)
	if err != nil {
		return "", err
	}
	defer func() {
		cleanupErr := artifact.Cleanup()
		if err == nil && cleanupErr != nil {
			err = cleanupErr
			transcript = ""
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	transcript, err = recognizer.RecognizeArtifact(ctx, artifact)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	transcript, err = NormalizeTranscript(transcript)
	if err != nil {
		return "", fmt.Errorf("normalize telegram voice transcript: %w", err)
	}
	return transcript, nil
}

// TranscribeTelegramVoice is an argument-order alias useful at composition
// sites that place the raw provider value before the recognizer.
func TranscribeTelegramVoice(ctx context.Context, client *Client, store *VoiceArtifactStore, raw Voice, recognizer ArtifactSpeechRecognizer) (string, error) {
	return TranscribeVoice(ctx, client, store, recognizer, raw)
}

// ArtifactRecognizerFromReference adapts the existing provider-neutral seam
// without pretending it can access media bytes. It is useful only for
// recognizers that already have an out-of-band artifact lookup by reference.
type ArtifactRecognizerFromReference struct {
	Recognizer SpeechRecognizer
}

func (adapter ArtifactRecognizerFromReference) RecognizeArtifact(ctx context.Context, artifact *VoiceArtifact) (string, error) {
	if adapter.Recognizer == nil {
		return "", ErrSpeechRecognitionUnavailable
	}
	if artifact == nil {
		return "", errors.New("voice artifact is required")
	}
	input := artifact.Input()
	if err := input.Validate(); err != nil {
		return "", err
	}
	return adapter.Recognizer.Recognize(ctx, input)
}
