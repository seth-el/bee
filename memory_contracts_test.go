package bee

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func testMemoryPolicy() MemoryPolicy { return DefaultMemoryPolicy() }

func testMemoryRecord(actor MemoryActor, sensitivity MemorySensitivity) MemoryRecord {
	at := time.Date(2026, 8, 16, 21, 0, 0, 0, time.UTC)
	return MemoryRecord{
		ID:          "memory-1",
		Namespace:   MemoryNamespaceDurableMemory,
		Key:         "preference.theme",
		SubjectID:   "user-1",
		Content:     "The user prefers concise technical answers.",
		Summary:     "Concise technical answers preferred.",
		Sensitivity: sensitivity,
		Tags:        map[string]string{"kind": "preference"},
		Source:      "user",
		CreatedAt:   at,
		UpdatedAt:   at,
		Confidence:  0.9,
		Version:     1,
		CreatedBy:   actor,
	}
}

func testMemoryParent() MemoryActor { return MemoryActor{Kind: MemoryActorParent, ID: "parent-1"} }
func testMemoryChild() MemoryActor  { return MemoryActor{Kind: MemoryActorChild, ID: "child-1"} }

func TestDefaultMemoryPolicyIsSafeAndBounded(t *testing.T) {
	policy := DefaultMemoryPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	if !policy.AllowExplicitWrites || policy.AllowSuggestedWrites || policy.AllowSensitiveMemory {
		t.Fatalf("unsafe write defaults: %#v", policy)
	}
	if policy.ChildAccess != MemoryChildReadOnly || policy.Retention != 0 || policy.MaxContextRecords <= 0 || policy.MaxContextBytes <= 0 {
		t.Fatalf("unsafe child/retention/context defaults: %#v", policy)
	}
	if policy.ChildMayWrite() || !policy.ChildMayRead() || !policy.ChildMayPropose() {
		t.Fatal("default child access is not read-only with proposal submission")
	}
}

func TestIdentityAndUserProfilesValidateSeparately(t *testing.T) {
	at := time.Date(2026, 8, 16, 21, 0, 0, 0, time.UTC)
	identity := IdentityProfile{ID: "assistant", Name: "Assistant", Mission: "Help carefully.", Values: []string{"honest"}, UpdatedAt: at}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	profile := UserProfile{ID: "user-1", DisplayName: "Alex", Preferences: map[string]string{"answer_style": "concise"}, UpdatedAt: at}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if MemoryNamespaceIdentity == MemoryNamespaceUserProfile || MemoryNamespaceIdentity == MemoryNamespaceDurableMemory || MemoryNamespaceSession == MemoryNamespaceDurableMemory {
		t.Fatal("memory boundaries must use distinct namespaces")
	}
}

