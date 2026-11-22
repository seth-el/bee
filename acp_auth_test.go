package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeACPAuthTransport struct {
	initialized ACPInitializeResult
	authParams  *ACPAuthenticateParams
	authErr     error
	diagnostics string
	calls       []string
	updates     chan ACPUpdate
}

func newFakeACPAuthTransport(initialized ACPInitializeResult) *fakeACPAuthTransport {
	return &fakeACPAuthTransport{initialized: initialized, updates: make(chan ACPUpdate, 4)}
}

func (transport *fakeACPAuthTransport) Start(context.Context) error {
	transport.calls = append(transport.calls, "start")
	return nil
}

func (transport *fakeACPAuthTransport) Initialize(context.Context, ACPInitializeParams) (ACPInitializeResult, error) {
	transport.calls = append(transport.calls, "initialize")
	return transport.initialized, nil
}

func (transport *fakeACPAuthTransport) Authenticate(_ context.Context, params ACPAuthenticateParams) error {
	transport.calls = append(transport.calls, "authenticate")
	copy := params
	transport.authParams = &copy
	return transport.authErr
}

func (transport *fakeACPAuthTransport) NewSession(context.Context, ACPSessionNewParams) (ACPSession, error) {
	transport.calls = append(transport.calls, "session/new")
	return ACPSession{SessionID: "fake-session"}, nil
}

func (transport *fakeACPAuthTransport) Prompt(context.Context, ACPPromptParams) (ACPPromptResult, error) {
	transport.calls = append(transport.calls, "session/prompt")
	return ACPPromptResult{StopReason: "end_turn", Summary: "done"}, nil
}

func (transport *fakeACPAuthTransport) Cancel(context.Context, string) error { return nil }
func (transport *fakeACPAuthTransport) Updates() <-chan ACPUpdate            { return transport.updates }
func (transport *fakeACPAuthTransport) Failure() error                       { return nil }
func (transport *fakeACPAuthTransport) Diagnostics() string                  { return transport.diagnostics }
func (transport *fakeACPAuthTransport) Close(context.Context) error          { return nil }

