package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/seth-el/bee"
)

type modelCaptureProvider struct {
	models   []string
	requests []bee.Request
}

func (provider *modelCaptureProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, fmt.Errorf("complete is not used")
}

func (provider *modelCaptureProvider) Stream(_ context.Context, request bee.Request, onDelta bee.StreamFunc) (bee.Response, error) {
	provider.models = append(provider.models, request.Model)
	provider.requests = append(provider.requests, request)
	if onDelta != nil {
		_ = onDelta("ok")
	}
	return bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: "ok"}, Usage: bee.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, Model: request.Model}, nil
}

func writeTestConfig(t *testing.T, baseURL, model string) string {
	t.Helper()
	config := bee.DefaultConfig()
	config.Provider.BaseURL = baseURL
	config.Provider.Model = model
	// Exercise the supported legacy single-provider shape without serializing
	// zero-valued profile routing or pinning the complete runtime defaults.
	document := struct {
		Provider bee.ProviderConfig `json:"provider"`
		Agent    struct {
			SystemPromptFile string `json:"system_prompt_file"`
		} `json:"agent"`
		// The specialist (child-model helpers such as automatic session
		// titles) would share the single scripted provider and consume its
		// replies; tests that want it configure it explicitly.
		Specialist struct {
			Enabled bool `json:"enabled"`
		} `json:"specialist"`
	}{Provider: config.Provider}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSSE(writer http.ResponseWriter, payload string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(writer, "data: %s\n\ndata: [DONE]\n\n", payload)
}

func hasTool(tools []struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}, name string) bool {
	for _, tool := range tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}
