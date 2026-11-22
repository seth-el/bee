package bee

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCodingBackend struct{ name string }

func (backend *fakeCodingBackend) Run(_ context.Context, task CodingBackendTask, _ CodingBackendObserver) (CodingBackendCompletion, error) {
	return CodingBackendCompletion{
		ChildCompletionPacket: ChildCompletionPacket{TaskID: task.Task.TaskID, ParentID: task.Task.ParentID, Status: ChildSucceeded, Summary: backend.name},
		Backend:               backend.name,
	}, nil
}

func TestCodingBackendConfigDefaultsDisabledAndExplicitRoles(t *testing.T) {
	config := DefaultCodingBackendConfig()
	if len(config.Backends) != 1 || config.Backends[0].Name != CodingBackendNameLocalChild || config.Backends[0].Kind != CodingBackendKindLocalChild {
		t.Fatalf("default adapter examples = %#v", config.Backends)
	}
	if config.Enabled {
		t.Fatal("coding backends must remain disabled by default")
	}
	defaults := config.EffectiveDefaults()
	if defaults.Implementer != CodingBackendNameLocalChild || defaults.Reviewer != CodingBackendNameLocalChild || defaults.Scout != CodingBackendNameLocalChild {
		t.Fatalf("defaults = %#v", defaults)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.Enabled = true
	config.Backends = []CodingBackendDefinition{
		{Name: "acp-primary", Kind: CodingBackendKindACP, Enabled: true, Command: "acp-agent"},
		{Name: CodingBackendNameLocalChild, Kind: CodingBackendKindLocalChild, Enabled: true},
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCodingBackendConfigRejectsSecretEnvironmentKeys(t *testing.T) {
	config := DefaultCodingBackendConfig()
	config.Enabled = true
	config.Backends = []CodingBackendDefinition{{
		Name:    "acp-primary",
		Kind:    CodingBackendKindACP,
		Enabled: true,
		Process: ACPProcessConfig{Command: "acp-agent", EnvAllowlist: []string{"OPENAI_API_KEY"}},
	}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "secret-shaped") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestCodingBackendRegistryBuildsACPWithoutLaunchingProcess(t *testing.T) {
	config := DefaultCodingBackendConfig()
	config.Enabled = true
	config.Backends = []CodingBackendDefinition{
		{Name: "acp-primary", Kind: CodingBackendKindACP, Enabled: true, Command: "binary-that-is-not-installed", Args: []string{"--acp"}},
		{Name: CodingBackendNameLocalChild, Kind: CodingBackendKindLocalChild, Enabled: true},
	}
	registry, err := NewCodingBackendRegistry(config)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := registry.Build("acp-primary")
	if err != nil {
		t.Fatal(err)
	}
	acp, ok := backend.(*ACPBackend)
	if !ok {
		t.Fatalf("backend type = %T", backend)
	}
	if acp.BackendName != "acp-primary" || acp.Config.Command != "binary-that-is-not-installed" || len(acp.Config.Args) != 1 || acp.Config.Args[0] != "--acp" {
		t.Fatalf("ACP backend = %#v", acp)
	}
}

func TestCodingBackendRegistryRoutesRolesAndUserOverride(t *testing.T) {
	config := DefaultCodingBackendConfig()
	config.Enabled = true
	config.DefaultImplementer = "fake-implementer"
	config.DefaultReviewer = "fake-reviewer"
	config.DefaultScout = CodingBackendNameLocalChild
	config.Backends = []CodingBackendDefinition{
		{Name: "fake-implementer", Kind: CodingBackendKindACP, Enabled: true, Command: "unused"},
		{Name: "fake-reviewer", Kind: CodingBackendKindACP, Enabled: true, Command: "unused"},
		{Name: CodingBackendNameLocalChild, Kind: CodingBackendKindLocalChild, Enabled: true},
	}
	registry, err := NewCodingBackendRegistryWithFactories(config, map[string]CodingBackendFactory{
		"fake-implementer": func(definition CodingBackendDefinition) (CodingBackend, error) {
			if definition.Name != "fake-implementer" {
				t.Fatalf("definition = %#v", definition)
			}
			return &fakeCodingBackend{name: definition.Name}, nil
		},
		"fake-reviewer": func(definition CodingBackendDefinition) (CodingBackend, error) {
			return &fakeCodingBackend{name: definition.Name}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	implementer, err := registry.Route(CodingRoleImplementer, "")
	if err != nil {
		t.Fatal(err)
	}
	if implementer.BackendName != "fake-implementer" || !implementer.Enabled || implementer.Backend == nil {
		t.Fatalf("implementer route = %#v", implementer)
	}
	override, err := registry.Route(CodingRoleImplementer, "fake-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if override.BackendName != "fake-reviewer" {
		t.Fatalf("override route = %#v", override)
	}
	if _, err := registry.Route(CodingRoleReviewer, CodingBackendNameLocalChild); err == nil {
		t.Fatal("expected missing local child factory")
	} else if !strings.Contains(err.Error(), "no registered factory") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodingBackendRegistryDisabledCompatibility(t *testing.T) {
	registry, err := NewCodingBackendRegistry(DefaultCodingBackendConfig())
	if err != nil {
		t.Fatal(err)
	}
	route, err := registry.Route(CodingRoleScout, "")
	if err != nil {
		t.Fatal(err)
	}
	if route.Enabled || route.Backend != nil || route.BackendName != CodingBackendNameLocalChild {
		t.Fatalf("disabled route = %#v", route)
	}
	if _, err := registry.Resolve(CodingRoleScout, ""); !errors.Is(err, ErrCodingBackendsDisabled) {
		t.Fatalf("resolve error = %v", err)
	}
}

func TestCodingBackendRoleValidationAndGroupedDefaults(t *testing.T) {
	config := DefaultCodingBackendConfig()
	config.DefaultImplementer = ""
	config.DefaultReviewer = ""
	config.DefaultScout = ""
	config.Defaults = CodingBackendRoleDefaults{Implementer: "acp-primary", Reviewer: "acp-primary", Scout: CodingBackendNameLocalChild}
	if got := config.EffectiveDefaults(); got.Implementer != "acp-primary" || got.Reviewer != "acp-primary" || got.Scout != CodingBackendNameLocalChild {
		t.Fatalf("grouped defaults = %#v", got)
	}
	if _, err := config.ResolveName(CodingBackendRole("unknown"), ""); err == nil {
		t.Fatal("expected invalid role error")
	}
	if _, err := config.ResolveName(CodingRoleScout, "bad/name"); err == nil {
		t.Fatal("expected unsafe override error")
	}
}
