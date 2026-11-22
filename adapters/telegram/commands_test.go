package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeChatCommandMapsToolLimit(t *testing.T) {
	for input, want := range map[string]string{
		"/tool_limit":         "/tool-limit",
		"/tool_limit off":     "/tool-limit off",
		"/tool_limit@bot on":  "/tool-limit@bot on",
		"/tool_limits":        "/tool_limits",
		"/status":             "/status",
		"tell me /tool_limit": "tell me /tool_limit",
	} {
		if got := NormalizeChatCommand(input); got != want {
			t.Fatalf("%q = %q, want %q", input, got, want)
		}
	}
}

func TestChatCommandsAreValidTelegramMenuEntries(t *testing.T) {
	help := ChatCommandHelp(true)
	for _, command := range ChatCommandMenu(true) {
		if strings.ContainsAny(command.Command, "-@ ") || command.Command != strings.ToLower(command.Command) || len(command.Command) > 32 || command.Description == "" || len(command.Description) > 256 {
			t.Fatalf("invalid menu entry %#v", command)
		}
		if !strings.Contains(help, "/"+command.Command) {
			t.Fatalf("help omits /%s", command.Command)
		}
	}
}

func TestSetMyCommandsPostsMenu(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/setMyCommands") {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "123:secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetMyCommands(context.Background(), ChatCommandMenu(false)); err != nil {
		t.Fatal(err)
	}
	var sent setMyCommandsRequest
	if err := json.Unmarshal(body, &sent); err != nil || len(sent.Commands) != len(ChatCommandMenu(false)) {
		t.Fatalf("sent %s, %v", body, err)
	}
}

func TestRouteBindingsPathConfig(t *testing.T) {
	config := testConfig(t)
	if got := config.RouteBindingsFile(); got != filepath.Join(config.SessionRoot, "telegram-route-bindings.json") {
		t.Fatalf("default bindings = %q", got)
	}
	config.RouteBindingsPath = filepath.Join(t.TempDir(), "routes.json")
	if err := config.Validate(); err != nil || config.RouteBindingsFile() != config.RouteBindingsPath {
		t.Fatalf("explicit bindings = %q, %v", config.RouteBindingsFile(), err)
	}
	config.RouteBindingsPath = "relative.json"
	if err := config.Validate(); err == nil {
		t.Fatal("relative route_bindings_path accepted")
	}
}
