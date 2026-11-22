package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const (
	MaxTelegramTextBytes = 4096
	DefaultSendAttempts  = 3
	DefaultRetryDelay    = 100 * time.Millisecond
)

type SleepFunc func(context.Context, time.Duration) error

type ChannelOptions struct {
	Sleep                        SleepFunc
	MaxAttempts                  int
	RetryDelay                   time.Duration
	AttachmentStore              *AttachmentArtifactStore
	VoiceSynthesizer             SpeechSynthesizer
	VoiceOutputStore             *VoiceOutputArtifactStore
	SynthesizeTextToVoice        bool
	FallbackToTextOnVoiceFailure bool
	// VoiceReplies, with VoiceSynthesizer, adds a spoken copy after the text
	// of replies to marked messages (voice notes, /speak). VoiceForAll speaks
	// every reply. A failed voice note never fails the send: the text has
	// already been delivered, and VoiceErrorLog hears about it.
	VoiceReplies    *VoiceReplies
	VoiceForAll     bool
	SpokenTextChars int
	VoiceErrorLog   func(string)
	// Markdown sends text replies as Telegram HTML converted from the
	// model's Markdown (code blocks, inline code, bold, links, lists). A
	// message Telegram refuses to parse is resent as plain text.
	Markdown bool
	// TableWidth is the widest table shown as an aligned grid; wider
	// tables are sent as one record per row. Zero means DefaultTableWidth.
	TableWidth int
}

type TelegramChannel struct {
	client                       *Client
	channelID                    bee.ChannelID
	accountID                    bee.AccountID
	sleep                        SleepFunc
	maxAttempts                  int
	retryDelay                   time.Duration
	attachmentStore              *AttachmentArtifactStore
	voiceSynthesizer             SpeechSynthesizer
	voiceOutputStore             *VoiceOutputArtifactStore
	synthesizeTextToVoice        bool
	fallbackToTextOnVoiceFailure bool
	markdown                     bool
	tableWidth                   int
	voiceReplies                 *VoiceReplies
	voiceForAll                  bool
	spokenTextChars              int
	voiceErrorLog                func(string)
}

var _ bee.Channel = (*TelegramChannel)(nil)

