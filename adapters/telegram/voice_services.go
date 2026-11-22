package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seth-el/bee"
)

// The voice services: an OpenAI-compatible faster-whisper transcription
// endpoint and a TTS server with trained voice
// profiles (voice_id). These replace the older Gemma/ref-audio providers for
// the live adapter; those remain for compatibility.

const (
	DefaultWhisperMaxResponseBytes = 64 << 10
	// DefaultSpokenTextChars is how much of a reply is read aloud: the most
	// voice.max_spoken_chars allows, about 11 minutes of speech, so in
	// practice whole replies are spoken.
	DefaultSpokenTextChars = 10000
	// ttsChunkChars bounds each TTS request. A long reply is spoken as
	// several requests whose audio is joined into one voice note, so no
	// single request runs into the HTTP timeout.
	ttsChunkChars  = 1500
	maxTTSPCMBytes = 64 << 20
	ttsBusyRetries = 3
)

// WhisperRecognizer posts a staged voice note to an OpenAI-compatible
// /v1/audio/transcriptions endpoint and returns the transcript.
type WhisperRecognizer struct {
	Endpoint   string
	HTTPClient *http.Client
}

func NewWhisperRecognizer(endpoint string) (*WhisperRecognizer, error) {
	if err := providerEndpoint("speech-to-text", endpoint); err != nil {
		return nil, err
	}
	return &WhisperRecognizer{Endpoint: strings.TrimSpace(endpoint), HTTPClient: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (recognizer *WhisperRecognizer) RecognizeArtifact(ctx context.Context, artifact *VoiceArtifact) (string, error) {
	if recognizer == nil || artifact == nil {
		return "", ErrSpeechRecognitionUnavailable
	}
	file, err := artifact.Open()
	if err != nil {
		return "", errors.New("open staged voice note")
	}
	defer file.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	name := "voice.ogg"
	if strings.Contains(artifact.ContentType(), "mpeg") {
		name = "voice.mp3"
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return "", errors.New("encode voice note")
	}
	if _, err := io.Copy(part, io.LimitReader(file, MaxTelegramVoiceFileSize+1)); err != nil {
		return "", errors.New("encode voice note")
	}
	if err := writer.Close(); err != nil {
		return "", errors.New("encode voice note")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, recognizer.Endpoint, &body)
	if err != nil {
		return "", errors.New("build speech-to-text request")
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := recognizer.HTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("speech-to-text service is unreachable")
	}
	defer response.Body.Close()
	data, err := providerReadBounded(response.Body, DefaultWhisperMaxResponseBytes)
	if err != nil {
		return "", errors.New("read speech-to-text response")
	}
	if response.StatusCode == http.StatusConflict {
		return "", errors.New("speech-to-text service is busy")
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("speech-to-text failed (HTTP %d)", response.StatusCode)
	}
	var payload struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Text == nil {
		return "", errors.New("speech-to-text response has no text")
	}
	return strings.TrimSpace(*payload.Text), nil
}

// TTSVoiceSynthesizer asks TTS for one trained voice (voice_id), which
// returns signed 16-bit mono PCM, and stages it as a Telegram OGG/Opus voice
// note.
type TTSVoiceSynthesizer struct {
	Endpoint      string
	VoiceID       string
	Instruction   string
	CfgScale      float64
	HTTPClient    *http.Client
	CommandRunner CommandRunner
	Store         *VoiceOutputArtifactStore
	// Sleep is used between retries when TTS reports it is busy.
	Sleep SleepFunc
}

func NewTTSVoiceSynthesizer(endpoint, voiceID, instruction string, cfgScale float64, store *VoiceOutputArtifactStore) (*TTSVoiceSynthesizer, error) {
	if err := providerEndpoint("TTS", endpoint); err != nil {
		return nil, err
	}
	if err := validateProviderText("TTS voice_id", voiceID, 128); err != nil {
		return nil, err
	}
	if instruction == "" {
		instruction = DefaultTTSInstruction
	}
	if err := validateProviderText("TTS instruction", instruction, MaxProviderPromptBytes); err != nil {
		return nil, err
	}
	if cfgScale <= 0 || cfgScale > 10 {
		cfgScale = 1.6
	}
	if store == nil {
		return nil, errors.New("TTS synthesizer requires a voice output store")
	}
	return &TTSVoiceSynthesizer{Endpoint: strings.TrimSpace(endpoint), VoiceID: voiceID, Instruction: instruction, CfgScale: cfgScale, HTTPClient: &http.Client{Timeout: 5 * time.Minute}, CommandRunner: execCommandRunner{}, Store: store, Sleep: sleepContext}, nil
}

func (synthesizer *TTSVoiceSynthesizer) Synthesize(ctx context.Context, text string) (bee.VoiceOutput, error) {
	if synthesizer == nil || synthesizer.Store == nil {
		return bee.VoiceOutput{}, ErrSpeechSynthesisUnavailable
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return bee.VoiceOutput{}, errors.New("nothing to speak")
	}
	var pcm []byte
	sampleRate := 0
	for _, chunk := range splitSpeech(text, ttsChunkChars) {
		part, rate, err := synthesizer.requestPCM(ctx, chunk)
		if err != nil {
			return bee.VoiceOutput{}, err
		}
		if sampleRate != 0 && rate != sampleRate {
			return bee.VoiceOutput{}, errors.New("TTS changed sample rate between parts")
		}
		sampleRate = rate
		if int64(len(pcm)+len(part)) > maxTTSPCMBytes {
			return bee.VoiceOutput{}, errors.New("speech response is too large")
		}
		pcm = append(pcm, part...)
	}
	if len(pcm) < 2 || len(pcm)%2 != 0 {
		return bee.VoiceOutput{}, errors.New("TTS returned invalid PCM")
	}
	durationMS := int64(len(pcm)/2) * 1000 / int64(sampleRate)
	tmpDir, err := os.MkdirTemp("", ".bee-tts-")
	if err != nil {
		return bee.VoiceOutput{}, errors.New("create speech staging directory")
	}
	defer os.RemoveAll(tmpDir)
	pcmPath := filepath.Join(tmpDir, "speech.pcm")
	oggPath := filepath.Join(tmpDir, "speech.ogg")
	if err := os.WriteFile(pcmPath, pcm, 0o600); err != nil {
		return bee.VoiceOutput{}, errors.New("stage speech PCM")
	}
	runner := synthesizer.CommandRunner
	if runner == nil {
		runner = execCommandRunner{}
	}
	if _, _, err := runner.Run(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", "1", "-i", pcmPath, "-c:a", "libopus", "-b:a", "32k", "-vn", "-f", "ogg", oggPath); err != nil {
		if ctx.Err() != nil {
			return bee.VoiceOutput{}, ctx.Err()
		}
		return bee.VoiceOutput{}, errors.New("convert speech to OGG (is ffmpeg installed?)")
	}
	ogg, err := readFileBounded(oggPath, MaxTelegramVoiceFileSize)
	if err != nil || len(ogg) < 4 || string(ogg[:4]) != "OggS" {
		return bee.VoiceOutput{}, errors.New("speech conversion did not produce OGG")
	}
	if durationMS <= 0 {
		durationMS = 1
	}
	return synthesizer.Store.Stage(ctx, ogg, "audio/ogg", durationMS)
}

func (synthesizer *TTSVoiceSynthesizer) requestPCM(ctx context.Context, text string) ([]byte, int, error) {
	client := synthesizer.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	for attempt := 0; ; attempt++ {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, field := range [][2]string{
			{"text", text},
			{"voice_id", synthesizer.VoiceID},
			{"instruction", synthesizer.Instruction},
			{"cfg_scale", strconv.FormatFloat(synthesizer.CfgScale, 'f', -1, 64)},
			{"split_chars", "500"},
		} {
			if err := writer.WriteField(field[0], field[1]); err != nil {
				return nil, 0, errors.New("encode speech request")
			}
		}
		if err := writer.Close(); err != nil {
			return nil, 0, errors.New("encode speech request")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, synthesizer.Endpoint, &body)
		if err != nil {
			return nil, 0, errors.New("build speech request")
		}
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			return nil, 0, errors.New("speech service is unreachable")
		}
		data, readErr := providerReadBounded(response.Body, maxTTSPCMBytes)
		response.Body.Close()
		if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusServiceUnavailable {
			if attempt >= ttsBusyRetries {
				return nil, 0, errors.New("speech service is busy")
			}
			sleep := synthesizer.Sleep
			if sleep == nil {
				sleep = sleepContext
			}
			if err := sleep(ctx, time.Duration(attempt+1)*2*time.Second); err != nil {
				return nil, 0, err
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("speech synthesis failed (HTTP %d)", response.StatusCode)
		}
		if readErr != nil {
			return nil, 0, errors.New("speech response is too large")
		}
		sampleRate := 24000
		if raw := response.Header.Get("X-Sample-Rate"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 8000 && parsed <= 96000 {
				sampleRate = parsed
			}
		}
		if format := response.Header.Get("X-Sample-Format"); format != "" && format != "s16le" {
			return nil, 0, fmt.Errorf("speech service returned unsupported sample format %q", format)
		}
		return data, sampleRate, nil
	}
}

var (
	fencedCodePattern  = regexp.MustCompile("(?s)```.*?(```|$)")
	inlineCodePattern  = regexp.MustCompile("`([^`]*)`")
	markdownLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	bareURLPattern     = regexp.MustCompile(`https?://\S+`)
	markerPattern      = regexp.MustCompile(`\[[a-z_]+:[^\]\n]{1,64}\]`)
	headingPattern     = regexp.MustCompile(`(?m)^\s{0,3}(#{1,6}|>|[-*+]|\d+\.)\s+`)
	emphasisPattern    = regexp.MustCompile(`(\*\*|__|\*|_|~~)`)
	whitespacePattern  = regexp.MustCompile(`[ \t]+`)
	blankLinesPattern  = regexp.MustCompile(`\n{3,}`)
	sentenceEndPattern = regexp.MustCompile(`[.!?…](\s|$)`)
)

// splitSpeech cuts text into pieces of at most maxChars runes, preferring
// paragraph breaks, then sentence ends, then spaces, so each piece is spoken
// with natural phrasing.
func splitSpeech(text string, maxChars int) []string {
	var parts []string
	for {
		text = strings.TrimSpace(text)
		if text == "" {
			return parts
		}
		runes := []rune(text)
		if len(runes) <= maxChars {
			return append(parts, text)
		}
		window := string(runes[:maxChars])
		cut := -1
		if i := strings.LastIndex(window, "\n\n"); i > len(window)/3 {
			cut = i
		} else if ends := sentenceEndPattern.FindAllStringIndex(window, -1); len(ends) > 0 && ends[len(ends)-1][0] > len(window)/3 {
			cut = ends[len(ends)-1][0] + 1
		} else if i := strings.LastIndex(window, " "); i > len(window)/3 {
			cut = i
		} else {
			cut = len(window)
		}
		parts = append(parts, strings.TrimSpace(window[:cut]))
		text = text[cut:]
	}
}

// SpeakableText turns a chat reply into something worth reading aloud: code
// blocks are skipped, links and markdown are flattened, avatar markers like
// [emotion:smile] are removed, and long replies are cut at a sentence with a
// pointer to the text. ok is false when nothing speakable is left.
func SpeakableText(text string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = DefaultSpokenTextChars
	}
	text = fencedCodePattern.ReplaceAllString(text, " (code is in the text) ")
	text = inlineCodePattern.ReplaceAllString(text, "$1")
	text = markdownLink.ReplaceAllString(text, "$1")
	text = bareURLPattern.ReplaceAllString(text, "a link")
	text = markerPattern.ReplaceAllString(text, "")
	text = headingPattern.ReplaceAllString(text, "")
	text = emphasisPattern.ReplaceAllString(text, "")
	text = whitespacePattern.ReplaceAllString(text, " ")
	text = blankLinesPattern.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text, true
	}
	cut := string(runes[:maxChars])
	ends := sentenceEndPattern.FindAllStringIndex(cut, -1)
	if len(ends) > 0 && ends[len(ends)-1][0] > len(cut)/2 {
		cut = cut[:ends[len(ends)-1][0]+1]
	} else if space := strings.LastIndex(cut, " "); space > len(cut)/2 {
		cut = cut[:space] + "…"
	}
	return strings.TrimSpace(cut) + " The rest is in the text.", true
}

