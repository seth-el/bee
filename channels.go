package bee

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Phase 8A defines provider-neutral channel contracts only. A live transport
// owns polling/webhooks, credentials, and provider-specific conversion; none
// of those concerns cross this package boundary.
const (
	MaxChannelIDLength           = 64
	MaxChannelMessageIDLength    = 128
	MaxChannelTextBytes          = 64 << 10
	MaxChannelMetadataEntries    = 32
	MaxChannelMetadataKeyBytes   = 64
	MaxChannelMetadataValueBytes = 1024
	MaxChannelMetadataBytes      = 16 << 10
	MaxChannelAttachments        = 16
	MaxChannelFilenameBytes      = 255
	MaxChannelContentTypeBytes   = 128
	MaxChannelReferenceBytes     = 512
	MaxChannelAttachmentBytes    = 128 << 20
	MaxChannelVoiceDurationMS    = 24 * 60 * 60 * 1000
	MaxChannelRouteKeyBytes      = 1024
	MaxChannelRunEvents          = 256
	MaxChannelRoutes             = 4096
)

var channelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
var channelContentTypePattern = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$`)

// Channel/account/conversation/thread identifiers are deliberately opaque
// strings. They are validated as safe components and never interpreted as
// filesystem paths or provider credentials.
type ChannelID string
type AccountID string
type ConversationID string
type ThreadID string
type SenderID string
type ChannelMessageID string
type ChannelUpdateID string

func (id ChannelID) Validate() error {
	return validateChannelIdentifier("channel id", string(id), MaxChannelIDLength, true)
}
func (id AccountID) Validate() error {
	return validateChannelIdentifier("account id", string(id), MaxChannelIDLength, true)
}
func (id ConversationID) Validate() error {
	return validateChannelIdentifier("conversation id", string(id), MaxChannelIDLength, true)
}
func (id ThreadID) Validate() error {
	return validateChannelIdentifier("thread id", string(id), MaxChannelIDLength, false)
}
func (id SenderID) Validate() error {
	return validateChannelIdentifier("sender id", string(id), MaxChannelIDLength, false)
}
func (id ChannelMessageID) Validate() error {
	return validateChannelIdentifier("message id", string(id), MaxChannelMessageIDLength, true)
}
func (id ChannelUpdateID) Validate() error {
	return validateChannelIdentifier("update id", string(id), MaxChannelMessageIDLength, false)
}

func validateChannelIdentifier(name, value string, max int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	if !utf8.ValidString(value) || !channelIDPattern.MatchString(value) {
		return fmt.Errorf("%s contains unsafe characters", name)
	}
	return nil
}

// ChannelMetadata is bounded, non-secret metadata supplied by a transport or
// application. It is not a credential bag and rejects secret-shaped keys.
type ChannelMetadata map[string]string

func (metadata ChannelMetadata) Validate() error {
	if len(metadata) > MaxChannelMetadataEntries {
		return fmt.Errorf("channel metadata has more than %d entries", MaxChannelMetadataEntries)
	}
	total := 0
	keys := make([]string, 0, len(metadata))
	for key, value := range metadata {
		if err := validateMetadataKey(key); err != nil {
			return err
		}
		if len(value) > MaxChannelMetadataValueBytes {
			return fmt.Errorf("channel metadata value %q exceeds %d bytes", key, MaxChannelMetadataValueBytes)
		}
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || channelLooksLikeSecret(value) {
			return fmt.Errorf("channel metadata value %q is invalid or secret-shaped", key)
		}
		total += len(key) + len(value)
		keys = append(keys, key)
	}
	if total > MaxChannelMetadataBytes {
		return fmt.Errorf("channel metadata exceeds %d bytes", MaxChannelMetadataBytes)
	}
	sort.Strings(keys)
	return nil
}

func validateMetadataKey(key string) error {
	if key == "" || len(key) > MaxChannelMetadataKeyBytes || !utf8.ValidString(key) {
		return errors.New("channel metadata key is invalid")
	}
	if !channelIDPattern.MatchString(key) || looksSecretKey(key) {
		return fmt.Errorf("channel metadata key %q is unsafe or secret-shaped", key)
	}
	return nil
}

func looksSecretKey(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"token", "secret", "password", "passwd", "credential", "authorization", "api_key", "apikey", "private_key", "access_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func channelLooksLikeSecret(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") || strings.HasPrefix(lower, "sk-") || strings.Contains(lower, "authorization=") || strings.Contains(lower, "token=")
}

func cloneChannelMetadata(metadata ChannelMetadata) ChannelMetadata {
	if metadata == nil {
		return nil
	}
	copy := make(ChannelMetadata, len(metadata))
	for key, value := range metadata {
		copy[key] = value
	}
	return copy
}

// AttachmentRef is a reference to an attachment owned by a channel adapter.
// Bee never treats Reference as a local path and never downloads it in Phase 8A.
type AttachmentRef struct {
	Reference   string          `json:"reference,omitempty"`
	ID          string          `json:"id,omitempty"`
	ArtifactID  string          `json:"artifact_id,omitempty"`
	Filename    string          `json:"filename,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	MediaType   string          `json:"media_type,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Status      string          `json:"status,omitempty"`
	SizeBytes   int64           `json:"size_bytes,omitempty"`
	SHA256      string          `json:"sha256,omitempty"`
	Metadata    ChannelMetadata `json:"metadata,omitempty"`
}

