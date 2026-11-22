package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitAndDoctorCLI(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bee-home")
	var output bytes.Buffer
	if err := run([]string{"init", "--home", home}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "initialized") {
		t.Fatalf("init output = %q", output.String())
	}
	output.Reset()
	if err := run([]string{"doctor", "--home", home, "--strict"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "PASS") {
		t.Fatalf("doctor output = %q", output.String())
	}
}
