package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/seth-el/bee"
)

type Normalized struct {
	Message        bee.InboundMessage
	RawAttachments []TelegramAttachment
	Ignored        bool
	Reason         string
}

func NormalizeUpdate(config Config, update Update) (Normalized, error) {
	return normalizeMessage(config, update, false)
}

// NormalizeAttachmentUpdate adds the narrow Phase 8C media set while keeping
// the default text poller unchanged. Raw provider metadata remains in the
// adapter result; callers should stage it before handing the final envelope to
// Bee.
func NormalizeAttachmentUpdate(config Config, update Update) (Normalized, error) {
	return normalizeMessage(config, update, true)
}

func normalizeMessage(config Config, update Update, allowAttachments bool) (Normalized, error) {
	if update.UpdateID < 0 || update.UpdateID > MaxCheckpointOffset {
		return Normalized{}, errors.New("telegram update id is out of bounds")
	}
	message := updateMessage(update)
	if message == nil {
		return Normalized{Ignored: true, Reason: "unsupported update"}, nil
	}
	if message.Chat.ID == 0 || message.MessageID <= 0 {
		return Normalized{Ignored: true, Reason: "malformed message"}, nil
	}
	if !config.AllowsChat(message.Chat.ID) {
		return Normalized{Ignored: true, Reason: "unauthorized chat"}, nil
	}
	if message.From == nil || message.From.ID == 0 {
		return Normalized{Ignored: true, Reason: "missing sender"}, nil
	}
	if !config.AllowsSender(message.From.ID) {
		return Normalized{Ignored: true, Reason: "unauthorized sender"}, nil
	}
	text := message.Text
	if allowAttachments && strings.TrimSpace(text) == "" {
		text = message.Caption
	}
	if len(text) > bee.MaxChannelTextBytes {
		return Normalized{Ignored: true, Reason: "message text exceeds bounded size"}, nil
	}
	rawAttachments := attachmentMetadataFromMessage(*message)
	if !allowAttachments {
		if strings.TrimSpace(text) == "" {
			return Normalized{Ignored: true, Reason: "unsupported or media-only message"}, nil
		}
		rawAttachments = nil
	}
	if strings.TrimSpace(text) == "" && len(rawAttachments) == 0 {
		return Normalized{Ignored: true, Reason: "unsupported or media-only message"}, nil
	}
	channelID := config.ChannelID
	if channelID == "" {
		channelID = DefaultChannelID
	}
	accountID := config.AccountID
	conversationID := opaqueChatID(message.Chat.ID)
	messageID := opaqueMessageID(int64(message.MessageID))
	updateID := opaqueUpdateID(update.UpdateID)
	threadID := ""
	if message.MessageThreadID > 0 {
		threadID = opaqueThreadID(int64(message.MessageThreadID))
	}
	reply := bee.ReplyMetadata{}
	if message.ReplyToMessage != nil && message.ReplyToMessage.MessageID > 0 {
		reply.ReplyToMessageID = bee.ChannelMessageID(opaqueMessageID(int64(message.ReplyToMessage.MessageID)))
	}
	if threadID != "" {
		reply.ThreadID = bee.ThreadID(threadID)
	}
	received := time.Time{}
	if message.Date > 0 {
		received = time.Unix(message.Date, 0).UTC()
	}
	inbound := bee.InboundMessage{
		ChannelID:      channelID,
		AccountID:      accountID,
		ConversationID: bee.ConversationID(conversationID),
		ThreadID:       bee.ThreadID(threadID),
		SenderID:       bee.SenderID(opaqueSenderID(message.From.ID)),
		MessageID:      bee.ChannelMessageID(messageID),
		UpdateID:       bee.ChannelUpdateID(updateID),
		Text:           text,
		Reply:          reply,
		ReceivedAt:     received,
	}
	if len(rawAttachments) > 0 {
		inbound.Attachments = make([]bee.AttachmentRef, 0, len(rawAttachments))
		for _, raw := range rawAttachments {
			inbound.Attachments = append(inbound.Attachments, attachmentRef(raw, attachmentReference(raw), raw.ContentType, raw.SizeBytes, ""))
		}
	}
	if err := inbound.Validate(); err != nil {
		return Normalized{}, fmt.Errorf("validate normalized telegram message: %w", err)
	}
	return Normalized{Message: inbound, RawAttachments: rawAttachments}, nil
}

