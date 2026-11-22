package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	PluginSchemaVersion    = 1
	PluginManifestFilename = "plugin.json"
)

// PluginExtension shares the external-process declaration used by executable
// skills while retaining plugin-specific manifest naming.
type PluginExtension = SkillExtension

// PluginManifest describes one versioned, package-contained external process.
// Plugins use the existing bee.extension.v1 boundary; Bee never loads package
// code into its own process.
type PluginManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	Description   string           `json:"description"`
	Extension     *PluginExtension `json:"extension"`
}

func (manifest PluginManifest) Validate() error {
	if manifest.SchemaVersion != PluginSchemaVersion {
		return fmt.Errorf("plugin schema_version must be %d", PluginSchemaVersion)
	}
	if err := validateSkillName(manifest.Name); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	if err := validateSkillVersion(manifest.Version); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return errors.New("plugin description is required")
	}
	if len(manifest.Description) > SkillMaxDescriptionBytes {
		return fmt.Errorf("plugin description exceeds %d bytes", SkillMaxDescriptionBytes)
	}
	for _, value := range manifest.Description {
		if unicode.IsControl(value) {
			return errors.New("plugin description contains control characters")
		}
	}
	if manifest.Extension == nil {
		return errors.New("plugin extension is required")
	}
	if err := manifest.Extension.validateNamed("plugin." + manifest.Name); err != nil {
		return fmt.Errorf("plugin extension: %w", err)
	}
	return nil
}

type PluginPackage struct {
	Manifest  PluginManifest `json:"manifest"`
	Checksum  string         `json:"checksum"`
	Path      string         `json:"path,omitempty"`
	Enabled   bool           `json:"enabled"`
	Active    bool           `json:"active"`
	Installed bool           `json:"installed"`
}

type PluginTestReport struct {
	Valid         bool          `json:"valid"`
	Package       PluginPackage `json:"package"`
	Files         int           `json:"files"`
	PackageBytes  int64         `json:"package_bytes"`
	ExtensionTest string        `json:"extension_test,omitempty"`
	Tools         []string      `json:"extension_tools,omitempty"`
}

type PluginExtensionActivation struct {
	PluginName    string
	PluginVersion string
	Config        ExtensionConfig
	ExpectedTools []string
}

// PluginRegistry is a versioned registry rooted independently from skills.
// It reuses the registry lock and activation-state format, but validates
// plugin.json packages and requires an extension for every revision.
type PluginRegistry struct{ backend *SkillRegistry }

func NewDefaultPluginRegistry() (*PluginRegistry, error) {
	root, err := DefaultBeePluginsRoot()
	if err != nil {
		return nil, err
	}
	return NewPluginRegistry(root)
}

func OpenDefaultPluginRegistry() (*PluginRegistry, bool, error) {
	root, err := DefaultBeePluginsRoot()
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("inspect plugin registry: %w", err)
	}
	registry, err := NewPluginRegistry(root)
	return registry, err == nil, err
}

func NewPluginRegistry(root string) (*PluginRegistry, error) {
	backend, err := NewSkillRegistry(root)
	if err != nil {
		return nil, err
	}
	return &PluginRegistry{backend: backend}, nil
}

func (registry *PluginRegistry) Root() string {
	if registry == nil || registry.backend == nil {
		return ""
	}
	return registry.backend.Root()
}

func (registry *PluginRegistry) SetClock(clock func() time.Time) {
	if registry != nil && registry.backend != nil {
		registry.backend.SetClock(clock)
	}
}

func (registry *PluginRegistry) packagePath(name, version string) string {
	return filepath.Join(registry.Root(), name, version)
}

func (registry *PluginRegistry) withLock(ctx context.Context, fn func() error) error {
	if registry == nil || registry.backend == nil {
		return errors.New("plugin registry is not configured")
	}
	return registry.backend.withLock(ctx, fn)
}

