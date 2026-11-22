package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const GoldenScenarioVersion = 1

// GoldenScenario is a deterministic, network-free harness fixture. Provider
// responses are scripted; expected output contains structure only, never model
// prose or full tool results.
type GoldenScenario struct {
	Version   int                     `json:"version"`
	Name      string                  `json:"name"`
	Turns     []GoldenTurn            `json:"turns"`
	Tools     []GoldenToolFixture     `json:"tools,omitempty"`
	Approval  ApprovalDecision        `json:"approval,omitempty"`
	Hooks     []GoldenHookFixture     `json:"hooks,omitempty"`
	MCP       *GoldenMCPFixture       `json:"mcp,omitempty"`
	Recurring *GoldenRecurringFixture `json:"recurring,omitempty"`
	Expected  []GoldenTraceEntry      `json:"expected"`
}

type GoldenTurn struct {
	Prompt    string                   `json:"prompt"`
	Responses []GoldenProviderResponse `json:"responses"`
}

type GoldenProviderResponse struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type GoldenToolFixture struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind,omitempty"`
	Output     string         `json:"output,omitempty"`
	Mutability ToolMutability `json:"mutability,omitempty"`
}

type GoldenHookFixture struct {
	Point   HookPoint   `json:"point"`
	Output  string      `json:"output,omitempty"`
	Outcome HookOutcome `json:"outcome,omitempty"`
}

type GoldenMCPFixture struct {
	Name string `json:"name"`
}

type GoldenRecurringFixture struct {
	StartAt         time.Time `json:"start_at"`
	TickAt          time.Time `json:"tick_at"`
	IntervalSeconds int       `json:"interval_seconds"`
	AllowedTools    []string  `json:"allowed_tools"`
	// FollowUp, when present, runs after the tick as an ordinary turn in a
	// fresh session (a new activation seam). The trace then records whether
	// that session's provider prefix carried the memory dashboard, proving a
	// file the job wrote is observed by the next session, not the job's own.
	FollowUp *GoldenTurn `json:"follow_up,omitempty"`
}

