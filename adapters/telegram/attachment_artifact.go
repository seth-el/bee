package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seth-el/bee"
)

const DefaultAttachmentArtifactMaxBytes int64 = MaxTelegramVoiceFileSize

type AttachmentKind string

const (
	AttachmentKindDocument AttachmentKind = "document"
	AttachmentKindPhoto    AttachmentKind = "photo"
	AttachmentKindAudio    AttachmentKind = "audio"
	AttachmentKindVoice    AttachmentKind = "voice"
)

// TelegramAttachment is bounded provider metadata. FileID remains adapter-owned
// and is never copied into a Bee envelope.
type TelegramAttachment struct {
	Kind         AttachmentKind
	FileID       string
	FileUniqueID string
	Filename     string
	ContentType  string
	SizeBytes    int64
	Width        int
	Height       int
	DurationSec  int64
	Performer    string
	Title        string
}

func (attachment TelegramAttachment) Validate() error {
	switch attachment.Kind {
	case AttachmentKindDocument, AttachmentKindPhoto, AttachmentKindAudio, AttachmentKindVoice:
	default:
		return errors.New("telegram attachment kind is unsupported")
	}
	if err := validateTelegramFileID(attachment.FileID); err != nil {
		return err
	}
	if attachment.FileUniqueID != "" {
		if err := validateTelegramFileID(attachment.FileUniqueID); err != nil {
			return errors.New("telegram attachment file_unique_id is invalid")
		}
	}
	if attachment.Filename != "" {
		if len(attachment.Filename) > bee.MaxChannelFilenameBytes || !utf8.ValidString(attachment.Filename) || telegramContainsControl(attachment.Filename) || strings.ContainsAny(attachment.Filename, `/\\`) || attachment.Filename == "." || attachment.Filename == ".." || strings.Contains(attachment.Filename, "..") {
			return errors.New("telegram attachment filename must be a safe basename")
		}
	}
	if attachment.ContentType != "" {
		candidate := bee.AttachmentRef{Reference: "telegram-attachment-validation", ContentType: attachment.ContentType}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("telegram attachment content type is invalid: %w", err)
		}
	}
	if attachment.SizeBytes < 0 || attachment.SizeBytes > MaxTelegramVoiceFileSize {
		return fmt.Errorf("telegram attachment size must be between 0 and %d bytes", MaxTelegramVoiceFileSize)
	}
	if attachment.Width < 0 || attachment.Width > 100000 || attachment.Height < 0 || attachment.Height > 100000 || attachment.DurationSec < 0 || attachment.DurationSec > MaxTelegramVoiceDurationSec {
		return errors.New("telegram attachment dimensions or duration are invalid")
	}
	for name, value := range map[string]string{"performer": attachment.Performer, "title": attachment.Title} {
		if len(value) > bee.MaxChannelMetadataValueBytes || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("telegram attachment %s is invalid or too large", name)
		}
	}
	return nil
}

type AttachmentArtifactStoreOptions struct {
	MaxBytes int64
	Index    *ArtifactIndex
}

type AttachmentArtifactStore struct {
	root     string
	maxBytes int64
	index    *ArtifactIndex
}

type AttachmentArtifact struct {
	store       *AttachmentArtifactStore
	path        string
	attachment  bee.AttachmentRef
	cleanupOnce sync.Once
	cleanupErr  error
}

func NewAttachmentArtifactStore(root string, options ...AttachmentArtifactStoreOptions) (*AttachmentArtifactStore, error) {
	abs, err := ensurePrivateArtifactRoot(root, "attachment artifact root")
	if err != nil {
		return nil, err
	}
	maxBytes := DefaultAttachmentArtifactMaxBytes
	if len(options) > 0 && options[0].MaxBytes > 0 && options[0].MaxBytes <= MaxTelegramVoiceFileSize {
		maxBytes = options[0].MaxBytes
	}
	var index *ArtifactIndex
	if len(options) > 0 {
		index = options[0].Index
	}
	return &AttachmentArtifactStore{root: abs, maxBytes: maxBytes, index: index}, nil
}

