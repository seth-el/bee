package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type acceptanceRequest struct {
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func TestCompiledBinaryM95Acceptance(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("M9.5 acceptance targets supported Linux only")
	}
	root := t.TempDir()
	t.Setenv("BEE_HOME", "")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("BEE_SESSION_ROOT", filepath.Join(root, "data", "bee", "sessions"))
	binary := filepath.Join(root, "bee")
	repoRoot := acceptanceRepoRoot(t)
	build := exec.Command("go", "build", "-o", binary, "./cmd/bee")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiled Bee: %v\n%s", err, output)
	}

	t.Run("ordinary chat uses the unified tool surface", func(t *testing.T) {
		server := acceptanceProvider(t, func(call int, tools []string) string {
			if call != 1 || len(tools) == 0 {
				t.Fatalf("ordinary request call=%d tools=%v", call, tools)
			}
			return acceptanceContentSSE("ordinary chat")
		})
		defer server.Close()
		config := writeTestConfig(t, server.URL, "acceptance-model")
		stdout, stderr, err := runCompiled(binary, "", "--config", config, "hello")
		if err != nil {
			t.Fatalf("ordinary chat: %v\nstderr=%s", err, stderr)
		}
		if !strings.Contains(stdout, "ordinary chat") {
			t.Fatalf("ordinary stdout=%q", stdout)
		}
	})

	t.Run("coding plan approval scope and persistence", func(t *testing.T) {
		project := filepath.Join(root, "project")
		if err := os.MkdirAll(project, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("inspect me\n"), 0600); err != nil {
			t.Fatal(err)
		}
		server := acceptanceProvider(t, func(call int, tools []string) string {
			switch call {
			case 1:
				acceptanceRequireTools(t, tools, "bee_plan", "read_file", "write_file")
				return acceptanceToolSSE("read-1", "read_file", `{"path":"README.md"}`)
			case 2:
				return acceptanceToolSSE("plan-1", "bee_plan", `{"id":"acceptance-plan","goal":"update project","inputs":["README.md"],"outputs":["result.txt"],"steps":["inspect","write","verify"],"acceptance":["result exists"]}`)
			case 3:
				return acceptanceToolSSE("write-1", "write_file", `{"path":"result.txt","content":"ACCEPTED SECRET"}`)
			case 4:
				return acceptanceContentSSE("coding complete")
			default:
				t.Fatalf("unexpected coding provider request %d tools=%v", call, tools)
				return ""
			}
		})
		defer server.Close()
		config := writeTestConfig(t, server.URL, "acceptance-model")
		stdout, stderr, err := runCompiled(binary, "", "--plan", "--config", config, "--scope-root", project, "inspect and update")
		if err != nil {
			t.Fatalf("coding acceptance: %v\nstderr=%s", err, stderr)
		}
		data, err := os.ReadFile(filepath.Join(project, "result.txt"))
		if err != nil || string(data) != "ACCEPTED SECRET" {
			t.Fatalf("result=%q err=%v", data, err)
		}
		if !strings.Contains(stdout, "coding complete") {
			t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
		}
		if strings.Contains(stderr, "ACCEPTED SECRET") {
			t.Fatalf("approval/audit leaked file contents: %q", stderr)
		}
		if sessions := acceptanceFiles(filepath.Join(root, "data"), ".jsonl"); len(sessions) == 0 {
			t.Fatal("compiled coding run did not persist a durable session")
		}
	})

	t.Run("denied approval does not mutate", func(t *testing.T) {
		project := filepath.Join(root, "denied-project")
		if err := os.MkdirAll(project, 0700); err != nil {
			t.Fatal(err)
		}
		server := acceptanceProvider(t, func(call int, tools []string) string {
			switch call {
			case 1:
				acceptanceRequireTools(t, tools, "bee_plan", "write_file")
				return acceptanceToolSSE("plan-1", "bee_plan", `{"id":"deny-plan","goal":"write","inputs":["root"],"outputs":["denied.txt"],"steps":["write"],"acceptance":["file exists"]}`)
			case 2:
				return acceptanceToolSSE("write-1", "write_file", `{"path":"denied.txt","content":"MUST NOT EXIST"}`)
			case 3:
				return acceptanceContentSSE("approval denied")
			default:
				t.Fatalf("unexpected denied provider request %d", call)
				return ""
			}
		})
		defer server.Close()
		config := writeTestConfig(t, server.URL, "acceptance-model")
		stdout, stderr, err := runCompiled(binary, "n\n", "--plan", "--policy", "prompted", "--config", config, "--scope-root", project, "write it")
		if err != nil {
			t.Fatalf("denied run: %v\nstderr=%s", err, stderr)
		}
		if _, err := os.Stat(filepath.Join(project, "denied.txt")); !os.IsNotExist(err) {
			t.Fatalf("denied mutation exists, stat err=%v", err)
		}
		if !strings.Contains(stdout, "approval denied") || !strings.Contains(stderr, "denied") {
			t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
		}
	})
}

func acceptanceRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate acceptance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func acceptanceProvider(t *testing.T, handler func(int, []string) string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload acceptanceRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		tools := make([]string, 0, len(payload.Tools))
		for _, tool := range payload.Tools {
			tools = append(tools, tool.Function.Name)
		}
		if payload.Tools == nil {
			tools = []string{}
		}
		response := handler(int(calls.Add(1)), tools)
		if response == "" {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, "data: %s\n\ndata: [DONE]\n\n", response)
	}))
}

func acceptanceRequireTools(t *testing.T, tools []string, required ...string) {
	t.Helper()
	for _, want := range required {
		found := false
		for _, got := range tools {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tools=%v missing %q", tools, want)
		}
	}
}

func acceptanceContentSSE(content string) string {
	data, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, data)
}

func acceptanceToolSSE(id, name, arguments string) string {
	args, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`, id, name, args)
}

func runCompiled(binary, stdin string, args ...string) (string, string, error) {
	command := exec.Command(binary, args...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func acceptanceFiles(root, suffix string) []string {
	var matches []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(path, suffix) {
			matches = append(matches, path)
		}
		return nil
	})
	return matches
}
