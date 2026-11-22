package bee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	CompletionManifestSchemaVersion = 1
	CompletionManifestFilename      = "completion.manifest.json"
	IntegrationManifestFilename     = "integration.manifest.json"
	MaxCompletionManifestBytes      = 1 << 20
	MaxCompletionManifestArtifacts  = 256
	MaxCompletionArtifactBytes      = int64(64 << 20)
)

const (
	CompletionManifestDelegated = "delegated_child"
	CompletionManifestACP       = "acp_backend"
	CompletionManifestIsolated  = "isolated_integration"
)

type CompletionArtifactDigest struct {
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	Fingerprint string `json:"sha256"`
}

type CompletionProviderIdentity struct {
	Profile      string       `json:"profile,omitempty"`
	Role         ProviderRole `json:"role,omitempty"`
	Type         string       `json:"type,omitempty"`
	Model        string       `json:"model,omitempty"`
	EndpointHost string       `json:"endpoint_host,omitempty"`
}

// CompletionManifest binds a terminal delegated result to the inputs and
// effective harness that produced it. It stores hashes, never task prompts,
// system prompts, tool descriptions, credentials, or artifact contents.
type CompletionManifest struct {
	SchemaVersion         int                        `json:"schema_version"`
	Kind                  string                     `json:"kind"`
	CompletedAt           time.Time                  `json:"completed_at"`
	TaskID                string                     `json:"task_id"`
	ParentID              string                     `json:"parent_id"`
	AttemptID             string                     `json:"attempt_id,omitempty"`
	Status                ChildStatus                `json:"status"`
	BaseCommit            string                     `json:"base_commit,omitempty"`
	TaskFingerprint       string                     `json:"task_fingerprint"`
	HarnessFingerprint    string                     `json:"harness_fingerprint"`
	CapabilityFingerprint string                     `json:"capability_fingerprint"`
	ProviderFingerprint   string                     `json:"provider_fingerprint"`
	Provider              CompletionProviderIdentity `json:"provider"`
	ResultFingerprint     string                     `json:"result_fingerprint"`
	UpstreamFingerprint   string                     `json:"upstream_fingerprint,omitempty"`
	Artifacts             []CompletionArtifactDigest `json:"artifacts,omitempty"`
	Fingerprint           string                     `json:"fingerprint"`
}

// CompletionManifestReference is the compact reference carried in completion
// packets and integration proposals. Path is descriptive; the fingerprint is
// authoritative when checking a saved manifest.
type CompletionManifestReference struct {
	Path                  string                     `json:"path"`
	Fingerprint           string                     `json:"fingerprint"`
	TaskFingerprint       string                     `json:"task_fingerprint"`
	HarnessFingerprint    string                     `json:"harness_fingerprint"`
	CapabilityFingerprint string                     `json:"capability_fingerprint"`
	ProviderFingerprint   string                     `json:"provider_fingerprint"`
	Provider              CompletionProviderIdentity `json:"provider"`
}

type CompletionManifestExpectation struct {
	ManifestFingerprint   string
	TaskFingerprint       string
	HarnessFingerprint    string
	CapabilityFingerprint string
	ProviderFingerprint   string
	BaseCommit            string
	ResultFingerprint     string
	AllowNonSuccess       bool
}

type CompletionManifestVerification struct {
	Reusable bool     `json:"reusable"`
	Problems []string `json:"problems,omitempty"`
}

type completionManifestIdentity struct {
	SchemaVersion         int
	Kind                  string
	CompletedAt           time.Time
	TaskID                string
	ParentID              string
	AttemptID             string
	Status                ChildStatus
	BaseCommit            string
	TaskFingerprint       string
	HarnessFingerprint    string
	CapabilityFingerprint string
	ProviderFingerprint   string
	Provider              CompletionProviderIdentity
	ResultFingerprint     string
	UpstreamFingerprint   string
	Artifacts             []CompletionArtifactDigest
}

type completionManifestInput struct {
	Kind                  string
	TaskID                string
	ParentID              string
	AttemptID             string
	Status                ChildStatus
	BaseCommit            string
	TaskFingerprint       string
	HarnessFingerprint    string
	CapabilityFingerprint string
	ProviderFingerprint   string
	Provider              CompletionProviderIdentity
	ResultFingerprint     string
	UpstreamFingerprint   string
	ArtifactRoot          string
	ArtifactPaths         []string
	Exclude               map[string]bool
}

