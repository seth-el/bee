package telegramcompat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seth-el/bee"
	"github.com/seth-el/bee/adapters/telegram"
)

// Options customizes the reusable Telegram poller composition for the
// separate cmd/bee-telegram compatibility executable. Telegram is not part of
// the web-only gateway Service lifecycle.
type Options struct {
	NoRouteBindings bool
	// Logf receives bounded operational lines (never message content).
	Logf func(format string, args ...any)
}

// voiceRuntime is the composed voice pipeline when voice is enabled.
type voiceRuntime struct {
	inputs     *telegram.VoiceArtifactStore
	recognizer telegram.ArtifactSpeechRecognizer
	replies    *telegram.VoiceReplies
}

// NewPoller composes a Telegram adapter around an existing Bee Application.
// It never creates another assistant or session store, and is intentionally
// kept outside the web-only gateway package.
func NewPoller(app *bee.Application, config telegram.Config, options Options) (*telegram.Poller, error) {
	if app == nil {
		return nil, errors.New("telegram poller application is required")
	}
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	token, err := config.Token()
	if err != nil {
		return nil, err
	}
	client, err := telegram.NewClient(config.APIBaseURL, token, telegram.NewHTTPClient())
	if err != nil {
		return nil, err
	}
	channelOptions := telegram.ChannelOptions{Markdown: !config.PlainText, TableWidth: config.TableWidth}
	var voice *voiceRuntime
	if config.VoiceEnabled() {
		voice, err = composeVoice(app.Config, config, &channelOptions, logf)
		if err != nil {
			return nil, fmt.Errorf("telegram voice: %w", err)
		}
	}
	channel, err := telegram.NewTelegramChannel(client, config, channelOptions)
	if err != nil {
		return nil, err
	}
	attachments, err := attachmentStore()
	if err != nil {
		return nil, fmt.Errorf("telegram attachments: %w", err)
	}
	media := &telegram.MediaHandler{Config: config.Normalized(), Client: client, Channel: channel, Attachments: attachments}
	checkpoint, err := telegram.NewCheckpointStore(config.CheckpointPath)
	if err != nil {
		return nil, err
	}
	var bindings bee.RouteBindingStore
	if !options.NoRouteBindings {
		bindings, err = bee.NewLocalRouteBindingStore(config.RouteBindingsFile())
		if err != nil {
			return nil, err
		}
	}
	router, err := bee.NewChannelRouter(app, []bee.Channel{channel}, bee.ChannelRouterOptions{
		RouteBindings:       bindings,
		Authorizer:          config.Policy(),
		SessionTitle:        sessionTitle,
		UnsupportedCommands: telegram.UnsupportedChatCommandsFor(voice != nil),
		CommandHelp:         telegram.ChatCommandHelp(voice != nil),
		ReplyOnFailure:      true,
		AttachmentReader:    media.ReadAttachment,
	})
	if err != nil {
		return nil, err
	}
	handler := bee.ChannelHandlerFunc(func(ctx context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		message.Text = telegram.NormalizeChatCommand(message.Text)
		if voice != nil {
			name, args := voiceCommand(message.Text)
			switch name {
			case "say":
				if args == "" {
					return bee.ChannelRouteResult{}, channel.SendText(ctx, message, "Usage: /say <text to read aloud>")
				}
				if err := channel.SendSpoken(ctx, message, args); err != nil {
					if ctx.Err() != nil {
						return bee.ChannelRouteResult{}, ctx.Err()
					}
					return bee.ChannelRouteResult{}, channel.SendText(ctx, message, "Couldn't speak that: "+err.Error())
				}
				return bee.ChannelRouteResult{}, nil
			case "speak":
				if args == "" {
					return bee.ChannelRouteResult{}, channel.SendText(ctx, message, "Usage: /speak <what you want answered as a voice note>")
				}
				voice.replies.Mark(message.MessageID)
				message.Text = args
			}
		}
		return router.Handle(ctx, message)
	})
	poller, err := telegram.NewPoller(client, config, checkpoint, handler)
	if err != nil {
		return nil, err
	}
	poller.ShowTyping = true
	poller.Logf = logf
	media.Handler = handler
	if voice != nil {
		media.Voice = &telegram.VoiceAwareHandler{
			Config:       config.Normalized(),
			Handler:      handler,
			Client:       client,
			Store:        voice.inputs,
			Recognizer:   voice.recognizer,
			Channel:      channel,
			VoiceReplies: voice.replies,
			Logf:         logf,
		}
	}
	poller.UpdateHandler = media
	return poller, nil
}

