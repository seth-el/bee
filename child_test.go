package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type childTestProvider struct {
	mu       sync.Mutex
	mode     string
	calls    int
	observed []Request
}

func (provider *childTestProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("complete is not used")
}

func (provider *childTestProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.calls++
	provider.observed = append(provider.observed, request)
	call := provider.calls
	provider.mu.Unlock()
	switch provider.mode {
	case "timeout":
		<-ctx.Done()
		return Response{}, ctx.Err()
	case "output":
		if err := onDelta(strings.Repeat("x", 32)); err != nil {
			return Response{}, err
		}
		return Response{Message: Message{Role: RoleAssistant, Content: "unreachable"}}, nil
	case "tool":
		if call == 1 {
			return Response{Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "touch", Arguments: `{"path":"child.txt"}`}}}}, nil
		}
	}
	if err := onDelta("child completed"); err != nil {
		return Response{}, err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: "child completed"}}, nil
}

func childTask(root string) ChildTask {
	return ChildTask{
		TaskID:             "task-1",
		ParentID:           "parent-1",
		Objective:          "perform bounded child work",
		Prompt:             "Do the requested child work.",
		Scope:              ExecutionScope{Root: root, TimeoutSeconds: 1, MaxOutputBytes: 64},
		Budget:             ChildBudget{TimeoutSeconds: 1, MaxSteps: 2, MaxOutputBytes: 64},
		AcceptanceCriteria: []string{"return a structured completion"},
	}
}

type childObservationCollector struct {
	mu     sync.Mutex
	events []ChildObservation
}

func (collector *childObservationCollector) ObserveChildEvent(event ChildObservation) {
	collector.mu.Lock()
	collector.events = append(collector.events, event)
	collector.mu.Unlock()
}

func (collector *childObservationCollector) Types() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	result := make([]string, 0, len(collector.events))
	for _, event := range collector.events {
		result = append(result, event.Type)
	}
	return result
}

func TestChildRunnerObservesLifecycleProviderAndCompletion(t *testing.T) {
	root := t.TempDir()
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, &childTestProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	collector := &childObservationCollector{}
	packet, err := runner.RunObserved(context.Background(), childTask(root), collector)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded {
		t.Fatalf("packet = %#v", packet)
	}
	types := collector.Types()
	for _, want := range []string{"started", "provider_delta", "assistant_message", "usage", "completed"} {
		found := false
		for _, got := range types {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("observations = %#v, missing %q", types, want)
		}
	}
}

