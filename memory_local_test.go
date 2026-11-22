package bee

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func localMemoryPolicy() MemoryPolicy {
	policy := DefaultMemoryPolicy()
	policy.Retention = 365 * 24 * time.Hour
	policy.MaxRecords = 32
	policy.MaxBytes = 1 << 20
	policy.MaxQueryRecords = 32
	policy.MaxQueryBytes = 1 << 20
	return policy
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func localEvent(id string, record MemoryRecord) MemoryEvent {
	return MemoryEvent{ID: id, Type: MemoryEventCreated, At: record.CreatedAt, Actor: testMemoryParent(), Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: record.Version}
}

func localUpdate(id string, record MemoryRecord, expected, next uint64) MemoryEvent {
	record.Version = next
	record.UpdatedAt = record.CreatedAt.Add(time.Minute * time.Duration(next))
	return MemoryEvent{ID: id, Type: MemoryEventUpdated, At: record.UpdatedAt, Actor: testMemoryParent(), Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, ExpectedVersion: expected, NewVersion: next}
}

func TestLocalMemoryStoreRestartPersistenceDeletionAndConflict(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	if err := store.Append(context.Background(), localEvent("event-create", record)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), localUpdate("event-update", record, 1, 2)); err != nil {
		t.Fatal(err)
	}
	bad := localUpdate("event-conflict", record, 1, 3)
	if err := store.Append(context.Background(), bad); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	store, err = NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryParent(), Key: "preference.theme"})
	if err != nil || len(result.Records) != 1 || result.Records[0].Version != 2 {
		t.Fatalf("retrieved = %#v, err = %v", result, err)
	}
	deleteEvent := MemoryEvent{ID: "event-delete", Type: MemoryEventDeleted, At: time.Now().UTC(), Actor: testMemoryParent(), RecordID: record.ID, Explicit: true, ExpectedVersion: 2}
	if err := store.Append(context.Background(), deleteEvent); err != nil {
		t.Fatal(err)
	}
	result, err = store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryParent(), Key: "preference.theme"})
	if err != nil || len(result.Records) != 0 {
		t.Fatalf("deleted retrieval = %#v, err = %v", result, err)
	}
	diagnostics, err := store.ReplayDiagnostics()
	if err != nil || diagnostics.Tombstones != 1 || diagnostics.Events != 3 {
		t.Fatalf("diagnostics = %#v, err = %v", diagnostics, err)
	}
}

func TestLocalMemoryStoreRejectsMalformedTruncatedAndOversizedLogs(t *testing.T) {
	policy := localMemoryPolicy()
	cases := []struct {
		name string
		data []byte
	}{
		{name: "malformed", data: []byte("{not-json}\n")},
		{name: "truncated", data: []byte(`{"id":"event"}`)},
		{name: "oversized", data: []byte(strings.Repeat("x", MaxMemoryBytes+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := privateTempDir(t)
			if err := os.WriteFile(filepath.Join(root, memoryEventsFile), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewLocalMemoryStore(root, policy); err == nil || !errors.Is(err, ErrMemoryCorrupt) {
				t.Fatalf("error = %v, want corruption", err)
			}
		})
	}
}

func TestLocalMemoryProposalSinkIsolationAndParentAcceptance(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewLocalMemoryProposalSink(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	childRecord := testMemoryRecord(testMemoryChild(), MemorySensitivityPersonal)
	proposal := MemoryProposal{ID: "proposal-1", Record: childRecord, ProposedBy: testMemoryChild(), Mode: MemoryWriteSuggested, Reason: "user stated a preference", AuditID: "audit-1", CreatedAt: childRecord.CreatedAt}
	if err := sink.SubmitProposal(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	if err := sink.SubmitProposal(context.Background(), proposal); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("duplicate proposal error = %v", err)
	}
	result, err := store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryParent(), Text: "concise"})
	if err != nil || len(result.Records) != 0 {
		t.Fatalf("proposal mutated durable records: %#v, err=%v", result, err)
	}
	if err := store.Append(context.Background(), localEvent("event-accepted", testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, memoryProposalsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, memoryEventsFile)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalMemoryExactRetrievalBoundsAndSensitiveAccess(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	policy.AllowSensitiveMemory = true
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	first := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	first.ID, first.Key = "memory-a", "preference.concise"
	second := first
	second.ID, second.Key, second.Content, second.Summary = "memory-b", "preference.verbose", "Verbose answers are requested.", "Verbose answers requested."
	sensitive := first
	sensitive.ID, sensitive.Key, sensitive.Sensitivity = "memory-c", "preference.secretish", MemorySensitivitySensitive
	for _, event := range []MemoryEvent{localEvent("event-a", first), localEvent("event-b", second), localEvent("event-c", sensitive)} {
		if err := store.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryChild(), Text: "concise", Limit: 1})
	if err != nil || len(result.Records) != 1 || result.Records[0].ID != "memory-a" {
		t.Fatalf("exact retrieval = %#v, err=%v", result, err)
	}
	if result.Truncated {
		t.Fatalf("single exact result should not be truncated: %#v", result)
	}
	result, err = store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryChild(), Namespace: MemoryNamespaceDurableMemory, Limit: 10})
	if err != nil || len(result.Records) != 2 {
		t.Fatalf("child sensitive filtering = %#v, err=%v", result, err)
	}
	result, err = store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryParent(), IncludeSensitive: true, Namespace: MemoryNamespaceDurableMemory, Limit: 10})
	if err != nil || len(result.Records) != 3 {
		t.Fatalf("parent sensitive retrieval = %#v, err=%v", result, err)
	}
	packet, err := store.RetrieveContext(context.Background(), MemoryQuery{Actor: testMemoryChild(), Text: "concise"}, "session-1")
	if err != nil || len(packet.Items) != 1 || packet.SessionID != "session-1" {
		t.Fatalf("context = %#v, err=%v", packet, err)
	}
}