func writeCompletionManifest(path string, input completionManifestInput) (CompletionManifest, error) {
	var manifest CompletionManifest
	if strings.TrimSpace(path) == "" {
		return manifest, errors.New("completion manifest path is required")
	}
	artifacts, err := collectCompletionArtifactDigests(input.ArtifactRoot, input.ArtifactPaths, input.Exclude)
	if err != nil {
		return manifest, err
	}
	manifest = CompletionManifest{
		SchemaVersion: CompletionManifestSchemaVersion, Kind: input.Kind, CompletedAt: time.Now().UTC(),
		TaskID: input.TaskID, ParentID: input.ParentID, AttemptID: input.AttemptID, Status: input.Status,
		BaseCommit: input.BaseCommit, TaskFingerprint: input.TaskFingerprint, HarnessFingerprint: input.HarnessFingerprint,
		CapabilityFingerprint: input.CapabilityFingerprint, ProviderFingerprint: input.ProviderFingerprint,
		Provider: input.Provider, ResultFingerprint: input.ResultFingerprint, UpstreamFingerprint: input.UpstreamFingerprint,
		Artifacts: artifacts,
	}
	manifest.Fingerprint = completionManifestFingerprint(manifest)
	if err := manifest.Validate(); err != nil {
		return CompletionManifest{}, err
	}
	if err := atomicWritePrivateJSON(path, manifest); err != nil {
		return CompletionManifest{}, fmt.Errorf("write completion manifest: %w", err)
	}
	return manifest, nil
}

func completionManifestFingerprint(manifest CompletionManifest) string {
	identity := completionManifestIdentity{
		SchemaVersion: manifest.SchemaVersion, Kind: manifest.Kind, CompletedAt: manifest.CompletedAt, TaskID: manifest.TaskID, ParentID: manifest.ParentID,
		AttemptID: manifest.AttemptID, Status: manifest.Status, BaseCommit: manifest.BaseCommit,
		TaskFingerprint: manifest.TaskFingerprint, HarnessFingerprint: manifest.HarnessFingerprint,
		CapabilityFingerprint: manifest.CapabilityFingerprint, ProviderFingerprint: manifest.ProviderFingerprint,
		Provider: manifest.Provider, ResultFingerprint: manifest.ResultFingerprint, UpstreamFingerprint: manifest.UpstreamFingerprint,
		Artifacts: append([]CompletionArtifactDigest(nil), manifest.Artifacts...),
	}
	sort.Slice(identity.Artifacts, func(i, j int) bool { return identity.Artifacts[i].Path < identity.Artifacts[j].Path })
	return fingerprintValue("bee-completion-manifest-v1", identity)
}

func (manifest CompletionManifest) Validate() error {
	if manifest.SchemaVersion != CompletionManifestSchemaVersion {
		return fmt.Errorf("completion manifest schema_version must be %d", CompletionManifestSchemaVersion)
	}
	switch manifest.Kind {
	case CompletionManifestDelegated, CompletionManifestACP, CompletionManifestIsolated:
	default:
		return fmt.Errorf("completion manifest kind %q is invalid", manifest.Kind)
	}
	if err := validateChildID("task_id", manifest.TaskID); err != nil {
		return err
	}
	if err := validateChildID("parent_id", manifest.ParentID); err != nil {
		return err
	}
	switch manifest.Status {
	case ChildSucceeded, ChildFailed, ChildTimedOut, ChildCancelled, ChildBudgetExceeded, ChildMalformed:
	default:
		return fmt.Errorf("completion manifest status %q is invalid", manifest.Status)
	}
	if manifest.CompletedAt.IsZero() {
		return errors.New("completion manifest completed_at is required")
	}
	for label, value := range map[string]string{
		"task_fingerprint": manifest.TaskFingerprint, "harness_fingerprint": manifest.HarnessFingerprint,
		"capability_fingerprint": manifest.CapabilityFingerprint, "provider_fingerprint": manifest.ProviderFingerprint,
		"result_fingerprint": manifest.ResultFingerprint, "fingerprint": manifest.Fingerprint,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("completion manifest %s is not a SHA-256 fingerprint", label)
		}
	}
	if manifest.UpstreamFingerprint != "" && !validSHA256(manifest.UpstreamFingerprint) {
		return errors.New("completion manifest upstream_fingerprint is invalid")
	}
	if len(manifest.Artifacts) > MaxCompletionManifestArtifacts {
		return fmt.Errorf("completion manifest artifacts exceed %d", MaxCompletionManifestArtifacts)
	}
	seen := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.Bytes < 0 || artifact.Bytes > MaxCompletionArtifactBytes {
			return fmt.Errorf("completion artifact %q has invalid size", artifact.Path)
		}
		if err := validateManifestRelativePath(artifact.Path); err != nil {
			return err
		}
		if !validSHA256(artifact.Fingerprint) {
			return fmt.Errorf("completion artifact %q fingerprint is invalid", artifact.Path)
		}
		if _, exists := seen[artifact.Path]; exists {
			return fmt.Errorf("completion artifact %q is duplicated", artifact.Path)
		}
		seen[artifact.Path] = struct{}{}
	}
	if manifest.Fingerprint != completionManifestFingerprint(manifest) {
		return errors.New("completion manifest fingerprint does not match its contents")
	}
	return nil
}

