package bee

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionID is a stable identifier for one append-only conversation history.
type SessionID string

// ProjectID is a stable, path-safe identifier for a named workspace.
type ProjectID string

// EventType identifies the typed records in a session's JSONL history.
type EventType string

const (
	EventLifecycle        EventType = "lifecycle"
	EventMessage          EventType = "message"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventPlan             EventType = "plan"
	EventPlanUpdate       EventType = "plan_update"
	EventPlanReset        EventType = "plan_reset"
	EventPlanMode         EventType = "plan_mode"
	EventApproval         EventType = "approval"
	EventAudit            EventType = "audit"
	EventUsage            EventType = "usage"
	EventReasoning        EventType = "reasoning"
	EventArtifact         EventType = "artifact"
	EventCompaction       EventType = "compaction"
	EventCheckpoint       EventType = "turn_checkpoint"
	EventCheckpointResume EventType = "turn_checkpoint_resume"
	EventCacheState       EventType = "cache_state"
	EventTimeSync         EventType = "time_sync"
	EventHookObservation  EventType = "hook_observation"
)

const MaxTimeSyncZoneBytes = 128

// LifecycleAction describes session state transitions.
type LifecycleAction string

const (
	LifecycleCreated LifecycleAction = "created"
	LifecycleForked  LifecycleAction = "forked"
	LifecycleClosed  LifecycleAction = "closed"
)

// SessionMetadata is intentionally limited to non-secret session attributes.
type SessionMetadata struct {
	ID          SessionID `json:"id,omitempty"`
	Title       string    `json:"title,omitempty"`
	Pinned      bool      `json:"pinned"`
	ProjectID   ProjectID `json:"project_id,omitempty"`
	ProjectRoot string    `json:"project_root,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	ParentID    SessionID `json:"parent_id,omitempty"`
	ParentEvent uint64    `json:"parent_event,omitempty"`
	// TitleSource records that Bee tried to title the session itself:
	// "auto" (the title was generated) or "auto_failed". Either way it is
	// never tried again, and a title set any other way is never replaced.
	TitleSource string `json:"title_source,omitempty"`
}

// SessionEvent payload types keep event contracts explicit and provider-neutral.
type MessageEvent struct {
	Message Message `json:"message"`
}
type ToolCallEvent struct {
	Call ToolCall `json:"call"`
}
type ToolResultEvent struct {
	CallID            string `json:"call_id,omitempty"`
	ToolName          string `json:"tool_name,omitempty"`
	EffectiveToolName string `json:"effective_tool_name,omitempty"`
	Output            string `json:"output,omitempty"`
	Error             string `json:"error,omitempty"`
}
type PlanEvent struct {
	Plan TaskPlan `json:"plan"`
}
type PlanResetEvent struct {
	Reason string `json:"reason,omitempty"`
}
type PlanModeEvent struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}
type ApprovalEvent struct {
	ID       string           `json:"id,omitempty"`
	Request  ApprovalRequest  `json:"request"`
	Decision ApprovalDecision `json:"decision,omitempty"`
	Status   ApprovalStatus   `json:"status,omitempty"`
	Reason   string           `json:"reason,omitempty"`
}
type AuditEventPayload struct {
	Audit AuditEvent `json:"audit"`
}
type HookObservationEvent struct {
	Observation HookObservation `json:"observation"`
}
type UsageEvent struct {
	Provider string              `json:"provider,omitempty"`
	Model    string              `json:"model,omitempty"`
	Usage    Usage               `json:"usage"`
	Timing   *ProviderCallTiming `json:"timing,omitempty"`
}

// TimeSyncEvent is a bounded provider-neutral clock observation. It is
// projected into a trusted tail control record, not visible chat history.
type TimeSyncEvent struct {
	UTC      string `json:"utc"`
	Local    string `json:"local"`
	Weekday  string `json:"weekday,omitempty"`
	TimeZone string `json:"time_zone"`
}

// CacheStateEvent is content-free durable telemetry for validating that a
// reconstructed agent still extends the provider-visible prefix. It stores
// hashes and byte counts only; prompt or tool-result content is never copied.
type CacheStateEvent struct {
	Fingerprint  string `json:"fingerprint"`
	Bytes        int    `json:"bytes"`
	Epoch        uint64 `json:"epoch"`
	ModelHash    string `json:"model_hash,omitempty"`
	TemplateHash string `json:"template_hash,omitempty"`
	ToolsHash    string `json:"tools_hash,omitempty"`
	HistoryHash  string `json:"history_hash,omitempty"`
	ChangeReason string `json:"change_reason,omitempty"`
}

// ReasoningEvent is private, append-only provider telemetry. Content is
// bounded explicitly; truncation is represented rather than silent.
type ReasoningEvent struct {
	RunID           string       `json:"run_id,omitempty"`
	Provider        string       `json:"provider,omitempty"`
	Profile         string       `json:"profile,omitempty"`
	Role            ProviderRole `json:"role,omitempty"`
	Model           string       `json:"model,omitempty"`
	Step            int          `json:"step"`
	Content         string       `json:"content,omitempty"`
	Complete        bool         `json:"complete"`
	Incomplete      bool         `json:"incomplete,omitempty"`
	FinishReason    string       `json:"finish_reason,omitempty"`
	ErrorClass      string       `json:"error_class,omitempty"`
	Truncated       bool         `json:"truncated,omitempty"`
	OriginalBytes   int          `json:"original_bytes,omitempty"`
	VisibleContent  bool         `json:"visible_content,omitempty"`
	ToolName        string       `json:"tool_name,omitempty"`
	ArgumentBytes   int          `json:"argument_bytes,omitempty"`
	RecoveryAttempt int          `json:"recovery_attempt,omitempty"`
	Retrying        bool         `json:"retrying,omitempty"`
}
type CompactionEvent struct {
	Through       uint64    `json:"through"`
	Summary       string    `json:"summary"`
	SourceSHA256  string    `json:"source_sha256,omitempty"`
	SummarySHA256 string    `json:"summary_sha256,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Profile       string    `json:"profile,omitempty"`
	Model         string    `json:"model,omitempty"`
	Method        string    `json:"method,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
}

// TurnStopReason identifies a controlled boundary rather than a failed turn.
// Checkpoints remain provider-neutral and contain references, not raw tool
// output, so adapters can safely offer an explicit continuation action.
type TurnStopReason string

const (
	TurnStopToolBudget   TurnStopReason = "tool_budget"
	TurnStopDeadline     TurnStopReason = "deadline"
	TurnStopNoProgress   TurnStopReason = "no_progress"
	TurnStopPlanEvidence TurnStopReason = "plan_evidence"
)

type CheckpointEvidence struct {
	CallID                 string `json:"call_id,omitempty"`
	ToolName               string `json:"tool_name"`
	Outcome                string `json:"outcome"`
	Target                 string `json:"target,omitempty"`
	CallFingerprint        string `json:"call_fingerprint,omitempty"`
	ObservationFingerprint string `json:"observation_fingerprint,omitempty"`
}

// CheckpointPlan is the bounded active-plan projection carried across a
// controlled stop. It gives a resumed model an authoritative progress anchor
// without relying on the older conversation projection surviving compaction.
type CheckpointPlan struct {
	ID          string `json:"id"`
	Goal        string `json:"goal,omitempty"`
	UpdatedSeq  uint64 `json:"updated_seq,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Pending     int    `json:"pending,omitempty"`
	InProgress  int    `json:"in_progress,omitempty"`
	Passed      int    `json:"passed,omitempty"`
	Failed      int    `json:"failed,omitempty"`
	Blocked     int    `json:"blocked,omitempty"`
}

