package bee

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxMemoryAuditIDLength    = MaxMemoryIDLength
	MaxMemoryDisplayContent   = 8 << 10
	MaxMemoryDisplayReason    = 2 << 10
	MaxMemoryExportRecords    = MaxMemoryRecords
	MaxMemoryExportBytes      = MaxMemoryBytes
	MaxMemoryImportErrors     = 128
	MaxMemoryProposalStatus   = "pending"
	MaxMemoryProposalApproved = "approved"
	MaxMemoryProposalRejected = "rejected"
)

var (
	ErrMemoryUnauthorized   = errors.New("memory actor is not authorized")
	ErrMemoryImportConflict = errors.New("memory import conflicts with existing state")
	ErrMemoryImportInvalid  = errors.New("memory import is invalid")
)

type MemoryService interface {
	Remember(context.Context, RememberRequest) (MemoryRecord, error)
	Edit(context.Context, EditMemoryRequest) (MemoryRecord, error)
	Forget(context.Context, ForgetRequest) error
	List(context.Context, ListMemoryRequest) (MemoryListResult, error)
	Show(context.Context, ShowMemoryRequest) (MemoryDisplayRecord, error)
	Search(context.Context, SearchMemoryRequest) (MemorySearchResult, error)
	SubmitProposal(context.Context, MemoryProposal) error
	ListProposals(context.Context, ListProposalRequest) (MemoryProposalListResult, error)
	ShowProposal(context.Context, ShowProposalRequest) (MemoryProposalView, error)
	ApproveProposal(context.Context, ApproveProposalRequest) (MemoryRecord, error)
	RejectProposal(context.Context, RejectProposalRequest) error
	Export(context.Context, MemoryExportRequest) (MemoryExport, error)
	Import(context.Context, MemoryImportRequest) (MemoryImportResult, error)
}

// MemoryFieldEditor and MemorySuperseder are additive lifecycle capabilities.
// Composition registers their model-facing tools only when the configured
// service actually implements the corresponding fail-closed semantics.
type MemoryFieldEditor interface {
	EditFields(context.Context, EditMemoryFieldsRequest) (MemoryRecord, error)
}

type MemorySuperseder interface {
	Supersede(context.Context, SupersedeMemoryRequest) (MemoryRecord, error)
}

// RememberRequest is an explicit, bounded parent-owned durable write.
type RememberRequest struct {
	Actor   MemoryActor     `json:"actor"`
	Record  MemoryRecord    `json:"record"`
	Mode    MemoryWriteMode `json:"mode"`
	AuditID string          `json:"audit_id"`
	Reason  string          `json:"reason"`
}

func (request RememberRequest) Validate(policy MemoryPolicy) error {
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return err
	}
	if request.Mode == "" {
		request.Mode = MemoryWriteExplicit
	}
	if request.Record.Version != 0 && request.Record.Version != 1 {
		return errors.New("remember record version must be zero or one")
	}
	if request.Record.CreatedBy.Kind != "" && request.Record.CreatedBy.Kind != MemoryActorParent {
		return errors.New("remember record must be parent-created")
	}
	return policy.AllowsWrite(request.Actor, request.Mode, request.Record.Sensitivity)
}

type EditMemoryRequest struct {
	Actor           MemoryActor     `json:"actor"`
	Record          MemoryRecord    `json:"record"`
	ExpectedVersion uint64          `json:"expected_version"`
	Mode            MemoryWriteMode `json:"mode"`
	AuditID         string          `json:"audit_id"`
	Reason          string          `json:"reason"`
}

func (request EditMemoryRequest) Validate(policy MemoryPolicy) error {
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return err
	}
	if request.ExpectedVersion == 0 {
		return errors.New("edit expected_version is required")
	}
	if request.Mode == "" {
		request.Mode = MemoryWriteExplicit
	}
	if request.Record.Version != 0 && request.Record.Version != request.ExpectedVersion && request.Record.Version != request.ExpectedVersion+1 {
		return errors.New("edit record version must match current or next version")
	}
	if err := policy.AllowsWrite(request.Actor, request.Mode, request.Record.Sensitivity); err != nil {
		return err
	}
	return nil
}

type ForgetRequest struct {
	Actor           MemoryActor `json:"actor"`
	RecordID        string      `json:"record_id"`
	ExpectedVersion uint64      `json:"expected_version"`
	AuditID         string      `json:"audit_id"`
	Reason          string      `json:"reason"`
}

// EditMemoryFieldsRequest is the parent-owned omission-preserving edit seam.
// The fields map contains only explicitly supplied mutable fields; the
// service loads the current full record before applying it.
type EditMemoryFieldsRequest struct {
	Actor           MemoryActor
	RecordID        string
	ExpectedVersion uint64
	Fields          map[string]any
	Mode            MemoryWriteMode
	AuditID         string
	Reason          string
}

// SupersedeMemoryRequest atomically creates Replacement and marks the old
// record superseded. Implementations that cannot provide an atomic store
// operation must fail closed rather than perform a two-step mutation.
type SupersedeMemoryRequest struct {
	Actor           MemoryActor
	RecordID        string
	ExpectedVersion uint64
	Replacement     MemoryRecord
	AuditID         string
	Reason          string
}

func (request ForgetRequest) Validate(policy MemoryPolicy) error {
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return err
	}
	if err := validateSafeID("forget record_id", request.RecordID, MaxMemoryIDLength); err != nil {
		return err
	}
	if request.ExpectedVersion == 0 {
		return errors.New("forget expected_version is required")
	}
	return policy.AllowsWrite(request.Actor, MemoryWriteExplicit, MemorySensitivityPublic)
}

func validateLifecycleAudit(actor MemoryActor, auditID, reason string) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind != MemoryActorParent {
		return fmt.Errorf("%w: parent actor required", ErrMemoryUnauthorized)
	}
	if err := validateSafeID("memory audit_id", auditID, MaxMemoryAuditIDLength); err != nil {
		return err
	}
	return validateText("memory audit reason", reason, MaxMemoryReasonLength, true)
}

type MemoryDisplayBounds struct {
	MaxRecords      int `json:"max_records,omitempty"`
	MaxBytes        int `json:"max_bytes,omitempty"`
	MaxContentBytes int `json:"max_content_bytes,omitempty"`
	MaxTags         int `json:"max_tags,omitempty"`
	MaxReasonBytes  int `json:"max_reason_bytes,omitempty"`
}

func (bounds MemoryDisplayBounds) effective(policy MemoryPolicy) MemoryDisplayBounds {
	if bounds.MaxRecords <= 0 {
		bounds.MaxRecords = policy.MaxQueryRecords
	}
	if bounds.MaxRecords > policy.MaxQueryRecords {
		bounds.MaxRecords = policy.MaxQueryRecords
	}
	if bounds.MaxBytes <= 0 {
		bounds.MaxBytes = policy.MaxQueryBytes
	}
	if bounds.MaxBytes > policy.MaxQueryBytes {
		bounds.MaxBytes = policy.MaxQueryBytes
	}
	if bounds.MaxContentBytes <= 0 {
		bounds.MaxContentBytes = MaxMemoryDisplayContent
	}
	if bounds.MaxContentBytes > MaxMemoryTextLength {
		bounds.MaxContentBytes = MaxMemoryTextLength
	}
	if bounds.MaxTags <= 0 {
		bounds.MaxTags = MaxMemoryTagCount
	}
	if bounds.MaxTags > MaxMemoryTagCount {
		bounds.MaxTags = MaxMemoryTagCount
	}
	if bounds.MaxReasonBytes <= 0 {
		bounds.MaxReasonBytes = MaxMemoryDisplayReason
	}
	if bounds.MaxReasonBytes > MaxMemoryReasonLength {
		bounds.MaxReasonBytes = MaxMemoryReasonLength
	}
	return bounds
}