func (reference CompletionManifestReference) Validate() error {
	if strings.TrimSpace(reference.Path) == "" || strings.ContainsRune(reference.Path, '\x00') {
		return errors.New("completion manifest reference path is invalid")
	}
	for label, value := range map[string]string{
		"fingerprint": reference.Fingerprint, "task_fingerprint": reference.TaskFingerprint,
		"harness_fingerprint": reference.HarnessFingerprint, "capability_fingerprint": reference.CapabilityFingerprint,
		"provider_fingerprint": reference.ProviderFingerprint,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("completion manifest reference %s is invalid", label)
		}
	}
	return nil
}

// ReadCompletionManifest loads one bounded owner-controlled manifest without
// following a manifest-file symlink.
func ReadCompletionManifest(path string) (CompletionManifest, error) {
	var manifest CompletionManifest
	if err := rejectBeeSymlinkAncestors(path); err != nil {
		return manifest, fmt.Errorf("completion manifest path is unsafe: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return manifest, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return manifest, errors.New("completion manifest must be a regular non-symlink file")
	}
	if info.Size() > MaxCompletionManifestBytes {
		return manifest, fmt.Errorf("completion manifest exceeds %d bytes", MaxCompletionManifestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, MaxCompletionManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return CompletionManifest{}, fmt.Errorf("decode completion manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CompletionManifest{}, errors.New("decode completion manifest: trailing JSON data")
	}
	if err := manifest.Validate(); err != nil {
		return CompletionManifest{}, err
	}
	return manifest, nil
}

// VerifyCompletionManifest determines whether saved output is reusable under
// an expected task/harness identity and whether every recorded artifact is
// still byte-identical. An absent expectation field is not compared.
func VerifyCompletionManifest(path string, expected CompletionManifestExpectation) (CompletionManifestVerification, error) {
	manifest, err := ReadCompletionManifest(path)
	if err != nil {
		return CompletionManifestVerification{}, err
	}
	verification := CompletionManifestVerification{}
	addMismatch := func(label, want, got string) {
		if want != "" && want != got {
			verification.Problems = append(verification.Problems, label+" changed")
		}
	}
	if manifest.Status != ChildSucceeded && !expected.AllowNonSuccess {
		verification.Problems = append(verification.Problems, "completion status is not succeeded")
	}
	addMismatch("manifest fingerprint", expected.ManifestFingerprint, manifest.Fingerprint)
	addMismatch("task fingerprint", expected.TaskFingerprint, manifest.TaskFingerprint)
	addMismatch("harness fingerprint", expected.HarnessFingerprint, manifest.HarnessFingerprint)
	addMismatch("capability fingerprint", expected.CapabilityFingerprint, manifest.CapabilityFingerprint)
	addMismatch("provider fingerprint", expected.ProviderFingerprint, manifest.ProviderFingerprint)
	addMismatch("base commit", expected.BaseCommit, manifest.BaseCommit)
	addMismatch("result fingerprint", expected.ResultFingerprint, manifest.ResultFingerprint)
	root := filepath.Dir(path)
	for _, artifact := range manifest.Artifacts {
		digest, digestErr := digestCompletionArtifact(root, artifact.Path)
		if digestErr != nil {
			verification.Problems = append(verification.Problems, fmt.Sprintf("artifact %s is unavailable", artifact.Path))
			continue
		}
		if digest.Bytes != artifact.Bytes || digest.Fingerprint != artifact.Fingerprint {
			verification.Problems = append(verification.Problems, fmt.Sprintf("artifact %s changed", artifact.Path))
		}
	}
	verification.Reusable = len(verification.Problems) == 0
	return verification, nil
}

func collectCompletionArtifactDigests(root string, paths []string, exclude map[string]bool) ([]CompletionArtifactDigest, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("completion artifact root is required")
	}
	root = filepath.Clean(root)
	var names []string
	if paths != nil {
		for _, path := range paths {
			if filepath.IsAbs(path) {
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return nil, fmt.Errorf("completion artifact %q escapes manifest root", path)
				}
				path = rel
			}
			names = append(names, filepath.ToSlash(filepath.Clean(path)))
		}
	} else {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root || entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk completion artifacts: %w", err)
		}
	}
	names = normalizedStrings(names)
	artifacts := make([]CompletionArtifactDigest, 0, len(names))
	var total int64
	for _, name := range names {
		if exclude != nil && exclude[name] {
			continue
		}
		if len(artifacts) >= MaxCompletionManifestArtifacts {
			return nil, fmt.Errorf("completion artifacts exceed %d files", MaxCompletionManifestArtifacts)
		}
		digest, err := digestCompletionArtifact(root, name)
		if err != nil {
			return nil, err
		}
		total += digest.Bytes
		if total > MaxCompletionArtifactBytes {
			return nil, fmt.Errorf("completion artifacts exceed %d bytes", MaxCompletionArtifactBytes)
		}
		artifacts = append(artifacts, digest)
	}
	return artifacts, nil
}

