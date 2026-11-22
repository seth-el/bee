package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxRouteBindingSessionIDBytes bounds the session identifier persisted in a
// route-binding state file. Session IDs are opaque to the channel layer, but
// still need a bounded, single-component representation for safe persistence.
const (
	MaxRouteBindingSessionIDBytes = 128
	MaxRouteBindingStateBytes     = 1 << 20
)

// ErrRouteBindingNotFound indicates that no durable session is bound to a
// validated route key.
var ErrRouteBindingNotFound = errors.New("route binding not found")

// ErrRouteBindingConflict indicates that a route is already bound to a
// different session ID.
var ErrRouteBindingConflict = errors.New("route binding conflict")

// RouteBindingStore durably maps one provider-neutral route to one Bee
// session. Bind is idempotent for the same session and rejects conflicting
// session IDs.
type RouteBindingStore interface {
	Lookup(context.Context, SessionRouteKey) (SessionID, error)
	Bind(context.Context, SessionRouteKey, SessionID) error
}

// RouteBindingRebinder is implemented by stores that can deliberately move a
// route to another session (a channel's /new or /resume). Bind keeps
// refusing implicit changes.
type RouteBindingRebinder interface {
	Rebind(context.Context, SessionRouteKey, SessionID) error
}

// LocalRouteBindingStore stores route bindings in one caller-selected JSON
// file. It is intentionally local-process synchronized only; atomic rename
// protects readers from partial files but does not provide cross-process
// locking or merge concurrent writers.
type LocalRouteBindingStore struct {
	path string
	mu   sync.Mutex
}

var _ RouteBindingStore = (*LocalRouteBindingStore)(nil)

// NewLocalRouteBindingStore creates a caller-rooted route-binding store. The
// file itself is created on the first successful Bind, with mode 0600. Its
// parent directory is created with mode 0700 when needed.
func NewLocalRouteBindingStore(path string) (*LocalRouteBindingStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("route binding store path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve route binding store path: %w", err)
	}
	abs = filepath.Clean(abs)
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return nil, errors.New("route binding store path must be a file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect route binding store path: %w", err)
	}
	return &LocalRouteBindingStore{path: abs}, nil
}

// Path returns the resolved caller-selected state-file path.
func (store *LocalRouteBindingStore) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

// Lookup returns the session bound to key, or ErrRouteBindingNotFound when the
// state file has no entry for that key.
func (store *LocalRouteBindingStore) Lookup(ctx context.Context, key SessionRouteKey) (SessionID, error) {
	if err := checkRouteBindingContext(ctx); err != nil {
		return "", err
	}
	if store == nil {
		return "", errors.New("route binding store is nil")
	}
	if err := validateRouteBindingKey(key); err != nil {
		return "", err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	bindings, err := store.read(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrRouteBindingNotFound
		}
		return "", err
	}
	sessionID, ok := bindings[key.String()]
	if !ok {
		return "", ErrRouteBindingNotFound
	}
	return SessionID(sessionID), nil
}

// Get is an alias for Lookup for callers that prefer key/value terminology.
func (store *LocalRouteBindingStore) Get(ctx context.Context, key SessionRouteKey) (SessionID, error) {
	return store.Lookup(ctx, key)
}

// Load is an alias for Lookup for callers that prefer session-store
// terminology.
func (store *LocalRouteBindingStore) Load(ctx context.Context, key SessionRouteKey) (SessionID, error) {
	return store.Lookup(ctx, key)
}

