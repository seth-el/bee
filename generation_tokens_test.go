package bee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMainMaxTokensOverrideOmitsWireFieldOnlyForMainRole(t *testing.T) {
	zero := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload["max_tokens"]; ok {
			t.Fatalf("main request unexpectedly sent max_tokens: %#v", payload["max_tokens"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"model\":\"local\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	config := DefaultConfig()
	config.Provider.BaseURL = server.URL
	config.ProviderRole = ProviderRoleMain
	config.Generation.MainMaxTokens = &zero
	provider, err := NewOpenAICompatibleProvider(config.Provider, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	options := app.agentOptions(nil, nil, "")
	if options.MaxTokensOverride == nil || *options.MaxTokensOverride != 0 {
		t.Fatalf("main agent max token override = %v, want pointer to zero", options.MaxTokensOverride)
	}
	agent, err := NewAgentWithOptions(config, provider, options)
	if err != nil {
		t.Fatal(err)
	}
	request, err := agent.request()
	if err != nil {
		t.Fatal(err)
	}
	if request.MaxTokens != 0 {
		t.Fatalf("main request MaxTokens = %d, want 0", request.MaxTokens)
	}
	if _, err := provider.Stream(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMainMaxTokensOverrideOmissionKeepsDefaultAndDoesNotAffectManualRole(t *testing.T) {
	for _, test := range []struct {
		name string
		role ProviderRole
		main *int
		want int
	}{
		{name: "omitted main override", role: ProviderRoleMain, want: DefaultMaxTokens},
		{name: "manual role ignores main override", role: ProviderRoleManual, main: func() *int { value := 0; return &value }(), want: DefaultMaxTokens},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig()
			config.ProviderRole = test.role
			config.Generation.MainMaxTokens = test.main
			provider := &childTestProvider{}
			app, err := NewApplication(config, provider, nil)
			if err != nil {
				t.Fatal(err)
			}
			agent, err := NewAgentWithOptions(config, provider, app.agentOptions(nil, nil, ""))
			if err != nil {
				t.Fatal(err)
			}
			request, err := agent.request()
			if err != nil {
				t.Fatal(err)
			}
			if request.MaxTokens != test.want {
				t.Fatalf("request MaxTokens = %d, want %d", request.MaxTokens, test.want)
			}
		})
	}
}

func TestMainMaxTokensOverrideDoesNotAffectGenericChildRequests(t *testing.T) {
	zero := 0
	mainProvider := &childTestProvider{}
	childProvider := &childTestProvider{}
	config := DefaultConfig()
	config.ProviderRole = ProviderRoleMain
	config.Generation.MainMaxTokens = &zero
	app, err := NewApplication(config, mainProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	childConfig := config
	childConfig.ProviderRole = ProviderRoleChild
	app.ChildConfig = childConfig
	app.ChildProvider = childProvider

	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), childTask(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if len(childProvider.observed) != 1 {
		t.Fatalf("child requests = %#v, want one request", childProvider.observed)
	}
	if childProvider.observed[0].MaxTokens != DefaultMaxTokens {
		t.Fatalf("child request MaxTokens = %d, want %d", childProvider.observed[0].MaxTokens, DefaultMaxTokens)
	}
}
