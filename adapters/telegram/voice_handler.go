package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/seth-el/bee"
)

// UpdateHandler is an optional raw-update composition seam for Poller. The
// default Poller path remains text-only and uses NormalizeUpdate directly.
type UpdateHandler interface {
	HandleUpdate(context.Context, Update) (bee.ChannelRouteResult, error)
}

// VoiceAwareHandler transcribes voice/audio updates and then delegates the
// resulting text to the existing Bee ChannelRouter/ChannelHandler. It does
// not create or own another Bee runtime.
type VoiceAwareHandler struct {
	// Logf receives bounded operational lines (never audio or transcripts).
	Logf       func(format string, args ...any)
	Config     Config
	Handler    bee.ChannelHandler
	Client     *Client
	Store      *VoiceArtifactStore
	Recognizer ArtifactSpeechRecognizer
	// Channel, when set, receives the transcript echo ("🎙 …") before the
	// message is routed, and a reply when transcription fails (instead of
	// failing the update, which would redeliver it forever).
	Channel *TelegramChannel
	// VoiceReplies marks transcribed voice notes so their reply is also
	// spoken.
	VoiceReplies *VoiceReplies
}

type VoiceRuntimeOptions struct {
	InputStore                   *VoiceArtifactStore
	OutputStore                  *VoiceOutputArtifactStore
	Recognizer                   ArtifactSpeechRecognizer
	Synthesizer                  SpeechSynthesizer
	FallbackToTextOnVoiceFailure bool
	ChannelOptions               ChannelOptions
}

// NewVoiceAwareChannel builds only the outbound voice-capable channel. The
// caller attaches the poller later, after its Bee router exists.
func NewVoiceAwareChannel(client *Client, config Config, options VoiceRuntimeOptions) (*TelegramChannel, error) {
	if options.OutputStore == nil || options.Synthesizer == nil {
		return nil, errors.New("voice runtime requires output store and synthesizer")
	}
	channelOptions := options.ChannelOptions
	channelOptions.VoiceOutputStore = options.OutputStore
	channelOptions.VoiceSynthesizer = options.Synthesizer
	channelOptions.SynthesizeTextToVoice = true
	channelOptions.FallbackToTextOnVoiceFailure = options.FallbackToTextOnVoiceFailure
	return NewTelegramChannel(client, config, channelOptions)
}

// NewVoiceAwareTransport is the compatibility composition path. New callers
// should use NewVoiceAwareChannel plus NewVoiceAwarePoller explicitly when a
// router must own the handler.
func NewVoiceAwareTransport(client *Client, config Config, checkpoint *CheckpointStore, handler bee.ChannelHandler, options VoiceRuntimeOptions) (*TelegramChannel, *Poller, error) {
	if options.InputStore == nil || options.Recognizer == nil {
		return nil, nil, errors.New("voice runtime requires input store and recognizer")
	}
	channel, err := NewVoiceAwareChannel(client, config, options)
	if err != nil {
		return nil, nil, err
	}
	poller, err := NewVoiceAwarePoller(client, config, checkpoint, handler, options.InputStore, options.Recognizer)
	if err != nil {
		return nil, nil, err
	}
	return channel, poller, nil
}

func NewVoiceAwareHandler(config Config, handler bee.ChannelHandler, client *Client, store *VoiceArtifactStore, recognizer ArtifactSpeechRecognizer) (*VoiceAwareHandler, error) {
	if handler == nil || client == nil || store == nil || recognizer == nil {
		return nil, errors.New("voice-aware handler requires handler, client, store, and recognizer")
	}
	config = config.Normalized()
	if err := validateChannelConfig(config); err != nil {
		return nil, err
	}
	return &VoiceAwareHandler{Config: config, Handler: handler, Client: client, Store: store, Recognizer: recognizer}, nil
}

func (handler *VoiceAwareHandler) HandleUpdate(ctx context.Context, update Update) (bee.ChannelRouteResult, error) {
	if handler == nil || handler.Handler == nil || handler.Client == nil || handler.Store == nil || handler.Recognizer == nil {
		return bee.ChannelRouteResult{}, errors.New("voice-aware handler is not configured")
	}
	if ctx == nil {
		return bee.ChannelRouteResult{}, errors.New("voice-aware handler context is nil")
	}
	normalized, err := NormalizeUpdate(handler.Config, update)
	if err != nil {
		return bee.ChannelRouteResult{}, err
	}
	if !normalized.Ignored {
		return handler.Handler.Handle(ctx, normalized.Message)
	}
	voiceNormalized, err := NormalizeVoiceUpdate(handler.Config, update)
	if err != nil {
		return bee.ChannelRouteResult{}, err
	}
	if voiceNormalized.Ignored {
		return bee.ChannelRouteResult{}, nil
	}
	message := voiceNormalized.Message
	raw := updateMessage(update)
	if raw == nil || (raw.Voice == nil && raw.Audio == nil) {
		return bee.ChannelRouteResult{}, errors.New("voice-aware update lost voice/audio metadata")
	}
	rawVoice := Voice{}
	if raw.Voice != nil {
		rawVoice = *raw.Voice
	} else {
		rawVoice = Voice{FileID: raw.Audio.FileID, Duration: raw.Audio.Duration, MimeType: raw.Audio.MimeType, FileSize: raw.Audio.FileSize}
	}
	transcript, err := TranscribeVoice(ctx, handler.Client, handler.Store, handler.Recognizer, rawVoice)
	if err != nil {
		if handler.Logf != nil && ctx.Err() == nil {
			// Error texts on this path are fixed, bounded strings: no token,
			// URL, audio or transcript.
			handler.Logf("voice in: transcription failed (duration %ds, %d bytes, %s): %v", rawVoice.Duration, rawVoice.FileSize, rawVoice.MimeType, err)
		}
		if handler.Channel != nil && ctx.Err() == nil {
			return bee.ChannelRouteResult{}, handler.Channel.SendText(ctx, message, "Couldn't transcribe that voice note: "+err.Error()+". Try again, or type it.")
		}
		return bee.ChannelRouteResult{}, fmt.Errorf("transcribe Telegram voice: %w", err)
	}
	message.Text = transcript
	if err := message.Validate(); err != nil {
		return bee.ChannelRouteResult{}, err
	}
	if handler.Channel != nil {
		if err := handler.Channel.SendText(ctx, message, "🎙 "+transcript); err != nil {
			return bee.ChannelRouteResult{}, err
		}
	}
	handler.VoiceReplies.Mark(message.MessageID)
	return handler.Handler.Handle(ctx, message)
}

// NewVoiceAwarePoller composes voice handling into the existing foreground
// poller without changing the default text-only constructor or adding a Bee
// runtime.
func NewVoiceAwarePoller(client *Client, config Config, checkpoint *CheckpointStore, handler bee.ChannelHandler, store *VoiceArtifactStore, recognizer ArtifactSpeechRecognizer) (*Poller, error) {
	poller, err := NewPoller(client, config, checkpoint, handler)
	if err != nil {
		return nil, err
	}
	voiceHandler, err := NewVoiceAwareHandler(config, handler, client, store, recognizer)
	if err != nil {
		return nil, err
	}
	poller.UpdateHandler = voiceHandler
	return poller, nil
}
