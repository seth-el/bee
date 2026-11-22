package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	SkillSchemaVersion        = 1
	SkillStateSchemaVersion   = 1
	SkillManifestFilename     = "skill.json"
	DefaultSkillInstructions  = "SKILL.md"
	SkillStateFilename        = "state.json"
	SkillMaxNameBytes         = 64
	SkillMaxVersionBytes      = 96
	SkillMaxDescriptionBytes  = 512
	SkillMaxManifestBytes     = 64 << 10
	SkillMaxStateBytes        = 1 << 20
	SkillMaxInstructionsBytes = 64 << 10
	// JSON escaping can expand bounded instruction text (for example, '<' is
	// emitted as "\u003c"). Keep skill_load above the generic executor limit,
	// while retaining a finite ceiling below the global self-bounded maximum.
	SkillMaxLoadOutputBytes   = 512 << 10
	SkillMaxPackageBytes      = 64 << 20
	SkillMaxPackageFiles      = 256
	SkillMaxTools             = 64
	SkillMaxInstalledPackages = 1024
)

// SkillManifest is the immutable contract stored in each versioned package.
// RequiredTools and OptionalTools describe dependencies; they never grant
// authority. Optional executable capabilities use Bee's existing extension
// protocol rather than loading code into the Bee process.
type SkillManifest struct {
	SchemaVersion int             `json:"schema_version"`
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	Description   string          `json:"description"`
	Instructions  string          `json:"instructions,omitempty"`
	RequiredTools []string        `json:"required_tools,omitempty"`
	OptionalTools []string        `json:"optional_tools,omitempty"`
	Extension     *SkillExtension `json:"extension,omitempty"`
}

// SkillExtension declares a package-contained bee.extension.v1 process. Its
// tool list is an allowlist checked against the initialize handshake before
// any tool is registered.
type SkillExtension struct {
	Name                  string   `json:"name"`
	Command               string   `json:"command"`
	Args                  []string `json:"args,omitempty"`
	WorkingDir            string   `json:"working_dir,omitempty"`
	EnvAllowlist          []string `json:"env_allowlist,omitempty"`
	Tools                 []string `json:"tools"`
	StartupTimeoutSeconds int      `json:"startup_timeout_seconds,omitempty"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds,omitempty"`
	MaxFrameBytes         int      `json:"max_frame_bytes,omitempty"`
	MaxMessageBytes       int      `json:"max_message_bytes,omitempty"`
}

func (manifest SkillManifest) Validate() error {
	if manifest.SchemaVersion != SkillSchemaVersion {
		return fmt.Errorf("skill schema_version must be %d", SkillSchemaVersion)
	}
	if err := validateSkillName(manifest.Name); err != nil {
		return err
	}
	if err := validateSkillVersion(manifest.Version); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return errors.New("skill description is required")
	}
	if len(manifest.Description) > SkillMaxDescriptionBytes {
		return fmt.Errorf("skill description exceeds %d bytes", SkillMaxDescriptionBytes)
	}
	for _, value := range manifest.Description {
		if unicode.IsControl(value) {
			return errors.New("skill description contains control characters")
		}
	}
	instructions := manifest.Instructions
	if instructions == "" {
		instructions = DefaultSkillInstructions
	}
	if _, err := validateSkillRelativePath("skill instructions", instructions); err != nil {
		return err
	}
	seenTools := make(map[string]string, len(manifest.RequiredTools)+len(manifest.OptionalTools))
	for label, names := range map[string][]string{"required": manifest.RequiredTools, "optional": manifest.OptionalTools} {
		if len(names) > SkillMaxTools {
			return fmt.Errorf("skill %s tools exceed %d entries", label, SkillMaxTools)
		}
		for _, name := range names {
			if err := (ToolDefinition{Name: name}).Validate(); err != nil {
				return fmt.Errorf("skill %s tool: %w", label, err)
			}
			if previous, exists := seenTools[name]; exists {
				return fmt.Errorf("skill tool %q is duplicated in %s and %s tools", name, previous, label)
			}
			seenTools[name] = label
		}
	}
	if manifest.Extension != nil {
		if err := manifest.Extension.validate(manifest.Name); err != nil {
			return err
		}
	}
	return nil
}

func (extension SkillExtension) validate(skillName string) error {
	return extension.validateNamed("skill." + skillName)
}

