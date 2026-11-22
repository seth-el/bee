package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	MaxMemoryIDLength          = 128
	MaxMemoryNamespaceLength   = 128
	MaxMemoryKeyLength         = 128
	MaxMemoryTextLength        = 64 << 10
	MaxMemorySummaryLength     = 8 << 10
	MaxMemorySourceLength      = 256
	MaxMemoryTagCount          = 32
	MaxMemoryTagValueLength    = 512
	MaxMemoryListLength        = 256
	MaxMemoryRecords           = 100_000
	MaxMemoryBytes             = 128 << 20
	MaxMemoryContextRecords    = 1024
	MaxMemoryContextBytes      = 4 << 20
	MaxMemoryQueryRecords      = 10_000
	MaxMemoryQueryBytes        = 4 << 20
	MaxMemoryProfileListLength = 64
)

// MemorySensitivity classifies durable memory before it is written or exposed
// to a child. Secrets are rejected unconditionally and are never a valid
// durable-memory value.
type MemoryStatus string

const (
	MemoryStatusActive     MemoryStatus = "active"
	MemoryStatusHistorical MemoryStatus = "historical"
	MemoryStatusSuperseded MemoryStatus = "superseded"
)

func (status MemoryStatus) valid() bool {
	return status == "" || status == MemoryStatusActive || status == MemoryStatusHistorical || status == MemoryStatusSuperseded
}

func (status MemoryStatus) effective() MemoryStatus {
	if status == "" {
		return MemoryStatusActive
	}
	return status
}

type MemorySensitivity string

const (
	MemorySensitivityUnspecified MemorySensitivity = ""
	MemorySensitivityPublic      MemorySensitivity = "public"
	MemorySensitivityPersonal    MemorySensitivity = "personal"
	MemorySensitivitySensitive   MemorySensitivity = "sensitive"
	MemorySensitivitySecret      MemorySensitivity = "secret"

	SensitivityPublic    = MemorySensitivityPublic
	SensitivityPersonal  = MemorySensitivityPersonal
	SensitivitySensitive = MemorySensitivitySensitive
	SensitivitySecret    = MemorySensitivitySecret

	MemorySensitivityPrivate = MemorySensitivityPersonal
)

func (sensitivity MemorySensitivity) valid() bool {
	switch sensitivity {
	case MemorySensitivityPublic, MemorySensitivityPersonal, MemorySensitivitySensitive, MemorySensitivitySecret:
		return true
	default:
		return false
	}
}

// MemoryNamespace prevents identity, profile, durable-memory, and transient
// session data from sharing one catch-all storage bucket.
type MemoryNamespace string

const (
	MemoryNamespaceIdentity      MemoryNamespace = "identity"
	MemoryNamespaceUserProfile   MemoryNamespace = "user_profile"
	MemoryNamespaceDurableMemory MemoryNamespace = "durable_memory"
	MemoryNamespaceSession       MemoryNamespace = "session"

	NamespaceIdentity      = MemoryNamespaceIdentity
	NamespaceUserProfile   = MemoryNamespaceUserProfile
	NamespaceDurableMemory = MemoryNamespaceDurableMemory
	NamespaceSession       = MemoryNamespaceSession
)

func (namespace MemoryNamespace) valid() bool {
	return validateNamespace(string(namespace)) == nil
}

// MemoryActorKind identifies who may request or authorize a memory action.
type MemoryActorKind string

type MemoryActorType = MemoryActorKind

const (
	MemoryActorParent MemoryActorKind = "parent"
	MemoryActorChild  MemoryActorKind = "child"
	MemoryActorUser   MemoryActorKind = "user"
	MemoryActorSystem MemoryActorKind = "system"

	ActorParent = MemoryActorParent
	ActorChild  = MemoryActorChild
	ActorUser   = MemoryActorUser
	ActorSystem = MemoryActorSystem

	MemoryActorTypeParent = MemoryActorParent
	MemoryActorTypeChild  = MemoryActorChild
	MemoryActorTypeUser   = MemoryActorUser
	MemoryActorTypeSystem = MemoryActorSystem
)

// MemoryActor is part of every auditable query, proposal, and event.
type MemoryActor struct {
	Kind MemoryActorKind `json:"kind"`
	ID   string          `json:"id"`
}

func (actor MemoryActor) Validate() error {
	switch actor.Kind {
	case MemoryActorParent, MemoryActorChild, MemoryActorUser, MemoryActorSystem:
	default:
		return fmt.Errorf("memory actor kind %q is invalid", actor.Kind)
	}
	return validateSafeID("memory actor id", actor.ID, MaxMemoryIDLength)
}

