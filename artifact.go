package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MaxArtifactBytes           int64 = 8 << 20
	MaxFileBackedArtifactBytes int64 = 1 << 30
	MaxArtifactPreviewBytes          = 64 << 10
	MaxArtifactFilenameBytes         = 255
)

var ErrArtifactNotFound = errors.New("artifact not found")

// Artifact is safe metadata only; the storage path is never exposed.
type Artifact struct {
	ID                string    `json:"id"`
	Filename          string    `json:"filename"`
	MediaType         string    `json:"media_type"`
	Kind              string    `json:"kind"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	CreatedAt         time.Time `json:"created_at"`
	SessionID         SessionID `json:"session_id"`
	RunID             string    `json:"run_id,omitempty"`
	Purpose           string    `json:"purpose,omitempty"`
	OriginalSize      int64     `json:"original_size,omitempty"`
	RetainedSize      int64     `json:"retained_size,omitempty"`
	OriginalTruncated bool      `json:"original_truncated,omitempty"`
	RetentionCapped   bool      `json:"retention_capped,omitempty"`
}

type ArtifactInput struct {
	Filename  string
	MediaType string
	Content   []byte
	// MaxBytes optionally overrides the generic artifact cap for a media-specific intake path.
	MaxBytes          int64
	Purpose           string
	OriginalSize      int64
	RetainedSize      int64
	OriginalTruncated bool
	RetentionCapped   bool
}

// ArtifactFileInput describes a private, regular file to ingest without
// materializing the payload in memory. MaxBytes is required so callers must
// explicitly opt into a larger, media-specific bound.
type ArtifactFileInput struct {
	Path      string
	Filename  string
	MediaType string
	MaxBytes  int64
}

// AttachmentArtifactResolver is the narrow provider seam for reading an
// immutable attachment payload. Implementations return bytes through an
// opaque reader; storage paths never cross the provider boundary.
type AttachmentArtifactResolver interface {
	OpenAttachmentPayload(context.Context, AttachmentRef) (Artifact, io.ReadCloser, error)
}

type ArtifactEvent struct {
	Artifact Artifact `json:"artifact"`
}

type artifactRunEmitterKey struct{}
type artifactSessionKey struct{}
type artifactRunKey struct{}

func withArtifactContext(ctx context.Context, sessionID SessionID, runID string) context.Context {
	ctx = context.WithValue(ctx, artifactSessionKey{}, sessionID)
	return context.WithValue(ctx, artifactRunKey{}, runID)
}

func SessionIDFromContext(ctx context.Context) (SessionID, bool) {
	id, ok := ctx.Value(artifactSessionKey{}).(SessionID)
	return id, ok
}

func RunIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(artifactRunKey{}).(string)
	return id, ok
}

func withArtifactRunEmitter(ctx context.Context, emit func(Artifact)) context.Context {
	return context.WithValue(ctx, artifactRunEmitterKey{}, emit)
}

func emitArtifactRunEvent(ctx context.Context, artifact Artifact) {
	if emit, ok := ctx.Value(artifactRunEmitterKey{}).(func(Artifact)); ok && emit != nil {
		emit(artifact)
	}
}

// ArtifactStore owns immutable payloads below a dedicated root.
type ArtifactStore struct {
	root  string
	store *SessionStore
}

func NewArtifactStore(root string, sessions *SessionStore) (*ArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artifact store root is required")
	}
	if sessions == nil {
		return nil, errors.New("artifact session store is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact store root: %w", err)
	}
	if err := ValidateBeePath(abs); err != nil {
		return nil, err
	}
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact store root: %w", err)
	}
	return &ArtifactStore{root: filepath.Clean(abs), store: sessions}, nil
}

func NewDefaultArtifactStore(sessions *SessionStore) (*ArtifactStore, error) {
	root, err := DefaultBeeArtifactRoot()
	if err != nil {
		return nil, err
	}
	return NewArtifactStore(root, sessions)
}

func (store *ArtifactStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func cleanArtifactFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxArtifactFilenameBytes || strings.ContainsRune(name, '\x00') {
		return "", errors.New("artifact filename is empty or too large")
	}
	if filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("artifact filename must be a safe basename")
	}
	return name, nil
}

func artifactMediaType(filename, provided string, content []byte) string {
	if provided = strings.TrimSpace(provided); provided != "" {
		if parsed, _, err := mime.ParseMediaType(provided); err == nil && parsed != "" {
			return parsed
		}
	}
	if inferred := mime.TypeByExtension(filepath.Ext(filename)); inferred != "" {
		return strings.Split(inferred, ";")[0]
	}
	return http.DetectContentType(content)
}

func artifactKind(mediaType string) string {
	if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
		return "text"
	}
	return "binary"
}

func (store *ArtifactStore) artifactDir(id string) string { return filepath.Join(store.root, id) }
func (store *ArtifactStore) metadataPath(id string) string {
	return filepath.Join(store.artifactDir(id), "metadata.json")
}
func (store *ArtifactStore) payloadPath(id string) string {
	return filepath.Join(store.artifactDir(id), "payload")
}

func validateArtifactID(id string) error {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) || strings.ContainsRune(id, '\x00') {
		return errors.New("invalid artifact id")
	}
	return nil
}

func (store *ArtifactStore) Create(ctx context.Context, sessionID SessionID, runID string, input ArtifactInput) (Artifact, error) {
	if store == nil || store.store == nil {
		return Artifact{}, errors.New("artifact store is not configured")
	}
	if err := validateSessionID(sessionID); err != nil {
		return Artifact{}, err
	}
	maxBytes := MaxArtifactBytes
	if input.MaxBytes > 0 {
		maxBytes = input.MaxBytes
	}
	if int64(len(input.Content)) > maxBytes {
		return Artifact{}, fmt.Errorf("artifact exceeds %d byte limit", maxBytes)
	}
	filename, err := cleanArtifactFilename(input.Filename)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := store.store.Load(sessionID); err != nil {
		return Artifact{}, err
	}
	id := newStableID()
	if err := validateArtifactID(id); err != nil {
		return Artifact{}, err
	}
	mediaType := artifactMediaType(filename, input.MediaType, input.Content)
	digest := sha256.Sum256(input.Content)
	artifact := Artifact{ID: id, Filename: filename, MediaType: mediaType, Kind: artifactKind(mediaType), Size: int64(len(input.Content)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: nowUTC(), SessionID: sessionID, RunID: strings.TrimSpace(runID), Purpose: strings.TrimSpace(input.Purpose), OriginalSize: input.OriginalSize, RetainedSize: input.RetainedSize, OriginalTruncated: input.OriginalTruncated, RetentionCapped: input.RetentionCapped}
	if err := validateArtifactRetentionMetadata(artifact); err != nil {
		return Artifact{}, err
	}
	dir := store.artifactDir(id)
	if err := ensurePrivateDir(dir, 0o700); err != nil {
		return Artifact{}, err
	}
	file, err := os.OpenFile(store.payloadPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, err
	}
	if _, err = file.Write(input.Content); err != nil {
		file.Close()
		return Artifact{}, err
	}
	if err = file.Close(); err != nil {
		return Artifact{}, err
	}
	meta, err := json.Marshal(artifact)
	if err != nil {
		return Artifact{}, err
	}
	metaFile, err := os.OpenFile(store.metadataPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := metaFile.Write(meta); err != nil {
		_ = metaFile.Close()
		return Artifact{}, err
	}
	if err := metaFile.Close(); err != nil {
		return Artifact{}, err
	}
	event, err := NewArtifactEvent(sessionID, artifact)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := store.store.Append(sessionID, event); err != nil {
		return Artifact{}, err
	}
	// Metadata and the session event are authoritative; cache failures are
	// isolated so a read-only index cannot reject a persisted artifact.
	_ = appendScopedIndexID(store.root, sessionID, artifact.ID)
	emitArtifactRunEvent(ctx, artifact)
	return artifact, nil
}

// CreateFromFile ingests one private regular file through a bounded streaming
// copy. It intentionally does not change Create's 8 MiB in-memory contract;
// larger media callers must provide an explicit MaxBytes bound.
func (store *ArtifactStore) CreateFromFile(ctx context.Context, sessionID SessionID, runID string, input ArtifactFileInput) (artifact Artifact, err error) {
	if store == nil || store.store == nil {
		return Artifact{}, errors.New("artifact store is not configured")
	}
	if err := validateSessionID(sessionID); err != nil {
		return Artifact{}, err
	}
	if input.MaxBytes <= 0 {
		return Artifact{}, errors.New("file artifact byte limit is required")
	}
	if input.MaxBytes > MaxFileBackedArtifactBytes {
		return Artifact{}, fmt.Errorf("file artifact exceeds %d byte limit", MaxFileBackedArtifactBytes)
	}
	filename, err := cleanArtifactFilename(input.Filename)
	if err != nil {
		return Artifact{}, err
	}
	if err := validatePrivateArtifactSource(input.Path, input.MaxBytes); err != nil {
		return Artifact{}, err
	}
	if _, err := store.store.Load(sessionID); err != nil {
		return Artifact{}, err
	}

	source, err := os.Open(input.Path)
	if err != nil {
		return Artifact{}, err
	}
	defer source.Close()

	id := newStableID()
	if err := validateArtifactID(id); err != nil {
		return Artifact{}, err
	}
	dir := store.artifactDir(id)
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := ensurePrivateDir(dir, 0o700); err != nil {
		return Artifact{}, err
	}
	payload, err := os.OpenFile(store.payloadPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, err
	}
	hash := sha256.New()
	limited := io.LimitReader(source, input.MaxBytes+1)
	count, copyErr := io.CopyBuffer(io.MultiWriter(payload, hash), limited, make([]byte, 32*1024))
	closeErr := payload.Close()
	if copyErr != nil {
		return Artifact{}, copyErr
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	if count > input.MaxBytes {
		return Artifact{}, fmt.Errorf("artifact exceeds %d byte limit", input.MaxBytes)
	}
	if stat, statErr := source.Stat(); statErr != nil {
		return Artifact{}, statErr
	} else if stat.Size() != count {
		return Artifact{}, errors.New("artifact source changed during ingestion")
	}

	mediaType := artifactMediaType(filename, input.MediaType, nil)
	artifact = Artifact{ID: id, Filename: filename, MediaType: mediaType, Kind: artifactKind(mediaType), Size: count, SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: nowUTC(), SessionID: sessionID, RunID: strings.TrimSpace(runID)}
	meta, err := json.Marshal(artifact)
	if err != nil {
		return Artifact{}, err
	}
	metaFile, err := os.OpenFile(store.metadataPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := metaFile.Write(meta); err != nil {
		_ = metaFile.Close()
		return Artifact{}, err
	}
	if err := metaFile.Close(); err != nil {
		return Artifact{}, err
	}
	event, err := NewArtifactEvent(sessionID, artifact)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := store.store.Append(sessionID, event); err != nil {
		return Artifact{}, err
	}
	_ = appendScopedIndexID(store.root, sessionID, artifact.ID)
	cleanup = false
	emitArtifactRunEvent(ctx, artifact)
	return artifact, nil
}

func validatePrivateArtifactSource(path string, maxBytes int64) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("artifact source path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("artifact source path contains a symlink")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact source is not a safe regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("artifact source must have private permissions")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return fmt.Errorf("artifact exceeds %d byte limit", maxBytes)
	}
	return nil
}

func (store *ArtifactStore) loadMetadata(id string) (Artifact, error) {
	if store == nil {
		return Artifact{}, errors.New("artifact store is nil")
	}
	if err := validateArtifactID(id); err != nil {
		return Artifact{}, err
	}
	info, err := os.Lstat(store.artifactDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Artifact{}, errors.New("artifact directory is not a safe directory")
	}
	metaInfo, err := os.Lstat(store.metadataPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, ErrArtifactNotFound
		}
		return Artifact{}, err
	}
	if !metaInfo.Mode().IsRegular() || metaInfo.Mode()&os.ModeSymlink != 0 {
		return Artifact{}, errors.New("artifact metadata is not a safe regular file")
	}
	var artifact Artifact
	data, err := os.ReadFile(store.metadataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode artifact metadata: %w", err)
	}
	if artifact.ID != id || artifact.SessionID == "" {
		return Artifact{}, errors.New("artifact metadata is invalid")
	}
	if err := validateArtifactRetentionMetadata(artifact); err != nil {
		return Artifact{}, errors.New("artifact metadata is invalid")
	}
	return artifact, nil
}

func validateArtifactRetentionMetadata(artifact Artifact) error {
	if artifact.Purpose != "" {
		if err := validateSafeID("artifact purpose", artifact.Purpose, 64); err != nil {
			return errors.New("artifact retention metadata is invalid")
		}
	}
	if artifact.OriginalSize < 0 || artifact.RetainedSize < 0 {
		return errors.New("artifact retention metadata is invalid")
	}
	if artifact.Purpose == commandArtifactPurpose {
		if artifact.RunID == "" || !artifact.OriginalTruncated || artifact.OriginalSize <= 0 || artifact.RetainedSize > artifact.OriginalSize || artifact.RetentionCapped != (artifact.RetainedSize < artifact.OriginalSize) {
			return errors.New("artifact retention metadata is invalid")
		}
		return nil
	}
	if artifact.OriginalSize != 0 || artifact.RetainedSize != 0 || artifact.OriginalTruncated || artifact.RetentionCapped {
		return errors.New("artifact retention metadata is invalid")
	}
	return nil
}

func (store *ArtifactStore) Get(id string) (Artifact, error) { return store.loadMetadata(id) }

func (store *ArtifactStore) scanSessionArtifacts(sessionID SessionID) ([]Artifact, []string, error) {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return nil, nil, err
	}
	result := make([]Artifact, 0)
	ids := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "attachments" {
			continue
		}
		artifact, loadErr := store.loadMetadata(entry.Name())
		if errors.Is(loadErr, ErrArtifactNotFound) {
			continue
		}
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if artifact.SessionID == sessionID {
			ids = append(ids, artifact.ID)
			result = append(result, artifact)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, ids, nil
}

func (store *ArtifactStore) List(sessionID SessionID) ([]Artifact, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if err := ensureScopedIndexDir(store.root); err != nil {
		result, _, scanErr := store.scanSessionArtifacts(sessionID)
		return result, scanErr
	}
	lock, lockErr := acquireSessionFileLock(scopedIndexLockPath(store.root, sessionID))
	if lockErr != nil {
		result, _, scanErr := store.scanSessionArtifacts(sessionID)
		return result, scanErr
	}
	defer lock.Close()
	index, ok, _ := readScopedIndex(store.root, sessionID)
	if ok {
		result := make([]Artifact, 0, len(index.IDs))
		for _, id := range index.IDs {
			artifact, loadErr := store.loadMetadata(id)
			if loadErr != nil || artifact.SessionID != sessionID {
				ok = false
				break
			}
			result = append(result, artifact)
		}
		if ok {
			sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
			return result, nil
		}
	}
	// Rebuild from authoritative metadata only when the bounded index is
	// missing, stale, or corrupt. Tie the sidecar to the timestamp observed by
	// a stable scan; a concurrent source mutation leaves it stale rather than
	// allowing an incomplete ID set to look current.
	var result []Artifact
	var ids []string
	for attempt := 0; attempt < 3; attempt++ {
		before, beforeErr := scopedSourceModTime(store.root)
		if beforeErr != nil {
			return nil, beforeErr
		}
		var scanErr error
		result, ids, scanErr = store.scanSessionArtifacts(sessionID)
		if scanErr != nil {
			return nil, scanErr
		}
		after, afterErr := scopedSourceModTime(store.root)
		if afterErr != nil {
			return nil, afterErr
		}
		if before == after {
			_ = writeScopedIndexAt(store.root, sessionID, ids, after)
			return result, nil
		}
	}
	return result, nil
}

func (store *ArtifactStore) OpenPayload(id string) (Artifact, io.ReadCloser, error) {
	artifact, err := store.loadMetadata(id)
	if err != nil {
		return Artifact{}, nil, err
	}
	info, err := os.Lstat(store.payloadPath(id))
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Artifact{}, nil, errors.New("artifact payload is not a safe regular file")
	}
	file, err := os.Open(store.payloadPath(id))
	if err != nil {
		return Artifact{}, nil, err
	}
	return artifact, file, nil
}

// OpenAttachmentPayload resolves only the artifact referenced by a stored
// attachment. The returned reader is owned by the caller and never exposes
// the artifact store path.
func (store *ArtifactStore) OpenAttachmentPayload(ctx context.Context, attachment AttachmentRef) (Artifact, io.ReadCloser, error) {
	if ctx == nil {
		return Artifact{}, nil, errors.New("attachment resolver context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, nil, err
	}
	if strings.TrimSpace(attachment.ArtifactID) == "" {
		return Artifact{}, nil, errors.New("attachment artifact id is required")
	}
	artifact, reader, err := store.OpenPayload(strings.TrimSpace(attachment.ArtifactID))
	if err != nil {
		return Artifact{}, nil, err
	}
	return artifact, reader, nil
}

func (store *ArtifactStore) Preview(id string) (Artifact, []byte, bool, error) {
	artifact, file, err := store.OpenPayload(id)
	if err != nil {
		return Artifact{}, nil, false, err
	}
	defer file.Close()
	if artifact.Kind != "text" {
		return artifact, nil, false, nil
	}
	limit := int64(MaxArtifactPreviewBytes)
	if artifact.Size < limit {
		limit = artifact.Size
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Artifact{}, nil, false, err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return artifact, data, truncated, nil
}
