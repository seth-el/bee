package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/seth-el/bee"
)

const (
	DefaultVoiceArtifactMaxBytes = MaxTelegramVoiceFileSize
	DefaultVoiceArtifactMaxSecs  = MaxTelegramVoiceDurationSec
)

type VoiceArtifactStoreOptions struct {
	MaxBytes       int64
	MaxDurationSec int64
}

// VoiceArtifactStore owns a caller-selected private root. It never accepts a
// path supplied by Telegram and never writes outside this root.
type VoiceArtifactStore struct {
	root           string
	maxBytes       int64
	maxDurationSec int64
}

// VoiceArtifact is adapter-owned staged media. Its path is intentionally
// available only to an explicitly composed artifact recognizer, not Bee.
type VoiceArtifact struct {
	store       *VoiceArtifactStore
	path        string
	input       bee.VoiceInput
	contentType string
	sizeBytes   int64
	durationMS  int64
	cleanupOnce sync.Once
	cleanupErr  error
}

func NewVoiceArtifactStore(root string, options ...VoiceArtifactStoreOptions) (*VoiceArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("voice artifact root is required")
	}
	abs, err := ensurePrivateArtifactRoot(root, "voice artifact root")
	if err != nil {
		return nil, err
	}
	optionsValue := VoiceArtifactStoreOptions{}
	if len(options) > 0 {
		optionsValue = options[0]
	}
	if optionsValue.MaxBytes <= 0 || optionsValue.MaxBytes > MaxTelegramVoiceFileSize {
		optionsValue.MaxBytes = DefaultVoiceArtifactMaxBytes
	}
	if optionsValue.MaxDurationSec <= 0 || optionsValue.MaxDurationSec > MaxTelegramVoiceDurationSec {
		optionsValue.MaxDurationSec = DefaultVoiceArtifactMaxSecs
	}
	return &VoiceArtifactStore{root: abs, maxBytes: optionsValue.MaxBytes, maxDurationSec: optionsValue.MaxDurationSec}, nil
}

func (store *VoiceArtifactStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *VoiceArtifactStore) maxDurationMS() int64 {
	return store.maxDurationSec * 1000
}

func (store *VoiceArtifactStore) Download(ctx context.Context, client *Client, raw Voice) (*VoiceArtifact, error) {
	if store == nil {
		return nil, errors.New("voice artifact store is nil")
	}
	if ctx == nil {
		return nil, errors.New("voice artifact context is nil")
	}
	if client == nil {
		return nil, errors.New("voice artifact Telegram client is required")
	}
	if err := raw.Validate(); err != nil {
		return nil, err
	}
	if raw.Duration*1000 > store.maxDurationMS() {
		return nil, errors.New("telegram voice duration exceeds artifact limit")
	}
	if raw.FileSize > store.maxBytes {
		return nil, errors.New("telegram voice metadata exceeds artifact limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(store.root, ".voice-")
	if err != nil {
		return nil, errors.New("create voice artifact directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.Remove(dir)
		return nil, errors.New("set voice artifact directory permissions")
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.RemoveAll(dir)
		}
	}()

	temporary, err := os.CreateTemp(dir, ".audio-*")
	if err != nil {
		return nil, errors.New("create voice artifact file")
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, errors.New("set voice artifact file permissions")
	}
	_, written, downloadErr := client.DownloadFileByID(ctx, raw.FileID, temporary, store.maxBytes)
	if closeErr := temporary.Close(); downloadErr == nil && closeErr != nil {
		downloadErr = errors.New("close voice artifact file")
	}
	if downloadErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, downloadErr
	}
	syncFile, err := os.OpenFile(temporaryPath, os.O_WRONLY, 0)
	if err != nil {
		return nil, errors.New("open voice artifact for sync")
	}
	if err := syncFile.Sync(); err != nil {
		_ = syncFile.Close()
		return nil, errors.New("sync voice artifact")
	}
	if err := syncFile.Close(); err != nil {
		return nil, errors.New("close synced voice artifact")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(temporaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("inspect staged voice artifact")
	}
	if written != info.Size() || info.Size() <= 0 || info.Size() > store.maxBytes {
		return nil, errors.New("staged voice artifact size is invalid")
	}
	if raw.FileSize > 0 && raw.FileSize != info.Size() {
		return nil, errors.New("staged voice artifact size does not match metadata")
	}
	contentType, err := detectVoiceContentType(temporaryPath, raw.MimeType)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalPath := filepath.Join(dir, "audio")
	if !store.contains(finalPath) {
		return nil, errors.New("voice artifact path escaped root")
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return nil, errors.New("commit voice artifact")
	}
	if err := os.Chmod(finalPath, 0o600); err != nil {
		return nil, errors.New("set committed voice artifact permissions")
	}
	mappedRaw := raw
	mappedRaw.MimeType = contentType
	input, err := VoiceInputFromRaw(mappedRaw)
	if err != nil {
		return nil, fmt.Errorf("map staged voice artifact: %w", err)
	}
	artifact := &VoiceArtifact{store: store, path: finalPath, input: input, contentType: contentType, sizeBytes: info.Size(), durationMS: raw.Duration * 1000}
	cleanupDir = false
	return artifact, nil
}

// Stage is a descriptive alias for Download at composition sites that call
// the resulting private file an artifact staging operation.
func (store *VoiceArtifactStore) Stage(ctx context.Context, client *Client, raw Voice) (*VoiceArtifact, error) {
	return store.Download(ctx, client, raw)
}

func detectVoiceContentType(path, declared string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open staged voice artifact")
	}
	defer file.Close()
	buffer := make([]byte, 512)
	n, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", errors.New("read staged voice artifact")
	}
	detected := http.DetectContentType(buffer[:n])
	contentType := strings.TrimSpace(strings.ToLower(declared))
	if contentType == "" {
		contentType = detected
	}
	if len(contentType) > bee.MaxChannelContentTypeBytes || !strings.HasPrefix(contentType, "audio/") && contentType != "application/ogg" {
		return "", errors.New("voice artifact content type is not audio")
	}
	knownAudioMagic := len(buffer[:n]) >= 4 && string(buffer[:4]) == "OggS"
	knownMP4Container := strings.HasPrefix(detected, "video/mp4") || detected == "application/mp4"
	declaredAudio := strings.HasPrefix(contentType, "audio/") || contentType == "application/ogg"
	if (strings.HasPrefix(detected, "text/") || detected == "application/json" || detected == "application/xml" || strings.HasPrefix(detected, "image/") || strings.HasPrefix(detected, "video/")) && !(knownAudioMagic || (declaredAudio && knownMP4Container)) {
		return "", fmt.Errorf("voice artifact content does not match audio metadata: declared=%s detected=%s", contentType, detected)
	}
	return contentType, nil
}

