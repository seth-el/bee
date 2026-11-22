package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type CheckpointStore struct {
	path string
	mu   sync.Mutex
}

type checkpointFile struct {
	Offset int64 `json:"offset"`
}

func NewCheckpointStore(path string) (*CheckpointStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("checkpoint path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("resolve checkpoint path")
	}
	if info, statErr := os.Lstat(abs); statErr == nil && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("checkpoint path must be a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, errors.New("inspect checkpoint path")
	}
	return &CheckpointStore{path: filepath.Clean(abs)}, nil
}
func (s *CheckpointStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *CheckpointStore) Load(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, errors.New("checkpoint store is nil")
	}
	if ctx == nil {
		return 0, errors.New("checkpoint context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.New("inspect checkpoint")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 4096 {
		return 0, errors.New("checkpoint file is invalid")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return 0, errors.New("read checkpoint")
	}
	var file checkpointFile
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, errors.New("decode checkpoint")
	}
	if err := validateOffset(file.Offset); err != nil {
		return 0, err
	}
	return file.Offset, nil
}

func (s *CheckpointStore) Save(ctx context.Context, offset int64) error {
	if s == nil {
		return errors.New("checkpoint store is nil")
	}
	if ctx == nil {
		return errors.New("checkpoint context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOffset(offset); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(checkpointFile{Offset: offset})
	if err != nil {
		return errors.New("encode checkpoint")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("create checkpoint directory")
	}
	temp, err := os.CreateTemp(dir, ".telegram-checkpoint-*.tmp")
	if err != nil {
		return errors.New("create checkpoint temp file")
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return errors.New("set checkpoint permissions")
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errors.New("write checkpoint")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errors.New("sync checkpoint")
	}
	if err := temp.Close(); err != nil {
		return errors.New("close checkpoint")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return errors.New("commit checkpoint")
	}
	return nil
}

func validateOffset(offset int64) error {
	if offset < 0 || offset > MaxCheckpointOffset {
		return fmt.Errorf("checkpoint offset must be between 0 and %d", MaxCheckpointOffset)
	}
	return nil
}