func (registry *PluginRegistry) Install(ctx context.Context, source string) (PluginPackage, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return PluginPackage{}, errors.New("plugin package source is required")
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return PluginPackage{}, fmt.Errorf("resolve plugin package source: %w", err)
	}
	sourcePackage, sourceStats, err := loadPluginPackage(filepath.Clean(abs), false)
	if err != nil {
		return PluginPackage{}, err
	}
	var installed PluginPackage
	err = registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		packages, err := registry.listPackages(state)
		if err != nil {
			return err
		}
		if len(packages) >= SkillMaxInstalledPackages {
			return fmt.Errorf("plugin registry exceeds %d installed packages", SkillMaxInstalledPackages)
		}
		destination := registry.packagePath(sourcePackage.Manifest.Name, sourcePackage.Manifest.Version)
		if _, statErr := os.Lstat(destination); statErr == nil {
			return fmt.Errorf("plugin %s@%s is already installed", sourcePackage.Manifest.Name, sourcePackage.Manifest.Version)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect plugin destination: %w", statErr)
		}
		if err := ensurePrivateDir(filepath.Dir(destination), 0o700); err != nil {
			return fmt.Errorf("create plugin name directory: %w", err)
		}
		stage, err := os.MkdirTemp(registry.Root(), ".install-")
		if err != nil {
			return fmt.Errorf("create plugin install stage: %w", err)
		}
		stageLive := true
		defer func() {
			if stageLive {
				_ = os.RemoveAll(stage)
			}
		}()
		if err := os.Chmod(stage, 0o700); err != nil {
			return err
		}
		if err := copySkillTree(sourcePackage.Path, stage); err != nil {
			return err
		}
		staged, stagedStats, err := loadPluginPackage(stage, true)
		if err != nil {
			return fmt.Errorf("validate staged plugin: %w", err)
		}
		if staged.Checksum != sourcePackage.Checksum || stagedStats.files != sourceStats.files || stagedStats.bytes != sourceStats.bytes {
			return errors.New("plugin package changed while it was being installed")
		}
		if err := os.Rename(stage, destination); err != nil {
			return fmt.Errorf("activate installed plugin package: %w", err)
		}
		stageLive = false
		installed = staged
		installed.Path = destination
		installed.Installed = true
		return nil
	})
	return installed, err
}

func (registry *PluginRegistry) List(ctx context.Context) ([]PluginPackage, error) {
	var packages []PluginPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		packages, err = registry.listPackages(state)
		return err
	})
	return packages, err
}

func (registry *PluginRegistry) listPackages(state SkillRegistryState) ([]PluginPackage, error) {
	entries, err := os.ReadDir(registry.Root())
	if err != nil {
		return nil, fmt.Errorf("list plugin registry: %w", err)
	}
	packages := make([]PluginPackage, 0)
	found := make(map[string]map[string]struct{})
	for _, nameEntry := range entries {
		if !nameEntry.IsDir() || strings.HasPrefix(nameEntry.Name(), ".") {
			continue
		}
		if err := validateSkillName(nameEntry.Name()); err != nil {
			return nil, fmt.Errorf("invalid plugin directory %q: %w", nameEntry.Name(), err)
		}
		versions, err := os.ReadDir(filepath.Join(registry.Root(), nameEntry.Name()))
		if err != nil {
			return nil, err
		}
		for _, versionEntry := range versions {
			if !versionEntry.IsDir() || strings.HasPrefix(versionEntry.Name(), ".") {
				continue
			}
			if len(packages) >= SkillMaxInstalledPackages {
				return nil, fmt.Errorf("plugin registry exceeds %d installed packages", SkillMaxInstalledPackages)
			}
			loaded, _, err := loadPluginPackage(registry.packagePath(nameEntry.Name(), versionEntry.Name()), true)
			if err != nil {
				return nil, fmt.Errorf("load plugin %s@%s: %w", nameEntry.Name(), versionEntry.Name(), err)
			}
			if loaded.Manifest.Name != nameEntry.Name() || loaded.Manifest.Version != versionEntry.Name() {
				return nil, fmt.Errorf("plugin package path does not match manifest for %s@%s", nameEntry.Name(), versionEntry.Name())
			}
			activation := state.Skills[loaded.Manifest.Name]
			loaded.Enabled = activation.Enabled && activation.ActiveVersion == loaded.Manifest.Version
			loaded.Active = activation.ActiveVersion == loaded.Manifest.Version
			loaded.Installed = true
			packages = append(packages, loaded)
			if found[loaded.Manifest.Name] == nil {
				found[loaded.Manifest.Name] = make(map[string]struct{})
			}
			found[loaded.Manifest.Name][loaded.Manifest.Version] = struct{}{}
		}
	}
	for name, activation := range state.Skills {
		if activation.ActiveVersion != "" {
			if _, exists := found[name][activation.ActiveVersion]; !exists {
				return nil, fmt.Errorf("plugin state references missing active package %s@%s", name, activation.ActiveVersion)
			}
		}
		if activation.PreviousVersion != "" {
			if _, exists := found[name][activation.PreviousVersion]; !exists {
				return nil, fmt.Errorf("plugin state references missing rollback package %s@%s", name, activation.PreviousVersion)
			}
		}
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Manifest.Name != packages[j].Manifest.Name {
			return packages[i].Manifest.Name < packages[j].Manifest.Name
		}
		return compareSkillVersions(packages[i].Manifest.Version, packages[j].Manifest.Version) > 0
	})
	return packages, nil
}