func NewTelegramChannel(client *Client, config Config, options ChannelOptions) (*TelegramChannel, error) {
	if client == nil {
		return nil, errors.New("telegram channel client is required")
	}
	config = config.Normalized()
	if err := validateChannelConfig(config); err != nil {
		return nil, err
	}
	if options.MaxAttempts <= 0 || options.MaxAttempts > 5 {
		options.MaxAttempts = DefaultSendAttempts
	}
	if options.RetryDelay < 0 || options.RetryDelay > 10*time.Second {
		options.RetryDelay = DefaultRetryDelay
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.VoiceSynthesizer != nil && options.VoiceOutputStore == nil {
		return nil, errors.New("telegram voice synthesizer requires an output artifact store")
	}
	return &TelegramChannel{client: client, channelID: config.ChannelID, accountID: config.AccountID, sleep: options.Sleep, maxAttempts: options.MaxAttempts, retryDelay: options.RetryDelay, attachmentStore: options.AttachmentStore, voiceSynthesizer: options.VoiceSynthesizer, voiceOutputStore: options.VoiceOutputStore, synthesizeTextToVoice: options.SynthesizeTextToVoice, fallbackToTextOnVoiceFailure: options.FallbackToTextOnVoiceFailure, voiceReplies: options.VoiceReplies, voiceForAll: options.VoiceForAll, spokenTextChars: options.SpokenTextChars, voiceErrorLog: options.VoiceErrorLog, markdown: options.Markdown, tableWidth: options.TableWidth}, nil
}

func (c *TelegramChannel) ID() bee.ChannelID {
	if c == nil {
		return ""
	}
	return c.channelID
}

func (c *TelegramChannel) Send(ctx context.Context, message bee.OutboundMessage) error {
	if c == nil || c.client == nil {
		return errors.New("telegram channel is not configured")
	}
	if ctx == nil {
		return errors.New("telegram send context is nil")
	}
	if err := message.Validate(); err != nil {
		return err
	}
	if message.ChannelID != c.channelID || message.AccountID != c.accountID {
		return errors.New("telegram outbound identity does not match channel")
	}
	chatID, err := decodeChatID(message.ConversationID)
	if err != nil {
		return err
	}
	threadID := int64(0)
	if message.ThreadID != "" {
		threadID, err = decodeThreadID(message.ThreadID)
		if err != nil {
			return err
		}
	}
	replyID := int64(0)
	if message.ReplyTo != "" {
		replyID, err = decodeMessageID(message.ReplyTo)
		if err != nil {
			return err
		}
	}
	if message.Voice != nil && len(message.Attachments) > 0 {
		return errors.New("telegram outbound message cannot mix voice and general attachments")
	}
	if message.Voice != nil {
		if err := c.sendVoiceWithRetry(ctx, chatID, threadID, replyID, *message.Voice); err == nil {
			return nil
		} else if !c.fallbackToTextOnVoiceFailure || strings.TrimSpace(message.Text) == "" {
			return err
		}
	}
	if len(message.Attachments) > 0 {
		if strings.TrimSpace(message.Text) != "" {
			if err := c.sendTextChunks(ctx, chatID, threadID, replyID, message.Text); err != nil {
				return err
			}
			replyID = 0
		}
		if err := c.sendAttachmentsWithRetry(ctx, chatID, threadID, replyID, message.Attachments, ""); err != nil {
			return err
		}
		return nil
	}
	if message.Voice == nil && c.synthesizeTextToVoice && strings.TrimSpace(message.Text) != "" {
		voice, synthErr := Synthesize(ctx, c.voiceSynthesizer, message.Text)
		if synthErr == nil {
			if sendErr := c.sendVoiceWithRetry(ctx, chatID, threadID, replyID, voice); sendErr == nil {
				return nil
			} else if !c.fallbackToTextOnVoiceFailure {
				return sendErr
			}
		} else if !c.fallbackToTextOnVoiceFailure {
			return synthErr
		}
	}
	if strings.TrimSpace(message.Text) == "" {
		return errors.New("telegram outbound message has no sendable text or voice")
	}
	wantVoice := c.voiceSynthesizer != nil && (c.voiceForAll || c.voiceReplies.Take(message.ReplyTo))
	if err := c.sendTextChunks(ctx, chatID, threadID, replyID, message.Text); err != nil {
		return err
	}
	if wantVoice {
		c.sendSpokenCopy(ctx, chatID, threadID, replyID, message.Text)
	}
	return nil
}

// sendSpokenCopy follows a delivered text reply with a voice note. Failures
// are reported, never returned: the text already reached the chat.
func (c *TelegramChannel) sendSpokenCopy(ctx context.Context, chatID, threadID, replyID int64, text string) {
	spoken, ok := SpeakableText(text, c.spokenTextChars)
	if !ok {
		return
	}
	// Telegram shows "recording voice…" for about five seconds per action;
	// keep it up while a long reply is synthesized.
	_ = c.client.SendChatAction(ctx, chatID, "record_voice")
	indicatorCtx, stopIndicator := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-indicatorCtx.Done():
				return
			case <-time.After(4500 * time.Millisecond):
			}
			_ = c.client.SendChatAction(indicatorCtx, chatID, "record_voice")
		}
	}()
	voice, err := c.voiceSynthesizer.Synthesize(ctx, spoken)
	stopIndicator()
	if err == nil {
		err = c.sendVoiceWithRetry(ctx, chatID, threadID, replyID, voice)
	}
	if err != nil && c.voiceErrorLog != nil && ctx.Err() == nil {
		c.voiceErrorLog("voice reply: " + err.Error())
	}
}

