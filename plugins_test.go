package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePluginPackageSource(t *testing.T, parent, name, version string) string {
	t.Helper()
	root := filepath.Join(parent, name+"-"+strings.ReplaceAll(version, ".", "-"))
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec \"$BEE_TEST_BINARY\" -test.run=TestExtensionHelperProcess --\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "extension"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := PluginManifest{
		SchemaVersion: PluginSchemaVersion,
		Name:          name,
		Version:       version,
		Description:   "Test plugin " + version,
		Extension: &SkillExtension{
			Name:                  "plugin." + name,
			Command:               "bin/extension",
			EnvAllowlist:          []string{"BEE_EXTENSION_MODE", "BEE_EXTENSION_NAME", "BEE_TEST_BINARY"},
			Tools:                 []string{"echo"},
			StartupTimeoutSeconds: 2,
			RequestTimeoutSeconds: 2,
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, PluginManifestFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func setPluginHelperEnvironment(t *testing.T, name string) {
	t.Helper()
	t.Setenv("BEE_EXTENSION_MODE", "normal")
	t.Setenv("BEE_EXTENSION_NAME", "plugin."+name)
	t.Setenv("BEE_TEST_BINARY", os.Args[0])
}

func TestPluginRegistryLifecycleAndRuntimeExtension(t *testing.T) {
	setPluginHelperEnvironment(t, "echoer")
	registry, err := NewPluginRegistry(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	sources := t.TempDir()
	v1 := writePluginPackageSource(t, sources, "echoer", "1.0.0")
	v2 := writePluginPackageSource(t, sources, "echoer", "1.1.0")
	if _, err := registry.Install(context.Background(), v1); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "echoer", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Install(context.Background(), v2); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "echoer", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := registry.Rollback(context.Background(), "echoer")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Manifest.Version != "1.0.0" {
		t.Fatalf("rollback version=%q", rolledBack.Manifest.Version)
	}
	report, err := registry.TestInstalled(context.Background(), "echoer", "1.0.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExtensionTest != "initialize handshake passed" {
		t.Fatalf("report=%#v", report)
	}

	skills, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := mustAssistantStore(t)
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store: sessions, JobStorePath: filepath.Join(t.TempDir(), "jobs.json"), TodoStorePath: filepath.Join(t.TempDir(), "todos.json"),
		ScopeRoot: t.TempDir(), MemoryRoot: filepath.Join(t.TempDir(), "memory"), ArtifactStore: artifacts,
		SkillRegistry: skills, PluginRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Capabilities.PluginNames) != 1 || app.Capabilities.PluginNames[0] != "echoer" {
		t.Fatalf("plugins=%#v", app.Capabilities.PluginNames)
	}
	for _, name := range []string{SkillCreateToolName, SkillTestToolName, SkillEnableToolName} {
		if _, ok := app.Executor.Registry.Lookup(name); !ok {
			t.Fatalf("missing builder tool %q", name)
		}
	}
	// Plugin authoring is pruned from the foreground; the enabled plugin's
	// own runtime tools are unaffected.
	for _, name := range []string{PluginListToolName, PluginCreateToolName, PluginTestToolName, PluginEnableToolName} {
		if _, ok := app.Executor.Registry.Lookup(name); ok {
			t.Fatalf("pruned plugin tool %q is still model-facing", name)
		}
	}
	catalogOutput, err := (PluginListTool{Plugins: registry}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var catalog PluginListResult
	if err := json.Unmarshal([]byte(catalogOutput), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Plugins) != 1 || catalog.Plugins[0].Name != "echoer" || len(catalog.Plugins[0].Tools) != 1 || catalog.Plugins[0].Tools[0] != "echo" {
		t.Fatalf("catalog=%#v", catalog)
	}
	if err := app.StartExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.CloseExtensions(context.Background()) })
	echo, ok := app.Executor.Registry.Lookup("echo")
	if !ok {
		t.Fatal("plugin tool was not registered")
	}
	output, err := echo.Execute(context.Background(), json.RawMessage(`{"text":"hello"}`))
	if err != nil || output != "hello" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestPluginPackageValidationRejectsUnknownFieldsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PluginManifestFilename), []byte(`{"schema_version":1,"name":"safe","version":"1.0.0","description":"safe","extension":{},"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TestPluginSource(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err=%v", err)
	}

	setPluginHelperEnvironment(t, "linked")
	source := writePluginPackageSource(t, t.TempDir(), "linked", "1.0.0")
	command := filepath.Join(source, "bin", "extension")
	if err := os.Remove(command); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.Args[0], command); err != nil {
		t.Fatal(err)
	}
	if _, err := TestPluginSource(context.Background(), source, false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err=%v", err)
	}
}

func TestBuilderToolsRequireApprovalAndBindTestedChecksum(t *testing.T) {
	scopeRoot := t.TempDir()
	scope := ExecutionScope{Root: scopeRoot}
	skills, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	createOutput, err := (SkillCreateTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"name":"reviewer","version":"1.0.0","description":"Review changes","instructions":"Inspect the diff carefully."}`))
	if err != nil {
		t.Fatal(err)
	}
	var created PackageBuildResult
	if err := json.Unmarshal([]byte(createOutput), &created); err != nil {
		t.Fatal(err)
	}
	testOutput, err := (SkillTestTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"source":`+mustJSONText(t, created.Path)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	var report SkillTestReport
	if err := json.Unmarshal([]byte(testOutput), &report); err != nil {
		t.Fatal(err)
	}
	enable := SkillEnableTool{Scope: scope, Skills: skills}
	toolRegistry, err := NewRegistry(enable)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"source":` + mustJSONText(t, created.Path) + `,"expected_checksum":` + mustJSONText(t, report.Package.Checksum) + `}`)
	deniedExecutor := &Executor{Registry: toolRegistry, Scope: scope}
	if _, err := deniedExecutor.Execute(context.Background(), SkillEnableToolName, args); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("missing approval err=%v", err)
	}
	approvals := 0
	executor := &Executor{Registry: toolRegistry, Scope: scope, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, Approval: ApprovalFunc(func(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
		approvals++
		if request.ToolName != SkillEnableToolName || !strings.Contains(string(request.Args), report.Package.Checksum) {
			t.Fatalf("approval request=%#v", request)
		}
		return ApprovalApproved, nil
	})}
	if err := os.WriteFile(filepath.Join(created.Path, DefaultSkillInstructions), []byte("Changed after review."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), SkillEnableToolName, args); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("changed package err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, DefaultSkillInstructions), []byte("Inspect the diff carefully."), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), SkillEnableToolName, args)
	if err != nil {
		t.Fatal(err)
	}
	if approvals != 2 || result.Truncated {
		t.Fatalf("approvals=%d result=%#v", approvals, result)
	}
	installed, err := skills.Inspect(context.Background(), "reviewer", "1.0.0")
	if err != nil || !installed.Enabled || installed.Checksum != report.Package.Checksum {
		t.Fatalf("installed=%#v err=%v", installed, err)
	}
}

