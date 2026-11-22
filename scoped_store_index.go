package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const scopedStoreIndexVersion = 1

type scopedStoreIndex struct {
	Version       int       `json:"version"`
	SessionID     SessionID `json:"session_id"`
	SourceModTime int64     `json:"source_mod_time_ns"`
	IDs           []string  `json:"ids"`
}

func scopedIndexDir(root string) string { return filepath.Join(root, ".session-index") }
func scopedIndexPath(root string, sessionID SessionID) string {
	return filepath.Join(scopedIndexDir(root), string(sessionID)+".json")
}
func scopedIndexLockPath(root string, sessionID SessionID) string {
	return scopedIndexPath(root, sessionID) + ".lock"
}

func ensureScopedIndexDir(root string) error { return ensurePrivateDir(scopedIndexDir(root), 0o700) }

func scopedSourceModTime(root string) (int64, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func readScopedIndexRaw(root string, sessionID SessionID) (scopedStoreIndex, bool, error) {
	data, err := os.ReadFile(scopedIndexPath(root, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return scopedStoreIndex{}, false, nil
	}
	if err != nil {
		return scopedStoreIndex{}, false, err
	}
	var index scopedStoreIndex
	if err := json.Unmarshal(data, &index); err != nil || index.Version != scopedStoreIndexVersion || index.SessionID != sessionID {
		return scopedStoreIndex{}, false, nil
	}
	return index, true, nil
}

func readScopedIndex(root string, sessionID SessionID) (scopedStoreIndex, bool, error) {
	sourceModTime, err := scopedSourceModTime(root)
	if err != nil {
		return scopedStoreIndex{}, false, err
	}
	index, ok, err := readScopedIndexRaw(root, sessionID)
	if err != nil || !ok {
		return scopedStoreIndex{}, false, err
	}
	if index.SourceModTime != sourceModTime {
		return scopedStoreIndex{}, false, nil
	}
	seen := make(map[string]struct{}, len(index.IDs))
	for _, id := range index.IDs {
		if id == "" {
			return scopedStoreIndex{}, false, nil
		}
		if _, ok := seen[id]; ok {
			return scopedStoreIndex{}, false, nil
		}
		seen[id] = struct{}{}
	}
	return index, true, nil
}

func writeScopedIndex(root string, sessionID SessionID, ids []string) error {
	if err := ensureScopedIndexDir(root); err != nil {
		return err
	}
	sourceModTime, err := scopedSourceModTime(root)
	if err != nil {
		return err
	}
	return writeScopedIndexAt(root, sessionID, ids, sourceModTime)
}

// writeScopedIndexAt installs an index tied to the source-directory timestamp
// observed by the authoritative scan. If the source changes after that scan,
// the sidecar remains deliberately stale and the next read rebuilds it rather
// than accepting an incomplete ID set as current.
func writeScopedIndexAt(root string, sessionID SessionID, ids []string, sourceModTime int64) error {
	if err := ensureScopedIndexDir(root); err != nil {
		return err
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	index := scopedStoreIndex{Version: scopedStoreIndexVersion, SessionID: sessionID, SourceModTime: sourceModTime, IDs: ids}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(scopedIndexDir(root), ".index-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, scopedIndexPath(root, sessionID)); err != nil {
		return fmt.Errorf("install scoped index: %w", err)
	}
	return nil
}

func writeScopedIndexLocked(root string, sessionID SessionID, ids []string) error {
	if err := ensureScopedIndexDir(root); err != nil {
		return err
	}
	lock, err := acquireSessionFileLock(scopedIndexLockPath(root, sessionID))
	if err != nil {
		return err
	}
	defer lock.Close()
	return writeScopedIndex(root, sessionID, ids)
}

func appendScopedIndexID(root string, sessionID SessionID, id string) error {
	if err := ensureScopedIndexDir(root); err != nil {
		return err
	}
	lock, err := acquireSessionFileLock(scopedIndexLockPath(root, sessionID))
	if err != nil {
		return err
	}
	defer lock.Close()
	index, ok, err := readScopedIndex(root, sessionID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		ok = false
	}
	if !ok {
		// A source timestamp changes after each create/update. Preserve a
		// structurally valid prior ID list across that expected staleness; List
		// still validates the timestamp and rebuilds if a write was missed.
		index, ok, _ = readScopedIndexRaw(root, sessionID)
	}
	if !ok {
		index = scopedStoreIndex{Version: scopedStoreIndexVersion, SessionID: sessionID}
	}
	for _, existing := range index.IDs {
		if existing == id {
			return writeScopedIndex(root, sessionID, index.IDs)
		}
	}
	index.IDs = append(index.IDs, id)
	return writeScopedIndex(root, sessionID, index.IDs)
}

func removeScopedIndexID(root string, sessionID SessionID, id string) error {
	if err := ensureScopedIndexDir(root); err != nil {
		return err
	}
	lock, err := acquireSessionFileLock(scopedIndexLockPath(root, sessionID))
	if err != nil {
		return err
	}
	defer lock.Close()
	index, ok, err := readScopedIndex(root, sessionID)
	if err != nil || !ok {
		return nil
	}
	filtered := index.IDs[:0]
	for _, existing := range index.IDs {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	return writeScopedIndex(root, sessionID, filtered)
}
