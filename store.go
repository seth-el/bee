package bee

import (
	"bufio"
	"bytes"
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
	"unicode/utf8"
)

var (
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionBusy means another process or goroutine owns the complete turn
	// lease for this durable session. Individual JSONL appends remain separately
	// locked; this lease protects conversational ordering across a full run.
	ErrSessionBusy = errors.New("session already has an active turn")
)

const (
	MaxSessionSearchQueryBytes   = 256
	MaxSessionSearchResults      = 50
	MaxSessionSearchSnippetBytes = 240
	MaxSessionSearchMatches      = 3
)

// SessionStore is a small standard-library JSONL store. Each session has one
// append-only 0600 file under root.
type SessionStore struct {
	root string
	mu   sync.Mutex
}

const sessionIndexVersion = 2

// sessionIndex is a rebuildable cache for the JSONL source of truth. Search
// excerpts are redacted at the display boundary; the private sidecar may retain
// the faithful indexed text and therefore inherits session secrecy.
type sessionIndex struct {
	Version         int                    `json:"version"`
	ID              SessionID              `json:"id"`
	SourceSize      int64                  `json:"source_size"`
	SourceModTimeNS int64                  `json:"source_mod_time_ns"`
	Summary         SessionSummary         `json:"summary"`
	Search          []sessionIndexDocument `json:"search,omitempty"`
}

type sessionIndexDocument struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

func NewSessionStore(root string) (*SessionStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("session store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve session store root: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, fmt.Errorf("validate session store root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create session store root: %w", err)
	}
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, fmt.Errorf("revalidate session store root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat session store root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session store root must not be a symlink")
	}
	if !info.IsDir() {
		return nil, errors.New("session store root is not a directory")
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("set session store root permissions: %w", err)
	}
	return &SessionStore{root: filepath.Clean(abs)}, nil
}

func NewDefaultSessionStore() (*SessionStore, error) {
	if os.Getenv("BEE_HOME") != "" || os.Getenv("XDG_DATA_HOME") == "" {
		if _, err := EnsureBeeLayout(); err != nil {
			return nil, err
		}
	} else {
		// Legacy XDG_DATA_HOME installs keep sessions outside ~/.bee, but the
		// rest of Bee (workspace, memory, SOUL.md, BEE.md) still lives there and
		// still needs its private layout, e.g. after a workspace restored from
		// git comes back 0644. Best effort: a layout problem must not stop the
		// session store from opening; `bee doctor` reports what remains.
		_, _ = EnsureBeeLayout()
	}
	root, err := DefaultBeeSessionRoot()
	if err != nil {
		return nil, err
	}
	return NewSessionStore(root)
}

func defaultUserDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user data directory: %w", err)
	}
	if os.PathSeparator == '\\' {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return appData, nil
		}
		return filepath.Join(home, "AppData", "Roaming"), nil
	}
	return filepath.Join(home, ".local", "share"), nil
}

func (store *SessionStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func sessionFileName(id SessionID) string { return string(id) + ".jsonl" }
func (store *SessionStore) path(id SessionID) string {
	return filepath.Join(store.root, sessionFileName(id))
}

func (store *SessionStore) indexPath(id SessionID) string {
	return filepath.Join(store.root, sessionFileName(id)+".idx.json")
}

func (store *SessionStore) Create(metadata SessionMetadata) (*Session, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	metadata, err := normalizeMetadata(metadata)
	if err != nil {
		return nil, err
	}
	if metadata.ID == "" {
		metadata.ID = SessionID(newStableID())
	}
	if err := validateSessionID(metadata.ID); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	path := store.path(metadata.ID)
	lock, err := acquireSessionFileLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("session %q already exists", metadata.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("check session: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create session file: %w", err)
	}
	defer file.Close()
	event, err := NewLifecycleEvent(metadata.ID, LifecycleCreated, "")
	if err != nil {
		return nil, err
	}
	event.Seq = 1
	if err := writeEvent(file, event); err != nil {
		return nil, err
	}
	metaEvent, err := newMetadataEvent(metadata.ID, metadata)
	if err != nil {
		return nil, err
	}
	metaEvent.Seq = 2
	if err := writeEvent(file, metaEvent); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync session file: %w", err)
	}
	metadata.UpdatedAt = metaEvent.At
	_ = store.writeSessionIndex(metadata.ID, []Event{event, metaEvent})
	return &Session{ID: metadata.ID, Metadata: metadata, Events: []Event{event, metaEvent}, store: store}, nil
}

func (store *SessionStore) CreateSession(metadata SessionMetadata) (*Session, error) {
	return store.Create(metadata)
}