func (registry *PluginRegistry) Inspect(ctx context.Context, name, version string) (PluginPackage, error) {
	if err := validateSkillName(name); err != nil {
		return PluginPackage{}, err
	}
	if version != "" {
		if err := validateSkillVersion(version); err != nil {
			return PluginPackage{}, err
		}
	}
	var result PluginPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		selected := version
		if selected == "" {
			selected = state.Skills[name].ActiveVersion
			if selected == "" {
				versions, err := registry.installedVersions(name)
				if err != nil {
					return err
				}
				if len(versions) == 0 {
					return fmt.Errorf("plugin %q is not installed", name)
				}
				selected = versions[0]
			}
		}
		result, _, err = loadPluginPackage(registry.packagePath(name, selected), true)
		if err != nil {
			return err
		}
		activation := state.Skills[name]
		result.Enabled = activation.Enabled && activation.ActiveVersion == selected
		result.Active = activation.ActiveVersion == selected
		result.Installed = true
		return nil
	})
	return result, err
}

func (registry *PluginRegistry) Enable(ctx context.Context, name, version string) (PluginPackage, error) {
	return registry.enable(ctx, name, version, "")
}

func (registry *PluginRegistry) enable(ctx context.Context, name, version, expectedChecksum string) (PluginPackage, error) {
	if err := validateSkillName(name); err != nil {
		return PluginPackage{}, err
	}
	if version != "" {
		if err := validateSkillVersion(version); err != nil {
			return PluginPackage{}, err
		}
	}
	var result PluginPackage
	err := registry.withLock(ctx, func() error {
		selected := version
		if selected == "" {
			versions, err := registry.installedVersions(name)
			if err != nil {
				return err
			}
			if len(versions) == 0 {
				return fmt.Errorf("plugin %q is not installed", name)
			}
			selected = versions[0]
		}
		loaded, _, err := loadPluginPackage(registry.packagePath(name, selected), true)
		if err != nil {
			return err
		}
		if expectedChecksum != "" && loaded.Checksum != expectedChecksum {
			return fmt.Errorf("installed plugin checksum %s does not match approved checksum %s", loaded.Checksum, expectedChecksum)
		}
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		activation := state.Skills[name]
		if activation.ActiveVersion != "" && activation.ActiveVersion != selected {
			activation.PreviousVersion = activation.ActiveVersion
		}
		activation.ActiveVersion = selected
		activation.Enabled = true
		activation.UpdatedAt = registry.backend.clock().UTC()
		state.Skills[name] = activation
		if err := registry.backend.saveState(state); err != nil {
			return err
		}
		loaded.Enabled, loaded.Active, loaded.Installed = true, true, true
		result = loaded
		return nil
	})
	return result, err
}

func (registry *PluginRegistry) Disable(ctx context.Context, name string) (SkillActivation, error) {
	if err := validateSkillName(name); err != nil {
		return SkillActivation{}, err
	}
	var result SkillActivation
	err := registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		activation, exists := state.Skills[name]
		if !exists || activation.ActiveVersion == "" {
			return fmt.Errorf("plugin %q has not been enabled", name)
		}
		activation.Enabled = false
		activation.UpdatedAt = registry.backend.clock().UTC()
		state.Skills[name] = activation
		if err := registry.backend.saveState(state); err != nil {
			return err
		}
		result = activation
		return nil
	})
	return result, err
}

func (registry *PluginRegistry) Rollback(ctx context.Context, name string) (PluginPackage, error) {
	if err := validateSkillName(name); err != nil {
		return PluginPackage{}, err
	}
	var result PluginPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		activation, exists := state.Skills[name]
		if !exists || !activation.Enabled || activation.ActiveVersion == "" {
			return fmt.Errorf("plugin %q is not enabled", name)
		}
		if activation.PreviousVersion == "" {
			return fmt.Errorf("plugin %q has no previous version", name)
		}
		loaded, _, err := loadPluginPackage(registry.packagePath(name, activation.PreviousVersion), true)
		if err != nil {
			return fmt.Errorf("previous plugin version is unavailable: %w", err)
		}
		activation.ActiveVersion, activation.PreviousVersion = activation.PreviousVersion, activation.ActiveVersion
		activation.UpdatedAt = registry.backend.clock().UTC()
		state.Skills[name] = activation
		if err := registry.backend.saveState(state); err != nil {
			return err
		}
		loaded.Enabled, loaded.Active, loaded.Installed = true, true, true
		result = loaded
		return nil
	})
	return result, err
}

