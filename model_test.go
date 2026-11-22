package bee

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type modelCaptureProvider struct {
	models []string
}

func (provider *modelCaptureProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("not used")
}
func (provider *modelCaptureProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.models = append(provider.models, request.Model)
	if onDelta != nil {
		_ = onDelta("ok")
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "ok"}, Model: request.Model}, nil
}

func TestRunModelOverrideReachesProviderAndEvents(t *testing.T) {
	provider := &modelCaptureProvider{}
	config := DefaultConfig()
	config.Provider.Model = "default-model"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello", Model: "explicit-model"})
	if err != nil {
		t.Fatal(err)
	}
	var sawModel bool
	for event := range events {
		if event.Model == "explicit-model" {
			sawModel = true
		}
	}
	if len(provider.models) != 1 || provider.models[0] != "explicit-model" || !sawModel {
		t.Fatalf("models=%#v saw=%v", provider.models, sawModel)
	}
}

func TestRunRejectsUnsafeModelBeforeProvider(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"", "model with spaces", "model\nname"} {
		if model == "" {
			continue
		}
		if _, err := app.Run(context.Background(), RunRequest{Prompt: "hello", Model: model}); err == nil {
			t.Fatalf("model %q accepted", model)
		}
	}
	if len(provider.models) != 0 {
		t.Fatalf("provider called for invalid models: %#v", provider.models)
	}
}

func TestStorelessApplicationRejectsModelChange(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.Run(context.Background(), RunRequest{Prompt: "first", Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	for range first {
	}
	second, err := app.Run(context.Background(), RunRequest{Prompt: "second", Model: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	var failure string
	for event := range second {
		if event.Type == RunFailure {
			failure = event.Error
		}
	}
	if !strings.Contains(failure, "cannot change model") {
		t.Fatalf("failure=%q", failure)
	}
	if len(provider.models) != 1 || provider.models[0] != "model-a" {
		t.Fatalf("provider models=%#v", provider.models)
	}
}

func TestRunTUIModelOverrideReachesProvider(t *testing.T) {
	provider := &modelCaptureProvider{}
	app, err := NewApplication(DefaultConfig(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunTUIWithSessionAndModel(context.Background(), app, strings.NewReader("hello\n/exit\n"), &strings.Builder{}, "", "tui-model"); err != nil {
		t.Fatal(err)
	}
	if len(provider.models) != 1 || provider.models[0] != "tui-model" {
		t.Fatalf("provider models=%#v", provider.models)
	}
}

func TestValidateModelNameAcceptsProviderIdentifiers(t *testing.T) {
	for _, model := range []string{"local", "qwen3:32b", "provider/model-v2", "model@2026"} {
		if err := ValidateModelName(model); err != nil {
			t.Fatalf("model %q rejected: %v", model, err)
		}
	}
	data, err := json.Marshal(RunRequest{Prompt: "hello", Model: "explicit"})
	if err != nil || !strings.Contains(string(data), `"model":"explicit"`) {
		t.Fatalf("request JSON=%s err=%v", data, err)
	}
}