func attachmentMetadataFromMessage(message Message) []TelegramAttachment {
	attachments := make([]TelegramAttachment, 0, 2)
	if message.Document != nil {
		attachments = append(attachments, TelegramAttachment{Kind: AttachmentKindDocument, FileID: message.Document.FileID, FileUniqueID: message.Document.FileUniqueID, Filename: message.Document.FileName, ContentType: message.Document.MimeType, SizeBytes: message.Document.FileSize})
	}
	if len(message.Photo) > 0 {
		photo := selectPhotoVariant(message.Photo)
		if photo.FileID != "" {
			attachments = append(attachments, TelegramAttachment{Kind: AttachmentKindPhoto, FileID: photo.FileID, FileUniqueID: photo.FileUniqueID, SizeBytes: photo.FileSize, Width: photo.Width, Height: photo.Height})
		}
	}
	if message.Audio != nil {
		attachments = append(attachments, TelegramAttachment{Kind: AttachmentKindAudio, FileID: message.Audio.FileID, FileUniqueID: message.Audio.FileUniqueID, Filename: "", ContentType: message.Audio.MimeType, SizeBytes: message.Audio.FileSize, DurationSec: message.Audio.Duration, Performer: message.Audio.Performer, Title: message.Audio.Title})
	}
	if message.Voice != nil {
		attachments = append(attachments, TelegramAttachment{Kind: AttachmentKindVoice, FileID: message.Voice.FileID, ContentType: message.Voice.MimeType, SizeBytes: message.Voice.FileSize, DurationSec: message.Voice.Duration})
	}
	return attachments
}

func selectPhotoVariant(photos []PhotoSize) PhotoSize {
	selected := PhotoSize{}
	for _, photo := range photos {
		if photo.FileID == "" {
			continue
		}
		if selected.FileID == "" || photo.Width > selected.Width || (photo.Width == selected.Width && photo.Height > selected.Height) || (photo.Width == selected.Width && photo.Height == selected.Height && photo.FileSize > selected.FileSize) {
			selected = photo
		}
	}
	return selected
}

// NormalizeVoiceUpdate is an explicit voice-aware seam. It intentionally does
// not change NormalizeUpdate: the default text poller continues to ignore
// voice-only updates until a caller composes a voice-aware handler.
func NormalizeVoiceUpdate(config Config, update Update) (Normalized, error) {
	if update.UpdateID < 0 || update.UpdateID > MaxCheckpointOffset {
		return Normalized{}, errors.New("telegram update id is out of bounds")
	}
	message := updateMessage(update)
	if message == nil {
		return Normalized{Ignored: true, Reason: "unsupported update"}, nil
	}
	if message.Chat.ID == 0 || message.MessageID <= 0 {
		return Normalized{Ignored: true, Reason: "malformed message"}, nil
	}
	if !config.AllowsChat(message.Chat.ID) {
		return Normalized{Ignored: true, Reason: "unauthorized chat"}, nil
	}
	if message.From == nil || message.From.ID == 0 {
		return Normalized{Ignored: true, Reason: "missing sender"}, nil
	}
	if !config.AllowsSender(message.From.ID) {
		return Normalized{Ignored: true, Reason: "unauthorized sender"}, nil
	}
	if message.Voice == nil && message.Audio == nil {
		return Normalized{Ignored: true, Reason: "unsupported or non-voice/audio message"}, nil
	}
	input, err := VoiceInputFromMessage(*message)
	if err != nil {
		return Normalized{}, err
	}
	channelID := config.ChannelID
	if channelID == "" {
		channelID = DefaultChannelID
	}
	conversationID := opaqueChatID(message.Chat.ID)
	threadID := ""
	if message.MessageThreadID > 0 {
		threadID = opaqueThreadID(int64(message.MessageThreadID))
	}
	reply := bee.ReplyMetadata{}
	if message.ReplyToMessage != nil && message.ReplyToMessage.MessageID > 0 {
		reply.ReplyToMessageID = bee.ChannelMessageID(opaqueMessageID(int64(message.ReplyToMessage.MessageID)))
	}
	if threadID != "" {
		reply.ThreadID = bee.ThreadID(threadID)
	}
	received := time.Time{}
	if message.Date > 0 {
		received = time.Unix(message.Date, 0).UTC()
	}
	inbound := bee.InboundMessage{
		ChannelID:      channelID,
		AccountID:      config.AccountID,
		ConversationID: bee.ConversationID(conversationID),
		ThreadID:       bee.ThreadID(threadID),
		SenderID:       bee.SenderID(opaqueSenderID(message.From.ID)),
		MessageID:      bee.ChannelMessageID(opaqueMessageID(int64(message.MessageID))),
		UpdateID:       bee.ChannelUpdateID(opaqueUpdateID(update.UpdateID)),
		Voice:          &input,
		Reply:          reply,
		ReceivedAt:     received,
	}
	if err := inbound.Validate(); err != nil {
		return Normalized{}, fmt.Errorf("validate normalized telegram voice: %w", err)
	}
	return Normalized{Message: inbound}, nil
}

