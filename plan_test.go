package bee

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanToolSchemaExposesTasksAndChecklist(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(planToolDefinition().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["tasks"]; !ok {
		t.Fatalf("schema missing tasks: %#v", properties)
	}
	tasks := properties["tasks"].(map[string]any)
	items := tasks["items"].(map[string]any)
	taskProperties := items["properties"].(map[string]any)
	if _, ok := taskProperties["checklist"]; !ok {
		t.Fatalf("task schema missing checklist: %#v", taskProperties)
	}
	if _, ok := properties["criteria"]; !ok {
		t.Fatalf("schema missing structured criteria: %#v", properties)
	}
	if _, ok := schema["anyOf"]; ok {
		t.Fatalf("schema contains a top-level union unsupported by some local tool parsers: %#v", schema)
	}
	required := make(map[string]bool)
	for _, value := range schema["required"].([]any) {
		required[value.(string)] = true
	}
	for _, name := range []string{"id", "goal", "inputs", "outputs", "steps", "acceptance"} {
		if !required[name] {
			t.Fatalf("schema does not require %q: %#v", name, schema["required"])
		}
	}
}

func TestTaskPlanValidation(t *testing.T) {
	root := t.TempDir()
	plan := TaskPlan{ID: "plan-1", Goal: "ship", Inputs: []string{"source"}, Outputs: []string{"artifact"}, Steps: []string{"inspect", "build"}, Acceptance: []string{"tests pass"}, Tasks: []ScopedTask{{ID: "task-1", Title: "build", Scope: ExecutionScope{Root: root}, Checklist: []ChecklistItem{{ID: "check-1", Text: "test"}}}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	plan.Tasks[0].Checklist = append(plan.Tasks[0].Checklist, ChecklistItem{ID: "check-1", Text: "duplicate"})
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate checklist") {
		t.Fatalf("err=%v", err)
	}
	plan.Tasks[0].Checklist = nil
	plan.Tasks = append(plan.Tasks, ScopedTask{ID: "task-1", Title: "duplicate", Scope: ExecutionScope{Root: filepath.Clean(root)}})
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate task") {
		t.Fatalf("err=%v", err)
	}
}