// attachmentStore stages inbound photos and files privately until the
// router has stored them in the session.
func attachmentStore() (*telegram.AttachmentArtifactStore, error) {
	state, err := bee.DefaultBeeStateRoot()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(state, "telegram-attachments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, errors.New("create attachment staging root")
	}
	return telegram.NewAttachmentArtifactStore(root)
}

// composeVoice resolves the adapter voice settings, filling in the default
// speaking style, and builds the recognizer and synthesizer.
func composeVoice(beeConfig bee.Config, config telegram.Config, channelOptions *telegram.ChannelOptions, logf func(string, ...any)) (*voiceRuntime, error) {
	settings := *config.Voice
	if settings.Instruction == "" {
		settings.Instruction = telegram.DefaultVoiceInstruction
	}
	if settings.CfgScale == 0 {
		settings.CfgScale = telegram.DefaultVoiceCfgScale
	}
	if settings.TranscribeURL == "" || settings.SpeechURL == "" || settings.VoiceID == "" {
		return nil, errors.New("set voice.transcribe_url, voice.speech_url and voice.voice_id")
	}
	root := settings.ArtifactRoot
	if root == "" {
		state, err := bee.DefaultBeeStateRoot()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(state, "telegram-voice")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, errors.New("create voice artifact root")
	}
	inputs, err := telegram.NewVoiceArtifactStore(filepath.Join(root, "in"))
	if err != nil {
		return nil, err
	}
	outputs, err := telegram.NewVoiceOutputArtifactStore(filepath.Join(root, "out"))
	if err != nil {
		return nil, err
	}
	recognizer, err := telegram.NewWhisperRecognizer(settings.TranscribeURL)
	if err != nil {
		return nil, err
	}
	synthesizer, err := telegram.NewTTSVoiceSynthesizer(settings.SpeechURL, settings.VoiceID, settings.Instruction, settings.CfgScale, outputs)
	if err != nil {
		return nil, err
	}
	replies := telegram.NewVoiceReplies()
	channelOptions.VoiceSynthesizer = synthesizer
	channelOptions.VoiceOutputStore = outputs
	channelOptions.SpokenTextChars = settings.MaxSpokenChars
	channelOptions.VoiceErrorLog = func(line string) { logf("%s", line) }
	switch settings.ReplyWithVoice {
	case "always":
		channelOptions.VoiceForAll = true
	case "never":
	default:
		channelOptions.VoiceReplies = replies
	}
	return &voiceRuntime{inputs: inputs, recognizer: recognizer, replies: replies}, nil
}

// voiceCommand recognizes /speak and /say.
func voiceCommand(text string) (string, string) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", ""
	}
	name, args := trimmed, ""
	if index := strings.IndexAny(trimmed, " \t\r\n"); index >= 0 {
		name, args = trimmed[:index], strings.TrimSpace(trimmed[index:])
	}
	name = strings.ToLower(strings.TrimPrefix(name, "/"))
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	switch name {
	case "speak", "say":
		return name, args
	}
	return "", ""
}

// sessionTitle names Telegram sessions so they read clearly next to TUI and
// HTTP sessions in shared listings.
func sessionTitle(bee.InboundMessage) string {
	return "Telegram · " + time.Now().Format("Jan 2")
}