func (registry *PluginRegistry) CheckRemove(ctx context.Context, name, version string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if err := validateSkillVersion(version); err != nil {
		return err
	}
	return registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		if state.Skills[name].ActiveVersion == version {
			return fmt.Errorf("plugin %s@%s is the active revision", name, version)
		}
		_, _, err = loadPluginPackage(registry.packagePath(name, version), true)
		return err
	})
}

func (registry *PluginRegistry) Remove(ctx context.Context, name, version string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if err := validateSkillVersion(version); err != nil {
		return err
	}
	return registry.withLock(ctx, func() error {
		state, err := registry.backend.loadState()
		if err != nil {
			return err
		}
		activation := state.Skills[name]
		if activation.ActiveVersion == version {
			return fmt.Errorf("plugin %s@%s is the active revision", name, version)
		}
		path := registry.packagePath(name, version)
		if _, _, err := loadPluginPackage(path, true); err != nil {
			return err
		}
		trash := filepath.Join(registry.Root(), fmt.Sprintf(".remove-%s-%d", name, registry.backend.clock().UnixNano()))
		if err := os.Rename(path, trash); err != nil {
			return fmt.Errorf("stage plugin removal: %w", err)
		}
		if activation.PreviousVersion == version {
			previous := activation
			activation.PreviousVersion = ""
			activation.UpdatedAt = registry.backend.clock().UTC()
			state.Skills[name] = activation
			if err := registry.backend.saveState(state); err != nil {
				_ = os.Rename(trash, path)
				return err
			}
			if err := os.RemoveAll(trash); err != nil {
				_ = os.Rename(trash, path)
				state.Skills[name] = previous
				_ = registry.backend.saveState(state)
				return fmt.Errorf("remove plugin package: %w", err)
			}
		} else if err := os.RemoveAll(trash); err != nil {
			_ = os.Rename(trash, path)
			return fmt.Errorf("remove plugin package: %w", err)
		}
		if entries, readErr := os.ReadDir(filepath.Dir(path)); readErr == nil && len(entries) == 0 {
			_ = os.Remove(filepath.Dir(path))
		}
		return nil
	})
}

func (registry *PluginRegistry) Enabled(ctx context.Context) ([]PluginPackage, error) {
	packages, err := registry.List(ctx)
	if err != nil {
		return nil, err
	}
	enabled := packages[:0]
	for _, pkg := range packages {
		if pkg.Enabled {
			enabled = append(enabled, pkg)
		}
	}
	return enabled, nil
}

func (registry *PluginRegistry) EnabledExtensions(ctx context.Context) ([]PluginExtensionActivation, error) {
	packages, err := registry.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]PluginExtensionActivation, 0, len(packages))
	seen := make(map[string]struct{})
	for _, pkg := range packages {
		config, err := pluginExtensionConfig(pkg)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[config.Name]; exists {
			return nil, fmt.Errorf("duplicate enabled plugin extension %q", config.Name)
		}
		seen[config.Name] = struct{}{}
		result = append(result, PluginExtensionActivation{PluginName: pkg.Manifest.Name, PluginVersion: pkg.Manifest.Version, Config: config, ExpectedTools: append([]string(nil), pkg.Manifest.Extension.Tools...)})
	}
	return result, nil
}

func (registry *PluginRegistry) TestInstalled(ctx context.Context, name, version string, startExtension bool) (PluginTestReport, error) {
	pkg, err := registry.Inspect(ctx, name, version)
	if err != nil {
		return PluginTestReport{}, err
	}
	return testPluginPackage(ctx, pkg, startExtension)
}

func TestPluginSource(ctx context.Context, source string, startExtension bool) (PluginTestReport, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return PluginTestReport{}, err
	}
	pkg, _, err := loadPluginPackage(filepath.Clean(abs), false)
	if err != nil {
		return PluginTestReport{}, err
	}
	return testPluginPackage(ctx, pkg, startExtension)
}