func writeIntegrationCompletionManifest(lease WorkspaceLease, completion ChildCompletionPacket, proposal IntegrationProposal, includeDiff bool) (*CompletionManifestReference, error) {
	manifestPath := filepath.Join(filepath.Dir(lease.ProposalArtifact), IntegrationManifestFilename)
	taskFingerprint := fingerprintValue("bee-isolated-task-v1", struct {
		TaskID   string
		ParentID string
	}{lease.TaskID, completion.ParentID})
	harnessFingerprint := fingerprintValue("bee-isolated-harness-v1", "unknown")
	capabilityFingerprint := fingerprintValue("bee-isolated-capabilities-v1", lease.ScopePath)
	providerFingerprint := fingerprintValue("bee-isolated-provider-v1", "unknown")
	provider := CompletionProviderIdentity{Type: "unreported", Model: "unreported"}
	upstreamFingerprint := ""
	if completion.Manifest != nil {
		taskFingerprint = completion.Manifest.TaskFingerprint
		harnessFingerprint = completion.Manifest.HarnessFingerprint
		capabilityFingerprint = completion.Manifest.CapabilityFingerprint
		providerFingerprint = completion.Manifest.ProviderFingerprint
		provider = completion.Manifest.Provider
		upstreamFingerprint = completion.Manifest.Fingerprint
	}
	status := completion.Status
	if !proposal.ReadyForReview && status == ChildSucceeded {
		status = ChildFailed
	}
	result := proposal
	result.CompletionManifest = nil
	result.Artifacts = append([]string(nil), proposal.Artifacts...)
	artifacts := make([]string, 0, 1)
	if includeDiff {
		artifacts = append(artifacts, proposal.DiffArtifact)
	}
	manifest, err := writeCompletionManifest(manifestPath, completionManifestInput{
		Kind: CompletionManifestIsolated, TaskID: lease.TaskID, ParentID: completion.ParentID,
		AttemptID: lease.AttemptID, Status: status, BaseCommit: lease.BaseCommit,
		TaskFingerprint: taskFingerprint, HarnessFingerprint: harnessFingerprint,
		CapabilityFingerprint: capabilityFingerprint, ProviderFingerprint: providerFingerprint,
		Provider:          provider,
		ResultFingerprint: fingerprintValue("bee-integration-proposal-v1", result), UpstreamFingerprint: upstreamFingerprint,
		ArtifactRoot: filepath.Dir(lease.ProposalArtifact), ArtifactPaths: artifacts,
		Exclude: map[string]bool{IntegrationManifestFilename: true, filepath.Base(lease.ProposalArtifact): true},
	})
	if err != nil {
		return nil, err
	}
	return completionManifestReference(manifestPath, manifest), nil
}

