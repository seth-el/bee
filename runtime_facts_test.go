package bee

import (
	"context"
	"strings"
	"testing"
)

func TestRuntimeContextNamesHostAndModelWithoutCredentials(t *testing.T) {
	facts := HostRuntimeFacts{Hostname: "testhost", Platform: "linux/amd64", User: "testuser"}
	rendered := renderRuntimeContext(facts, "glm-flash", "http://user:hunter2@203.0.113.37:8081/v1?key=abc")
	for _, want := range []string{"host: testhost (linux/amd64)", "user: testuser", "model: glm-flash, served from 203.0.113.37:8081"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("runtime context %q lacks %q", rendered, want)
		}
	}
	for _, leaked := range []string{"hunter2", "key=abc", "/v1"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("runtime context leaked %q: %q", leaked, rendered)
		}
	}
	if renderRuntimeContext(HostRuntimeFacts{}, "m", "http://x") != "" {
		t.Fatal("runtime context rendered without a hostname")
	}
	if got := sanitizeRuntimeFact("tes<t>\n\"x\" y"); got != "testxy" {
		t.Fatalf("sanitized fact = %q", got)
	}
}

func TestAgentComposesRuntimeContextForParentOnly(t *testing.T) {
	runtimeContext := renderRuntimeContext(HostRuntimeFacts{Hostname: "testhost", Platform: "linux/amd64"}, "glm", "http://127.0.0.1:1")
	for _, role := range []ProviderRole{ProviderRoleMain, ProviderRoleChild} {
		provider := &fakeProvider{}
		config := DefaultConfig()
		config.ProviderRole = role
		agent, err := NewAgentWithOptions(config, provider, AgentOptions{RuntimeContext: runtimeContext})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := agent.Run(context.Background(), "again", nil); err != nil {
			t.Fatal(err)
		}
		first := provider.requests[0].Messages[0].Content
		present := strings.Contains(first, "<bee-runtime>\nhost: testhost (linux/amd64)\nmodel: glm, served from 127.0.0.1:1\n</bee-runtime>")
		if present != (role != ProviderRoleChild) {
			t.Fatalf("role %s: runtime block present = %v", role, present)
		}
		if provider.requests[1].Messages[0].Content != first {
			t.Fatalf("role %s: system prompt changed between turns", role)
		}
	}
}

func TestRuntimeFactsSwitchDefaultsOn(t *testing.T) {
	if !(AssistantConfig{}).RuntimeFactsEnabled() {
		t.Fatal("runtime facts are off by default")
	}
	off := false
	if (AssistantConfig{RuntimeFacts: &off}).RuntimeFactsEnabled() {
		t.Fatal("runtime_facts: false was ignored")
	}
	var app *Application
	if app.runtimeContext("m") != "" || (&Application{}).runtimeContext("m") != "" {
		t.Fatal("runtime context rendered without facts")
	}
}
