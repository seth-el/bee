package bee

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recordingApproval struct {
	mu       sync.Mutex
	decision ApprovalDecision
	requests []ApprovalRequest
}

func (approval *recordingApproval) Approve(_ context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	approval.mu.Lock()
	defer approval.mu.Unlock()
	approval.requests = append(approval.requests, request)
	return approval.decision, nil
}

func (approval *recordingApproval) count() int {
	approval.mu.Lock()
	defer approval.mu.Unlock()
	return len(approval.requests)
}

type grantFixture struct {
	home, workspace, project string
	app                      *Application
	approval                 *recordingApproval
}

func newGrantFixture(t *testing.T, decision ApprovalDecision) grantFixture {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	workspace := filepath.Join(home, ".bee", "workspace")
	project := filepath.Join(home, "projects", "demo")
	for _, dir := range []string{workspace, filepath.Join(project, ".git"), filepath.Join(project, "src")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("BEE_HOME", filepath.Join(home, ".bee"))
	approval := &recordingApproval{decision: decision}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), MemoryRoot: filepath.Join(workspace, "memory"), Policy: AssistantPolicyTrusted, ApprovalMode: ApprovalModeManual, Approval: approval})
	if err != nil {
		t.Fatal(err)
	}
	return grantFixture{home: home, workspace: workspace, project: project, app: app, approval: approval}
}

func grantContext(store *SessionWriteGrants, id SessionID) context.Context {
	return withSessionWriteGrants(withArtifactContext(context.Background(), id, "run"), store)
}

func writeFileArgs(path string) json.RawMessage {
	return json.RawMessage(`{"path":` + strconvQuote(path) + `,"content":"x","operation":"create"}`)
}

