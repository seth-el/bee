package bee

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVoiceOutputPathRejectsUnsafeAndUnexpectedPaths(t *testing.T) {
	workspace := t.TempDir()
	config := resolvedSpeechConfig{SynthesisWorkingDirectory: workspace, SynthesisOutputDirectory: filepath.Join(workspace, "generated", "voice-replies")}
	for _, output := range []string{
		"MEDIA:/etc/passwd",
		"MEDIA:./generated/scouts/escape.ogg",
		"MEDIA:./generated/voice-replies/other.ogg",
		"MEDIA:file:///home/testuser/elsewhere/generated/voice-replies/bee-voice-run.ogg",
	} {
		if _, err := parseVoiceOutputPath(output, "bee-voice-run", config); err == nil {
			t.Fatalf("unsafe voice output accepted: %q", output)
		}
	}
}

func TestVoiceCommandTTSModesSplitGeneratedAndLiteralPaths(t *testing.T) {
	cases := []struct {
		name    string
		command VoiceCommand
		want    string
	}{
		{name: "generated /speak", command: VoiceCommand{Generate: true}, want: voiceTTSModeDirection},
		{name: "generated mode wins over stale field", command: VoiceCommand{Generate: true, TTSMode: voiceTTSModeClone}, want: voiceTTSModeDirection},
		{name: "literal /say", command: VoiceCommand{Generate: false}, want: voiceTTSModeClone},
		{name: "literal /say", command: VoiceCommand{TTSMode: voiceTTSModeClone}, want: voiceTTSModeClone},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := voiceCommandTTSMode(test.command)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("mode = %q, want %q", got, test.want)
			}
		})
	}
	if _, err := voiceCommandTTSMode(VoiceCommand{TTSMode: "invalid"}); err == nil {
		t.Fatal("invalid TTS mode was accepted")
	}
}

func TestWithVoiceTTSModeReplacesInheritedEnvironment(t *testing.T) {
	env := withVoiceTTSMode([]string{"PATH=/usr/bin", "BEE_SPEECH_MODE=direction", "HOME=/tmp"}, voiceTTSModeClone)
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "BEE_SPEECH_MODE=") {
			count++
			if entry != "BEE_SPEECH_MODE=clone" {
				t.Fatalf("TTS mode env = %q", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("TTS mode env count = %d, want 1", count)
	}
}

func TestVoiceTextAndSlugBoundsFailClosed(t *testing.T) {
	if err := validateVoiceText(strings.Repeat("x", MaxVoiceTextBytes+1)); err == nil {
		t.Fatal("oversized voice text was accepted")
	}
	if err := validateVoiceSlug("../escape"); err == nil {
		t.Fatal("unsafe voice slug was accepted")
	}
	if err := validateVoiceSlug("safe-voice-run"); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceSynthesisUsesConfiguredPortablePaths(t *testing.T) {
	workspace := t.TempDir()
	wrapper := filepath.Join(workspace, "synthesize.sh")
	script := "#!/bin/sh\nmkdir -p generated/voice-replies\nprintf 'audio' > \"generated/voice-replies/$2.ogg\"\nprintf 'MEDIA:./generated/voice-replies/%s.ogg\\n' \"$2\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	config := resolvedSpeechConfig{
		SynthesisWrapper:          wrapper,
		SynthesisWorkingDirectory: workspace,
		SynthesisOutputDirectory:  filepath.Join(workspace, "generated", "voice-replies"),
	}
	payload, err := synthesizeVoice(context.Background(), config, "hello", "bee-voice-portable", voiceTTSModeClone)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "audio" {
		t.Fatalf("voice payload=%q", payload)
	}
	if _, err := os.Stat(filepath.Join(config.SynthesisOutputDirectory, "bee-voice-portable.ogg")); !os.IsNotExist(err) {
		t.Fatalf("voice output was not cleaned up: %v", err)
	}
}

func TestVoiceSynthesisIsDisabledWithoutConfiguredWrapper(t *testing.T) {
	config, err := DefaultConfig().Speech.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synthesizeVoice(context.Background(), config, "hello", "bee-voice-disabled", voiceTTSModeClone); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("disabled synthesis error=%v", err)
	}
}