// Delete removes one durable session. The identifier is validated before any
// filesystem path is constructed, and symlinked or otherwise non-regular
// session paths are rejected rather than followed.
func (store *SessionStore) Delete(id SessionID) error {
	if store == nil {
		return errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := store.path(id)
	turnLock, err := tryAcquireSessionFileLock(path+".turn.lock", ErrSessionBusy)
	if err != nil {
		return err
	}
	defer turnLock.Close()

	store.mu.Lock()
	defer store.mu.Unlock()

	lockPath := path + ".lock"
	lock, err := acquireSessionFileLock(lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("stat session: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("session path is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSessionNotFound
		}
		return fmt.Errorf("delete session: %w", err)
	}
	var cleanupErrors []error
	for _, sidecar := range []string{store.indexPath(id), conversationHistoryIndexPath(path)} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove session sidecar %q: %w", filepath.Base(sidecar), err))
		}
	}
	if len(cleanupErrors) > 0 {
		return fmt.Errorf("delete session sidecars: %w", errors.Join(cleanupErrors...))
	}
	return nil
}

func (store *SessionStore) acquireTurnLease(id SessionID) (*SessionFileLock, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	path := store.path(id)
	lock, err := tryAcquireSessionFileLock(path+".turn.lock", ErrSessionBusy)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		_ = lock.Close()
		return nil, ErrSessionNotFound
	}
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("stat session: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = lock.Close()
		return nil, errors.New("session path is not a regular file")
	}
	return lock, nil
}

func (store *SessionStore) Append(id SessionID, event Event) (Event, error) {
	stored, err := store.AppendEvents(id, []Event{event})
	if err != nil {
		return Event{}, err
	}
	return stored[0], nil
}

// AppendEvents appends a contiguous batch while holding the session file
// lock. Callers that need multiple related records (for example, a voice
// turn's user and assistant messages) can therefore preserve their ordering
// even when other goroutines are appending to the same session.
func (store *SessionStore) AppendEvents(id SessionID, pending []Event) ([]Event, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, errors.New("at least one event is required")
	}
	for _, event := range pending {
		if event.Type == "" {
			return nil, errors.New("event type is required")
		}
		if event.SessionID != "" && event.SessionID != id {
			return nil, errors.New("event session id does not match target")
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.appendEventsLocked(id, pending)
}

// appendEventsLocked is the shared atomic JSONL append path. The caller holds
// store.mu; the per-session file lock keeps separate store instances safe.
func (store *SessionStore) appendEventsLocked(id SessionID, pending []Event) ([]Event, error) {
	path := store.path(id)
	lock, err := acquireSessionFileLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	return store.appendEventsWithFileLock(id, pending)
}

func (store *SessionStore) appendEventsWithFileLock(id SessionID, pending []Event) ([]Event, error) {
	path := store.path(id)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("open session: %w", err)
	}
	defer file.Close()
	if err := repairFinalJSONLLine(file, path); err != nil {
		return nil, err
	}
	sourceInfo, sourceStatErr := file.Stat()
	var index sessionIndex
	indexValid := false
	if sourceStatErr == nil {
		index, indexValid, _ = readSessionIndex(store.indexPath(id), id, sourceInfo)
	}
	existingEventCount := 0
	if !indexValid {
		events, replayErr := replayFile(path, id)
		if replayErr != nil {
			return nil, replayErr
		}
		index, err = sessionIndexFromEvents(id, events)
		if err != nil {
			return nil, err
		}
		existingEventCount = len(events)
	} else {
		existingEventCount = index.Summary.EventCount
	}
	stored := make([]Event, 0, len(pending))
	for _, event := range pending {
		event.SessionID = id
		event.Seq = uint64(existingEventCount + len(stored) + 1)
		if event.ID == "" {
			event.ID = newStableID()
		}
		if event.At.IsZero() {
			event.At = nowUTC()
		}
		if event.Data == nil {
			event.Data = []byte(`{}`)
		}
		if err := writeEvent(file, event); err != nil {
			return nil, err
		}
		stored = append(stored, event)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync session file: %w", err)
	}
	index = appendSessionIndex(index, stored)
	if info, statErr := file.Stat(); statErr == nil {
		index.SourceSize = info.Size()
		index.SourceModTimeNS = info.ModTime().UnixNano()
		_ = store.writeSessionIndexValue(index)
	}
	return stored, nil
}

