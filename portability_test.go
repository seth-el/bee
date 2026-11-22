package bee

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func TestPortableConfigResolvesAgainstNewBeeHome(t *testing.T) {
	sourceHome := filepath.Join(t.TempDir(), "source-user", ".bee")
	targetHome := filepath.Join(t.TempDir(), "vm-user", ".bee")
	t.Setenv("BEE_HOME", sourceHome)
	if _, err := InitializeBee(sourceHome); err != nil {
		t.Fatal(err)
	}
	document := []byte(`{
  "agent": {"system_prompt_file": "workspace/SOUL.md"},
  "semantic": {"index_root": "state/memory-semantic"},
  "speech": {
    "transcription_script": "tools/transcribe",
    "synthesis_wrapper": "tools/synthesize",
    "synthesis_working_directory": "workspace",
    "synthesis_output_directory": "workspace/generated/voice-replies"
  }
}`)
	if err := os.WriteFile(filepath.Join(sourceHome, "config.json"), document, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Setenv("BEE_HOME", targetHome); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeBee(targetHome); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetHome, "config.json"), document, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	promptFile, err := resolveSystemPromptFile(config.Agent.SystemPromptFile)
	if err != nil {
		t.Fatal(err)
	}
	speech, err := config.Speech.resolve()
	if err != nil {
		t.Fatal(err)
	}
	indexRoot, err := config.Semantic.EffectiveIndexRoot()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := DefaultBeeSessionRoot()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"system prompt": promptFile,
		"transcription": speech.TranscriptionScript,
		"synthesis":     speech.SynthesisWrapper,
		"working":       speech.SynthesisWorkingDirectory,
		"output":        speech.SynthesisOutputDirectory,
		"semantic":      indexRoot,
		"sessions":      sessions,
	} {
		if !pathWithin(targetHome, path) || strings.Contains(path, sourceHome) {
			t.Fatalf("%s path %q did not follow target BEE_HOME %q", name, path, targetHome)
		}
	}
}

func TestExecutableSourceHasNoUserSpecificHomePaths(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate portability test")
	}
	root := filepath.Dir(currentFile)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != root && (info.Name() == ".git" || info.Name() == "generated" || info.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{"/home/otheruser", "/home/testuser"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("runtime source %s contains user-specific path %q", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
