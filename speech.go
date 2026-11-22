package bee

import (
	"errors"
	"path/filepath"
	"strings"
)

// SpeechConfig controls optional host-local transcription and synthesis
// helpers. Executables are disabled unless explicitly configured. Relative
// paths are resolved beneath BEE_HOME so a profile can move between hosts.
type SpeechConfig struct {
	TranscriptionScript       string `json:"transcription_script,omitempty"`
	SynthesisWrapper          string `json:"synthesis_wrapper,omitempty"`
	SynthesisWorkingDirectory string `json:"synthesis_working_directory,omitempty"`
	SynthesisOutputDirectory  string `json:"synthesis_output_directory,omitempty"`
}

type resolvedSpeechConfig struct {
	TranscriptionScript       string
	SynthesisWrapper          string
	SynthesisWorkingDirectory string
	SynthesisOutputDirectory  string
}

func (config SpeechConfig) Validate() error {
	for name, value := range map[string]string{
		"transcription_script":        config.TranscriptionScript,
		"synthesis_wrapper":           config.SynthesisWrapper,
		"synthesis_working_directory": config.SynthesisWorkingDirectory,
		"synthesis_output_directory":  config.SynthesisOutputDirectory,
	} {
		if strings.ContainsRune(value, '\x00') {
			return errors.New("speech " + name + " contains NUL")
		}
	}
	return nil
}

func (config SpeechConfig) resolveTranscriptionScript() (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	return resolveBeeHomePath(config.TranscriptionScript)
}

func (config SpeechConfig) resolve() (resolvedSpeechConfig, error) {
	transcription, err := config.resolveTranscriptionScript()
	if err != nil {
		return resolvedSpeechConfig{}, err
	}
	wrapper, err := resolveBeeHomePath(config.SynthesisWrapper)
	if err != nil {
		return resolvedSpeechConfig{}, err
	}
	working := strings.TrimSpace(config.SynthesisWorkingDirectory)
	if working == "" {
		working = "workspace"
	}
	working, err = resolveBeeHomePath(working)
	if err != nil {
		return resolvedSpeechConfig{}, err
	}
	output := strings.TrimSpace(config.SynthesisOutputDirectory)
	if output == "" {
		output = filepath.Join(working, "generated", "voice-replies")
	} else {
		output, err = resolveBeeHomePath(output)
		if err != nil {
			return resolvedSpeechConfig{}, err
		}
	}
	return resolvedSpeechConfig{
		TranscriptionScript:       transcription,
		SynthesisWrapper:          wrapper,
		SynthesisWorkingDirectory: working,
		SynthesisOutputDirectory:  output,
	}, nil
}