// TurnCheckpoint is the durable continuation contract emitted after Bee has
// used a reserved completion with tool execution disabled to land a bounded
// segment while retaining a cache-stable tool-schema prefix.
type TurnCheckpoint struct {
	ID               string               `json:"id"`
	RunID            string               `json:"run_id,omitempty"`
	WhyStopped       TurnStopReason       `json:"why_stopped"`
	Summary          string               `json:"summary,omitempty"`
	CompletedWork    []string             `json:"completed_work,omitempty"`
	VerifiedFindings []string             `json:"verified_findings,omitempty"`
	UnresolvedWork   []string             `json:"unresolved_work,omitempty"`
	Blockers         []string             `json:"blockers,omitempty"`
	NextAction       string               `json:"next_action"`
	Plan             *CheckpointPlan      `json:"plan,omitempty"`
	Evidence         []CheckpointEvidence `json:"evidence,omitempty"`
	ToolStepsUsed    int                  `json:"tool_steps_used"`
	SegmentsUsed     int                  `json:"segments_used"`
	CreatedAt        time.Time            `json:"created_at"`
}
type TurnCheckpointResumeEvent struct {
	CheckpointID string `json:"checkpoint_id"`
}
type LifecycleEvent struct {
	Action LifecycleAction `json:"action"`
	Reason string          `json:"reason,omitempty"`
}

// Event is the append-only source-of-truth record. Data contains exactly one
// of the typed payloads above, encoded as JSON.
type Event struct {
	ID        string          `json:"id"`
	SessionID SessionID       `json:"session_id"`
	Seq       uint64          `json:"seq"`
	Type      EventType       `json:"type"`
	At        time.Time       `json:"at"`
	Data      json.RawMessage `json:"data"`
}

