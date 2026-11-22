package bee

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigDisplayVisibilityDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantTools    bool
		wantMode     ToolDisplayMode
		wantThinking bool
	}{
		{name: "omitted", body: `{}`, wantTools: true, wantMode: ToolDisplayCompact, wantThinking: true},
		{name: "assistant fields omitted", body: `{"assistant":{"policy":"trusted"}}`, wantTools: true, wantMode: ToolDisplayCompact, wantThinking: true},
		{name: "both hidden", body: `{"assistant":{"show_tool_calls":false,"show_thinking":false}}`, wantMode: ToolDisplayCompact},
		{name: "independent overrides", body: `{"assistant":{"show_tool_calls":false,"show_thinking":true}}`, wantMode: ToolDisplayCompact, wantThinking: true},
		{name: "full tool display", body: `{"assistant":{"tool_display":"full"}}`, wantTools: true, wantMode: ToolDisplayFull, wantThinking: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.Assistant.ShowToolCalls != test.wantTools || config.Assistant.ToolDisplay != test.wantMode || config.Assistant.ShowThinking != test.wantThinking {
				t.Fatalf("assistant visibility = tools:%v mode:%s thinking:%v, want tools:%v mode:%s thinking:%v", config.Assistant.ShowToolCalls, config.Assistant.ToolDisplay, config.Assistant.ShowThinking, test.wantTools, test.wantMode, test.wantThinking)
			}
		})
	}
}

func TestApplicationInitializesDisplayVisibilityFromConfig(t *testing.T) {
	config := DefaultConfig()
	config.Provider.Model = "test"
	config.Assistant.ShowToolCalls = false
	config.Assistant.ShowThinking = false

	app, err := NewApplication(config, &fakeProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	visibility := app.DisplayVisibility()
	if visibility.ToolCalls || visibility.ToolMode != ToolDisplayOff || visibility.Thinking {
		t.Fatalf("initial visibility = %#v, want both hidden", visibility)
	}

	visibility = app.SetThinkingVisible(true)
	if visibility.ToolCalls || !visibility.Thinking {
		t.Fatalf("runtime override visibility = %#v, want tools hidden and thinking visible", visibility)
	}
}

func TestApplicationToolDisplayModesPreserveLegacyToggle(t *testing.T) {
	app := &Application{}
	if visibility := app.DisplayVisibility(); visibility.ToolMode != ToolDisplayCompact || !visibility.ToolCalls {
		t.Fatalf("default visibility=%#v", visibility)
	}
	if visibility := app.SetToolDisplayMode(ToolDisplayFull); visibility.ToolMode != ToolDisplayFull || !visibility.ToolCalls {
		t.Fatalf("full visibility=%#v", visibility)
	}
	if visibility := app.SetToolCallsVisible(false); visibility.ToolMode != ToolDisplayOff || visibility.ToolCalls {
		t.Fatalf("hidden visibility=%#v", visibility)
	}
	if visibility := app.SetToolCallsVisible(true); visibility.ToolMode != ToolDisplayFull || !visibility.ToolCalls {
		t.Fatalf("legacy on visibility=%#v", visibility)
	}
}

func TestLoadConfigRejectsInvalidToolDisplayMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"assistant":{"tool_display":"verbose"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "tool_display") {
		t.Fatalf("LoadConfig error=%v, want invalid tool_display", err)
	}
}
