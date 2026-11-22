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
	mutationJournalFile   = "journal.jsonl"
	mutationPreimageDir   = "preimages"
	mutationLockFile      = ".journal.lock"
	mutationMaxEntryBytes = 1 << 20
	mutationMaxFileBytes  = 8 << 20
	mutationMaxTotalBytes = 64 << 20
	mutationMaxEntries    = 10000
)

type MutationFileState struct {
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}
type MutationJournalEntry struct {
	ID       string            `json:"id"`
	At       time.Time         `json:"at"`
	ScopeID  string            `json:"scope_id"`
	Tool     string            `json:"tool"`
	Path     string            `json:"path"`
	Before   MutationFileState `json:"before"`
	After    MutationFileState `json:"after"`
	Preimage string            `json:"preimage,omitempty"`
	Result   string            `json:"result,omitempty"`
	UndoOf   string            `json:"undo_of,omitempty"`
}
type MutationJournal struct {
	root             string
	scopeRoot        string
	scopeID          string
	mu               *sync.Mutex
	additionalScopes []*MutationJournal
}

var mutationJournalLocks sync.Map

type mutationJournalLock struct {
	path, token string
	once        sync.Once
	err         error
}

func (lock *mutationJournalLock) Release() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		data, err := os.ReadFile(lock.path)
		if err != nil {
			lock.err = err
			return
		}
		var m memoryLockMetadata
		if json.Unmarshal(data, &m) != nil || m.Token != lock.token {
			lock.err = errors.New("journal lock ownership changed")
			return
		}
		lock.err = os.Remove(lock.path)
	})
	return lock.err
}

func NewMutationJournal(root, scopeRoot string) (*MutationJournal, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(scopeRoot) {
		return nil, errors.New("mutation journal paths must be absolute")
	}
	root, _ = filepath.Abs(root)
	scopeRoot, _ = filepath.Abs(scopeRoot)
	root = filepath.Clean(root)
	scopeRoot = filepath.Clean(scopeRoot)
	if err := rejectBeeSymlinkAncestors(root); err != nil {
		return nil, fmt.Errorf("journal root ancestors: %w", err)
	}
	sum := sha256.Sum256([]byte(scopeRoot))
	scopeID := hex.EncodeToString(sum[:])[:16]
	scopedRoot := filepath.Join(root, scopeID)
	if err := rejectBeeSymlinkAncestors(scopedRoot); err != nil {
		return nil, fmt.Errorf("journal scoped root ancestors: %w", err)
	}
	if err := ensurePrivateJournalRoot(scopedRoot); err != nil {
		return nil, err
	}
	lock := &sync.Mutex{}
	actual, _ := mutationJournalLocks.LoadOrStore(scopedRoot, lock)
	return &MutationJournal{root: scopedRoot, scopeRoot: scopeRoot, scopeID: scopeID, mu: actual.(*sync.Mutex)}, nil
}

// NewMutationJournalForRoots opens one backward-compatible journal per
// readable execution root. The first root remains the primary journal returned
// by Root, while Record, Entries, Show, and Undo operate across the complete
// set. Keeping the existing per-root scope IDs and on-disk layout means adding
// a secondary root does not orphan mutations previously recorded for the
// primary root.
func NewMutationJournalForRoots(root string, scopeRoots []string) (*MutationJournal, error) {
	if len(scopeRoots) == 0 {
		return nil, errors.New("mutation journal requires at least one scope root")
	}
	var primary *MutationJournal
	seen := make(map[string]struct{}, len(scopeRoots))
	for _, scopeRoot := range scopeRoots {
		if !filepath.IsAbs(scopeRoot) {
			return nil, errors.New("mutation journal paths must be absolute")
		}
		resolved, err := filepath.Abs(scopeRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve mutation journal scope root: %w", err)
		}
		resolved = filepath.Clean(resolved)
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		journal, err := NewMutationJournal(root, resolved)
		if err != nil {
			return nil, err
		}
		if primary == nil {
			primary = journal
			continue
		}
		primary.additionalScopes = append(primary.additionalScopes, journal)
	}
	if primary == nil {
		return nil, errors.New("mutation journal requires at least one scope root")
	}
	return primary, nil
}