func newEvent(sessionID SessionID, typ EventType, payload any) (Event, error) {
	if strings.TrimSpace(string(sessionID)) == "" {
		return Event{}, errors.New("session id is required")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event: %w", typ, err)
	}
	return Event{ID: newStableID(), SessionID: sessionID, Type: typ, At: time.Now().UTC(), Data: data}, nil
}

func NewMessageEvent(sessionID SessionID, message Message) (Event, error) {
	return newEvent(sessionID, EventMessage, MessageEvent{Message: message})
}

// DecodeMessageEvent decodes one provider-neutral message event for durable
// history projections.
func DecodeMessageEvent(event Event) (MessageEvent, error) {
	return decodeEventPayload[MessageEvent](event, EventMessage)
}

// NewTimeSyncPayload snapshots one instant in UTC and the configured IANA
// zone. Second precision keeps the durable/provider record constant-sized.
func NewTimeSyncPayload(now time.Time, timeZone string) (TimeSyncEvent, error) {
	timeZone = strings.TrimSpace(timeZone)
	if timeZone == "" {
		return TimeSyncEvent{}, errors.New("time sync time zone is required")
	}
	if len(timeZone) > MaxTimeSyncZoneBytes {
		return TimeSyncEvent{}, fmt.Errorf("time sync time zone exceeds %d bytes", MaxTimeSyncZoneBytes)
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return TimeSyncEvent{}, fmt.Errorf("time sync time zone: %w", err)
	}
	utc := now.UTC().Truncate(time.Second)
	local := utc.In(location)
	payload := TimeSyncEvent{
		UTC:      utc.Format(time.RFC3339),
		Local:    local.Format(time.RFC3339),
		Weekday:  local.Weekday().String(),
		TimeZone: timeZone,
	}
	if err := payload.validate(); err != nil {
		return TimeSyncEvent{}, err
	}
	return payload, nil
}

func (payload TimeSyncEvent) validate() error {
	if strings.TrimSpace(payload.TimeZone) != payload.TimeZone || payload.TimeZone == "" || len(payload.TimeZone) > MaxTimeSyncZoneBytes {
		return errors.New("time sync time zone is invalid")
	}
	location, err := time.LoadLocation(payload.TimeZone)
	if err != nil {
		return fmt.Errorf("time sync time zone: %w", err)
	}
	utc, err := time.Parse(time.RFC3339, payload.UTC)
	if err != nil {
		return fmt.Errorf("time sync UTC timestamp: %w", err)
	}
	_, offset := utc.Zone()
	if offset != 0 || payload.UTC != utc.UTC().Format(time.RFC3339) {
		return errors.New("time sync UTC timestamp must be canonical UTC RFC3339")
	}
	local, err := time.Parse(time.RFC3339, payload.Local)
	if err != nil {
		return fmt.Errorf("time sync local timestamp: %w", err)
	}
	if !local.Equal(utc) || payload.Local != utc.In(location).Format(time.RFC3339) {
		return errors.New("time sync local timestamp does not match UTC and time zone")
	}
	if payload.Weekday != "" && payload.Weekday != utc.In(location).Weekday().String() {
		return errors.New("time sync weekday does not match local timestamp and time zone")
	}
	return nil
}

func NewTimeSyncEvent(sessionID SessionID, payload TimeSyncEvent) (Event, error) {
	if err := payload.validate(); err != nil {
		return Event{}, err
	}
	return newEvent(sessionID, EventTimeSync, payload)
}

func DecodeTimeSyncEvent(event Event) (TimeSyncEvent, error) {
	payload, err := decodeEventPayload[TimeSyncEvent](event, EventTimeSync)
	if err != nil {
		return TimeSyncEvent{}, err
	}
	if err := payload.validate(); err != nil {
		return TimeSyncEvent{}, err
	}
	return payload, nil
}

func timeSyncControlMessage(payload TimeSyncEvent) (Message, error) {
	if err := payload.validate(); err != nil {
		return Message{}, err
	}
	var rendered strings.Builder
	rendered.WriteString(`<bee-clock utc="`)
	if err := xml.EscapeText(&rendered, []byte(payload.UTC)); err != nil {
		return Message{}, err
	}
	rendered.WriteString(`" local="`)
	if err := xml.EscapeText(&rendered, []byte(payload.Local)); err != nil {
		return Message{}, err
	}
	if payload.Weekday != "" {
		rendered.WriteString(`" weekday="`)
		if err := xml.EscapeText(&rendered, []byte(payload.Weekday)); err != nil {
			return Message{}, err
		}
	}
	rendered.WriteString(`" tz="`)
	if err := xml.EscapeText(&rendered, []byte(payload.TimeZone)); err != nil {
		return Message{}, err
	}
	rendered.WriteString(`"/>`)
	return Message{Role: RoleSystem, Content: rendered.String()}, nil
}
func NewToolCallEvent(sessionID SessionID, call ToolCall) (Event, error) {
	return newEvent(sessionID, EventToolCall, ToolCallEvent{Call: call})
}