// UpdatePlan atomically reads the current durable plan, applies one validated
// update, and appends the update event while holding the same store/file lock
// used by ordinary session writes. Concurrent turns therefore cannot lose a
// criterion transition.
func (store *SessionStore) UpdatePlan(id SessionID, update PlanUpdate) (TaskPlan, uint64, error) {
	if store == nil {
		return TaskPlan{}, 0, errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return TaskPlan{}, 0, err
	}
	if err := update.Validate(); err != nil {
		return TaskPlan{}, 0, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	lock, err := acquireSessionFileLock(store.path(id) + ".lock")
	if err != nil {
		return TaskPlan{}, 0, err
	}
	defer lock.Close()
	events, err := replayFile(store.path(id), id)
	if err != nil {
		return TaskPlan{}, 0, err
	}
	plan, _, err := latestSessionPlanState(events)
	if err != nil {
		return TaskPlan{}, 0, fmt.Errorf("load current session plan: %w", err)
	}
	if plan == nil {
		return TaskPlan{}, 0, errors.New("plan update requires an existing plan")
	}
	resolved, err := resolvePlanEvidenceSelectors(planEvidenceIndexFromEvents(events), update)
	if err != nil {
		return TaskPlan{}, 0, err
	}
	if err := validateObservedPlanEvidence(events, resolved); err != nil {
		return TaskPlan{}, 0, err
	}
	updated, err := applyPlanUpdate(*plan, resolved)
	if err != nil {
		return TaskPlan{}, 0, err
	}
	event, err := NewPlanUpdateEvent(id, resolved)
	if err != nil {
		return TaskPlan{}, 0, err
	}
	stored, err := store.appendEventsWithFileLock(id, []Event{event})
	if err != nil {
		return TaskPlan{}, 0, err
	}
	return updated, stored[0].Seq, nil
}

func (store *SessionStore) AppendEvent(id SessionID, event Event) (Event, error) {
	return store.Append(id, event)
}

func (store *SessionStore) Load(id SessionID) (*Session, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	lock, err := acquireSessionFileLock(store.path(id) + ".lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	events, err := replayFile(store.path(id), id)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrSessionNotFound
	}
	metadata := SessionMetadata{ID: id, CreatedAt: events[0].At, UpdatedAt: events[len(events)-1].At}
	for _, event := range events {
		if event.Type == EventLifecycle {
			payload, decodeErr := decodeEventPayload[LifecycleEvent](event, EventLifecycle)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if payload.Action == LifecycleCreated && event.Seq == 1 {
				// Metadata is intentionally not reconstructed from arbitrary user data;
				// project association is stored in the safe sidecar metadata event below.
			}
			if payload.Action == LifecycleForked {
				metadata.ParentEvent = event.Seq - 1
			}
		}
		if event.Type == EventMetadata {
			meta, decodeErr := decodeEventPayload[MetadataEvent](event, EventMetadata)
			if decodeErr != nil {
				return nil, decodeErr
			}
			metadata = meta.Metadata
			metadata.ID = id
		}
	}
	return &Session{ID: id, Metadata: metadata, Events: events, store: store}, nil
}

func (store *SessionStore) Replay(id SessionID) ([]Event, error) {
	session, err := store.Load(id)
	if err != nil {
		return nil, err
	}
	return append([]Event(nil), session.Events...), nil
}

// SessionRepairReport is the bounded operator report for an explicit offline
// durable-history repair. Repair never executes a tool and never claims to
// recover the original malformed argument bytes.
type SessionRepairReport struct {
	SessionID        SessionID `json:"session_id"`
	Path             string    `json:"path"`
	DryRun           bool      `json:"dry_run"`
	Applied          bool      `json:"applied"`
	Changed          bool      `json:"changed"`
	InvalidToolCalls int       `json:"invalid_tool_calls"`
	RepairedEvents   int       `json:"repaired_events"`
	PreservedEvents  int       `json:"preserved_events"`
	BackupPath       string    `json:"backup_path,omitempty"`
	ReplayBehavior   string    `json:"replay_behavior"`
}

const (
	maxSessionRepairBytes  = 64 << 20
	maxSessionRepairEvents = 100_000
)

// RepairSession performs a bounded, local-only repair of malformed historical
// assistant tool-call arguments. Dry-run is the default. Apply acquires the
// normal session lock, writes a private atomic backup, and atomically replaces
// the JSONL source with the same events except that invalid arguments become a
// valid inert object. It does not mutate on startup or execute any command.
func (store *SessionStore) RepairSession(id SessionID, apply bool) (SessionRepairReport, error) {
	if store == nil {
		return SessionRepairReport{}, errors.New("session store is nil")
	}
	if err := validateSessionID(id); err != nil {
		return SessionRepairReport{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	path := store.path(id)
	lock, err := acquireSessionFileLock(path + ".lock")
	if err != nil {
		return SessionRepairReport{}, err
	}
	defer lock.Close()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return SessionRepairReport{}, ErrSessionNotFound
	}
	if err != nil {
		return SessionRepairReport{}, fmt.Errorf("stat session for repair: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SessionRepairReport{}, errors.New("session path is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return SessionRepairReport{}, errors.New("session file is not private; expected owner-only permissions before repair")
	}
	if info.Size() > maxSessionRepairBytes {
		return SessionRepairReport{}, fmt.Errorf("session file exceeds repair limit of %d bytes", maxSessionRepairBytes)
	}
	events, err := replayFile(path, id)
	if err != nil {
		return SessionRepairReport{}, err
	}
	if len(events) > maxSessionRepairEvents {
		return SessionRepairReport{}, fmt.Errorf("session exceeds repair limit of %d events", maxSessionRepairEvents)
	}
	if err := ensureSessionFileUnchanged(path, info); err != nil {
		return SessionRepairReport{}, err
	}
	repaired := make([]Event, len(events))
	copy(repaired, events)
	report := SessionRepairReport{SessionID: id, Path: path, DryRun: !apply, ReplayBehavior: "invalid or structurally incomplete historical tool-call groups are omitted from provider replay; durable events are not rewritten by replay"}
	for index := range repaired {
		changed, invalid, repairErr := repairSessionEvent(&repaired[index])
		if repairErr != nil {
			return SessionRepairReport{}, fmt.Errorf("inspect session event %d: %w", index+1, repairErr)
		}
		if changed {
			report.Changed = true
			report.RepairedEvents++
			report.InvalidToolCalls += invalid
		} else {
			report.PreservedEvents++
		}
	}
	if !report.Changed || !apply {
		return report, nil
	}
	backupPath, err := atomicPrivateSessionBackup(path, info)
	if err != nil {
		return SessionRepairReport{}, err
	}
	temporary, err := os.CreateTemp(store.root, ".session-repair-*")
	if err != nil {
		return SessionRepairReport{}, fmt.Errorf("create repaired session: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return SessionRepairReport{}, fmt.Errorf("set repaired session permissions: %w", err)
	}
	for _, event := range repaired {
		if err := writeEvent(temporary, event); err != nil {
			_ = temporary.Close()
			return SessionRepairReport{}, err
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return SessionRepairReport{}, fmt.Errorf("sync repaired session: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return SessionRepairReport{}, fmt.Errorf("close repaired session: %w", err)
	}
	if err := ensureSessionFileUnchanged(path, info); err != nil {
		return SessionRepairReport{}, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return SessionRepairReport{}, fmt.Errorf("install repaired session: %w", err)
	}
	if err := syncSessionDirectory(store.root); err != nil {
		return SessionRepairReport{}, err
	}
	_ = os.Remove(store.indexPath(id))
	_ = os.Remove(conversationHistoryIndexPath(path))
	report.Applied = true
	report.BackupPath = backupPath
	return report, nil
}

func repairSessionEvent(event *Event) (bool, int, error) {
	if event == nil {
		return false, 0, errors.New("nil session event")
	}
	switch event.Type {
	case EventToolCall:
		payload, err := DecodeToolCallEvent(*event)
		if err != nil {
			return false, 0, err
		}
		if err := validateToolCallArguments(payload.Call); err == nil {
			return false, 0, nil
		}
		payload.Call.Arguments = inertRepairedToolArguments(payload.Call.Name)
		data, err := json.Marshal(payload)
		if err != nil {
			return false, 0, err
		}
		event.Data = data
		return true, 1, nil
	case EventMessage:
		payload, err := DecodeMessageEvent(*event)
		if err != nil {
			return false, 0, err
		}
		if payload.Message.Role != RoleAssistant {
			return false, 0, nil
		}
		changed := false
		invalid := 0
		for index := range payload.Message.ToolCalls {
			if err := validateToolCallArguments(payload.Message.ToolCalls[index]); err == nil {
				continue
			}
			payload.Message.ToolCalls[index].Arguments = inertRepairedToolArguments(payload.Message.ToolCalls[index].Name)
			changed = true
			invalid++
		}
		if !changed {
			return false, 0, nil
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return false, 0, err
		}
		event.Data = data
		return true, invalid, nil
	default:
		return false, 0, nil
	}
}

func inertRepairedToolArguments(toolName string) string {
	data, _ := json.Marshal(map[string]any{"_bee_repaired": true, "_bee_original_tool": toolName})
	return string(data)
}

func ensureSessionFileUnchanged(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck session before repair: %w", err)
	}
	if !os.SameFile(expected, current) || current.Size() != expected.Size() || current.ModTime() != expected.ModTime() {
		return errors.New("session changed during repair; retry after confirming exclusive local access")
	}
	return nil
}

func atomicPrivateSessionBackup(path string, expected os.FileInfo) (string, error) {
	source, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open session for backup: %w", err)
	}
	defer source.Close()
	if info, statErr := source.Stat(); statErr != nil {
		return "", fmt.Errorf("stat session backup source: %w", statErr)
	} else if !os.SameFile(expected, info) || info.Size() != expected.Size() || info.ModTime() != expected.ModTime() {
		return "", errors.New("session changed before backup; retry after confirming exclusive local access")
	}
	for attempt := 0; attempt < 16; attempt++ {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("rewind session backup source: %w", err)
		}
		backupPath := fmt.Sprintf("%s.bak-%s-%02d", path, nowUTC().Format("20060102T150405.000000000Z"), attempt)
		temporary, createErr := os.OpenFile(backupPath+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			continue
		}
		if createErr != nil {
			return "", fmt.Errorf("create session backup: %w", createErr)
		}
		if _, copyErr := io.Copy(temporary, source); copyErr != nil {
			_ = temporary.Close()
			_ = os.Remove(backupPath + ".tmp")
			return "", fmt.Errorf("copy session backup: %w", copyErr)
		}
		if syncErr := temporary.Sync(); syncErr != nil {
			_ = temporary.Close()
			_ = os.Remove(backupPath + ".tmp")
			return "", fmt.Errorf("sync session backup: %w", syncErr)
		}
		if closeErr := temporary.Close(); closeErr != nil {
			_ = os.Remove(backupPath + ".tmp")
			return "", fmt.Errorf("close session backup: %w", closeErr)
		}
		if err := os.Link(backupPath+".tmp", backupPath); err != nil {
			_ = os.Remove(backupPath + ".tmp")
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", fmt.Errorf("install session backup without clobbering: %w", err)
		}
		if err := os.Remove(backupPath + ".tmp"); err != nil {
			return "", fmt.Errorf("remove temporary session backup: %w", err)
		}
		if err := syncSessionDirectory(filepath.Dir(path)); err != nil {
			return "", err
		}
		return backupPath, nil
	}
	return "", errors.New("could not allocate unique session backup path")
}

func syncSessionDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for durability: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory for durability: %w", err)
	}
	return nil
}

// SessionSummary avoids loading message contents for list views.
type SessionSummary struct {
	ID          SessionID `json:"id"`
	Title       string    `json:"title,omitempty"`
	Pinned      bool      `json:"pinned"`
	ProjectID   ProjectID `json:"project_id,omitempty"`
	ProjectRoot string    `json:"project_root,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	EventCount  int       `json:"event_count"`
	ParentID    SessionID `json:"parent_id,omitempty"`
}

// SessionSearchOptions bounds a linear scan over persisted session history.
// ProjectID and Unassigned mirror the normal session-list filters.
type SessionSearchOptions struct {
	Query      string
	ProjectID  ProjectID
	Unassigned bool
	Limit      int
	// DisplayMessagesOnly limits matching to the user/assistant projection
	// used by owner web history, excluding system and tool content.
	DisplayMessagesOnly bool
	// MatchSummaryFields also includes safe title or session-ID matches even
	// when no display-message snippet matches.
	MatchSummaryFields bool
}

// SessionSearchMatch is a short, redacted excerpt from one persisted message.
type SessionSearchMatch struct {
	Role    Role   `json:"role"`
	Snippet string `json:"snippet"`
}

// SessionSearchResult combines normal session metadata with bounded matches.
type SessionSearchResult struct {
	SessionSummary
	Matches []SessionSearchMatch `json:"matches"`
}

// SearchSessions performs a locked, pinned-first then newest-first linear scan of persisted
// user, assistant, and tool message content. The store is intentionally not
// indexed yet; the lock gives callers a consistent view while files are read.
func (store *SessionStore) SearchSessions(options SessionSearchOptions) ([]SessionSearchResult, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	query := strings.TrimSpace(options.Query)
	if query == "" {
		return nil, errors.New("search query is required")
	}
	if len(query) > MaxSessionSearchQueryBytes || utf8.RuneCountInString(query) > MaxSessionSearchQueryBytes {
		return nil, fmt.Errorf("search query must be at most %d characters", MaxSessionSearchQueryBytes)
	}
	limit := options.Limit
	if limit <= 0 || limit > MaxSessionSearchResults {
		limit = MaxSessionSearchResults
	}
	needle := strings.ToLower(query)

	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := os.ReadDir(store.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search sessions: %w", err)
	}
	results := make([]SessionSearchResult, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := SessionID(strings.TrimSuffix(entry.Name(), ".jsonl"))
		path := filepath.Join(store.root, entry.Name())
		lock, lockErr := acquireSessionFileLock(path + ".lock")
		if lockErr != nil {
			return nil, lockErr
		}
		index, indexErr := store.ensureSessionIndexLocked(id)
		_ = lock.Close()
		if errors.Is(indexErr, ErrSessionNotFound) {
			continue
		}
		if indexErr != nil {
			return nil, indexErr
		}
		summary := index.Summary
		if options.Unassigned && summary.ProjectID != "" {
			continue
		}
		if options.ProjectID != "" && !options.Unassigned && summary.ProjectID != options.ProjectID {
			continue
		}
		matches := make([]SessionSearchMatch, 0, MaxSessionSearchMatches)
		for _, document := range index.Search {
			if options.DisplayMessagesOnly && document.Role != RoleUser && document.Role != RoleAssistant {
				continue
			}
			if !strings.Contains(strings.ToLower(document.Content), needle) {
				continue
			}
			matches = append(matches, SessionSearchMatch{Role: document.Role, Snippet: sessionSearchSnippet(document.Content, query, MaxSessionSearchSnippetBytes)})
			if len(matches) >= MaxSessionSearchMatches {
				break
			}
		}
		summaryMatch := options.MatchSummaryFields && (strings.Contains(strings.ToLower(summary.Title), needle) ||
			strings.Contains(strings.ToLower(string(summary.ID)), needle))
		if len(matches) > 0 || summaryMatch {
			results = append(results, SessionSearchResult{SessionSummary: summary, Matches: matches})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Pinned != results[j].Pinned {
			return results[i].Pinned
		}
		if results[i].UpdatedAt.Equal(results[j].UpdatedAt) {
			return results[i].ID < results[j].ID
		}
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func searchableEventContent(event Event) (Role, string, bool, error) {
	switch event.Type {
	case EventMessage:
		payload, err := decodeEventPayload[MessageEvent](event, EventMessage)
		if err != nil {
			return "", "", false, err
		}
		if payload.Message.Role != RoleUser && payload.Message.Role != RoleAssistant && payload.Message.Role != RoleTool {
			return "", "", false, nil
		}
		return payload.Message.Role, payload.Message.Content, strings.TrimSpace(payload.Message.Content) != "", nil
	case EventToolResult:
		payload, err := decodeEventPayload[ToolResultEvent](event, EventToolResult)
		if err != nil {
			return "", "", false, err
		}
		content := payload.Output
		if payload.Error != "" {
			content = "error: " + payload.Error
		}
		return RoleTool, content, strings.TrimSpace(content) != "", nil
	default:
		return "", "", false, nil
	}
}

func sessionSearchSnippet(content, query string, maxBytes int) string {
	content = strings.Join(strings.Fields(content), " ")
	if content == "" {
		return ""
	}
	lowerContent := strings.ToLower(content)
	index := strings.Index(lowerContent, strings.ToLower(query))
	if index < 0 {
		return truncateUTF8(redactString(content), maxBytes)
	}
	start := index - 72
	if start < 0 {
		start = 0
	}
	end := index + len(query) + 96
	if end > len(content) {
		end = len(content)
	}
	snippet := content[start:end]
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(content) {
		snippet += "…"
	}
	return truncateUTF8(redactString(snippet), maxBytes)
}

func sessionIndexFromEvents(id SessionID, events []Event) (sessionIndex, error) {
	if len(events) == 0 {
		return sessionIndex{}, ErrSessionNotFound
	}
	index := sessionIndex{
		Version: sessionIndexVersion,
		ID:      id,
		Summary: SessionSummary{ID: id, CreatedAt: events[0].At, UpdatedAt: events[len(events)-1].At, EventCount: len(events)},
	}
	for _, event := range events {
		if event.Type == EventMetadata {
			meta, err := decodeEventPayload[MetadataEvent](event, EventMetadata)
			if err != nil {
				return sessionIndex{}, err
			}
			index.Summary.Title = meta.Metadata.Title
			index.Summary.Pinned = meta.Metadata.Pinned
			index.Summary.ProjectID = meta.Metadata.ProjectID
			index.Summary.ProjectRoot = meta.Metadata.ProjectRoot
			index.Summary.ParentID = meta.Metadata.ParentID
		}
		role, content, ok, err := searchableEventContent(event)
		if err != nil {
			return sessionIndex{}, err
		}
		if ok {
			index.Search = append(index.Search, sessionIndexDocument{Role: role, Content: content})
		}
	}
	return index, nil
}

func (store *SessionStore) writeSessionIndex(id SessionID, events []Event) error {
	index, err := sessionIndexFromEvents(id, events)
	if err != nil {
		return err
	}
	info, err := os.Stat(store.path(id))
	if err != nil {
		return err
	}
	index.SourceSize = info.Size()
	index.SourceModTimeNS = info.ModTime().UnixNano()
	return store.writeSessionIndexValue(index)
}

func appendSessionIndex(index sessionIndex, events []Event) sessionIndex {
	if index.Summary.EventCount == 0 && len(events) > 0 {
		index.Summary.ID = index.ID
	}
	for _, event := range events {
		index.Summary.EventCount++
		index.Summary.UpdatedAt = event.At
		if event.Type == EventMetadata {
			if meta, err := decodeEventPayload[MetadataEvent](event, EventMetadata); err == nil {
				index.Summary.Title = meta.Metadata.Title
				index.Summary.Pinned = meta.Metadata.Pinned
				index.Summary.ProjectID = meta.Metadata.ProjectID
				index.Summary.ProjectRoot = meta.Metadata.ProjectRoot
				index.Summary.ParentID = meta.Metadata.ParentID
			}
		}
		if role, content, ok, err := searchableEventContent(event); err == nil && ok {
			index.Search = append(index.Search, sessionIndexDocument{Role: role, Content: content})
		}
	}
	return index
}

func (store *SessionStore) writeSessionIndexValue(index sessionIndex) error {
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("encode session index: %w", err)
	}
	temporary, err := os.CreateTemp(store.root, ".session-index-*")
	if err != nil {
		return fmt.Errorf("create session index: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set session index permissions: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write session index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync session index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close session index: %w", err)
	}
	if err := os.Rename(temporaryPath, store.indexPath(index.ID)); err != nil {
		return fmt.Errorf("install session index: %w", err)
	}
	return nil
}

func readSessionIndex(path string, id SessionID, source os.FileInfo) (sessionIndex, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionIndex{}, false, nil
	}
	if err != nil {
		return sessionIndex{}, false, err
	}
	var index sessionIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return sessionIndex{}, false, nil
	}
	if index.Version != sessionIndexVersion || index.ID != id || index.Summary.ID != id || index.Summary.EventCount <= 0 || index.Summary.CreatedAt.IsZero() || index.Summary.UpdatedAt.IsZero() || index.SourceSize != source.Size() || index.SourceModTimeNS != source.ModTime().UnixNano() {
		return sessionIndex{}, false, nil
	}
	for _, document := range index.Search {
		if document.Role != RoleUser && document.Role != RoleAssistant && document.Role != RoleTool {
			return sessionIndex{}, false, nil
		}
	}
	return index, true, nil
}

// ensureSessionIndexLocked must be called while holding the per-session lock.
// A missing, corrupt, or stale sidecar is rebuilt from the JSONL source.
func (store *SessionStore) ensureSessionIndexLocked(id SessionID) (sessionIndex, error) {
	path := store.path(id)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionIndex{}, ErrSessionNotFound
		}
		return sessionIndex{}, err
	}
	if index, ok, readErr := readSessionIndex(store.indexPath(id), id, info); readErr == nil && ok {
		return index, nil
	}
	events, err := replayFile(path, id)
	if err != nil {
		return sessionIndex{}, err
	}
	index, err := sessionIndexFromEvents(id, events)
	if err != nil {
		return sessionIndex{}, err
	}
	index.SourceSize = info.Size()
	index.SourceModTimeNS = info.ModTime().UnixNano()
	// The JSONL file is authoritative. A read-only or otherwise unwritable
	// index directory must not make an otherwise readable session disappear;
	// return the rebuilt in-memory value and let a later request retry the
	// sidecar installation.
	_ = store.writeSessionIndexValue(index)
	return index, nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}

func (store *SessionStore) List() ([]SessionSummary, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	dir := store.root
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var result []SessionSummary
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := SessionID(strings.TrimSuffix(entry.Name(), ".jsonl"))
		path := filepath.Join(dir, entry.Name())
		lock, lockErr := acquireSessionFileLock(path + ".lock")
		if lockErr != nil {
			return nil, lockErr
		}
		index, indexErr := store.ensureSessionIndexLocked(id)
		_ = lock.Close()
		if errors.Is(indexErr, ErrSessionNotFound) {
			continue
		}
		if indexErr != nil {
			return nil, indexErr
		}
		result = append(result, index.Summary)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Pinned != result[j].Pinned {
			return result[i].Pinned
		}
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (store *SessionStore) ListSummaries() ([]SessionSummary, error) { return store.List() }

func (store *SessionStore) Fork(id SessionID, through uint64, metadata SessionMetadata) (*Session, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	sourceLock, err := acquireSessionFileLock(store.path(id) + ".lock")
	if err != nil {
		return nil, err
	}
	defer sourceLock.Close()
	sourceEvents, err := replayFile(store.path(id), id)
	if err != nil {
		return nil, err
	}
	if through > uint64(len(sourceEvents)) {
		return nil, fmt.Errorf("fork boundary %d exceeds event count %d", through, len(sourceEvents))
	}
	metadata, err = normalizeMetadata(metadata)
	if err != nil {
		return nil, err
	}
	if metadata.ID == "" {
		metadata.ID = SessionID(newStableID())
	}
	if err := validateSessionID(metadata.ID); err != nil {
		return nil, err
	}
	metadata.ParentID, metadata.ParentEvent = id, through
	if metadata.Title == "" {
		metadata.Title = string(id) + " fork"
	}
	path := store.path(metadata.ID)
	if metadata.ID == id {
		return nil, errors.New("fork destination must differ from source")
	}
	destinationLock, lockErr := acquireSessionFileLock(path + ".lock")
	if lockErr != nil {
		return nil, lockErr
	}
	defer destinationLock.Close()
	if _, statErr := os.Stat(path); statErr == nil {
		return nil, fmt.Errorf("session %q already exists", metadata.ID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create fork file: %w", err)
	}
	defer file.Close()
	var copied []Event
	for _, original := range sourceEvents[:int(through)] {
		clone := original
		clone.SessionID, clone.ID = metadata.ID, newStableID()
		if err := writeEvent(file, clone); err != nil {
			return nil, err
		}
		copied = append(copied, clone)
	}
	metaEvent, err := newMetadataEvent(metadata.ID, metadata)
	if err != nil {
		return nil, err
	}
	metaEvent.Seq = uint64(len(copied) + 1)
	if err := writeEvent(file, metaEvent); err != nil {
		return nil, err
	}
	copied = append(copied, metaEvent)
	forkEvent, err := NewLifecycleEvent(metadata.ID, LifecycleForked, fmt.Sprintf("forked from %s at event %d", id, through))
	if err != nil {
		return nil, err
	}
	forkEvent.Seq = uint64(len(copied) + 1)
	if err := writeEvent(file, forkEvent); err != nil {
		return nil, err
	}
	copied = append(copied, forkEvent)
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync fork session file: %w", err)
	}
	metadata.UpdatedAt = forkEvent.At
	_ = store.writeSessionIndex(metadata.ID, copied)
	return &Session{ID: metadata.ID, Metadata: metadata, Events: copied, store: store}, nil
}

func (store *SessionStore) ForkSession(id SessionID, through uint64, metadata SessionMetadata) (*Session, error) {
	return store.Fork(id, through, metadata)
}

func (store *SessionStore) Compact(id SessionID, through uint64, summary string) error {
	session, err := store.Load(id)
	if err != nil {
		return err
	}
	if through > uint64(len(session.Events)) {
		return fmt.Errorf("compaction boundary %d exceeds event count %d", through, len(session.Events))
	}
	return session.Compact(through, summary)
}

func replayFile(path string, id SessionID) ([]Event, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var events []Event
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var event Event
			if unmarshalErr := json.Unmarshal(trimmed, &event); unmarshalErr != nil {
				if readErr == io.EOF && isTruncatedJSON(unmarshalErr) {
					break
				}
				return nil, fmt.Errorf("decode session JSONL %q line %d: %w", path, len(events)+1, unmarshalErr)
			}
			if event.SessionID != id {
				return nil, fmt.Errorf("session event %d has mismatched session id %q", len(events)+1, event.SessionID)
			}
			if event.Seq != uint64(len(events)+1) {
				return nil, fmt.Errorf("session event sequence %d is %d", len(events)+1, event.Seq)
			}
			if event.Type == "" || event.ID == "" || event.At.IsZero() {
				return nil, fmt.Errorf("session event %d is incomplete", len(events)+1)
			}
			events = append(events, event)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read session JSONL: %w", readErr)
		}
	}
	return events, nil
}

func repairFinalJSONLLine(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat session file for repair: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read session file for repair: %w", err)
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	lineStart := bytes.LastIndexByte(data, '\n') + 1
	finalLine := bytes.TrimSpace(data[lineStart:])
	var event Event
	if err := json.Unmarshal(finalLine, &event); err == nil {
		if _, writeErr := file.Write([]byte{'\n'}); writeErr != nil {
			return fmt.Errorf("terminate final JSONL line: %w", writeErr)
		}
		return nil
	} else if !isTruncatedJSON(err) {
		return fmt.Errorf("repair session JSONL %q: %w", path, err)
	}
	if err := file.Truncate(int64(lineStart)); err != nil {
		return fmt.Errorf("discard truncated final JSONL line: %w", err)
	}
	return nil
}

func writeEvent(writer io.Writer, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode session event: %w", err)
	}
	data = append(data, '\n')
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("append session event: %w", err)
	}
	return nil
}

func validateSessionID(id SessionID) error {
	value := string(id)
	if strings.TrimSpace(value) == "" {
		return errors.New("session id is required")
	}
	if value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return errors.New("session id must be a single path-safe component")
	}
	return nil
}

func isTruncatedJSON(err error) bool {
	return strings.Contains(err.Error(), "unexpected end of JSON input")
}
func nowUTC() time.Time { return time.Now().UTC() }

// Metadata is a safe, explicit event rather than arbitrary file contents.
const EventMetadata EventType = "metadata"

type MetadataEvent struct {
	Metadata SessionMetadata `json:"metadata"`
}

func newMetadataEvent(id SessionID, metadata SessionMetadata) (Event, error) {
	return newEvent(id, EventMetadata, MetadataEvent{Metadata: metadata})
}

func redactString(value string) string {
	for _, marker := range []string{"api_key=", "api_key:", "token=", "token:", "password=", "password:", "secret=", "secret:"} {
		for {
			index := strings.Index(strings.ToLower(value), marker)
			if index < 0 {
				break
			}
			start := index + len(marker)
			if strings.HasPrefix(strings.ToLower(value[start:]), "[redacted]") {
				break
			}
			end := start
			for end < len(value) && !strings.ContainsRune(" \t\r\n,;}'\"", rune(value[end])) {
				end++
			}
			value = value[:start] + "[REDACTED]" + value[end:]
		}
	}
	return value
}

func isSecretKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, marker := range []string{"api_key", "apikey", "access_token", "token", "secret", "password", "authorization", "credential"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
