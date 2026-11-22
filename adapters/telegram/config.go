package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const (
	DefaultChannelID       = bee.ChannelID("telegram")
	DefaultAPIBaseURL      = "https://api.telegram.org"
	DefaultPollTimeoutSec  = 25
	DefaultPollLimit       = 100
	MaxTokenBytes          = 512
	MaxConfigBytes         = 1 << 20
	MaxAccountIDBytes      = bee.MaxChannelIDLength
	MaxAPIBaseURLBytes     = 512
	MaxAllowlistEntries    = 256
	MaxPollTimeoutSeconds  = 300
	MaxPollLimit           = 100
	MaxCheckpointOffset    = int64(1<<62 - 1)
	MaxSessionRootPathByte = 4096
)

type Config struct {
	ChannelID           bee.ChannelID `json:"channel_id"`
	AccountID           bee.AccountID `json:"account_id"`
	ExpectedBotID       int64         `json:"expected_bot_id"`
	ExpectedBotUsername string        `json:"expected_bot_username"`
	TokenFile           string        `json:"token_file"`
	APIBaseURL          string        `json:"api_base_url"`
	PollTimeoutSeconds  int           `json:"poll_timeout_seconds"`
	PollLimit           int           `json:"poll_limit"`
	CheckpointPath      string        `json:"checkpoint_path"`
	SessionRoot         string        `json:"session_root"`
	// RouteBindingsPath stores which session each chat is in. Empty keeps
	// the original location, telegram-route-bindings.json in session_root.
	// Set it when session_root is the shared Bee session store, so the
	// adapter's state stays out of the store.
	RouteBindingsPath string  `json:"route_bindings_path,omitempty"`
	AllowedChats      []int64 `json:"allowed_chats"`
	AllowedSenders    []int64 `json:"allowed_senders"`
	// Voice turns on voice notes: incoming ones are transcribed, and replies
	// to them come back spoken as well as written. Absent means off.
	Voice *VoiceConfig `json:"voice,omitempty"`
	// NotifyChatID, when set, receives a push for each new inbox
	// notification (job paused/failed by default). It must be one of
	// allowed_chats. Zero (the default) disables push.
	NotifyChatID int64 `json:"notify_chat_id,omitempty"`
	// NotifyStates overrides which notification states are pushed
	// (succeeded, failed, paused). Empty means paused and failed.
	NotifyStates []string `json:"notify_states,omitempty"`
	// PlainText sends replies exactly as the model wrote them, without
	// converting Markdown to Telegram formatting.
	PlainText bool `json:"plain_text,omitempty"`
	// TableWidth is the widest Markdown table, in characters, shown as an
	// aligned grid; wider ones are sent as one short record per row. Zero
	// means 36, about what a phone shows in portrait without wrapping.
	TableWidth int `json:"table_width,omitempty"`
}

func DefaultConfig() Config {
	return Config{ChannelID: DefaultChannelID, APIBaseURL: DefaultAPIBaseURL, PollTimeoutSeconds: DefaultPollTimeoutSec, PollLimit: DefaultPollLimit}
}

func (c Config) Validate() error {
	if err := c.validateCore(); err != nil {
		return err
	}
	if strings.TrimSpace(c.TokenFile) == "" {
		return errors.New("token_file is required")
	}
	if err := validatePrivateFilePath("token_file", c.TokenFile); err != nil {
		return err
	}
	if err := validatePrivateFilePath("checkpoint_path", c.CheckpointPath); err != nil {
		return err
	}
	if strings.TrimSpace(c.SessionRoot) == "" {
		return errors.New("session_root is required")
	}
	if c.RouteBindingsPath != "" {
		if err := validatePrivateFilePath("route_bindings_path", c.RouteBindingsPath); err != nil {
			return err
		}
		if !filepath.IsAbs(c.RouteBindingsPath) {
			return errors.New("route_bindings_path must be absolute")
		}
	}
	if len(c.SessionRoot) > MaxSessionRootPathByte || !utf8.ValidString(c.SessionRoot) || strings.IndexByte(c.SessionRoot, 0) >= 0 || filepath.Base(c.SessionRoot) == "." || filepath.Base(c.SessionRoot) == ".." {
		return errors.New("session_root is invalid")
	}
	if info, err := os.Lstat(c.SessionRoot); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return errors.New("session_root must be an owner-only directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect session_root")
	}
	return nil
}

