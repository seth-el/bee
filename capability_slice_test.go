package bee

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderAndACPZeroTimeoutsPreserveParentCancellation(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: "http://127.0.0.1:1", Model: "local", TimeoutSeconds: 0}, "")
	if err != nil {
		t.Fatal(err)
	}
	if provider.client.Timeout != 0 {
		t.Fatalf("provider client timeout=%s, want unlimited", provider.client.Timeout)
	}
	if got := (ACPProcessConfig{}).requestTimeout(); got != 0 {
		t.Fatalf("ACP request timeout=%s, want unlimited", got)
	}
	if got := (ChildProcessConfig{}).requestTimeout(); got != 0 {
		t.Fatalf("child request timeout=%s, want unlimited", got)
	}
	parent, cancelParent := context.WithCancel(context.Background())
	request, cancelRequest := contextWithOptionalTimeout(parent, 0)
	cancelParent()
	defer cancelRequest()
	if !errors.Is(request.Err(), context.Canceled) {
		t.Fatalf("optional timeout context err=%v, want parent cancellation", request.Err())
	}

	explicit, cancelExplicit := contextWithOptionalTimeout(context.Background(), time.Millisecond)
	defer cancelExplicit()
	if deadline, ok := explicit.Deadline(); !ok || time.Until(deadline) <= 0 {
		t.Fatal("explicit timeout did not install a deadline")
	}
}

type blockingCapabilityProvider struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (provider *blockingCapabilityProvider) Complete(ctx context.Context, _ Request) (Response, error) {
	return provider.Stream(ctx, Request{}, nil)
}

func (provider *blockingCapabilityProvider) Stream(ctx context.Context, _ Request, _ StreamFunc) (Response, error) {
	select {
	case provider.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case provider.cancelled <- struct{}{}:
	default:
	}
	return Response{}, ctx.Err()
}

func TestStopKillAndCancelDispatchCancelActiveTopLevelRuns(t *testing.T) {
	provider := &blockingCapabilityProvider{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1)}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Run(context.Background(), RunRequest{Prompt: "long running"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	result := DispatchCommand(context.Background(), "/stop", CommandContext{Application: app})
	if !result.Handled || result.Kind != commandKindCancel || result.CancelledRuns != 1 || !strings.Contains(result.Text, "cancelled 1") {
		t.Fatalf("stop result=%#v", result)
	}
	select {
	case <-provider.cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel provider context")
	}
	for _, command := range []string{"/kill", "/cancel"} {
		result = DispatchCommand(context.Background(), command, CommandContext{Application: app})
		if !result.Handled || result.Kind != commandKindCancel || result.CancelledRuns != 0 {
			t.Fatalf("%s result=%#v", command, result)
		}
	}
}

func TestRuntimeInfoToolIsRegisteredAndRedactsSecretEnvironment(t *testing.T) {
	t.Setenv("BEE_PUBLIC_TEST", "visible")
	t.Setenv("BEE_API_TOKEN_TEST", "do-not-show")
	t.Setenv("BEE_PRIVATE_TEST", "do-not-show")
	root := t.TempDir()
	config := DefaultConfig()
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: root, MemoryRoot: filepath.Join(root, "memory")})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := app.Executor.Registry.Lookup("runtime_info")
	if !ok {
		t.Fatal("runtime_info is not registered")
	}
	definition := tool.Definition()
	if definition.Mutability != ToolMutabilityReadOnly || definition.Risk != RiskLow {
		t.Fatalf("runtime_info definition=%#v", definition)
	}
	output, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var info RuntimeInfo
	if err := json.Unmarshal([]byte(output), &info); err != nil {
		t.Fatal(err)
	}
	if info.OS == "" || info.Arch == "" || info.Runtime == "" || info.PID <= 0 || info.WorkingDirectory == "" || info.ScopeRoot != root {
		t.Fatalf("runtime info missing truthful metadata=%#v", info)
	}
	if info.Provider == "" || info.Profile == "" || info.Model == "" {
		t.Fatalf("runtime info missing application metadata=%#v", info)
	}
	if !capabilityTestContainsString(info.ActiveToolNames, "runtime_info") {
		t.Fatalf("runtime info active tools=%#v", info.ActiveToolNames)
	}
	if info.Environment["BEE_PUBLIC_TEST"] != "visible" {
		t.Fatalf("non-secret environment missing=%#v", info.Environment)
	}
	encoded := string(output)
	for _, secret := range []string{"do-not-show", "BEE_API_TOKEN_TEST", "BEE_PRIVATE_TEST"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("runtime_info leaked %q: %s", secret, encoded)
		}
	}
}

func capabilityTestContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