func (bounds MemoryDisplayBounds) Validate(policy MemoryPolicy) error {
	if bounds.MaxRecords < 0 || bounds.MaxBytes < 0 || bounds.MaxContentBytes < 0 || bounds.MaxTags < 0 || bounds.MaxReasonBytes < 0 {
		return errors.New("memory display bounds must not be negative")
	}
	effective := bounds.effective(policy)
	if effective.MaxRecords <= 0 || effective.MaxBytes <= 0 || effective.MaxContentBytes <= 0 || effective.MaxTags <= 0 || effective.MaxReasonBytes <= 0 {
		return errors.New("memory display bounds must be positive")
	}
	return nil
}

type ListMemoryRequest struct {
	Actor            MemoryActor         `json:"actor"`
	Namespace        MemoryNamespace     `json:"namespace,omitempty"`
	SubjectID        string              `json:"subject_id,omitempty"`
	IncludeSensitive bool                `json:"include_sensitive,omitempty"`
	Bounds           MemoryDisplayBounds `json:"bounds"`
}

type ShowMemoryRequest struct {
	Actor            MemoryActor         `json:"actor"`
	RecordID         string              `json:"record_id"`
	IncludeSensitive bool                `json:"include_sensitive,omitempty"`
	Bounds           MemoryDisplayBounds `json:"bounds"`
}

type SearchMemoryRequest struct {
	Query   MemoryQuery            `json:"query"`
	Options MemoryRetrievalOptions `json:"options"`
	Bounds  MemoryDisplayBounds    `json:"bounds"`
}