func updateMessage(update Update) *Message {
	if update.Message != nil {
		return update.Message
	}
	if update.EditedMessage != nil {
		return update.EditedMessage
	}
	if update.ChannelPost != nil {
		return update.ChannelPost
	}
	return update.EditedChannelPost
}

func opaqueChatID(id int64) string {
	return "tg-chat-" + signedBase10(id)
}
func opaqueSenderID(id int64) string  { return "tg-user-" + unsignedBase10(id) }
func opaqueMessageID(id int64) string { return "tg-msg-" + unsignedBase10(id) }
func opaqueUpdateID(id int64) string  { return "tg-upd-" + unsignedBase10(id) }
func opaqueThreadID(id int64) string  { return "tg-thread-" + unsignedBase10(id) }

func signedBase10(id int64) string {
	if id < 0 {
		return "n" + strconv.FormatInt(-id, 10)
	}
	return "p" + strconv.FormatInt(id, 10)
}
func unsignedBase10(id int64) string {
	if id < 0 {
		return "n" + strconv.FormatInt(-id, 10)
	}
	return "p" + strconv.FormatInt(id, 10)
}

func decodeChatID(value bee.ConversationID) (int64, error) {
	text := string(value)
	if !strings.HasPrefix(text, "tg-chat-") {
		return 0, errors.New("telegram conversation id is not recognized")
	}
	return parseSigned(text[len("tg-chat-"):])
}
func decodeMessageID(value bee.ChannelMessageID) (int64, error) {
	return parseUnsignedPrefix(string(value), "tg-msg-")
}
func decodeThreadID(value bee.ThreadID) (int64, error) {
	return parseUnsignedPrefix(string(value), "tg-thread-")
}
func parseUnsignedPrefix(value, prefix string) (int64, error) {
	if !strings.HasPrefix(value, prefix) {
		return 0, errors.New("telegram opaque id is not recognized")
	}
	return parseUnsigned(value[len(prefix):], false)
}
func parseSigned(value string) (int64, error) { return parseUnsigned(value, true) }
func parseUnsigned(value string, signed bool) (int64, error) {
	if len(value) < 2 || (value[0] != 'p' && value[0] != 'n') {
		return 0, errors.New("telegram opaque id is malformed")
	}
	if value[0] == 'n' && !signed {
		return 0, errors.New("telegram opaque id must be non-negative")
	}
	number, err := strconv.ParseInt(value[1:], 10, 64)
	if err != nil || number <= 0 {
		return 0, errors.New("telegram opaque id is malformed")
	}
	if value[0] == 'n' {
		return -number, nil
	}
	return number, nil
}
