package bee

import (
	"bufio"
	"context"
	"crypto/rand"
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
	"unicode"
)

const (
	memoryEventsFile       = "events.jsonl"
	memoryProposalsFile    = "proposals.jsonl"
	memoryIdentityFile     = "identity.json"
	memoryProfilesDir      = "profiles"
	memoryWriterLockFile   = ".memory.lock"
	proposalWriterLockFile = ".proposals.lock"
	memoryMaxEventLine     = 1 << 20
	memoryMaxSnapshotSize  = 1 << 20
	memoryMaxLockBytes     = 4096
)

var (
	ErrMemoryNotFound = errors.New("memory record not found")
	ErrMemoryConflict = errors.New("memory version conflict")
	ErrMemoryCorrupt  = errors.New("memory storage is corrupt")
	ErrMemoryLockHeld = errors.New("memory storage writer lock is held")
)

type memoryState struct {
	record  MemoryRecord
	version uint64
	deleted bool
}

type memorySharedLock struct {
	mu sync.Mutex
}

var memoryProcessLocks sync.Map // map[string]*memorySharedLock

// LocalMemoryStore is a standard-library-only append-only durable memory store.
// Its root is caller-provided and is never inferred from the repository.
type LocalMemoryStore struct {
	root   string
	policy MemoryPolicy
	mu     *memorySharedLock

	state                map[string]memoryState
	eventIDs             map[string]struct{}
	batchFailureInjector func(string) error
	replayLoaded         bool
	replayExists         bool
	replaySize           int64
	replayMtimeNS        int64
}

// LocalMemoryBackend is a descriptive alias for callers that think in terms
// of a backend rather than the MemoryStore contract.
type LocalMemoryBackend = LocalMemoryStore

var _ MemoryStore = (*LocalMemoryStore)(nil)

// NewLocalMemoryStore creates or opens a private durable-memory root. Existing
// storage is replayed completely; malformed, truncated, oversized, unsafe, or
// policy-invalid data fails closed.
func NewLocalMemoryStore(root string, policy MemoryPolicy) (*LocalMemoryStore, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("memory policy: %w", err)
	}
	abs, err := preparePrivateStorageRoot(root)
	if err != nil {
		return nil, err
	}
	shared := &memorySharedLock{}
	actual, _ := memoryProcessLocks.LoadOrStore(abs, shared)
	store := &LocalMemoryStore{root: abs, policy: policy, mu: actual.(*memorySharedLock), state: make(map[string]memoryState), eventIDs: make(map[string]struct{})}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

// NewLocalDurableMemoryStore is a descriptive compatibility alias.
func NewLocalDurableMemoryStore(root string, policy MemoryPolicy) (*LocalMemoryStore, error) {
	return NewLocalMemoryStore(root, policy)
}

func NewLocalMemoryBackend(root string, policy MemoryPolicy) (*LocalMemoryStore, error) {
	return NewLocalMemoryStore(root, policy)
}

func (store *LocalMemoryStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *LocalMemoryStore) Policy() MemoryPolicy {
	if store == nil {
		return MemoryPolicy{}
	}
	return store.policy
}

