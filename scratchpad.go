package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ScratchpadDefaultTTL     = time.Hour
	ScratchpadMinTTL         = time.Second
	ScratchpadMaxTTL         = 7 * 24 * time.Hour
	ScratchpadMaxKeyBytes    = 128
	ScratchpadMaxValueBytes  = 16 << 10
	ScratchpadMaxEntries     = 64
	ScratchpadMaxTotalBytes  = 256 << 10
	ScratchpadMaxOutputBytes = 24 << 10
	ScratchpadMaxListItems   = 64
	ScratchpadSnapshotSlack  = ScratchpadMaxEntries * 512
)

const scratchpadMaxTTLSeconds = int(ScratchpadMaxTTL / time.Second)

const ScratchpadMaxSnapshotBytes = ScratchpadMaxTotalBytes + ScratchpadSnapshotSlack

var (
	ErrScratchpadSessionRequired = errors.New("scratchpad requires a durable session context")
	ErrScratchpadNotFound        = errors.New("scratchpad entry not found")
)

type ScratchpadClock func() time.Time

type ScratchpadEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ScratchpadMetadata struct {
	Key       string    `json:"key"`
	SizeBytes int       `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type scratchpadSnapshot struct {
	Version int                        `json:"version"`
	Entries map[string]ScratchpadEntry `json:"entries,omitempty"`
}

type ScratchpadStore struct {
	root     string
	sessions *SessionStore
	clock    ScratchpadClock
	mu       sync.Mutex
}

func NewScratchpadStore(root string, sessions *SessionStore) (*ScratchpadStore, error) {
	if sessions == nil {
		return nil, errors.New("scratchpad session store is required")
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("scratchpad root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve scratchpad root: %w", err)
	}
	if err := ValidateBeePath(abs); err != nil {
		return nil, err
	}
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create scratchpad root: %w", err)
	}
	return &ScratchpadStore{root: filepath.Clean(abs), sessions: sessions, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func NewDefaultScratchpadStore(sessions *SessionStore) (*ScratchpadStore, error) {
	if sessions == nil {
		return nil, errors.New("scratchpad session store is required")
	}
	return NewScratchpadStore(filepath.Join(sessions.Root(), "scratchpad"), sessions)
}

func (store *ScratchpadStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}
func (store *ScratchpadStore) SetClock(clock ScratchpadClock) {
	if store != nil && clock != nil {
		store.clock = clock
	}
}

func (store *ScratchpadStore) path(sessionID SessionID) string {
	return filepath.Join(store.root, string(sessionID)+".json")
}
func (store *ScratchpadStore) lockPath(sessionID SessionID) string {
	return store.path(sessionID) + ".lock"
}

func (store *ScratchpadStore) validateSession(ctx context.Context) (SessionID, error) {
	if store == nil || store.sessions == nil {
		return "", errors.New("scratchpad store is not configured")
	}
	id, ok := SessionIDFromContext(ctx)
	if !ok || strings.TrimSpace(string(id)) == "" {
		return "", ErrScratchpadSessionRequired
	}
	if err := validateSessionID(id); err != nil {
		return "", err
	}
	if _, err := store.sessions.Load(id); err != nil {
		return "", fmt.Errorf("scratchpad session: %w", err)
	}
	return id, nil
}

func (store *ScratchpadStore) withSnapshot(ctx context.Context, sessionID SessionID, mutate func(*scratchpadSnapshot) error) (scratchpadSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	lockPath := store.lockPath(sessionID)
	if info, statErr := os.Lstat(lockPath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return scratchpadSnapshot{}, errors.New("scratchpad lock must not be a symlink")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return scratchpadSnapshot{}, fmt.Errorf("inspect scratchpad lock: %w", statErr)
	}
	lock, err := acquireSessionFileLock(lockPath)
	if err != nil {
		return scratchpadSnapshot{}, err
	}
	defer lock.Close()
	if err := rejectBeeSymlinkAncestors(store.path(sessionID)); err != nil {
		return scratchpadSnapshot{}, err
	}
	snap, err := store.loadSnapshot(sessionID)
	if err != nil {
		return scratchpadSnapshot{}, err
	}
	pruned, err := pruneScratchpad(&snap, store.clock().UTC())
	if err != nil {
		return scratchpadSnapshot{}, err
	}
	if mutate != nil {
		if err := mutate(&snap); err != nil {
			return scratchpadSnapshot{}, err
		}
		if err := store.saveSnapshot(sessionID, snap); err != nil {
			return scratchpadSnapshot{}, err
		}
	} else if pruned {
		if err := store.saveSnapshot(sessionID, snap); err != nil {
			return scratchpadSnapshot{}, err
		}
	}
	return snap, nil
}

func (store *ScratchpadStore) loadSnapshot(sessionID SessionID) (scratchpadSnapshot, error) {
	path := store.path(sessionID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return scratchpadSnapshot{Version: 1, Entries: map[string]ScratchpadEntry{}}, nil
	}
	if err != nil {
		return scratchpadSnapshot{}, fmt.Errorf("stat scratchpad: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return scratchpadSnapshot{}, errors.New("scratchpad snapshot must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return scratchpadSnapshot{}, fmt.Errorf("protect scratchpad snapshot: %w", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return scratchpadSnapshot{}, fmt.Errorf("read scratchpad: %w", err)
	}
	if len(data) > ScratchpadMaxSnapshotBytes {
		return scratchpadSnapshot{}, errors.New("scratchpad snapshot exceeds bounds")
	}
	var snap scratchpadSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return scratchpadSnapshot{}, fmt.Errorf("decode scratchpad: %w", err)
	}
	if snap.Version == 0 {
		snap.Version = 1
	}
	if snap.Version != 1 {
		return scratchpadSnapshot{}, errors.New("unsupported scratchpad snapshot version")
	}
	if snap.Entries == nil {
		snap.Entries = map[string]ScratchpadEntry{}
	}
	if err := validateScratchpadSnapshot(&snap); err != nil {
		return scratchpadSnapshot{}, err
	}
	return snap, nil
}

func validateScratchpadSnapshot(snap *scratchpadSnapshot) error {
	if len(snap.Entries) > ScratchpadMaxEntries {
		return errors.New("scratchpad snapshot has too many entries")
	}
	total := 0
	for key, entry := range snap.Entries {
		if err := validateSafeKey("scratchpad key", key, ScratchpadMaxKeyBytes); err != nil {
			return err
		}
		if entry.Key != key {
			return errors.New("scratchpad entry key does not match map key")
		}
		if entry.ExpiresAt.IsZero() || entry.UpdatedAt.IsZero() || !entry.ExpiresAt.After(entry.UpdatedAt) {
			return errors.New("scratchpad entry timestamps are invalid")
		}
		if err := validateText("scratchpad value", entry.Value, ScratchpadMaxValueBytes, true); err != nil {
			return err
		}
		total += len(entry.Value)
		if total > ScratchpadMaxTotalBytes {
			return errors.New("scratchpad snapshot exceeds byte quota")
		}
	}
	return nil
}

func pruneScratchpad(snap *scratchpadSnapshot, now time.Time) (bool, error) {
	if err := validateScratchpadSnapshot(snap); err != nil {
		return false, err
	}
	changed := false
	for key, entry := range snap.Entries {
		if !entry.ExpiresAt.After(now) {
			delete(snap.Entries, key)
			changed = true
		}
	}
	return changed, nil
}

func (store *ScratchpadStore) saveSnapshot(sessionID SessionID, snap scratchpadSnapshot) error {
	if _, err := pruneScratchpad(&snap, store.clock().UTC()); err != nil {
		return err
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if len(data) > ScratchpadMaxSnapshotBytes {
		return errors.New("scratchpad snapshot exceeds bounds")
	}
	path := store.path(sessionID)
	tmp, err := os.CreateTemp(store.root, ".scratchpad-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	n, err := tmp.Write(data)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n != len(data) {
		_ = tmp.Close()
		return errors.New("short write while saving scratchpad")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if dir, openErr := os.Open(store.root); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (store *ScratchpadStore) Set(ctx context.Context, key, value string, ttl time.Duration) (ScratchpadMetadata, error) {
	sessionID, err := store.validateSession(ctx)
	if err != nil {
		return ScratchpadMetadata{}, err
	}
	if err := validateSafeKey("scratchpad key", key, ScratchpadMaxKeyBytes); err != nil {
		return ScratchpadMetadata{}, err
	}
	if err := validateText("scratchpad value", value, ScratchpadMaxValueBytes, true); err != nil {
		return ScratchpadMetadata{}, err
	}
	ttlSeconds := int64(ttl / time.Second)
	if ttl == 0 {
		ttlSeconds = 0
	}
	if ttl < 0 || ttl%time.Second != 0 || ttlSeconds > int64(scratchpadMaxTTLSeconds) {
		return ScratchpadMetadata{}, fmt.Errorf("scratchpad ttl must be between %s and %s", ScratchpadMinTTL, ScratchpadMaxTTL)
	}
	if ttl == 0 {
		ttl = ScratchpadDefaultTTL
	} else if ttlSeconds < int64(ScratchpadMinTTL/time.Second) {
		return ScratchpadMetadata{}, fmt.Errorf("scratchpad ttl must be between %s and %s", ScratchpadMinTTL, ScratchpadMaxTTL)
	}
	now := store.clock().UTC()
	entry := ScratchpadEntry{Key: key, Value: value, UpdatedAt: now, ExpiresAt: now.Add(ttl)}
	snap, err := store.withSnapshot(ctx, sessionID, func(s *scratchpadSnapshot) error {
		if _, ok := s.Entries[key]; !ok && len(s.Entries) >= ScratchpadMaxEntries {
			return errors.New("scratchpad entry quota exceeded")
		}
		total := 0
		for k, existing := range s.Entries {
			if k != key {
				total += len(existing.Value)
			}
		}
		if total+len(value) > ScratchpadMaxTotalBytes {
			return errors.New("scratchpad byte quota exceeded")
		}
		s.Entries[key] = entry
		return nil
	})
	if err != nil {
		return ScratchpadMetadata{}, err
	}
	_ = snap
	return ScratchpadMetadata{Key: key, SizeBytes: len(value), UpdatedAt: entry.UpdatedAt, ExpiresAt: entry.ExpiresAt}, nil
}

func (store *ScratchpadStore) Get(ctx context.Context, key string) (ScratchpadEntry, error) {
	sessionID, err := store.validateSession(ctx)
	if err != nil {
		return ScratchpadEntry{}, err
	}
	if err := validateSafeKey("scratchpad key", key, ScratchpadMaxKeyBytes); err != nil {
		return ScratchpadEntry{}, err
	}
	snap, err := store.withSnapshot(ctx, sessionID, nil)
	if err != nil {
		return ScratchpadEntry{}, err
	}
	entry, ok := snap.Entries[key]
	if !ok {
		return ScratchpadEntry{}, ErrScratchpadNotFound
	}
	entry.Value = truncateUTF8(entry.Value, ScratchpadMaxValueBytes)
	return entry, nil
}

func (store *ScratchpadStore) List(ctx context.Context) ([]ScratchpadMetadata, error) {
	sessionID, err := store.validateSession(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := store.withSnapshot(ctx, sessionID, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ScratchpadMetadata, 0, len(snap.Entries))
	for _, entry := range snap.Entries {
		out = append(out, ScratchpadMetadata{Key: entry.Key, SizeBytes: len(entry.Value), UpdatedAt: entry.UpdatedAt, ExpiresAt: entry.ExpiresAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > ScratchpadMaxListItems {
		out = out[:ScratchpadMaxListItems]
	}
	return out, nil
}

func (store *ScratchpadStore) Delete(ctx context.Context, key string) (bool, error) {
	sessionID, err := store.validateSession(ctx)
	if err != nil {
		return false, err
	}
	if err := validateSafeKey("scratchpad key", key, ScratchpadMaxKeyBytes); err != nil {
		return false, err
	}
	deleted := false
	_, err = store.withSnapshot(ctx, sessionID, func(s *scratchpadSnapshot) error {
		if _, ok := s.Entries[key]; ok {
			delete(s.Entries, key)
			deleted = true
		}
		return nil
	})
	return deleted, err
}

type scratchSetArgs struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}
type scratchKeyArgs struct {
	Key string `json:"key"`
}
type scratchSetResult struct {
	Key       string    `json:"key"`
	SizeBytes int       `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ScratchSetTool struct{ Store *ScratchpadStore }

func (tool ScratchSetTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "scratch_set", Description: "Store one bounded, private TTL scratchpad value in the current durable session. This is not durable memory and is never auto-promoted.", Risk: RiskMedium, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"},"value":{"type":"string"},"ttl_seconds":{"type":"integer"}},"required":["key","value"],"additionalProperties":false}`)}
}
func (tool ScratchSetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("scratch_set store is not configured")
	}
	var in scratchSetArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("scratch_set arguments: %w", err)
	}
	if in.TTLSeconds < 0 || in.TTLSeconds > scratchpadMaxTTLSeconds {
		return "", fmt.Errorf("scratch_set ttl_seconds must be between 1 and %d, or 0 for the default", scratchpadMaxTTLSeconds)
	}
	var ttl time.Duration
	if in.TTLSeconds != 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	meta, err := tool.Store.Set(ctx, in.Key, in.Value, ttl)
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(scratchSetResult{Key: meta.Key, SizeBytes: meta.SizeBytes, UpdatedAt: meta.UpdatedAt, ExpiresAt: meta.ExpiresAt}, ScratchpadMaxOutputBytes)
}