func (j *MutationJournal) journals() []*MutationJournal {
	if j == nil {
		return nil
	}
	result := make([]*MutationJournal, 0, 1+len(j.additionalScopes))
	result = append(result, j)
	result = append(result, j.additionalScopes...)
	return result
}
func ensurePrivateJournalRoot(root string) error {
	if err := os.MkdirAll(filepath.Join(root, mutationPreimageDir), 0700); err != nil {
		return err
	}
	for _, p := range []string{root, filepath.Join(root, mutationPreimageDir)} {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return errors.New("journal root/components must be private real directories")
		}
		if err := os.Chmod(p, 0700); err != nil {
			return err
		}
	}
	return nil
}
func (j *MutationJournal) Root() string {
	if j == nil {
		return ""
	}
	return j.root
}
func (j *MutationJournal) relative(path string) (string, error) {
	cleaned := filepath.Clean(path)
	// Record runs after the target has been written, so resolving the complete
	// path normally succeeds and assigns symlinked targets to their physical
	// scope root. Direct Record callers may describe a missing evidence path;
	// retain the historical lexical behavior for that case.
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	rel, err := filepath.Rel(j.scopeRoot, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("mutation path escapes execution scope")
	}
	return filepath.ToSlash(rel), nil
}

func (j *MutationJournal) journalForPath(path string) (*MutationJournal, string, error) {
	for _, journal := range j.journals() {
		rel, err := journal.relative(path)
		if err == nil {
			return journal, rel, nil
		}
	}
	return nil, "", errors.New("mutation path escapes all execution scope roots")
}

func fileState(path string) (MutationFileState, []byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return MutationFileState{}, nil, nil
	}
	if err != nil {
		return MutationFileState{}, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return MutationFileState{}, nil, errors.New("mutation target must be a regular non-symlink file")
	}
	if info.Size() > mutationMaxFileBytes {
		return MutationFileState{}, nil, fmt.Errorf("mutation file exceeds %d bytes", mutationMaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return MutationFileState{}, nil, err
	}
	sum := sha256.Sum256(data)
	return MutationFileState{Exists: true, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Mode: uint32(info.Mode().Perm())}, data, nil
}
func (j *MutationJournal) acquire(ctx context.Context) (*mutationJournalLock, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	path := filepath.Join(j.root, mutationLockFile)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("journal lock is unsafe")
		}
		return nil, errors.New("journal lock is held")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	token := newStableID()
	data, _ := json.Marshal(memoryLockMetadata{PID: os.Getpid(), Token: token, CreatedAt: time.Now().UTC()})
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &mutationJournalLock{path: path, token: token}, nil
}

func (j *MutationJournal) readEntriesLocked() ([]MutationJournalEntry, []byte, error) {
	path := filepath.Join(j.root, mutationJournalFile)
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, nil, errors.New("journal file is unsafe")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, statErr
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(data) > mutationMaxTotalBytes {
		return nil, nil, errors.New("mutation journal total quota exceeded")
	}
	var out []MutationJournalEntry
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 4096), mutationMaxEntryBytes)
	for sc.Scan() {
		var e MutationJournalEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, nil, err
		}
		if e.ScopeID != j.scopeID {
			return nil, nil, errors.New("mutation journal scope mismatch")
		}
		if e.Preimage != "" {
			p := filepath.Join(j.root, filepath.FromSlash(e.Preimage))
			info, err := os.Lstat(p)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return nil, nil, errors.New("mutation journal preimage is missing")
			}
		}
		out = append(out, e)
		if len(out) > mutationMaxEntries {
			return nil, nil, errors.New("mutation journal entry limit exceeded")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	used := make(map[string]struct{}, len(out))
	for _, entry := range out {
		if entry.Preimage != "" {
			used[filepath.Clean(filepath.FromSlash(entry.Preimage))] = struct{}{}
		}
	}
	if files, err := os.ReadDir(filepath.Join(j.root, mutationPreimageDir)); err == nil {
		for _, file := range files {
			if strings.HasPrefix(file.Name(), ".preimage-") {
				_ = os.Remove(filepath.Join(j.root, mutationPreimageDir, file.Name()))
				continue
			}
			rel := filepath.Join(mutationPreimageDir, file.Name())
			if _, ok := used[filepath.Clean(rel)]; !ok {
				_ = os.Remove(filepath.Join(j.root, rel))
			}
		}
	}
	return out, data, nil
}
func atomicPrivateReplace(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bee-journal-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	n, err := tmp.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	_ = dir.Sync() // best-effort after rename; rename is the logical commit point
	_ = dir.Close()
	return nil
}
func (j *MutationJournal) appendLocked(ctx context.Context, entry MutationJournalEntry, preimage []byte) error {
	lock, err := j.acquire(ctx)
	if err != nil {
		return err
	}
	defer lock.Release()
	return j.appendTransactionLocked(entry, preimage)
}