func (store *AttachmentArtifactStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *AttachmentArtifactStore) Stage(ctx context.Context, client *Client, raw TelegramAttachment) (*AttachmentArtifact, error) {
	if store == nil {
		return nil, errors.New("attachment artifact store is nil")
	}
	if ctx == nil {
		return nil, errors.New("attachment artifact context is nil")
	}
	if client == nil {
		return nil, errors.New("attachment artifact Telegram client is required")
	}
	if err := raw.Validate(); err != nil {
		return nil, err
	}
	if raw.SizeBytes > store.maxBytes {
		return nil, errors.New("telegram attachment metadata exceeds artifact limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref := attachmentReference(raw)
	finalDir := filepath.Join(store.root, ".attachment-"+ref[len("tg-attachment-v1-"):])
	finalPath := filepath.Join(finalDir, "payload")
	if store.contains(finalPath) {
		if artifact, err := store.inspectExisting(finalPath, ref, raw); err == nil {
			return artifact, nil
		}
	}
	dir, err := os.MkdirTemp(store.root, ".attachment-stage-")
	if err != nil {
		return nil, errors.New("create attachment staging directory")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, errors.New("set attachment staging directory permissions")
	}
	tmp, err := os.CreateTemp(dir, ".payload-")
	if err != nil {
		return nil, errors.New("create attachment staging file")
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, errors.New("set attachment staging file permissions")
	}
	_, _, downloadErr := client.DownloadFileByID(ctx, raw.FileID, tmp, store.maxBytes)
	closeErr := tmp.Close()
	if downloadErr == nil && closeErr != nil {
		downloadErr = errors.New("close attachment staging file")
	}
	if downloadErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, downloadErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(tmpPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > store.maxBytes {
		return nil, errors.New("staged attachment size or permissions are invalid")
	}
	if raw.SizeBytes > 0 && raw.SizeBytes != info.Size() {
		return nil, errors.New("staged attachment size does not match metadata")
	}
	contentType, err := inspectAttachmentContent(tmpPath, raw.Kind, raw.ContentType)
	if err != nil {
		return nil, err
	}
	checksum, err := fileSHA256(tmpPath, store.maxBytes)
	if err != nil {
		return nil, errors.New("checksum staged attachment")
	}
	attachment := attachmentRef(raw, ref, contentType, info.Size(), checksum)
	if err := attachment.Validate(); err != nil {
		return nil, fmt.Errorf("validate staged attachment: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, "payload")); err != nil {
		return nil, errors.New("commit staged attachment file")
	}
	if err := os.Chmod(filepath.Join(dir, "payload"), 0o600); err != nil {
		return nil, errors.New("set committed attachment permissions")
	}
	if err := os.Rename(dir, finalDir); err != nil {
		if existing, inspectErr := store.inspectExisting(finalPath, ref, raw); inspectErr == nil {
			cleanup = true
			return existing, nil
		}
		return nil, errors.New("commit attachment artifact")
	}
	if store.index != nil {
		now := time.Now().UTC()
		if err := store.index.Upsert(ArtifactRecord{Reference: attachment.Reference, Kind: string(raw.Kind), Filename: attachment.Filename, ContentType: attachment.ContentType, SizeBytes: attachment.SizeBytes, SHA256: attachment.SHA256, CreatedAt: now, State: ArtifactStaged}); err != nil {
			_ = os.Remove(finalPath)
			_ = os.Remove(finalDir)
			return nil, fmt.Errorf("record attachment artifact: %w", err)
		}
	}
	cleanup = false
	return &AttachmentArtifact{store: store, path: finalPath, attachment: attachment}, nil
}

func (store *AttachmentArtifactStore) inspectExisting(path, ref string, raw TelegramAttachment) (*AttachmentArtifact, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > store.maxBytes {
		return nil, errors.New("attachment artifact is unavailable")
	}
	contentType, err := inspectAttachmentContent(path, raw.Kind, raw.ContentType)
	if err != nil {
		return nil, err
	}
	checksum, err := fileSHA256(path, store.maxBytes)
	if err != nil {
		return nil, err
	}
	attachment := attachmentRef(raw, ref, contentType, info.Size(), checksum)
	if err := attachment.Validate(); err != nil {
		return nil, err
	}
	return &AttachmentArtifact{store: store, path: path, attachment: attachment}, nil
}

func (store *AttachmentArtifactStore) Resolve(ref bee.AttachmentRef) (*AttachmentArtifact, error) {
	if store == nil {
		return nil, errors.New("attachment artifact store is nil")
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	const prefix = "tg-attachment-v1-"
	if !strings.HasPrefix(ref.Reference, prefix) || len(ref.Reference) != len(prefix)+64 || !isLowerHexDigest(ref.Reference[len(prefix):]) {
		return nil, errors.New("attachment reference is not owned by Telegram adapter")
	}
	path := filepath.Join(store.root, ".attachment-"+ref.Reference[len(prefix):], "payload")
	if !store.contains(path) {
		return nil, errors.New("attachment reference escaped root")
	}
	artifact, err := store.inspectExisting(path, ref.Reference, TelegramAttachment{Kind: AttachmentKindDocument, Filename: ref.Filename, ContentType: ref.ContentType, SizeBytes: ref.SizeBytes})
	if err != nil {
		// Resolve cannot infer the original media kind from a provider-neutral
		// ref, so validate the file and metadata without a kind-specific check.
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > store.maxBytes {
			return nil, errors.New("attachment artifact is unavailable")
		}
		checksum, hashErr := fileSHA256(path, store.maxBytes)
		if hashErr != nil || (ref.SHA256 != "" && checksum != ref.SHA256) || (ref.SizeBytes > 0 && info.Size() != ref.SizeBytes) {
			return nil, errors.New("attachment artifact checksum or size mismatch")
		}
		artifact = &AttachmentArtifact{store: store, path: path, attachment: ref}
	}
	return artifact, nil
}

func (artifact *AttachmentArtifact) Reference() bee.AttachmentRef {
	if artifact == nil {
		return bee.AttachmentRef{}
	}
	return artifact.attachment
}
func (artifact *AttachmentArtifact) Path() string {
	if artifact == nil {
		return ""
	}
	return artifact.path
}
func (artifact *AttachmentArtifact) Open() (*os.File, error) {
	if artifact == nil || artifact.store == nil || !artifact.store.contains(artifact.path) {
		return nil, errors.New("attachment artifact is unavailable")
	}
	info, err := os.Lstat(artifact.path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("attachment artifact is not a private regular file")
	}
	return os.Open(artifact.path)
}
func (artifact *AttachmentArtifact) Cleanup() error {
	if artifact == nil {
		return nil
	}
	artifact.cleanupOnce.Do(func() {
		if artifact.store == nil || !artifact.store.contains(artifact.path) {
			artifact.cleanupErr = errors.New("attachment artifact path escaped root")
			return
		}
		if info, err := os.Lstat(artifact.path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				artifact.cleanupErr = errors.New("attachment artifact is not a regular file")
				return
			}
			if err := os.Remove(artifact.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				artifact.cleanupErr = errors.New("remove attachment artifact")
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("inspect attachment artifact")
			return
		}
		if err := os.Remove(filepath.Dir(artifact.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			artifact.cleanupErr = errors.New("remove attachment artifact directory")
		}
	})
	return artifact.cleanupErr
}
func (store *AttachmentArtifactStore) Remove(artifact *AttachmentArtifact) error {
	if artifact == nil {
		return nil
	}
	if artifact.store != store {
		return errors.New("attachment artifact belongs to another store")
	}
	return artifact.Cleanup()
}
func (store *AttachmentArtifactStore) contains(path string) bool {
	return store != nil && store.root != "" && artifactPathWithin(store.root, path)
}

func attachmentReference(raw TelegramAttachment) string {
	canonical := strings.Join([]string{"telegram-attachment-v1", string(raw.Kind), raw.FileID, raw.FileUniqueID, raw.Filename, strings.ToLower(raw.ContentType), strconv.FormatInt(raw.SizeBytes, 10), strconv.Itoa(raw.Width), strconv.Itoa(raw.Height), strconv.FormatInt(raw.DurationSec, 10)}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return "tg-attachment-v1-" + hex.EncodeToString(digest[:])
}
func attachmentRef(raw TelegramAttachment, ref, contentType string, size int64, checksum string) bee.AttachmentRef {
	filename := raw.Filename
	if filename == "" {
		switch raw.Kind {
		case AttachmentKindPhoto:
			filename = "photo"
		case AttachmentKindAudio:
			filename = "audio"
		case AttachmentKindVoice:
			filename = "voice"
		default:
			filename = "attachment"
		}
	}
	metadata := bee.ChannelMetadata{"telegram.kind": string(raw.Kind)}
	if raw.FileUniqueID != "" {
		metadata["telegram.file_unique_id"] = raw.FileUniqueID
	}
	if raw.Width > 0 {
		metadata["telegram.width"] = strconv.Itoa(raw.Width)
	}
	if raw.Height > 0 {
		metadata["telegram.height"] = strconv.Itoa(raw.Height)
	}
	if raw.DurationSec > 0 {
		metadata["telegram.duration_sec"] = strconv.FormatInt(raw.DurationSec, 10)
	}
	if raw.Performer != "" {
		metadata["telegram.performer"] = raw.Performer
	}
	if raw.Title != "" {
		metadata["telegram.title"] = raw.Title
	}
	return bee.AttachmentRef{Reference: ref, Filename: filename, ContentType: contentType, SizeBytes: size, SHA256: checksum, Metadata: metadata}
}

func inspectAttachmentContent(path string, kind AttachmentKind, declared string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open staged attachment")
	}
	defer file.Close()
	buffer := make([]byte, 512)
	n, readErr := io.ReadFull(file, buffer)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return "", errors.New("read staged attachment")
	}
	detected := strings.ToLower(http.DetectContentType(buffer[:n]))
	contentType := strings.ToLower(strings.TrimSpace(declared))
	if contentType == "" {
		contentType = detected
	}
	if len(contentType) > bee.MaxChannelContentTypeBytes || !utf8.ValidString(contentType) {
		return "", errors.New("attachment content type is invalid")
	}
	known := attachmentMagicKind(buffer[:n])
	switch kind {
	case AttachmentKindPhoto:
		if !strings.HasPrefix(contentType, "image/") || known != "image" {
			return "", errors.New("attachment content does not match photo metadata")
		}
	case AttachmentKindAudio, AttachmentKindVoice:
		if !(strings.HasPrefix(contentType, "audio/") || contentType == "application/ogg") || known != "audio" {
			return "", errors.New("attachment content does not match audio metadata")
		}
	case AttachmentKindDocument:
		if strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "audio/") || strings.HasPrefix(contentType, "video/") {
			return "", errors.New("attachment content does not match document metadata")
		}
		if contentType == "application/pdf" && known != "pdf" {
			return "", errors.New("attachment PDF magic is invalid")
		}
	}
	return contentType, nil
}
func attachmentMagicKind(data []byte) string {
	switch {
	case len(data) >= 4 && string(data[:4]) == "OggS":
		return "audio"
	case len(data) >= 4 && string(data[:4]) == "fLaC":
		return "audio"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "audio"
	case len(data) >= 3 && string(data[:3]) == "ID3":
		return "audio"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image"
	case len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return "image"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image"
	case len(data) >= 4 && string(data[:4]) == "%PDF":
		return "pdf"
	default:
		return "unknown"
	}
}
func isLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func fileSHA256(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var digest hash.Hash = sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maxBytes+1))
	if err != nil || written > maxBytes {
		return "", errors.New("attachment checksum exceeded bound")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func ensurePrivateArtifactRoot(root, name string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New(name + " is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("resolve " + name)
	}
	abs = filepath.Clean(abs)
	if err := rejectSymlinkedAncestors(abs); err != nil {
		return "", fmt.Errorf("%s uses a symlinked path: %w", name, err)
	}
	if info, statErr := os.Lstat(abs); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return "", errors.New(name + " must be an owner-only directory")
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", errors.New("create " + name)
		}
		if err := os.Chmod(abs, 0o700); err != nil {
			return "", errors.New("set " + name + " permissions")
		}
	} else {
		return "", errors.New("inspect " + name)
	}
	return abs, nil
}

func rejectSymlinkedAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("symlinked path component")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect path component")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}
func artifactPathWithin(root, path string) bool {
	candidate := filepath.Clean(path)
	base := filepath.Clean(root)
	return candidate != base && strings.HasPrefix(candidate, base+string(os.PathSeparator))
}
