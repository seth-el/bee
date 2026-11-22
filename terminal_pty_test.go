package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testTerminalManager(t *testing.T) *TerminalManager {
	return testTerminalManagerWithBuffer(t, 4096)
}

func testTerminalManagerWithBuffer(t *testing.T, maxBuffer int) *TerminalManager {
	t.Helper()
	cfg := DefaultTerminalConfig()
	cfg.Enabled = true
	cfg.MaxBufferBytes = maxBuffer
	cfg.MaxWaitMS = 250
	manager, err := NewTerminalManager(ExecutionScope{Root: t.TempDir(), EnvAllowlist: []string{"PATH", "HOME", "TERM"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func waitTerminal(t *testing.T, manager *TerminalManager, id string, want string) TerminalReadResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result, err := manager.Read(context.Background(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Output, want) || result.Status != TerminalSessionRunning {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal output did not contain %q", want)
	return TerminalReadResult{}
}

func TestTerminalReportsExplicitUnsandboxedMode(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ExecutionMode != "unsandboxed" {
		t.Fatalf("execution_mode=%q", opened.ExecutionMode)
	}
}

func TestTerminalPersistentCwdEnvAndIncrementalRead(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Submit(context.Background(), opened.ID, "export BEE_TERM_TEST=ok; mkdir -p pty-child; cd pty-child; printf '%s' \"$BEE_TERM_TEST\"", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Output, "ok") || first.Running {
		t.Fatalf("first=%#v", first)
	}
	second, err := manager.Submit(context.Background(), opened.ID, "pwd", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Output, "pty-child") {
		t.Fatalf("cwd did not persist: %#v", second)
	}
	cursor := second.Cursor
	if err := manager.Write(context.Background(), opened.ID, "printf incremental"); err != nil {
		t.Fatal(err)
	}
	got := waitTerminal(t, manager, opened.ID, "incremental")
	if got.Cursor <= cursor || !strings.Contains(got.Output, "incremental") {
		t.Fatalf("incremental=%#v cursor=%d", got, cursor)
	}
	read, err := manager.Read(context.Background(), opened.ID, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Output, "incremental") {
		t.Fatalf("read=%#v", read)
	}
}

func TestTerminalRunningThenLaterReadAndTruncation(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Submit(context.Background(), opened.ID, "sleep .3; printf done", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Running {
		t.Fatalf("expected running result: %#v", result)
	}
	later := waitTerminal(t, manager, opened.ID, "done")
	if !strings.Contains(later.Output, "done") {
		t.Fatalf("later=%#v", later)
	}
	manager.CloseAll()
	manager = testTerminalManagerWithBuffer(t, 64)
	opened, err = manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		_ = manager.Write(context.Background(), opened.ID, "printf x\n")
	}
	time.Sleep(100 * time.Millisecond)
	truncated, err := manager.Read(context.Background(), opened.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncated || truncated.BaseCursor == 0 {
		t.Fatalf("expected truncation: %#v", truncated)
	}
}

func TestTerminalInterruptResizeShellDeathAndBounds(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Resize(context.Background(), opened.ID, 120, 40); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Submit(context.Background(), opened.ID, "sleep 10", 10*time.Millisecond)
	if err != nil || !result.Running {
		t.Fatalf("sleep result=%#v err=%v", result, err)
	}
	if err := manager.Interrupt(context.Background(), opened.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, inspectErr := manager.Inspect(opened.ID)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if info.Status != TerminalSessionRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg := manager.config
	if err := manager.Write(context.Background(), opened.ID, strings.Repeat("x", cfg.MaxInputBytes+1)); err == nil {
		t.Fatal("oversized input accepted")
	}
	if err := manager.Close(context.Background(), opened.ID); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Inspect(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status == TerminalSessionRunning {
		t.Fatalf("shell still running: %#v", info)
	}
	dead, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(context.Background(), dead.ID, "exit\n"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err = manager.Inspect(dead.ID)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != TerminalSessionRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info.Status == TerminalSessionRunning {
		t.Fatalf("dead shell still running: %#v", info)
	}

}

func TestTerminalManagerConcurrentOperationsAndTool(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = manager.Write(context.Background(), opened.ID, "printf .") }()
	}
	wg.Wait()
	tool := TerminalTool{Manager: manager}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"inspect","session_id":"`+opened.ID+`"}`))
	if err != nil || !strings.Contains(output, opened.ID) {
		t.Fatalf("tool output=%s err=%v", output, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"bad"}`)); err == nil {
		t.Fatal("bad action accepted")
	}
	if _, err := manager.Inspect("missing"); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("missing err=%v", err)
	}
}

func TestTerminalToolOpenCommandAndSubmitInputAlias(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	tool := TerminalTool{Manager: manager}

	openedJSON, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"open","cwd":".","command":"export BEE_OPEN_VALUE=ready; printf 'OPEN:%s\\n' \"$BEE_OPEN_VALUE\""}`))
	if err != nil {
		t.Fatal(err)
	}
	var opened TerminalSessionInfo
	if err := json.Unmarshal([]byte(openedJSON), &opened); err != nil {
		t.Fatal(err)
	}
	if opened.ID == "" {
		t.Fatalf("open result=%s", openedJSON)
	}
	first := waitTerminal(t, manager, opened.ID, "OPEN:ready")

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"submit","session_id":"`+opened.ID+`","input":"printf 'SUBMIT:%s\\n' \"$BEE_OPEN_VALUE\""}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		result, readErr := manager.Read(context.Background(), opened.ID, first.Cursor)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(result.Output, "SUBMIT:ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submit input alias produced no command output: %#v", result)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"submit","session_id":"`+opened.ID+`"}`)); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("missing submit command err=%v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"run","session_id":"`+opened.ID+`","command":"true","input":"true"}`)); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("conflicting command aliases err=%v", err)
	}
}

