package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentDefaultPromptSurvivesAbsentSoulFile(t *testing.T) {
	t.Setenv("BEE_HOME", t.TempDir())
	config := DefaultConfig()
	provider := &fakeProvider{}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.Messages(); len(got) != 1 || got[0].Content != DefaultSystemPrompt {
		t.Fatalf("messages=%#v", got)
	}
}

func TestAgentDefaultPromptSurvivesMissingHomeEnvironment(t *testing.T) {
	t.Setenv("BEE_HOME", "")
	t.Setenv("HOME", "")
	agent, err := NewAgent(DefaultConfig(), &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if messages := agent.Messages(); len(messages) != 1 || messages[0].Content != DefaultSystemPrompt {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestAgentLoadsSoulFileWithCapabilityPreamble(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	path := filepath.Join(home, "workspace", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Prefer concise answers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(DefaultConfig(), &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) != 1 || !strings.HasPrefix(messages[0].Content, DefaultSystemPrompt+"\n\n") || !strings.HasSuffix(messages[0].Content, "Prefer concise answers.") {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestAgentLoadsBeeWorkspaceContextAfterSoul(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("Soul instructions."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "BEE.md"), []byte("Workspace identity and preferences."), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(DefaultConfig(), &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) != 1 {
		t.Fatalf("messages=%#v", messages)
	}
	prompt := messages[0].Content
	soulIndex := strings.Index(prompt, "Soul instructions.")
	workspaceIndex := strings.Index(prompt, "Workspace identity and preferences.")
	if soulIndex < 0 || workspaceIndex < 0 || soulIndex >= workspaceIndex {
		t.Fatalf("workspace context order is wrong: %q", prompt)
	}
	if strings.Count(prompt, "<bee-workspace-context>") != 1 || strings.Count(prompt, "</bee-workspace-context>") != 1 {
		t.Fatalf("workspace context was not delimited exactly once: %q", prompt)
	}
}

func TestBeeWorkspaceContextIsReReadWhenAgentIsConstructed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "BEE.md")
	if err := os.WriteFile(path, []byte("first workspace revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := NewAgent(DefaultConfig(), &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second workspace revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := NewAgent(DefaultConfig(), &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Messages()[0].Content, "first workspace revision") || strings.Contains(first.Messages()[0].Content, "second workspace revision") {
		t.Fatalf("first agent context changed: %#v", first.Messages())
	}
	if !strings.Contains(second.Messages()[0].Content, "second workspace revision") || strings.Contains(second.Messages()[0].Content, "first workspace revision") {
		t.Fatalf("second agent did not reload context: %#v", second.Messages())
	}
}

func TestBeeWorkspaceContextIsParentOnlyAndExplicitEmptyStillDisablesSystemMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "BEE.md"), []byte("Parent-only workspace context."), 0o600); err != nil {
		t.Fatal(err)
	}

	childConfig := DefaultConfig()
	childConfig.ProviderRole = ProviderRoleChild
	child, err := NewAgent(childConfig, &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(child.Messages()[0].Content, "Parent-only workspace context.") {
		t.Fatalf("child received workspace context: %#v", child.Messages())
	}

	configPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"agent":{"system_prompt":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyConfig, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := NewAgent(emptyConfig, &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Messages()) != 0 {
		t.Fatalf("explicit empty prompt loaded workspace context: %#v", empty.Messages())
	}
}

func TestBeeWorkspaceContextRejectsUnsafeFilesAndBoundsReads(t *testing.T) {
	for _, test := range []struct {
		name string
		make func(*testing.T, string)
		want string
	}{
		{
			name: "permissions",
			make: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "owner-only",
		},
		{
			name: "symlink",
			make: func(t *testing.T, path string) {
				target := filepath.Join(filepath.Dir(path), "target.md")
				if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "symlink",
		},
		{
			name: "size",
			make: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxWorkspaceContextFileBytes+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "exceeds",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BEE_HOME", home)
			workspace := filepath.Join(home, "workspace")
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			test.make(t, filepath.Join(workspace, "BEE.md"))
			_, err := NewAgent(DefaultConfig(), &fakeProvider{})
			if err == nil || !strings.Contains(err.Error(), "Bee workspace context file") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSystemPromptFileRejectsUnsafeFilesAndBoundsReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	workspace := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("permissions", func(t *testing.T) {
		path := filepath.Join(workspace, "permissions.md")
		if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
		config := DefaultConfig()
		config.Agent.SystemPromptFile = path
		if _, err := NewAgent(config, &fakeProvider{}); err == nil || !strings.Contains(err.Error(), "owner-only") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(workspace, "target.md")
		link := filepath.Join(workspace, "link.md")
		if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		config := DefaultConfig()
		config.Agent.SystemPromptFile = link
		if _, err := NewAgent(config, &fakeProvider{}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("size", func(t *testing.T) {
		secret := "DO_NOT_LEAK"
		path := filepath.Join(workspace, "large.md")
		if err := os.WriteFile(path, []byte(strings.Repeat(secret, MaxSystemPromptFileBytes/len(secret)+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		config := DefaultConfig()
		config.Agent.SystemPromptFile = path
		_, err := NewAgent(config, &fakeProvider{})
		if err == nil || !strings.Contains(err.Error(), "exceeds") || strings.Contains(err.Error(), secret) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestSystemPromptFileResolvesBeeHomeRelativeAndTildePaths(t *testing.T) {
	home := t.TempDir()
	tildeHome := t.TempDir()
	t.Setenv("BEE_HOME", home)
	t.Setenv("HOME", tildeHome)
	beeSoul := filepath.Join(home, "workspace", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(beeSoul), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beeSoul, []byte("from bee home"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"workspace/SOUL.md", "~/.bee/workspace/SOUL.md"} {
		t.Run(path, func(t *testing.T) {
			config := DefaultConfig()
			config.Agent.SystemPromptFile = path
			agent, err := NewAgent(config, &fakeProvider{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(agent.Messages()[0].Content, "from bee home") {
				t.Fatalf("messages=%#v", agent.Messages())
			}
		})
	}

	generic := filepath.Join(tildeHome, "soul.md")
	if err := os.WriteFile(generic, []byte("from tilde"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Agent.SystemPromptFile = "~/soul.md"
	agent, err := NewAgent(config, &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(agent.Messages()[0].Content, "from tilde") {
		t.Fatalf("messages=%#v", agent.Messages())
	}
}

func TestExplicitInlineSystemPromptWinsOverFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	path := filepath.Join(home, "workspace", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("file instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "workspace", "BEE.md"), []byte("workspace context"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Agent.SystemPrompt = "inline instructions"
	config.Agent.SystemPromptFile = path
	agent, err := NewAgent(config, &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if messages := agent.Messages(); len(messages) != 1 || !strings.HasPrefix(messages[0].Content, "inline instructions\n\n") || !strings.Contains(messages[0].Content, "workspace context") || strings.Contains(messages[0].Content, "file instructions") {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestLoadedConfigTracksExplicitInlinePromptPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "workspace", "BEE.md"), []byte("loaded workspace context"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(`{"agent":{"system_prompt":"inline","system_prompt_file":"missing.md"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Agent.SystemPromptFile != "missing.md" {
		t.Fatalf("system prompt file=%q", config.Agent.SystemPromptFile)
	}
	agent, err := NewAgent(config, &fakeProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if messages := agent.Messages(); len(messages) != 1 || !strings.HasPrefix(messages[0].Content, "inline\n\n") || !strings.Contains(messages[0].Content, "loaded workspace context") {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestGeneralAssistantReceivesComposedSystemPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	path := filepath.Join(home, "workspace", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("general assistant instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), JobStore: mustJobStore(t), ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	app.Provider = provider
	events, err := app.Run(context.Background(), RunRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider request count=%d", len(provider.requests))
	}
	found := false
	for _, message := range provider.requests[0].Messages {
		instructions := strings.Index(message.Content, "general assistant instructions")
		moodProtocol := strings.Index(message.Content, moodDeltaPrefix)
		if message.Role == RoleSystem && instructions >= 0 && moodProtocol > instructions {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("composed system prompt missing from %d messages", len(provider.requests[0].Messages))
	}
}

func mustJobStore(t *testing.T) *JobStore {
	t.Helper()
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}