// GoldenTraceEntry is the normalized behavioral contract. Args are canonical
// JSON objects; outcomes are bounded categories; payload content is omitted.
type GoldenTraceEntry struct {
	Kind      string          `json:"kind"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Outcome   string          `json:"outcome,omitempty"`
	Point     HookPoint       `json:"point,omitempty"`
	HookIndex *int            `json:"hook_index,omitempty"`
	Path      string          `json:"path,omitempty"`
}

func (scenario GoldenScenario) Validate() error {
	if scenario.Version != GoldenScenarioVersion {
		return fmt.Errorf("scenario version must be %d", GoldenScenarioVersion)
	}
	if strings.TrimSpace(scenario.Name) == "" {
		return errors.New("scenario name is required")
	}
	if len(scenario.Turns) == 0 || len(scenario.Turns) > 16 {
		return errors.New("scenario must contain between 1 and 16 turns")
	}
	responseCount := 0
	for turnIndex, turn := range scenario.Turns {
		if strings.TrimSpace(turn.Prompt) == "" {
			return fmt.Errorf("turns[%d] prompt is required", turnIndex)
		}
		if len(turn.Responses) == 0 || len(turn.Responses) > 32 {
			return fmt.Errorf("turns[%d] must contain between 1 and 32 responses", turnIndex)
		}
		responseCount += len(turn.Responses)
		for responseIndex, response := range turn.Responses {
			if strings.TrimSpace(response.Content) == "" && len(response.ToolCalls) == 0 {
				return fmt.Errorf("turns[%d] responses[%d] is empty", turnIndex, responseIndex)
			}
			for _, call := range response.ToolCalls {
				if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
					return errors.New("scripted tool calls require id and name")
				}
				if canonical, err := canonicalJSONObject(json.RawMessage(call.Arguments)); err != nil || len(canonical) == 0 {
					return fmt.Errorf("tool call %q arguments must be a JSON object", call.Name)
				}
			}
		}
	}
	if responseCount > 128 {
		return errors.New("scenario contains too many provider responses")
	}
	seenTools := make(map[string]struct{}, len(scenario.Tools))
	for index, tool := range scenario.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("tools[%d] name is required", index)
		}
		if _, duplicate := seenTools[tool.Name]; duplicate {
			return fmt.Errorf("duplicate tool %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
		if tool.Kind != "" && tool.Kind != "static" && tool.Kind != "write_file" {
			return fmt.Errorf("tools[%d] kind %q is unsupported", index, tool.Kind)
		}
	}
	if scenario.Approval != "" && scenario.Approval != ApprovalApproved && scenario.Approval != ApprovalDenied {
		return fmt.Errorf("approval %q is invalid", scenario.Approval)
	}
	for index, hook := range scenario.Hooks {
		config := LifecycleHookConfig{Point: hook.Point, Command: "golden-fixture"}
		if err := config.Validate(); err != nil {
			return fmt.Errorf("hooks[%d]: %w", index, err)
		}
		if hook.Outcome != "" && hook.Outcome != HookSucceeded && hook.Outcome != HookNonzeroExit && hook.Outcome != HookTimedOut && hook.Outcome != HookCanceled && hook.Outcome != HookStartFailed {
			return fmt.Errorf("hooks[%d] outcome %q is invalid", index, hook.Outcome)
		}
	}
	if scenario.MCP != nil && strings.TrimSpace(scenario.MCP.Name) == "" {
		return errors.New("mcp name is required")
	}
	if scenario.Recurring != nil {
		if len(scenario.Turns) != 1 {
			return errors.New("recurring scenario requires exactly one turn")
		}
		fixture := scenario.Recurring
		if fixture.StartAt.IsZero() || fixture.TickAt.IsZero() || fixture.StartAt.Location() != time.UTC || fixture.TickAt.Location() != time.UTC || fixture.TickAt.Before(fixture.StartAt) {
			return errors.New("recurring scenario requires ordered UTC start_at and tick_at")
		}
		if fixture.IntervalSeconds < int(minRecurringInterval/time.Second) {
			return errors.New("recurring scenario interval is too short")
		}
		for _, name := range fixture.AllowedTools {
			if !validMCPName(name) {
				return fmt.Errorf("recurring scenario tool %q is invalid", name)
			}
		}
		if followUp := fixture.FollowUp; followUp != nil && (strings.TrimSpace(followUp.Prompt) == "" || len(followUp.Responses) == 0) {
			return errors.New("recurring follow_up requires a prompt and responses")
		}
	}
	return nil
}

func LoadGoldenScenario(path string) (GoldenScenario, error) {
	file, err := os.Open(path)
	if err != nil {
		return GoldenScenario{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return GoldenScenario{}, err
	}
	if len(data) > 1<<20 {
		return GoldenScenario{}, errors.New("scenario fixture exceeds 1048576 bytes")
	}
	var scenario GoldenScenario
	if err := decodeConfigJSON(data, &scenario); err != nil {
		return GoldenScenario{}, fmt.Errorf("decode scenario: %w", err)
	}
	if err := scenario.Validate(); err != nil {
		return GoldenScenario{}, fmt.Errorf("validate scenario: %w", err)
	}
	return scenario, nil
}

type goldenScriptedProvider struct {
	responses []GoldenProviderResponse
	requests  []Request
}

func (provider *goldenScriptedProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("golden provider supports streaming only")
}

func (provider *goldenScriptedProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.requests = append(provider.requests, request)
	if len(provider.responses) == 0 {
		return Response{}, errors.New("golden provider exhausted")
	}
	fixture := provider.responses[0]
	provider.responses = provider.responses[1:]
	if fixture.Content != "" && onDelta != nil {
		if err := onDelta(fixture.Content); err != nil {
			return Response{}, err
		}
	}
	calls := append([]ToolCall(nil), fixture.ToolCalls...)
	return Response{Message: Message{Role: RoleAssistant, Content: fixture.Content, ToolCalls: calls}}, nil
}

// RunGoldenScenario executes a fixture against the ordinary application,
// executor, approval, MCP, session, hook, and mutation seams. executable is
// the bee-eval binary used only by the stdio MCP fixture.
func RunGoldenScenario(ctx context.Context, scenario GoldenScenario, executable string) ([]GoldenTraceEntry, error) {
	if err := scenario.Validate(); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "bee-eval-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		return nil, err
	}
	journal, err := NewMutationJournal(filepath.Join(root, "mutations"), root)
	if err != nil {
		return nil, err
	}
	scope := ExecutionScope{Root: root, WriteRoot: root, MaxOutputBytes: 64 << 10}
	config := DefaultConfig()
	config.Memory.RecallMode = MemoryRecallOff
	for _, fixture := range scenario.Hooks {
		config.Hooks.Commands = append(config.Hooks.Commands, LifecycleHookConfig{Point: fixture.Point, Command: "golden-fixture", Args: []string{fmt.Sprintf("%d", len(config.Hooks.Commands))}})
	}
	if scenario.MCP != nil {
		if strings.TrimSpace(executable) == "" {
			return nil, errors.New("MCP scenario requires the bee-eval executable")
		}
		config.MCP.Servers = []MCPServerConfig{{Name: scenario.MCP.Name, Enabled: true, Command: executable, Args: []string{"mcp-stub"}, ReadOnlyTools: []string{"echo"}}}
	}
	var responses []GoldenProviderResponse
	for _, turn := range scenario.Turns {
		responses = append(responses, turn.Responses...)
	}
	if scenario.Recurring != nil && scenario.Recurring.FollowUp != nil {
		responses = append(responses, scenario.Recurring.FollowUp.Responses...)
	}
	provider := &goldenScriptedProvider{responses: responses}
	app, err := NewApplication(config, provider, sessions)
	if err != nil {
		return nil, err
	}
	// The scenario root stands in for the memory root, so a fixture that writes
	// MEMORY.md exercises the real activation-seam dashboard composition.
	app.Dashboard, err = NewMemoryDashboard(root)
	if err != nil {
		return nil, err
	}
	if len(scenario.Hooks) > 0 {
		fixtures := append([]GoldenHookFixture(nil), scenario.Hooks...)
		app.lifecycleHooks.execute = func(_ context.Context, _ ExecutionScope, config LifecycleHookConfig, index int, _ []byte) HookObservation {
			outcome := fixtures[index].Outcome
			if outcome == "" {
				outcome = HookSucceeded
			}
			return HookObservation{Point: config.Point, Index: index, Outcome: outcome, Output: fixtures[index].Output}
		}
	}
	registry, err := goldenRegistry(scope, journal, scenario.Tools)
	if err != nil {
		return nil, err
	}
	decision := scenario.Approval
	if decision == "" {
		decision = ApprovalDenied
	}
	app.Executor = &Executor{
		Registry: registry,
		Scope:    scope,
		Policy:   GeneralAssistantPolicy{Mode: AssistantPolicyPrompted},
		Approval: ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return decision, nil }),
	}
	if err := app.StartExtensions(ctx); err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = app.Close(context.Background())
		}
	}()
	var sessionID SessionID
	var recurringTrace *GoldenTraceEntry
	var followUpTrace *GoldenTraceEntry
	if scenario.Recurring != nil {
		if err := app.RestrictTools(scenario.Recurring.AllowedTools); err != nil {
			return nil, err
		}
		jobStore, storeErr := NewJobStore(filepath.Join(root, "jobs.json"))
		if storeErr != nil {
			return nil, storeErr
		}
		definition := RecurringJobsConfig{Jobs: []RecurringJobConfig{{
			ID: "golden", Enabled: true, Prompt: scenario.Turns[0].Prompt,
			Schedule:   RecurringScheduleConfig{StartAt: scenario.Recurring.StartAt, IntervalSeconds: scenario.Recurring.IntervalSeconds, TimeZone: "UTC"},
			Contract:   JobContract{AllowedTools: append([]string(nil), scenario.Recurring.AllowedTools...), WorkspacePaths: []string{root}, MaxWallClockSeconds: 30, MaxToolCalls: 8, OutputTarget: RecurringOutputSession, NotificationTarget: RecurringNotificationInbox},
			MaxCatchUp: 1, CatchUpWindowSecs: 3600, ConcurrencyPolicy: RecurringConcurrencyForbid,
		}}}
		scheduler, schedulerErr := NewRecurringScheduler(definition, jobStore)
		if schedulerErr != nil {
			return nil, schedulerErr
		}
		results, tickErr := scheduler.Tick(ctx, scenario.Recurring.TickAt, func(runCtx context.Context, job Job) (JobExecution, error) {
			events, runErr := app.Run(runCtx, RunRequest{Prompt: job.Request.Prompt, DisableCommands: true})
			if runErr != nil {
				return JobExecution{}, runErr
			}
			execution := JobExecution{Summary: "golden"}
			for event := range events {
				if event.SessionID != "" && execution.SessionID == "" {
					execution.SessionID = event.SessionID
				}
				if event.Type == RunEnd && event.Error != "" {
					return execution, errors.New("golden recurring run failed")
				}
			}
			return execution, nil
		}, nil)
		if tickErr != nil || len(results) != 1 || results[0].State != JobSucceeded || len(results[0].AttemptsLog) != 1 || results[0].AttemptsLog[0].SessionID == "" {
			if tickErr != nil {
				return nil, tickErr
			}
			return nil, errors.New("golden recurring firing did not produce one journaled session")
		}
		sessionID = results[0].AttemptsLog[0].SessionID
		entry := GoldenTraceEntry{Kind: "job", Outcome: string(results[0].State)}
		recurringTrace = &entry
		if followUp := scenario.Recurring.FollowUp; followUp != nil {
			jobRequests := len(provider.requests)
			if jobRequests == 0 || strings.Contains(goldenSystemPrefix(provider.requests[0]), "<bee-memory-dashboard>") {
				return nil, errors.New("golden recurring job observed a dashboard before writing it")
			}
			events, runErr := app.Run(ctx, RunRequest{Prompt: followUp.Prompt, DisableCommands: true})
			if runErr != nil {
				return nil, runErr
			}
			var followUpSession SessionID
			for event := range events {
				if event.Type == RunSession && followUpSession == "" {
					followUpSession = event.SessionID
				}
				if event.Type == RunEnd && event.Error != "" {
					return nil, errors.New(event.Error)
				}
			}
			if followUpSession == "" || followUpSession == sessionID || len(provider.requests) <= jobRequests {
				return nil, errors.New("golden follow-up did not run in a fresh session")
			}
			outcome := "absent"
			if strings.Contains(goldenSystemPrefix(provider.requests[jobRequests]), "<bee-memory-dashboard>") {
				outcome = "present"
			}
			dashboardEntry := GoldenTraceEntry{Kind: "next_session_prefix", Path: filepath.ToSlash(MemoryDashboardRelativePath()), Outcome: outcome}
			followUpTrace = &dashboardEntry
		}
	} else {
		for _, turn := range scenario.Turns {
			events, runErr := app.Run(ctx, RunRequest{SessionID: sessionID, Prompt: turn.Prompt, DisableCommands: true})
			if runErr != nil {
				return nil, runErr
			}
			for event := range events {
				if event.Type == RunSession && sessionID == "" {
					sessionID = event.SessionID
				}
				if event.Type == RunEnd && event.Error != "" {
					return nil, errors.New(event.Error)
				}
			}
		}
	}
	if len(provider.responses) != 0 {
		return nil, fmt.Errorf("golden provider retained %d unused responses", len(provider.responses))
	}
	if err := app.Close(ctx); err != nil {
		return nil, err
	}
	closed = true
	session, err := sessions.Load(sessionID)
	if err != nil {
		return nil, err
	}
	mutations, err := journal.Entries()
	if err != nil {
		return nil, err
	}
	trace, err := ProjectGoldenTrace(session.EventsSnapshot(), mutations)
	if err != nil {
		return nil, err
	}
	if recurringTrace != nil {
		trace = append(trace, *recurringTrace)
	}
	if followUpTrace != nil {
		trace = append(trace, *followUpTrace)
	}
	return trace, nil
}

// goldenSystemPrefix joins the leading system messages of one provider
// request: the stable prefix the dashboard is composed into.
func goldenSystemPrefix(request Request) string {
	var prefix strings.Builder
	for _, message := range request.Messages {
		if message.Role != RoleSystem {
			break
		}
		prefix.WriteString(message.Content)
		prefix.WriteByte('\n')
	}
	return prefix.String()
}

func goldenRegistry(scope ExecutionScope, journal *MutationJournal, fixtures []GoldenToolFixture) (*Registry, error) {
	registry, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	for _, fixture := range fixtures {
		if fixture.Kind == "write_file" {
			if fixture.Name != "write_file" {
				return nil, errors.New("write_file fixture must use the canonical tool name")
			}
			if err := registry.Register(WriteFileTool{Scope: scope, Journal: journal}); err != nil {
				return nil, err
			}
			continue
		}
		fixture := fixture
		mutability := fixture.Mutability
		if mutability == "" {
			mutability = ToolMutabilityReadOnly
		}
		tool := ToolFunc{ToolDefinition: ToolDefinition{Name: fixture.Name, Description: "deterministic golden fixture", Mutability: mutability}, Fn: func(context.Context, json.RawMessage) (string, error) { return fixture.Output, nil }}
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func ProjectGoldenTrace(events []Event, mutations []MutationJournalEntry) ([]GoldenTraceEntry, error) {
	trace := make([]GoldenTraceEntry, 0)
	for _, event := range events {
		switch event.Type {
		case EventLifecycle:
			payload, err := decodeEventPayload[LifecycleEvent](event, EventLifecycle)
			if err != nil {
				return nil, err
			}
			trace = append(trace, GoldenTraceEntry{Kind: "session", Outcome: string(payload.Action)})
		case EventMessage:
			payload, err := DecodeMessageEvent(event)
			if err != nil {
				return nil, err
			}
			if payload.Message.Role == RoleUser {
				trace = append(trace, GoldenTraceEntry{Kind: "turn", Outcome: "user"})
			}
		case EventToolCall:
			payload, err := DecodeToolCallEvent(event)
			if err != nil {
				return nil, err
			}
			args, err := canonicalJSONObject(json.RawMessage(payload.Call.Arguments))
			if err != nil {
				return nil, err
			}
			trace = append(trace, GoldenTraceEntry{Kind: "tool_call", Tool: payload.Call.Name, Args: args})
		case EventApproval:
			payload, err := decodeEventPayload[ApprovalEvent](event, EventApproval)
			if err != nil {
				return nil, err
			}
			outcome := string(payload.Decision)
			if outcome == "" {
				outcome = string(payload.Status)
			}
			trace = append(trace, GoldenTraceEntry{Kind: "approval", Tool: payload.Request.ToolName, Outcome: outcome})
		case EventHookObservation:
			payload, err := DecodeHookObservationEvent(event)
			if err != nil {
				return nil, err
			}
			index := payload.Observation.Index
			trace = append(trace, GoldenTraceEntry{Kind: "hook", Point: payload.Observation.Point, HookIndex: &index, Outcome: string(payload.Observation.Outcome)})
		case EventToolResult:
			payload, err := DecodeToolResultEvent(event)
			if err != nil {
				return nil, err
			}
			outcome := "succeeded"
			if payload.Error != "" {
				outcome = "failed"
			}
			tool := payload.EffectiveToolName
			if tool == "" {
				tool = payload.ToolName
			}
			trace = append(trace, GoldenTraceEntry{Kind: "tool_result", Tool: tool, Outcome: outcome})
		}
	}
	for _, mutation := range mutations {
		trace = append(trace, GoldenTraceEntry{Kind: "mutation", Tool: mutation.Tool, Path: mutation.Path, Outcome: "recorded"})
	}
	return trace, nil
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("arguments are not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("arguments contain trailing data")
	}
	return json.Marshal(object)
}

func CompareGoldenTrace(expected, actual []GoldenTraceEntry) error {
	expectedJSON, _ := json.MarshalIndent(expected, "", "  ")
	actualJSON, _ := json.MarshalIndent(actual, "", "  ")
	if bytes.Equal(expectedJSON, actualJSON) {
		return nil
	}
	return errors.New(UnifiedGoldenDiff(string(expectedJSON), string(actualJSON)))
}

// UnifiedGoldenDiff returns the smallest changed contiguous line window plus
// one context line on each side. Trace entries are already structural JSON, so
// this stays readable without importing a text-diff dependency.
func UnifiedGoldenDiff(expected, actual string) string {
	want := strings.Split(expected, "\n")
	got := strings.Split(actual, "\n")
	prefix := 0
	for prefix < len(want) && prefix < len(got) && want[prefix] == got[prefix] {
		prefix++
	}
	wantEnd, gotEnd := len(want), len(got)
	for wantEnd > prefix && gotEnd > prefix && want[wantEnd-1] == got[gotEnd-1] {
		wantEnd--
		gotEnd--
	}
	contextStart := prefix
	if contextStart > 0 {
		contextStart--
	}
	contextWantEnd := wantEnd
	contextGotEnd := gotEnd
	if contextWantEnd < len(want) {
		contextWantEnd++
	}
	if contextGotEnd < len(got) {
		contextGotEnd++
	}
	var diff strings.Builder
	fmt.Fprintf(&diff, "--- expected\n+++ actual\n@@ -%d,%d +%d,%d @@\n", contextStart+1, contextWantEnd-contextStart, contextStart+1, contextGotEnd-contextStart)
	if contextStart < prefix {
		diff.WriteString(" " + want[contextStart] + "\n")
	}
	for _, line := range want[prefix:wantEnd] {
		diff.WriteString("-" + line + "\n")
	}
	for _, line := range got[prefix:gotEnd] {
		diff.WriteString("+" + line + "\n")
	}
	if wantEnd < len(want) {
		diff.WriteString(" " + want[wantEnd] + "\n")
	}
	return strings.TrimSuffix(diff.String(), "\n")
}

func GoldenFixturePaths(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errors.New("no golden scenario fixtures found")
	}
	return paths, nil
}