// SendSpoken synthesizes text and sends it as a voice note in reply to an
// inbound message (the /say command).
func (c *TelegramChannel) SendSpoken(ctx context.Context, inbound bee.InboundMessage, text string) error {
	if c == nil || c.voiceSynthesizer == nil {
		return ErrSpeechSynthesisUnavailable
	}
	spoken, ok := SpeakableText(text, c.spokenTextChars)
	if !ok {
		return errors.New("nothing to speak")
	}
	voice, err := c.voiceSynthesizer.Synthesize(ctx, spoken)
	if err != nil {
		return err
	}
	return c.Send(ctx, bee.OutboundMessage{ChannelID: inbound.ChannelID, AccountID: inbound.AccountID, ConversationID: inbound.ConversationID, ThreadID: inbound.ThreadID, ReplyTo: inbound.MessageID, Voice: &voice})
}

// SendText sends a plain reply to an inbound message.
func (c *TelegramChannel) SendText(ctx context.Context, inbound bee.InboundMessage, text string) error {
	return c.Send(ctx, bee.OutboundMessage{ChannelID: inbound.ChannelID, AccountID: inbound.AccountID, ConversationID: inbound.ConversationID, ThreadID: inbound.ThreadID, ReplyTo: inbound.MessageID, Text: text})
}

