package bee

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commandArtifactTestStore(t *testing.T) (*SessionStore, *ArtifactStore, SessionID) {
	t.Helper()
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "command artifacts"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	return sessions, store, session.ID
}

func TestExecRetentionCreatesSessionBoundArtifactAndReadPages(t *testing.T) {
	sessions, store, sessionID := commandArtifactTestStore(t)
	root := t.TempDir()
	scope := ExecutionScope{Root: root, TrustedForeground: true, MaxOutputBytes: 1024}
	var emitted Artifact
	ctx := withArtifactRunEmitter(withArtifactContext(context.Background(), sessionID, "run-exec"), func(artifact Artifact) { emitted = artifact })
	tool := ExecTool{Scope: scope, ArtifactStore: store}
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"printf 'OUT-'; printf 'ERR-' >&2; yes X | head -c 4000"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.Continuation == nil || result.Continuation.Status != "available" || result.Continuation.ArtifactID == "" {
		t.Fatalf("result=%#v", result)
	}
	if strings.Contains(result.Output, store.Root()) {
		t.Fatalf("result disclosed artifact path: %q", result.Output)
	}
	pageJSON, err := (ArtifactReadTool{Store: store}).Execute(ctx, []byte(`{"artifact_id":"`+result.Continuation.ArtifactID+`","stream":"stdout","offset":0,"limit":128}`))
	if err != nil {
		t.Fatal(err)
	}
	var page artifactReadResult
	if err := json.Unmarshal([]byte(pageJSON), &page); err != nil {
		t.Fatal(err)
	}
	if page.ReturnedBytes != 128 || page.NextOffset != 128 || page.EOF || page.StreamRetainedBytes <= int64(page.ReturnedBytes) || !page.OriginalTruncated || page.Encoding != "base64" || !strings.HasPrefix(page.Text, "OUT-") {
		t.Fatalf("page=%#v", page)
	}
	decoded, err := base64.StdEncoding.DecodeString(page.DataBase64)
	if err != nil || len(decoded) != 128 {
		t.Fatalf("decoded page len=%d err=%v", len(decoded), err)
	}
	var rebuilt []byte
	offset := int64(0)
	for {
		pageJSON, readErr := (ArtifactReadTool{Store: store}).Execute(ctx, []byte(`{"artifact_id":"`+result.Continuation.ArtifactID+`","stream":"stdout","offset":`+fmt.Sprint(offset)+`,"limit":333}`))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var next artifactReadResult
		if err := json.Unmarshal([]byte(pageJSON), &next); err != nil {
			t.Fatal(err)
		}
		chunk, err := base64.StdEncoding.DecodeString(next.DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, chunk...)
		if next.EOF {
			if next.NextOffset != next.StreamRetainedBytes {
				t.Fatalf("final page=%#v", next)
			}
			break
		}
		if next.NextOffset <= offset {
			t.Fatalf("page did not advance: %#v", next)
		}
		offset = next.NextOffset
	}
	if int64(len(rebuilt)) != result.Continuation.StdoutRetainedBytes || !strings.HasPrefix(string(rebuilt), "OUT-") {
		t.Fatalf("rebuilt stdout bytes=%d continuation=%#v", len(rebuilt), result.Continuation)
	}
	stderrJSON, err := (ArtifactReadTool{Store: store}).Execute(ctx, []byte(`{"artifact_id":"`+result.Continuation.ArtifactID+`","stream":"stderr","offset":0,"limit":128}`))
	if err != nil {
		t.Fatal(err)
	}
	var stderrPage artifactReadResult
	if err := json.Unmarshal([]byte(stderrJSON), &stderrPage); err != nil || stderrPage.Text != "ERR-" || !stderrPage.EOF {
		t.Fatalf("stderr page=%#v err=%v", stderrPage, err)
	}
	artifact, err := store.Get(result.Continuation.ArtifactID)
	if err != nil || artifact.SessionID != sessionID || artifact.RunID != "run-exec" || artifact.Purpose != commandArtifactPurpose {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	if emitted.ID != artifact.ID {
		t.Fatalf("emitted=%#v artifact=%#v", emitted, artifact)
	}
	loaded, err := sessions.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	foundEvent := false
	for _, event := range loaded.EventsSnapshot() {
		if event.Type != EventArtifact {
			continue
		}
		payload, decodeErr := decodeEventPayload[ArtifactEvent](event, EventArtifact)
		if decodeErr == nil && payload.Artifact.ID == artifact.ID {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatal("retained artifact event was not recorded")
	}
}

func TestRunCommandRetentionPreservesStreamsAndNonUTF8(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	scope := ExecutionScope{Root: t.TempDir(), MaxOutputBytes: 1024}
	ctx := withArtifactContext(context.Background(), sessionID, "run-command")
	tool := RunCommandTool{Scope: scope, ArtifactStore: store}
	output, err := tool.Execute(ctx, json.RawMessage(`{"argv":["sh","-c","printf 'OUT-\\377\\376'; printf 'ERR-MARK' >&2; i=0; while [ $i -lt 4000 ]; do printf Y; i=$((i+1)); done"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope commandOutputEnvelope
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Truncated || envelope.Continuation == nil || envelope.Continuation.Status != "available" {
		t.Fatalf("envelope=%#v", envelope)
	}
	artifact, err := store.Get(envelope.Continuation.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	_, file, err := store.OpenPayload(artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var payload commandArtifactPayload
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	stdout, err := base64.StdEncoding.DecodeString(payload.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := base64.StdEncoding.DecodeString(payload.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdout), "OUT-") || !strings.Contains(string(stderr), "ERR-MARK") || !payload.OriginalTruncated || payload.StdoutRetainedBytes != int64(len(stdout)) || payload.StderrRetainedBytes != int64(len(stderr)) {
		t.Fatalf("payload stdout=%q stderr=%q payload=%#v", stdout[:minCommandInt(len(stdout), 32)], stderr[:minCommandInt(len(stderr), 32)], payload)
	}
}

func TestCommandRetentionReportsHardCap(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	capture := newCommandCapture(1024, true)
	if _, err := capture.stdout.Write([]byte(strings.Repeat("c", int(MaxCommandArtifactOutputBytes)+1024))); err != nil {
		t.Fatal(err)
	}
	continuation := retainCommandOutput(withArtifactContext(context.Background(), sessionID, "run-cap"), store, &capture)
	if continuation == nil || continuation.Status != "available" || !continuation.RetainedCapped || continuation.RetainedOutputBytes > MaxCommandArtifactOutputBytes {
		t.Fatalf("continuation=%#v retained=%d", continuation, continuation.RetainedOutputBytes)
	}
	artifact, err := store.Get(continuation.ArtifactID)
	if err != nil || !artifact.RetentionCapped || !artifact.OriginalTruncated {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
}

func TestArtifactReadEnforcesSessionAndBounds(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	otherSessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := otherSessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	capture := newCommandCapture(1024, true)
	_, _ = capture.stdout.Write([]byte(strings.Repeat("x", 2000)))
	artifact, err := store.Create(context.Background(), sessionID, "run", ArtifactInput{Filename: commandArtifactFilename, MediaType: "application/json", Content: mustCommandPayload(t, &capture), Purpose: commandArtifactPurpose, OriginalSize: capture.originalBytes(), RetainedSize: capture.retainedBytes(), OriginalTruncated: true})
	if err != nil {
		t.Fatal(err)
	}
	tool := ArtifactReadTool{Store: store}
	if _, err := tool.Execute(withArtifactContext(context.Background(), other.ID, "other-run"), []byte(`{"artifact_id":"`+artifact.ID+`","stream":"stdout","offset":0,"limit":1}`)); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("cross-session read err=%v", err)
	}
	if _, err := tool.Execute(withArtifactContext(context.Background(), sessionID, "run"), []byte(`{"artifact_id":"`+artifact.ID+`","stream":"stdout","offset":0,"limit":65537}`)); err == nil {
		t.Fatal("oversized page accepted")
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"artifact_id":"`+artifact.ID+`","stream":"stdout","offset":0,"limit":1}`)); err == nil || !strings.Contains(err.Error(), "durable session") {
		t.Fatalf("missing context err=%v", err)
	}
}

func TestCommandRetentionFailureDoesNotEraseBoundedResult(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	brokenStore := *store
	brokenStore.store = nil
	tool := ExecTool{Scope: ExecutionScope{Root: t.TempDir(), TrustedForeground: true, MaxOutputBytes: 1024}, ArtifactStore: &brokenStore}
	ctx := withArtifactContext(context.Background(), sessionID, "run-failure")
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"yes Z | head -c 4000"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.Continuation == nil || result.Continuation.Status != "unavailable" || result.Output == "" {
		t.Fatalf("retention failure result=%#v", result)
	}
	if strings.Contains(result.Continuation.NextAction, store.Root()) {
		t.Fatal("retention diagnostic disclosed path")
	}
}

func TestCommandRetentionHandlesTimeoutAndCompositionBoundaries(t *testing.T) {
	sessions, store, sessionID := commandArtifactTestStore(t)
	tool := ExecTool{Scope: ExecutionScope{Root: t.TempDir(), TrustedForeground: true, MaxOutputBytes: 1024}, ArtifactStore: store}
	ctx, cancel := context.WithTimeout(withArtifactContext(context.Background(), sessionID, "run-timeout"), 50*time.Millisecond)
	defer cancel()
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"yes T | head -c 4000; sleep 1"}`))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Continuation == nil || result.Continuation.Status != "available" {
		t.Fatalf("timeout retention=%#v", result)
	}
	app, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: sessions, ArtifactStore: store, ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Executor.Registry.Lookup(ArtifactReadToolName); !ok {
		t.Fatal("ordinary composition omitted artifact_read")
	}
	execValue, ok := app.Executor.Registry.Lookup(ExecToolName)
	execTool, execOK := execValue.(ExecTool)
	if !ok || !execOK || execTool.ArtifactStore != store {
		t.Fatalf("trusted exec artifact wiring=%#v", execValue)
	}
	locked, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mustAssistantStore(t), ScopeRoot: t.TempDir(), Policy: AssistantPolicyLocked})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Executor != nil {
		t.Fatal("locked composition unexpectedly has executor")
	}
	child, err := copyChildExecutor(app.Executor, ChildTask{TaskID: "artifact-child", ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{ArtifactReadToolName}, Scope: ExecutionScope{Root: app.Executor.Scope.Root}, Budget: ChildBudget{}})
	if err == nil || child != nil || !strings.Contains(err.Error(), "parent-owned") {
		t.Fatalf("child artifact_read projection err=%v child=%#v", err, child)
	}
}

func TestArtifactReadRejectsCorruptionDeletionAndArgumentEcho(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	ctx := withArtifactContext(context.Background(), sessionID, "run-corrupt")
	makeArtifact := func(runID string) Artifact {
		capture := newCommandCapture(16, true)
		_, _ = capture.stdout.Write([]byte(strings.Repeat("payload", 20)))
		artifact, err := store.Create(context.Background(), sessionID, runID, ArtifactInput{Filename: commandArtifactFilename, MediaType: "application/json", Content: mustCommandPayload(t, &capture), Purpose: commandArtifactPurpose, OriginalSize: capture.originalBytes(), RetainedSize: capture.retainedBytes(), OriginalTruncated: true, RetentionCapped: capture.retentionCapped()})
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	corrupt := makeArtifact("run-corrupt")
	data, err := os.ReadFile(store.payloadPath(corrupt.ID))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(store.payloadPath(corrupt.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := ArtifactReadTool{Store: store}
	if _, err := tool.Execute(ctx, []byte(`{"artifact_id":"`+corrupt.ID+`","stream":"stdout","limit":16}`)); err == nil || err.Error() != "artifact is unavailable" {
		t.Fatalf("corrupt artifact err=%v", err)
	}
	deleted := makeArtifact("run-deleted")
	if err := os.Remove(store.payloadPath(deleted.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, []byte(`{"artifact_id":"`+deleted.ID+`","stream":"stdout","limit":16}`)); err == nil || err.Error() != "artifact is unavailable" {
		t.Fatalf("deleted artifact err=%v", err)
	}
	if _, err := tool.Execute(ctx, []byte(`{"password=do-not-echo":true}`)); err == nil || err.Error() != "invalid artifact_read arguments" || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("argument error=%v", err)
	}
}

func TestArtifactReadNonUTF8UsesBase64WithoutText(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	capture := newCommandCapture(1, true)
	_, _ = capture.stdout.Write([]byte{0xff, 0xfe, 'A'})
	artifact, err := store.Create(context.Background(), sessionID, "run-binary", ArtifactInput{Filename: commandArtifactFilename, MediaType: "application/json", Content: mustCommandPayload(t, &capture), Purpose: commandArtifactPurpose, OriginalSize: capture.originalBytes(), RetainedSize: capture.retainedBytes(), OriginalTruncated: true, RetentionCapped: capture.retentionCapped()})
	if err != nil {
		t.Fatal(err)
	}
	output, err := (ArtifactReadTool{Store: store}).Execute(withArtifactContext(context.Background(), sessionID, "reader"), []byte(`{"artifact_id":"`+artifact.ID+`","stream":"stdout","limit":16}`))
	if err != nil {
		t.Fatal(err)
	}
	var result artifactReadResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.DataBase64)
	if err != nil || string(decoded) != string([]byte{0xff, 0xfe, 'A'}) || result.Text != "" {
		t.Fatalf("result=%#v decoded=%v err=%v", result, decoded, err)
	}
}

func TestCommandRetentionCompositionAndChildFallback(t *testing.T) {
	sessions, store, sessionID := commandArtifactTestStore(t)
	root := t.TempDir()
	prompted, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: sessions, ArtifactStore: store, ScopeRoot: root, Policy: AssistantPolicyPrompted})
	if err != nil {
		t.Fatal(err)
	}
	value, ok := prompted.Executor.Registry.Lookup(ExecToolName)
	execTool, execOK := value.(ExecTool)
	if !ok || !execOK || execTool.ArtifactStore != store {
		t.Fatalf("prompted exec artifact wiring=%#v", value)
	}
	mismatchedSessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ComposeGeneralAssistant(DefaultConfig(), "", AssistantCompositionOptions{Store: mismatchedSessions, ArtifactStore: store, ScopeRoot: root}); err == nil || !strings.Contains(err.Error(), "configured session store") {
		t.Fatalf("mismatched artifact/session store err=%v", err)
	}
	baseScope := ExecutionScope{Root: root, MaxOutputBytes: 1024}
	registry, err := NewRegistry(RunCommandTool{Scope: baseScope, ArtifactStore: store})
	if err != nil {
		t.Fatal(err)
	}
	base := &Executor{Registry: registry, Scope: baseScope}
	child, err := copyChildExecutor(base, ChildTask{TaskID: "command-child", ParentID: "parent", Objective: "inspect", Prompt: "inspect", AllowedTools: []string{"run_command"}, Scope: baseScope, Budget: ChildBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	childValue, ok := child.Registry.Lookup("run_command")
	childTool, childOK := childValue.(RunCommandTool)
	if !ok || !childOK || childTool.ArtifactStore != nil {
		t.Fatalf("child run_command artifact wiring=%#v", childValue)
	}
	before, err := store.List(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	output, err := childTool.Execute(withArtifactContext(context.Background(), sessionID, "parent-context"), json.RawMessage(`{"argv":["sh","-c","i=0; while [ $i -lt 4000 ]; do printf C; i=$((i+1)); done"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "artifact_id") {
		t.Fatalf("child command retained parent artifact: %s", output)
	}
	after, err := store.List(sessionID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("child artifact count before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestCommandRetentionSkipsUntruncatedOutput(t *testing.T) {
	_, store, sessionID := commandArtifactTestStore(t)
	tool := ExecTool{Scope: ExecutionScope{Root: t.TempDir(), TrustedForeground: true, MaxOutputBytes: 1024}, ArtifactStore: store}
	output, err := tool.Execute(withArtifactContext(context.Background(), sessionID, "run-small"), json.RawMessage(`{"command":"printf ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result execResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.List(sessionID)
	if err != nil || result.Truncated || result.Continuation != nil || len(artifacts) != 0 {
		t.Fatalf("result=%#v artifacts=%#v err=%v", result, artifacts, err)
	}
}

func mustCommandPayload(t *testing.T, capture *commandCapture) []byte {
	t.Helper()
	payload, err := capture.artifactPayload()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func minCommandInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