func TestMemoryRecordValidationRejectsUnsafeFieldsAndSecretShapes(t *testing.T) {
	policy := testMemoryPolicy()
	cases := []struct {
		name string
		edit func(*MemoryRecord)
		want string
	}{
		{"unsafe namespace", func(record *MemoryRecord) { record.Namespace = "durable memory" }, "namespace"},
		{"non-durable namespace", func(record *MemoryRecord) { record.Namespace = MemoryNamespaceIdentity }, "outside durable"},
		{"unsafe key", func(record *MemoryRecord) { record.Key = "../token" }, "key"},
		{"secret content", func(record *MemoryRecord) { record.Content = "api_key=sk-test-secret" }, "secret"},
		{"too long content", func(record *MemoryRecord) { record.Content = strings.Repeat("x", MaxMemoryTextLength+1) }, "exceeds"},
		{"bad confidence", func(record *MemoryRecord) { record.Confidence = math.NaN() }, "confidence"},
		{"bad timestamps", func(record *MemoryRecord) { record.UpdatedAt = record.CreatedAt.Add(-time.Second) }, "updated_at"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
			test.edit(&record)
			err := record.Validate(policy)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSensitiveAndSecretMemoryPolicy(t *testing.T) {
	policy := testMemoryPolicy()
	if err := testMemoryRecord(testMemoryParent(), MemorySensitivitySensitive).Validate(policy); err == nil {
		t.Fatal("expected sensitive memory rejection")
	}
	secret := testMemoryRecord(testMemoryParent(), MemorySensitivitySecret)
	if err := secret.Validate(policy); err == nil {
		t.Fatal("expected secret memory rejection")
	}
	policy.AllowSensitiveMemory = true
	if err := testMemoryRecord(testMemoryParent(), MemorySensitivitySensitive).Validate(policy); err != nil {
		t.Fatal(err)
	}
	if err := testMemoryRecord(testMemoryParent(), MemorySensitivitySecret).Validate(policy); err == nil {
		t.Fatal("secret memory must remain rejected")
	}
}

func TestParentOwnedStoreEventsAndChildProposalBoundary(t *testing.T) {
	policy := testMemoryPolicy()
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	created := MemoryEvent{ID: "event-1", Type: MemoryEventCreated, At: record.CreatedAt, Actor: testMemoryParent(), Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: 1}
	if err := created.ValidateForStore(policy); err != nil {
		t.Fatal(err)
	}
	childEvent := created
	childEvent.ID = "event-child"
	childEvent.Actor = testMemoryChild()
	if err := childEvent.ValidateForStore(policy); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("child store event error = %v", err)
	}
	nonExplicit := created
	nonExplicit.ID = "event-implicit"
	nonExplicit.Explicit = false
	if err := nonExplicit.ValidateForStore(policy); err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("implicit store event error = %v", err)
	}
	proposal := MemoryProposal{ID: "proposal-1", Record: testMemoryRecord(testMemoryChild(), MemorySensitivityPersonal), ProposedBy: testMemoryChild(), Mode: MemoryWriteSuggested, Reason: "The user stated a stable preference.", AuditID: "audit-1", CreatedAt: record.CreatedAt}
	if err := proposal.Validate(policy); err != nil {
		t.Fatal(err)
	}
	proposal.ProposedBy = testMemoryParent()
	if err := proposal.Validate(policy); err == nil || !strings.Contains(err.Error(), "child") {
		t.Fatalf("parent proposal error = %v", err)
	}
	proposal.ProposedBy = testMemoryChild()
	proposal.Record.CreatedBy = testMemoryParent()
	if err := proposal.Validate(policy); err == nil || !strings.Contains(err.Error(), "creating actor") {
		t.Fatalf("parent-created proposal error = %v", err)
	}
}

func TestMemoryEventConflictAndVersionSemantics(t *testing.T) {
	policy := testMemoryPolicy()
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	updated := MemoryEvent{ID: "event-2", Type: MemoryEventUpdated, At: record.UpdatedAt, Actor: testMemoryParent(), Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, ExpectedVersion: 1, NewVersion: 2}
	if err := updated.ValidateForStore(policy); err != nil {
		t.Fatal(err)
	}
	updated.NewVersion = 1
	if err := updated.Validate(policy); err == nil || !strings.Contains(err.Error(), "increasing") {
		t.Fatalf("version error = %v", err)
	}
	conflict := MemoryEvent{ID: "event-conflict", Type: MemoryEventConflict, At: record.UpdatedAt, Actor: testMemoryParent(), RecordID: record.ID, ConflictWith: "event-foreign", Explicit: true, Reason: "expected version did not match"}
	if err := conflict.ValidateForStore(policy); err != nil {
		t.Fatal(err)
	}
	conflict.ConflictWith = "bad id/"
	if err := conflict.Validate(policy); err == nil {
		t.Fatal("expected unsafe conflict id rejection")
	}
}

func TestMemoryQueryAndContextBounds(t *testing.T) {
	policy := testMemoryPolicy()
	query := MemoryQuery{Actor: testMemoryChild(), Namespace: MemoryNamespaceDurableMemory, Text: "preference", Limit: 2, MaxBytes: 1024}
	if err := query.Validate(policy); err != nil {
		t.Fatal(err)
	}
	query.IncludeSensitive = true
	if err := query.Validate(policy); err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("sensitive child query error = %v", err)
	}
	query.IncludeSensitive = false
	query.Namespace = MemoryNamespaceIdentity
	if err := query.Validate(policy); err == nil || !strings.Contains(err.Error(), "outside durable") {
		t.Fatalf("non-durable query namespace error = %v", err)
	}
	item := MemoryContextItem{RecordID: "memory-1", Namespace: MemoryNamespaceDurableMemory, Key: "preference.theme", Content: "concise", Sensitivity: MemorySensitivityPersonal, Score: 0.8}
	packet := MemoryContextPacket{SessionID: "session-1", Items: []MemoryContextItem{item}, UsedBytes: 64}
	if err := packet.Validate(policy, testMemoryChild()); err != nil {
		t.Fatal(err)
	}
	packet.Items = make([]MemoryContextItem, policy.MaxContextRecords+1)
	if err := packet.Validate(policy, testMemoryChild()); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("context bound error = %v", err)
	}
}

func TestMemoryJSONRoundTripIsDeterministic(t *testing.T) {
	policy := testMemoryPolicy()
	record := testMemoryRecord(testMemoryParent(), MemorySensitivityPersonal)
	if err := record.Validate(policy); err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("canonical JSON changed:\n%s\n%s", first, second)
	}
	var decoded MemoryRecord
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(policy); err != nil {
		t.Fatal(err)
	}
	third, err := CanonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(third) {
		t.Fatalf("round-trip JSON changed:\n%s\n%s", first, third)
	}
}

type memoryContractFakeStore struct{}

func (memoryContractFakeStore) Retrieve(context.Context, MemoryQuery) (MemoryRetrievalResult, error) {
	return MemoryRetrievalResult{}, nil
}
func (memoryContractFakeStore) Append(context.Context, MemoryEvent) error { return nil }

var _ MemoryStore = memoryContractFakeStore{}
var _ MemoryRetriever = memoryContractFakeStore{}
