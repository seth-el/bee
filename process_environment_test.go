package bee

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

const (
	processEnvironmentAllowed = "BEE_PROCESS_ENV_ALLOWED"
	processEnvironmentHidden  = "BEE_PROCESS_ENV_HIDDEN"
)

// Capture the exec environment before TestMain adds its isolated HOME roots.
// The helper subprocess reports this snapshot so the assertion covers the
// actual process boundary rather than later test setup.
var processEnvironmentAtStartup = os.Environ()

func TestAllowlistedChildEnvironmentReceivesExactlyAllowedVariables(t *testing.T) {
	t.Setenv(processEnvironmentAllowed, "visible")
	t.Setenv(processEnvironmentHidden, "hidden")

	command := exec.Command(os.Args[0], "-test.run=^TestAllowlistedChildEnvironmentHelperProcess$", "--")
	setAllowlistedChildEnvironment(command, []string{processEnvironmentAllowed}, false)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var received []string
	if err := json.Unmarshal(output, &received); err != nil {
		t.Fatalf("decode child environment: %v", err)
	}
	if len(received) != 1 || received[0] != processEnvironmentAllowed+"=visible" {
		// Do not print the received environment: a regression could otherwise
		// copy a parent credential into test output.
		t.Fatalf("child received %d environment entries; want exactly one allowlisted entry", len(received))
	}
}

func TestAllowlistedChildEnvironmentHelperProcess(t *testing.T) {
	if os.Getenv(processEnvironmentAllowed) == "" {
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(processEnvironmentAtStartup); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