func (c Config) validateCore() error {
	if c.ChannelID == "" {
		c.ChannelID = DefaultChannelID
	}
	if err := c.ChannelID.Validate(); err != nil {
		return fmt.Errorf("channel_id: %w", err)
	}
	if err := c.AccountID.Validate(); err != nil {
		return fmt.Errorf("account_id: %w", err)
	}
	if c.ExpectedBotID <= 0 {
		return errors.New("expected_bot_id must be positive")
	}
	if err := validateBotUsername(c.ExpectedBotUsername); err != nil {
		return err
	}
	base := strings.TrimSpace(c.APIBaseURL)
	if base == "" {
		base = DefaultAPIBaseURL
	}
	if len(base) > MaxAPIBaseURLBytes || !utf8.ValidString(base) {
		return fmt.Errorf("api_base_url exceeds %d bytes", MaxAPIBaseURLBytes)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("api_base_url must be an absolute http(s) URL without credentials or query")
	}
	if c.PollTimeoutSeconds <= 0 || c.PollTimeoutSeconds > MaxPollTimeoutSeconds {
		return fmt.Errorf("poll_timeout_seconds must be between 1 and %d", MaxPollTimeoutSeconds)
	}
	if c.PollLimit <= 0 || c.PollLimit > MaxPollLimit {
		return fmt.Errorf("poll_limit must be between 1 and %d", MaxPollLimit)
	}
	if len(c.AllowedChats) == 0 || len(c.AllowedChats) > MaxAllowlistEntries {
		return fmt.Errorf("allowed_chats must contain 1..%d entries", MaxAllowlistEntries)
	}
	if len(c.AllowedSenders) == 0 || len(c.AllowedSenders) > MaxAllowlistEntries {
		return fmt.Errorf("allowed_senders must contain 1..%d entries", MaxAllowlistEntries)
	}
	if err := validateIDs("allowed_chats", c.AllowedChats); err != nil {
		return err
	}
	if err := validateIDs("allowed_senders", c.AllowedSenders); err != nil {
		return err
	}
	if c.NotifyChatID != 0 && !containsID(c.AllowedChats, c.NotifyChatID) {
		return errors.New("notify_chat_id must be one of allowed_chats")
	}
	if len(c.NotifyStates) > 0 && c.NotifyChatID == 0 {
		return errors.New("notify_states requires notify_chat_id")
	}
	if c.TableWidth < 0 || c.TableWidth > 200 {
		return errors.New("table_width must be between 0 and 200")
	}
	if err := c.Voice.validate(); err != nil {
		return err
	}
	if err := bee.ValidateNotificationPushStates(c.NotifyStates); err != nil {
		return fmt.Errorf("notify_states: %w", err)
	}
	return nil
}

// Default speaking style for synthesized replies when voice.instruction and
// voice.cfg_scale are not set.
const (
	DefaultVoiceInstruction = "Keep the same speaker identity. Speak naturally and conversationally, with subtle variation in pace, pitch, and emphasis. Avoid an announcer-like or exaggerated delivery unless the selected style explicitly calls for heightened performance."
	DefaultVoiceCfgScale    = 1.6
)

// VoiceConfig configures the voice services: an OpenAI-compatible
// transcription endpoint and a speech endpoint. Transcription and speech URLs
// and the voice ID are required when voice is enabled.
type VoiceConfig struct {
	Enabled       bool    `json:"enabled"`
	TranscribeURL string  `json:"transcribe_url,omitempty"`
	SpeechURL     string  `json:"speech_url,omitempty"`
	VoiceID       string  `json:"voice_id,omitempty"`
	Instruction   string  `json:"instruction,omitempty"`
	CfgScale      float64 `json:"cfg_scale,omitempty"`
	// ReplyWithVoice is "voice_notes" (default: speak replies to voice notes
	// and /speak), "always", or "never".
	ReplyWithVoice string `json:"reply_with_voice,omitempty"`
	// MaxSpokenChars bounds how much of a reply is read aloud (default and
	// maximum 10000, about 11 minutes).
	MaxSpokenChars int `json:"max_spoken_chars,omitempty"`
	// ArtifactRoot holds transient audio files (default
	// <bee state>/telegram-voice).
	ArtifactRoot string `json:"artifact_root,omitempty"`
}

func (v *VoiceConfig) validate() error {
	if v == nil {
		return nil
	}
	switch v.ReplyWithVoice {
	case "", "voice_notes", "always", "never":
	default:
		return errors.New(`voice.reply_with_voice must be "voice_notes", "always", or "never"`)
	}
	if v.MaxSpokenChars < 0 || v.MaxSpokenChars > 10000 {
		return errors.New("voice.max_spoken_chars must be between 0 and 10000")
	}
	if v.CfgScale < 0 || v.CfgScale > 10 {
		return errors.New("voice.cfg_scale must be between 0 and 10")
	}
	if v.ArtifactRoot != "" && !filepath.IsAbs(v.ArtifactRoot) {
		return errors.New("voice.artifact_root must be absolute")
	}
	return nil
}

