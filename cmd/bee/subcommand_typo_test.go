package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestLikelySubcommandTypo(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"sesions", "list"}, "sessions"},
		{[]string{"plugins"}, "plugin"},
		{[]string{"skills", "list"}, "skill"},
		{[]string{"doctr"}, "doctor"},
		{[]string{"memroy"}, "memory"},
		{[]string{"confg"}, "config"},
		{[]string{"hlep"}, ""}, // distance 2 at length 4: treated as a prompt
	} {
		got, ok := likelySubcommandTypo(test.args)
		if test.want == "" {
			if ok {
				t.Fatalf("%v flagged as typo of %q", test.args, got)
			}
			continue
		}
		if !ok || got != test.want {
			t.Fatalf("%v = %q, %v; want %q", test.args, got, ok, test.want)
		}
	}
	// Ordinary prompts must still reach the model.
	for _, args := range [][]string{
		{"hello"}, {"hi"}, {"thanks"}, {"jokes"}, {"status"},
		{"what is on my calendar"},
		{"fix", "the", "tests"},
		{"Plugins"},
		{"jobs"}, {"sessions"}, // exact subcommands dispatch, never typo-flagged
		{"--", "sesions"},
	} {
		if got, ok := likelySubcommandTypo(args); ok {
			t.Fatalf("prompt %v flagged as typo of %q", args, got)
		}
	}
}

func TestHelpWordPrintsUsageWithoutAPrompt(t *testing.T) {
	// `bee help` behaves exactly like `bee --help` (usage, flag-help
	// sentinel) and never becomes a prompt.
	var output, errorOutput bytes.Buffer
	err := run([]string{"help"}, strings.NewReader(""), &output, &errorOutput)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("bee help err=%v, want flag.ErrHelp like --help", err)
	}
	if !strings.Contains(errorOutput.String(), "Usage: bee") || strings.Contains(output.String(), "session:") {
		t.Fatalf("help output=%q stderr=%q", output.String(), errorOutput.String())
	}
}

func TestSubcommandTypoFailsBeforeAnyModelCall(t *testing.T) {
	var output, errorOutput bytes.Buffer
	err := run([]string{"sesions", "list"}, strings.NewReader(""), &output, &errorOutput)
	if err == nil || !strings.Contains(err.Error(), "bee sessions") || !strings.Contains(err.Error(), "bee -- sesions list") {
		t.Fatalf("typo err=%v", err)
	}
	if strings.Contains(output.String(), "session:") {
		t.Fatalf("typo started a session: %q", output.String())
	}
}