func (extension SkillExtension) validateNamed(wantName string) error {
	if extension.Name != wantName {
		return fmt.Errorf("package extension name must be %q", wantName)
	}
	if _, err := validateSkillRelativePath("skill extension command", extension.Command); err != nil {
		return err
	}
	if extension.WorkingDir != "" {
		if _, err := validateSkillRelativePath("skill extension working_dir", extension.WorkingDir); err != nil {
			return err
		}
	}
	if len(extension.Tools) == 0 {
		return errors.New("skill extension tools are required")
	}
	if len(extension.Tools) > SkillMaxTools {
		return fmt.Errorf("skill extension tools exceed %d entries", SkillMaxTools)
	}
	seen := make(map[string]struct{}, len(extension.Tools))
	for _, name := range extension.Tools {
		if err := (ToolDefinition{Name: name}).Validate(); err != nil {
			return fmt.Errorf("skill extension tool: %w", err)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate skill extension tool %q", name)
		}
		seen[name] = struct{}{}
	}
	if len(extension.Args) > 64 {
		return errors.New("skill extension args exceed 64 entries")
	}
	for _, arg := range extension.Args {
		if strings.ContainsRune(arg, '\x00') || len(arg) > 4096 {
			return errors.New("skill extension argument is invalid")
		}
	}
	config := ExtensionConfig{
		Name:                  extension.Name,
		Enabled:               true,
		Command:               extension.Command,
		Args:                  extension.Args,
		WorkingDir:            extension.WorkingDir,
		EnvAllowlist:          extension.EnvAllowlist,
		StartupTimeoutSeconds: extension.StartupTimeoutSeconds,
		RequestTimeoutSeconds: extension.RequestTimeoutSeconds,
		MaxFrameBytes:         extension.MaxFrameBytes,
		MaxMessageBytes:       extension.MaxMessageBytes,
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("skill extension: %w", err)
	}
	return nil
}

func validateSkillName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || len(name) > SkillMaxNameBytes {
		return fmt.Errorf("skill name must contain 1-%d lowercase ASCII bytes", SkillMaxNameBytes)
	}
	for index, value := range []byte(name) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || index > 0 && (value == '-' || value == '_') {
			continue
		}
		return fmt.Errorf("skill name %q must use lowercase letters, digits, hyphens, or underscores", name)
	}
	return nil
}

func validateSkillVersion(version string) error {
	if version == "" || version != strings.TrimSpace(version) || len(version) > SkillMaxVersionBytes {
		return fmt.Errorf("skill version must contain 1-%d bytes", SkillMaxVersionBytes)
	}
	main := version
	if plus := strings.IndexByte(main, '+'); plus >= 0 {
		if !validSemverIdentifiers(main[plus+1:], false) {
			return fmt.Errorf("skill version %q has invalid build metadata", version)
		}
		main = main[:plus]
	}
	if dash := strings.IndexByte(main, '-'); dash >= 0 {
		if !validSemverIdentifiers(main[dash+1:], true) {
			return fmt.Errorf("skill version %q has invalid prerelease metadata", version)
		}
		main = main[:dash]
	}
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return fmt.Errorf("skill version %q must be semantic MAJOR.MINOR.PATCH", version)
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return fmt.Errorf("skill version %q must be semantic MAJOR.MINOR.PATCH", version)
		}
		if _, err := strconv.ParseUint(part, 10, 31); err != nil {
			return fmt.Errorf("skill version %q must be semantic MAJOR.MINOR.PATCH", version)
		}
	}
	return nil
}

func validSemverIdentifiers(value string, prerelease bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, char := range identifier {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' {
				continue
			}
			return false
		}
		if prerelease && len(identifier) > 1 && identifier[0] == '0' && skillDigitsOnly(identifier) {
			return false
		}
	}
	return true
}

func validateSkillRelativePath(label, value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') || filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be a non-empty package-relative path", label)
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes the skill package", label)
	}
	return clean, nil
}