func testPluginPackage(ctx context.Context, pkg PluginPackage, startExtension bool) (PluginTestReport, error) {
	_, stats, err := loadPluginPackage(pkg.Path, pkg.Installed)
	if err != nil {
		return PluginTestReport{}, err
	}
	report := PluginTestReport{Valid: true, Package: pkg, Files: stats.files, PackageBytes: stats.bytes, Tools: append([]string(nil), pkg.Manifest.Extension.Tools...)}
	if !startExtension {
		report.ExtensionTest = "static validation only"
		return report, nil
	}
	config, err := pluginExtensionConfig(pkg)
	if err != nil {
		return PluginTestReport{}, err
	}
	host, err := NewExtensionHost(config)
	if err != nil {
		return PluginTestReport{}, err
	}
	defer host.Close(context.Background())
	if err := host.Start(ctx); err != nil {
		return PluginTestReport{}, err
	}
	if err := validatePackageExtensionCapabilities(pkg.Manifest.Extension.Tools, host.Capabilities()); err != nil {
		return PluginTestReport{}, err
	}
	report.ExtensionTest = "initialize handshake passed"
	report.Tools = host.ToolNames()
	return report, nil
}

func pluginExtensionConfig(pkg PluginPackage) (ExtensionConfig, error) {
	if pkg.Manifest.Extension == nil {
		return ExtensionConfig{}, errors.New("plugin has no extension")
	}
	extension := pkg.Manifest.Extension
	command, _ := validateSkillRelativePath("plugin extension command", extension.Command)
	commandPath := filepath.Join(pkg.Path, command)
	info, err := os.Lstat(commandPath)
	if err != nil {
		return ExtensionConfig{}, fmt.Errorf("inspect plugin extension command: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return ExtensionConfig{}, errors.New("plugin extension command must be a package-contained executable regular file")
	}
	workingDir := pkg.Path
	if extension.WorkingDir != "" {
		relative, _ := validateSkillRelativePath("plugin extension working_dir", extension.WorkingDir)
		workingDir = filepath.Join(pkg.Path, relative)
		info, err := os.Lstat(workingDir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ExtensionConfig{}, errors.New("plugin extension working_dir must be a package-contained real directory")
		}
	}
	config := ExtensionConfig{
		Name: extension.Name, Enabled: true, Command: commandPath,
		Args: append([]string(nil), extension.Args...), WorkingDir: workingDir,
		EnvAllowlist:          append([]string(nil), extension.EnvAllowlist...),
		StartupTimeoutSeconds: extension.StartupTimeoutSeconds, RequestTimeoutSeconds: extension.RequestTimeoutSeconds,
		MaxFrameBytes: extension.MaxFrameBytes, MaxMessageBytes: extension.MaxMessageBytes,
	}
	if err := config.Validate(); err != nil {
		return ExtensionConfig{}, err
	}
	return config, nil
}

func (registry *PluginRegistry) installedVersions(name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(registry.Root(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := validateSkillVersion(entry.Name()); err != nil {
			return nil, fmt.Errorf("invalid installed plugin version %q", entry.Name())
		}
		versions = append(versions, entry.Name())
	}
	sort.Slice(versions, func(i, j int) bool { return compareSkillVersions(versions[i], versions[j]) > 0 })
	return versions, nil
}

func loadPluginPackage(root string, requirePrivate bool) (PluginPackage, skillPackageStats, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return PluginPackage{}, skillPackageStats{}, fmt.Errorf("inspect plugin package: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return PluginPackage{}, skillPackageStats{}, errors.New("plugin package must be a real directory")
	}
	if requirePrivate && info.Mode().Perm()&0o077 != 0 {
		return PluginPackage{}, skillPackageStats{}, errors.New("installed plugin package directory must be owner-only")
	}
	manifestData, err := readSkillFile(filepath.Join(root, PluginManifestFilename), requirePrivate, SkillMaxManifestBytes)
	if err != nil {
		return PluginPackage{}, skillPackageStats{}, fmt.Errorf("read plugin manifest: %w", err)
	}
	var manifest PluginManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return PluginPackage{}, skillPackageStats{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return PluginPackage{}, skillPackageStats{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return PluginPackage{}, skillPackageStats{}, err
	}
	checksum, stats, err := checksumSkillTree(root, requirePrivate)
	if err != nil {
		return PluginPackage{}, stats, err
	}
	pkg := PluginPackage{Manifest: manifest, Checksum: checksum, Path: root, Installed: requirePrivate}
	if _, err := pluginExtensionConfig(pkg); err != nil {
		return PluginPackage{}, stats, err
	}
	return pkg, stats, nil
}
