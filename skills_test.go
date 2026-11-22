package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSkillPackageSource(t *testing.T, parent, name, version, instructions string, required []string) string {
	t.Helper()
	root := filepath.Join(parent, name+"-"+strings.ReplaceAll(version, ".", "-"))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := SkillManifest{
		SchemaVersion: SkillSchemaVersion,
		Name:          name,
		Version:       version,
		Description:   "Test skill " + version,
		Instructions:  DefaultSkillInstructions,
		RequiredTools: append([]string(nil), required...),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SkillManifestFilename), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DefaultSkillInstructions), []byte(instructions), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSkillRegistryInstallEnableRollbackAndRemove(t *testing.T) {
	registry, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	registry.SetClock(func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) })
	sources := t.TempDir()
	v1 := writeSkillPackageSource(t, sources, "review", "1.0.0", "Review carefully.\n", []string{"read_file"})
	v2 := writeSkillPackageSource(t, sources, "review", "1.1.0", "Review carefully and run tests.\n", []string{"read_file"})

	installedV1, err := registry.Install(context.Background(), v1)
	if err != nil {
		t.Fatal(err)
	}
	if installedV1.Manifest.Version != "1.0.0" || installedV1.Enabled || installedV1.Checksum == "" {
		t.Fatalf("installed v1=%#v", installedV1)
	}
	if _, err := registry.Enable(context.Background(), "review", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Install(context.Background(), v2); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "review", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := registry.Rollback(context.Background(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Manifest.Version != "1.0.0" {
		t.Fatalf("rollback version=%q", rolledBack.Manifest.Version)
	}
	pkg, instructions, err := registry.LoadEnabled(context.Background(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.Version != "1.0.0" || instructions != "Review carefully.\n" {
		t.Fatalf("loaded pkg=%#v instructions=%q", pkg, instructions)
	}

	if _, err := registry.Enable(context.Background(), "review", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Remove(context.Background(), "review", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	packages, err := registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0].Manifest.Version != "1.1.0" || !packages[0].Enabled {
		t.Fatalf("packages=%#v", packages)
	}
	stateInfo, err := os.Stat(filepath.Join(registry.Root(), SkillStateFilename))
	if err != nil {
		t.Fatal(err)
	}
	if stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%v", stateInfo.Mode().Perm())
	}
	if err := registry.Remove(context.Background(), "review", "1.1.0"); err == nil || !strings.Contains(err.Error(), "active revision") {
		t.Fatalf("active removal err=%v", err)
	}
}

func TestSkillPackageValidationRejectsUnknownFieldsEscapesAndSymlinks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown", `{"schema_version":1,"name":"safe","version":"1.0.0","description":"safe","unexpected":true}`, "unknown field"},
		{"escape", `{"schema_version":1,"name":"safe","version":"1.0.0","description":"safe","instructions":"../outside.md"}`, "escapes"},
		{"version", `{"schema_version":1,"name":"safe","version":"latest","description":"safe"}`, "semantic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, SkillManifestFilename), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, DefaultSkillInstructions), []byte("instructions"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := TestSkillSource(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}

	root := writeSkillPackageSource(t, t.TempDir(), "linked", "1.0.0", "safe", nil)
	if err := os.Symlink(filepath.Join(root, DefaultSkillInstructions), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := TestSkillSource(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err=%v", err)
	}
}

func TestCompareSkillVersionsUsesSemanticVersionPrecedence(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
	}
	for index := 1; index < len(ordered); index++ {
		if compareSkillVersions(ordered[index-1], ordered[index]) >= 0 {
			t.Fatalf("expected %q before %q", ordered[index-1], ordered[index])
		}
	}
	if compareSkillVersions("1.0.0+build.1", "1.0.0+build.2") != 0 {
		t.Fatal("build metadata must not affect semantic version precedence")
	}
}

func TestSkillToolsListAndLoadEnabledInstructions(t *testing.T) {
	registry, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	source := writeSkillPackageSource(t, t.TempDir(), "review", "1.0.0", "Use read_file before making claims.\n", []string{"read_file"})
	if _, err := registry.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "review", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	tools, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "read_file", Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	listOutput, err := (SkillListTool{Skills: registry, Tools: tools}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var list SkillListResult
	if err := json.Unmarshal([]byte(listOutput), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Skills) != 1 || list.Skills[0].Name != "review" || len(list.Skills[0].MissingTools) != 0 {
		t.Fatalf("list=%#v", list)
	}
	loadOutput, err := (SkillLoadTool{Skills: registry, Tools: tools}).Execute(context.Background(), json.RawMessage(`{"name":"review"}`))
	if err != nil {
		t.Fatal(err)
	}
	var loaded SkillLoadResult
	if err := json.Unmarshal([]byte(loadOutput), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Version != "1.0.0" || !strings.Contains(loaded.Instructions, "Use read_file") || !strings.Contains(loaded.Trust, "does not override") {
		t.Fatalf("loaded=%#v", loaded)
	}
	if _, err := (SkillLoadTool{Skills: registry, Tools: newSkillTestRegistry(t)}).Execute(context.Background(), json.RawMessage(`{"name":"review"}`)); err == nil || !strings.Contains(err.Error(), "unavailable tools") {
		t.Fatalf("missing tool err=%v", err)
	}
}

func TestSkillLoadPreservesBoundedInstructionsPastGenericExecutorLimit(t *testing.T) {
	registry, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	instructions := strings.Repeat("<", 20<<10)
	source := writeSkillPackageSource(t, t.TempDir(), "large", "1.0.0", instructions, []string{"read_file"})
	if _, err := registry.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "large", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	tools, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "read_file", Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.Register(SkillLoadTool{Skills: registry, Tools: tools}); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: tools, Scope: ExecutionScope{Root: t.TempDir(), MaxOutputBytes: 64}}
	result, err := executor.Execute(context.Background(), SkillLoadToolName, json.RawMessage(`{"name":"large"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated {
		t.Fatal("skill_load was truncated by the generic executor limit")
	}
	var loaded SkillLoadResult
	if err := json.Unmarshal([]byte(result.Output), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Instructions != instructions {
		t.Fatalf("loaded instruction bytes=%d, want %d", len(loaded.Instructions), len(instructions))
	}
}

func newSkillTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestComposeGeneralAssistantExposesEnabledSkills(t *testing.T) {
	registry, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	source := writeSkillPackageSource(t, t.TempDir(), "review", "1.0.0", "Inspect before reviewing.\n", []string{"read_file"})
	if _, err := registry.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "review", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	sessions := mustAssistantStore(t)
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store:         sessions,
		JobStorePath:  filepath.Join(t.TempDir(), "jobs.json"),
		TodoStorePath: filepath.Join(t.TempDir(), "todos.json"),
		ScopeRoot:     t.TempDir(),
		MemoryRoot:    filepath.Join(t.TempDir(), "memory"),
		ArtifactStore: artifacts,
		SkillRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{SkillListToolName, SkillLoadToolName} {
		if _, ok := app.Executor.Registry.Lookup(name); !ok {
			t.Fatalf("missing composed skill tool %q", name)
		}
	}
	if len(app.Capabilities.SkillNames) != 1 || app.Capabilities.SkillNames[0] != "review" {
		t.Fatalf("skill capabilities=%#v", app.Capabilities.SkillNames)
	}
}

func TestExecutableSkillUsesExtensionBoundaryAndExactToolAllowlist(t *testing.T) {
	t.Setenv("BEE_EXTENSION_MODE", "normal")
	t.Setenv("BEE_EXTENSION_NAME", "skill.echoer")
	t.Setenv("BEE_TEST_BINARY", os.Args[0])
	source := writeSkillPackageSource(t, t.TempDir(), "echoer", "1.0.0", "Call echo when asked to echo.\n", []string{"echo"})
	binDir := filepath.Join(source, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec \"$BEE_TEST_BINARY\" -test.run=TestExtensionHelperProcess --\n"
	if err := os.WriteFile(filepath.Join(binDir, "extension"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := SkillManifest{
		SchemaVersion: SkillSchemaVersion,
		Name:          "echoer",
		Version:       "1.0.0",
		Description:   "Echo through an extension",
		Instructions:  DefaultSkillInstructions,
		RequiredTools: []string{"echo"},
		Extension: &SkillExtension{
			Name:                  "skill.echoer",
			Command:               "bin/extension",
			EnvAllowlist:          []string{"BEE_EXTENSION_MODE", "BEE_EXTENSION_NAME", "BEE_TEST_BINARY"},
			Tools:                 []string{"echo"},
			StartupTimeoutSeconds: 2,
			RequestTimeoutSeconds: 2,
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, SkillManifestFilename), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewSkillRegistry(filepath.Join(t.TempDir(), "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Enable(context.Background(), "echoer", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	report, err := registry.TestInstalled(context.Background(), "echoer", "1.0.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExtensionTest != "initialize handshake passed" || len(report.Tools) != 1 || report.Tools[0] != "echo" {
		t.Fatalf("report=%#v", report)
	}

	sessions := mustAssistantStore(t)
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{
		Store:         sessions,
		JobStorePath:  filepath.Join(t.TempDir(), "jobs.json"),
		TodoStorePath: filepath.Join(t.TempDir(), "todos.json"),
		ScopeRoot:     t.TempDir(),
		MemoryRoot:    filepath.Join(t.TempDir(), "memory"),
		ArtifactStore: artifacts,
		SkillRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.StartExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.CloseExtensions(context.Background()) })
	echo, ok := app.Executor.Registry.Lookup("echo")
	if !ok {
		t.Fatal("skill extension tool was not registered")
	}
	output, err := echo.Execute(context.Background(), json.RawMessage(`{"text":"hello"}`))
	if err != nil || output != "hello" {
		t.Fatalf("echo output=%q err=%v", output, err)
	}
	if err := validatePackageExtensionCapabilities([]string{"different"}, app.ExtensionHosts[0].Capabilities()); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("allowlist mismatch err=%v", err)
	}
}
