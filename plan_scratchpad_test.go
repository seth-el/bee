package bee

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTrustedExecutorAllowsScratchWritesBeforePlanButNotOtherMutation(t *testing.T) {
	_, store, _, ctx := newScratchFixture(t)
	other := ToolFunc{ToolDefinition: ToolDefinition{Name: "ordinary_mutation", Description: "ordinary mutation", Risk: RiskMedium, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, Fn: func(context.Context, json.RawMessage) (string, error) { return `{}`, nil }}
	registry, err := NewRegistry(ScratchSetTool{Store: store}, ScratchDeleteTool{Store: store}, other)
	if err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Registry: registry, Scope: ExecutionScope{Root: t.TempDir()}, Policy: GeneralAssistantPolicy{Mode: AssistantPolicyTrusted}, planGate: &PlanGate{}}
	if _, err := executor.Execute(ctx, "scratch_set", json.RawMessage(`{"key":"draft","value":"before plan"}`)); err != nil {
		t.Fatalf("scratch before plan: %v", err)
	}
	if _, err := executor.Execute(ctx, "ordinary_mutation", json.RawMessage(`{}`)); err == nil {
		t.Fatal("ordinary mutation bypassed plan gate")
	}
}