// DecodeToolCallEvent decodes one provider-neutral tool-call event for
// durable history projections.
func DecodeToolCallEvent(event Event) (ToolCallEvent, error) {
	return decodeEventPayload[ToolCallEvent](event, EventToolCall)
}
func NewToolResultEvent(sessionID SessionID, result ToolResultEvent) (Event, error) {
	return newEvent(sessionID, EventToolResult, result)
}

// DecodeToolResultEvent decodes one provider-neutral tool-result event for
// durable history projections.
func DecodeToolResultEvent(event Event) (ToolResultEvent, error) {
	return decodeEventPayload[ToolResultEvent](event, EventToolResult)
}
func NewPlanEvent(sessionID SessionID, plan TaskPlan) (Event, error) {
	return newEvent(sessionID, EventPlan, PlanEvent{Plan: plan})
}
func NewPlanUpdateEvent(sessionID SessionID, update PlanUpdate) (Event, error) {
	return newEvent(sessionID, EventPlanUpdate, PlanUpdateEvent{Update: update})
}
func NewPlanResetEvent(sessionID SessionID, reason string) (Event, error) {
	return newEvent(sessionID, EventPlanReset, PlanResetEvent{Reason: reason})
}
func NewPlanModeEvent(sessionID SessionID, enabled bool, reason string) (Event, error) {
	return newEvent(sessionID, EventPlanMode, PlanModeEvent{Enabled: enabled, Reason: reason})
}
func NewApprovalEvent(sessionID SessionID, approval ApprovalEvent) (Event, error) {
	return newEvent(sessionID, EventApproval, approval)
}
func NewAuditEvent(sessionID SessionID, audit AuditEvent) (Event, error) {
	return newEvent(sessionID, EventAudit, AuditEventPayload{Audit: audit})
}
func NewHookObservationEvent(sessionID SessionID, observation HookObservation) (Event, error) {
	return newEvent(sessionID, EventHookObservation, HookObservationEvent{Observation: observation})
}

func DecodeHookObservationEvent(event Event) (HookObservationEvent, error) {
	return decodeEventPayload[HookObservationEvent](event, EventHookObservation)
}
func NewUsageEvent(sessionID SessionID, provider, model string, usage Usage) (Event, error) {
	return newEvent(sessionID, EventUsage, UsageEvent{Provider: provider, Model: model, Usage: usage})
}
func NewCacheStateEvent(sessionID SessionID, state CacheStateEvent) (Event, error) {
	if state.Bytes <= 0 || state.Epoch == 0 || len(state.Fingerprint) != 64 {
		return Event{}, errors.New("cache state requires a fingerprint, positive byte length, and epoch")
	}
	for _, hash := range []string{state.ModelHash, state.TemplateHash, state.ToolsHash, state.HistoryHash} {
		if hash != "" && len(hash) != 64 {
			return Event{}, errors.New("cache state component hash must be empty or 64 bytes")
		}
	}
	state.ChangeReason = truncateUTF8(strings.TrimSpace(state.ChangeReason), 256)
	return newEvent(sessionID, EventCacheState, state)
}

func latestSessionCacheState(events []Event) (CacheStateEvent, bool, error) {
	var latest CacheStateEvent
	found := false
	for _, event := range events {
		if event.Type != EventCacheState {
			continue
		}
		state, err := decodeEventPayload[CacheStateEvent](event, EventCacheState)
		if err != nil {
			return CacheStateEvent{}, false, err
		}
		latest = state
		found = true
	}
	return latest, found, nil
}
func NewReasoningEvent(sessionID SessionID, reasoning ReasoningEvent) (Event, error) {
	if len(reasoning.Content) > MaxReasoningBytes {
		reasoning.OriginalBytes = len(reasoning.Content)
		reasoning.Content = truncateUTF8(reasoning.Content, MaxReasoningBytes)
		reasoning.Truncated = true
	}
	return newEvent(sessionID, EventReasoning, reasoning)
}

