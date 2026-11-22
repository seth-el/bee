package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const DefaultVoiceOutputMaxBytes int64 = MaxTelegramVoiceFileSize

type VoiceOutputArtifactStoreOptions struct {
	MaxBytes int64
}

// VoiceOutputArtifactStore maps opaque VoiceOutput references to private files.
// The reference contains only a digest and never a local path.
type VoiceOutputArtifactStore struct {
	root     string
	maxBytes int64
}

type VoiceOutputArtifact struct {
	store       *VoiceOutputArtifactStore
	path        string
	output      bee.VoiceOutput
	cleanupOnce sync.Once
	cleanupErr  error
}

func NewVoiceOutputArtifactStore(root string, options ...VoiceOutputArtifactStoreOptions) (*VoiceOutputArtifactStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("voice output artifact root is required")
	}
	abs, err := ensurePrivateArtifactRoot(root, "voice output artifact root")
	if err != nil {
		return nil, err
	}
	maxBytes := DefaultVoiceOutputMaxBytes
	if len(options) > 0 && options[0].MaxBytes > 0 && options[0].MaxBytes <= MaxTelegramVoiceFileSize {
		maxBytes = options[0].MaxBytes
	}
	return &VoiceOutputArtifactStore{root: abs, maxBytes: maxBytes}, nil
}

func (store *VoiceOutputArtifactStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *VoiceOutputArtifactStore) Stage(ctx context.Context, data []byte, contentType string, durationMS int64) (bee.VoiceOutput, error) {
	if store == nil {
		return bee.VoiceOutput{}, errors.New("voice output artifact store is nil")
	}
	if ctx == nil {
		return bee.VoiceOutput{}, errors.New("voice output artifact context is nil")
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	if len(data) == 0 || int64(len(data)) > store.maxBytes {
		return bee.VoiceOutput{}, errors.New("voice output artifact exceeds bounded size")
	}
	if len(contentType) > bee.MaxChannelContentTypeBytes || !utf8.ValidString(contentType) || !strings.HasPrefix(strings.ToLower(contentType), "audio/") {
		return bee.VoiceOutput{}, errors.New("voice output artifact content type is invalid")
	}
	if strings.EqualFold(contentType, "audio/ogg") && (len(data) < 4 || string(data[:4]) != "OggS") {
		return bee.VoiceOutput{}, errors.New("voice output artifact is not OGG")
	}
	output := bee.VoiceOutput{Reference: "tg-voice-output-v1-" + digestBytes(data), ContentType: strings.ToLower(contentType), DurationMS: durationMS}
	if err := output.Validate(); err != nil {
		return bee.VoiceOutput{}, fmt.Errorf("validate voice output artifact: %w", err)
	}
	dir, err := os.MkdirTemp(store.root, ".voice-output-")
	if err != nil {
		return bee.VoiceOutput{}, errors.New("create voice output artifact directory")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, "audio.ogg")
	if !store.contains(path) {
		return bee.VoiceOutput{}, errors.New("voice output artifact path escaped root")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return bee.VoiceOutput{}, errors.New("write voice output artifact")
	}
	if err := ctx.Err(); err != nil {
		return bee.VoiceOutput{}, err
	}
	cleanup = false
	return output, nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (store *VoiceOutputArtifactStore) Resolve(output bee.VoiceOutput) (*VoiceOutputArtifact, error) {
	if store == nil {
		return nil, errors.New("voice output artifact store is nil")
	}
	if err := output.Validate(); err != nil {
		return nil, err
	}
	prefix := "tg-voice-output-v1-"
	if !strings.HasPrefix(output.Reference, prefix) || len(output.Reference) != len(prefix)+64 {
		return nil, errors.New("voice output reference is not owned by Telegram adapter")
	}
	name := output.Reference[len(prefix):]
	for _, char := range name {
		if !((char >= 'a' && char <= 'f') || (char >= '0' && char <= '9')) {
			return nil, errors.New("voice output reference is invalid")
		}
	}
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return nil, errors.New("inspect voice output artifact root")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".voice-output-") {
			continue
		}
		path := filepath.Join(store.root, entry.Name(), "audio.ogg")
		if !store.contains(path) {
			continue
		}
		data, readErr := readFileBounded(path, store.maxBytes)
		if readErr != nil || digestBytes(data) != name {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			continue
		}
		return &VoiceOutputArtifact{store: store, path: path, output: output}, nil
	}
	return nil, errors.New("voice output artifact is unavailable")
}

func (artifact *VoiceOutputArtifact) Output() bee.VoiceOutput {
	if artifact == nil {
		return bee.VoiceOutput{}
	}
	return artifact.output
}

func (artifact *VoiceOutputArtifact) Open() (*os.File, error) {
	if artifact == nil || artifact.store == nil || artifact.path == "" || !artifact.store.contains(artifact.path) {
		return nil, errors.New("voice output artifact is unavailable")
	}
	info, err := os.Lstat(artifact.path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("voice output artifact is not a private regular file")
	}
	return os.Open(artifact.path)
}

func (artifact *VoiceOutputArtifact) Cleanup() error {
	if artifact == nil {
		return nil
	}
	artifact.cleanupOnce.Do(func() {
		if artifact.store == nil || !artifact.store.contains(artifact.path) {
			artifact.cleanupErr = errors.New("voice output artifact path escaped root")
			return
		}
		if err := os.Remove(artifact.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("remove voice output artifact")
			return
		}
		if err := os.Remove(filepath.Dir(artifact.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("remove voice output artifact directory")
		}
	})
	return artifact.cleanupErr
}

func (store *VoiceOutputArtifactStore) Remove(artifact *VoiceOutputArtifact) error {
	if artifact == nil {
		return nil
	}
	if artifact.store != store {
		return errors.New("voice output artifact belongs to another store")
	}
	return artifact.Cleanup()
}

func (store *VoiceOutputArtifactStore) contains(path string) bool {
	if store == nil || store.root == "" {
		return false
	}
	candidate := filepath.Clean(path)
	return candidate != store.root && strings.HasPrefix(candidate, filepath.Clean(store.root)+string(os.PathSeparator))
}
