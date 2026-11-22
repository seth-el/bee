package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func TestNonInteractivePromptedPolicyAllowsOnlyExplicitAutoMode(t *testing.T) {
	for _, mode := range []bee.ApprovalMode{"", bee.ApprovalModeManual, "invalid"} {
		if err := validateNonInteractiveApprovalChannel(bee.AssistantPolicyPrompted, mode, true, false); err == nil {
			t.Fatalf("mode %q did not fail closed", mode)
		}
	}
	if err := validateNonInteractiveApprovalChannel(bee.AssistantPolicyPrompted, bee.ApprovalModeAuto, true, false); err != nil {
		t.Fatalf("auto mode rejected: %v", err)
	}
	if err := validateNonInteractiveApprovalChannel(bee.AssistantPolicyPrompted, bee.ApprovalModeAuto, false, true); err != nil {
		t.Fatalf("auto RPC mode rejected: %v", err)
	}
}

func TestBeeCodeCommandIsRemoved(t *testing.T) {
	var stderr bytes.Buffer
	if err := run([]string{"code", "--project-root", t.TempDir(), "inspect"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("bee code err=%v stderr=%q", err, stderr.String())
	}
}

func TestOrdinaryBeeHelpExposesCodingCompositionFlags(t *testing.T) {
	var stderr bytes.Buffer
	err := run([]string{"--help"}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if err == nil {
		t.Fatal("expected flag help sentinel")
	}
	for _, name := range []string{"-coding-project-root", "-coding-managed-root", "-coding-base-commit", "-serve", "-serve-addr", "-serve-token-file", "-serve-policy"} {
		if !strings.Contains(stderr.String(), name) {
			t.Fatalf("help missing %q: %s", name, stderr.String())
		}
	}
}