func DecodeReasoningEvent(event Event) (ReasoningEvent, error) {
	return decodeEventPayload[ReasoningEvent](event, EventReasoning)
}
func NewArtifactEvent(sessionID SessionID, artifact Artifact) (Event, error) {
	return newEvent(sessionID, EventArtifact, ArtifactEvent{Artifact: artifact})
}

// DecodeArtifactEvent decodes one provider-neutral artifact event for durable
// history projections.
func DecodeArtifactEvent(event Event) (ArtifactEvent, error) {
	return decodeEventPayload[ArtifactEvent](event, EventArtifact)
}
func NewCompactionEvent(sessionID SessionID, through uint64, summary string) (Event, error) {
	return NewCompactionEventWithProvenance(sessionID, CompactionEvent{Through: through, Summary: summary})
}

func NewCompactionEventWithProvenance(sessionID SessionID, compaction CompactionEvent) (Event, error) {
	if strings.TrimSpace(compaction.Summary) == "" {
		return Event{}, errors.New("compaction summary is required")
	}
	return newEvent(sessionID, EventCompaction, compaction)
}

func NewTurnCheckpointEvent(sessionID SessionID, checkpoint TurnCheckpoint) (Event, error) {
	if strings.TrimSpace(checkpoint.ID) == "" {
		checkpoint.ID = newStableID()
	}
	if len(checkpoint.ID) > 256 {
		return Event{}, errors.New("turn checkpoint id exceeds 256 bytes")
	}
	switch checkpoint.WhyStopped {
	case TurnStopToolBudget, TurnStopDeadline, TurnStopNoProgress, TurnStopPlanEvidence:
	default:
		return Event{}, errors.New("turn checkpoint stop reason is invalid")
	}
	if strings.TrimSpace(checkpoint.NextAction) == "" {
		return Event{}, errors.New("turn checkpoint next action is required")
	}
	checkpoint.Summary = truncateUTF8(checkpoint.Summary, 16<<10)
	checkpoint.NextAction = truncateUTF8(checkpoint.NextAction, 1024)
	checkpoint.CompletedWork = boundedCheckpointStrings(checkpoint.CompletedWork)
	checkpoint.VerifiedFindings = boundedCheckpointStrings(checkpoint.VerifiedFindings)
	checkpoint.UnresolvedWork = boundedCheckpointStrings(checkpoint.UnresolvedWork)
	checkpoint.Blockers = boundedCheckpointStrings(checkpoint.Blockers)
	if len(checkpoint.Evidence) > 64 {
		checkpoint.Evidence = checkpoint.Evidence[:64]
	}
	for index := range checkpoint.Evidence {
		checkpoint.Evidence[index].CallID = truncateUTF8(checkpoint.Evidence[index].CallID, 256)
		checkpoint.Evidence[index].ToolName = truncateUTF8(checkpoint.Evidence[index].ToolName, 256)
		checkpoint.Evidence[index].Outcome = truncateUTF8(checkpoint.Evidence[index].Outcome, 64)
		checkpoint.Evidence[index].Target = truncateUTF8(checkpoint.Evidence[index].Target, 512)
		checkpoint.Evidence[index].CallFingerprint = truncateUTF8(checkpoint.Evidence[index].CallFingerprint, 64)
		checkpoint.Evidence[index].ObservationFingerprint = truncateUTF8(checkpoint.Evidence[index].ObservationFingerprint, 64)
	}
	if checkpoint.Plan != nil {
		checkpoint.Plan.ID = truncateUTF8(strings.TrimSpace(checkpoint.Plan.ID), 256)
		checkpoint.Plan.Goal = truncateUTF8(strings.TrimSpace(checkpoint.Plan.Goal), 1024)
		checkpoint.Plan.Fingerprint = truncateUTF8(strings.TrimSpace(checkpoint.Plan.Fingerprint), 64)
		if checkpoint.Plan.ID == "" {
			checkpoint.Plan = nil
		}
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = time.Now().UTC()
	}
	return newEvent(sessionID, EventCheckpoint, checkpoint)
}

func NewTurnCheckpointResumeEvent(sessionID SessionID, checkpointID string) (Event, error) {
	if strings.TrimSpace(checkpointID) == "" {
		return Event{}, errors.New("turn checkpoint id is required")
	}
	if len(checkpointID) > 256 {
		return Event{}, errors.New("turn checkpoint id exceeds 256 bytes")
	}
	return newEvent(sessionID, EventCheckpointResume, TurnCheckpointResumeEvent{CheckpointID: checkpointID})
}

func boundedCheckpointStrings(values []string) []string {
	if len(values) > 64 {
		values = values[:64]
	}
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = truncateUTF8(value, 1024)
	}
	return result
}