func TestVoiceCommandRunRejectsMissingDurableArtifactSession(t *testing.T) {
	app, err := NewApplication(DefaultConfig(), &commandCountingProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "/say hello"})
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure bool
	var sawEnd bool
	for event := range events {
		if event.Type == RunFailure {
			sawFailure = true
		}
		if event.Type == RunEnd {
			sawEnd = true
		}
	}
	if !sawFailure || !sawEnd {
		t.Fatal("voice run without a durable session did not fail with terminal events")
	}
}

func TestGeneratedVoiceChildOmitsMaxTokensWithoutChangingGlobalCap(t *testing.T) {
	var payload map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"model\":\"voice-child\",\"choices\":[{\"delta\":{\"content\":\"spoken answer\"}}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	config := DefaultConfig()
	config.Provider.BaseURL = server.URL
	config.Provider.Model = "voice-child"
	config.Generation.MaxTokens = 321
	provider := &recordingVoiceProvider{delegate: mustProviderForVoiceTest(t, config.Provider)}
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := generateVoiceAnswer(context.Background(), app, "answer briefly", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "spoken answer" {
		t.Fatalf("answer = %q", answer)
	}
	if len(provider.requests) != 1 || provider.requests[0].MaxTokens != 0 {
		t.Fatalf("generated voice child request = %#v, want MaxTokens=0", provider.requests)
	}
	if _, ok := payload["max_tokens"]; ok {
		t.Fatalf("generated voice child sent max_tokens: %s", payload["max_tokens"])
	}
	if config.Generation.MaxTokens != 321 {
		t.Fatalf("global generation cap changed to %d", config.Generation.MaxTokens)
	}
	agent, err := NewAgent(config, &childTestProvider{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	if request.MaxTokens != 321 {
		t.Fatalf("ordinary request max_tokens = %d, want 321", request.MaxTokens)
	}
}

func TestGeneratedVoiceChildUsesVoiceProviderWhileGenericChildUsesChildProvider(t *testing.T) {
	mainProvider := &childTestProvider{}
	childProvider := &childTestProvider{}
	voiceProvider := &childTestProvider{}
	config := DefaultConfig()
	config.Provider.Model = "main-model"
	app, err := NewApplication(config, mainProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	childConfig := config
	childConfig.Provider.Model = "child-model"
	app.ChildProvider = childProvider
	app.ChildConfig = childConfig
	voiceConfig := config
	voiceConfig.Provider.Model = "voice-model"
	app.VoiceProvider = voiceProvider
	app.VoiceConfig = voiceConfig

	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), childTask(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	answer, err := generateVoiceAnswer(context.Background(), app, "answer briefly", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "child completed" {
		t.Fatalf("voice answer = %q", answer)
	}
	if len(mainProvider.observed) != 0 {
		t.Fatalf("main provider observed generic/voice child requests: %#v", mainProvider.observed)
	}
	if len(childProvider.observed) != 1 || childProvider.observed[0].Model != "child-model" {
		t.Fatalf("child provider observed = %#v", childProvider.observed)
	}
	if len(voiceProvider.observed) != 1 || voiceProvider.observed[0].Model != "voice-model" || voiceProvider.observed[0].MaxTokens != 0 {
		t.Fatalf("voice provider observed = %#v", voiceProvider.observed)
	}
}

func TestBuildVoiceHistoryContextIsBoundedDelimitedAndOmitsCurrentPromptAndTools(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "voice history"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "The old answer should be remembered."}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "The old answer is bounded reference context."}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolCall(ToolCall{ID: "tool-1", Name: "run_command", Arguments: `{"command":"cat secrets"}`}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendToolResult(ToolResultEvent{CallID: "tool-1", ToolName: "run_command", Output: "tool output must not be projected"}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "current request"}); err != nil {
		t.Fatal(err)
	}
	history, err := buildVoiceHistoryContext(session.EventsSnapshot(), "current request")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) > MaxVoiceHistoryBytes {
		t.Fatalf("history length = %d, want <= %d", len(history), MaxVoiceHistoryBytes)
	}
	if !strings.HasPrefix(history, "<BEE_PRIOR_CONVERSATION>\n") || !strings.HasSuffix(history, "</BEE_PRIOR_CONVERSATION>") {
		t.Fatalf("history is not delimited: %q", history)
	}
	if !strings.Contains(history, "The old answer") {
		t.Fatalf("history omitted prior message: %q", history)
	}
	if strings.Contains(history, "current request") || strings.Contains(history, "tool output") || strings.Contains(history, "run_command") {
		t.Fatalf("history leaked current prompt or tool authority: %q", history)
	}
}

func TestGeneratedVoiceChildIncludesPriorSessionConversation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"model\":\"voice-child\",\"choices\":[{\"delta\":{\"content\":\"spoken answer\"}}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "voice child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessages(
		Message{Role: RoleUser, Content: "Remember that the garden key is blue."},
		Message{Role: RoleAssistant, Content: "I will remember the blue garden key."},
	); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Provider.BaseURL = server.URL
	config.Provider.Model = "voice-child"
	provider := &recordingVoiceProvider{delegate: mustProviderForVoiceTest(t, config.Provider)}
	app, err := NewApplication(config, provider, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generateVoiceAnswerWithSession(context.Background(), app, "What color is the garden key?", "parent", session); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	var joined strings.Builder
	for _, message := range provider.requests[0].Messages {
		joined.WriteString(message.Content)
		joined.WriteByte('\n')
	}
	requestText := joined.String()
	if !strings.Contains(requestText, "garden key is blue") {
		t.Fatalf("generated child did not receive prior history: %q", requestText)
	}
	if strings.Count(requestText, "What color is the garden key?") != 1 {
		t.Fatalf("current prompt was duplicated in child context: %q", requestText)
	}
	if !strings.Contains(requestText, "<BEE_PRIOR_CONVERSATION>") || !strings.Contains(requestText, "</BEE_PRIOR_CONVERSATION>") {
		t.Fatalf("generated child history was not delimited: %q", requestText)
	}
}

func TestSuccessfulGeneratedAndLiteralVoiceTurnsAppendDurableMessages(t *testing.T) {
	for _, test := range []struct {
		name   string
		prompt string
		spoken string
	}{
		{name: "generated /speak", prompt: "what is the status", spoken: "The status is healthy."},
		{name: "literal /say", prompt: "say the status", spoken: "The status is healthy."},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewSessionStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.Create(SessionMetadata{Title: "voice persistence"})
			if err != nil {
				t.Fatal(err)
			}
			if err := appendSuccessfulVoiceTurn(session, test.prompt, test.spoken); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := loaded.Messages()
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 2 || messages[0].Role != RoleUser || messages[0].Content != test.prompt || messages[1].Role != RoleAssistant || messages[1].Content != test.spoken {
				t.Fatalf("durable voice messages = %#v", messages)
			}
		})
	}
}

func TestFailedVoiceTurnDoesNotAppendFalseAssistantMessage(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{Title: "voice failure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := appendSuccessfulVoiceTurn(session, "say the status", strings.Repeat("x", MaxVoiceTextBytes+1)); err == nil {
		t.Fatal("oversized failed voice answer was accepted")
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := loaded.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("failed voice turn persisted messages: %#v", messages)
	}
}

func mustProviderForVoiceTest(t *testing.T, config ProviderConfig) Provider {
	t.Helper()
	provider, err := NewOpenAICompatibleProvider(config, "")
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

type recordingVoiceProvider struct {
	delegate Provider
	requests []Request
}

func (provider *recordingVoiceProvider) Complete(ctx context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	return provider.delegate.Complete(ctx, request)
}

func (provider *recordingVoiceProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	return provider.delegate.Stream(ctx, request, onDelta)
}