func TestLocalIdentityAndUserProfileSnapshotsAreSeparateAndPrivate(t *testing.T) {
	root := privateTempDir(t)
	identity, err := NewLocalIdentityStore(root)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := NewLocalUserProfileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 16, 21, 0, 0, 0, time.UTC)
	if err := identity.Save(context.Background(), IdentityProfile{ID: "assistant", Name: "Assistant", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := profiles.Save(context.Background(), UserProfile{ID: "user-1", DisplayName: "Alex", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	loadedIdentity, err := identity.Load(context.Background())
	if err != nil || loadedIdentity.Revision != 1 {
		t.Fatalf("identity = %#v, err=%v", loadedIdentity, err)
	}
	loadedProfile, err := profiles.Load(context.Background(), "user-1")
	if err != nil || loadedProfile.Revision != 1 {
		t.Fatalf("profile = %#v, err=%v", loadedProfile, err)
	}
	loadedProfile.DisplayName = "Updated"
	loadedProfile.Revision = 0
	if err := profiles.Save(context.Background(), loadedProfile); err != nil {
		t.Fatal(err)
	}
	loadedProfile, err = profiles.Load(context.Background(), "user-1")
	if err != nil || loadedProfile.DisplayName != "Updated" || loadedProfile.Revision != 2 {
		t.Fatalf("replacement profile = %#v, err=%v", loadedProfile, err)
	}
	if _, err := os.Stat(filepath.Join(root, memoryEventsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile writes must not create event log, err=%v", err)
	}
	for _, path := range []string{filepath.Join(root, memoryIdentityFile), filepath.Join(root, memoryProfilesDir, "user-1.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o", path, info.Mode().Perm())
		}
	}
}

func TestLocalMemoryWriterLockContentionAndExplicitRelease(t *testing.T) {
	root := privateTempDir(t)
	store, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.AcquireWriterLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewLocalMemoryStore(root, localMemoryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.AcquireWriterLock(context.Background()); !errors.Is(err, ErrMemoryLockHeld) {
		t.Fatalf("contention error = %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	lock, err = other.AcquireWriterLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, memoryWriterLockFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireWriterLock(context.Background()); !errors.Is(err, ErrMemoryLockHeld) {
		t.Fatalf("ambiguous stale lock error = %v", err)
	}
}

func TestLocalMemoryRejectsUnsafeRootsAndMetadata(t *testing.T) {
	if _, err := NewLocalMemoryStore("", localMemoryPolicy()); err == nil {
		t.Fatal("empty root accepted")
	}
	repoRoot := privateTempDir(t)
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalMemoryStore(filepath.Join(repoRoot, "memory"), localMemoryPolicy()); err == nil {
		t.Fatal("repository-overlapping root accepted")
	}
	symlinkParent := privateTempDir(t)
	target := privateTempDir(t)
	link := filepath.Join(symlinkParent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewLocalMemoryStore(filepath.Join(link, "memory"), localMemoryPolicy()); err == nil {
		t.Fatal("symlinked root accepted")
	}
	unsafe := privateTempDir(t)
	if err := os.Chmod(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalMemoryStore(unsafe, localMemoryPolicy()); err == nil {
		t.Fatal("world-readable root accepted")
	}
}

func TestLocalMemoryAppendHonorsConfiguredByteLimit(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	policy.MaxBytes = 1
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), localEvent("event-too-large", testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal))); err == nil {
		t.Fatal("oversized configured event accepted")
	}
}

func TestLocalMemoryEventJSONShapeIsValidatedOnReplay(t *testing.T) {
	root := privateTempDir(t)
	policy := localMemoryPolicy()
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	event := localEvent("event-1", record)
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, memoryEventsFile), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Retrieve(context.Background(), MemoryQuery{Actor: testMemoryParent(), Key: record.Key})
	if err != nil || len(result.Records) != 1 {
		t.Fatalf("result = %#v, err=%v", result, err)
	}
}