type ScratchGetTool struct{ Store *ScratchpadStore }

func (tool ScratchGetTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "scratch_get", Description: "Read one bounded TTL scratchpad value from the current durable session; expired values are pruned.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`)}
}
func (tool ScratchGetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("scratch_get store is not configured")
	}
	var in scratchKeyArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("scratch_get arguments: %w", err)
	}
	entry, err := tool.Store.Get(ctx, in.Key)
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(entry, ScratchpadMaxOutputBytes)
}

type ScratchListTool struct{ Store *ScratchpadStore }

func (tool ScratchListTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "scratch_list", Description: "List bounded scratchpad metadata for the current durable session without returning values.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (tool ScratchListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("scratch_list store is not configured")
	}
	if err := parseArgs(args, &struct{}{}); err != nil {
		return "", fmt.Errorf("scratch_list arguments: %w", err)
	}
	list, err := tool.Store.List(ctx)
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(list, ScratchpadMaxOutputBytes)
}

type ScratchDeleteTool struct{ Store *ScratchpadStore }

func (tool ScratchDeleteTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "scratch_delete", Description: "Delete one scratchpad value from the current durable session; absent keys are reported idempotently.", Risk: RiskMedium, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`)}
}
func (tool ScratchDeleteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Store == nil {
		return "", errors.New("scratch_delete store is not configured")
	}
	var in scratchKeyArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("scratch_delete arguments: %w", err)
	}
	deleted, err := tool.Store.Delete(ctx, in.Key)
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(map[string]any{"key": in.Key, "deleted": deleted}, ScratchpadMaxOutputBytes)
}

func marshalBoundedJSON(value any, max int) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > max {
		return "", fmt.Errorf("tool output exceeds %d bytes", max)
	}
	return string(data), nil
}
