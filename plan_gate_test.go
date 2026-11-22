package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func validTestPlan(root string) TaskPlan {
	return TaskPlan{ID: "plan-1", Goal: "change the project", Inputs: []string{"project state"}, Outputs: []string{"updated file"}, Steps: []string{"inspect", "change", "test"}, Acceptance: []string{"tests pass"}, Scope: root}
}

func TestPlanGateBlocksUnknownAndAmbiguousMutations(t *testing.T) {
	gate := &PlanGate{}
	if err := gate.Check(ToolDefinition{Name: "custom"}); !errors.Is(err, ErrPlanRequired) {
		t.Fatalf("unknown tool err=%v", err)
	}
	plan := validTestPlan(t.TempDir())
	plan.Questions = []string{"which file?"}
	if err := gate.SetPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(ToolDefinition{Name: "custom", Mutability: ToolMutabilityMutating}); !errors.Is(err, ErrPlanHasClarification) {
		t.Fatalf("ambiguous mutation err=%v", err)
	}
	if err := gate.Check(ToolDefinition{Name: "grep", Mutability: ToolMutabilityReadOnly}); err != nil {
		t.Fatalf("read-only err=%v", err)
	}
}

func TestAgentPlanPersistsAndExplicitResetClearsGate(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCodingExecutor(ExecutionScope{Root: root}, ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalApproved, nil }), nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := validTestPlan(root)
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: executor, RequirePlan: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.Plan(); got == nil || got.ID != plan.ID {
		t.Fatalf("agent plan=%#v", got)
	}
	if err := agent.ResetPlan("new task"); err != nil {
		t.Fatal(err)
	}
	if agent.Plan() != nil {
		t.Fatalf("plan survived reset: agent=%#v", agent.Plan())
	}
	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestPlanFromEvents(loaded.Events); got != nil {
		t.Fatalf("persisted plan survived reset: %#v", got)
	}
}

func TestAgentCanDeferQuestionAndResumeMutationWithoutReset(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	question := "Ask the user to visually review the finished result in the browser."
	plan := validTestPlan(root)
	plan.Questions = []string{question}
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	executor, err := NewCodingExecutor(ExecutionScope{Root: root}, ApprovalFunc(func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
		return ApprovalApproved, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: executor, RequirePlan: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"blocked.txt","content":"blocked"}`)); !errors.Is(err, ErrPlanHasClarification) {
		t.Fatalf("mutation before disposition err=%v", err)
	}
	args, err := json.Marshal(PlanUpdate{PlanID: plan.ID, QuestionUpdates: []PlanQuestionUpdate{{
		Question:    question,
		Disposition: PlanQuestionDeferred,
		Reason:      "The review is a post-implementation acceptance check.",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	result := agent.executePlanTool(context.Background(), PlanUpdateToolName, args)
	if result.Error != "" {
		t.Fatalf("plan update result=%#v", result)
	}
	if current := agent.Plan(); current == nil || current.NeedsClarification() || len(current.DeferredValidations) != 1 {
		t.Fatalf("current plan=%#v", current)
	}
	if _, err := agent.executor.Execute(context.Background(), "write_file", json.RawMessage(`{"path":"resumed.txt","content":"ok"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPlanScopeMismatchFailsClosed(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendPlan(validTestPlan(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: &Executor{Registry: mustTestRegistry(t), Scope: ExecutionScope{Root: other}}, RequirePlan: true, Session: session}); err == nil {
		t.Fatal("scope mismatch accepted")
	}
}

func TestActivePlanSnapshotSurvivesCompaction(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleUser, Content: "change it"}); err != nil {
		t.Fatal(err)
	}
	plan := validTestPlan(root)
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(Message{Role: RoleAssistant, Content: "planned"}); err != nil {
		t.Fatal(err)
	}
	events := session.EventsSnapshot()
	through := events[len(events)-1].Seq
	if err := session.Compact(through, "older work compacted"); err != nil {
		t.Fatal(err)
	}
	executor, err := NewCodingExecutor(ExecutionScope{Root: root}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: executor, RequirePlan: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range agent.Messages() {
		if message.Role == RoleSystem && strings.HasPrefix(message.Content, planStateMessagePrefix) && strings.Contains(message.Content, plan.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("active plan snapshot missing after compaction: %#v", agent.Messages())
	}
}

func TestEquivalentPlanIsReusedAndReplacementRequiresReset(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(SessionMetadata{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	plan := validTestPlan(root)
	if err := session.AppendPlan(plan); err != nil {
		t.Fatal(err)
	}
	executor, err := NewCodingExecutor(ExecutionScope{Root: root}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgentWithOptions(DefaultConfig(), &fakeProvider{}, AgentOptions{Executor: executor, RequirePlan: true, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	recomposed := cloneTaskPlan(plan)
	recomposed.ID = "recomposed-plan-id"
	for index := range recomposed.Criteria {
		recomposed.Criteria[index].ID = fmt.Sprintf("recomposed-criterion-%d", index)
	}
	for taskIndex := range recomposed.Tasks {
		recomposed.Tasks[taskIndex].ID = fmt.Sprintf("recomposed-task-%d", taskIndex)
		for itemIndex := range recomposed.Tasks[taskIndex].Checklist {
			recomposed.Tasks[taskIndex].Checklist[itemIndex].ID = fmt.Sprintf("recomposed-item-%d-%d", taskIndex, itemIndex)
		}
	}
	args, _ := json.Marshal(recomposed)
	before := len(session.EventsSnapshot())
	result := agent.executePlanTool(context.Background(), PlanToolName, args)
	if result.Error != "" || !strings.Contains(result.Output, "reused") || len(session.EventsSnapshot()) != before {
		t.Fatalf("equivalent result=%#v events=%d want=%d", result, len(session.EventsSnapshot()), before)
	}
	replacement := plan
	replacement.ID = "replacement"
	replacement.Goal = "different task"
	args, _ = json.Marshal(replacement)
	result = agent.executePlanTool(context.Background(), PlanToolName, args)
	if !strings.Contains(result.Error, "/plan reset") {
		t.Fatalf("replacement result=%#v", result)
	}
}

func mustTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry(ToolFunc{ToolDefinition: ToolDefinition{Name: "read", Mutability: ToolMutabilityReadOnly}, Fn: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