func digestCompletionArtifact(root, relative string) (CompletionArtifactDigest, error) {
	if err := validateManifestRelativePath(relative); err != nil {
		return CompletionArtifactDigest{}, err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := rejectBeeSymlinkAncestors(path); err != nil {
		return CompletionArtifactDigest{}, fmt.Errorf("completion artifact %q is unsafe: %w", relative, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return CompletionArtifactDigest{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return CompletionArtifactDigest{}, fmt.Errorf("completion artifact %q must be a regular non-symlink file", relative)
	}
	if info.Size() > MaxCompletionArtifactBytes {
		return CompletionArtifactDigest{}, fmt.Errorf("completion artifact %q exceeds %d bytes", relative, MaxCompletionArtifactBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return CompletionArtifactDigest{}, err
	}
	defer file.Close()
	h := sha256.New()
	written, err := io.Copy(h, io.LimitReader(file, MaxCompletionArtifactBytes+1))
	if err != nil {
		return CompletionArtifactDigest{}, err
	}
	if written != info.Size() {
		return CompletionArtifactDigest{}, fmt.Errorf("completion artifact %q changed while hashing", relative)
	}
	return CompletionArtifactDigest{Path: filepath.ToSlash(relative), Bytes: written, Fingerprint: hex.EncodeToString(h.Sum(nil))}, nil
}

func validateManifestRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("completion artifact path %q is invalid", value)
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("completion artifact path %q escapes its root", value)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func completionManifestReference(path string, manifest CompletionManifest) *CompletionManifestReference {
	return &CompletionManifestReference{
		Path: path, Fingerprint: manifest.Fingerprint, TaskFingerprint: manifest.TaskFingerprint,
		HarnessFingerprint: manifest.HarnessFingerprint, CapabilityFingerprint: manifest.CapabilityFingerprint,
		ProviderFingerprint: manifest.ProviderFingerprint, Provider: manifest.Provider,
	}
}

func completionTaskFingerprint(task ChildTask) string {
	logicalScope := task.Scope
	if task.ParentScopeRoot != "" {
		logicalScope.Root = task.ParentScopeRoot
		logicalScope.Roots = nil
	}
	logicalScope.RemoteGrant = nil
	identity := struct {
		TaskID             string
		ParentID           string
		Objective          string
		Prompt             string
		AllowedTools       []string
		Mutability         TaskMutability
		Scope              ExecutionScope
		Budget             ChildBudget
		AcceptanceCriteria []string
		MemoryProjection   *ChildMemoryProjection
	}{task.TaskID, task.ParentID, task.Objective, task.Prompt, normalizedStrings(task.AllowedTools), task.Mutability, logicalScope, task.Budget, append([]string(nil), task.AcceptanceCriteria...), task.MemoryProjection}
	return fingerprintValue("bee-completion-task-v1", identity)
}

func completionCapabilityFingerprint(task ChildTask, definitions []ToolDefinition, requirePlan bool) string {
	definitions, _ = normalizeToolDefinitions(definitions)
	logicalScope := task.Scope
	if task.ParentScopeRoot != "" {
		logicalScope.Root = task.ParentScopeRoot
		logicalScope.Roots = nil
	}
	logicalScope.RemoteGrant = nil
	identity := struct {
		AllowedTools []string
		Definitions  []ToolDefinition
		Scope        ExecutionScope
		RequirePlan  bool
	}{normalizedStrings(task.AllowedTools), definitions, logicalScope, requirePlan}
	return fingerprintValue("bee-completion-capabilities-v1", identity)
}

func childHarnessFingerprint(config Config, definitions []ToolDefinition, requirePlan bool) (string, CompletionProviderIdentity, string, error) {
	prompt, err := effectiveSystemPrompt(config.Agent, false)
	if err != nil {
		return "", CompletionProviderIdentity{}, "", err
	}
	definitions, _ = normalizeToolDefinitions(definitions)
	providerConfig := sanitizedProviderConfig(config.Provider)
	provider := CompletionProviderIdentity{Profile: config.ProviderProfile, Role: config.ProviderRole, Type: providerConfig.Type, Model: providerConfig.Model}
	if parsed, parseErr := url.Parse(providerConfig.BaseURL); parseErr == nil {
		provider.EndpointHost = parsed.Hostname()
	}
	providerFingerprint := fingerprintValue("bee-provider-v1", struct {
		Profile string
		Role    ProviderRole
		Config  ProviderConfig
	}{config.ProviderProfile, config.ProviderRole, providerConfig})
	harness := struct {
		ProviderFingerprint string
		Generation          GenerationConfig
		Agent               AgentConfig
		PromptFingerprint   string
		Context             ContextKernelConfig
		RequirePlan         bool
		Tools               []ToolDefinition
	}{ProviderFingerprint: providerFingerprint, Generation: config.Generation, Agent: config.Agent, PromptFingerprint: fingerprintBytes([]byte(prompt)), Context: config.Context, RequirePlan: requirePlan, Tools: definitions}
	harness.Agent.SystemPrompt = ""
	harness.Agent.SystemPromptFile = ""
	return fingerprintValue("bee-child-harness-v1", harness), provider, providerFingerprint, nil
}

func completionPacketFingerprint(packet ChildCompletionPacket) string {
	copy := packet
	copy.Manifest = nil
	filtered := make([]string, 0, len(copy.Artifacts))
	for _, artifact := range copy.Artifacts {
		base := filepath.Base(artifact)
		if base == CompletionManifestFilename || base == IntegrationManifestFilename || base == "completion.json" {
			continue
		}
		filtered = append(filtered, artifact)
	}
	copy.Artifacts = filtered
	return fingerprintValue("bee-child-completion-v1", copy)
}

func finalizeDelegatedCompletion(task ChildTask, packet ChildCompletionPacket, artifactDir string, config Config, definitions []ToolDefinition, requirePlan bool, kind string, harnessOverride, providerOverride string, providerIdentity CompletionProviderIdentity) (ChildCompletionPacket, error) {
	if packet.TaskID == "" {
		packet.TaskID = task.TaskID
	}
	if packet.ParentID == "" {
		packet.ParentID = task.ParentID
	}
	harnessFingerprint := harnessOverride
	providerFingerprint := providerOverride
	provider := providerIdentity
	if harnessFingerprint == "" || providerFingerprint == "" {
		var err error
		harnessFingerprint, provider, providerFingerprint, err = childHarnessFingerprint(config, definitions, requirePlan)
		if err != nil {
			return packet, err
		}
	}
	baseCommit, attemptID := "", ""
	if task.Workspace != nil {
		baseCommit = task.Workspace.BaseCommit
		attemptID = task.Workspace.AttemptID
	}
	input := completionManifestInput{
		Kind: kind, TaskID: packet.TaskID, ParentID: packet.ParentID, AttemptID: attemptID, Status: packet.Status,
		BaseCommit: baseCommit, TaskFingerprint: completionTaskFingerprint(task), HarnessFingerprint: harnessFingerprint,
		CapabilityFingerprint: completionCapabilityFingerprint(task, definitions, requirePlan), ProviderFingerprint: providerFingerprint,
		Provider: provider, ResultFingerprint: completionPacketFingerprint(packet), ArtifactRoot: artifactDir,
		Exclude: map[string]bool{"completion.json": true, CompletionManifestFilename: true, IntegrationManifestFilename: true},
	}
	manifestPath := filepath.Join(artifactDir, CompletionManifestFilename)
	manifest, err := writeCompletionManifest(manifestPath, input)
	if err != nil {
		return packet, err
	}
	referencePath := filepath.ToSlash(CompletionManifestFilename)
	if relative, relErr := filepath.Rel(task.Scope.Root, manifestPath); relErr == nil {
		referencePath = filepath.ToSlash(relative)
	}
	packet.Manifest = completionManifestReference(referencePath, manifest)
	packet.Artifacts = appendUnique(packet.Artifacts, referencePath)
	return packet, nil
}