func (store *VoiceArtifactStore) contains(path string) bool {
	if store == nil || store.root == "" {
		return false
	}
	root := filepath.Clean(store.root)
	candidate := filepath.Clean(path)
	return candidate != root && strings.HasPrefix(candidate, root+string(os.PathSeparator))
}

func (artifact *VoiceArtifact) Input() bee.VoiceInput {
	if artifact == nil {
		return bee.VoiceInput{}
	}
	return artifact.input
}

func (artifact *VoiceArtifact) Path() string {
	if artifact == nil {
		return ""
	}
	return artifact.path
}

func (artifact *VoiceArtifact) ContentType() string {
	if artifact == nil {
		return ""
	}
	return artifact.contentType
}

func (artifact *VoiceArtifact) SizeBytes() int64 {
	if artifact == nil {
		return 0
	}
	return artifact.sizeBytes
}

func (artifact *VoiceArtifact) DurationMS() int64 {
	if artifact == nil {
		return 0
	}
	return artifact.durationMS
}

func (artifact *VoiceArtifact) Open() (*os.File, error) {
	if artifact == nil || artifact.store == nil || artifact.path == "" {
		return nil, errors.New("voice artifact is not available")
	}
	if !artifact.store.contains(artifact.path) {
		return nil, errors.New("voice artifact path escaped root")
	}
	info, err := os.Lstat(artifact.path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("voice artifact is not a private regular file")
	}
	return os.Open(artifact.path)
}

func (artifact *VoiceArtifact) Cleanup() error {
	if artifact == nil {
		return nil
	}
	artifact.cleanupOnce.Do(func() {
		if artifact.store == nil || !artifact.store.contains(artifact.path) {
			artifact.cleanupErr = errors.New("voice artifact path escaped root")
			return
		}
		if info, err := os.Lstat(artifact.path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				artifact.cleanupErr = errors.New("voice artifact is not a regular file")
				return
			}
			if err := os.Remove(artifact.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				artifact.cleanupErr = errors.New("remove voice artifact file")
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("inspect voice artifact file")
			return
		}
		if err := os.Remove(filepath.Dir(artifact.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("remove voice artifact directory")
		}
	})
	return artifact.cleanupErr
}

// Remove is an explicit store-owned cleanup alias.
func (store *VoiceArtifactStore) Remove(artifact *VoiceArtifact) error {
	if artifact == nil {
		return nil
	}
	if artifact.store != store {
		return errors.New("voice artifact belongs to another store")
	}
	return artifact.Cleanup()
}