func DecodeTurnCheckpointEvent(event Event) (TurnCheckpoint, error) {
	return decodeEventPayload[TurnCheckpoint](event, EventCheckpoint)
}
func NewLifecycleEvent(sessionID SessionID, action LifecycleAction, reason string) (Event, error) {
	return newEvent(sessionID, EventLifecycle, LifecycleEvent{Action: action, Reason: reason})
}

// Session is an in-memory view backed by a SessionStore. Events remain the
// source of truth; Messages is always derived when requested.
type Session struct {
	ID       SessionID
	Metadata SessionMetadata
	Events   []Event
	store    *SessionStore
	mu       sync.RWMutex
}

func (session *Session) Append(event Event) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	stored, err := session.store.Append(session.ID, event)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.Events = append(session.Events, stored)
	session.Metadata.UpdatedAt = stored.At
	session.mu.Unlock()
	return nil
}

// ReplaceMetadata appends a complete safe metadata snapshot without rewriting
// history. The stable ID and creation timestamp remain owned by the existing
// session, while title, pin state, project association, and root are replaced.
func (session *Session) ReplaceMetadata(metadata SessionMetadata) error {
	return session.replaceMetadataAt(metadata, time.Time{})
}

// replaceMetadataAt is ReplaceMetadata with the metadata event stamped at
// at (zero means now). A backfill passes the session's last event time so
// the change does not move an old session to the top of the listing.
func (session *Session) replaceMetadataAt(metadata SessionMetadata, at time.Time) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	current := metadata
	if current.ProjectRoot != "" {
		root, err := filepath.Abs(current.ProjectRoot)
		if err != nil {
			return fmt.Errorf("resolve project root: %w", err)
		}
		current.ProjectRoot = filepath.Clean(root)
	}
	current.ID = session.ID
	if current.CreatedAt.IsZero() {
		current.CreatedAt = session.Metadata.CreatedAt
	}
	if current.CreatedAt.IsZero() {
		current.CreatedAt = time.Now().UTC()
	}
	current.UpdatedAt = time.Now().UTC()
	if !at.IsZero() {
		current.UpdatedAt = at.UTC()
	}
	event, err := newMetadataEvent(session.ID, current)
	if err != nil {
		return err
	}
	if !at.IsZero() {
		event.At = at.UTC()
	}
	if err := session.Append(event); err != nil {
		return err
	}
	session.mu.Lock()
	session.Metadata = current
	session.mu.Unlock()
	return nil
}

// UpdateMetadata appends a safe metadata snapshot without rewriting history.
// Callers may update the human-facing title/project association/root and set
// Pinned true; use UpdatePinned to explicitly clear the pin state. The stable
// ID and creation timestamp remain owned by the existing session.
func (session *Session) UpdateMetadata(metadata SessionMetadata) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	current := session.Metadata
	if metadata.Title != "" {
		current.Title = metadata.Title
	}
	if metadata.ProjectID != "" {
		current.ProjectID = metadata.ProjectID
	}
	if metadata.ProjectRoot != "" {
		current.ProjectRoot = metadata.ProjectRoot
	}
	if metadata.Pinned {
		current.Pinned = true
	}
	return session.ReplaceMetadata(current)
}

// UpdatePinned durably changes the pin state while preserving all other
// metadata, including project association and title.
func (session *Session) UpdatePinned(pinned bool) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	current := session.Metadata
	current.Pinned = pinned
	return session.ReplaceMetadata(current)
}

// UpdateProjectID changes only the session's optional project association. It
// permits an empty ID so callers can safely unassign a session.
func (session *Session) UpdateProjectID(projectID ProjectID) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	current := session.Metadata
	current.ProjectID = projectID
	current.ID = session.ID
	if current.CreatedAt.IsZero() {
		current.CreatedAt = time.Now().UTC()
	}
	current.UpdatedAt = time.Now().UTC()
	event, err := newMetadataEvent(session.ID, current)
	if err != nil {
		return err
	}
	if err := session.Append(event); err != nil {
		return err
	}
	session.mu.Lock()
	session.Metadata = current
	session.mu.Unlock()
	return nil
}

func (session *Session) AppendMessage(message Message) error {
	event, err := NewMessageEvent(session.ID, message)
	if err != nil {
		return err
	}
	return session.Append(event)
}

// AppendMessages appends related message records as one contiguous batch.
// This is useful for synthetic turns whose user and assistant messages must
// remain paired in durable history under concurrent session activity.
func (session *Session) AppendMessages(messages ...Message) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	if len(messages) == 0 {
		return errors.New("at least one message is required")
	}
	events := make([]Event, 0, len(messages))
	for _, message := range messages {
		event, err := NewMessageEvent(session.ID, message)
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	stored, err := session.store.AppendEvents(session.ID, events)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.Events = append(session.Events, stored...)
	if len(stored) > 0 {
		session.Metadata.UpdatedAt = stored[len(stored)-1].At
	}
	session.mu.Unlock()
	return nil
}