// "Approve for this session" on an out-of-bounds write grants the enclosing
// git work tree, after which further writes there no longer ask, for the
// file tools and the exec sandbox alike, in that session only.
func TestApproveForSessionGrantsTheProjectForThatSessionOnly(t *testing.T) {
	fixture := newGrantFixture(t, ApprovalApprovedForSession)
	store := NewSessionWriteGrants()
	sessionA := grantContext(store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := fixture.app.Executor.Execute(sessionA, "write_file", writeFileArgs(filepath.Join(fixture.project, "src", "a.go"))); err != nil {
		t.Fatal(err)
	}
	if fixture.approval.count() != 1 || fixture.approval.requests[0].SessionGrant != fixture.project {
		t.Fatalf("approval requests=%#v, want one offering %q", fixture.approval.requests, fixture.project)
	}
	if _, err := fixture.app.Executor.Execute(sessionA, "write_file", writeFileArgs(filepath.Join(fixture.project, "b.go"))); err != nil {
		t.Fatal(err)
	}
	if fixture.approval.count() != 1 {
		t.Fatalf("granted write asked again: %d requests", fixture.approval.count())
	}
	// Another session has no grant.
	sessionB := grantContext(store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := fixture.app.Executor.Execute(sessionB, "write_file", writeFileArgs(filepath.Join(fixture.project, "c.go"))); err != nil {
		t.Fatal(err)
	}
	if fixture.approval.count() != 2 {
		t.Fatalf("grant leaked into another session: %d requests", fixture.approval.count())
	}
	if requireBubblewrapOrSkip(t) {
		target := filepath.Join(fixture.project, "from-exec.txt")
		result, err := fixture.app.Executor.Execute(sessionA, ExecToolName, json.RawMessage(`{"command":"printf ok > `+shellSingleQuote(target)+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if data, readErr := os.ReadFile(target); readErr != nil || string(data) != "ok" {
			t.Fatalf("exec could not write the granted project: %q %v (%s)", data, readErr, result.Output)
		}
	}
}

func TestApproveOnceRecordsNoGrant(t *testing.T) {
	fixture := newGrantFixture(t, ApprovalApproved)
	store := NewSessionWriteGrants()
	ctx := grantContext(store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for _, name := range []string{"a.go", "b.go"} {
		if _, err := fixture.app.Executor.Execute(ctx, "write_file", writeFileArgs(filepath.Join(fixture.project, name))); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.approval.count() != 2 || len(store.Roots("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")) != 0 {
		t.Fatalf("approve-once recorded a grant: requests=%d roots=%v", fixture.approval.count(), store.Roots("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	}
}

func TestSessionGrantIsNotOfferedForBroadDirsHostExecOrWithoutSession(t *testing.T) {
	fixture := newGrantFixture(t, ApprovalApprovedForSession)
	store := NewSessionWriteGrants()
	ctx := grantContext(store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	// Directly in home (no enclosing project): granting would expose all of home.
	if _, err := fixture.app.Executor.Execute(ctx, "write_file", writeFileArgs(filepath.Join(fixture.home, "loose.txt"))); err != nil {
		t.Fatal(err)
	}
	if got := fixture.approval.requests[len(fixture.approval.requests)-1].SessionGrant; got != "" {
		t.Fatalf("offered a grant for the home directory: %q", got)
	}
	if roots := store.Roots("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); len(roots) != 0 {
		t.Fatalf("approved_session without an offer recorded %v", roots)
	}
	// host_exec stays approve-every-time.
	_, _ = fixture.app.Executor.Execute(ctx, HostExecToolName, json.RawMessage(`{"command":"true"}`))
	for _, request := range fixture.approval.requests {
		if request.ToolName == HostExecToolName && request.SessionGrant != "" {
			t.Fatalf("host_exec offered a session grant: %#v", request)
		}
	}
	// No session in context: nothing to remember it in.
	before := fixture.approval.count()
	if _, err := fixture.app.Executor.Execute(context.Background(), "write_file", writeFileArgs(filepath.Join(fixture.project, "n.go"))); err != nil {
		t.Fatal(err)
	}
	if fixture.approval.count() != before+1 || fixture.approval.requests[before].SessionGrant != "" {
		t.Fatalf("session grant offered without a session: %#v", fixture.approval.requests[before:])
	}
}

func TestSessionWriteGrantsAreBoundedAndValidated(t *testing.T) {
	store := NewSessionWriteGrants()
	id := SessionID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if store.Add(id, "relative/path") || store.Add("", "/abs") || store.Add("../bad", "/abs") {
		t.Fatal("invalid grant accepted")
	}
	for index := 0; index < MaxSessionWriteGrants; index++ {
		if !store.Add(id, filepath.Join("/grants", strings.Repeat("d", index+1))) {
			t.Fatalf("grant %d refused", index)
		}
	}
	if store.Add(id, "/grants/one-too-many") {
		t.Fatal("grant bound not enforced")
	}
	if !store.Add(id, "/grants/d/nested") {
		t.Fatal("a directory already covered by a grant should be a no-op success")
	}
}

func TestOwnerDecisionValidationAcceptsSessionApproval(t *testing.T) {
	for decision, valid := range map[ApprovalDecision]bool{ApprovalApproved: true, ApprovalDenied: true, ApprovalApprovedForSession: true, "approve": false, "": false} {
		if decision.ValidOwnerDecision() != valid {
			t.Fatalf("%q valid=%v", decision, decision.ValidOwnerDecision())
		}
	}
	if !ApprovalApprovedForSession.Approves() || ApprovalDenied.Approves() {
		t.Fatal("Approves() mismatch")
	}
	broker := NewApprovalBroker()
	request := ApprovalRequest{ID: "approval-session-test", ToolName: "write_file"}
	done := make(chan ApprovalDecision, 1)
	go func() {
		decision, _ := broker.Approve(context.Background(), request)
		done <- decision
	}()
	for {
		if err := broker.Decide(request.ID, ApprovalApprovedForSession); err == nil {
			break
		}
	}
	if got := <-done; got != ApprovalApprovedForSession {
		t.Fatalf("broker decision=%q", got)
	}
}

func TestExecReportsSandboxWriteHint(t *testing.T) {
	if !requireBubblewrapOrSkip(t) {
		return
	}
	root := sandboxTmpTestRoot(t)
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{workspace, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scope := ExecutionScope{Root: root, WriteRoot: workspace, EnforceWriteRoots: true, TrustedForeground: true}
	output, err := ExecTool{Scope: scope}.Execute(context.Background(), mustJSON(map[string]any{"command": "printf x > " + shellSingleQuote(filepath.Join(outside, "f"))}))
	if err != nil {
		t.Fatal(err)
	}
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.SandboxHint, workspace) || !strings.Contains(result.SandboxHint, "write_file") || !strings.Contains(result.SandboxHint, "--write-root") {
		t.Fatalf("hint=%q output=%q", result.SandboxHint, result.Output)
	}
	ok, err := ExecTool{Scope: scope}.Execute(context.Background(), mustJSON(map[string]any{"command": "printf fine"}))
	if err != nil || strings.Contains(ok, "sandbox_hint") {
		t.Fatalf("successful command got a hint: %s %v", ok, err)
	}
}

func requireBubblewrapOrSkip(t *testing.T) bool {
	t.Helper()
	if insideBeeProcessSandbox() {
		t.Log("inside a Bee sandbox; skipping the Bubblewrap part")
		return false
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Log("bubblewrap unavailable; skipping the Bubblewrap part")
		return false
	}
	return true
}

func TestTUISessionKeyOnlyWhenAGrantIsOffered(t *testing.T) {
	app := &Application{approvalBroker: NewApprovalBroker()}
	without := &fullscreenTUI{app: app, pendingApproval: &ApprovalEvent{ID: "approval-x", Request: ApprovalRequest{ToolName: "write_file"}}}
	if _, _, _, err := without.handleKey(context.Background(), tuiKey{kind: tuiKeyRune, rune: 's'}, nil); err != nil {
		t.Fatalf("s without an offered grant should be ignored: %v", err)
	}
	if without.pendingApproval == nil || strings.Contains(without.notice, "this session") {
		t.Fatalf("pending=%v notice=%q", without.pendingApproval, without.notice)
	}
	with := &fullscreenTUI{app: app, pendingApproval: &ApprovalEvent{ID: "approval-y", Request: ApprovalRequest{ToolName: "write_file", SessionGrant: "/home/u/projects/demo"}}}
	with.handleKey(context.Background(), tuiKey{kind: tuiKeyRune, rune: 'x'}, nil)
	if !strings.Contains(with.notice, "s allow") || !strings.Contains(with.notice, "demo") {
		t.Fatalf("notice does not offer the session key: %q", with.notice)
	}
	// With a grant offered, s is a decision: it reaches the broker (which
	// reports the request as not pending in this isolated test).
	if _, _, _, err := with.handleKey(context.Background(), tuiKey{kind: tuiKeyRune, rune: 's'}, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("s with an offered grant did not decide: %v", err)
	}
}