func TestPluginBuilderScaffoldMustBeImplementedBeforeApproval(t *testing.T) {
	setPluginHelperEnvironment(t, "builder")
	scope := ExecutionScope{Root: t.TempDir()}
	extension := SkillExtension{Name: "plugin.builder", Command: "bin/extension", EnvAllowlist: []string{"BEE_EXTENSION_MODE", "BEE_EXTENSION_NAME", "BEE_TEST_BINARY"}, Tools: []string{"echo"}, StartupTimeoutSeconds: 2, RequestTimeoutSeconds: 2}
	input, err := json.Marshal(map[string]any{"name": "builder", "version": "1.0.0", "description": "Builder plugin", "extension": extension})
	if err != nil {
		t.Fatal(err)
	}
	createdOutput, err := (PluginCreateTool{Scope: scope}).Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var created PackageBuildResult
	if err := json.Unmarshal([]byte(createdOutput), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := (PluginTestTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"source":`+mustJSONText(t, created.Path)+`}`)); err == nil {
		t.Fatal("placeholder plugin unexpectedly passed live testing")
	}
	script := "#!/bin/sh\nexec \"$BEE_TEST_BINARY\" -test.run=TestExtensionHelperProcess --\n"
	if err := os.WriteFile(filepath.Join(created.Path, "bin", "extension"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	testTool := PluginTestTool{Scope: scope}
	testRegistry, err := NewRegistry(testTool)
	if err != nil {
		t.Fatal(err)
	}
	testArgs := json.RawMessage(`{"source":` + mustJSONText(t, created.Path) + `}`)
	if _, err := (&Executor{Registry: testRegistry, Scope: scope}).Execute(context.Background(), PluginTestToolName, testArgs); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("unapproved live test err=%v", err)
	}
	testOutput, err := testTool.Execute(context.Background(), testArgs)
	if err != nil {
		t.Fatal(err)
	}
	var report PluginTestReport
	if err := json.Unmarshal([]byte(testOutput), &report); err != nil {
		t.Fatal(err)
	}
	plugins, err := NewPluginRegistry(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	enable := PluginEnableTool{Scope: scope, Plugins: plugins}
	registry, err := NewRegistry(enable)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: scope, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalApproved, nil })}
	args := json.RawMessage(`{"source":` + mustJSONText(t, created.Path) + `,"expected_checksum":` + mustJSONText(t, report.Package.Checksum) + `}`)
	if _, err := executor.Execute(context.Background(), PluginEnableToolName, args); err != nil {
		t.Fatal(err)
	}
	installed, err := plugins.Inspect(context.Background(), "builder", "1.0.0")
	if err != nil || !installed.Enabled {
		t.Fatalf("installed=%#v err=%v", installed, err)
	}
}

func mustJSONText(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