// AppendMessageWithTimeSync persists one admitted user message and its clock
// observation as a contiguous batch. The user record deliberately precedes
// the clock so the synthesized control remains at the provider-context tail.
func (session *Session) AppendMessageWithTimeSync(message Message, timeSync TimeSyncEvent) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	if message.Role != RoleUser {
		return errors.New("time sync can only accompany a user message")
	}
	messageEvent, err := NewMessageEvent(session.ID, message)
	if err != nil {
		return err
	}
	timeEvent, err := NewTimeSyncEvent(session.ID, timeSync)
	if err != nil {
		return err
	}
	stored, err := session.store.AppendEvents(session.ID, []Event{messageEvent, timeEvent})
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.Events = append(session.Events, stored...)
	if len(stored) > 0 {
		session.Metadata.UpdatedAt = stored[len(stored)-1].At
	}
	session.mu.Unlock()
	return nil
}

func (session *Session) AppendToolCall(call ToolCall) error {
	event, err := NewToolCallEvent(session.ID, call)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendToolResult(result ToolResultEvent) error {
	event, err := NewToolResultEvent(session.ID, result)
	if err != nil {
		return err
	}
	return session.Append(event)
}

// AppendProviderFailure appends an incomplete visible assistant prefix and
// its private provider-step diagnostic as one contiguous durable transaction.
// The in-memory session view is updated only after the store append succeeds.
func (session *Session) AppendProviderFailure(message *Message, reasoning ReasoningEvent) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	events := make([]Event, 0, 2)
	if message != nil && message.Content != "" {
		messageCopy := *message
		messageCopy.Role = RoleAssistant
		messageCopy.Incomplete = true
		messageCopy.ToolCalls = nil
		event, err := NewMessageEvent(session.ID, messageCopy)
		if err != nil {
			return err
		}
		events = append(events, event)
	}
	reasoningEvent, err := NewReasoningEvent(session.ID, reasoning)
	if err != nil {
		return err
	}
	events = append(events, reasoningEvent)
	stored, err := session.store.AppendEvents(session.ID, events)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.Events = append(session.Events, stored...)
	if len(stored) > 0 {
		session.Metadata.UpdatedAt = stored[len(stored)-1].At
	}
	session.mu.Unlock()
	return nil
}
func (session *Session) AppendPlan(plan TaskPlan) error {
	event, err := NewPlanEvent(session.ID, plan)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendPlanUpdate(update PlanUpdate) error {
	event, err := NewPlanUpdateEvent(session.ID, update)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendPlanReset(reason string) error {
	event, err := NewPlanResetEvent(session.ID, reason)
	if err != nil {
		return err
	}
	return session.Append(event)
}

// AppendPlanMode records the effective mode without changing the current
// plan. It is used to pin a config-derived default to a durable session.
func (session *Session) AppendPlanMode(enabled bool, reason string) error {
	event, err := NewPlanModeEvent(session.ID, enabled, reason)
	if err != nil {
		return err
	}
	return session.Append(event)
}

// EnablePlanMode starts a fresh planning epoch and records the enabled mode as
// one contiguous append. The reset must precede the mode event so a resumed
// session can never reuse a plan from before the transition.
func (session *Session) EnablePlanMode(reason string) error {
	if session == nil || session.store == nil {
		return errors.New("session is not attached to a store")
	}
	reset, err := NewPlanResetEvent(session.ID, reason)
	if err != nil {
		return err
	}
	mode, err := NewPlanModeEvent(session.ID, true, reason)
	if err != nil {
		return err
	}
	stored, err := session.store.AppendEvents(session.ID, []Event{reset, mode})
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.Events = append(session.Events, stored...)
	if len(stored) > 0 {
		session.Metadata.UpdatedAt = stored[len(stored)-1].At
	}
	session.mu.Unlock()
	return nil
}
func (session *Session) AppendApproval(approval ApprovalEvent) error {
	event, err := NewApprovalEvent(session.ID, approval)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendAudit(audit AuditEvent) error {
	event, err := NewAuditEvent(session.ID, audit)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendHookObservation(observation HookObservation) error {
	event, err := NewHookObservationEvent(session.ID, observation)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) RecordUsage(provider, model string, usage Usage) error {
	return session.RecordUsageWithTiming(provider, model, usage, nil)
}

// RecordUsageWithTiming also keeps the call's performance telemetry.
func (session *Session) RecordUsageWithTiming(provider, model string, usage Usage, timing *ProviderCallTiming) error {
	event, err := newEvent(session.ID, EventUsage, UsageEvent{Provider: provider, Model: model, Usage: usage, Timing: timing})
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) RecordCacheState(state CacheStateEvent) error {
	event, err := NewCacheStateEvent(session.ID, state)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) AppendReasoning(reasoning ReasoningEvent) error {
	event, err := NewReasoningEvent(session.ID, reasoning)
	if err != nil {
		return err
	}
	return session.Append(event)
}
func (session *Session) Compact(through uint64, summary string) error {
	event, err := NewCompactionEvent(session.ID, through, summary)
	if err != nil {
		return err
	}
	return session.Append(event)
}

func (session *Session) CompactWithProvenance(compaction CompactionEvent) error {
	event, err := NewCompactionEventWithProvenance(session.ID, compaction)
	if err != nil {
		return err
	}
	return session.Append(event)
}

func (session *Session) AppendTurnCheckpoint(checkpoint TurnCheckpoint) (TurnCheckpoint, error) {
	event, err := NewTurnCheckpointEvent(session.ID, checkpoint)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	decoded, err := DecodeTurnCheckpointEvent(event)
	if err != nil {
		return TurnCheckpoint{}, err
	}
	if err := session.Append(event); err != nil {
		return TurnCheckpoint{}, err
	}
	return decoded, nil
}

func (session *Session) AppendTurnCheckpointResume(checkpointID string) error {
	event, err := NewTurnCheckpointResumeEvent(session.ID, checkpointID)
	if err != nil {
		return err
	}
	return session.Append(event)
}

// LatestPendingTurnCheckpoint returns the newest checkpoint that has not been
// selected for continuation. Resume events are append-only consumption marks;
// the source checkpoint remains available for audit and explicit replay.
func (session *Session) LatestPendingTurnCheckpoint() (TurnCheckpoint, error) {
	if session == nil {
		return TurnCheckpoint{}, errors.New("session is required")
	}
	events := session.EventsSnapshot()
	resumed := make(map[string]struct{})
	for _, event := range events {
		if event.Type != EventCheckpointResume {
			continue
		}
		payload, err := decodeEventPayload[TurnCheckpointResumeEvent](event, EventCheckpointResume)
		if err != nil {
			return TurnCheckpoint{}, err
		}
		resumed[payload.CheckpointID] = struct{}{}
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != EventCheckpoint {
			continue
		}
		checkpoint, err := DecodeTurnCheckpointEvent(events[index])
		if err != nil {
			return TurnCheckpoint{}, err
		}
		if _, used := resumed[checkpoint.ID]; !used {
			return checkpoint, nil
		}
	}
	return TurnCheckpoint{}, errors.New("no pending turn checkpoint")
}

func (session *Session) TurnCheckpoint(id string) (TurnCheckpoint, error) {
	if session == nil {
		return TurnCheckpoint{}, errors.New("session is required")
	}
	events := session.EventsSnapshot()
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != EventCheckpoint {
			continue
		}
		checkpoint, err := DecodeTurnCheckpointEvent(events[index])
		if err != nil {
			return TurnCheckpoint{}, err
		}
		if id == "" || checkpoint.ID == id {
			return checkpoint, nil
		}
	}
	return TurnCheckpoint{}, errors.New("turn checkpoint not found")
}
func (session *Session) Messages() ([]Message, error) {
	events := session.EventsSnapshot()
	return MessagesFromEvents(events)
}

func (session *Session) EventsSnapshot() []Event {
	if session == nil {
		return nil
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	return append([]Event(nil), session.Events...)
}

func newStableID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		raw[6] = (raw[6] & 0x0f) | 0x40
		raw[8] = (raw[8] & 0x3f) | 0x80
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func normalizeMetadata(metadata SessionMetadata) (SessionMetadata, error) {
	if metadata.ProjectRoot != "" {
		root, err := filepath.Abs(metadata.ProjectRoot)
		if err != nil {
			return SessionMetadata{}, fmt.Errorf("resolve project root: %w", err)
		}
		metadata.ProjectRoot = filepath.Clean(root)
	}
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now().UTC()
	}
	metadata.UpdatedAt = metadata.CreatedAt
	return metadata, nil
}

func decodeEventPayload[T any](event Event, expected EventType) (T, error) {
	var value T
	if event.Type != expected {
		return value, fmt.Errorf("event %s is not %s", event.Type, expected)
	}
	if err := json.Unmarshal(event.Data, &value); err != nil {
		return value, fmt.Errorf("decode %s event: %w", expected, err)
	}
	return value, nil
}
