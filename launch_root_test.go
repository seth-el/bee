package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEligibleLaunchRootRefusesBroadOrSensitiveDirectories(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	beeHome := filepath.Join(home, ".bee")
	sessions := filepath.Join(home, ".local", "share", "bee", "sessions")
	project := filepath.Join(home, "projects", "bee")
	for _, dir := range []string{filepath.Join(beeHome, "state"), filepath.Join(beeHome, "secrets"), filepath.Join(beeHome, "workspace"), filepath.Join(sessions, "x"), project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	link := filepath.Join(base, "project-link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(beeHome, "workspace")
	for _, test := range []struct {
		dir, want string
	}{
		{project, ""},
		{link, ""},
		{"/", "the filesystem root"},
		{home, "home directory"},
		{base, "home directory"},
		{beeHome, "Bee's home"},
		{filepath.Join(beeHome, "state"), "session transcript store"}, // state/ now holds the default store (G6)
		{filepath.Join(beeHome, "secrets"), "secrets"},
		{filepath.Join(home, ".local", "share"), "session transcript store"},
		{filepath.Join(sessions, "x"), "inside the session transcript store"},
		{filepath.Join(workspace), "already writable"},
		{filepath.Join(base, "missing"), "not a directory"},
	} {
		dir, reason := EligibleLaunchRoot(test.dir, []string{workspace})
		if test.want == "" {
			if reason != "" || dir != project {
				t.Fatalf("%s: dir=%q reason=%q, want %q accepted", test.dir, dir, reason, project)
			}
			continue
		}
		if !strings.Contains(reason, test.want) {
			t.Fatalf("%s: reason=%q, want %q", test.dir, reason, test.want)
		}
	}
}

// Starting Bee in a project directory makes that directory writable, like
// coding agents do, while the Bee workspace stays the preferred root.
func TestLaunchRootBecomesWritableAndKnownToTheModel(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	workspace := filepath.Join(home, ".bee", "workspace")
	project := filepath.Join(base, "srv", "project") // outside home: must also become readable
	for _, dir := range []string{workspace, project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", filepath.Join(home, ".bee"))
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), MemoryRoot: filepath.Join(workspace, "memory"), LaunchRoot: project, Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual})
	if err != nil {
		t.Fatal(err)
	}
	if app.Capabilities.LaunchRoot != project || app.Capabilities.WriteRoot != workspace {
		t.Fatalf("capabilities=%#v", app.Capabilities)
	}
	if !containsString(app.Capabilities.WriteRoots, project) || !containsString(app.Capabilities.ScopeRoots, project) {
		t.Fatalf("launch root not readable+writable: %#v", app.Capabilities)
	}
	target := filepath.Join(project, "main.go")
	if _, err := app.Executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":`+strconvQuote(target)+`,"content":"package main\n","operation":"create"}`)); err != nil {
		t.Fatalf("write in launch dir needed approval: %v", err)
	}
	prompt := app.agentOptions(nil, nil, "")
	if prompt.LaunchDir != project {
		t.Fatalf("agent options LaunchDir=%q", prompt.LaunchDir)
	}
	parent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{LaunchDir: project})
	if err != nil {
		t.Fatal(err)
	}
	if text := parent.Messages()[0].Content; !strings.Contains(text, "<bee-launch-directory>") || !strings.Contains(text, project) || !strings.Contains(text, "BEE_LAUNCH_DIR") {
		t.Fatalf("parent prompt lacks launch directory: %q", text)
	}
	childConfig := DefaultConfig()
	childConfig.ProviderRole = ProviderRoleChild
	child, err := NewAgentWithOptions(childConfig, &fakeProvider{}, AgentOptions{LaunchDir: project})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range child.Messages() {
		if strings.Contains(message.Content, "bee-launch-directory") {
			t.Fatal("child received launch directory context")
		}
	}
	without, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without.Messages()[0].Content, "bee-launch-directory") {
		t.Fatal("prompt mentions a launch directory without one")
	}
	env, err := withBeeExecPaths(nil, app.Executor.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(env, "BEE_LAUNCH_DIR="+project) {
		t.Fatalf("exec env lacks BEE_LAUNCH_DIR: %v", env)
	}
}

func TestLaunchRootFromHomeIsIgnored(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	workspace := filepath.Join(home, ".bee", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", filepath.Join(home, ".bee"))
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), MemoryRoot: filepath.Join(workspace, "memory"), LaunchRoot: home})
	if err != nil {
		t.Fatal(err)
	}
	if app.Capabilities.LaunchRoot != "" || containsString(app.Capabilities.WriteRoots, home) {
		t.Fatalf("home became writable: %#v", app.Capabilities)
	}
}