func (c *TelegramChannel) sendTextChunks(ctx context.Context, chatID, threadID, replyID int64, text string) error {
	if c.markdown {
		return c.sendFormattedChunks(ctx, chatID, threadID, replyID, text)
	}
	chunks, err := splitUTF8(text, MaxTelegramTextBytes)
	if err != nil {
		return err
	}
	for index, chunk := range chunks {
		request := SendMessageRequest{ChatID: chatID, Text: chunk, MessageThreadID: threadID}
		if index == 0 {
			request.ReplyToMessageID = replyID
		}
		if err := c.sendWithRetry(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func (c *TelegramChannel) sendFormattedChunks(ctx context.Context, chatID, threadID, replyID int64, text string) error {
	if !utf8.ValidString(text) {
		return errors.New("text must be valid UTF-8")
	}
	for index, chunk := range FormatMarkdown(text, MarkdownOptions{MaxBytes: MaxTelegramTextBytes, TableWidth: c.tableWidth}) {
		request := SendMessageRequest{ChatID: chatID, Text: chunk.HTML, ParseMode: "HTML", MessageThreadID: threadID}
		if index == 0 {
			request.ReplyToMessageID = replyID
		}
		err := c.sendWithRetry(ctx, request)
		if err != nil && isParseRejection(err) {
			request.Text, request.ParseMode = chunk.Plain, ""
			err = c.sendWithRetry(ctx, request)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// isParseRejection reports a 400 from Telegram, which for an HTML message
// almost always means it could not parse the entities.
func isParseRejection(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.HTTPStatus == http.StatusBadRequest || apiErr.Code == http.StatusBadRequest)
}

func (c *TelegramChannel) sendAttachmentsWithRetry(ctx context.Context, chatID, threadID, replyID int64, attachments []bee.AttachmentRef, caption string) error {
	for index, attachment := range attachments {
		if err := c.sendAttachmentWithRetry(ctx, chatID, threadID, replyID, attachment, caption, index == 0); err != nil {
			return err
		}
		replyID = 0
	}
	return nil
}

func (c *TelegramChannel) sendAttachmentWithRetry(ctx context.Context, chatID, threadID, replyID int64, reference bee.AttachmentRef, caption string, first bool) error {
	if c.attachmentStore == nil {
		return errors.New("telegram outbound attachment store is not configured")
	}
	artifact, err := c.attachmentStore.Resolve(reference)
	if err != nil {
		return err
	}
	defer artifact.Cleanup()
	if c.attachmentStore.index != nil {
		_ = c.attachmentStore.index.Mark(reference.Reference, ArtifactDelivering, nil)
	}
	var last error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.sendAttachmentOnce(ctx, chatID, threadID, replyID, reference, caption, first); err == nil {
			if c.attachmentStore.index != nil {
				now := time.Now().UTC()
				_ = c.attachmentStore.index.Mark(reference.Reference, ArtifactDelivered, &now)
			}
			return nil
		} else {
			last = err
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.Temporary() || attempt+1 >= c.maxAttempts {
				break
			}
			delay := c.retryDelay
			if apiErr.RetryAfter > delay {
				delay = apiErr.RetryAfter
			}
			if err := c.sleep(ctx, delay); err != nil {
				return err
			}
		}
	}
	if c.attachmentStore.index != nil {
		_ = c.attachmentStore.index.Mark(reference.Reference, ArtifactFailed, nil)
	}
	return fmt.Errorf("telegram attachment send failed: %w", last)
}

func (c *TelegramChannel) sendAttachmentOnce(ctx context.Context, chatID, threadID, replyID int64, reference bee.AttachmentRef, caption string, first bool) error {
	if c.attachmentStore == nil {
		return errors.New("telegram outbound attachment store is not configured")
	}
	artifact, err := c.attachmentStore.Resolve(reference)
	if err != nil {
		return err
	}
	file, err := artifact.Open()
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > MaxFileBytes {
		return errors.New("telegram outbound attachment file is invalid")
	}
	contentType := strings.ToLower(reference.ContentType)
	field := "document"
	method := "document"
	if strings.HasPrefix(contentType, "audio/") {
		field, method = "audio", "audio"
	} else if strings.HasPrefix(contentType, "image/") {
		field, method = "photo", "photo"
	}
	if !first {
		caption = ""
	}
	request := SendAttachmentRequest{ChatID: chatID, Attachment: file, FieldName: field, Filename: reference.Filename, ContentType: contentType, SizeBytes: info.Size(), Caption: caption, MessageThreadID: threadID, ReplyToMessageID: replyID}
	_, err = func() (Message, error) {
		switch method {
		case "audio":
			return c.client.SendAudio(ctx, request)
		case "photo":
			return c.client.SendPhoto(ctx, request)
		default:
			return c.client.SendDocument(ctx, request)
		}
	}()
	return err
}

func (c *TelegramChannel) sendVoiceWithRetry(ctx context.Context, chatID, threadID, replyID int64, output bee.VoiceOutput) error {
	if c.voiceOutputStore == nil {
		return errors.New("telegram voice output store is not configured")
	}
	artifact, err := c.voiceOutputStore.Resolve(output)
	if err != nil {
		return err
	}
	defer artifact.Cleanup()
	file, err := artifact.Open()
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > MaxTelegramVoiceFileSize {
		return errors.New("telegram voice output file is invalid")
	}
	request := SendVoiceRequest{ChatID: chatID, Voice: file, Filename: "voice.ogg", ContentType: output.ContentType, SizeBytes: info.Size(), MessageThreadID: threadID, ReplyToMessageID: replyID}
	var last error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := c.client.SendVoice(ctx, request); err == nil {
			return nil
		} else {
			last = err
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.Temporary() || attempt+1 >= c.maxAttempts {
				break
			}
			delay := c.retryDelay
			if apiErr.RetryAfter > delay {
				delay = apiErr.RetryAfter
			}
			if err := c.sleep(ctx, delay); err != nil {
				return err
			}
			if _, err := file.Seek(0, 0); err != nil {
				return errors.New("rewind telegram voice output")
			}
		}
	}
	return fmt.Errorf("telegram voice send failed: %w", last)
}

func (c *TelegramChannel) sendWithRetry(ctx context.Context, request SendMessageRequest) error {
	var last error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := c.client.SendMessage(ctx, request)
		if err == nil {
			return nil
		}
		last = err
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Temporary() || attempt+1 >= c.maxAttempts {
			break
		}
		delay := c.retryDelay
		if apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
	return fmt.Errorf("telegram send failed: %w", last)
}

func splitUTF8(text string, maxBytes int) ([]string, error) {
	if maxBytes <= 0 || !utf8.ValidString(text) {
		return nil, errors.New("text must be valid UTF-8")
	}
	if text == "" {
		return []string{""}, nil
	}
	chunks := make([]string, 0, (len(text)/maxBytes)+1)
	for len(text) > 0 {
		if len(text) <= maxBytes {
			chunks = append(chunks, text)
			break
		}
		cut := maxBytes
		for cut > 0 && (text[cut]&0xc0) == 0x80 {
			cut--
		}
		if cut == 0 {
			return nil, errors.New("unable to split UTF-8 text")
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