func TestTerminalToolReadHonorsWaitMS(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	tool := TerminalTool{Manager: manager}
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	initial, err := manager.Read(context.Background(), opened.ID, 0)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = manager.Write(context.Background(), opened.ID, "printf 'WAITED\\n'\n")
	}()
	started := time.Now()
	output, err := tool.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"action":"read","session_id":%q,"cursor":%d,"wait_ms":500}`, opened.ID, initial.Cursor)))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 25*time.Millisecond {
		t.Fatalf("read returned before new output: %s", output)
	}
	var result TerminalReadResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "WAITED") {
		t.Fatalf("waited read output=%q", result.Output)
	}
}

func TestTerminalConcurrentSubmissionsAreSerialized(t *testing.T) {
	manager := testTerminalManager(t)
	defer manager.CloseAll()
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan TerminalReadResult, 2)
	for _, marker := range []string{"first", "second"} {
		wg.Add(1)
		go func(marker string) {
			defer wg.Done()
			result, submitErr := manager.Submit(context.Background(), opened.ID, "printf '"+marker+"'", time.Second)
			if submitErr != nil {
				t.Errorf("submit %s: %v", marker, submitErr)
				return
			}
			results <- result
		}(marker)
	}
	wg.Wait()
	close(results)
	for result := range results {
		if !result.Completed || result.Running {
			t.Fatalf("submission did not complete: %#v", result)
		}
	}
}

func TestApplicationShutdownClosesTerminalSessions(t *testing.T) {
	manager := testTerminalManager(t)
	opened, err := manager.Open(context.Background(), ".", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{Terminal: manager}
	if err := app.CloseExtensions(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Inspect(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status == TerminalSessionRunning {
		t.Fatalf("terminal session survived application shutdown: %#v", info)
	}
}

func TestTerminalConfigGateAndCompose(t *testing.T) {
	if _, err := NewTerminalManager(ExecutionScope{Root: t.TempDir()}, TerminalConfig{}); !errors.Is(err, ErrTerminalDisabled) {
		t.Fatalf("disabled manager err=%v", err)
	}
	config := DefaultConfig()
	if config.Terminal.Enabled {
		t.Fatal("terminal must be disabled by default")
	}
	if err := config.Terminal.Validate(); err != nil {
		t.Fatal(err)
	}
	config.Terminal.Enabled = true
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	memoryRoot := filepath.Join(t.TempDir(), "memory")
	if err := os.Mkdir(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	jobRoot := filepath.Join(t.TempDir(), "jobs")
	if err := os.Mkdir(jobRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	app, err := ComposeGeneralAssistant(config, "", AssistantCompositionOptions{
		Store: store, ScopeRoot: t.TempDir(), MemoryRoot: memoryRoot, JobStorePath: filepath.Join(jobRoot, "jobs.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.Terminal == nil {
		t.Fatal("enabled terminal manager was not attached to application")
	}
	if _, ok := app.Executor.Registry.Lookup(TerminalToolName); !ok {
		t.Fatal("enabled terminal tool was not registered")
	}
}