func (j *MutationJournal) appendTransactionLocked(entry MutationJournalEntry, preimage []byte) error {
	entries, old, err := j.readEntriesLocked()
	if err != nil {
		return err
	}
	if len(entries) >= mutationMaxEntries {
		return errors.New("mutation journal entry limit exceeded")
	}
	for _, existing := range entries {
		if existing.ID == entry.ID {
			return errors.New("mutation journal entry id already exists")
		}
	}
	line, _ := json.Marshal(entry)
	line = append(line, '\n')
	if len(line) > mutationMaxEntryBytes {
		return errors.New("mutation journal entry exceeds limit")
	}
	var prePath string
	if entry.Before.Exists {
		if len(preimage) > mutationMaxFileBytes {
			return errors.New("mutation preimage exceeds limit")
		}
		prePath = filepath.ToSlash(filepath.Join(mutationPreimageDir, entry.ID))
		if _, err := os.Lstat(filepath.Join(j.root, filepath.FromSlash(prePath))); err == nil {
			return errors.New("mutation preimage already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		entry.Preimage = prePath
		line, _ = json.Marshal(entry)
		line = append(line, '\n')
		if len(line) > mutationMaxEntryBytes {
			return errors.New("mutation journal entry exceeds limit")
		}
	}
	var preTemp string
	if entry.Before.Exists {
		f, err := os.CreateTemp(filepath.Join(j.root, mutationPreimageDir), ".preimage-")
		if err != nil {
			return err
		}
		preTemp = f.Name()
		n, werr := f.Write(preimage)
		if werr == nil && n != len(preimage) {
			werr = io.ErrShortWrite
		}
		if werr == nil {
			werr = f.Sync()
		}
		if closeErr := f.Close(); werr == nil {
			werr = closeErr
		}
		if werr != nil {
			_ = os.Remove(preTemp)
			return werr
		}
		if err := os.Rename(preTemp, filepath.Join(j.root, filepath.FromSlash(prePath))); err != nil {
			return err
		}
	}
	newData := append(append([]byte(nil), old...), line...)
	if int64(len(newData)) > mutationMaxTotalBytes {
		if prePath != "" {
			_ = os.Remove(filepath.Join(j.root, filepath.FromSlash(prePath)))
		}
		return errors.New("mutation journal total quota exceeded")
	}
	var preBytes int64
	ents := append(append([]MutationJournalEntry(nil), entries...), entry)
	for _, e := range ents {
		if e.Preimage != "" {
			if st, err := os.Stat(filepath.Join(j.root, filepath.FromSlash(e.Preimage))); err == nil {
				preBytes += st.Size()
			} else {
				if prePath != "" {
					_ = os.Remove(filepath.Join(j.root, filepath.FromSlash(prePath)))
				}
				return err
			}
		}
	}
	if int64(len(newData))+preBytes > mutationMaxTotalBytes {
		if prePath != "" {
			_ = os.Remove(filepath.Join(j.root, filepath.FromSlash(prePath)))
		}
		return errors.New("mutation journal total quota exceeded")
	}
	if err := atomicPrivateReplace(filepath.Join(j.root, mutationJournalFile), newData); err != nil {
		if prePath != "" {
			_ = os.Remove(filepath.Join(j.root, filepath.FromSlash(prePath)))
		}
		return err
	}
	return nil
}
func (j *MutationJournal) Record(ctx context.Context, tool, path string, before MutationFileState, beforeData []byte, after MutationFileState, result string) (MutationJournalEntry, error) {
	if j == nil {
		return MutationJournalEntry{}, nil
	}
	journal, rel, err := j.journalForPath(path)
	if err != nil {
		return MutationJournalEntry{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entry := MutationJournalEntry{ID: "mutation-" + newStableID(), At: time.Now().UTC(), ScopeID: journal.scopeID, Tool: tool, Path: rel, Before: before, After: after, Result: truncateText(result, 1024)}
	if err := journal.appendLocked(ctx, entry, beforeData); err != nil {
		return MutationJournalEntry{}, err
	}
	entry.Preimage = ""
	if before.Exists {
		entry.Preimage = filepath.ToSlash(filepath.Join(mutationPreimageDir, entry.ID))
	}
	return entry, nil
}
func (j *MutationJournal) entriesForScope() ([]MutationJournalEntry, error) {
	if j == nil {
		return nil, errors.New("mutation journal is nil")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := j.acquire(context.Background())
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	entries, _, err := j.readEntriesLocked()
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (j *MutationJournal) Entries() ([]MutationJournalEntry, error) {
	if j == nil {
		return nil, errors.New("mutation journal is nil")
	}
	var entries []MutationJournalEntry
	for _, journal := range j.journals() {
		scopeEntries, err := journal.entriesForScope()
		if err != nil {
			return nil, err
		}
		entries = append(entries, scopeEntries...)
	}
	sort.Slice(entries, func(a, b int) bool {
		if entries[a].At.Equal(entries[b].At) {
			return entries[a].ID < entries[b].ID
		}
		return entries[a].At.Before(entries[b].At)
	})
	return entries, nil
}
func (j *MutationJournal) Show(id string) (MutationJournalEntry, error) {
	entries, err := j.Entries()
	if err != nil {
		return MutationJournalEntry{}, err
	}
	for _, e := range entries {
		if e.ID == id {
			return e, nil
		}
	}
	return MutationJournalEntry{}, errors.New("mutation journal entry not found")
}
func (j *MutationJournal) Undo(ctx context.Context, id string) (MutationJournalEntry, error) {
	if j == nil {
		return MutationJournalEntry{}, errors.New("mutation journal is nil")
	}
	var targetJournal *MutationJournal
	for _, journal := range j.journals() {
		entries, err := journal.entriesForScope()
		if err != nil {
			return MutationJournalEntry{}, err
		}
		for _, entry := range entries {
			if entry.ID != id {
				continue
			}
			if targetJournal != nil {
				return MutationJournalEntry{}, errors.New("mutation journal entry id is ambiguous across scope roots")
			}
			targetJournal = journal
		}
	}
	if targetJournal == nil {
		return MutationJournalEntry{}, errors.New("mutation journal entry not found")
	}
	return targetJournal.undoForScope(ctx, id)
}

func (j *MutationJournal) undoForScope(ctx context.Context, id string) (MutationJournalEntry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	lock, err := j.acquire(ctx)
	if err != nil {
		return MutationJournalEntry{}, err
	}
	defer lock.Release()
	entries, _, err := j.readEntriesLocked()
	if err != nil {
		return MutationJournalEntry{}, err
	}
	var target *MutationJournalEntry
	for i := range entries {
		if entries[i].ID == id {
			target = &entries[i]
		}
	}
	if target == nil {
		return MutationJournalEntry{}, errors.New("mutation journal entry not found")
	}
	path := filepath.Join(j.scopeRoot, filepath.FromSlash(target.Path))
	current, data, err := fileState(path)
	if err != nil {
		return MutationJournalEntry{}, err
	}
	if current != target.After {
		return MutationJournalEntry{}, errors.New("mutation undo refused: current file does not match recorded postimage")
	}
	if target.Before.Exists {
		pre, err := os.ReadFile(filepath.Join(j.root, filepath.FromSlash(target.Preimage)))
		if err != nil {
			return MutationJournalEntry{}, err
		}
		if err := atomicWriteFile(path, pre, os.FileMode(target.Before.Mode)); err != nil {
			return MutationJournalEntry{}, err
		}
	} else if err := os.Remove(path); err != nil {
		return MutationJournalEntry{}, err
	}
	after, _, err := fileState(path)
	if err != nil {
		return MutationJournalEntry{}, err
	}
	entry := MutationJournalEntry{ID: "mutation-" + newStableID(), At: time.Now().UTC(), ScopeID: j.scopeID, Tool: "mutation_undo", Path: target.Path, Before: current, After: after, UndoOf: target.ID, Result: "undid mutation"}
	if err := j.appendTransactionLocked(entry, data); err != nil {
		if current.Exists {
			_ = atomicWriteFile(path, data, os.FileMode(current.Mode))
		} else {
			_ = os.Remove(path)
		}
		return MutationJournalEntry{}, err
	}
	return entry, nil
}
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	p, err := prepareAtomicFile(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(p)
	return os.Rename(p, path)
}

// atomicCreateFile publishes a fully written file without replacing an entry
// that appeared after the caller's existence check. Bee supports Linux, where
// linking a same-directory temporary file provides the required no-clobber
// commit boundary.
func atomicCreateFile(path string, data []byte, mode os.FileMode) error {
	p, err := prepareAtomicFile(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(p)
	return os.Link(p, path)
}

func prepareAtomicFile(path string, data []byte, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bee-mutation-")
	if err != nil {
		return "", err
	}
	p := tmp.Name()
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		os.Remove(p)
		return "", err
	}
	n, err := tmp.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(p)
		return "", err
	}
	return p, nil
}

type MutationListTool struct{ Journal *MutationJournal }

func (t MutationListTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "mutation_list", Description: "List bounded durable evidence for built-in write_file mutations. Arbitrary commands and external processes are not journaled or reversible.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (t MutationListTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	if t.Journal == nil {
		return "", errors.New("mutation journal is not configured")
	}
	if err := parseArgs(args, &struct{}{}); err != nil {
		return "", err
	}
	e, err := t.Journal.Entries()
	if err != nil {
		return "", err
	}
	truncated := false
	if len(e) > 50 {
		e = e[len(e)-50:]
		truncated = true
	}
	out := struct {
		Entries   []MutationJournalEntry `json:"entries"`
		Truncated bool                   `json:"truncated,omitempty"`
	}{e, truncated}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if len(data) > maxGitReviewBytes {
		return "", errors.New("mutation list output exceeds bound")
	}
	return string(data), nil
}

type mutationIDArgs struct {
	ID string `json:"id"`
}
type MutationShowTool struct{ Journal *MutationJournal }

func (t MutationShowTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "mutation_show", Description: "Show one bounded durable mutation journal entry.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)}
}
func (t MutationShowTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	if t.Journal == nil {
		return "", errors.New("mutation journal is not configured")
	}
	var in mutationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if err := validateSafeID("mutation id", in.ID, MaxMemoryIDLength); err != nil {
		return "", err
	}
	e, err := t.Journal.Show(in.ID)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if len(data) > maxGitReviewBytes {
		return "", errors.New("mutation show output exceeds bound")
	}
	return string(data), nil
}

type MutationUndoTool struct{ Journal *MutationJournal }

func (t MutationUndoTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "mutation_undo", Description: "Safely undo one built-in file mutation only when the current file still matches its recorded postimage.", Risk: RiskHigh, Mutability: ToolMutabilityMutating, Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)}
}
func (t MutationUndoTool) RequiresExplicitApproval() bool { return true }
func (t MutationUndoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Journal == nil {
		return "", errors.New("mutation journal is not configured")
	}
	var in mutationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if err := validateSafeID("mutation id", in.ID, MaxMemoryIDLength); err != nil {
		return "", err
	}
	e, err := t.Journal.Undo(ctx, in.ID)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Undone bool                 `json:"undone"`
		Entry  MutationJournalEntry `json:"entry"`
	}{true, e})
	if err != nil {
		return "", err
	}
	if len(data) > maxGitReviewBytes {
		return "", errors.New("mutation undo output exceeds bound")
	}
	return string(data), nil
}