func (store *LocalMemoryStore) Append(ctx context.Context, event MemoryEvent) error {
	if store == nil {
		return errors.New("memory store is nil")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := event.ValidateForStore(store.policy); err != nil {
		return fmt.Errorf("validate memory event: %w", err)
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := contextErr(ctx); err != nil {
		return err
	}
	lock, err := store.acquireFileLockLocked(ctx, memoryWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	// Replay while holding the cross-process lock so another writer cannot
	// append between replay and version validation. Apply to a copy until the
	// durable line has been synced; failed writes must not alter the index.
	if err := store.replayLocked(); err != nil {
		return err
	}
	if _, exists := store.eventIDs[event.ID]; exists {
		return fmt.Errorf("memory event %q already exists: %w", event.ID, ErrMemoryConflict)
	}
	nextState := cloneMemoryState(store.state)
	if err := applyMemoryEvent(nextState, event); err != nil {
		return err
	}
	if len(nextState) > store.policy.MaxRecords {
		return fmt.Errorf("memory store exceeds %d records", store.policy.MaxRecords)
	}
	path := filepath.Join(store.root, memoryEventsFile)
	file, err := openPrivateAppend(path)
	if err != nil {
		return fmt.Errorf("open memory event log: %w", err)
	}
	defer file.Close()
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode memory event: %w", err)
	}
	if len(line) > memoryMaxEventLine {
		return fmt.Errorf("memory event exceeds %d bytes", memoryMaxEventLine)
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat memory event log: %w", err)
	}
	if info.Size()+int64(len(line)+1) > int64(store.policy.MaxBytes) || info.Size()+int64(len(line)+1) > int64(MaxMemoryBytes) {
		return fmt.Errorf("memory event log exceeds configured or absolute byte limit")
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append memory event: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync memory event log: %w", err)
	}
	store.state = nextState
	store.eventIDs[event.ID] = struct{}{}
	store.cacheReplayMetadataLocked(path)
	return nil
}

// AppendBatch validates and commits a group of events under one writer lock
// and one fsync. It is intentionally an optional extension to MemoryStore so
// compatibility stores remain usable while atomic lifecycle operations fail
// closed when the extension is unavailable.
func (store *LocalMemoryStore) AppendBatch(ctx context.Context, events []MemoryEvent) error {
	if store == nil {
		return errors.New("memory store is nil")
	}
	if len(events) == 0 {
		return errors.New("memory event batch is empty")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	lock, err := store.acquireFileLockLocked(ctx, memoryWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := store.replayLocked(); err != nil {
		return err
	}
	nextState := cloneMemoryState(store.state)
	nextIDs := make(map[string]struct{}, len(store.eventIDs)+len(events))
	for id := range store.eventIDs {
		nextIDs[id] = struct{}{}
	}
	lines := make([][]byte, 0, len(events))
	total := int64(0)
	for _, event := range events {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := event.ValidateForStore(store.policy); err != nil {
			return fmt.Errorf("validate memory event: %w", err)
		}
		if _, exists := nextIDs[event.ID]; exists {
			return fmt.Errorf("memory event %q already exists: %w", event.ID, ErrMemoryConflict)
		}
		if err := applyMemoryEvent(nextState, event); err != nil {
			return err
		}
		line, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode memory event: %w", err)
		}
		if len(line) > memoryMaxEventLine {
			return fmt.Errorf("memory event exceeds %d bytes", memoryMaxEventLine)
		}
		lines = append(lines, append(line, '\n'))
		total += int64(len(line) + 1)
		nextIDs[event.ID] = struct{}{}
	}
	if len(nextState) > store.policy.MaxRecords {
		return fmt.Errorf("memory store exceeds %d records", store.policy.MaxRecords)
	}
	path := filepath.Join(store.root, memoryEventsFile)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		info = nil
	} else if err != nil {
		return fmt.Errorf("stat memory event log: %w", err)
	}
	currentSize := int64(0)
	if info != nil {
		currentSize = info.Size()
	}
	if currentSize+total > int64(store.policy.MaxBytes) || currentSize+total > int64(MaxMemoryBytes) {
		return errors.New("memory event log exceeds configured or absolute byte limit")
	}
	tmp, err := os.CreateTemp(store.root, ".memory-events-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if old, openErr := os.Open(path); openErr == nil {
		if _, err := io.Copy(tmp, old); err != nil {
			old.Close()
			return fmt.Errorf("copy memory event log: %w", err)
		}
		_ = old.Close()
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	if store.batchFailureInjector != nil {
		if err := store.batchFailureInjector("batch_write"); err != nil {
			return err
		}
	}
	for _, line := range lines {
		n, err := tmp.Write(line)
		if err != nil {
			return fmt.Errorf("write memory event transaction: %w", err)
		}
		if n != len(line) {
			return io.ErrShortWrite
		}
	}
	if store.batchFailureInjector != nil {
		if err := store.batchFailureInjector("batch_sync"); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync memory event transaction: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if store.batchFailureInjector != nil {
		if err := store.batchFailureInjector("batch_rename"); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit memory event transaction: %w", err)
	}
	if dir, err := os.Open(store.root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	store.state, store.eventIDs = nextState, nextIDs
	store.cacheReplayMetadataLocked(path)
	return nil
}

func (store *LocalMemoryStore) Retrieve(ctx context.Context, query MemoryQuery) (MemoryRetrievalResult, error) {
	if store == nil {
		return MemoryRetrievalResult{}, errors.New("memory store is nil")
	}
	if err := query.Validate(store.policy); err != nil {
		return MemoryRetrievalResult{}, err
	}
	if err := contextErr(ctx); err != nil {
		return MemoryRetrievalResult{}, err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return MemoryRetrievalResult{}, err
	}
	asOf := query.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	maxRecords := query.Limit
	if maxRecords == 0 {
		maxRecords = store.policy.MaxQueryRecords
	}
	maxBytes := query.MaxBytes
	if maxBytes == 0 {
		maxBytes = store.policy.MaxQueryBytes
	}
	if maxRecords > store.policy.MaxQueryRecords {
		maxRecords = store.policy.MaxQueryRecords
	}
	if maxBytes > store.policy.MaxQueryBytes {
		maxBytes = store.policy.MaxQueryBytes
	}

	type candidate struct {
		record MemoryRecord
		score  int
	}
	candidates := make([]candidate, 0, len(store.state))
	for _, state := range store.state {
		if state.deleted || isMemoryExpired(state.record, store.policy, asOf) || !memoryMatchesTemporal(state.record, query, asOf) {
			continue
		}
		if state.record.Sensitivity == MemorySensitivitySensitive && !query.IncludeSensitive {
			continue
		}
		if query.Namespace != "" && state.record.Namespace != query.Namespace {
			continue
		}
		if query.Key != "" && state.record.Key != query.Key {
			continue
		}
		if query.SubjectID != "" && state.record.SubjectID != query.SubjectID {
			continue
		}
		score, matched := exactMemoryMatch(state.record, query.Text)
		if strings.TrimSpace(query.Text) != "" && !matched {
			continue
		}
		candidates = append(candidates, candidate{record: state.record, score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if !candidates[i].record.UpdatedAt.Equal(candidates[j].record.UpdatedAt) {
			return candidates[i].record.UpdatedAt.After(candidates[j].record.UpdatedAt)
		}
		if candidates[i].record.ID != candidates[j].record.ID {
			return candidates[i].record.ID < candidates[j].record.ID
		}
		return candidates[i].record.Key < candidates[j].record.Key
	})

	result := MemoryRetrievalResult{Total: len(candidates)}
	for _, item := range candidates {
		if len(result.Records) >= maxRecords {
			result.Truncated = true
			break
		}
		encoded, err := json.Marshal(item.record)
		if err != nil {
			return MemoryRetrievalResult{}, fmt.Errorf("encode retrieved record: %w", err)
		}
		if result.Bytes+len(encoded) > maxBytes {
			result.Truncated = true
			break
		}
		result.Records = append(result.Records, item.record)
		result.Bytes += len(encoded)
	}
	if len(result.Records) < result.Total {
		result.Truncated = true
	}
	if err := result.Validate(store.policy); err != nil {
		return MemoryRetrievalResult{}, fmt.Errorf("validate retrieval result: %w", err)
	}
	return result, nil
}

// RetrieveContext performs exact retrieval and projects it into a separately
// validated transient context packet. It does not persist the packet.
func (store *LocalMemoryStore) RetrieveContext(ctx context.Context, query MemoryQuery, sessionID string) (MemoryContextPacket, error) {
	result, err := store.Retrieve(ctx, query)
	if err != nil {
		return MemoryContextPacket{}, err
	}
	return ProjectMemoryContext(store.policy, query.Actor, sessionID, result)
}

// ProjectMemoryContext turns a validated retrieval result into a bounded,
// provider-neutral transient projection.
func ProjectMemoryContext(policy MemoryPolicy, actor MemoryActor, sessionID string, result MemoryRetrievalResult) (MemoryContextPacket, error) {
	if err := result.Validate(policy); err != nil {
		return MemoryContextPacket{}, err
	}
	packet := MemoryContextPacket{SessionID: sessionID}
	for _, record := range result.Records {
		item := MemoryContextItem{RecordID: record.ID, Namespace: record.Namespace, Key: record.Key, Content: record.Content, Summary: record.Summary, Sensitivity: record.Sensitivity}
		item.Updated, item.Status = memoryContextProvenance(record)
		encoded, err := json.Marshal(item)
		if err != nil {
			return MemoryContextPacket{}, fmt.Errorf("encode memory context item: %w", err)
		}
		if len(packet.Items) >= policy.MaxContextRecords || packet.UsedBytes+len(encoded) > policy.MaxContextBytes {
			packet.Truncated = true
			break
		}
		packet.Items = append(packet.Items, item)
		packet.UsedBytes += len(encoded)
	}
	if len(packet.Items) < len(result.Records) {
		packet.Truncated = true
	}
	if err := packet.Validate(policy, actor); err != nil {
		return MemoryContextPacket{}, err
	}
	return packet, nil
}

// AcquireWriterLock explicitly acquires the cross-process writer lock. Locks
// are never reclaimed automatically: malformed, stale, or live-looking lock
// metadata fails closed and requires explicit operator intervention.
func (store *LocalMemoryStore) AcquireWriterLock(ctx context.Context) (*MemoryWriterLock, error) {
	if store == nil {
		return nil, errors.New("memory store is nil")
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	return store.acquireFileLockLocked(ctx, memoryWriterLockFile)
}

type MemoryWriterLock struct {
	path  string
	token string
	once  sync.Once
	err   error
}

func (lock *MemoryWriterLock) Release() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		file, err := os.Open(lock.path)
		if err != nil {
			lock.err = fmt.Errorf("read writer lock for release: %w", err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, memoryMaxLockBytes+1))
		if err != nil {
			lock.err = fmt.Errorf("read writer lock for release: %w", err)
			return
		}
		if len(data) > memoryMaxLockBytes {
			lock.err = errors.New("writer lock metadata is oversized; refusing release")
			return
		}
		var metadata memoryLockMetadata
		if err := json.Unmarshal(data, &metadata); err != nil || metadata.Token != lock.token {
			lock.err = errors.New("writer lock ownership changed; refusing release")
			return
		}
		if err := os.Remove(lock.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			lock.err = fmt.Errorf("release writer lock: %w", err)
		}
	})
	return lock.err
}

func (store *LocalMemoryStore) acquireFileLockLocked(ctx context.Context, filename string) (*MemoryWriterLock, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	path := filepath.Join(store.root, filename)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("writer lock is a symlink: %w", ErrMemoryLockHeld)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > memoryMaxLockBytes {
			return nil, fmt.Errorf("writer lock metadata is unsafe or oversized: %w", ErrMemoryLockHeld)
		}
		return nil, fmt.Errorf("writer lock exists (including stale or ambiguous locks): %w", ErrMemoryLockHeld)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect writer lock: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	metadata := memoryLockMetadata{PID: os.Getpid(), Token: token, CreatedAt: time.Now().UTC()}
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("writer lock contention: %w", ErrMemoryLockHeld)
		}
		return nil, fmt.Errorf("create writer lock: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write writer lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("sync writer lock: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close writer lock: %w", err)
	}
	return &MemoryWriterLock{path: path, token: token}, nil
}

type memoryLockMetadata struct {
	PID       int       `json:"pid"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

func (metadata memoryLockMetadata) validate() error {
	if metadata.PID <= 0 || metadata.PID > 1<<30 {
		return errors.New("writer lock pid is invalid")
	}
	if err := validateSafeID("writer lock token", metadata.Token, 128); err != nil {
		return err
	}
	return validateTimestamp("writer lock created_at", metadata.CreatedAt, true)
}

// ReplayDiagnostics is a bounded snapshot of the last successful replay.
type MemoryReplayDiagnostics struct {
	Events     int `json:"events"`
	Records    int `json:"records"`
	Tombstones int `json:"tombstones"`
}

func (store *LocalMemoryStore) ReplayDiagnostics() (MemoryReplayDiagnostics, error) {
	if store == nil {
		return MemoryReplayDiagnostics{}, errors.New("memory store is nil")
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	if err := store.replayLocked(); err != nil {
		return MemoryReplayDiagnostics{}, err
	}
	diagnostics := MemoryReplayDiagnostics{Records: len(store.state)}
	for _, state := range store.state {
		if state.deleted {
			diagnostics.Tombstones++
		}
	}
	if info, err := os.Stat(filepath.Join(store.root, memoryEventsFile)); err == nil && info.Size() > 0 {
		count, err := countJSONLLines(filepath.Join(store.root, memoryEventsFile), memoryMaxEventLine)
		if err != nil {
			return MemoryReplayDiagnostics{}, err
		}
		diagnostics.Events = count
	}
	return diagnostics, nil
}

func (store *LocalMemoryStore) replayLocked() error {
	path := filepath.Join(store.root, memoryEventsFile)
	if err := validatePrivateMetadata(path, false, MaxMemoryBytes); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return fmt.Errorf("validate memory event log: %w: %v", ErrMemoryCorrupt, err)
		}
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if store.replayLoaded && !store.replayExists {
			return nil
		}
		store.state = make(map[string]memoryState)
		store.eventIDs = make(map[string]struct{})
		store.replayLoaded = true
		store.replayExists = false
		store.replaySize = 0
		store.replayMtimeNS = 0
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat memory event log: %w", err)
	}
	if info.Size() > int64(store.policy.MaxBytes) {
		return fmt.Errorf("memory event log exceeds configured %d bytes: %w", store.policy.MaxBytes, ErrMemoryCorrupt)
	}
	if info.Size() > int64(MaxMemoryBytes) {
		return fmt.Errorf("memory event log exceeds %d bytes: %w", MaxMemoryBytes, ErrMemoryCorrupt)
	}
	if store.replayLoaded && store.replayExists && store.replaySize == info.Size() && store.replayMtimeNS == info.ModTime().UnixNano() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open memory event log: %w", err)
	}
	defer file.Close()
	state := make(map[string]memoryState)
	seenEvents := make(map[string]struct{})
	reader := bufio.NewReaderSize(file, 64<<10)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := readBoundedLine(reader, memoryMaxEventLine)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return fmt.Errorf("read memory event log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, readErr)
		}
		var event MemoryEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode memory event log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, err)
		}
		if err := event.ValidateForStore(store.policy); err != nil {
			return fmt.Errorf("validate memory event log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, err)
		}
		if _, exists := seenEvents[event.ID]; exists {
			return fmt.Errorf("duplicate memory event id %q: %w", event.ID, ErrMemoryCorrupt)
		}
		seenEvents[event.ID] = struct{}{}
		if err := applyMemoryEvent(state, event); err != nil {
			return fmt.Errorf("replay memory event line %d: %w: %v", lineNumber, ErrMemoryCorrupt, err)
		}
		if len(state) > store.policy.MaxRecords {
			return fmt.Errorf("replay memory event line %d exceeds %d records: %w", lineNumber, store.policy.MaxRecords, ErrMemoryCorrupt)
		}
	}
	store.state = state
	store.eventIDs = seenEvents
	store.replayLoaded = true
	store.replayExists = true
	store.replaySize = info.Size()
	store.replayMtimeNS = info.ModTime().UnixNano()
	return nil
}

func (store *LocalMemoryStore) cacheReplayMetadataLocked(path string) {
	info, err := os.Stat(path)
	if err != nil {
		store.replayLoaded = false
		return
	}
	store.replayLoaded = true
	store.replayExists = true
	store.replaySize = info.Size()
	store.replayMtimeNS = info.ModTime().UnixNano()
}

func (store *LocalMemoryStore) invalidateReplayCache() {
	if store == nil {
		return
	}
	store.mu.mu.Lock()
	store.replayLoaded = false
	store.mu.mu.Unlock()
}

func cloneMemoryState(source map[string]memoryState) map[string]memoryState {
	clone := make(map[string]memoryState, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func applyMemoryEvent(state map[string]memoryState, event MemoryEvent) error {
	switch event.Type {
	case MemoryEventCreated, MemoryEventImported:
		if event.Type == MemoryEventCreated && event.Record.Version != 1 {
			return errors.New("created memory record version must be 1")
		}
		if event.Type == MemoryEventImported && event.Record.Version == 0 {
			return errors.New("imported memory record version must be positive")
		}
		if _, exists := state[event.Record.ID]; exists {
			return fmt.Errorf("memory record %q already exists: %w", event.Record.ID, ErrMemoryConflict)
		}
		state[event.Record.ID] = memoryState{record: event.Record, version: event.Record.Version}
	case MemoryEventUpdated:
		current, exists := state[event.RecordID]
		if !exists || current.deleted {
			return fmt.Errorf("memory record %q cannot be updated: %w", event.RecordID, ErrMemoryConflict)
		}
		if current.version != event.ExpectedVersion {
			return fmt.Errorf("memory record %q expected version %d, current version %d: %w", event.RecordID, event.ExpectedVersion, current.version, ErrMemoryConflict)
		}
		if event.NewVersion != event.ExpectedVersion+1 {
			return errors.New("updated memory record version must increment by one")
		}
		if !event.Record.CreatedAt.Equal(current.record.CreatedAt) || event.Record.CreatedBy != current.record.CreatedBy {
			return errors.New("updated memory record identity metadata must remain stable")
		}
		if event.Record.Version != event.NewVersion {
			return errors.New("updated memory record version does not match new_version")
		}
		state[event.RecordID] = memoryState{record: event.Record, version: event.NewVersion}
	case MemoryEventDeleted:
		current, exists := state[event.RecordID]
		if !exists || current.deleted {
			return fmt.Errorf("memory record %q cannot be deleted: %w", event.RecordID, ErrMemoryConflict)
		}
		if current.version != event.ExpectedVersion {
			return fmt.Errorf("memory record %q expected version %d, current version %d: %w", event.RecordID, event.ExpectedVersion, current.version, ErrMemoryConflict)
		}
		state[event.RecordID] = memoryState{record: current.record, version: event.ExpectedVersion + 1, deleted: true}
	case MemoryEventConflict:
		// Conflict records are audit-only and intentionally do not mutate state.
	default:
		return errors.New("unsupported durable memory event type")
	}
	return nil
}

func isMemoryExpired(record MemoryRecord, policy MemoryPolicy, asOf time.Time) bool {
	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(asOf) {
		return true
	}
	return policy.Retention > 0 && !record.CreatedAt.Add(policy.Retention).After(asOf)
}

func exactMemoryMatch(record MemoryRecord, query string) (int, bool) {
	queryTokens := memoryTokens(query)
	if len(queryTokens) == 0 {
		return 0, true
	}
	fields := []string{record.ID, string(record.Namespace), record.Key, record.SubjectID, record.Content, record.Summary, record.Source}
	for key, value := range record.Tags {
		fields = append(fields, key, value)
	}
	fieldTokens := make(map[string]struct{})
	whole := make(map[string]struct{})
	for _, field := range fields {
		whole[strings.ToLower(strings.TrimSpace(field))] = struct{}{}
		for _, token := range memoryTokens(field) {
			fieldTokens[token] = struct{}{}
		}
	}
	matched := 0
	for _, token := range queryTokens {
		if _, ok := fieldTokens[token]; ok {
			matched++
		}
	}
	if matched == len(queryTokens) {
		return matched, true
	}
	if _, ok := whole[strings.ToLower(strings.TrimSpace(query))]; ok {
		return len(queryTokens) + 1, true
	}
	return matched, false
}

func memoryTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func preparePrivateStorageRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("memory storage root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve memory storage root: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := rejectRepositoryOverlap(abs); err != nil {
		return "", err
	}
	if err := ensurePrivatePathComponents(abs); err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create memory storage root: %w", err)
	}
	if err := enforcePrivateDir(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func ensurePrivatePathComponents(path string) error {
	current := path
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("memory storage path component %q is a symlink", current)
			}
			// The caller-provided root itself must be private. Ancestors may be
			// shared system directories such as /tmp; their permissions do not
			// grant access to the private root.
			if current == path && info.IsDir() && info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("memory storage directory %q has unsafe permissions %o", current, info.Mode().Perm())
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect memory storage path %q: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func rejectRepositoryOverlap(path string) error {
	current := path
	for {
		if info, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("memory storage root overlaps repository with symlinked .git metadata")
			}
			// Some restricted test/container environments place an empty .git
			// sentinel at /tmp. It is not repository metadata and must not make
			// every private temporary memory root unusable.
			if info.IsDir() {
				entries, readErr := os.ReadDir(filepath.Join(current, ".git"))
				if readErr != nil {
					return fmt.Errorf("inspect repository metadata: %w", readErr)
				}
				if len(entries) == 0 && filepath.Clean(current) == filepath.Clean(os.TempDir()) {
					goto nextParent
				}
			}
			if isMarkedBeeWorkspaceStorage(path, current) {
				return nil
			}
			return fmt.Errorf("memory storage root %q overlaps repository/worktree %q", path, current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect repository metadata: %w", err)
		}
	nextParent:
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

func isMarkedBeeWorkspaceStorage(path, repositoryRoot string) bool {
	workspace, err := DefaultBeeWorkspaceRoot()
	if err != nil {
		return false
	}
	workspace, _ = filepath.Abs(filepath.Clean(workspace))
	path, _ = filepath.Abs(filepath.Clean(path))
	repositoryRoot, _ = filepath.Abs(filepath.Clean(repositoryRoot))
	if repositoryRoot != workspace || (path != workspace && !strings.HasPrefix(path, workspace+string(filepath.Separator))) {
		return false
	}
	marker := filepath.Join(workspace, ".bee-workspace")
	info, err := os.Lstat(marker)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600
}

func enforcePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat private directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("memory storage root is not a private directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("memory storage root has unsafe permissions %o", info.Mode().Perm())
	}
	return nil
}

func validatePrivateMetadata(path string, directory bool, maxBytes int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect private metadata %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private metadata %q is a symlink", path)
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("private metadata directory %q is unsafe", path)
		}
		return nil
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private metadata file %q is unsafe", path)
	}
	if maxBytes > 0 && info.Size() > int64(maxBytes) {
		return fmt.Errorf("private metadata file %q exceeds %d bytes", path, maxBytes)
	}
	return nil
}

func openPrivateAppend(path string) (*os.File, error) {
	if err := validatePrivateMetadata(path, false, MaxMemoryBytes); err != nil && !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(path); statErr == nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("memory event log has unsafe permissions or type")
	}
	return file, nil
}

func readBoundedLine(reader *bufio.Reader, max int) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if len(line) > max {
		return nil, errors.New("line exceeds bounded event size")
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil, io.EOF
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("truncated final JSONL record")
		}
		return nil, err
	}
	return bytesTrimSuffixNewline(line), nil
}

func bytesTrimSuffixNewline(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func countJSONLLines(path string, maxLine int) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	count := 0
	for {
		_, err := readBoundedLine(reader, maxLine)
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
		count++
	}
}

func atomicWritePrivateJSON(path string, value any) error {
	if err := validatePrivateMetadata(path, false, memoryMaxSnapshotSize); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return err
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > memoryMaxSnapshotSize {
		return fmt.Errorf("snapshot exceeds %d bytes", memoryMaxSnapshotSize)
	}
	dir := filepath.Dir(path)
	if err := ensurePrivatePathComponents(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := enforcePrivateDir(dir); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+token)
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func readPrivateJSON(path string, value any) error {
	if err := validatePrivateMetadata(path, false, memoryMaxSnapshotSize); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode private snapshot: %w", err)
	}
	return nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func randomToken() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate storage token: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

// LocalMemoryProposalSink appends validated child proposals to a separate log.
// It never mutates the durable-memory record index or event log.
type LocalMemoryProposalSink struct {
	root        string
	policy      MemoryPolicy
	mu          *memorySharedLock
	proposalIDs map[string]struct{}
}

var _ MemoryProposalSink = (*LocalMemoryProposalSink)(nil)

func NewLocalMemoryProposalSink(root string, policy MemoryPolicy) (*LocalMemoryProposalSink, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("memory policy: %w", err)
	}
	abs, err := preparePrivateStorageRoot(root)
	if err != nil {
		return nil, err
	}
	shared := &memorySharedLock{}
	actual, _ := memoryProcessLocks.LoadOrStore(abs, shared)
	sink := &LocalMemoryProposalSink{root: abs, policy: policy, mu: actual.(*memorySharedLock), proposalIDs: make(map[string]struct{})}
	sink.mu.mu.Lock()
	defer sink.mu.mu.Unlock()
	if err := sink.replayLocked(); err != nil {
		return nil, err
	}
	return sink, nil
}

func (sink *LocalMemoryProposalSink) Root() string {
	if sink == nil {
		return ""
	}
	return sink.root
}

// AcquireWriterLock reserves proposal/review writes for a higher-level local
// transaction. The caller must release the returned lock explicitly.
func (sink *LocalMemoryProposalSink) AcquireWriterLock(ctx context.Context) (*MemoryWriterLock, error) {
	if sink == nil {
		return nil, errors.New("memory proposal sink is nil")
	}
	sink.mu.mu.Lock()
	defer sink.mu.mu.Unlock()
	return (&LocalMemoryStore{root: sink.root, policy: sink.policy, mu: sink.mu}).acquireFileLockLocked(ctx, proposalWriterLockFile)
}

func (sink *LocalMemoryProposalSink) SubmitProposal(ctx context.Context, proposal MemoryProposal) error {
	if sink == nil {
		return errors.New("memory proposal sink is nil")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := proposal.Validate(sink.policy); err != nil {
		return fmt.Errorf("validate memory proposal: %w", err)
	}
	sink.mu.mu.Lock()
	defer sink.mu.mu.Unlock()
	lock, err := (&LocalMemoryStore{root: sink.root, policy: sink.policy, mu: sink.mu}).acquireFileLockLocked(ctx, proposalWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := sink.replayLocked(); err != nil {
		return err
	}
	if _, exists := sink.proposalIDs[proposal.ID]; exists {
		return fmt.Errorf("memory proposal %q already exists: %w", proposal.ID, ErrMemoryConflict)
	}
	path := filepath.Join(sink.root, memoryProposalsFile)
	file, err := openPrivateAppend(path)
	if err != nil {
		return err
	}
	defer file.Close()
	line, err := json.Marshal(proposal)
	if err != nil {
		return err
	}
	if len(line) > memoryMaxEventLine {
		return fmt.Errorf("memory proposal exceeds %d bytes", memoryMaxEventLine)
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(line)+1) > int64(sink.policy.MaxBytes) || info.Size()+int64(len(line)+1) > int64(MaxMemoryBytes) {
		return errors.New("memory proposal log exceeds configured or absolute byte limit")
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	sink.proposalIDs[proposal.ID] = struct{}{}
	return nil
}

func (sink *LocalMemoryProposalSink) replayLocked() error {
	path := filepath.Join(sink.root, memoryProposalsFile)
	seen := make(map[string]struct{})
	sink.proposalIDs = seen
	if err := validatePrivateMetadata(path, false, MaxMemoryBytes); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return err
		}
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := readBoundedLine(reader, memoryMaxEventLine)
		if errors.Is(readErr, io.EOF) {
			sink.proposalIDs = seen
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read proposal log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, readErr)
		}
		var proposal MemoryProposal
		if err := json.Unmarshal(line, &proposal); err != nil {
			return fmt.Errorf("decode proposal log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, err)
		}
		if err := proposal.Validate(sink.policy); err != nil {
			return fmt.Errorf("validate proposal log line %d: %w: %v", lineNumber, ErrMemoryCorrupt, err)
		}
		if _, exists := seen[proposal.ID]; exists {
			return fmt.Errorf("duplicate proposal id %q: %w", proposal.ID, ErrMemoryCorrupt)
		}
		seen[proposal.ID] = struct{}{}
	}
}

// IdentityStore and UserProfileStore intentionally use snapshot files and do
// not share the durable-memory event log.
type IdentityStore interface {
	Load(context.Context) (IdentityProfile, error)
	Save(context.Context, IdentityProfile) error
}

type UserProfileStore interface {
	Load(context.Context, string) (UserProfile, error)
	Save(context.Context, UserProfile) error
}

type LocalIdentityStore struct {
	root string
	mu   *memorySharedLock
}

var _ IdentityStore = (*LocalIdentityStore)(nil)

func NewLocalIdentityStore(root string) (*LocalIdentityStore, error) {
	abs, err := preparePrivateStorageRoot(root)
	if err != nil {
		return nil, err
	}
	shared := &memorySharedLock{}
	actual, _ := memoryProcessLocks.LoadOrStore(abs, shared)
	return &LocalIdentityStore{root: abs, mu: actual.(*memorySharedLock)}, nil
}

func NewLocalIdentityProfileStore(root string) (*LocalIdentityStore, error) {
	return NewLocalIdentityStore(root)
}

func (store *LocalIdentityStore) Load(ctx context.Context) (IdentityProfile, error) {
	if store == nil {
		return IdentityProfile{}, errors.New("identity store is nil")
	}
	if err := contextErr(ctx); err != nil {
		return IdentityProfile{}, err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	var profile IdentityProfile
	err := readPrivateJSON(filepath.Join(store.root, memoryIdentityFile), &profile)
	if errors.Is(err, os.ErrNotExist) {
		return IdentityProfile{}, fmt.Errorf("identity profile: %w", ErrMemoryNotFound)
	}
	if err != nil {
		return IdentityProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return IdentityProfile{}, fmt.Errorf("validate identity profile: %w", err)
	}
	return profile, nil
}

func (store *LocalIdentityStore) Save(ctx context.Context, profile IdentityProfile) error {
	if store == nil {
		return errors.New("identity store is nil")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("validate identity profile: %w", err)
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	lock, err := (&LocalMemoryStore{root: store.root, mu: store.mu}).acquireFileLockLocked(ctx, memoryWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	path := filepath.Join(store.root, memoryIdentityFile)
	if existing, err := store.loadLocked(); err == nil {
		if profile.Revision == 0 {
			profile.Revision = existing.Revision + 1
		} else if profile.Revision <= existing.Revision {
			return fmt.Errorf("identity profile revision %d must exceed current revision %d", profile.Revision, existing.Revision)
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrMemoryNotFound) {
		return err
	} else if profile.Revision == 0 {
		profile.Revision = 1
	}
	profile.UpdatedAt = profile.UpdatedAt.UTC()
	if err := profile.Validate(); err != nil {
		return err
	}
	return atomicWritePrivateJSON(path, profile)
}

func (store *LocalIdentityStore) loadLocked() (IdentityProfile, error) {
	var profile IdentityProfile
	err := readPrivateJSON(filepath.Join(store.root, memoryIdentityFile), &profile)
	if err != nil {
		return IdentityProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return IdentityProfile{}, err
	}
	return profile, nil
}

func (store *LocalIdentityStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

// Get and Put are convenience aliases for callers that prefer key/value names.
func (store *LocalIdentityStore) Get(ctx context.Context) (IdentityProfile, error) {
	return store.Load(ctx)
}
func (store *LocalIdentityStore) Put(ctx context.Context, profile IdentityProfile) error {
	return store.Save(ctx, profile)
}

type LocalUserProfileStore struct {
	root string
	mu   *memorySharedLock
}

var _ UserProfileStore = (*LocalUserProfileStore)(nil)

func NewLocalUserProfileStore(root string) (*LocalUserProfileStore, error) {
	abs, err := preparePrivateStorageRoot(root)
	if err != nil {
		return nil, err
	}
	profiles := filepath.Join(abs, memoryProfilesDir)
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		return nil, err
	}
	if err := enforcePrivateDir(profiles); err != nil {
		return nil, err
	}
	shared := &memorySharedLock{}
	actual, _ := memoryProcessLocks.LoadOrStore(abs, shared)
	return &LocalUserProfileStore{root: abs, mu: actual.(*memorySharedLock)}, nil
}

func (store *LocalUserProfileStore) Load(ctx context.Context, id string) (UserProfile, error) {
	if store == nil {
		return UserProfile{}, errors.New("user profile store is nil")
	}
	if err := validateSafeID("user profile id", id, MaxMemoryIDLength); err != nil {
		return UserProfile{}, err
	}
	if err := contextErr(ctx); err != nil {
		return UserProfile{}, err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	var profile UserProfile
	err := readPrivateJSON(store.path(id), &profile)
	if errors.Is(err, os.ErrNotExist) {
		return UserProfile{}, fmt.Errorf("user profile: %w", ErrMemoryNotFound)
	}
	if err != nil {
		return UserProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return UserProfile{}, fmt.Errorf("validate user profile: %w", err)
	}
	if profile.ID != id {
		return UserProfile{}, errors.New("user profile id does not match path")
	}
	return profile, nil
}

func (store *LocalUserProfileStore) Save(ctx context.Context, profile UserProfile) error {
	if store == nil {
		return errors.New("user profile store is nil")
	}
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("validate user profile: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	store.mu.mu.Lock()
	defer store.mu.mu.Unlock()
	lock, err := (&LocalMemoryStore{root: store.root, mu: store.mu}).acquireFileLockLocked(ctx, memoryWriterLockFile)
	if err != nil {
		return err
	}
	defer lock.Release()
	path := store.path(profile.ID)
	if existing, err := store.loadLocked(profile.ID); err == nil {
		if profile.Revision == 0 {
			profile.Revision = existing.Revision + 1
		} else if profile.Revision <= existing.Revision {
			return fmt.Errorf("user profile revision %d must exceed current revision %d", profile.Revision, existing.Revision)
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrMemoryNotFound) {
		return err
	} else if profile.Revision == 0 {
		profile.Revision = 1
	}
	profile.UpdatedAt = profile.UpdatedAt.UTC()
	if err := profile.Validate(); err != nil {
		return err
	}
	return atomicWritePrivateJSON(path, profile)
}

func (store *LocalUserProfileStore) loadLocked(id string) (UserProfile, error) {
	var profile UserProfile
	if err := readPrivateJSON(store.path(id), &profile); err != nil {
		return UserProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return UserProfile{}, err
	}
	if profile.ID != id {
		return UserProfile{}, errors.New("user profile id does not match path")
	}
	return profile, nil
}

func (store *LocalUserProfileStore) path(id string) string {
	return filepath.Join(store.root, memoryProfilesDir, id+".json")
}

func (store *LocalUserProfileStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *LocalUserProfileStore) Get(ctx context.Context, id string) (UserProfile, error) {
	return store.Load(ctx, id)
}
func (store *LocalUserProfileStore) Put(ctx context.Context, profile UserProfile) error {
	return store.Save(ctx, profile)
}