func (attachment AttachmentRef) Validate() error {
	if strings.TrimSpace(attachment.Reference) == "" {
		return errors.New("attachment reference is required")
	}
	if err := validateOpaqueReference("attachment reference", attachment.Reference); err != nil {
		return err
	}
	if attachment.Filename != "" {
		if len(attachment.Filename) > MaxChannelFilenameBytes || !utf8.ValidString(attachment.Filename) || containsControl(attachment.Filename) || strings.ContainsAny(attachment.Filename, `/\\`) || attachment.Filename == "." || attachment.Filename == ".." || strings.Contains(attachment.Filename, "..") {
			return errors.New("attachment filename must be a safe basename")
		}
	}
	if attachment.ContentType != "" && (len(attachment.ContentType) > MaxChannelContentTypeBytes || !channelContentTypePattern.MatchString(attachment.ContentType)) {
		return errors.New("attachment content type is invalid")
	}
	if attachment.SizeBytes < 0 || attachment.SizeBytes > MaxChannelAttachmentBytes {
		return fmt.Errorf("attachment size must be between 0 and %d bytes", MaxChannelAttachmentBytes)
	}
	if attachment.SHA256 != "" && (len(attachment.SHA256) != 64 || !isLowerHex(attachment.SHA256)) {
		return errors.New("attachment sha256 must be 64 lowercase hexadecimal characters")
	}
	return attachment.Metadata.Validate()
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validateOpaqueReference(name, value string) error {
	if len(value) > MaxChannelReferenceBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || strings.ContainsRune(value, '\\') {
		return fmt.Errorf("%s is invalid or exceeds %d bytes", name, MaxChannelReferenceBytes)
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "~") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") || strings.HasPrefix(strings.ToLower(trimmed), "file:") || (len(trimmed) >= 2 && ((trimmed[0] >= 'A' && trimmed[0] <= 'Z') || (trimmed[0] >= 'a' && trimmed[0] <= 'z')) && trimmed[1] == ':') {
		return fmt.Errorf("%s must not be an arbitrary filesystem path", name)
	}
	parsed, parseErr := url.Parse(trimmed)
	if strings.Contains(trimmed, "/") && (parseErr != nil || parsed.Scheme == "") {
		return fmt.Errorf("%s must not be an arbitrary filesystem path", name)
	}
	if parseErr == nil && parsed.User != nil {
		return fmt.Errorf("%s must not contain URL credentials", name)
	}
	if channelLooksLikeSecret(trimmed) || looksLikeSecret(trimmed) {
		return fmt.Errorf("%s is secret-shaped", name)
	}
	return nil
}

func containsControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

// VoiceInput and VoiceOutput carry provider-neutral references only. Audio
// conversion, STT, TTS, and attachment retrieval are deferred to later phases.
type VoiceInput struct {
	Reference   string          `json:"reference"`
	ContentType string          `json:"content_type,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	Metadata    ChannelMetadata `json:"metadata,omitempty"`
}

func (voice VoiceInput) Validate() error {
	if err := validateOpaqueReference("voice input reference", voice.Reference); err != nil {
		return err
	}
	if voice.ContentType != "" && (len(voice.ContentType) > MaxChannelContentTypeBytes || !channelContentTypePattern.MatchString(voice.ContentType) || !strings.HasPrefix(strings.ToLower(voice.ContentType), "audio/")) {
		return errors.New("voice input content type must be an audio media type")
	}
	if voice.DurationMS < 0 || voice.DurationMS > MaxChannelVoiceDurationMS {
		return fmt.Errorf("voice input duration must be between 0 and %d milliseconds", MaxChannelVoiceDurationMS)
	}
	return voice.Metadata.Validate()
}

type VoiceOutput struct {
	Reference   string          `json:"reference"`
	ContentType string          `json:"content_type,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	Metadata    ChannelMetadata `json:"metadata,omitempty"`
}

func (voice VoiceOutput) Validate() error {
	if err := validateOpaqueReference("voice output reference", voice.Reference); err != nil {
		return err
	}
	if voice.ContentType != "" && (len(voice.ContentType) > MaxChannelContentTypeBytes || !channelContentTypePattern.MatchString(voice.ContentType) || !strings.HasPrefix(strings.ToLower(voice.ContentType), "audio/")) {
		return errors.New("voice output content type must be an audio media type")
	}
	if voice.DurationMS < 0 || voice.DurationMS > MaxChannelVoiceDurationMS {
		return fmt.Errorf("voice output duration must be between 0 and %d milliseconds", MaxChannelVoiceDurationMS)
	}
	return voice.Metadata.Validate()
}

// ReplyMetadata preserves provider-neutral reply/thread intent. A transport
// decides how to map this to its native reply API.
type ReplyMetadata struct {
	ReplyToMessageID ChannelMessageID `json:"reply_to_message_id,omitempty"`
	ThreadID         ThreadID         `json:"thread_id,omitempty"`
	Quote            string           `json:"quote,omitempty"`
	Metadata         ChannelMetadata  `json:"metadata,omitempty"`
}

func (reply ReplyMetadata) Validate() error {
	if err := validateChannelIdentifier("reply-to message id", string(reply.ReplyToMessageID), MaxChannelMessageIDLength, false); err != nil {
		return err
	}
	if err := reply.ThreadID.Validate(); err != nil {
		return err
	}
	if err := validateBoundedText("reply quote", reply.Quote, MaxChannelTextBytes, false); err != nil {
		return err
	}
	return reply.Metadata.Validate()
}

