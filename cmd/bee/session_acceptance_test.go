package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompiledBinaryDurableChatResumesSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("durable acceptance targets Linux")
	}
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		call := calls.Add(1)
		if call == 1 {
			if len(payload.Messages) < 2 || payload.Messages[len(payload.Messages)-2].Content != "first durable turn" || !strings.HasPrefix(payload.Messages[len(payload.Messages)-1].Content, "<bee-clock ") {
				t.Fatalf("first messages=%#v", payload.Messages)
			}
			writeSSE(writer, `{"choices":[{"delta":{"content":"first response"}}]}`)
			return
		}
		found := false
		for _, message := range payload.Messages {
			if message.Role == "user" && message.Content == "first durable turn" {
				found = true
			}
		}
		if !found {
			t.Fatalf("resumed request lost first turn: %#v", payload.Messages)
		}
		writeSSE(writer, `{"choices":[{"delta":{"content":"resumed response"}}]}`)
	}))
	defer server.Close()
	config := writeTestConfig(t, server.URL, "session-acceptance")
	binary := buildAcceptanceBinary(t, root)

	stdout, stderr, err := runCompiled(binary, "", "--config", config, "first durable turn")
	if err != nil {
		t.Fatalf("first run: %v stderr=%s", err, stderr)
	}
	prefix := "session: "
	lineStart := strings.Index(stdout, prefix)
	if lineStart < 0 {
		t.Fatalf("missing session line: %q", stdout)
	}
	line := stdout[lineStart:]
	line = line[:strings.IndexByte(line, '\n')]
	sessionID := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if sessionID == "" {
		t.Fatalf("empty session id: %q", stdout)
	}

	stdout, stderr, err = runCompiled(binary, "", "--config", config, "--session", sessionID, "second durable turn")
	if err != nil {
		t.Fatalf("resumed run: %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "resumed response") {
		t.Fatalf("resumed stdout=%q", stdout)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d", calls.Load())
	}

	// G8: --session last is the most recent session; --resume still works
	// as a deprecated alias.
	stdout, stderr, err = runCompiled(binary, "", "--config", config, "--session", "last", "third durable turn")
	if err != nil || !strings.Contains(stdout, "resumed response") {
		t.Fatalf("--session last: %v stdout=%q stderr=%s", err, stdout, stderr)
	}
	stdout, stderr, err = runCompiled(binary, "", "--config", config, "--resume", sessionID, "fourth durable turn")
	if err != nil || !strings.Contains(stdout, "resumed response") || !strings.Contains(stderr, "--resume is deprecated") {
		t.Fatalf("--resume alias: %v stdout=%q stderr=%s", err, stdout, stderr)
	}
	if calls.Load() != 4 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
}

func buildAcceptanceBinary(t *testing.T, root string) string {
	t.Helper()
	binary := filepath.Join(root, "bee")
	repoRoot := acceptanceRepoRoot(t)
	command := exec.Command("go", "build", "-o", binary, "./cmd/bee")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v output=%s", err, output)
	}
	return binary
}