// VoiceEnabled reports whether voice notes are on.
func (c Config) VoiceEnabled() bool { return c.Voice != nil && c.Voice.Enabled }

func containsID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func validateChannelConfig(c Config) error { return c.validateCore() }

// RouteBindingsFile is the chat-to-session binding file.
func (c Config) RouteBindingsFile() string {
	if strings.TrimSpace(c.RouteBindingsPath) != "" {
		return filepath.Clean(c.RouteBindingsPath)
	}
	return filepath.Join(c.SessionRoot, "telegram-route-bindings.json")
}

func validatePrivateFilePath(name, path string) error {
	if strings.TrimSpace(path) == "" || len(path) > MaxSessionRootPathByte || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%s is required and must be a bounded path", name)
	}
	if filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return fmt.Errorf("%s must name a file", name)
	}
	return nil
}

func validateBotUsername(username string) error {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 || strings.HasPrefix(username, "@") {
		return errors.New("expected_bot_username is invalid")
	}
	for _, char := range username {
		if !(char >= 'A' && char <= 'Z') && !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' {
			return errors.New("expected_bot_username is invalid")
		}
	}
	return nil
}

func validateIDs(name string, ids []int64) error {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return fmt.Errorf("%s cannot contain zero", name)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%s contains duplicate id", name)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, errors.New("telegram config path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, fmt.Errorf("inspect telegram config: %w", err)
	}
	if info.IsDir() || info.Size() > MaxConfigBytes {
		return Config{}, errors.New("telegram config is invalid or too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read telegram config: %w", err)
	}
	config := DefaultConfig()
	if err := strictJSON(data, &config); err != nil {
		return Config{}, fmt.Errorf("parse telegram config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate telegram config: %w", err)
	}
	return config, nil
}

func strictJSON(data []byte, value any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}

func (c Config) Token() (string, error) {
	if err := validatePrivateFilePath("token_file", c.TokenFile); err != nil {
		return "", err
	}
	info, err := os.Lstat(c.TokenFile)
	if err != nil {
		return "", errors.New("read telegram token file")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return "", errors.New("telegram token file must be a regular owner-only file")
	}
	if info.Size() <= 0 || info.Size() > MaxTokenBytes {
		return "", errors.New("telegram token file has an invalid size")
	}
	data, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", errors.New("read telegram token file")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > MaxTokenBytes || !utf8.ValidString(token) || strings.ContainsAny(token, "\r\n \t/?#%") {
		return "", errors.New("telegram token file contains an invalid token")
	}
	return token, nil
}

func (c Config) AllowsChat(id int64) bool   { return exactID(c.AllowedChats, id) }
func (c Config) AllowsSender(id int64) bool { return exactID(c.AllowedSenders, id) }

func (c Config) Policy() bee.ChannelPolicy {
	config := c.Normalized()
	policy := bee.ChannelPolicy{MaxTextBytes: bee.MaxChannelTextBytes, MaxAttachments: bee.MaxChannelAttachments, MaxMetadataBytes: bee.MaxChannelMetadataBytes}
	for _, id := range config.AllowedChats {
		policy.AllowedConversations = append(policy.AllowedConversations, bee.ConversationID(opaqueChatID(id)))
	}
	for _, id := range config.AllowedSenders {
		policy.AllowedSenders = append(policy.AllowedSenders, bee.SenderID(opaqueSenderID(id)))
	}
	return policy
}

func exactID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func (c Config) Normalized() Config {
	copy := c
	if copy.ChannelID == "" {
		copy.ChannelID = DefaultChannelID
	}
	if copy.APIBaseURL == "" {
		copy.APIBaseURL = DefaultAPIBaseURL
	}
	if copy.PollTimeoutSeconds == 0 {
		copy.PollTimeoutSeconds = DefaultPollTimeoutSec
	}
	if copy.PollLimit == 0 {
		copy.PollLimit = DefaultPollLimit
	}
	copy.AllowedChats = append([]int64(nil), c.AllowedChats...)
	copy.AllowedSenders = append([]int64(nil), c.AllowedSenders...)
	sort.Slice(copy.AllowedChats, func(i, j int) bool { return copy.AllowedChats[i] < copy.AllowedChats[j] })
	sort.Slice(copy.AllowedSenders, func(i, j int) bool { return copy.AllowedSenders[i] < copy.AllowedSenders[j] })
	return copy
}