// InboundMessage is the normalized message envelope handed to the parent
// router by a channel adapter.
type InboundMessage struct {
	ChannelID      ChannelID        `json:"channel_id"`
	AccountID      AccountID        `json:"account_id"`
	ConversationID ConversationID   `json:"conversation_id"`
	ThreadID       ThreadID         `json:"thread_id,omitempty"`
	SenderID       SenderID         `json:"sender_id,omitempty"`
	MessageID      ChannelMessageID `json:"message_id"`
	UpdateID       ChannelUpdateID  `json:"update_id,omitempty"`
	Text           string           `json:"text,omitempty"`
	Attachments    []AttachmentRef  `json:"attachments,omitempty"`
	Voice          *VoiceInput      `json:"voice,omitempty"`
	Reply          ReplyMetadata    `json:"reply,omitempty"`
	Metadata       ChannelMetadata  `json:"metadata,omitempty"`
	ReceivedAt     time.Time        `json:"received_at,omitempty"`
}

func (message InboundMessage) Validate() error {
	if err := message.ChannelID.Validate(); err != nil {
		return err
	}
	if err := message.AccountID.Validate(); err != nil {
		return err
	}
	if err := message.ConversationID.Validate(); err != nil {
		return err
	}
	if err := message.ThreadID.Validate(); err != nil {
		return err
	}
	if err := message.SenderID.Validate(); err != nil {
		return err
	}
	if err := message.MessageID.Validate(); err != nil {
		return err
	}
	if err := message.UpdateID.Validate(); err != nil {
		return err
	}
	if err := validateBoundedText("inbound text", message.Text, MaxChannelTextBytes, false); err != nil {
		return err
	}
	if message.Text == "" && len(message.Attachments) == 0 && message.Voice == nil {
		return errors.New("inbound message must contain text, an attachment, or voice input")
	}
	if len(message.Attachments) > MaxChannelAttachments {
		return fmt.Errorf("inbound message has more than %d attachments", MaxChannelAttachments)
	}
	for index, attachment := range message.Attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("attachment %d: %w", index, err)
		}
	}
	if message.Voice != nil {
		if err := message.Voice.Validate(); err != nil {
			return err
		}
	}
	if err := message.Reply.Validate(); err != nil {
		return err
	}
	return message.Metadata.Validate()
}

// OutboundMessage is the normalized response envelope sent by a channel
// adapter. It deliberately contains no channel handle or credential.
type OutboundMessage struct {
	ChannelID      ChannelID        `json:"channel_id"`
	AccountID      AccountID        `json:"account_id"`
	ConversationID ConversationID   `json:"conversation_id"`
	ThreadID       ThreadID         `json:"thread_id,omitempty"`
	ReplyTo        ChannelMessageID `json:"reply_to,omitempty"`
	Text           string           `json:"text,omitempty"`
	Attachments    []AttachmentRef  `json:"attachments,omitempty"`
	Voice          *VoiceOutput     `json:"voice,omitempty"`
	Metadata       ChannelMetadata  `json:"metadata,omitempty"`
}

func (message OutboundMessage) Validate() error {
	if err := message.ChannelID.Validate(); err != nil {
		return err
	}
	if err := message.AccountID.Validate(); err != nil {
		return err
	}
	if err := message.ConversationID.Validate(); err != nil {
		return err
	}
	if err := message.ThreadID.Validate(); err != nil {
		return err
	}
	if err := validateChannelIdentifier("reply-to message id", string(message.ReplyTo), MaxChannelMessageIDLength, false); err != nil {
		return err
	}
	if err := validateBoundedText("outbound text", message.Text, MaxChannelTextBytes, false); err != nil {
		return err
	}
	if message.Text == "" && len(message.Attachments) == 0 && message.Voice == nil {
		return errors.New("outbound message must contain text, an attachment, or voice output")
	}
	if len(message.Attachments) > MaxChannelAttachments {
		return fmt.Errorf("outbound message has more than %d attachments", MaxChannelAttachments)
	}
	for index, attachment := range message.Attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("attachment %d: %w", index, err)
		}
	}
	if message.Voice != nil {
		if err := message.Voice.Validate(); err != nil {
			return err
		}
	}
	return message.Metadata.Validate()
}

func validateBoundedText(name, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s is invalid UTF-8 or contains NUL", name)
	}
	return nil
}

// SessionRouteKey is a deterministic, validated mapping from one external
// conversation to one Bee session. Thread identity is included when present.
type SessionRouteKey string

func NewSessionRouteKey(channel ChannelID, account AccountID, conversation ConversationID, thread ThreadID) (SessionRouteKey, error) {
	if err := channel.Validate(); err != nil {
		return "", err
	}
	if err := account.Validate(); err != nil {
		return "", err
	}
	if err := conversation.Validate(); err != nil {
		return "", err
	}
	if err := thread.Validate(); err != nil {
		return "", err
	}
	encode := func(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }
	key := SessionRouteKey("bee-route-v1." + encode(string(channel)) + "." + encode(string(account)) + "." + encode(string(conversation)) + "." + encode(string(thread)))
	if err := key.Validate(); err != nil {
		return "", err
	}
	return key, nil
}

