package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MaxAttachmentBytes        int64 = 104857600
	MaxAttachmentExtractBytes       = 64 << 10
	MaxAttachmentCount              = 8
)

var ErrAttachmentNotFound = errors.New("attachment not found")

// Attachment is safe metadata for one uploaded file. Binary content remains
// in ArtifactStore and is never serialized in a session event.
type Attachment struct {
	ID                   string    `json:"id"`
	SessionID            SessionID `json:"session_id"`
	ArtifactID           string    `json:"artifact_id"`
	Filename             string    `json:"filename"`
	MediaType            string    `json:"media_type"`
	Kind                 string    `json:"kind"`
	Size                 int64     `json:"size"`
	Status               string    `json:"status"`
	Error                string    `json:"error,omitempty"`
	Interpretation       string    `json:"interpretation,omitempty"`
	TranscriptArtifactID string    `json:"transcript_artifact_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type AttachmentStoreOptions struct {
	AudioScriptPath string
}

type AttachmentStore struct {
	root        string
	artifacts   *ArtifactStore
	audioScript string
}

func NewAttachmentStore(root string, artifacts *ArtifactStore, options ...AttachmentStoreOptions) (*AttachmentStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("attachment store root is required")
	}
	if artifacts == nil {
		return nil, errors.New("attachment artifact store is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := ValidateBeePath(abs); err != nil {
		return nil, err
	}
	if err := rejectBeeSymlinkAncestors(abs); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment store root: %w", err)
	}
	script := ""
	if len(options) > 0 && strings.TrimSpace(options[0].AudioScriptPath) != "" {
		script = strings.TrimSpace(options[0].AudioScriptPath)
	}
	return &AttachmentStore{root: filepath.Clean(abs), artifacts: artifacts, audioScript: script}, nil
}

func (store *AttachmentStore) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

func (store *AttachmentStore) metadataPath(id string) string {
	return filepath.Join(store.root, id+".json")
}

func validateAttachmentID(id string) error {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) || strings.ContainsRune(id, '\x00') {
		return errors.New("invalid attachment id")
	}
	return nil
}

func sniffAttachmentMediaType(filename, provided string, data []byte) string {
	// Sniff the bytes first. A client-provided type is only a fallback when the
	// detector cannot distinguish the payload.
	if len(data) > 0 {
		if detected := http.DetectContentType(data); detected != "application/octet-stream" {
			return strings.Split(detected, ";")[0]
		}
	}
	if inferred := mimeTypeByExtension(filename); inferred != "" {
		return inferred
	}
	if strings.TrimSpace(provided) != "" {
		return strings.Split(strings.TrimSpace(provided), ";")[0]
	}
	return "application/octet-stream"
}

func mimeTypeByExtension(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".log":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".css", ".html", ".rs", ".java", ".c", ".h", ".cpp", ".sh":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	case ".webm":
		return "audio/webm"
	default:
		return ""
	}
}

func attachmentKind(media string) string {
	if strings.HasPrefix(media, "text/") || media == "application/json" || strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml") || media == "application/xml" || media == "text/yaml" {
		return "text"
	}
	if strings.HasPrefix(media, "audio/") {
		return "audio"
	}
	if strings.HasPrefix(media, "image/") {
		return "image"
	}
	return "binary"
}

func (store *AttachmentStore) writeMetadata(att Attachment) error {
	data, err := json.Marshal(att)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(store.root, ".attachment-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, store.metadataPath(att.ID))
}

func (store *AttachmentStore) load(id string) (Attachment, error) {
	if store == nil {
		return Attachment{}, errors.New("attachment store is nil")
	}
	if err := validateAttachmentID(id); err != nil {
		return Attachment{}, err
	}
	info, err := os.Lstat(store.metadataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Attachment{}, ErrAttachmentNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Attachment{}, errors.New("attachment metadata is not a safe regular file")
	}
	data, err := os.ReadFile(store.metadataPath(id))
	if err != nil {
		return Attachment{}, err
	}
	var att Attachment
	if err := json.Unmarshal(data, &att); err != nil {
		return Attachment{}, err
	}
	if att.ID != id || att.SessionID == "" || att.ArtifactID == "" {
		return Attachment{}, errors.New("attachment metadata is invalid")
	}
	return att, nil
}

func (store *AttachmentStore) Get(id string) (Attachment, error) { return store.load(id) }

func (store *AttachmentStore) scanSessionAttachments(sessionID SessionID) ([]Attachment, []string, error) {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return nil, nil, err
	}
	result := make([]Attachment, 0)
	ids := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		att, loadErr := store.load(strings.TrimSuffix(entry.Name(), ".json"))
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if att.SessionID == sessionID {
			ids = append(ids, att.ID)
			result = append(result, att)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, ids, nil
}

func (store *AttachmentStore) List(sessionID SessionID) ([]Attachment, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	if err := ensureScopedIndexDir(store.root); err != nil {
		result, _, scanErr := store.scanSessionAttachments(sessionID)
		return result, scanErr
	}
	lock, lockErr := acquireSessionFileLock(scopedIndexLockPath(store.root, sessionID))
	if lockErr != nil {
		result, _, scanErr := store.scanSessionAttachments(sessionID)
		return result, scanErr
	}
	defer lock.Close()
	index, ok, _ := readScopedIndex(store.root, sessionID)
	if ok {
		result := make([]Attachment, 0, len(index.IDs))
		for _, id := range index.IDs {
			att, loadErr := store.load(id)
			if loadErr != nil || att.SessionID != sessionID {
				ok = false
				break
			}
			result = append(result, att)
		}
		if ok {
			sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
			return result, nil
		}
	}
	var result []Attachment
	var ids []string
	for attempt := 0; attempt < 3; attempt++ {
		before, beforeErr := scopedSourceModTime(store.root)
		if beforeErr != nil {
			return nil, beforeErr
		}
		var scanErr error
		result, ids, scanErr = store.scanSessionAttachments(sessionID)
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

func (store *AttachmentStore) Create(ctx context.Context, sessionID SessionID, filename, mediaType string, data []byte) (Attachment, error) {
	if err := validateSessionID(sessionID); err != nil {
		return Attachment{}, err
	}
	if int64(len(data)) > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf("attachment exceeds %d byte limit", MaxAttachmentBytes)
	}
	if len(data) == 0 {
		return Attachment{}, errors.New("attachment is empty")
	}
	if _, err := store.artifacts.store.Load(sessionID); err != nil {
		return Attachment{}, err
	}
	artifact, err := store.artifacts.Create(ctx, sessionID, "", ArtifactInput{Filename: filename, MediaType: sniffAttachmentMediaType(filename, mediaType, data), Content: data, MaxBytes: MaxAttachmentBytes})
	if err != nil {
		return Attachment{}, err
	}
	status := "uploaded"
	interpretation := ""
	kind := attachmentKind(artifact.MediaType)
	if kind == "image" {
		status, interpretation = "ready", "not_interpreted_provider_capability_unavailable"
	}
	att := Attachment{ID: "att-" + newStableID(), SessionID: sessionID, ArtifactID: artifact.ID, Filename: artifact.Filename, MediaType: artifact.MediaType, Kind: kind, Size: artifact.Size, Status: status, Interpretation: interpretation, CreatedAt: nowUTC(), UpdatedAt: nowUTC()}
	if err := store.writeMetadata(att); err != nil {
		return Attachment{}, err
	}
	_ = appendScopedIndexID(store.root, sessionID, att.ID)
	return att, nil
}

func (store *AttachmentStore) UpdateStatus(id, status, detail string) (Attachment, error) {
	att, err := store.load(id)
	if err != nil {
		return Attachment{}, err
	}
	att.Status, att.Error, att.UpdatedAt = status, strings.TrimSpace(detail), nowUTC()
	if err := store.writeMetadata(att); err != nil {
		return Attachment{}, err
	}
	_ = appendScopedIndexID(store.root, att.SessionID, att.ID)
	return att, nil
}

func (store *AttachmentStore) StartAudioProcessing(ctx context.Context, id string) {
	go func() { _, _ = store.ProcessAudio(ctx, id) }()
}

func (store *AttachmentStore) ProcessAudio(ctx context.Context, id string) (Attachment, error) {
	att, err := store.load(id)
	if err != nil {
		return Attachment{}, err
	}
	if att.Kind != "audio" {
		return att, errors.New("attachment is not audio")
	}
	if strings.TrimSpace(store.audioScript) == "" {
		err := errors.New("audio transcription is not configured")
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	if _, err := store.UpdateStatus(id, "processing", ""); err != nil {
		return Attachment{}, err
	}
	artifact, file, err := store.artifacts.OpenPayload(att.ArtifactID)
	if err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	defer file.Close()
	tmp, err := os.CreateTemp("", "bee-attachment-audio-*")
	if err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	if _, err := io.CopyN(tmp, file, artifact.Size); err != nil {
		_ = tmp.Close()
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	if err := tmp.Close(); err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	cmd := exec.CommandContext(ctx, store.audioScript, tmpName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	if err := cmd.Start(); err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	transcript, readErr := io.ReadAll(io.LimitReader(stdout, MaxAttachmentExtractBytes+1))
	waitErr := cmd.Wait()
	if readErr != nil {
		_, _ = store.UpdateStatus(id, "failed", readErr.Error())
		return Attachment{}, readErr
	}
	if waitErr != nil {
		_, _ = store.UpdateStatus(id, "failed", waitErr.Error())
		return Attachment{}, waitErr
	}
	if len(transcript) > MaxAttachmentExtractBytes {
		err = fmt.Errorf("audio transcript exceeds %d byte limit", MaxAttachmentExtractBytes)
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	text := strings.TrimSpace(string(transcript))
	if text == "" {
		err = errors.New("audio transcription returned empty text")
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	derived, err := store.artifacts.Create(ctx, att.SessionID, "attachment:"+id, ArtifactInput{Filename: att.Filename + ".txt", MediaType: "text/plain", Content: []byte(text)})
	if err != nil {
		_, _ = store.UpdateStatus(id, "failed", err.Error())
		return Attachment{}, err
	}
	att, err = store.load(id)
	if err != nil {
		return Attachment{}, err
	}
	att.Status, att.Error, att.TranscriptArtifactID, att.UpdatedAt = "ready", "", derived.ID, nowUTC()
	if err := store.writeMetadata(att); err != nil {
		return Attachment{}, err
	}
	return att, nil
}

func (store *AttachmentStore) ResolveForTurn(sessionID SessionID, ids []string) ([]AttachmentRef, string, error) {
	return store.ResolveForTurnWithImageCapability(sessionID, ids, false)
}

// ResolveForTurnWithImageCapability resolves attachment references and
// injects only bounded text/transcript context. Images are left for a
// capability-aware provider when supportsImages is true; otherwise the
// existing explicit unsupported note is retained.
func (store *AttachmentStore) ResolveForTurnWithImageCapability(sessionID SessionID, ids []string, supportsImages bool) ([]AttachmentRef, string, error) {
	if len(ids) > MaxAttachmentCount {
		return nil, "", fmt.Errorf("at most %d attachments may be sent", MaxAttachmentCount)
	}
	refs := make([]AttachmentRef, 0, len(ids))
	var context strings.Builder
	for _, id := range ids {
		att, err := store.load(strings.TrimSpace(id))
		if err != nil {
			return nil, "", err
		}
		if att.SessionID != sessionID {
			return nil, "", errors.New("attachment does not belong to session")
		}
		if att.Kind == "audio" && att.Status != "ready" {
			return nil, "", fmt.Errorf("attachment %q is not ready", att.Filename)
		}
		ref := AttachmentRef{ID: att.ID, ArtifactID: att.ArtifactID, Filename: att.Filename, MediaType: att.MediaType, Kind: att.Kind, Status: att.Status}
		refs = append(refs, ref)
		if att.Kind == "text" {
			_, data, truncated, previewErr := store.artifacts.Preview(att.ArtifactID)
			if previewErr != nil {
				return nil, "", previewErr
			}
			context.WriteString("\n--- attachment: ")
			context.WriteString(att.Filename)
			context.WriteString(" ---\n")
			context.Write(data)
			if truncated {
				context.WriteString("\n[attachment preview truncated]")
			}
			context.WriteString("\n--- end attachment ---\n")
		} else if att.Kind == "audio" && att.TranscriptArtifactID != "" {
			_, data, truncated, previewErr := store.artifacts.Preview(att.TranscriptArtifactID)
			if previewErr != nil {
				return nil, "", previewErr
			}
			context.WriteString("\n--- transcript from attachment: ")
			context.WriteString(att.Filename)
			context.WriteString(" ---\n")
			context.Write(data)
			if truncated {
				context.WriteString("\n[transcript truncated]")
			}
			context.WriteString("\n--- end transcript ---\n")
		} else if att.Kind == "image" && !supportsImages {
			context.WriteString("\nAttachment ")
			context.WriteString(att.Filename)
			context.WriteString(" was stored but not interpreted because the configured provider has no declared image capability.\n")
		}
	}
	return refs, context.String(), nil
}
