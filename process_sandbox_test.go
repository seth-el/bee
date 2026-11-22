package bee

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestDetectBubblewrapClassifiesFirstUseFailures(t *testing.T) {
	const path = "/nix/store/example-bubblewrap/bin/bwrap"
	tests := []struct {
		name   string
		lookup bubblewrapLookupFunc
		probe  bubblewrapProbeFunc
		exists bubblewrapExistsFunc
		want   []string
	}{
		{
			name:   "missing from PATH",
			lookup: func(string) (string, error) { return "", exec.ErrNotFound },
			probe:  func(string) (bool, error) { t.Fatal("probe called after missing lookup"); return false, nil },
			exists: func(string) error { return os.ErrNotExist },
			want:   []string{"not found in PATH", "pkgs.bubblewrap", "did not run the command unsandboxed"},
		},
		{
			name:   "dynamic loader missing",
			lookup: func(string) (string, error) { return path, nil },
			probe: func(string) (bool, error) {
				return false, &os.PathError{Op: "fork/exec", Path: path, Err: syscall.ENOENT}
			},
			exists: func(string) error { return nil },
			want:   []string{"found at", "could not load", "programs.nix-ld", "did not run the command unsandboxed"},
		},
		{
			name:   "other spawn error",
			lookup: func(string) (string, error) { return path, nil },
			probe: func(string) (bool, error) {
				return false, &os.PathError{Op: "fork/exec", Path: path, Err: syscall.EACCES}
			},
			exists: func(string) error { return nil },
			want:   []string{"could not be started", "permission denied", "Nix runtime closure", "did not run the command unsandboxed"},
		},
		{
			name:   "started but check failed",
			lookup: func(string) (string, error) { return path, nil },
			probe:  func(string) (bool, error) { return true, errors.New("exit status 1") },
			exists: func(string) error { return nil },
			want:   []string{"started", "--version check failed", "did not run the command unsandboxed"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := detectBubblewrap(test.lookup, test.probe, test.exists)
			if err == nil {
				t.Fatal("failure was not classified")
			}
			for _, fragment := range test.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("error %q does not contain %q", err, fragment)
				}
			}
			for _, forbidden := range []string{"apt", "dnf", "gentoo", "emerge"} {
				if strings.Contains(strings.ToLower(err.Error()), forbidden) {
					t.Fatalf("error contains forbidden package-manager guidance %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestDetectBubblewrapAcceptsRunnableExecutable(t *testing.T) {
	const path = "/nix/store/example-bubblewrap/bin/bwrap"
	got, err := detectBubblewrap(
		func(string) (string, error) { return path, nil },
		func(string) (bool, error) { return true, nil },
		func(string) error { return nil },
	)
	if err != nil || got != path {
		t.Fatalf("path=%q err=%v", got, err)
	}
}

func TestSandboxInvocationsMarkBubblewrapChildren(t *testing.T) {
	t.Setenv(processSandboxMarker, "")
	root := t.TempDir()
	program := filepath.Join(root, "command")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	setResolvedBubblewrapForTest(t, "/test/bwrap")
	scope := ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true}

	for _, test := range []struct {
		name  string
		build func(ExecutionScope, string, []string, string) (string, []string, error)
	}{
		{name: "write root", build: sandboxCommandInvocation},
		{name: "read only", build: sandboxReadOnlyCommandInvocation},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotProgram, args, err := test.build(scope, program, []string{"argument"}, root)
			if err != nil {
				t.Fatal(err)
			}
			if gotProgram != "/test/bwrap" {
				t.Fatalf("program=%q", gotProgram)
			}
			if !containsArgumentSequence(args, "--setenv", processSandboxMarker, processSandboxMarkerValue) {
				t.Fatalf("sandbox args do not overwrite the nesting marker: %q", args)
			}
		})
	}
}

func TestSandboxInvocationsSkipBubblewrapWhenAlreadyNested(t *testing.T) {
	t.Setenv(processSandboxMarker, processSandboxMarkerValue)
	root := t.TempDir()
	program := filepath.Join(root, "nested-command")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	resetBubblewrapAvailabilityForTest(t)

	previousOutput := log.Writer()
	var warning bytes.Buffer
	log.SetOutput(&warning)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	previousWarningOnce := processSandboxWarningOnce
	processSandboxWarningOnce = &sync.Once{}
	t.Cleanup(func() { processSandboxWarningOnce = previousWarningOnce })

	scope := ExecutionScope{Root: root, WriteRoot: root, EnforceWriteRoots: true}
	for _, build := range []func(ExecutionScope, string, []string, string) (string, []string, error){
		sandboxCommandInvocation,
		sandboxReadOnlyCommandInvocation,
	} {
		gotProgram, args, err := build(scope, filepath.Base(program), []string{"argument"}, root)
		if err != nil {
			t.Fatal(err)
		}
		if gotProgram != program {
			t.Fatalf("program=%q want %q", gotProgram, program)
		}
		if strings.Join(args, " ") != "argument" {
			t.Fatalf("args=%q", args)
		}
	}
	const warningText = "process sandbox: already inside a Bee sandbox; running unsandboxed-within-sandbox"
	if count := strings.Count(warning.String(), warningText); count != 1 {
		t.Fatalf("nested warning count=%d output=%q", count, warning.String())
	}
}

func TestSandboxMarkerDoesNotChangeUnsafeOptOut(t *testing.T) {
	t.Setenv(processSandboxMarker, processSandboxMarkerValue)
	resetBubblewrapAvailabilityForTest(t)

	program, args, err := sandboxCommandInvocation(
		ExecutionScope{EnforceWriteRoots: false},
		"not-resolved-under-unsafe-opt-out",
		[]string{"argument"},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if program != "not-resolved-under-unsafe-opt-out" || strings.Join(args, " ") != "argument" {
		t.Fatalf("program=%q args=%q", program, args)
	}
}

func TestPreserveProcessSandboxMarkerOverwritesChildEnvironment(t *testing.T) {
	t.Setenv(processSandboxMarker, processSandboxMarkerValue)
	original := []string{"PATH=/bin", processSandboxMarker + "=forged"}
	got := preserveProcessSandboxMarker(original, true)
	if strings.Join(got, "|") != "PATH=/bin|"+processSandboxMarker+"="+processSandboxMarkerValue {
		t.Fatalf("env=%q", got)
	}
	if original[1] != processSandboxMarker+"=forged" {
		t.Fatalf("input environment mutated: %q", original)
	}
	if got := preserveProcessSandboxMarker([]string{"PATH=/bin"}, false); len(got) != 1 {
		t.Fatalf("unsafe invocation unexpectedly received marker: %q", got)
	}
}

func containsArgumentSequence(arguments []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(arguments) {
		return false
	}
	for start := 0; start <= len(arguments)-len(sequence); start++ {
		matched := true
		for offset := range sequence {
			if arguments[start+offset] != sequence[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func setResolvedBubblewrapForTest(t *testing.T, path string) {
	t.Helper()
	processBubblewrapAvailability = bubblewrapAvailability{}
	processBubblewrapAvailability.once.Do(func() {
		processBubblewrapAvailability.path = path
	})
	t.Cleanup(func() { processBubblewrapAvailability = bubblewrapAvailability{} })
}

func resetBubblewrapAvailabilityForTest(t *testing.T) {
	t.Helper()
	processBubblewrapAvailability = bubblewrapAvailability{}
	t.Cleanup(func() { processBubblewrapAvailability = bubblewrapAvailability{} })
}