func TestChildRunnerSuccessAndArtifacts(t *testing.T) {
	root := t.TempDir()
	provider := &childTestProvider{}
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	task := childTask(root)
	packet, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded || packet.Summary != "child completed" {
		t.Fatalf("packet = %#v", packet)
	}
	if len(packet.Artifacts) == 0 || !strings.HasSuffix(packet.Artifacts[0], "completion.json") {
		t.Fatalf("artifacts = %#v", packet.Artifacts)
	}
	if packet.Manifest == nil || !strings.HasSuffix(packet.Manifest.Path, CompletionManifestFilename) {
		t.Fatalf("manifest = %#v; packet = %#v", packet.Manifest, packet)
	}
	manifestPath := filepath.Join(root, "generated", "children", task.TaskID, CompletionManifestFilename)
	if _, err := ReadCompletionManifest(manifestPath); err != nil {
		t.Fatalf("read completion manifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "generated", "children", task.TaskID, "completion.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeChildCompletionPacket(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TaskID != task.TaskID {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestChildRunnerReportsChangedFilesAndCommands(t *testing.T) {
	root := t.TempDir()
	provider := &childTestProvider{mode: "tool"}
	config := DefaultConfig()
	config.Provider.Model = "test"
	tool := ToolFunc{ToolDefinition: ToolDefinition{Name: "touch", Risk: RiskLow, Parameters: json.RawMessage(`{"type":"object"}`)}, Fn: func(_ context.Context, args json.RawMessage) (string, error) {
		var input struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(root, input.Path), []byte("ok"), 0o644); err != nil {
			return "", err
		}
		return "wrote " + input.Path, nil
	}}
	registry, err := NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{Root: root, TimeoutSeconds: 1, MaxOutputBytes: 64}
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.Executor = NewExecutor(registry, scope)
	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	task := childTask(root)
	task.AllowedTools = []string{"touch"}
	packet, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildSucceeded {
		t.Fatalf("packet = %#v", packet)
	}
	if len(packet.ChangedFiles) != 1 || packet.ChangedFiles[0] != "child.txt" {
		t.Fatalf("changed files = %#v", packet.ChangedFiles)
	}
}

func TestChildExecutorStripsTrustedSSHFromSameRootRunCommand(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(RunCommandTool{Scope: ExecutionScope{Root: root, TrustedForeground: true}})
	if err != nil {
		t.Fatal(err)
	}
	base := NewExecutor(registry, ExecutionScope{Root: root, TrustedForeground: true})
	child, err := copyChildExecutor(base, ChildTask{TaskID: "child-ssh", ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{"run_command"}, Scope: ExecutionScope{Root: root}, Budget: ChildBudget{}, AcceptanceCriteria: []string{"done"}})
	if err != nil {
		t.Fatal(err)
	}
	if child.Scope.TrustedForeground || child.Scope.RemoteGrant != nil {
		t.Fatalf("child scope retained SSH authority: %#v", child.Scope)
	}
	tool, ok := child.Registry.Lookup("run_command")
	if !ok {
		t.Fatal("child run_command missing")
	}
	rebound, ok := tool.(RunCommandTool)
	if !ok {
		t.Fatalf("child run_command type=%T", tool)
	}
	if rebound.Scope.TrustedForeground || rebound.Scope.RemoteGrant != nil {
		t.Fatalf("child tool retained SSH authority: %#v", rebound.Scope)
	}
	if _, err := rebound.Execute(context.Background(), json.RawMessage(`{"target":"ssh","host":"vm.example","user":"testuser","argv":["hostname"]}`)); err == nil {
		t.Fatal("child unrestricted SSH unexpectedly succeeded")
	}
}

func TestChildExecutorRejectsParentOwnedMediaSearch(t *testing.T) {
	registry, err := NewRegistry(MediaSearchTool{Service: NewMediaSearchService(nil)})
	if err != nil {
		t.Fatal(err)
	}
	base := NewExecutor(registry, ExecutionScope{Root: t.TempDir()})
	_, err = copyChildExecutor(base, ChildTask{TaskID: "child-media", ParentID: "parent", Objective: "search", Prompt: "search", AllowedTools: []string{"media_search"}, Scope: ExecutionScope{Root: base.Scope.Root}, Budget: ChildBudget{}})
	if err == nil || !strings.Contains(err.Error(), "parent-owned") {
		t.Fatalf("media search child projection error=%v", err)
	}
}

func TestChildExecutorRejectsParentOwnedMediaRetrieve(t *testing.T) {
	registry, err := NewRegistry(MediaRetrieveTool{Search: NewMediaSearchService(nil), Media: NewMediaService(nil)})
	if err != nil {
		t.Fatal(err)
	}
	base := NewExecutor(registry, ExecutionScope{Root: t.TempDir()})
	_, err = copyChildExecutor(base, ChildTask{TaskID: "child-media-retrieve", ParentID: "parent", Objective: "retrieve", Prompt: "retrieve", AllowedTools: []string{MediaRetrieveToolName}, Scope: ExecutionScope{Root: base.Scope.Root}, Budget: ChildBudget{}})
	if err == nil || !strings.Contains(err.Error(), "parent-owned") {
		t.Fatalf("media retrieve child projection error=%v", err)
	}
}

func TestChildRunnerTimeoutAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(context.Context) (context.Context, context.CancelFunc)
		want   ChildStatus
	}{
		{name: "timeout", cancel: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, 20*time.Millisecond)
		}, want: ChildTimedOut},
		{name: "cancel", cancel: func(parent context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(parent) }, want: ChildCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			provider := &childTestProvider{mode: "timeout"}
			config := DefaultConfig()
			config.Provider.Model = "test"
			app, err := NewApplication(config, provider, nil)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := NewChildRunner(app)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := test.cancel(context.Background())
			if test.name == "cancel" {
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			} else {
				defer cancel()
			}
			packet, err := runner.Run(ctx, childTask(root))
			if err != nil {
				t.Fatal(err)
			}
			if packet.Status != test.want {
				t.Fatalf("status = %q, packet = %#v", packet.Status, packet)
			}
		})
	}
}

func TestChildRunnerOutputBudget(t *testing.T) {
	root := t.TempDir()
	provider := &childTestProvider{mode: "output"}
	config := DefaultConfig()
	config.Provider.Model = "test"
	app, err := NewApplication(config, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewChildRunner(app)
	if err != nil {
		t.Fatal(err)
	}
	task := childTask(root)
	task.Budget.MaxOutputBytes = 8
	packet, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Status != ChildBudgetExceeded || !packet.OutputTruncated {
		t.Fatalf("packet = %#v", packet)
	}
	if len(packet.Summary) != 8 {
		t.Fatalf("summary length = %d (%q)", len(packet.Summary), packet.Summary)
	}
}

func TestDecodeChildCompletionPacketRejectsMalformedOrIncomplete(t *testing.T) {
	for _, input := range []string{"", "not-json", `{"task_id":"task-1"}`, `{"task_id":"task-1","parent_id":"parent-1","status":"unknown","summary":"x"}`} {
		if _, err := DecodeChildCompletionPacket([]byte(input)); err == nil {
			t.Fatalf("input %q unexpectedly decoded", input)
		}
	}
}