// MemoryChildAccess is deliberately narrower than a general permission enum.
type MemoryChildAccess string

const (
	MemoryChildNoAccess     MemoryChildAccess = "none"
	MemoryChildReadOnly     MemoryChildAccess = "read_only"
	MemoryChildProposalOnly MemoryChildAccess = "proposal_only"
)

func (access MemoryChildAccess) valid() bool {
	return access == MemoryChildNoAccess || access == MemoryChildReadOnly || access == MemoryChildProposalOnly
}

// MemoryWriteMode distinguishes an explicit user/parent decision from a
// suggestion that still requires policy approval before durable persistence.
type MemoryWriteMode string

const (
	MemoryWriteExplicit  MemoryWriteMode = "explicit"
	MemoryWriteSuggested MemoryWriteMode = "suggested"

	WriteExplicit  = MemoryWriteExplicit
	WriteSuggested = MemoryWriteSuggested
)

// MemoryEventType identifies append-only durable-memory audit events.
type MemoryEventType string

const (
	MemoryEventCreated           MemoryEventType = "created"
	MemoryEventUpdated           MemoryEventType = "updated"
	MemoryEventDeleted           MemoryEventType = "deleted"
	MemoryEventProposalSubmitted MemoryEventType = "proposal_submitted"
	MemoryEventConflict          MemoryEventType = "conflict"
	MemoryEventImported          MemoryEventType = "imported"

	MemoryEventCreate = MemoryEventCreated
	MemoryEventUpdate = MemoryEventUpdated
	MemoryEventDelete = MemoryEventDeleted

	MemoryEventTypeCreated           = MemoryEventCreated
	MemoryEventTypeUpdated           = MemoryEventUpdated
	MemoryEventTypeDeleted           = MemoryEventDeleted
	MemoryEventTypeProposalSubmitted = MemoryEventProposalSubmitted
	MemoryEventTypeConflict          = MemoryEventConflict
	MemoryEventTypeImported          = MemoryEventImported
)

func (eventType MemoryEventType) valid() bool {
	switch eventType {
	case MemoryEventCreated, MemoryEventUpdated, MemoryEventDeleted, MemoryEventProposalSubmitted, MemoryEventConflict, MemoryEventImported:
		return true
	default:
		return false
	}
}

// IdentityProfile is the assistant's stable self-description. It is intentionally not a
// memory record and cannot be used as a user profile or session context.
type IdentityProfile struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Mission    string    `json:"mission,omitempty"`
	Values     []string  `json:"values,omitempty"`
	Boundaries []string  `json:"boundaries,omitempty"`
	Voice      string    `json:"voice,omitempty"`
	Revision   uint64    `json:"revision"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (profile IdentityProfile) Validate() error {
	if err := validateSafeID("identity profile id", profile.ID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateText("identity profile name", profile.Name, MaxMemoryKeyLength, true); err != nil {
		return err
	}
	if err := validateText("identity profile mission", profile.Mission, MaxMemoryTextLength, false); err != nil {
		return err
	}
	if err := validateTextList("identity profile values", profile.Values, MaxMemoryProfileListLength, MaxMemorySummaryLength); err != nil {
		return err
	}
	if err := validateTextList("identity profile boundaries", profile.Boundaries, MaxMemoryProfileListLength, MaxMemorySummaryLength); err != nil {
		return err
	}
	if err := validateText("identity profile voice", profile.Voice, MaxMemorySummaryLength, false); err != nil {
		return err
	}
	return validateTimestamp("identity profile updated_at", profile.UpdatedAt, true)
}

// UserProfile stores explicit user preferences and profile attributes. It is
// separate from both durable memories and the transient session context.
type UserProfile struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name,omitempty"`
	Locale      string            `json:"locale,omitempty"`
	TimeZone    string            `json:"time_zone,omitempty"`
	Preferences map[string]string `json:"preferences,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
	Revision    uint64            `json:"revision"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