func (key SessionRouteKey) Validate() error {
	value := string(key)
	if len(value) == 0 || len(value) > MaxChannelRouteKeyBytes || !utf8.ValidString(value) || strings.ContainsAny(value, `/\\`) {
		return errors.New("session route key is invalid")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 5 || parts[0] != "bee-route-v1" {
		return errors.New("session route key has invalid encoding")
	}
	decoded := make([]string, 4)
	for index, part := range parts[1:] {
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(value) != part {
			return errors.New("session route key has invalid component")
		}
		decoded[index] = string(value)
	}
	if err := ChannelID(decoded[0]).Validate(); err != nil {
		return errors.New("session route key has invalid channel component")
	}
	if err := AccountID(decoded[1]).Validate(); err != nil {
		return errors.New("session route key has invalid account component")
	}
	if err := ConversationID(decoded[2]).Validate(); err != nil {
		return errors.New("session route key has invalid conversation component")
	}
	if err := ThreadID(decoded[3]).Validate(); err != nil {
		return errors.New("session route key has invalid thread component")
	}
	return nil
}

func (key SessionRouteKey) String() string { return string(key) }

// Channel is the only transport capability visible to the router. It is a
// send seam, not a polling/webhook interface; credentials remain adapter-owned.
type Channel interface {
	ID() ChannelID
	Send(context.Context, OutboundMessage) error
}

// ChannelAuthorizer is parent-owned authorization. Returning an error rejects
// the inbound message before session lookup or model execution.
type ChannelAuthorizer interface {
	Authorize(context.Context, InboundMessage) error
}
type ChannelAuthorizerFunc func(context.Context, InboundMessage) error

func (fn ChannelAuthorizerFunc) Authorize(ctx context.Context, message InboundMessage) error {
	return fn(ctx, message)
}

// MessageDeduplicator atomically claims an inbound identity. false means the
// message was already processed and must not run the model a second time.
type MessageDeduplicator interface {
	Claim(context.Context, InboundMessage) (bool, error)
}

// MessageDeduplicatorReleaser optionally removes a claim after processing
// fails so a delivery can be retried.
type MessageDeduplicatorReleaser interface {
	Release(context.Context, InboundMessage) error
}

// MemoryMessageDeduplicator is a bounded-process fake/default idempotency seam.
type MemoryMessageDeduplicator struct {
	mu   sync.Mutex
	seen map[string]struct{}
	max  int
}

func NewMemoryMessageDeduplicator(max int) *MemoryMessageDeduplicator {
	if max <= 0 {
		max = 4096
	}
	return &MemoryMessageDeduplicator{seen: make(map[string]struct{}), max: max}
}
func (deduplicator *MemoryMessageDeduplicator) Claim(_ context.Context, message InboundMessage) (bool, error) {
	if deduplicator == nil {
		return false, errors.New("message deduplicator is nil")
	}
	key := inboundDeduplicationKey(message)
	deduplicator.mu.Lock()
	defer deduplicator.mu.Unlock()
	if _, exists := deduplicator.seen[key]; exists {
		return false, nil
	}
	if len(deduplicator.seen) >= deduplicator.max {
		return false, errors.New("message deduplicator capacity exceeded")
	}
	deduplicator.seen[key] = struct{}{}
	return true, nil
}
func (deduplicator *MemoryMessageDeduplicator) Release(_ context.Context, message InboundMessage) error {
	if deduplicator == nil {
		return errors.New("message deduplicator is nil")
	}
	deduplicator.mu.Lock()
	defer deduplicator.mu.Unlock()
	delete(deduplicator.seen, inboundDeduplicationKey(message))
	return nil
}
func inboundDeduplicationKey(message InboundMessage) string {
	if message.UpdateID != "" {
		return string(message.ChannelID) + "\x00" + string(message.AccountID) + "\x00" + string(message.ConversationID) + "\x00update\x00" + string(message.UpdateID)
	}
	return string(message.ChannelID) + "\x00" + string(message.AccountID) + "\x00message\x00" + string(message.ConversationID) + "\x00" + string(message.MessageID)
}

// ChannelPolicy is a bounded sender/conversation authorization policy. Empty
// allowlists mean "not restricted"; non-empty lists are exact-match lists.
type ChannelPolicy struct {
	AllowedSenders       []SenderID
	AllowedConversations []ConversationID
	MaxTextBytes         int
	MaxAttachments       int
	MaxMetadataBytes     int
}

func (policy ChannelPolicy) Authorize(_ context.Context, message InboundMessage) error {
	if err := message.Validate(); err != nil {
		return err
	}
	if len(policy.AllowedSenders) > 0 && !containsSender(policy.AllowedSenders, message.SenderID) {
		return errors.New("sender is not authorized for channel route")
	}
	if len(policy.AllowedConversations) > 0 && !containsConversation(policy.AllowedConversations, message.ConversationID) {
		return errors.New("conversation is not authorized for channel route")
	}
	if policy.MaxTextBytes > 0 && len(message.Text) > policy.MaxTextBytes {
		return fmt.Errorf("message text exceeds policy limit %d", policy.MaxTextBytes)
	}
	if policy.MaxAttachments > 0 && len(message.Attachments) > policy.MaxAttachments {
		return fmt.Errorf("message attachments exceed policy limit %d", policy.MaxAttachments)
	}
	if policy.MaxMetadataBytes > 0 && metadataBytes(message.Metadata) > policy.MaxMetadataBytes {
		return fmt.Errorf("message metadata exceeds policy limit %d", policy.MaxMetadataBytes)
	}
	return nil
}
func containsSender(values []SenderID, target SenderID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func containsConversation(values []ConversationID, target ConversationID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func metadataBytes(metadata ChannelMetadata) int {
	total := 0
	for key, value := range metadata {
		total += len(key) + len(value)
	}
	return total
}

// ChannelHandler is the normalized inbound seam used by adapters and tests.
type ChannelHandler interface {
	Handle(context.Context, InboundMessage) (ChannelRouteResult, error)
}

// AttachmentAwareHandler is an explicit opt-in for attachment-only prompts.
// Ordinary ChannelHandlers continue to require normalized text in the router.
type AttachmentAwareHandler interface {
	ChannelHandler
	HandleAttachment(context.Context, InboundMessage) (ChannelRouteResult, error)
}
type ChannelHandlerFunc func(context.Context, InboundMessage) (ChannelRouteResult, error)

func (fn ChannelHandlerFunc) Handle(ctx context.Context, message InboundMessage) (ChannelRouteResult, error) {
	return fn(ctx, message)
}

type ChannelRouterOptions struct {
	Authorizer         ChannelAuthorizer
	AttachmentHandler  AttachmentAwareHandler
	Deduplicator       MessageDeduplicator
	RouteBindings      RouteBindingStore
	MaxOutputTextBytes int
	MaxEvents          int
	MaxRoutes          int
	// SessionTitle names sessions the router creates. Empty (or an empty
	// result) keeps the "channel:<route key>" title.
	SessionTitle func(InboundMessage) string
	// UnsupportedCommands are slash commands this channel answers with a
	// short "not available here" reply instead of running them.
	UnsupportedCommands []CommandName
	// CommandHelp, when set, replaces the /help reply with a channel-specific
	// command list.
	CommandHelp string
	// AttachmentReader loads an inbound attachment's bytes so images can be
	// passed to the model the same way HTTP uploads are: validated, stored
	// in the session's attachment store, and sent as AttachmentIDs. Nil keeps
	// attachments out of runs.
	AttachmentReader func(context.Context, AttachmentRef) (ChannelFile, error)
	// ReplyOnFailure answers a failed turn in-channel and consumes the
	// message, instead of returning the error for redelivery. A busy session
	// gets a "try again" reply. Route-binding failures are always returned.
	ReplyOnFailure bool
}

type ChannelRouteResult struct {
	RouteKey  SessionRouteKey
	SessionID SessionID
	Duplicate bool
	Events    []RunEvent
	Outbound  *OutboundMessage
}

// ChannelRouter serializes messages per route, while allowing unrelated
// conversations to use separate sessions. It never passes Channel to Agent or
// Application; it only calls Application.Run and sends a bounded reply.
type ChannelRouter struct {
	Application       *Application
	channels          map[ChannelID]Channel
	attachmentHandler AttachmentAwareHandler
	authorizer        ChannelAuthorizer
	deduplicator      MessageDeduplicator
	routeBindings     RouteBindingStore
	maxOutput         int
	maxEvents         int
	maxRoutes         int
	sessionTitle      func(InboundMessage) string
	unsupported       map[CommandName]bool
	commandHelp       string
	replyOnFailure    bool
	attachmentReader  func(context.Context, AttachmentRef) (ChannelFile, error)
	mu                sync.Mutex
	routes            map[SessionRouteKey]*channelRouteState
}

type channelRouteState struct {
	mu        sync.Mutex
	loaded    bool
	sessionID SessionID
}

func NewChannelRouter(application *Application, channels []Channel, options ChannelRouterOptions) (*ChannelRouter, error) {
	if application == nil {
		return nil, errors.New("channel router application is required")
	}
	if application.Store == nil {
		return nil, errors.New("channel router requires an application session store for route isolation")
	}
	if len(channels) == 0 {
		return nil, errors.New("at least one channel is required")
	}
	registered := make(map[ChannelID]Channel, len(channels))
	for index, channel := range channels {
		if channel == nil {
			return nil, fmt.Errorf("channel %d is nil", index)
		}
		id := channel.ID()
		if err := id.Validate(); err != nil {
			return nil, err
		}
		if _, exists := registered[id]; exists {
			return nil, fmt.Errorf("duplicate channel id %q", id)
		}
		registered[id] = channel
	}
	if options.MaxOutputTextBytes <= 0 || options.MaxOutputTextBytes > MaxChannelTextBytes {
		options.MaxOutputTextBytes = MaxChannelTextBytes
	}
	if options.MaxEvents <= 0 || options.MaxEvents > MaxChannelRunEvents {
		options.MaxEvents = MaxChannelRunEvents
	}
	if options.MaxRoutes <= 0 || options.MaxRoutes > MaxChannelRoutes {
		options.MaxRoutes = MaxChannelRoutes
	}
	if options.Deduplicator == nil {
		options.Deduplicator = NewMemoryMessageDeduplicator(4096)
	}
	unsupported := make(map[CommandName]bool, len(options.UnsupportedCommands))
	for _, name := range options.UnsupportedCommands {
		unsupported[name] = true
	}
	return &ChannelRouter{Application: application, channels: registered, attachmentHandler: options.AttachmentHandler, authorizer: options.Authorizer, deduplicator: options.Deduplicator, routeBindings: options.RouteBindings, maxOutput: options.MaxOutputTextBytes, maxEvents: options.MaxEvents, maxRoutes: options.MaxRoutes, sessionTitle: options.SessionTitle, unsupported: unsupported, commandHelp: options.CommandHelp, replyOnFailure: options.ReplyOnFailure, attachmentReader: options.AttachmentReader, routes: make(map[SessionRouteKey]*channelRouteState)}, nil
}

func (router *ChannelRouter) RegisterChannel(channel Channel) error {
	if router == nil || channel == nil {
		return errors.New("router and channel are required")
	}
	id := channel.ID()
	if err := id.Validate(); err != nil {
		return err
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if _, exists := router.channels[id]; exists {
		return fmt.Errorf("channel %q is already registered", id)
	}
	router.channels[id] = channel
	return nil
}

func (router *ChannelRouter) Handle(ctx context.Context, inbound InboundMessage) (ChannelRouteResult, error) {
	if router == nil || router.Application == nil {
		return ChannelRouteResult{}, errors.New("channel router is not configured")
	}
	if err := inbound.Validate(); err != nil {
		return ChannelRouteResult{}, err
	}
	if router.authorizer != nil {
		if err := router.authorizer.Authorize(ctx, inbound); err != nil {
			return ChannelRouteResult{}, fmt.Errorf("authorize inbound message: %w", err)
		}
	}
	channel, ok := router.channel(inbound.ChannelID)
	if !ok {
		return ChannelRouteResult{}, fmt.Errorf("channel %q is not registered", inbound.ChannelID)
	}
	routeKey, err := NewSessionRouteKey(inbound.ChannelID, inbound.AccountID, inbound.ConversationID, inbound.ThreadID)
	if err != nil {
		return ChannelRouteResult{}, err
	}
	readableAttachments := len(inbound.Attachments) > 0 && router.attachmentReader != nil
	attachmentOnly := strings.TrimSpace(inbound.Text) == "" && len(inbound.Attachments) > 0 && router.attachmentHandler != nil && !readableAttachments
	if strings.TrimSpace(inbound.Text) == "" && !attachmentOnly && !readableAttachments {
		return ChannelRouteResult{}, errors.New("channel routing currently requires normalized text input or an attachment-aware handler")
	}
	claimed, err := router.deduplicator.Claim(ctx, inbound)
	if err != nil {
		return ChannelRouteResult{}, fmt.Errorf("claim inbound message: %w", err)
	}
	result := ChannelRouteResult{RouteKey: routeKey}
	if !claimed {
		result.Duplicate = true
		return result, nil
	}
	if attachmentOnly {
		attachmentResult, attachmentErr := router.attachmentHandler.HandleAttachment(ctx, inbound)
		if attachmentErr != nil {
			_ = releaseMessageClaim(ctx, router.deduplicator, inbound)
			return ChannelRouteResult{}, attachmentErr
		}
		return attachmentResult, nil
	}
	state, err := router.routeState(ctx, routeKey)
	if err != nil {
		_ = releaseMessageClaim(ctx, router.deduplicator, inbound)
		return ChannelRouteResult{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	title := "channel:" + routeKey.String()
	if router.sessionTitle != nil {
		if custom := strings.TrimSpace(router.sessionTitle(inbound)); custom != "" {
			title = custom
		}
	}
	commandName, isCommand := channelCommandName(inbound.Text)
	var responseText string
	if isCommand && router.unsupported[commandName] {
		responseText = fmt.Sprintf("/%s isn't available in this chat. Send /help for the commands that are.", commandName)
	} else if isCommand && commandName == CommandHelp && router.commandHelp != "" {
		responseText = router.commandHelp
	} else {
		request := RunRequest{Prompt: inbound.Text, SessionID: state.sessionID, Title: title, TitleProvisional: true}
		var err error
		if readableAttachments && !isCommand {
			request, err = router.withAttachments(ctx, state, routeKey, inbound, request)
		}
		var turn channelTurn
		if err == nil {
			turn, err = router.runTurn(ctx, state, routeKey, request, isCommand)
		}
		if err == nil && turn.continueRequest != nil {
			prefix := turn.text
			turn, err = router.runTurn(ctx, state, routeKey, *turn.continueRequest, false)
			if err == nil {
				turn.text = strings.TrimSpace(prefix + "\n\n" + turn.text)
			}
		}
		result.Events = turn.events
		result.SessionID = turn.sessionID
		switch {
		case err == nil:
			responseText = turn.text
			if turn.fallback != "" && strings.TrimSpace(responseText) != "" {
				responseText += "\n\n(Answered by " + turn.fallback + ": the main model server is unreachable.)"
			}
		case router.replyOnFailure && errors.Is(err, ErrSessionBusy):
			responseText = "That conversation is busy in another Bee client (the TUI or another client). Send your message again once it finishes."
		case router.replyOnFailure && ctx.Err() == nil && RunErrorKind(err) == ErrorKindModelUnreachable:
			responseText = "The model server isn't reachable right now. " + boundedCommandText(err.Error()) + "\nSend your message again once it's back."
		case router.replyOnFailure && ctx.Err() == nil && !errors.Is(err, errRouteBinding):
			responseText = "Bee couldn't answer that: " + boundedCommandText(err.Error()) + "\nSend it again to retry."
		default:
			_ = releaseMessageClaim(ctx, router.deduplicator, inbound)
			return ChannelRouteResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = releaseMessageClaim(context.Background(), router.deduplicator, inbound)
		return ChannelRouteResult{}, err
	}
	if result.SessionID == "" {
		result.SessionID = state.sessionID
	}
	if strings.TrimSpace(responseText) == "" {
		return result, nil
	}
	outbound := OutboundMessage{ChannelID: inbound.ChannelID, AccountID: inbound.AccountID, ConversationID: inbound.ConversationID, ThreadID: inbound.ThreadID, ReplyTo: inbound.MessageID, Text: responseText, Metadata: ChannelMetadata{"bee.route_key": routeKey.String()}}
	if err := outbound.Validate(); err != nil {
		_ = releaseMessageClaim(ctx, router.deduplicator, inbound)
		return ChannelRouteResult{}, err
	}
	if err := channel.Send(ctx, outbound); err != nil {
		_ = releaseMessageClaim(ctx, router.deduplicator, inbound)
		return ChannelRouteResult{}, fmt.Errorf("send channel response: %w", err)
	}
	result.Outbound = &outbound
	return result, nil
}

// ChannelFile is one inbound attachment's content, loaded by the adapter.
type ChannelFile struct {
	Filename  string
	MediaType string
	Data      []byte
}

// ChannelImageOnlyPrompt stands in for the text of an image sent without a
// caption.
const ChannelImageOnlyPrompt = "(sent an image)"

// withAttachments stores the inbound images in the route's session (creating
// the session first if the route has none yet) and adds them to the run.
// Images get the same validation as HTTP uploads. Anything that is not a
// usable image is named in the prompt so the assistant can say it was not received.
func (router *ChannelRouter) withAttachments(ctx context.Context, state *channelRouteState, routeKey SessionRouteKey, inbound InboundMessage, request RunRequest) (RunRequest, error) {
	app := router.Application
	if app.AttachmentStore == nil {
		return request, errors.New("this Bee has no attachment store, so images can't be received")
	}
	if state.sessionID == "" {
		session, err := app.Store.Create(SessionMetadata{Title: request.Title, TitleSource: provisionalTitleSource(request.Title, request.TitleProvisional)})
		if err != nil {
			return request, fmt.Errorf("create session: %w", err)
		}
		if router.routeBindings != nil {
			if err := router.routeBindings.Bind(ctx, routeKey, session.ID); err != nil {
				return request, fmt.Errorf("%w: persist route binding: %v", errRouteBinding, err)
			}
		}
		state.sessionID = session.ID
		request.SessionID = session.ID
	}
	var skipped []string
	for _, ref := range inbound.Attachments {
		file, err := router.attachmentReader(ctx, ref)
		name := strings.TrimSpace(file.Filename)
		if name == "" {
			name = strings.TrimSpace(ref.Filename)
		}
		if name == "" {
			name = "a file"
		}
		if err != nil {
			skipped = append(skipped, name+" (could not be downloaded)")
			continue
		}
		mediaType, err := validateOpenAIWebImage(ctx, file.MediaType, file.Data)
		if err != nil {
			if strings.HasPrefix(file.MediaType, "image/") {
				skipped = append(skipped, name+" ("+err.Error()+")")
			} else {
				skipped = append(skipped, name+" (only images can be passed on so far)")
			}
			continue
		}
		filename := file.Filename
		if !validOpenAIWebAttachmentFilename(filename) || !strings.Contains(filename, ".") {
			filename = "telegram-image" + imageExtension(mediaType)
		}
		attachment, err := app.AttachmentStore.Create(ctx, request.SessionID, filename, mediaType, file.Data)
		if err != nil {
			return request, fmt.Errorf("store image: %w", err)
		}
		request.AttachmentIDs = append(request.AttachmentIDs, attachment.ID)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = ChannelImageOnlyPrompt
		if len(request.AttachmentIDs) == 0 {
			request.Prompt = "(sent a file)"
		}
	}
	if len(skipped) > 0 {
		request.Prompt += "\n\n[Bee note: these attachments were not passed to you: " + strings.Join(skipped, "; ") + ".]"
	}
	return request, nil
}

func imageExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".jpg"
}

// errRouteBinding marks failures to persist a route's session binding. They
// are never answered in-channel: the message is released for redelivery.
var errRouteBinding = errors.New("route binding")

type channelTurn struct {
	events          []RunEvent
	sessionID       SessionID
	text            string
	continueRequest *RunRequest
	// fallback names the profile that answered when the main model server
	// was unreachable (G10).
	fallback string
}

// channelCommandName reports the slash command a channel message invokes, if
// any. Telegram-style "/cmd@botname" suffixes are accepted.
func channelCommandName(text string) (CommandName, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	name, _ := splitCommandInput(trimmed)
	name = strings.TrimPrefix(name, "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	if name == "" {
		return "", false
	}
	return CommandName(strings.ToLower(name)), true
}

// runTurn runs one request on a route and collects its reply. Ordinary turns
// bind a new route to the session the run creates, and refuse a different
// session. Local command turns reply with the command's text; /new and
// /resume <id> deliberately rebind the route to the chosen session.
func (router *ChannelRouter) runTurn(ctx context.Context, state *channelRouteState, routeKey SessionRouteKey, request RunRequest, isCommand bool) (channelTurn, error) {
	var turn channelTurn
	if isCommand {
		request.Prompt = stripChannelCommandMention(request.Prompt)
	}
	events, err := router.Application.Run(ctx, request)
	if err != nil {
		return turn, err
	}
	turn.events = make([]RunEvent, 0, router.maxEvents)
	var runErr error
	var routeBindingErr error
	var command *LocalCommandEvent
	for event := range events {
		if len(turn.events) < router.maxEvents {
			turn.events = append(turn.events, event)
		}
		if event.Type == RunLocalCommand && event.Command != nil {
			copy := *event.Command
			command = &copy
		}
		if !isCommand && event.Type == RunSession && event.SessionID != "" {
			if router.routeBindings != nil {
				if state.sessionID == "" {
					if err := router.routeBindings.Bind(ctx, routeKey, event.SessionID); err != nil && routeBindingErr == nil {
						routeBindingErr = fmt.Errorf("%w: persist route binding: %v", errRouteBinding, err)
					}
				} else if state.sessionID != event.SessionID && routeBindingErr == nil {
					routeBindingErr = fmt.Errorf("%w changed from session %q to %q", errRouteBinding, state.sessionID, event.SessionID)
				}
			}
			if routeBindingErr == nil {
				state.sessionID = event.SessionID
			}
			turn.sessionID = event.SessionID
		}
		if event.Type == RunProviderFallback && event.ProviderFallback != nil {
			turn.fallback = event.ProviderFallback.To
		}
		if event.Type == RunAssistantDelta {
			turn.text = boundedAppend(turn.text, event.Delta, router.maxOutput)
		}
		if event.Type == RunAssistant && event.Message != nil && turn.text == "" {
			turn.text = boundedAppend(turn.text, event.Message.Content, router.maxOutput)
		}
		if event.Type == RunEnd && event.Response != nil && turn.text == "" {
			turn.text = boundedAppend(turn.text, event.Response.Message.Content, router.maxOutput)
		}
		if runErr == nil && (event.Type == RunFailure || event.Type == RunEnd) && event.Error != "" {
			runErr = formatRunError(event)
		}
	}
	if routeBindingErr != nil {
		state.sessionID = ""
		state.loaded = false
		return turn, routeBindingErr
	}
	if command == nil {
		return turn, runErr
	}
	// A local command: its text is the reply, including errors, so a bad
	// command never turns into a failed (and redelivered) message.
	turn.text = boundedAppend("", command.Text, router.maxOutput)
	if command.Kind == commandKindError {
		return turn, nil
	}
	target := SessionID("")
	if command.NewSession != nil {
		target = command.NewSession.Session.ID
	} else if command.Resume != nil && command.Resume.Selected != nil {
		target = command.Resume.Selected.ID
	}
	if target != "" && target != state.sessionID {
		if err := router.rebind(ctx, routeKey, target); err != nil {
			return turn, fmt.Errorf("%w: switch route to session %q: %v", errRouteBinding, target, err)
		}
		state.sessionID = target
	}
	if target != "" {
		turn.sessionID = target
	}
	if command.Continue != nil && state.sessionID != "" {
		turn.continueRequest = &RunRequest{Prompt: command.Continue.Prompt, SessionID: state.sessionID, CheckpointID: command.Continue.CheckpointID, DisableCommands: true}
	}
	return turn, nil
}

func (router *ChannelRouter) rebind(ctx context.Context, key SessionRouteKey, sessionID SessionID) error {
	if router.routeBindings == nil {
		return nil
	}
	if rebinder, ok := router.routeBindings.(RouteBindingRebinder); ok {
		return rebinder.Rebind(ctx, key, sessionID)
	}
	return errors.New("this route binding store cannot switch sessions")
}

func stripChannelCommandMention(text string) string {
	trimmed := strings.TrimSpace(text)
	name, args := splitCommandInput(trimmed)
	at := strings.IndexByte(name, '@')
	if at < 0 {
		return trimmed
	}
	if strings.TrimSpace(args) == "" {
		return name[:at]
	}
	return name[:at] + " " + strings.TrimSpace(args)
}

func releaseMessageClaim(ctx context.Context, deduplicator MessageDeduplicator, message InboundMessage) error {
	releaser, ok := deduplicator.(MessageDeduplicatorReleaser)
	if !ok {
		return nil
	}
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	return releaser.Release(ctx, message)
}

func (router *ChannelRouter) channel(id ChannelID) (Channel, bool) {
	router.mu.Lock()
	defer router.mu.Unlock()
	channel, ok := router.channels[id]
	return channel, ok
}
func (router *ChannelRouter) routeState(ctx context.Context, key SessionRouteKey) (*channelRouteState, error) {
	router.mu.Lock()
	state := router.routes[key]
	if state == nil {
		if len(router.routes) >= router.maxRoutes {
			router.mu.Unlock()
			return nil, errors.New("channel router route capacity exceeded")
		}
		state = &channelRouteState{}
		router.routes[key] = state
	}
	router.mu.Unlock()

	state.mu.Lock()
	if state.loaded {
		state.mu.Unlock()
		return state, nil
	}
	if router.routeBindings != nil {
		sessionID, err := router.routeBindings.Lookup(ctx, key)
		if err != nil && !errors.Is(err, ErrRouteBindingNotFound) {
			state.mu.Unlock()
			router.mu.Lock()
			if router.routes[key] == state {
				delete(router.routes, key)
			}
			router.mu.Unlock()
			return nil, fmt.Errorf("load route binding: %w", err)
		}
		if err == nil {
			state.sessionID = sessionID
		}
	}
	state.loaded = true
	state.mu.Unlock()
	return state, nil
}

func boundedAppend(current, addition string, max int) string {
	if max <= 0 {
		return current
	}
	remaining := max - len(current)
	if remaining <= 0 {
		return current
	}
	if len(addition) <= remaining {
		return current + addition
	}
	addition = addition[:remaining]
	for len(addition) > 0 && !utf8.ValidString(addition) {
		addition = addition[:len(addition)-1]
	}
	return current + addition
}

// InMemoryChannel is a transport-free fake useful for tests and examples.
type InMemoryChannel struct {
	ChannelID ChannelID
	mu        sync.Mutex
	Sent      []OutboundMessage
}

func NewInMemoryChannel(id ChannelID) (*InMemoryChannel, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return &InMemoryChannel{ChannelID: id}, nil
}
func (channel *InMemoryChannel) ID() ChannelID {
	if channel == nil {
		return ""
	}
	return channel.ChannelID
}
func (channel *InMemoryChannel) Send(_ context.Context, message OutboundMessage) error {
	if channel == nil {
		return errors.New("in-memory channel is nil")
	}
	if err := message.Validate(); err != nil {
		return err
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	channel.Sent = append(channel.Sent, cloneOutboundMessage(message))
	return nil
}
func (channel *InMemoryChannel) Messages() []OutboundMessage {
	if channel == nil {
		return nil
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	messages := make([]OutboundMessage, len(channel.Sent))
	for index, message := range channel.Sent {
		messages[index] = cloneOutboundMessage(message)
	}
	return messages
}

func cloneAttachmentRef(attachment AttachmentRef) AttachmentRef {
	attachment.Metadata = cloneChannelMetadata(attachment.Metadata)
	return attachment
}

func cloneOutboundMessage(message OutboundMessage) OutboundMessage {
	copy := message
	copy.Metadata = cloneChannelMetadata(message.Metadata)
	if message.Attachments != nil {
		copy.Attachments = make([]AttachmentRef, len(message.Attachments))
		for index, attachment := range message.Attachments {
			copy.Attachments[index] = cloneAttachmentRef(attachment)
		}
	}
	if message.Voice != nil {
		voice := *message.Voice
		voice.Metadata = cloneChannelMetadata(message.Voice.Metadata)
		copy.Voice = &voice
	}
	return copy
}