// VoiceReplies remembers which inbound messages should be answered with a
// voice note as well as text: voice notes, and /speak prompts. It is bounded;
// the oldest entries are dropped.
type VoiceReplies struct {
	mu    sync.Mutex
	order []bee.ChannelMessageID
	set   map[bee.ChannelMessageID]bool
}

const maxVoiceReplies = 256

func NewVoiceReplies() *VoiceReplies {
	return &VoiceReplies{set: map[bee.ChannelMessageID]bool{}}
}

func (replies *VoiceReplies) Mark(id bee.ChannelMessageID) {
	if replies == nil || id == "" {
		return
	}
	replies.mu.Lock()
	defer replies.mu.Unlock()
	if replies.set[id] {
		return
	}
	replies.set[id] = true
	replies.order = append(replies.order, id)
	if len(replies.order) > maxVoiceReplies {
		delete(replies.set, replies.order[0])
		replies.order = replies.order[1:]
	}
}

// Take reports whether id was marked, and forgets it.
func (replies *VoiceReplies) Take(id bee.ChannelMessageID) bool {
	if replies == nil || id == "" {
		return false
	}
	replies.mu.Lock()
	defer replies.mu.Unlock()
	if !replies.set[id] {
		return false
	}
	delete(replies.set, id)
	return true
}