func (profile UserProfile) Validate() error {
	if err := validateSafeID("user profile id", profile.ID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateText("user profile display_name", profile.DisplayName, MaxMemoryKeyLength, false); err != nil {
		return err
	}
	if err := validateText("user profile locale", profile.Locale, MaxMemoryKeyLength, false); err != nil {
		return err
	}
	if err := validateText("user profile time_zone", profile.TimeZone, MaxMemoryKeyLength, false); err != nil {
		return err
	}
	if err := validateStringMap("user profile preferences", profile.Preferences, MaxMemoryTagCount, MaxMemoryKeyLength, MaxMemoryTagValueLength); err != nil {
		return err
	}
	if err := validateTextList("user profile notes", profile.Notes, MaxMemoryProfileListLength, MaxMemorySummaryLength); err != nil {
		return err
	}
	return validateTimestamp("user profile updated_at", profile.UpdatedAt, true)
}

// MemoryProvenance identifies the exact reviewed source behind an imported
// memory record. It is bounded evidence, not prompt text or a secret store.
type MemoryProvenance struct {
	InventoryID   string   `json:"inventory_id"`
	CandidateID   string   `json:"candidate_id"`
	DecisionID    string   `json:"decision_id"`
	RootLabel     string   `json:"root_label"`
	RelativePath  string   `json:"relative_path"`
	FileSHA256    string   `json:"file_sha256"`
	ContentSHA256 string   `json:"content_sha256"`
	LineStart     int      `json:"line_start"`
	LineEnd       int      `json:"line_end"`
	HeadingPath   []string `json:"heading_path,omitempty"`
}

func (provenance MemoryProvenance) Validate() error {
	if provenance.InventoryID == "" || provenance.CandidateID == "" || provenance.DecisionID == "" || provenance.RootLabel == "" || provenance.RelativePath == "" || provenance.FileSHA256 == "" || provenance.ContentSHA256 == "" {
		return errors.New("memory provenance is incomplete")
	}
	if provenance.LineStart < 1 || provenance.LineEnd < provenance.LineStart {
		return errors.New("memory provenance line range is invalid")
	}
	if len(provenance.HeadingPath) > 32 {
		return errors.New("memory provenance heading path is too deep")
	}
	for _, heading := range provenance.HeadingPath {
		if err := validateText("memory provenance heading", heading, MaxMemorySummaryLength, true); err != nil {
			return err
		}
	}
	return nil
}

// MemoryRecord is one durable, bounded, user-approved memory candidate or
// stored record. It has no session messages, identity instructions, or model
// prompt fields.
type MemoryRecord struct {
	ID           string            `json:"id"`
	Namespace    MemoryNamespace   `json:"namespace"`
	Key          string            `json:"key"`
	SubjectID    string            `json:"subject_id,omitempty"`
	Content      string            `json:"content"`
	Summary      string            `json:"summary,omitempty"`
	Sensitivity  MemorySensitivity `json:"sensitivity"`
	Tags         map[string]string `json:"tags,omitempty"`
	Source       string            `json:"source,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	ExpiresAt    time.Time         `json:"expires_at,omitempty"`
	Confidence   float64           `json:"confidence"`
	Version      uint64            `json:"version"`
	CreatedBy    MemoryActor       `json:"created_by"`
	Status       MemoryStatus      `json:"status,omitempty"`
	SupersededBy string            `json:"superseded_by,omitempty"`
	Provenance   *MemoryProvenance `json:"provenance,omitempty"`
}

func (record MemoryRecord) Validate(policy MemoryPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if err := validateSafeID("memory record id", record.ID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateDurableNamespace(string(record.Namespace)); err != nil {
		return err
	}
	if err := validateSafeKey("memory record key", record.Key, MaxMemoryKeyLength); err != nil {
		return err
	}
	if record.SubjectID != "" {
		if err := validateSafeID("memory record subject_id", record.SubjectID, MaxMemoryIDLength); err != nil {
			return err
		}
	}
	if err := validateText("memory record content", record.Content, MaxMemoryTextLength, true); err != nil {
		return err
	}
	if err := validateText("memory record summary", record.Summary, MaxMemorySummaryLength, false); err != nil {
		return err
	}
	if !record.Sensitivity.valid() {
		return fmt.Errorf("memory record sensitivity %q is invalid", record.Sensitivity)
	}
	if err := policy.allowsSensitivity(record.Sensitivity); err != nil {
		return err
	}
	if err := validateStringMap("memory record tags", record.Tags, MaxMemoryTagCount, MaxMemoryKeyLength, MaxMemoryTagValueLength); err != nil {
		return err
	}
	if err := validateText("memory record source", record.Source, MaxMemorySourceLength, false); err != nil {
		return err
	}
	if err := validateTimestamp("memory record created_at", record.CreatedAt, true); err != nil {
		return err
	}
	if err := validateTimestamp("memory record updated_at", record.UpdatedAt, true); err != nil {
		return err
	}
	if record.UpdatedAt.Before(record.CreatedAt) {
		return errors.New("memory record updated_at must not precede created_at")
	}
	if !record.ExpiresAt.IsZero() && record.ExpiresAt.Before(record.UpdatedAt) {
		return errors.New("memory record expires_at must not precede updated_at")
	}
	if err := validateConfidence(record.Confidence); err != nil {
		return err
	}
	if record.Version == 0 {
		return errors.New("memory record version must be positive")
	}
	if !record.Status.valid() {
		return fmt.Errorf("memory record status %q is invalid", record.Status)
	}
	if record.SupersededBy != "" {
		if err := validateSafeID("memory record superseded_by", record.SupersededBy, MaxMemoryIDLength); err != nil {
			return err
		}
	}
	if record.Status.effective() == MemoryStatusSuperseded && record.SupersededBy == "" {
		return errors.New("superseded memory record requires superseded_by")
	}
	if err := record.CreatedBy.Validate(); err != nil {
		return fmt.Errorf("memory record created_by: %w", err)
	}
	if record.Provenance != nil {
		if err := record.Provenance.Validate(); err != nil {
			return fmt.Errorf("memory record provenance: %w", err)
		}
	}
	return nil
}

// MemoryProposal is the only durable-memory write-shaped value a child may
// produce. A parent must validate and explicitly accept it before creating a
// MemoryEvent for a store.
type MemoryProposal struct {
	ID         string          `json:"id"`
	Record     MemoryRecord    `json:"record"`
	ProposedBy MemoryActor     `json:"proposed_by"`
	Mode       MemoryWriteMode `json:"mode"`
	Reason     string          `json:"reason"`
	AuditID    string          `json:"audit_id"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (proposal MemoryProposal) Validate(policy MemoryPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if !policy.ChildMayPropose() {
		return errors.New("child memory proposals are disabled by policy")
	}
	if err := validateSafeID("memory proposal id", proposal.ID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := proposal.ProposedBy.Validate(); err != nil {
		return fmt.Errorf("memory proposal proposed_by: %w", err)
	}
	if proposal.ProposedBy.Kind != MemoryActorChild {
		return errors.New("only a child actor may submit a memory proposal")
	}
	if proposal.Mode != MemoryWriteExplicit && proposal.Mode != MemoryWriteSuggested {
		return fmt.Errorf("memory proposal mode %q is invalid", proposal.Mode)
	}
	if err := validateText("memory proposal reason", proposal.Reason, MaxMemorySummaryLength, true); err != nil {
		return err
	}
	if err := validateSafeID("memory proposal audit_id", proposal.AuditID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateTimestamp("memory proposal created_at", proposal.CreatedAt, true); err != nil {
		return err
	}
	if proposal.Record.CreatedBy.Kind != MemoryActorChild {
		return errors.New("child memory proposals must identify the child as the creating actor")
	}
	if err := proposal.Record.Validate(policy); err != nil {
		return fmt.Errorf("memory proposal record: %w", err)
	}
	return nil
}

// MemoryEvent is an append-only audit record. Store implementations should
// accept only ValidateForStore events; proposal-submitted events belong to a
// proposal sink and are not durable record mutations.
type MemoryEvent struct {
	ID              string          `json:"id"`
	Type            MemoryEventType `json:"type"`
	At              time.Time       `json:"at"`
	Actor           MemoryActor     `json:"actor"`
	Record          MemoryRecord    `json:"record,omitempty"`
	RecordID        string          `json:"record_id,omitempty"`
	Proposal        MemoryProposal  `json:"proposal,omitempty"`
	ProposalID      string          `json:"proposal_id,omitempty"`
	Mode            MemoryWriteMode `json:"mode,omitempty"`
	Explicit        bool            `json:"explicit"`
	ExpectedVersion uint64          `json:"expected_version,omitempty"`
	NewVersion      uint64          `json:"new_version,omitempty"`
	ConflictWith    string          `json:"conflict_with,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

func (event MemoryEvent) Validate(policy MemoryPolicy) error {
	if err := validateSafeID("memory event id", event.ID, MaxMemoryIDLength); err != nil {
		return err
	}
	if !event.Type.valid() {
		return fmt.Errorf("memory event type %q is invalid", event.Type)
	}
	if err := validateTimestamp("memory event at", event.At, true); err != nil {
		return err
	}
	if err := event.Actor.Validate(); err != nil {
		return fmt.Errorf("memory event actor: %w", err)
	}
	switch event.Type {
	case MemoryEventCreated, MemoryEventImported:
		if event.RecordID != "" && event.RecordID != event.Record.ID {
			return errors.New("memory created/imported event record_id does not match record id")
		}
		if err := event.Record.Validate(policy); err != nil {
			return fmt.Errorf("memory created/imported event record: %w", err)
		}
		if event.RecordID == "" {
			event.RecordID = event.Record.ID
		}
		if event.Type == MemoryEventCreated && event.Record.Version != 1 {
			return errors.New("created memory record version must be 1")
		}
		if event.Type == MemoryEventImported && event.Record.Version == 0 {
			return errors.New("imported memory record version must be positive")
		}
		if event.NewVersion != 0 && event.NewVersion != event.Record.Version {
			return errors.New("memory created/imported event new_version does not match record version")
		}
	case MemoryEventUpdated:
		if event.RecordID == "" {
			return errors.New("memory updated event record_id is required")
		}
		if event.RecordID != event.Record.ID {
			return errors.New("memory updated event record_id does not match record id")
		}
		if event.ExpectedVersion == 0 || event.NewVersion == 0 || event.NewVersion <= event.ExpectedVersion {
			return errors.New("memory updated event requires increasing expected_version and new_version")
		}
		if err := event.Record.Validate(policy); err != nil {
			return fmt.Errorf("memory updated event record: %w", err)
		}
	case MemoryEventDeleted:
		if err := validateSafeID("memory deleted event record_id", event.RecordID, MaxMemoryIDLength); err != nil {
			return err
		}
		if event.ExpectedVersion == 0 {
			return errors.New("memory deleted event expected_version is required")
		}
	case MemoryEventProposalSubmitted:
		if event.Actor.Kind != MemoryActorChild {
			return errors.New("proposal-submitted events require a child actor")
		}
		if err := event.Proposal.Validate(policy); err != nil {
			return fmt.Errorf("memory proposal event: %w", err)
		}
		if event.ProposalID != "" && event.ProposalID != event.Proposal.ID {
			return errors.New("memory proposal event proposal_id does not match proposal id")
		}
	case MemoryEventConflict:
		if err := validateSafeID("memory conflict event record_id", event.RecordID, MaxMemoryIDLength); err != nil {
			return err
		}
		if err := validateSafeID("memory conflict event conflict_with", event.ConflictWith, MaxMemoryIDLength); err != nil {
			return err
		}
		if err := validateText("memory conflict event reason", event.Reason, MaxMemorySummaryLength, true); err != nil {
			return err
		}
	}
	if event.Mode != "" && event.Mode != MemoryWriteExplicit && event.Mode != MemoryWriteSuggested {
		return fmt.Errorf("memory event mode %q is invalid", event.Mode)
	}
	if event.Reason != "" {
		if err := validateText("memory event reason", event.Reason, MaxMemorySummaryLength, false); err != nil {
			return err
		}
	}
	return nil
}

// ValidateForStore is the enforceable parent-owned write boundary. A child
// event can be validated as an observation/proposal, but cannot pass this
// method for a durable store mutation.
func (event MemoryEvent) ValidateForStore(policy MemoryPolicy) error {
	if err := event.Validate(policy); err != nil {
		return err
	}
	if event.Type == MemoryEventProposalSubmitted {
		return errors.New("proposal-submitted events must go to a proposal sink, not a memory store")
	}
	if event.Actor.Kind != MemoryActorParent {
		return errors.New("durable memory store events require the parent actor")
	}
	if !event.Explicit {
		return errors.New("durable memory store events must be explicit")
	}
	if event.Mode != "" && event.Mode != MemoryWriteExplicit {
		return errors.New("durable memory store events must use explicit write mode")
	}
	if event.Type == MemoryEventCreated || event.Type == MemoryEventUpdated || event.Type == MemoryEventImported {
		if event.Record.CreatedBy.Kind != MemoryActorParent {
			return errors.New("durable records must identify the parent as their creating actor")
		}
		if err := policy.AllowsWrite(event.Actor, MemoryWriteExplicit, event.Record.Sensitivity); err != nil {
			return err
		}
	}
	if event.Type == MemoryEventDeleted || event.Type == MemoryEventConflict {
		if err := policy.AllowsWrite(event.Actor, MemoryWriteExplicit, MemorySensitivityPublic); err != nil {
			return err
		}
	}
	return nil
}

// MemoryQuery is a bounded, actor-scoped read request. It is not a session
// context and does not carry identity instructions or arbitrary prompt text.
type MemoryQuery struct {
	Actor            MemoryActor     `json:"actor"`
	Namespace        MemoryNamespace `json:"namespace,omitempty"`
	Key              string          `json:"key,omitempty"`
	SubjectID        string          `json:"subject_id,omitempty"`
	Text             string          `json:"text,omitempty"`
	IncludeSensitive bool            `json:"include_sensitive,omitempty"`
	Limit            int             `json:"limit,omitempty"`
	MaxBytes         int             `json:"max_bytes,omitempty"`
	AsOf             time.Time       `json:"as_of,omitempty"`
	Since            time.Time       `json:"since,omitempty"`
	Before           time.Time       `json:"before,omitempty"`
	CurrentOnly      bool            `json:"current_only,omitempty"`
}

func (query MemoryQuery) Validate(policy MemoryPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if err := query.Actor.Validate(); err != nil {
		return fmt.Errorf("memory query actor: %w", err)
	}
	if query.Actor.Kind == MemoryActorChild && !policy.ChildMayRead() {
		return errors.New("child memory reads are disabled by policy")
	}
	if query.Namespace != "" {
		if err := validateDurableNamespace(string(query.Namespace)); err != nil {
			return err
		}
	}
	if query.Key != "" {
		if err := validateSafeKey("memory query key", query.Key, MaxMemoryKeyLength); err != nil {
			return err
		}
	}
	if query.SubjectID != "" {
		if err := validateSafeID("memory query subject_id", query.SubjectID, MaxMemoryIDLength); err != nil {
			return err
		}
	}
	if err := validateText("memory query text", query.Text, MaxMemorySummaryLength, false); err != nil {
		return err
	}
	if query.Namespace == "" && query.Key == "" && strings.TrimSpace(query.Text) == "" {
		return errors.New("memory query needs a namespace, key, or text")
	}
	if !query.AsOf.IsZero() && !query.Since.IsZero() && query.Since.After(query.AsOf) {
		return errors.New("memory query since must not be after as_of")
	}
	if !query.Before.IsZero() && !query.Since.IsZero() && query.Since.After(query.Before) {
		return errors.New("memory query since must not be after before")
	}
	if !query.Before.IsZero() && !query.AsOf.IsZero() && query.AsOf.After(query.Before) {
		return errors.New("memory query as_of must not be after before")
	}
	if query.IncludeSensitive {
		if query.Actor.Kind == MemoryActorChild {
			return errors.New("child queries may not request sensitive memory")
		}
		if !policy.AllowSensitiveMemory {
			return errors.New("sensitive memory is disabled by policy")
		}
	}
	if query.Limit < 0 || query.Limit > policy.MaxQueryRecords {
		return fmt.Errorf("memory query limit must be between 0 and %d", policy.MaxQueryRecords)
	}
	if query.MaxBytes < 0 || query.MaxBytes > policy.MaxQueryBytes {
		return fmt.Errorf("memory query max_bytes must be between 0 and %d", policy.MaxQueryBytes)
	}
	return validateTimestamp("memory query as_of", query.AsOf, false)
}

// MemoryRetrievalResult is a bounded provider-neutral result, not a retrieval
// implementation. Records remain durable-memory records until projected into
// a separate MemoryContextPacket.
type MemoryRetrievalResult struct {
	Records   []MemoryRecord `json:"records,omitempty"`
	Total     int            `json:"total"`
	Bytes     int            `json:"bytes"`
	Truncated bool           `json:"truncated,omitempty"`
}

func (result MemoryRetrievalResult) Validate(policy MemoryPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if result.Total < 0 || result.Total < len(result.Records) {
		return errors.New("memory retrieval total is inconsistent")
	}
	if len(result.Records) > policy.MaxRecords {
		return fmt.Errorf("memory retrieval exceeds %d records", policy.MaxRecords)
	}
	if result.Bytes < 0 || result.Bytes > policy.MaxBytes {
		return fmt.Errorf("memory retrieval bytes must be between 0 and %d", policy.MaxBytes)
	}
	actualBytes := 0
	for index, record := range result.Records {
		if err := record.Validate(policy); err != nil {
			return fmt.Errorf("memory retrieval record %d: %w", index, err)
		}
		recordBytes, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("memory retrieval record %d: encode: %w", index, err)
		}
		actualBytes += len(recordBytes)
	}
	if actualBytes > policy.MaxBytes {
		return fmt.Errorf("memory retrieval records exceed %d bytes", policy.MaxBytes)
	}
	return nil
}

// MemoryContextItem is a bounded transient projection of one durable record.
// It intentionally contains no write authority and no session event history.
type MemoryContextItem struct {
	RecordID    string            `json:"record_id"`
	Namespace   MemoryNamespace   `json:"namespace"`
	Key         string            `json:"key"`
	Content     string            `json:"content"`
	Summary     string            `json:"summary,omitempty"`
	Sensitivity MemorySensitivity `json:"sensitivity"`
	Score       float64           `json:"score,omitempty"`
	// Updated is the record's last-updated date (UTC, YYYY-MM-DD) and Status
	// its lifecycle status when not active. Both are shown to the model so it
	// can tell an old fact from a current one.
	Updated string       `json:"updated,omitempty"`
	Status  MemoryStatus `json:"status,omitempty"`
}

// memoryContextProvenance returns the date and non-active status shown next to
// a recalled record.
func memoryContextProvenance(record MemoryRecord) (updated string, status MemoryStatus) {
	at := record.UpdatedAt
	if at.IsZero() {
		at = record.CreatedAt
	}
	if !at.IsZero() {
		updated = at.UTC().Format("2006-01-02")
	}
	if effective := record.Status.effective(); effective != MemoryStatusActive {
		status = effective
	}
	return updated, status
}

func (item MemoryContextItem) Validate(policy MemoryPolicy, actor MemoryActor) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("memory context actor: %w", err)
	}
	if err := validateSafeID("memory context record_id", item.RecordID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateDurableNamespace(string(item.Namespace)); err != nil {
		return err
	}
	if err := validateSafeKey("memory context key", item.Key, MaxMemoryKeyLength); err != nil {
		return err
	}
	if err := validateText("memory context content", item.Content, MaxMemoryTextLength, true); err != nil {
		return err
	}
	if err := validateText("memory context summary", item.Summary, MaxMemorySummaryLength, false); err != nil {
		return err
	}
	if !item.Sensitivity.valid() {
		return fmt.Errorf("memory context sensitivity %q is invalid", item.Sensitivity)
	}
	if item.Updated != "" {
		if _, err := time.Parse("2006-01-02", item.Updated); err != nil {
			return errors.New("memory context updated date is invalid")
		}
	}
	if !item.Status.valid() {
		return fmt.Errorf("memory context status %q is invalid", item.Status)
	}
	if err := policy.allowsSensitivity(item.Sensitivity); err != nil {
		return err
	}
	if actor.Kind == MemoryActorChild && item.Sensitivity == MemorySensitivitySensitive {
		return errors.New("child context may not contain sensitive memory")
	}
	if err := validateConfidence(item.Score); err != nil {
		return fmt.Errorf("memory context score: %w", err)
	}
	return nil
}

// MemoryContextPacket is a bounded transient projection for one request. It
// must not be persisted as a durable memory record or used as a write request.
type MemoryContextPacket struct {
	SessionID string              `json:"session_id"`
	Items     []MemoryContextItem `json:"items,omitempty"`
	UsedBytes int                 `json:"used_bytes"`
	Truncated bool                `json:"truncated,omitempty"`
}

func (packet MemoryContextPacket) Validate(policy MemoryPolicy, actor MemoryActor) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("memory policy: %w", err)
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind == MemoryActorChild && !policy.ChildMayRead() {
		return errors.New("child memory context is disabled by policy")
	}
	if packet.SessionID != "" {
		if err := validateSafeID("memory context session_id", packet.SessionID, MaxMemoryIDLength); err != nil {
			return err
		}
	}
	if len(packet.Items) > policy.MaxContextRecords {
		return fmt.Errorf("memory context exceeds %d records", policy.MaxContextRecords)
	}
	if packet.UsedBytes < 0 || packet.UsedBytes > policy.MaxContextBytes {
		return fmt.Errorf("memory context used_bytes must be between 0 and %d", policy.MaxContextBytes)
	}
	actualBytes := 0
	for index, item := range packet.Items {
		if err := item.Validate(policy, actor); err != nil {
			return fmt.Errorf("memory context item %d: %w", index, err)
		}
		itemBytes, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("memory context item %d: encode: %w", index, err)
		}
		actualBytes += len(itemBytes)
	}
	if actualBytes > policy.MaxContextBytes {
		return fmt.Errorf("memory context items exceed %d bytes", policy.MaxContextBytes)
	}
	return nil
}

// MemoryRetriever is the read-only retrieval boundary. It must honor the
// actor and bounds in MemoryQuery and return a validated result.
type MemoryRetriever interface {
	Retrieve(context.Context, MemoryQuery) (MemoryRetrievalResult, error)
}

// MemoryStore is the parent-owned durable-memory boundary. Append must reject
// any event that fails MemoryEvent.ValidateForStore; children use the separate
// MemoryProposalSink instead.
type MemoryStore interface {
	MemoryRetriever
	Append(context.Context, MemoryEvent) error
}

// MemoryProposalSink is the only write-shaped boundary exposed to children.
type MemoryProposalSink interface {
	SubmitProposal(context.Context, MemoryProposal) error
}

// CanonicalJSON provides deterministic standard-library JSON bytes for public
// memory contracts. encoding/json sorts string map keys, so repeated calls are
// stable without a custom serializer or provider dependency.
func CanonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func validateSafeID(label, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", label, max)
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			if index == 0 && char == '.' {
				return fmt.Errorf("%s starts with an unsafe character", label)
			}
			continue
		}
		return fmt.Errorf("%s contains an unsafe character", label)
	}
	return nil
}

func validateSafeKey(label, value string, max int) error {
	return validateSafeID(label, value, max)
}

func validateNamespace(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("memory namespace is required")
	}
	if len(value) > MaxMemoryNamespaceLength {
		return fmt.Errorf("memory namespace exceeds %d bytes", MaxMemoryNamespaceLength)
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" {
			return errors.New("memory namespace contains an empty segment")
		}
		if err := validateSafeID("memory namespace", part, MaxMemoryNamespaceLength); err != nil {
			return err
		}
	}
	return nil
}

func validateDurableNamespace(value string) error {
	if err := validateNamespace(value); err != nil {
		return err
	}
	if value != string(MemoryNamespaceDurableMemory) && !strings.HasPrefix(value, string(MemoryNamespaceDurableMemory)+"/") {
		return fmt.Errorf("memory namespace %q is outside durable memory", value)
	}
	return nil
}

func validateText(label, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", label, max)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains a NUL byte", label)
	}
	if looksLikeSecret(value) {
		return fmt.Errorf("%s looks like credential or secret material", label)
	}
	return nil
}

func validateTextList(label string, values []string, maxCount, maxValue int) error {
	if len(values) > maxCount {
		return fmt.Errorf("%s exceeds %d entries", label, maxCount)
	}
	for index, value := range values {
		if err := validateText(fmt.Sprintf("%s[%d]", label, index), value, maxValue, true); err != nil {
			return err
		}
	}
	return nil
}

func validateStringMap(label string, values map[string]string, maxCount, maxKey, maxValue int) error {
	if len(values) > maxCount {
		return fmt.Errorf("%s exceeds %d entries", label, maxCount)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Iterate in sorted order so validation errors are deterministic even when
	// a caller supplied a map with multiple malformed entries.
	sort.Strings(keys)
	for _, key := range keys {
		if err := validateSafeKey(label+" key", key, maxKey); err != nil {
			return err
		}
		if err := validateText(label+"["+key+"]", values[key], maxValue, true); err != nil {
			return err
		}
	}
	return nil
}

func looksLikeSecret(value string) bool {
	lower := strings.ToLower(value)
	markers := []string{
		"-----begin ",
		"authorization: bearer ",
		"access_token=", "refresh_token=", "api_key=", "apikey=", "client_secret=", "password=", "private_key=",
		"sk-", "ghp_", "github_pat_", "xoxb-", "xoxp-", "aws_access_key_id=", "aws_secret_access_key=",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ValidateMemoryWriteRequest makes the parent-owned write rule convenient for
// callers without requiring them to construct an event first.
type MemoryWriteRequest struct {
	Actor    MemoryActor     `json:"actor"`
	Record   MemoryRecord    `json:"record"`
	Mode     MemoryWriteMode `json:"mode"`
	Explicit bool            `json:"explicit"`
	AuditID  string          `json:"audit_id"`
	Reason   string          `json:"reason"`
}

func (request MemoryWriteRequest) Validate(policy MemoryPolicy) error {
	if err := validateSafeID("memory write audit_id", request.AuditID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateText("memory write reason", request.Reason, MaxMemorySummaryLength, true); err != nil {
		return err
	}
	if !request.Explicit {
		return errors.New("durable memory writes must be explicit")
	}
	if request.Actor.Kind != MemoryActorParent {
		return errors.New("durable memory writes require the parent actor")
	}
	if request.Record.CreatedBy.Kind != MemoryActorParent {
		return errors.New("durable records must identify the parent as their creating actor")
	}
	if err := request.Record.Validate(policy); err != nil {
		return err
	}
	return policy.AllowsWrite(request.Actor, request.Mode, request.Record.Sensitivity)
}