type MemoryDisplayRecord struct {
	Record    MemoryRecord `json:"record"`
	Redacted  bool         `json:"redacted,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

type MemoryListResult struct {
	Records   []MemoryDisplayRecord `json:"records,omitempty"`
	Total     int                   `json:"total"`
	Bytes     int                   `json:"bytes"`
	Truncated bool                  `json:"truncated,omitempty"`
}

type MemoryDisplayHit struct {
	Record      MemoryDisplayRecord `json:"record"`
	Score       MemoryScore         `json:"score"`
	Explanation string              `json:"explanation,omitempty"`
	Reason      string              `json:"reason,omitempty"`
}

type MemorySearchResult struct {
	Hits        []MemoryDisplayHit         `json:"hits,omitempty"`
	Total       int                        `json:"total"`
	Bytes       int                        `json:"bytes"`
	Truncated   bool                       `json:"truncated,omitempty"`
	Diagnostics MemoryRetrievalDiagnostics `json:"diagnostics"`
}

type MemoryProposalStatus string

type MemoryProposalView struct {
	Proposal       MemoryProposal       `json:"proposal"`
	Record         MemoryDisplayRecord  `json:"record"`
	Status         MemoryProposalStatus `json:"status"`
	DecisionID     string               `json:"decision_id,omitempty"`
	DecisionReason string               `json:"decision_reason,omitempty"`
	DecidedAt      time.Time            `json:"decided_at,omitempty"`
}

type ListProposalRequest struct {
	Actor            MemoryActor         `json:"actor"`
	IncludeSensitive bool                `json:"include_sensitive,omitempty"`
	Bounds           MemoryDisplayBounds `json:"bounds"`
}
type ShowProposalRequest struct {
	Actor            MemoryActor         `json:"actor"`
	ProposalID       string              `json:"proposal_id"`
	IncludeSensitive bool                `json:"include_sensitive,omitempty"`
	Bounds           MemoryDisplayBounds `json:"bounds"`
}
type MemoryProposalListResult struct {
	Proposals []MemoryProposalView `json:"proposals,omitempty"`
	Total     int                  `json:"total"`
	Bytes     int                  `json:"bytes"`
	Truncated bool                 `json:"truncated,omitempty"`
}

type ApproveProposalRequest struct {
	Actor      MemoryActor `json:"actor"`
	ProposalID string      `json:"proposal_id"`
	AuditID    string      `json:"audit_id"`
	Reason     string      `json:"reason"`
}
type RejectProposalRequest struct {
	Actor      MemoryActor `json:"actor"`
	ProposalID string      `json:"proposal_id"`
	AuditID    string      `json:"audit_id"`
	Reason     string      `json:"reason"`
}

func (request ListProposalRequest) Validate(policy MemoryPolicy) error {
	return validateReviewActor(request.Actor, policy)
}
func (request ShowProposalRequest) Validate(policy MemoryPolicy) error {
	if err := validateReviewActor(request.Actor, policy); err != nil {
		return err
	}
	return validateSafeID("memory proposal id", request.ProposalID, MaxMemoryIDLength)
}
func (request ApproveProposalRequest) Validate(policy MemoryPolicy) error {
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return err
	}
	return validateSafeID("memory proposal id", request.ProposalID, MaxMemoryIDLength)
}
func (request RejectProposalRequest) Validate(policy MemoryPolicy) error {
	return ApproveProposalRequest{Actor: request.Actor, ProposalID: request.ProposalID, AuditID: request.AuditID, Reason: request.Reason}.Validate(policy)
}
func validateReviewActor(actor MemoryActor, policy MemoryPolicy) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind != MemoryActorParent && actor.Kind != MemoryActorSystem {
		return fmt.Errorf("%w: parent review actor required", ErrMemoryUnauthorized)
	}
	return policy.Validate()
}

type MemoryExportRequest struct {
	Actor            MemoryActor `json:"actor"`
	IncludeSensitive bool        `json:"include_sensitive,omitempty"`
	AuditID          string      `json:"audit_id"`
	Reason           string      `json:"reason"`
}
type MemoryExport struct {
	FormatVersion   int                    `json:"format_version"`
	Identity        *IdentityProfile       `json:"identity,omitempty"`
	Profiles        []UserProfile          `json:"profiles,omitempty"`
	Memory          []MemoryRecord         `json:"memory,omitempty"`
	Proposals       []MemoryProposal       `json:"proposals,omitempty"`
	ProposalReviews []MemoryProposalReview `json:"proposal_reviews,omitempty"`
}
type MemoryProposalReview struct {
	ProposalID string               `json:"proposal_id"`
	Status     MemoryProposalStatus `json:"status"`
	DecisionID string               `json:"decision_id"`
	Reason     string               `json:"reason"`
	At         time.Time            `json:"at"`
	Actor      MemoryActor          `json:"actor"`
}

type MemoryImportRequest struct {
	Actor   MemoryActor   `json:"actor"`
	Payload []byte        `json:"payload"`
	Export  *MemoryExport `json:"export,omitempty"`
	DryRun  bool          `json:"dry_run,omitempty"`
	AuditID string        `json:"audit_id"`
	Reason  string        `json:"reason"`
}
type MemoryImportConflict struct {
	Category string `json:"category"`
	ID       string `json:"id"`
	Reason   string `json:"reason"`
}
type MemoryImportResult struct {
	DryRun    bool                   `json:"dry_run"`
	Applied   bool                   `json:"applied"`
	Valid     bool                   `json:"valid"`
	Identity  int                    `json:"identity"`
	Profiles  int                    `json:"profiles"`
	Memory    int                    `json:"memory"`
	Proposals int                    `json:"proposals"`
	Conflicts []MemoryImportConflict `json:"conflicts,omitempty"`
	Errors    []string               `json:"errors,omitempty"`
}

func (request MemoryExportRequest) Validate(policy MemoryPolicy) error {
	return validateLifecycleAudit(request.Actor, request.AuditID, request.Reason)
}
func (request MemoryImportRequest) Validate(policy MemoryPolicy) error {
	return validateLifecycleAudit(request.Actor, request.AuditID, request.Reason)
}

// LocalMemoryService is the explicit lifecycle seam over the Phase 7B/7C
// stores. It never adds channels or prompt assembly.
type LocalMemoryService struct {
	Memory       MemoryStore
	Proposals    MemoryProposalSink
	Identity     IdentityStore
	Profiles     UserProfileStore
	policy       MemoryPolicy
	mutationMu   sync.RWMutex
	mutationHook func()
}

var _ MemoryService = (*LocalMemoryService)(nil)

func NewLocalMemoryService(root string, policy MemoryPolicy) (*LocalMemoryService, error) {
	memory, err := NewLocalMemoryStore(root, policy)
	if err != nil {
		return nil, err
	}
	proposals, err := NewLocalMemoryProposalSink(root, policy)
	if err != nil {
		return nil, err
	}
	identity, err := NewLocalIdentityStore(root)
	if err != nil {
		return nil, err
	}
	profiles, err := NewLocalUserProfileStore(root)
	if err != nil {
		return nil, err
	}
	return NewLocalMemoryServiceWithStores(memory, proposals, identity, profiles, policy)
}
func NewLocalMemoryLifecycleService(root string, policy MemoryPolicy) (*LocalMemoryService, error) {
	return NewLocalMemoryService(root, policy)
}
func NewLocalMemoryServiceWithStores(memory MemoryStore, proposals MemoryProposalSink, identity IdentityStore, profiles UserProfileStore, policy MemoryPolicy) (*LocalMemoryService, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if memory == nil || proposals == nil || identity == nil || profiles == nil {
		return nil, errors.New("all memory lifecycle stores are required")
	}
	return &LocalMemoryService{Memory: memory, Proposals: proposals, Identity: identity, Profiles: profiles, policy: policy}, nil
}
func (service *LocalMemoryService) Policy() MemoryPolicy {
	if service == nil {
		return MemoryPolicy{}
	}
	return service.policy
}

// SetMutationHook installs a best-effort notification invoked after durable
// memory state changes. The hook must return quickly; semantic refresh uses it
// only to enqueue coalesced background work.
func (service *LocalMemoryService) SetMutationHook(hook func()) {
	if service == nil {
		return
	}
	service.mutationMu.Lock()
	service.mutationHook = hook
	service.mutationMu.Unlock()
}

func (service *LocalMemoryService) notifyMutation() {
	if service == nil {
		return
	}
	service.mutationMu.RLock()
	hook := service.mutationHook
	service.mutationMu.RUnlock()
	if hook != nil {
		hook()
	}
}

func (service *LocalMemoryService) Remember(ctx context.Context, request RememberRequest) (MemoryRecord, error) {
	if service == nil {
		return MemoryRecord{}, errors.New("memory service is nil")
	}
	if request.Mode == "" {
		request.Mode = MemoryWriteExplicit
	}
	if err := request.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	record := request.Record
	now := time.Now().UTC()
	if record.ID == "" {
		return MemoryRecord{}, errors.New("remember record id is required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	record.Version = 1
	record.CreatedBy = request.Actor
	if err := record.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	event := MemoryEvent{ID: request.AuditID, Type: MemoryEventCreated, At: now, Actor: request.Actor, Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: 1, Reason: request.Reason}
	if err := service.Memory.Append(ctx, event); err != nil {
		service.appendConflictAudit(ctx, request.Actor, record.ID, request.AuditID, err)
		return MemoryRecord{}, err
	}
	service.notifyMutation()
	return record, nil
}

func (service *LocalMemoryService) Edit(ctx context.Context, request EditMemoryRequest) (MemoryRecord, error) {
	if service == nil {
		return MemoryRecord{}, errors.New("memory service is nil")
	}
	if request.Mode == "" {
		request.Mode = MemoryWriteExplicit
	}
	if err := request.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	record := request.Record
	if record.ID == "" {
		return MemoryRecord{}, errors.New("edit record id is required")
	}
	record.Version = request.ExpectedVersion + 1
	if record.CreatedAt.IsZero() {
		return MemoryRecord{}, errors.New("edit created_at is required")
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	// The creator is identity metadata and stays fixed across edits (the store
	// rejects a change); the event's actor records who made this edit. Only a
	// caller that supplies no creator gets the editing actor filled in.
	if record.CreatedBy.ID == "" {
		record.CreatedBy = request.Actor
	}
	if err := record.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	event := MemoryEvent{ID: request.AuditID, Type: MemoryEventUpdated, At: time.Now().UTC(), Actor: request.Actor, Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, ExpectedVersion: request.ExpectedVersion, NewVersion: record.Version, Reason: request.Reason}
	if err := service.Memory.Append(ctx, event); err != nil {
		service.appendConflictAudit(ctx, request.Actor, record.ID, request.AuditID, err)
		return MemoryRecord{}, err
	}
	service.notifyMutation()
	return record, nil
}

func (service *LocalMemoryService) EditFields(ctx context.Context, request EditMemoryFieldsRequest) (MemoryRecord, error) {
	if service == nil {
		return MemoryRecord{}, errors.New("memory service is nil")
	}
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return MemoryRecord{}, err
	}
	if err := validateSafeID("edit record_id", request.RecordID, MaxMemoryIDLength); err != nil {
		return MemoryRecord{}, err
	}
	if request.ExpectedVersion == 0 {
		return MemoryRecord{}, errors.New("edit expected_version is required")
	}
	if err := service.policy.AllowsWrite(request.Actor, MemoryWriteExplicit, MemorySensitivityPublic); err != nil {
		return MemoryRecord{}, err
	}
	reader, ok := service.Memory.(interface {
		GetRecord(context.Context, MemoryActor, string, bool) (MemoryRecord, error)
	})
	if !ok {
		return MemoryRecord{}, errors.New("memory store does not support omission-preserving edits")
	}
	current, err := reader.GetRecord(ctx, request.Actor, request.RecordID, false)
	if err != nil {
		return MemoryRecord{}, err
	}
	if current.Version != request.ExpectedVersion {
		return MemoryRecord{}, fmt.Errorf("memory record %q expected version %d, current version %d: %w", request.RecordID, request.ExpectedVersion, current.Version, ErrMemoryConflict)
	}
	if err := applyMemoryFieldPatch(&current, request.Fields); err != nil {
		return MemoryRecord{}, err
	}
	// The updated date tells a reader when the record's facts were last
	// stated, so any edit to them stamps the current time. A status-only edit
	// keeps the date: retiring a record to history must not make it look new.
	for name := range request.Fields {
		if name != "status" {
			current.UpdatedAt = time.Time{}
			break
		}
	}
	return service.Edit(ctx, EditMemoryRequest{Actor: request.Actor, Record: current, ExpectedVersion: request.ExpectedVersion, Mode: MemoryWriteExplicit, AuditID: request.AuditID, Reason: request.Reason})
}

func (service *LocalMemoryService) Supersede(ctx context.Context, request SupersedeMemoryRequest) (MemoryRecord, error) {
	if service == nil {
		return MemoryRecord{}, errors.New("memory service is nil")
	}
	if err := validateLifecycleAudit(request.Actor, request.AuditID, request.Reason); err != nil {
		return MemoryRecord{}, err
	}
	if err := validateSafeID("supersede record_id", request.RecordID, MaxMemoryIDLength); err != nil {
		return MemoryRecord{}, err
	}
	if request.ExpectedVersion == 0 {
		return MemoryRecord{}, errors.New("supersede expected_version is required")
	}
	if err := service.policy.AllowsWrite(request.Actor, MemoryWriteExplicit, request.Replacement.Sensitivity); err != nil {
		return MemoryRecord{}, err
	}
	reader, ok := service.Memory.(interface {
		GetRecord(context.Context, MemoryActor, string, bool) (MemoryRecord, error)
	})
	batch, atomic := service.Memory.(interface {
		AppendBatch(context.Context, []MemoryEvent) error
	})
	if !ok || !atomic {
		return MemoryRecord{}, errors.New("memory store does not support atomic supersession")
	}
	old, err := reader.GetRecord(ctx, request.Actor, request.RecordID, false)
	if err != nil {
		return MemoryRecord{}, err
	}
	if old.Version != request.ExpectedVersion {
		return MemoryRecord{}, fmt.Errorf("memory record %q expected version %d, current version %d: %w", request.RecordID, request.ExpectedVersion, old.Version, ErrMemoryConflict)
	}
	replacement := request.Replacement
	if replacement.ID == "" {
		replacement.ID = memorySupersedeRecordID(old.Namespace, replacement.Key, request.AuditID)
	}
	if err := validateSafeID("supersede replacement id", replacement.ID, MaxMemoryIDLength); err != nil {
		return MemoryRecord{}, err
	}
	if replacement.ID == request.RecordID {
		return MemoryRecord{}, errors.New("supersede replacement must have a distinct id")
	}
	now := time.Now().UTC()
	if replacement.CreatedAt.IsZero() {
		replacement.CreatedAt = now
	}
	if replacement.UpdatedAt.IsZero() {
		replacement.UpdatedAt = now
	}
	replacement.Version = 1
	replacement.CreatedBy = request.Actor
	// Lifecycle metadata is always parent/service-owned, never caller-owned.
	replacement.Namespace = old.Namespace
	replacement.Status = MemoryStatusActive
	replacement.SupersededBy = ""
	if err := replacement.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	old.Status = MemoryStatusSuperseded
	old.SupersededBy = replacement.ID
	old.Version = request.ExpectedVersion + 1
	old.UpdatedAt = now
	if err := old.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	// One batch append holds the store lock across validation, replay, and
	// fsync, so failure cannot leave a half-applied supersession.
	events := []MemoryEvent{
		{ID: request.AuditID + "-replacement", Type: MemoryEventCreated, At: now, Actor: request.Actor, Record: replacement, RecordID: replacement.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: 1, Reason: request.Reason},
		{ID: request.AuditID, Type: MemoryEventUpdated, At: now, Actor: request.Actor, Record: old, RecordID: old.ID, Mode: MemoryWriteExplicit, Explicit: true, ExpectedVersion: request.ExpectedVersion, NewVersion: old.Version, Reason: trimText(request.Reason+"; superseded_by="+replacement.ID, MaxMemoryReasonLength)},
	}
	if err := batch.AppendBatch(ctx, events); err != nil {
		service.appendConflictAudit(ctx, request.Actor, request.RecordID, request.AuditID, err)
		return MemoryRecord{}, err
	}
	service.notifyMutation()
	return replacement, nil
}

func applyMemoryFieldPatch(record *MemoryRecord, fields map[string]any) error {
	if record == nil {
		return errors.New("memory edit record is nil")
	}
	for name, value := range fields {
		switch name {
		case "key":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit key must be a string")
			}
			record.Key = v
		case "subject_id":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit subject_id must be a string")
			}
			record.SubjectID = v
		case "content":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit content must be a string")
			}
			record.Content = v
		case "summary":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit summary must be a string")
			}
			record.Summary = v
		case "sensitivity":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit sensitivity must be a string")
			}
			record.Sensitivity = MemorySensitivity(v)
		case "status":
			// An edit can retire a record to history or bring it back. Marking
			// one superseded needs its replacement, which is memory_supersede.
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit status must be a string")
			}
			status := MemoryStatus(v)
			if status != MemoryStatusActive && status != MemoryStatusHistorical {
				return errors.New(`memory edit status must be "active" or "historical"; use supersede to replace a record`)
			}
			if record.Status.effective() == MemoryStatusSuperseded {
				return errors.New("memory edit cannot change the status of a superseded record")
			}
			record.Status = status
		case "tags":
			v, ok := value.(map[string]any)
			if !ok {
				return errors.New("memory edit tags must be an object")
			}
			tags := make(map[string]string, len(v))
			for k, raw := range v {
				s, ok := raw.(string)
				if !ok {
					return errors.New("memory edit tag values must be strings")
				}
				tags[k] = s
			}
			record.Tags = tags
		case "source":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit source must be a string")
			}
			record.Source = v
		case "expires_at":
			v, ok := value.(string)
			if !ok {
				return errors.New("memory edit expires_at must be RFC3339 string")
			}
			if v == "" {
				record.ExpiresAt = time.Time{}
				continue
			}
			parsed, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return errors.New("memory edit expires_at must be RFC3339")
			}
			record.ExpiresAt = parsed.UTC()
		case "confidence":
			v, ok := value.(float64)
			if !ok {
				return errors.New("memory edit confidence must be a number")
			}
			record.Confidence = v
		default:
			return fmt.Errorf("memory edit field %q is not mutable", name)
		}
	}
	return nil
}

func (service *LocalMemoryService) Forget(ctx context.Context, request ForgetRequest) error {
	if service == nil {
		return errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return err
	}
	event := MemoryEvent{ID: request.AuditID, Type: MemoryEventDeleted, At: time.Now().UTC(), Actor: request.Actor, RecordID: request.RecordID, Explicit: true, ExpectedVersion: request.ExpectedVersion, Reason: request.Reason}
	if err := service.Memory.Append(ctx, event); err != nil {
		service.appendConflictAudit(ctx, request.Actor, request.RecordID, request.AuditID, err)
		return err
	}
	service.notifyMutation()
	return nil
}

func (service *LocalMemoryService) List(ctx context.Context, request ListMemoryRequest) (MemoryListResult, error) {
	if service == nil {
		return MemoryListResult{}, errors.New("memory service is nil")
	}
	if err := request.Actor.Validate(); err != nil {
		return MemoryListResult{}, err
	}
	if request.Actor.Kind == MemoryActorChild && !service.policy.ChildMayRead() {
		return MemoryListResult{}, ErrMemoryUnauthorized
	}
	if request.IncludeSensitive && (request.Actor.Kind == MemoryActorChild || !service.policy.AllowSensitiveMemory) {
		return MemoryListResult{}, errors.New("sensitive memory is not authorized")
	}
	if err := request.Bounds.Validate(service.policy); err != nil {
		return MemoryListResult{}, err
	}
	query := MemoryQuery{Actor: request.Actor, Namespace: request.Namespace, SubjectID: request.SubjectID, IncludeSensitive: request.IncludeSensitive, Limit: request.Bounds.effective(service.policy).MaxRecords, MaxBytes: request.Bounds.effective(service.policy).MaxBytes}
	if query.Namespace == "" {
		query.Namespace = MemoryNamespaceDurableMemory
	}
	if err := query.Validate(service.policy); err != nil {
		return MemoryListResult{}, err
	}
	result, err := service.Memory.Retrieve(ctx, query)
	if err != nil {
		return MemoryListResult{}, err
	}
	return service.displayList(result.Records, result.Total, result.Truncated, request.Actor, request.IncludeSensitive, request.Bounds), nil
}

func (service *LocalMemoryService) Show(ctx context.Context, request ShowMemoryRequest) (MemoryDisplayRecord, error) {
	if service == nil {
		return MemoryDisplayRecord{}, errors.New("memory service is nil")
	}
	if err := request.Actor.Validate(); err != nil {
		return MemoryDisplayRecord{}, err
	}
	if err := validateSafeID("memory record id", request.RecordID, MaxMemoryIDLength); err != nil {
		return MemoryDisplayRecord{}, err
	}
	bounds := request.Bounds.effective(service.policy)
	if reader, ok := service.Memory.(interface {
		GetRecord(context.Context, MemoryActor, string, bool) (MemoryRecord, error)
	}); ok {
		record, err := reader.GetRecord(ctx, request.Actor, request.RecordID, request.IncludeSensitive)
		if err != nil {
			return MemoryDisplayRecord{}, err
		}
		return service.displayRecord(record, request.Actor, request.IncludeSensitive, bounds), nil
	}
	list, err := service.List(ctx, ListMemoryRequest{Actor: request.Actor, IncludeSensitive: request.IncludeSensitive, Bounds: bounds})
	if err != nil {
		return MemoryDisplayRecord{}, err
	}
	for _, item := range list.Records {
		if item.Record.ID == request.RecordID {
			return item, nil
		}
	}
	return MemoryDisplayRecord{}, ErrMemoryNotFound
}

func (service *LocalMemoryService) Search(ctx context.Context, request SearchMemoryRequest) (MemorySearchResult, error) {
	if service == nil {
		return MemorySearchResult{}, errors.New("memory service is nil")
	}
	if err := request.Query.Validate(service.policy); err != nil {
		return MemorySearchResult{}, err
	}
	if err := request.Bounds.Validate(service.policy); err != nil {
		return MemorySearchResult{}, err
	}
	effective := request.Options
	bounds := request.Bounds.effective(service.policy)
	if effective.MaxResults == 0 && effective.ResultLimit == 0 {
		effective.MaxResults = bounds.MaxRecords
	}
	if effective.MaxBytes == 0 {
		effective.MaxBytes = bounds.MaxBytes
	}
	result, err := service.rankedRetriever().RetrieveRanked(ctx, request.Query, effective)
	if err != nil {
		return MemorySearchResult{}, err
	}
	out := MemorySearchResult{Total: result.Total, Truncated: result.Truncated, Diagnostics: result.Diagnostics}
	for _, hit := range result.Hits {
		item := service.displayRecord(hit.Record, request.Query.Actor, request.Query.IncludeSensitive, bounds)
		explanation := hit.Explanation
		reason := hit.Reason
		if len(explanation) > bounds.MaxReasonBytes {
			explanation = explanation[:bounds.MaxReasonBytes]
		}
		if len(reason) > bounds.MaxReasonBytes {
			reason = reason[:bounds.MaxReasonBytes]
		}
		out.Hits = append(out.Hits, MemoryDisplayHit{Record: item, Score: hit.Score, Explanation: explanation, Reason: reason})
		encoded, _ := json.Marshal(out.Hits[len(out.Hits)-1])
		if out.Bytes+len(encoded) > bounds.MaxBytes {
			out.Hits = out.Hits[:len(out.Hits)-1]
			out.Truncated = true
			break
		}
		out.Bytes += len(encoded)
	}
	if len(out.Hits) < len(result.Hits) {
		out.Truncated = true
	}
	return out, nil
}

func (service *LocalMemoryService) rankedRetriever() RankedMemoryRetriever {
	if retriever, ok := service.Memory.(RankedMemoryRetriever); ok {
		return retriever
	}
	return rankedAdapter{service.Memory}
}

type rankedAdapter struct{ MemoryStore }

func (adapter rankedAdapter) RetrieveRanked(ctx context.Context, query MemoryQuery, options MemoryRetrievalOptions) (RankedMemoryResult, error) {
	result, err := adapter.Retrieve(ctx, query)
	if err != nil {
		return RankedMemoryResult{}, err
	}
	hits := make([]RankedMemoryHit, 0, len(result.Records))
	for _, record := range result.Records {
		hits = append(hits, RankedMemoryHit{Record: record, Score: MemoryScore{Total: 0, Value: 0}, Reason: "bounded memory result"})
	}
	return RankedMemoryResult{Hits: hits, Total: result.Total, Bytes: result.Bytes, Truncated: result.Truncated, Diagnostics: MemoryRetrievalDiagnostics{Candidates: result.Total, Returned: len(hits)}}, nil
}

func (service *LocalMemoryService) displayList(records []MemoryRecord, total int, truncated bool, actor MemoryActor, includeSensitive bool, bounds MemoryDisplayBounds) MemoryListResult {
	effective := bounds.effective(service.policy)
	result := MemoryListResult{Total: total, Truncated: truncated}
	for _, record := range records {
		item := service.displayRecord(record, actor, includeSensitive, effective)
		encoded, _ := json.Marshal(item)
		if len(result.Records) >= effective.MaxRecords || result.Bytes+len(encoded) > effective.MaxBytes {
			result.Truncated = true
			break
		}
		result.Records = append(result.Records, item)
		result.Bytes += len(encoded)
	}
	if len(result.Records) < len(records) {
		result.Truncated = true
	}
	return result
}
func (service *LocalMemoryService) displayRecord(record MemoryRecord, actor MemoryActor, includeSensitive bool, bounds MemoryDisplayBounds) MemoryDisplayRecord {
	copy := record
	item := MemoryDisplayRecord{Record: copy}
	authorized := actor.Kind == MemoryActorParent || actor.Kind == MemoryActorSystem
	if record.Sensitivity == MemorySensitivitySensitive && (!includeSensitive || !authorized || !service.policy.AllowSensitiveMemory) {
		item.Record.Content = ""
		item.Record.Summary = ""
		item.Record.Source = ""
		item.Redacted = true
	}
	if len(item.Record.Content) > bounds.MaxContentBytes {
		item.Record.Content = item.Record.Content[:bounds.MaxContentBytes]
		item.Truncated = true
	}
	if len(item.Record.Summary) > bounds.MaxReasonBytes {
		item.Record.Summary = item.Record.Summary[:bounds.MaxReasonBytes]
		item.Truncated = true
	}
	if item.Redacted {
		item.Record.Tags = nil
	}
	if len(item.Record.Tags) > bounds.MaxTags {
		keys := make([]string, 0, len(item.Record.Tags))
		for key := range item.Record.Tags {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		trimmed := make(map[string]string, bounds.MaxTags)
		for _, key := range keys[:bounds.MaxTags] {
			trimmed[key] = item.Record.Tags[key]
		}
		item.Record.Tags = trimmed
		item.Truncated = true
	}
	return item
}

func (service *LocalMemoryService) SubmitProposal(ctx context.Context, proposal MemoryProposal) error {
	if service == nil || service.Proposals == nil {
		return errors.New("memory proposal service is nil")
	}
	return service.Proposals.SubmitProposal(ctx, proposal)
}

func (service *LocalMemoryService) ListProposals(ctx context.Context, request ListProposalRequest) (MemoryProposalListResult, error) {
	if service == nil {
		return MemoryProposalListResult{}, errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return MemoryProposalListResult{}, err
	}
	if request.IncludeSensitive && !service.policy.AllowSensitiveMemory {
		return MemoryProposalListResult{}, errors.New("sensitive memory is disabled by policy")
	}
	if err := request.Bounds.Validate(service.policy); err != nil {
		return MemoryProposalListResult{}, err
	}
	proposals, reviews, err := service.loadProposals(ctx)
	if err != nil {
		return MemoryProposalListResult{}, err
	}
	bounds := request.Bounds.effective(service.policy)
	out := MemoryProposalListResult{}
	pending := 0
	for _, proposal := range proposals {
		review := reviews[proposal.ID]
		if review.Status != "" && review.Status != MemoryProposalStatus(MaxMemoryProposalStatus) {
			continue
		}
		pending++
		view := service.proposalView(proposal, review, request.Actor, request.IncludeSensitive, bounds)
		encoded, _ := json.Marshal(view)
		if len(out.Proposals) >= bounds.MaxRecords || out.Bytes+len(encoded) > bounds.MaxBytes {
			out.Truncated = true
			continue
		}
		out.Proposals = append(out.Proposals, view)
		out.Bytes += len(encoded)
	}
	out.Total = pending
	if len(out.Proposals) < pending {
		out.Truncated = true
	}
	return out, nil
}
func (service *LocalMemoryService) ShowProposal(ctx context.Context, request ShowProposalRequest) (MemoryProposalView, error) {
	if service == nil {
		return MemoryProposalView{}, errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return MemoryProposalView{}, err
	}
	if request.IncludeSensitive && !service.policy.AllowSensitiveMemory {
		return MemoryProposalView{}, errors.New("sensitive memory is disabled by policy")
	}
	if err := request.Bounds.Validate(service.policy); err != nil {
		return MemoryProposalView{}, err
	}
	proposals, reviews, err := service.loadProposals(ctx)
	if err != nil {
		return MemoryProposalView{}, err
	}
	for _, proposal := range proposals {
		if proposal.ID == request.ProposalID {
			return service.proposalView(proposal, reviews[proposal.ID], request.Actor, request.IncludeSensitive, request.Bounds.effective(service.policy)), nil
		}
	}
	return MemoryProposalView{}, ErrMemoryNotFound
}
func (service *LocalMemoryService) proposalView(proposal MemoryProposal, review MemoryProposalReview, actor MemoryActor, includeSensitive bool, bounds MemoryDisplayBounds) MemoryProposalView {
	display := service.displayRecord(proposal.Record, actor, includeSensitive, bounds)
	// Do not leave an unredacted copy nested in Proposal.Record.
	proposal.Record = display.Record
	return MemoryProposalView{Proposal: proposal, Record: display, Status: review.StatusOrPending(), DecisionID: review.DecisionID, DecisionReason: trimText(review.Reason, bounds.MaxReasonBytes), DecidedAt: review.At}
}

func (service *LocalMemoryService) ApproveProposal(ctx context.Context, request ApproveProposalRequest) (MemoryRecord, error) {
	if err := request.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	proposals, reviews, err := service.loadProposals(ctx)
	if err != nil {
		return MemoryRecord{}, err
	}
	review := reviews[request.ProposalID]
	if review.Status != "" && review.Status != MemoryProposalStatus(MaxMemoryProposalStatus) {
		return MemoryRecord{}, errors.New("memory proposal is already decided")
	}
	var selected *MemoryProposal
	for index := range proposals {
		if proposals[index].ID == request.ProposalID {
			selected = &proposals[index]
			break
		}
	}
	if selected == nil {
		return MemoryRecord{}, ErrMemoryNotFound
	}
	record := selected.Record
	record.CreatedBy = request.Actor
	record.Version = 1
	if err := record.Validate(service.policy); err != nil {
		return MemoryRecord{}, err
	}
	event := MemoryEvent{ID: request.AuditID, Type: MemoryEventCreated, At: time.Now().UTC(), Actor: request.Actor, Record: record, RecordID: record.ID, ProposalID: selected.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: 1, Reason: request.Reason + "; provenance=" + selected.ID}
	if err := service.Memory.Append(ctx, event); err != nil {
		return MemoryRecord{}, err
	}
	service.notifyMutation()
	if err := service.appendReview(ctx, MemoryProposalReview{ProposalID: selected.ID, Status: MemoryProposalStatus(MaxMemoryProposalApproved), DecisionID: request.AuditID, Reason: request.Reason, At: event.At, Actor: request.Actor}); err != nil {
		return MemoryRecord{}, err
	}
	return record, nil
}
func (service *LocalMemoryService) RejectProposal(ctx context.Context, request RejectProposalRequest) error {
	if service == nil {
		return errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return err
	}
	_, reviews, err := service.loadProposals(ctx)
	if err != nil {
		return err
	}
	review := reviews[request.ProposalID]
	if review.Status != "" && review.Status != MemoryProposalStatus(MaxMemoryProposalStatus) {
		return errors.New("memory proposal is already decided")
	}
	if _, err := service.ShowProposal(ctx, ShowProposalRequest{Actor: request.Actor, ProposalID: request.ProposalID}); err != nil {
		return err
	}
	return service.appendReview(ctx, MemoryProposalReview{ProposalID: request.ProposalID, Status: MemoryProposalStatus(MaxMemoryProposalRejected), DecisionID: request.AuditID, Reason: request.Reason, At: time.Now().UTC(), Actor: request.Actor})
}

func (review MemoryProposalReview) StatusOrPending() MemoryProposalStatus {
	if review.Status == "" {
		return MemoryProposalStatus(MaxMemoryProposalStatus)
	}
	return review.Status
}

func (service *LocalMemoryService) Export(ctx context.Context, request MemoryExportRequest) (MemoryExport, error) {
	if service == nil {
		return MemoryExport{}, errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return MemoryExport{}, err
	}
	out := MemoryExport{FormatVersion: 1}
	if identity, err := service.Identity.Load(ctx); err == nil {
		out.Identity = &identity
	} else if !errors.Is(err, ErrMemoryNotFound) {
		return MemoryExport{}, err
	}
	if local, ok := service.Profiles.(*LocalUserProfileStore); ok {
		profiles, err := loadAllProfiles(ctx, local)
		if err != nil {
			return MemoryExport{}, err
		}
		out.Profiles = profiles
	}
	reader, ok := service.Memory.(MemoryRecordReader)
	if !ok {
		return MemoryExport{}, errors.New("memory export requires a full record reader")
	}
	records, err := reader.ListRecords(ctx, request.Actor, request.IncludeSensitive)
	if err != nil {
		return MemoryExport{}, err
	}
	if len(records) > MaxMemoryExportRecords {
		return MemoryExport{}, errors.New("memory export exceeds bounded record count")
	}
	out.Memory = append(out.Memory, records...)
	proposals, reviews, err := service.loadProposals(ctx)
	if err != nil {
		return MemoryExport{}, err
	}
	allowedProposals := make(map[string]struct{}, len(proposals))
	for _, proposal := range proposals {
		if proposal.Record.Sensitivity == MemorySensitivitySensitive && !request.IncludeSensitive {
			continue
		}
		out.Proposals = append(out.Proposals, proposal)
		allowedProposals[proposal.ID] = struct{}{}
	}
	for _, review := range reviews {
		if _, allowed := allowedProposals[review.ProposalID]; allowed {
			out.ProposalReviews = append(out.ProposalReviews, review)
		}
	}
	sortExport(&out)
	data, err := CanonicalJSON(out)
	if err != nil || len(data) > MaxMemoryExportBytes {
		if err == nil {
			err = errors.New("memory export exceeds bounded size")
		}
		return MemoryExport{}, err
	}
	return out, nil
}

func (service *LocalMemoryService) Import(ctx context.Context, request MemoryImportRequest) (MemoryImportResult, error) {
	result := MemoryImportResult{DryRun: request.DryRun}
	if service == nil {
		return result, errors.New("memory service is nil")
	}
	if err := request.Validate(service.policy); err != nil {
		return result, err
	}
	export := request.Export
	if export == nil {
		if len(request.Payload) == 0 {
			return result, fmt.Errorf("%w: payload is required", ErrMemoryImportInvalid)
		}
		if len(request.Payload) > MaxMemoryExportBytes {
			return result, fmt.Errorf("%w: payload exceeds bounded size", ErrMemoryImportInvalid)
		}
		if err := json.Unmarshal(request.Payload, &export); err != nil {
			result.Errors = []string{err.Error()}
			return result, fmt.Errorf("%w: %v", ErrMemoryImportInvalid, err)
		}
	}
	conflicts, validationErrors := service.validateImport(ctx, *export, request)
	result.Conflicts, result.Errors = conflicts, validationErrors
	result.Valid = len(conflicts) == 0 && len(validationErrors) == 0
	result.Identity, result.Profiles, result.Memory, result.Proposals = boolInt(export.Identity != nil), len(export.Profiles), len(export.Memory), len(export.Proposals)
	if !result.Valid {
		if len(conflicts) > 0 {
			return result, ErrMemoryImportConflict
		}
		return result, ErrMemoryImportInvalid
	}
	if request.DryRun {
		return result, nil
	}
	if err := service.applyImportAtomic(ctx, *export, request); err != nil {
		return result, err
	}
	result.Applied = true
	service.notifyMutation()
	return result, nil
}

func (service *LocalMemoryService) appendConflictAudit(ctx context.Context, actor MemoryActor, recordID, auditID string, cause error) {
	if !errors.Is(cause, ErrMemoryConflict) {
		return
	}
	sum := sha256.Sum256([]byte("conflict\\x00" + auditID + "\\x00" + recordID))
	event := MemoryEvent{ID: "conflict-" + hex.EncodeToString(sum[:])[:32], Type: MemoryEventConflict, At: time.Now().UTC(), Actor: actor, RecordID: recordID, ConflictWith: auditID, Explicit: true, Reason: trimText("optimistic version conflict: "+cause.Error(), MaxMemorySummaryLength)}
	_ = service.Memory.Append(ctx, event)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (service *LocalMemoryService) validateImport(ctx context.Context, export MemoryExport, request MemoryImportRequest) ([]MemoryImportConflict, []string) {
	var conflicts []MemoryImportConflict
	var errs []string
	if export.FormatVersion != 1 {
		errs = append(errs, "unsupported export format_version")
	}
	if len(export.Memory) > MaxMemoryExportRecords || len(export.Profiles) > MaxMemoryExportRecords || len(export.Proposals) > MaxMemoryExportRecords || len(export.ProposalReviews) > MaxMemoryExportRecords {
		errs = append(errs, "memory import exceeds bounded record count")
	}
	if encoded, err := CanonicalJSON(export); err != nil {
		errs = append(errs, "memory import encoding: "+err.Error())
	} else if len(encoded) > MaxMemoryExportBytes {
		errs = append(errs, "memory import exceeds bounded byte size")
	}
	seen := map[string]map[string]bool{"memory": {}, "proposals": {}, "profiles": {}}
	if export.Identity != nil {
		if err := export.Identity.Validate(); err != nil {
			errs = append(errs, "identity: "+err.Error())
		} else if _, err := service.Identity.Load(ctx); err == nil {
			conflicts = append(conflicts, MemoryImportConflict{Category: "identity", ID: export.Identity.ID, Reason: "identity already exists"})
		} else if !errors.Is(err, ErrMemoryNotFound) {
			errs = append(errs, err.Error())
		}
	}
	for _, profile := range export.Profiles {
		if err := profile.Validate(); err != nil {
			errs = append(errs, "profile: "+err.Error())
			continue
		}
		if seen["profiles"][profile.ID] {
			errs = append(errs, "duplicate profile "+profile.ID)
		}
		seen["profiles"][profile.ID] = true
		if _, err := service.Profiles.Load(ctx, profile.ID); err == nil {
			conflicts = append(conflicts, MemoryImportConflict{Category: "profile", ID: profile.ID, Reason: "profile already exists"})
		} else if !errors.Is(err, ErrMemoryNotFound) {
			errs = append(errs, err.Error())
		}
	}
	for _, record := range export.Memory {
		if err := record.Validate(service.policy); err != nil {
			errs = append(errs, "memory: "+err.Error())
			continue
		}
		if record.CreatedBy.Kind != MemoryActorParent {
			errs = append(errs, "memory record must be parent-created")
		}
		if seen["memory"][record.ID] {
			errs = append(errs, "duplicate memory "+record.ID)
		}
		seen["memory"][record.ID] = true
		list, err := service.Memory.Retrieve(ctx, MemoryQuery{Actor: request.Actor, Namespace: MemoryNamespaceDurableMemory, Text: record.ID, Limit: 1})
		if err == nil {
			for _, current := range list.Records {
				if current.ID == record.ID {
					conflicts = append(conflicts, MemoryImportConflict{Category: "memory", ID: record.ID, Reason: "memory record already exists"})
				}
			}
		}
	}
	proposalIDs := map[string]bool{}
	for _, proposal := range export.Proposals {
		if err := proposal.Validate(service.policy); err != nil {
			errs = append(errs, "proposal: "+err.Error())
			continue
		}
		if seen["proposals"][proposal.ID] {
			errs = append(errs, "duplicate proposal "+proposal.ID)
		}
		seen["proposals"][proposal.ID] = true
		proposalIDs[proposal.ID] = true
		if local, ok := service.Proposals.(*LocalMemoryProposalSink); ok {
			if _, found, err := local.findProposal(ctx, proposal.ID); err != nil && !errors.Is(err, ErrMemoryNotFound) {
				errs = append(errs, err.Error())
			} else if found {
				conflicts = append(conflicts, MemoryImportConflict{Category: "proposal", ID: proposal.ID, Reason: "proposal already exists"})
			}
		}
	}
	seenReviews := map[string]bool{}
	for _, review := range export.ProposalReviews {
		if err := validateImportedReview(review); err != nil {
			errs = append(errs, "proposal review: "+err.Error())
			continue
		}
		if seenReviews[review.ProposalID] {
			errs = append(errs, "duplicate proposal review "+review.ProposalID)
		}
		seenReviews[review.ProposalID] = true
		if !proposalIDs[review.ProposalID] {
			errs = append(errs, "proposal review references unknown proposal "+review.ProposalID)
		}
	}
	if len(errs) > MaxMemoryImportErrors {
		errs = errs[:MaxMemoryImportErrors]
	}
	return conflicts, errs
}
func validateImportedReview(review MemoryProposalReview) error {
	if err := validateSafeID("proposal review proposal_id", review.ProposalID, MaxMemoryIDLength); err != nil {
		return err
	}
	if review.Status != MemoryProposalStatus(MaxMemoryProposalApproved) && review.Status != MemoryProposalStatus(MaxMemoryProposalRejected) {
		return errors.New("proposal review status is invalid")
	}
	if err := validateSafeID("proposal review decision_id", review.DecisionID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := validateText("proposal review reason", review.Reason, MaxMemoryReasonLength, true); err != nil {
		return err
	}
	if err := review.Actor.Validate(); err != nil {
		return err
	}
	if review.Actor.Kind != MemoryActorParent && review.Actor.Kind != MemoryActorSystem {
		return errors.New("proposal review actor must be parent or system")
	}
	return validateTimestamp("proposal review at", review.At, true)
}

// applyImportAtomic stages a complete local import beside the live store and
// swaps the prepared directory into place only after every category replays
// successfully. Non-local stores are rejected rather than receiving a
// misleading partially-applied import.
func (service *LocalMemoryService) applyImportAtomic(ctx context.Context, export MemoryExport, request MemoryImportRequest) error {
	localMemory, memoryOK := service.Memory.(*LocalMemoryStore)
	localProposals, proposalsOK := service.Proposals.(*LocalMemoryProposalSink)
	_, identityOK := service.Identity.(*LocalIdentityStore)
	_, profilesOK := service.Profiles.(*LocalUserProfileStore)
	if !memoryOK || !proposalsOK || !identityOK || !profilesOK {
		return errors.New("atomic memory import requires local memory, proposal, identity, and profile stores")
	}

	memoryLock, err := localMemory.AcquireWriterLock(ctx)
	if err != nil {
		return err
	}
	proposalLock, err := localProposals.AcquireWriterLock(ctx)
	if err != nil {
		_ = memoryLock.Release()
		return err
	}
	swapped := false
	defer func() {
		if !swapped {
			_ = proposalLock.Release()
			_ = memoryLock.Release()
		}
	}()

	// Revalidate under both locks so a concurrent process cannot invalidate the
	// conflict check between validation and staging.
	if conflicts, validationErrors := service.validateImport(ctx, export, request); len(conflicts) != 0 || len(validationErrors) != 0 {
		return ErrMemoryImportConflict
	}

	root := localMemory.Root()
	token, err := randomToken()
	if err != nil {
		return err
	}
	stageRoot := root + ".import-" + token
	backupRoot := root + ".backup-" + token
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		return fmt.Errorf("create import staging root: %w", err)
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageRoot)
		}
	}()
	if err := copyLocalMemoryRoot(root, stageRoot); err != nil {
		return err
	}
	staged, err := NewLocalMemoryService(stageRoot, service.policy)
	if err != nil {
		return fmt.Errorf("open import staging store: %w", err)
	}
	if err := staged.applyImportDirect(ctx, export, request); err != nil {
		return fmt.Errorf("stage memory import: %w", err)
	}
	if err := staged.validateLocalState(ctx); err != nil {
		return fmt.Errorf("validate staged memory import: %w", err)
	}

	if err := os.Rename(root, backupRoot); err != nil {
		return fmt.Errorf("reserve live memory root: %w", err)
	}
	if err := os.Rename(stageRoot, root); err != nil {
		_ = os.Rename(backupRoot, root)
		return fmt.Errorf("activate staged memory root: %w", err)
	}
	cleanupStage = false
	swapped = true
	localMemory.invalidateReplayCache()

	// The lock files moved with the old root. Point the lock handles at the
	// backup copy so explicit release removes only the old lock metadata.
	memoryLock.path = filepath.Join(backupRoot, memoryWriterLockFile)
	proposalLock.path = filepath.Join(backupRoot, proposalWriterLockFile)
	if err := memoryLock.Release(); err != nil {
		return fmt.Errorf("release import memory lock: %w", err)
	}
	if err := proposalLock.Release(); err != nil {
		return fmt.Errorf("release import proposal lock: %w", err)
	}
	if err := syncDirectory(filepath.Dir(root)); err != nil {
		return fmt.Errorf("sync import root parent: %w", err)
	}
	if err := os.RemoveAll(backupRoot); err != nil {
		return fmt.Errorf("remove import backup: %w", err)
	}
	return nil
}

func (service *LocalMemoryService) validateLocalState(ctx context.Context) error {
	if local, ok := service.Memory.(*LocalMemoryStore); ok {
		if _, err := local.ReplayDiagnostics(); err != nil {
			return err
		}
	}
	if local, ok := service.Proposals.(*LocalMemoryProposalSink); ok {
		if _, _, err := local.listWithReviews(ctx); err != nil {
			return err
		}
	}
	return nil
}

func copyLocalMemoryRoot(source, destination string) error {
	if err := os.MkdirAll(filepath.Join(destination, memoryProfilesDir), 0o700); err != nil {
		return fmt.Errorf("create staged profile directory: %w", err)
	}
	for _, name := range []string{memoryEventsFile, memoryProposalsFile, memoryIdentityFile, memoryProposalReviewsFile} {
		if err := copyOptionalPrivateFile(filepath.Join(source, name), filepath.Join(destination, name), MaxMemoryBytes); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(source, memoryProfilesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read profile storage: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := copyOptionalPrivateFile(filepath.Join(source, memoryProfilesDir, entry.Name()), filepath.Join(destination, memoryProfilesDir, entry.Name()), memoryMaxSnapshotSize); err != nil {
			return err
		}
	}
	return nil
}

func copyOptionalPrivateFile(source, destination string, maxBytes int) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect import source %s: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe import source file %s", source)
	}
	if info.Size() > int64(maxBytes) {
		return fmt.Errorf("import source file %s exceeds %d bytes", source, maxBytes)
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	_ = file.Close()
	if readErr != nil {
		return fmt.Errorf("read import source %s: %w", source, readErr)
	}
	if len(data) > maxBytes {
		return fmt.Errorf("import source file %s exceeds %d bytes", source, maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (service *LocalMemoryService) applyImportDirect(ctx context.Context, export MemoryExport, request MemoryImportRequest) error {
	if export.Identity != nil {
		if err := service.Identity.Save(ctx, *export.Identity); err != nil {
			return err
		}
	}
	for _, profile := range export.Profiles {
		if err := service.Profiles.Save(ctx, profile); err != nil {
			return err
		}
	}
	for _, record := range export.Memory {
		event := MemoryEvent{ID: importEventID(request.AuditID, record.ID), Type: MemoryEventImported, At: time.Now().UTC(), Actor: request.Actor, Record: record, RecordID: record.ID, Mode: MemoryWriteExplicit, Explicit: true, NewVersion: record.Version, Reason: request.Reason + "; imported"}
		if err := service.Memory.Append(ctx, event); err != nil {
			return err
		}
	}
	for _, proposal := range export.Proposals {
		if err := service.Proposals.SubmitProposal(ctx, proposal); err != nil {
			return err
		}
	}
	for _, review := range export.ProposalReviews {
		if err := service.appendReview(ctx, review); err != nil {
			return err
		}
	}
	return nil
}
func importEventID(auditID, recordID string) string {
	sum := sha256.Sum256([]byte(auditID + "\x00" + recordID))
	return "import-" + hex.EncodeToString(sum[:])[:32]
}

func sortExport(export *MemoryExport) {
	sort.Slice(export.Profiles, func(i, j int) bool { return export.Profiles[i].ID < export.Profiles[j].ID })
	sort.Slice(export.Memory, func(i, j int) bool { return export.Memory[i].ID < export.Memory[j].ID })
	sort.Slice(export.Proposals, func(i, j int) bool { return export.Proposals[i].ID < export.Proposals[j].ID })
	sort.Slice(export.ProposalReviews, func(i, j int) bool {
		return export.ProposalReviews[i].ProposalID < export.ProposalReviews[j].ProposalID
	})
}
func trimText(value string, max int) string {
	if len(value) > max {
		return value[:max]
	}
	return value
}

const memoryProposalReviewsFile = "proposal_reviews.jsonl"

type localProposalState struct {
	proposal MemoryProposal
	review   MemoryProposalReview
}

func (service *LocalMemoryService) loadProposals(ctx context.Context) ([]MemoryProposal, map[string]MemoryProposalReview, error) {
	if local, ok := service.Proposals.(*LocalMemoryProposalSink); ok {
		return local.listWithReviews(ctx)
	}
	return nil, nil, errors.New("proposal listing requires local proposal sink")
}
func (service *LocalMemoryService) appendReview(ctx context.Context, review MemoryProposalReview) error {
	local, ok := service.Proposals.(*LocalMemoryProposalSink)
	if !ok {
		return errors.New("proposal review requires local proposal sink")
	}
	return local.appendReview(ctx, review)
}
func (local *LocalMemoryProposalSink) findProposal(ctx context.Context, id string) (MemoryProposal, bool, error) {
	proposals, _, err := local.listWithReviews(ctx)
	if err != nil {
		return MemoryProposal{}, false, err
	}
	for _, proposal := range proposals {
		if proposal.ID == id {
			return proposal, true, nil
		}
	}
	return MemoryProposal{}, false, ErrMemoryNotFound
}
func (local *LocalMemoryProposalSink) listWithReviews(ctx context.Context) ([]MemoryProposal, map[string]MemoryProposalReview, error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	local.mu.mu.Lock()
	defer local.mu.mu.Unlock()
	if err := local.replayLocked(); err != nil {
		return nil, nil, err
	}
	proposals := make([]MemoryProposal, 0, len(local.proposalIDs))
	path := filepath.Join(local.root, memoryProposalsFile)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return proposals, map[string]MemoryProposalReview{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for {
		var proposal MemoryProposal
		err := decoder.Decode(&proposal)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("decode proposal list: %w", err)
		}
		proposals = append(proposals, proposal)
	}
	reviews, err := readProposalReviews(local.root)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(proposals, func(i, j int) bool { return proposals[i].ID < proposals[j].ID })
	return proposals, reviews, nil
}
func (local *LocalMemoryProposalSink) appendReview(ctx context.Context, review MemoryProposalReview) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := validateSafeID("proposal review proposal_id", review.ProposalID, MaxMemoryIDLength); err != nil {
		return err
	}
	if err := review.Actor.Validate(); err != nil {
		return err
	}
	if review.Status != MemoryProposalStatus(MaxMemoryProposalApproved) && review.Status != MemoryProposalStatus(MaxMemoryProposalRejected) {
		return errors.New("proposal review status is invalid")
	}
	if err := validateText("proposal review reason", review.Reason, MaxMemoryReasonLength, true); err != nil {
		return err
	}
	local.mu.mu.Lock()
	defer local.mu.mu.Unlock()
	lock, err := (&LocalMemoryStore{root: local.root, policy: local.policy, mu: local.mu}).acquireFileLockLocked(ctx, proposalWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	reviews, err := readProposalReviews(local.root)
	if err != nil {
		return err
	}
	if existing, ok := reviews[review.ProposalID]; ok && existing.Status != "" {
		return ErrMemoryConflict
	}
	path := filepath.Join(local.root, memoryProposalReviewsFile)
	file, err := openPrivateAppend(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, _ := json.Marshal(review)
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}
func readProposalReviews(root string) (map[string]MemoryProposalReview, error) {
	reviews := map[string]MemoryProposalReview{}
	path := filepath.Join(root, memoryProposalReviewsFile)
	if err := validatePrivateMetadata(path, false, MaxMemoryBytes); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return nil, fmt.Errorf("validate proposal review log: %w", err)
		}
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return reviews, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := readBoundedLine(reader, memoryMaxEventLine)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read proposal review line %d: %w", lineNumber, readErr)
		}
		var review MemoryProposalReview
		if err := json.Unmarshal(line, &review); err != nil {
			return nil, fmt.Errorf("decode proposal review line %d: %w", lineNumber, err)
		}
		if err := validateImportedReview(review); err != nil {
			return nil, fmt.Errorf("validate proposal review line %d: %w", lineNumber, err)
		}
		if _, exists := reviews[review.ProposalID]; exists {
			return nil, fmt.Errorf("duplicate proposal review %q", review.ProposalID)
		}
		reviews[review.ProposalID] = review
	}
	return reviews, nil
}

func loadAllProfiles(ctx context.Context, store *LocalUserProfileStore) ([]UserProfile, error) {
	entries, err := os.ReadDir(filepath.Join(store.root, memoryProfilesDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var profiles []UserProfile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		profile, err := store.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles, nil
}

// MemoryExportJSON is a deterministic, bounded export helper.
func MemoryExportJSON(export MemoryExport) ([]byte, error) {
	sortExport(&export)
	data, err := CanonicalJSON(export)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMemoryExportBytes {
		return nil, errors.New("memory export exceeds bounded size")
	}
	return data, nil
}

type memoryNoopTime struct{}

var _ = memoryNoopTime{}
