package bee

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDelegatedCompletionManifestBindsHarnessAndArtifacts(t *testing.T) {
	root := t.TempDir()
	artifactDir := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "result.txt")
	if err := os.WriteFile(artifactPath, []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := ChildTask{
		TaskID: "task-a", ParentID: "parent-a", Objective: "inspect", Prompt: "do the work",
		AllowedTools: []string{"read_file"}, Mutability: TaskReadOnly, Scope: ExecutionScope{Root: root},
	}
	packet := ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildSucceeded, Summary: "done", Artifacts: []string{"artifacts/result.txt"}}
	config := DefaultConfig()
	config.Provider.Model = "manifest-model"
	config.Agent.SystemPrompt = "child prompt"
	config.Agent.systemPromptExplicit = true
	definitions := []ToolDefinition{{Name: "read_file", Mutability: ToolMutabilityReadOnly}}
	packet, err := finalizeDelegatedCompletion(task, packet, artifactDir, config, definitions, false, CompletionManifestDelegated, "", "", CompletionProviderIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Manifest == nil {
		t.Fatal("completion manifest reference is missing")
	}
	manifestPath := filepath.Join(artifactDir, CompletionManifestFilename)
	manifest, err := ReadCompletionManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Fingerprint != packet.Manifest.Fingerprint || len(manifest.Artifacts) != 1 || manifest.Artifacts[0].Path != "result.txt" {
		t.Fatalf("manifest = %#v; reference = %#v", manifest, packet.Manifest)
	}
	verification, err := VerifyCompletionManifest(manifestPath, CompletionManifestExpectation{
		ManifestFingerprint: packet.Manifest.Fingerprint,
		TaskFingerprint:     packet.Manifest.TaskFingerprint, HarnessFingerprint: packet.Manifest.HarnessFingerprint,
		CapabilityFingerprint: packet.Manifest.CapabilityFingerprint, ProviderFingerprint: packet.Manifest.ProviderFingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Reusable {
		t.Fatalf("verification = %#v", verification)
	}
	if err := os.WriteFile(artifactPath, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verification, err = VerifyCompletionManifest(manifestPath, CompletionManifestExpectation{})
	if err != nil {
		t.Fatal(err)
	}
	if verification.Reusable || len(verification.Problems) == 0 {
		t.Fatalf("changed artifact verification = %#v", verification)
	}
}

func TestCompletionManifestRejectsSymlinkArtifact(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(artifactRoot, "link")); err != nil {
		t.Fatal(err)
	}
	_, err := writeCompletionManifest(filepath.Join(artifactRoot, CompletionManifestFilename), completionManifestInput{
		Kind: CompletionManifestDelegated, TaskID: "task", ParentID: "parent", Status: ChildSucceeded,
		TaskFingerprint: fingerprintValue("test", "task"), HarnessFingerprint: fingerprintValue("test", "harness"),
		CapabilityFingerprint: fingerprintValue("test", "capability"), ProviderFingerprint: fingerprintValue("test", "provider"),
		ResultFingerprint: fingerprintValue("test", "result"), ArtifactRoot: artifactRoot,
	})
	if err == nil {
		t.Fatal("symlink artifact was accepted")
	}
}