func TestACPAuthenticateParamsUsesStandardAPIKeyWireShape(t *testing.T) {
	params := ACPAuthenticateParams{MethodID: ACPAuthMethodAPIKey, Meta: ACPAuthenticateMeta{APIKey: ACPAPIKeyAuth{APIKey: "secret"}}}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"methodId":"api-key","_meta":{"api-key":{"apiKey":"secret"}}}`; got != want {
		t.Fatalf("wire = %s, want %s", got, want)
	}
	if err := params.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestACPBackendAuthenticatesAdvertisedAPIKeyBeforeSession(t *testing.T) {
	secret := "api-key-test-secret"
	t.Setenv(ACPAuthEnvOpenAI, secret)
	transport := newFakeACPAuthTransport(ACPInitializeResult{ProtocolVersion: ACPProtocolVersion, AuthMethods: []ACPAuthMethod{{ID: ACPAuthMethodAPIKey}}})
	root := t.TempDir()
	backend, err := NewACPBackend(ACPProcessConfig{Command: "unused", AuthEnvVar: ACPAuthEnvOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) { return transport, nil }
	completion, err := backend.Run(context.Background(), acpTestTask(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Status != ChildSucceeded {
		t.Fatalf("completion = %#v", completion)
	}
	if transport.authParams == nil || transport.authParams.Meta.APIKey.APIKey != secret {
		t.Fatalf("auth params = %#v", transport.authParams)
	}
	if got, want := strings.Join(transport.calls, ","), "start,initialize,authenticate,session/new,session/prompt"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
}

func TestACPBackendFailsClosedWhenConfiguredAuthEnvIsMissing(t *testing.T) {
	t.Setenv(ACPAuthEnvOpenAI, "")
	transport := newFakeACPAuthTransport(ACPInitializeResult{ProtocolVersion: ACPProtocolVersion, AuthMethods: []ACPAuthMethod{{ID: ACPAuthMethodAPIKey}}})
	backend, err := NewACPBackend(ACPProcessConfig{Command: "unused", AuthEnvVar: ACPAuthEnvOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) { return transport, nil }
	completion, err := backend.Run(context.Background(), acpTestTask(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(completion.Summary, "authentication required") || !strings.Contains(completion.Summary, "auth_env_var") {
		t.Fatalf("summary = %q", completion.Summary)
	}
	if len(transport.calls) != 2 || transport.calls[1] != "initialize" {
		t.Fatalf("calls = %#v", transport.calls)
	}
}

func TestACPBackendFailsClosedOnUnsupportedAuthMethod(t *testing.T) {
	t.Setenv(ACPAuthEnvOpenAI, "unused-secret")
	transport := newFakeACPAuthTransport(ACPInitializeResult{ProtocolVersion: ACPProtocolVersion, AuthMethods: []ACPAuthMethod{{ID: "oauth"}}})
	backend, err := NewACPBackend(ACPProcessConfig{Command: "unused", AuthEnvVar: ACPAuthEnvOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) { return transport, nil }
	completion, err := backend.Run(context.Background(), acpTestTask(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(completion.Summary, "no supported authentication method") {
		t.Fatalf("summary = %q", completion.Summary)
	}
	if len(transport.calls) != 2 || transport.calls[1] != "initialize" {
		t.Fatalf("calls = %#v", transport.calls)
	}
}

func TestACPBackendRedactsAuthSecretFromCompletionEventsAndArtifact(t *testing.T) {
	secret := "secret-that-must-not-leak"
	t.Setenv(ACPAuthEnvOpenAI, secret)
	transport := newFakeACPAuthTransport(ACPInitializeResult{ProtocolVersion: ACPProtocolVersion, AuthMethods: []ACPAuthMethod{{ID: ACPAuthMethodAPIKey}}})
	transport.diagnostics = "adapter echoed " + secret
	// The fake emits provider output containing the credential to exercise the
	// parent-side redaction boundary. This is not a supported provider behavior.
	transportPrompt := func(context.Context, ACPPromptParams) (ACPPromptResult, error) {
		transport.calls = append(transport.calls, "session/prompt")
		transport.updates <- ACPUpdate{SessionID: "fake-session", Type: "agent_message_chunk", Text: secret, Info: secret, Artifact: secret, Diff: secret, Payload: json.RawMessage(fmt.Sprintf(`{"secret":%q}`, secret))}
		return ACPPromptResult{StopReason: "end_turn", Summary: secret}, nil
	}
	backend, err := NewACPBackend(ACPProcessConfig{Command: "unused", AuthEnvVar: ACPAuthEnvOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) {
		return &fakeACPAuthLeakTransport{fakeACPAuthTransport: transport, prompt: transportPrompt}, nil
	}
	var events []CodingBackendEvent
	root := t.TempDir()
	completion, err := backend.Run(context.Background(), acpTestTask(root), codingBackendObserverFunc(func(event CodingBackendEvent) { events = append(events, event) }))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("completion leaked secret: %s", data)
	}
	for _, event := range events {
		eventData, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(eventData), secret) {
			t.Fatalf("event leaked secret: %s", eventData)
		}
	}
	packetPath := filepath.Join(root, "generated", "children", "acp-task", "completion.json")
	packet, err := os.ReadFile(packetPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(packet), secret) {
		t.Fatalf("artifact leaked secret: %s", packet)
	}
}

type fakeACPAuthLeakTransport struct {
	*fakeACPAuthTransport
	prompt func(context.Context, ACPPromptParams) (ACPPromptResult, error)
}

func (transport *fakeACPAuthLeakTransport) Prompt(ctx context.Context, params ACPPromptParams) (ACPPromptResult, error) {
	return transport.prompt(ctx, params)
}

func TestACPProcessConfigAuthEnvVarIsExplicitAndRestricted(t *testing.T) {
	if err := (ACPProcessConfig{Command: "acp-agent", AuthEnvVar: "TOKEN"}).Validate(); err == nil || !strings.Contains(err.Error(), "auth_env_var") {
		t.Fatalf("validation error = %v", err)
	}
	if err := (ACPProcessConfig{Command: "acp-agent", AuthEnvVar: ACPAuthEnvOpenAI}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ACPProcessConfig{Command: "acp-agent", AuthMethod: "oauth"}).Validate(); err == nil || !strings.Contains(err.Error(), "auth_method") {
		t.Fatalf("auth method validation error = %v", err)
	}
	definition := CodingBackendDefinition{Command: "acp-agent", AuthEnvVar: ACPAuthEnvOpenAI}
	if got := definition.processConfig().AuthEnvVar; got != ACPAuthEnvOpenAI {
		t.Fatalf("definition auth env = %q", got)
	}
}

func TestACPAuthTransportErrorDoesNotLeakSecret(t *testing.T) {
	secret := "auth-error-secret"
	t.Setenv(ACPAuthEnvOpenAI, secret)
	transport := newFakeACPAuthTransport(ACPInitializeResult{ProtocolVersion: ACPProtocolVersion, AuthMethods: []ACPAuthMethod{{ID: ACPAuthMethodAPIKey}}})
	transport.authErr = errors.New("provider rejected " + secret)
	transport.diagnostics = "stderr " + secret
	backend, err := NewACPBackend(ACPProcessConfig{Command: "unused", AuthEnvVar: ACPAuthEnvOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	backend.TransportFactory = func(ACPProcessConfig) (ACPTransport, error) { return transport, nil }
	completion, err := backend.Run(context.Background(), acpTestTask(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("completion leaked secret: %s", data)
	}
	if strings.Contains(completion.Summary, "[REDACTED]") == false {
		t.Fatalf("summary was not visibly redacted: %q", completion.Summary)
	}
}
