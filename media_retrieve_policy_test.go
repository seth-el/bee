package bee

import "testing"

func TestMediaRetrieveUsesNarrowPlanGateExceptionOnly(t *testing.T) {
	gate := &PlanGate{}
	if err := gate.Check(MediaRetrieveTool{}.Definition()); err != nil {
		t.Fatalf("media_retrieve was blocked without a coding plan: %v", err)
	}
	if err := gate.Check(WriteFileTool{}.Definition()); err != ErrPlanRequired {
		t.Fatalf("write_file plan gate changed: %v", err)
	}
	if err := gate.Check(ScratchSetTool{}.Definition()); err != nil {
		t.Fatalf("scratch_set should be usable before a plan: %v", err)
	}
	if err := gate.Check(ScratchDeleteTool{}.Definition()); err != nil {
		t.Fatalf("scratch_delete should be usable before a plan: %v", err)
	}
	if err := gate.Check(MemoryRememberTool{}.Definition()); err != ErrPlanRequired {
		t.Fatalf("memory_remember plan gate changed: %v", err)
	}
}
