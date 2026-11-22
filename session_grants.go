package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxSessionWriteGrants bounds the directories one session may be granted.
const MaxSessionWriteGrants = 32

// SessionWriteGrants records directories the user approved as writable for
// the rest of one session ("approve for this session" on an out-of-bounds
// write). Grants live in memory for the process: a restart, or another
// session, starts without them. They extend only the write roots, for the
// file tools and the exec sandbox alike; host_exec and package activation
// are never session-grantable.
type SessionWriteGrants struct {
	mu     sync.Mutex
	grants map[SessionID][]string
}

func NewSessionWriteGrants() *SessionWriteGrants {
	return &SessionWriteGrants{grants: make(map[SessionID][]string)}
}

// Add grants dir for session id. It reports false when the grant was not
// recorded (bad input or the per-session bound).
func (store *SessionWriteGrants) Add(id SessionID, dir string) bool {
	if store == nil || id == "" || validateSessionID(id) != nil || !filepath.IsAbs(dir) {
		return false
	}
	dir = filepath.Clean(dir)
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.grants[id] {
		if existing == dir || scopeWithin(existing, dir) {
			return true
		}
	}
	if len(store.grants[id]) >= MaxSessionWriteGrants {
		return false
	}
	store.grants[id] = append(store.grants[id], dir)
	return true
}

func (store *SessionWriteGrants) Roots(id SessionID) []string {
	if store == nil || id == "" {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]string(nil), store.grants[id]...)
}

type sessionWriteGrantsKey struct{}

func withSessionWriteGrants(ctx context.Context, store *SessionWriteGrants) context.Context {
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionWriteGrantsKey{}, store)
}

// sessionWriteGrantsFor returns the grant store and session for this call,
// or nil when either is missing (no session, or a caller without grants).
func sessionWriteGrantsFor(ctx context.Context) (*SessionWriteGrants, SessionID) {
	if ctx == nil {
		return nil, ""
	}
	store, _ := ctx.Value(sessionWriteGrantsKey{}).(*SessionWriteGrants)
	id, _ := SessionIDFromContext(ctx)
	if store == nil || id == "" {
		return nil, ""
	}
	return store, id
}

// withSessionGrants adds this session's granted directories to the write
// roots. A grant outside every readable root is ignored rather than widening
// reads.
func (scope ExecutionScope) withSessionGrants(ctx context.Context) ExecutionScope {
	store, id := sessionWriteGrantsFor(ctx)
	if store == nil {
		return scope
	}
	readRoots, err := scope.rootPaths()
	if err != nil {
		return scope
	}
	extra := make([]string, 0)
	for _, dir := range store.Roots(id) {
		if pathWithinAny(readRoots, dir) {
			extra = append(extra, dir)
		}
	}
	if len(extra) == 0 {
		return scope
	}
	if strings.TrimSpace(scope.WriteRoot) == "" {
		scope.WriteRoot = scope.Root
	}
	scope.WriteRoots = append(append([]string(nil), scope.WriteRoots...), extra...)
	return scope
}

// sessionGrantDirectory picks the directory "approve for this session" would
// grant for an approved write starting at start (the target's directory):
// the nearest enclosing git work tree, so a whole project becomes writable
// rather than one folder, else start itself (or its nearest existing
// ancestor). It must lie inside a readable root and pass the same guards as
// the launch directory (never /, home, Bee's home, secrets, or sessions).
func sessionGrantDirectory(scope ExecutionScope, start string) string {
	readRoots, err := scope.rootPaths()
	if err != nil || !filepath.IsAbs(start) {
		return ""
	}
	writeRoots, _ := scope.writeRootPaths()
	dir := filepath.Clean(start)
	for dir != "" && !directoryExists(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	candidate := dir
	for current := dir; pathWithinAny(readRoots, current); {
		if info, statErr := os.Stat(filepath.Join(current, ".git")); statErr == nil && (info.IsDir() || info.Mode().IsRegular()) {
			candidate = current
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if !pathWithinAny(readRoots, candidate) {
		return ""
	}
	granted, reason := EligibleLaunchRoot(candidate, writeRoots)
	if reason != "" {
		return ""
	}
	return granted
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
