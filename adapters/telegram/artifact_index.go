package telegram

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/seth-el/bee"
)

type ArtifactState string

const (
	ArtifactStaged     ArtifactState = "staged"
	ArtifactDelivering ArtifactState = "delivering"
	ArtifactDelivered  ArtifactState = "delivered"
	ArtifactFailed     ArtifactState = "failed"
	ArtifactExpired    ArtifactState = "expired"
)

type ArtifactRecord struct {
	Reference   string        `json:"reference"`
	Kind        string        `json:"kind"`
	Filename    string        `json:"filename,omitempty"`
	ContentType string        `json:"content_type,omitempty"`
	SizeBytes   int64         `json:"size_bytes"`
	SHA256      string        `json:"sha256,omitempty"`
	RouteKey    string        `json:"route_key,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	ExpiresAt   time.Time     `json:"expires_at"`
	State       ArtifactState `json:"state"`
	DeliveredAt *time.Time    `json:"delivered_at,omitempty"`
}

type ArtifactIndex struct {
	path string
	mu   sync.Mutex
}

func NewArtifactIndex(path string) (*ArtifactIndex, error) {
	if path == "" || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return nil, errors.New("artifact index path is invalid")
	}
	if err := rejectSymlinkedAncestors(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return &ArtifactIndex{path: filepath.Clean(path)}, nil
}

func (index *ArtifactIndex) Upsert(record ArtifactRecord) error {
	if index == nil || index.path == "" {
		return errors.New("artifact index is not configured")
	}
	if err := (bee.AttachmentRef{Reference: record.Reference, Filename: record.Filename, ContentType: record.ContentType, SizeBytes: record.SizeBytes, SHA256: record.SHA256}).Validate(); err != nil {
		return err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	records, err := index.loadLocked()
	if err != nil {
		return err
	}
	found := false
	for i := range records {
		if records[i].Reference == record.Reference {
			records[i] = record
			found = true
			break
		}
	}
	if !found {
		records = append(records, record)
	}
	return index.saveLocked(records)
}

func (index *ArtifactIndex) Mark(reference string, state ArtifactState, deliveredAt *time.Time) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	records, err := index.loadLocked()
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].Reference == reference {
			records[i].State = state
			records[i].DeliveredAt = deliveredAt
			return index.saveLocked(records)
		}
	}
	return os.ErrNotExist
}

func (index *ArtifactIndex) Get(reference string) (ArtifactRecord, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	records, err := index.loadLocked()
	if err != nil {
		return ArtifactRecord{}, err
	}
	for _, record := range records {
		if record.Reference == reference {
			return record, nil
		}
	}
	return ArtifactRecord{}, os.ErrNotExist
}

func (index *ArtifactIndex) Expire(now time.Time) ([]ArtifactRecord, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	records, err := index.loadLocked()
	if err != nil {
		return nil, err
	}
	var expired []ArtifactRecord
	for i := range records {
		if !records[i].ExpiresAt.IsZero() && !records[i].ExpiresAt.After(now) && records[i].State != ArtifactExpired {
			records[i].State = ArtifactExpired
			expired = append(expired, records[i])
		}
	}
	if len(expired) > 0 {
		err = index.saveLocked(records)
	}
	return expired, err
}

func (index *ArtifactIndex) Records(now time.Time) ([]ArtifactRecord, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	records, err := index.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactRecord, 0, len(records))
	for _, record := range records {
		if record.State == ArtifactExpired || (!record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now)) {
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

func (index *ArtifactIndex) loadLocked() ([]ArtifactRecord, error) {
	data, err := os.ReadFile(index.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("read artifact index")
	}
	var records []ArtifactRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, errors.New("decode artifact index")
	}
	return records, nil
}

func (index *ArtifactIndex) saveLocked(records []ArtifactRecord) error {
	if err := os.MkdirAll(filepath.Dir(index.path), 0o700); err != nil {
		return errors.New("create artifact index directory")
	}
	data, err := json.Marshal(records)
	if err != nil {
		return errors.New("encode artifact index")
	}
	tmp, err := os.CreateTemp(filepath.Dir(index.path), ".artifact-index-*")
	if err != nil {
		return errors.New("create artifact index temp")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("set artifact index permissions")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("write artifact index")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("sync artifact index")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("close artifact index")
	}
	if err := os.Rename(name, index.path); err != nil {
		return errors.New("commit artifact index")
	}
	return nil
}