type SkillActivation struct {
	Enabled         bool      `json:"enabled"`
	ActiveVersion   string    `json:"active_version,omitempty"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type SkillRegistryState struct {
	SchemaVersion int                        `json:"schema_version"`
	Skills        map[string]SkillActivation `json:"skills,omitempty"`
}

type SkillPackage struct {
	Manifest  SkillManifest `json:"manifest"`
	Checksum  string        `json:"checksum"`
	Path      string        `json:"path,omitempty"`
	Enabled   bool          `json:"enabled"`
	Active    bool          `json:"active"`
	Installed bool          `json:"installed"`
}

type SkillTestReport struct {
	Valid         bool         `json:"valid"`
	Package       SkillPackage `json:"package"`
	Instructions  int          `json:"instruction_bytes"`
	Files         int          `json:"files"`
	PackageBytes  int64        `json:"package_bytes"`
	ExtensionTest string       `json:"extension_test,omitempty"`
	Tools         []string     `json:"extension_tools,omitempty"`
}

type SkillExtensionActivation struct {
	SkillName     string
	SkillVersion  string
	Config        ExtensionConfig
	ExpectedTools []string
}

type SkillRegistry struct {
	root  string
	mu    sync.Mutex
	clock func() time.Time
}

func NewDefaultSkillRegistry() (*SkillRegistry, error) {
	root, err := DefaultBeeSkillsRoot()
	if err != nil {
		return nil, err
	}
	return NewSkillRegistry(root)
}

// OpenDefaultSkillRegistry opens an initialized registry without creating one.
// This keeps library composition read-only when Bee has not been initialized.
func OpenDefaultSkillRegistry() (*SkillRegistry, bool, error) {
	root, err := DefaultBeeSkillsRoot()
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("inspect skill registry: %w", err)
	}
	registry, err := NewSkillRegistry(root)
	return registry, err == nil, err
}

func NewSkillRegistry(root string) (*SkillRegistry, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("skill registry root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve skill registry root: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := ValidateBeePath(abs); err != nil {
		return nil, err
	}
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, fmt.Errorf("skill registry root is unsafe: %w", err)
	}
	if err := ensurePrivateDir(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create skill registry: %w", err)
	}
	return &SkillRegistry{root: abs, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func (registry *SkillRegistry) Root() string {
	if registry == nil {
		return ""
	}
	return registry.root
}

func (registry *SkillRegistry) SetClock(clock func() time.Time) {
	if registry != nil && clock != nil {
		registry.clock = clock
	}
}

func (registry *SkillRegistry) statePath() string {
	return filepath.Join(registry.root, SkillStateFilename)
}
func (registry *SkillRegistry) lockPath() string { return filepath.Join(registry.root, ".state.lock") }
func (registry *SkillRegistry) packagePath(name, version string) string {
	return filepath.Join(registry.root, name, version)
}

func (registry *SkillRegistry) withLock(ctx context.Context, fn func() error) error {
	if registry == nil {
		return errors.New("skill registry is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if err := rejectBeeSymlinkAncestors(registry.root); err != nil {
		return errors.New("skill registry is unsafe")
	}
	info, err := os.Lstat(registry.root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("skill registry must be a private real directory")
	}
	if info, err := os.Lstat(registry.lockPath()); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("skill registry lock is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect skill registry lock: %w", err)
	}
	lock, err := acquireSessionFileLockContext(ctx, registry.lockPath())
	if err != nil {
		return fmt.Errorf("lock skill registry: %w", err)
	}
	defer lock.Close()
	return fn()
}

func (registry *SkillRegistry) loadState() (SkillRegistryState, error) {
	state := SkillRegistryState{SchemaVersion: SkillStateSchemaVersion, Skills: map[string]SkillActivation{}}
	path := registry.statePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("inspect skill state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return state, errors.New("skill state must be a private 0600 regular file")
	}
	if info.Size() > SkillMaxStateBytes {
		return state, errors.New("skill state is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return state, fmt.Errorf("open skill state: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, SkillMaxStateBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode skill state: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return state, fmt.Errorf("decode skill state: %w", err)
	}
	if state.SchemaVersion != SkillStateSchemaVersion {
		return state, fmt.Errorf("skill state schema_version must be %d", SkillStateSchemaVersion)
	}
	if state.Skills == nil {
		state.Skills = map[string]SkillActivation{}
	}
	if len(state.Skills) > SkillMaxInstalledPackages {
		return state, fmt.Errorf("skill state exceeds %d entries", SkillMaxInstalledPackages)
	}
	for name, activation := range state.Skills {
		if err := validateSkillName(name); err != nil {
			return state, fmt.Errorf("skill state: %w", err)
		}
		if activation.ActiveVersion != "" {
			if err := validateSkillVersion(activation.ActiveVersion); err != nil {
				return state, fmt.Errorf("skill %q active version: %w", name, err)
			}
		}
		if activation.PreviousVersion != "" {
			if err := validateSkillVersion(activation.PreviousVersion); err != nil {
				return state, fmt.Errorf("skill %q previous version: %w", name, err)
			}
		}
		if activation.Enabled && activation.ActiveVersion == "" {
			return state, fmt.Errorf("enabled skill %q has no active version", name)
		}
	}
	return state, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func (registry *SkillRegistry) saveState(state SkillRegistryState) error {
	state.SchemaVersion = SkillStateSchemaVersion
	if state.Skills == nil {
		state.Skills = map[string]SkillActivation{}
	}
	if err := atomicWritePrivateJSON(registry.statePath(), state); err != nil {
		return fmt.Errorf("write skill state: %w", err)
	}
	return nil
}

func (registry *SkillRegistry) Install(ctx context.Context, source string) (SkillPackage, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return SkillPackage{}, errors.New("skill package source is required")
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return SkillPackage{}, fmt.Errorf("resolve skill package source: %w", err)
	}
	sourcePackage, sourceStats, err := loadSkillPackage(filepath.Clean(abs), false)
	if err != nil {
		return SkillPackage{}, err
	}
	var installed SkillPackage
	err = registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		packages, err := registry.listPackages(state)
		if err != nil {
			return err
		}
		if len(packages) >= SkillMaxInstalledPackages {
			return fmt.Errorf("skill registry exceeds %d installed packages", SkillMaxInstalledPackages)
		}
		destination := registry.packagePath(sourcePackage.Manifest.Name, sourcePackage.Manifest.Version)
		if _, statErr := os.Lstat(destination); statErr == nil {
			return fmt.Errorf("skill %s@%s is already installed", sourcePackage.Manifest.Name, sourcePackage.Manifest.Version)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect skill destination: %w", statErr)
		}
		nameRoot := filepath.Dir(destination)
		if err := ensurePrivateDir(nameRoot, 0o700); err != nil {
			return fmt.Errorf("create skill name directory: %w", err)
		}
		stage, err := os.MkdirTemp(registry.root, ".install-")
		if err != nil {
			return fmt.Errorf("create skill install stage: %w", err)
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
		staged, stagedStats, err := loadSkillPackage(stage, true)
		if err != nil {
			return fmt.Errorf("validate staged skill: %w", err)
		}
		if staged.Checksum != sourcePackage.Checksum || stagedStats.files != sourceStats.files || stagedStats.bytes != sourceStats.bytes {
			return errors.New("skill package changed while it was being installed")
		}
		if err := os.Rename(stage, destination); err != nil {
			return fmt.Errorf("activate installed skill package: %w", err)
		}
		stageLive = false
		installed = staged
		installed.Path = destination
		installed.Installed = true
		return nil
	})
	return installed, err
}

func (registry *SkillRegistry) List(ctx context.Context) ([]SkillPackage, error) {
	var packages []SkillPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		packages, err = registry.listPackages(state)
		return err
	})
	return packages, err
}

func (registry *SkillRegistry) listPackages(state SkillRegistryState) ([]SkillPackage, error) {
	nameEntries, err := os.ReadDir(registry.root)
	if err != nil {
		return nil, fmt.Errorf("list skill registry: %w", err)
	}
	packages := make([]SkillPackage, 0)
	found := make(map[string]map[string]struct{})
	for _, nameEntry := range nameEntries {
		if !nameEntry.IsDir() || strings.HasPrefix(nameEntry.Name(), ".") {
			continue
		}
		if err := validateSkillName(nameEntry.Name()); err != nil {
			return nil, fmt.Errorf("invalid skill directory %q: %w", nameEntry.Name(), err)
		}
		versionEntries, err := os.ReadDir(filepath.Join(registry.root, nameEntry.Name()))
		if err != nil {
			return nil, fmt.Errorf("list skill %q versions: %w", nameEntry.Name(), err)
		}
		for _, versionEntry := range versionEntries {
			if !versionEntry.IsDir() || strings.HasPrefix(versionEntry.Name(), ".") {
				continue
			}
			if len(packages) >= SkillMaxInstalledPackages {
				return nil, fmt.Errorf("skill registry exceeds %d installed packages", SkillMaxInstalledPackages)
			}
			loaded, _, err := loadSkillPackage(registry.packagePath(nameEntry.Name(), versionEntry.Name()), true)
			if err != nil {
				return nil, fmt.Errorf("load skill %s@%s: %w", nameEntry.Name(), versionEntry.Name(), err)
			}
			if loaded.Manifest.Name != nameEntry.Name() || loaded.Manifest.Version != versionEntry.Name() {
				return nil, fmt.Errorf("skill package path does not match manifest for %s@%s", nameEntry.Name(), versionEntry.Name())
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
				return nil, fmt.Errorf("skill state references missing active package %s@%s", name, activation.ActiveVersion)
			}
		}
		if activation.PreviousVersion != "" {
			if _, exists := found[name][activation.PreviousVersion]; !exists {
				return nil, fmt.Errorf("skill state references missing rollback package %s@%s", name, activation.PreviousVersion)
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

func (registry *SkillRegistry) Inspect(ctx context.Context, name, version string) (SkillPackage, error) {
	if err := validateSkillName(name); err != nil {
		return SkillPackage{}, err
	}
	if version != "" {
		if err := validateSkillVersion(version); err != nil {
			return SkillPackage{}, err
		}
	}
	var result SkillPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.loadState()
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
					return fmt.Errorf("skill %q is not installed", name)
				}
				selected = versions[0]
			}
		}
		result, _, err = loadSkillPackage(registry.packagePath(name, selected), true)
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

func (registry *SkillRegistry) Enable(ctx context.Context, name, version string) (SkillPackage, error) {
	return registry.enable(ctx, name, version, "")
}

func (registry *SkillRegistry) enable(ctx context.Context, name, version, expectedChecksum string) (SkillPackage, error) {
	if err := validateSkillName(name); err != nil {
		return SkillPackage{}, err
	}
	if version != "" {
		if err := validateSkillVersion(version); err != nil {
			return SkillPackage{}, err
		}
	}
	var result SkillPackage
	err := registry.withLock(ctx, func() error {
		selected := version
		if selected == "" {
			versions, err := registry.installedVersions(name)
			if err != nil {
				return err
			}
			if len(versions) == 0 {
				return fmt.Errorf("skill %q is not installed", name)
			}
			selected = versions[0]
		}
		loaded, _, err := loadSkillPackage(registry.packagePath(name, selected), true)
		if err != nil {
			return err
		}
		if expectedChecksum != "" && loaded.Checksum != expectedChecksum {
			return fmt.Errorf("installed skill checksum %s does not match approved checksum %s", loaded.Checksum, expectedChecksum)
		}
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		activation := state.Skills[name]
		if activation.ActiveVersion != "" && activation.ActiveVersion != selected {
			activation.PreviousVersion = activation.ActiveVersion
		}
		activation.ActiveVersion = selected
		activation.Enabled = true
		activation.UpdatedAt = registry.clock().UTC()
		state.Skills[name] = activation
		if err := registry.saveState(state); err != nil {
			return err
		}
		loaded.Enabled = true
		loaded.Active = true
		loaded.Installed = true
		result = loaded
		return nil
	})
	return result, err
}

func (registry *SkillRegistry) Disable(ctx context.Context, name string) (SkillActivation, error) {
	if err := validateSkillName(name); err != nil {
		return SkillActivation{}, err
	}
	var result SkillActivation
	err := registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		activation, exists := state.Skills[name]
		if !exists || activation.ActiveVersion == "" {
			return fmt.Errorf("skill %q has not been enabled", name)
		}
		activation.Enabled = false
		activation.UpdatedAt = registry.clock().UTC()
		state.Skills[name] = activation
		if err := registry.saveState(state); err != nil {
			return err
		}
		result = activation
		return nil
	})
	return result, err
}

func (registry *SkillRegistry) Rollback(ctx context.Context, name string) (SkillPackage, error) {
	if err := validateSkillName(name); err != nil {
		return SkillPackage{}, err
	}
	var result SkillPackage
	err := registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		activation, exists := state.Skills[name]
		if !exists || !activation.Enabled || activation.ActiveVersion == "" {
			return fmt.Errorf("skill %q is not enabled", name)
		}
		if activation.PreviousVersion == "" {
			return fmt.Errorf("skill %q has no previous version", name)
		}
		loaded, _, err := loadSkillPackage(registry.packagePath(name, activation.PreviousVersion), true)
		if err != nil {
			return fmt.Errorf("previous skill version is unavailable: %w", err)
		}
		activation.ActiveVersion, activation.PreviousVersion = activation.PreviousVersion, activation.ActiveVersion
		activation.UpdatedAt = registry.clock().UTC()
		state.Skills[name] = activation
		if err := registry.saveState(state); err != nil {
			return err
		}
		loaded.Enabled = true
		loaded.Active = true
		loaded.Installed = true
		result = loaded
		return nil
	})
	return result, err
}

func (registry *SkillRegistry) Remove(ctx context.Context, name, version string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if err := validateSkillVersion(version); err != nil {
		return err
	}
	return registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		activation := state.Skills[name]
		if activation.ActiveVersion == version {
			return fmt.Errorf("skill %s@%s is the active revision", name, version)
		}
		path := registry.packagePath(name, version)
		if _, _, err := loadSkillPackage(path, true); err != nil {
			return err
		}
		trash := filepath.Join(registry.root, fmt.Sprintf(".remove-%s-%d", name, registry.clock().UnixNano()))
		if err := os.Rename(path, trash); err != nil {
			return fmt.Errorf("stage skill removal: %w", err)
		}
		if activation.PreviousVersion == version {
			previousActivation := activation
			activation.PreviousVersion = ""
			activation.UpdatedAt = registry.clock().UTC()
			state.Skills[name] = activation
			if err := registry.saveState(state); err != nil {
				_ = os.Rename(trash, path)
				return err
			}
			if err := os.RemoveAll(trash); err != nil {
				_ = os.Rename(trash, path)
				state.Skills[name] = previousActivation
				_ = registry.saveState(state)
				return fmt.Errorf("remove skill package: %w", err)
			}
		} else if err := os.RemoveAll(trash); err != nil {
			_ = os.Rename(trash, path)
			return fmt.Errorf("remove skill package: %w", err)
		}
		nameRoot := filepath.Dir(path)
		if entries, readErr := os.ReadDir(nameRoot); readErr == nil && len(entries) == 0 {
			_ = os.Remove(nameRoot)
		}
		return nil
	})
}

// CheckRemove verifies that a package exists and is not the retained active
// revision. It is used by the CLI's non-mutating removal preview.
func (registry *SkillRegistry) CheckRemove(ctx context.Context, name, version string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if err := validateSkillVersion(version); err != nil {
		return err
	}
	return registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		if state.Skills[name].ActiveVersion == version {
			return fmt.Errorf("skill %s@%s is the active revision", name, version)
		}
		_, _, err = loadSkillPackage(registry.packagePath(name, version), true)
		return err
	})
}

func (registry *SkillRegistry) Enabled(ctx context.Context) ([]SkillPackage, error) {
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

func (registry *SkillRegistry) LoadEnabled(ctx context.Context, name string) (SkillPackage, string, error) {
	if err := validateSkillName(name); err != nil {
		return SkillPackage{}, "", err
	}
	var pkg SkillPackage
	var instructions string
	err := registry.withLock(ctx, func() error {
		state, err := registry.loadState()
		if err != nil {
			return err
		}
		activation, exists := state.Skills[name]
		if !exists || !activation.Enabled || activation.ActiveVersion == "" {
			return fmt.Errorf("skill %q is not enabled", name)
		}
		loaded, _, err := loadSkillPackage(registry.packagePath(name, activation.ActiveVersion), true)
		if err != nil {
			return err
		}
		path, _ := validateSkillRelativePath("skill instructions", loaded.Manifest.Instructions)
		data, err := readSkillFile(filepath.Join(loaded.Path, path), true, SkillMaxInstructionsBytes)
		if err != nil {
			return fmt.Errorf("read skill instructions: %w", err)
		}
		pkg = loaded
		pkg.Enabled = true
		pkg.Active = true
		pkg.Installed = true
		instructions = string(data)
		return nil
	})
	return pkg, instructions, err
}

func (registry *SkillRegistry) EnabledExtensions(ctx context.Context) ([]SkillExtensionActivation, error) {
	packages, err := registry.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	activations := make([]SkillExtensionActivation, 0)
	seen := make(map[string]struct{})
	for _, pkg := range packages {
		if pkg.Manifest.Extension == nil {
			continue
		}
		config, err := skillExtensionConfig(pkg)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[config.Name]; exists {
			return nil, fmt.Errorf("duplicate enabled skill extension %q", config.Name)
		}
		seen[config.Name] = struct{}{}
		activations = append(activations, SkillExtensionActivation{SkillName: pkg.Manifest.Name, SkillVersion: pkg.Manifest.Version, Config: config, ExpectedTools: append([]string(nil), pkg.Manifest.Extension.Tools...)})
	}
	return activations, nil
}

func (registry *SkillRegistry) TestInstalled(ctx context.Context, name, version string, startExtension bool) (SkillTestReport, error) {
	pkg, err := registry.Inspect(ctx, name, version)
	if err != nil {
		return SkillTestReport{}, err
	}
	return testSkillPackage(ctx, pkg, startExtension)
}

func TestSkillSource(ctx context.Context, source string, startExtension bool) (SkillTestReport, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return SkillTestReport{}, err
	}
	pkg, _, err := loadSkillPackage(filepath.Clean(abs), false)
	if err != nil {
		return SkillTestReport{}, err
	}
	return testSkillPackage(ctx, pkg, startExtension)
}

func testSkillPackage(ctx context.Context, pkg SkillPackage, startExtension bool) (SkillTestReport, error) {
	_, stats, err := loadSkillPackage(pkg.Path, pkg.Installed)
	if err != nil {
		return SkillTestReport{}, err
	}
	path, _ := validateSkillRelativePath("skill instructions", pkg.Manifest.Instructions)
	instructions, err := readSkillFile(filepath.Join(pkg.Path, path), pkg.Installed, SkillMaxInstructionsBytes)
	if err != nil {
		return SkillTestReport{}, err
	}
	report := SkillTestReport{Valid: true, Package: pkg, Instructions: len(instructions), Files: stats.files, PackageBytes: stats.bytes}
	if pkg.Manifest.Extension == nil {
		return report, nil
	}
	config, err := skillExtensionConfig(pkg)
	if err != nil {
		return SkillTestReport{}, err
	}
	if !startExtension {
		report.ExtensionTest = "static validation only"
		report.Tools = append([]string(nil), pkg.Manifest.Extension.Tools...)
		return report, nil
	}
	host, err := NewExtensionHost(config)
	if err != nil {
		return SkillTestReport{}, err
	}
	defer host.Close(context.Background())
	if err := host.Start(ctx); err != nil {
		return SkillTestReport{}, err
	}
	if err := validatePackageExtensionCapabilities(pkg.Manifest.Extension.Tools, host.Capabilities()); err != nil {
		return SkillTestReport{}, err
	}
	report.ExtensionTest = "initialize handshake passed"
	report.Tools = host.ToolNames()
	return report, nil
}

func skillExtensionConfig(pkg SkillPackage) (ExtensionConfig, error) {
	extension := pkg.Manifest.Extension
	if extension == nil {
		return ExtensionConfig{}, errors.New("skill has no extension")
	}
	command, _ := validateSkillRelativePath("skill extension command", extension.Command)
	commandPath := filepath.Join(pkg.Path, command)
	info, err := os.Lstat(commandPath)
	if err != nil {
		return ExtensionConfig{}, fmt.Errorf("inspect skill extension command: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return ExtensionConfig{}, errors.New("skill extension command must be a package-contained executable regular file")
	}
	workingDir := pkg.Path
	if extension.WorkingDir != "" {
		relative, _ := validateSkillRelativePath("skill extension working_dir", extension.WorkingDir)
		workingDir = filepath.Join(pkg.Path, relative)
		info, err := os.Lstat(workingDir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ExtensionConfig{}, errors.New("skill extension working_dir must be a package-contained real directory")
		}
	}
	config := ExtensionConfig{
		Name:                  extension.Name,
		Enabled:               true,
		Command:               commandPath,
		Args:                  append([]string(nil), extension.Args...),
		WorkingDir:            workingDir,
		EnvAllowlist:          append([]string(nil), extension.EnvAllowlist...),
		StartupTimeoutSeconds: extension.StartupTimeoutSeconds,
		RequestTimeoutSeconds: extension.RequestTimeoutSeconds,
		MaxFrameBytes:         extension.MaxFrameBytes,
		MaxMessageBytes:       extension.MaxMessageBytes,
	}
	if err := config.Validate(); err != nil {
		return ExtensionConfig{}, err
	}
	return config, nil
}

func validatePackageExtensionCapabilities(expected []string, capabilities ExtensionCapabilities) error {
	if len(capabilities.Providers) > 0 || len(capabilities.Hooks) > 0 || len(capabilities.Context) > 0 {
		return errors.New("package extensions may advertise only tools and notifications")
	}
	actual := make([]string, 0, len(capabilities.Tools))
	for _, definition := range capabilities.Tools {
		actual = append(actual, definition.Name)
	}
	want := append([]string(nil), expected...)
	sort.Strings(actual)
	sort.Strings(want)
	if strings.Join(actual, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("package extension tools %v do not match manifest allowlist %v", actual, want)
	}
	return nil
}

func (registry *SkillRegistry) installedVersions(name string) ([]string, error) {
	root := filepath.Join(registry.root, name)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list skill versions: %w", err)
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := validateSkillVersion(entry.Name()); err != nil {
			return nil, fmt.Errorf("invalid installed skill version %q", entry.Name())
		}
		versions = append(versions, entry.Name())
	}
	sort.Slice(versions, func(i, j int) bool { return compareSkillVersions(versions[i], versions[j]) > 0 })
	return versions, nil
}

func compareSkillVersions(left, right string) int {
	parse := func(value string) ([3]uint64, string) {
		var result [3]uint64
		main := value
		if index := strings.IndexByte(main, '+'); index >= 0 {
			main = main[:index]
		}
		pre := ""
		if index := strings.IndexByte(main, '-'); index >= 0 {
			pre = main[index+1:]
			main = main[:index]
		}
		for index, part := range strings.Split(main, ".") {
			result[index], _ = strconv.ParseUint(part, 10, 31)
		}
		return result, pre
	}
	lv, lp := parse(left)
	rv, rp := parse(right)
	for index := range lv {
		if lv[index] < rv[index] {
			return -1
		}
		if lv[index] > rv[index] {
			return 1
		}
	}
	if lp == rp {
		return 0
	}
	if lp == "" {
		return 1
	}
	if rp == "" {
		return -1
	}
	leftParts := strings.Split(lp, ".")
	rightParts := strings.Split(rp, ".")
	count := len(leftParts)
	if len(rightParts) < count {
		count = len(rightParts)
	}
	for index := 0; index < count; index++ {
		leftNumeric := skillDigitsOnly(leftParts[index])
		rightNumeric := skillDigitsOnly(rightParts[index])
		switch {
		case leftNumeric && rightNumeric:
			if len(leftParts[index]) != len(rightParts[index]) {
				if len(leftParts[index]) < len(rightParts[index]) {
					return -1
				}
				return 1
			}
			if compared := strings.Compare(leftParts[index], rightParts[index]); compared != 0 {
				return compared
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			if compared := strings.Compare(leftParts[index], rightParts[index]); compared != 0 {
				return compared
			}
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1
	}
	if len(leftParts) > len(rightParts) {
		return 1
	}
	return 0
}

func skillDigitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

type skillPackageStats struct {
	files int
	bytes int64
}

func loadSkillPackage(root string, requirePrivate bool) (SkillPackage, skillPackageStats, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return SkillPackage{}, skillPackageStats{}, fmt.Errorf("inspect skill package: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return SkillPackage{}, skillPackageStats{}, errors.New("skill package must be a real directory")
	}
	if requirePrivate && info.Mode().Perm()&0o077 != 0 {
		return SkillPackage{}, skillPackageStats{}, errors.New("installed skill package directory must be owner-only")
	}
	manifestData, err := readSkillFile(filepath.Join(root, SkillManifestFilename), requirePrivate, SkillMaxManifestBytes)
	if err != nil {
		return SkillPackage{}, skillPackageStats{}, fmt.Errorf("read skill manifest: %w", err)
	}
	var manifest SkillManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return SkillPackage{}, skillPackageStats{}, fmt.Errorf("decode skill manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SkillPackage{}, skillPackageStats{}, fmt.Errorf("decode skill manifest: %w", err)
	}
	if manifest.Instructions == "" {
		manifest.Instructions = DefaultSkillInstructions
	}
	if err := manifest.Validate(); err != nil {
		return SkillPackage{}, skillPackageStats{}, err
	}
	instructionPath, _ := validateSkillRelativePath("skill instructions", manifest.Instructions)
	instructions, err := readSkillFile(filepath.Join(root, instructionPath), requirePrivate, SkillMaxInstructionsBytes)
	if err != nil {
		return SkillPackage{}, skillPackageStats{}, fmt.Errorf("read skill instructions: %w", err)
	}
	if strings.TrimSpace(string(instructions)) == "" {
		return SkillPackage{}, skillPackageStats{}, errors.New("skill instructions are empty")
	}
	for _, value := range string(instructions) {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return SkillPackage{}, skillPackageStats{}, errors.New("skill instructions contain unsupported control characters")
		}
	}
	checksum, stats, err := checksumSkillTree(root, requirePrivate)
	if err != nil {
		return SkillPackage{}, skillPackageStats{}, err
	}
	pkg := SkillPackage{Manifest: manifest, Checksum: checksum, Path: root, Installed: requirePrivate}
	if manifest.Extension != nil {
		if _, err := skillExtensionConfig(pkg); err != nil {
			return SkillPackage{}, skillPackageStats{}, err
		}
	}
	return pkg, stats, nil
}

func readSkillFile(path string, requirePrivate bool, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("skill file must be a regular non-symlink file")
	}
	if requirePrivate && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("installed skill file must be owner-only")
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("skill file exceeds %d bytes", maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("skill file exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func checksumSkillTree(root string, requirePrivate bool) (string, skillPackageStats, error) {
	type entry struct {
		rel  string
		path string
		mode os.FileMode
		size int64
	}
	var files []entry
	stats := skillPackageStats{}
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill package contains symlink %q", path)
		}
		if info.IsDir() {
			if requirePrivate && info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("installed skill directory %q must be owner-only", path)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill package contains unsupported file %q", path)
		}
		if requirePrivate && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("installed skill file %q must be owner-only", path)
		}
		stats.files++
		stats.bytes += info.Size()
		if stats.files > SkillMaxPackageFiles || stats.bytes > SkillMaxPackageBytes {
			return errors.New("skill package exceeds file or byte limits")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, entry{rel: filepath.ToSlash(rel), path: path, mode: info.Mode(), size: info.Size()})
		return nil
	})
	if err != nil {
		return "", stats, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	hash := sha256.New()
	for _, item := range files {
		_, _ = io.WriteString(hash, item.rel)
		_, _ = io.WriteString(hash, "\x00")
		if item.mode&0o100 != 0 {
			_, _ = io.WriteString(hash, "x")
		}
		_, _ = io.WriteString(hash, "\x00")
		file, err := os.Open(item.path)
		if err != nil {
			return "", stats, err
		}
		written, copyErr := io.Copy(hash, io.LimitReader(file, item.size+1))
		closeErr := file.Close()
		if copyErr != nil {
			return "", stats, copyErr
		}
		if closeErr != nil {
			return "", stats, closeErr
		}
		if written != item.size {
			return "", stats, errors.New("skill package changed while checksumming")
		}
		_, _ = io.WriteString(hash, "\x00")
	}
	return hex.EncodeToString(hash.Sum(nil)), stats, nil
}

func copySkillTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill package contains symlink %q", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill package contains unsupported file %q", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if info.Mode()&0o100 != 0 {
			mode = 0o700
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, SkillMaxPackageBytes+1))
		inputErr := input.Close()
		outputErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		if outputErr != nil {
			return outputErr
		}
		return os.Chmod(target, mode)
	})
}