// Bind creates a route binding, or verifies an existing identical binding. A
// different existing session ID is a conflict and is never overwritten.
func (store *LocalRouteBindingStore) Bind(ctx context.Context, key SessionRouteKey, sessionID SessionID) error {
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	if store == nil {
		return errors.New("route binding store is nil")
	}
	if err := validateRouteBindingKey(key); err != nil {
		return err
	}
	if err := validateRouteBindingSessionID(sessionID); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	bindings, err := store.read(ctx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bindings == nil {
		bindings = make(map[string]string)
	}
	if existing, ok := bindings[key.String()]; ok {
		if existing == string(sessionID) {
			return nil
		}
		return fmt.Errorf("%w for %q: already bound to session %q", ErrRouteBindingConflict, key, existing)
	}
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	bindings[key.String()] = string(sessionID)
	return store.write(ctx, bindings)
}

// Rebind points key at sessionID, replacing any existing binding. It is the
// explicit owner action behind a channel's /new and /resume.
func (store *LocalRouteBindingStore) Rebind(ctx context.Context, key SessionRouteKey, sessionID SessionID) error {
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	if store == nil {
		return errors.New("route binding store is nil")
	}
	if err := validateRouteBindingKey(key); err != nil {
		return err
	}
	if err := validateRouteBindingSessionID(sessionID); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	bindings, err := store.read(ctx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bindings == nil {
		bindings = make(map[string]string)
	}
	if bindings[key.String()] == string(sessionID) {
		return nil
	}
	bindings[key.String()] = string(sessionID)
	return store.write(ctx, bindings)
}

// Set is an alias for Bind.
func (store *LocalRouteBindingStore) Set(ctx context.Context, key SessionRouteKey, sessionID SessionID) error {
	return store.Bind(ctx, key, sessionID)
}

// Put is an alias for Bind.
func (store *LocalRouteBindingStore) Put(ctx context.Context, key SessionRouteKey, sessionID SessionID) error {
	return store.Bind(ctx, key, sessionID)
}

func validateRouteBindingKey(key SessionRouteKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return nil
}

func validateRouteBindingSessionID(id SessionID) error {
	value := string(id)
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return errors.New("route binding session id is required")
	}
	if len(value) > MaxRouteBindingSessionIDBytes {
		return fmt.Errorf("route binding session id exceeds %d bytes", MaxRouteBindingSessionIDBytes)
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, `/\\`) || filepath.Base(value) != value || value == "." || value == ".." || containsControl(value) {
		return errors.New("route binding session id is not path-safe")
	}
	return nil
}

func checkRouteBindingContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("route binding context is nil")
	}
	return ctx.Err()
}

func (store *LocalRouteBindingStore) read(ctx context.Context) (map[string]string, error) {
	if err := checkRouteBindingContext(ctx); err != nil {
		return nil, err
	}
	info, err := os.Stat(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("inspect route binding state: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("route binding state path is a directory")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("route binding state permissions are %o, want 600", info.Mode().Perm())
	}
	if info.Size() > MaxRouteBindingStateBytes {
		return nil, fmt.Errorf("route binding state exceeds %d bytes", MaxRouteBindingStateBytes)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		return nil, fmt.Errorf("read route binding state: %w", err)
	}
	if err := checkRouteBindingContext(ctx); err != nil {
		return nil, err
	}
	var bindings map[string]string
	if err := json.Unmarshal(data, &bindings); err != nil {
		return nil, fmt.Errorf("decode route binding state: %w", err)
	}
	if bindings == nil {
		return nil, errors.New("decode route binding state: root must be a JSON object")
	}
	if len(bindings) > MaxChannelRoutes {
		return nil, fmt.Errorf("route binding state has more than %d entries", MaxChannelRoutes)
	}
	for rawKey, rawSessionID := range bindings {
		key := SessionRouteKey(rawKey)
		if err := validateRouteBindingKey(key); err != nil {
			return nil, fmt.Errorf("route binding state key %q is invalid: %w", rawKey, err)
		}
		if err := validateRouteBindingSessionID(SessionID(rawSessionID)); err != nil {
			return nil, fmt.Errorf("route binding state session for %q is invalid: %w", rawKey, err)
		}
	}
	return bindings, nil
}

func (store *LocalRouteBindingStore) write(ctx context.Context, bindings map[string]string) error {
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	data, err := json.Marshal(bindings)
	if err != nil {
		return fmt.Errorf("encode route binding state: %w", err)
	}
	dir := filepath.Dir(store.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create route binding state directory: %w", err)
	}
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".route-bindings-*.tmp")
	if err != nil {
		return fmt.Errorf("create route binding temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set route binding temp permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write route binding state: %w", err)
	}
	if err := checkRouteBindingContext(ctx); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync route binding state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close route binding state: %w", err)
	}
	if err := checkRouteBindingContext(ctx); err != nil {
		return err
	}
	if err := os.Rename(tempPath, store.path); err != nil {
		return fmt.Errorf("commit route binding state: %w", err)
	}
	return nil
}
