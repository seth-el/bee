package bee

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// SessionMigrationReport describes moving the legacy XDG session store to
// the configured root.
type SessionMigrationReport struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Sessions int    `json:"sessions"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Applied  bool   `json:"applied"`
	// Status is one of: nothing_to_migrate, already_migrated, ready, moved.
	Status string `json:"status"`
}

// ErrSessionStoreInUse means a session lock in the store being migrated is
// held by a running Bee process.
var ErrSessionStoreInUse = errors.New("session store is in use")

// MigrateLegacySessionStore moves the legacy XDG session store to the
// configured root. With apply false it only reports. It is idempotent: with
// nothing in the legacy store it does nothing. It refuses when the
// destination already holds anything, when a session lock is held, or when
// the two roots are on different filesystems (the move is a single rename,
// so modes, sidecars, mutation journals and scratchpads move together and
// nothing is half-copied).
func MigrateLegacySessionStore(apply bool) (SessionMigrationReport, error) {
	to, _, err := ConfiguredBeeSessionRoot()
	if err != nil {
		return SessionMigrationReport{}, err
	}
	return migrateSessionStore(LegacyBeeSessionRoot(), to, apply)
}

func migrateSessionStore(from, to string, apply bool) (SessionMigrationReport, error) {
	report := SessionMigrationReport{From: from, To: to, Status: "nothing_to_migrate"}
	if from == "" || !SessionRootHasSessions(from) {
		if SessionRootHasSessions(to) {
			report.Status = "already_migrated"
		}
		return report, nil
	}
	if info, err := os.Lstat(from); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return report, errors.New("legacy session store is not a plain directory")
	}
	if err := rejectBeeSymlinkAncestors(to); err != nil {
		return report, fmt.Errorf("validate destination: %w", err)
	}
	if entries, err := os.ReadDir(to); err == nil && len(entries) > 0 {
		return report, fmt.Errorf("destination %s is not empty; move or remove its contents first, nothing was changed", to)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return report, errors.New("read destination session store")
	}
	var locks []string
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("legacy session store contains a symlink: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Dir(path) == from && strings.HasSuffix(entry.Name(), ".jsonl") {
			report.Sessions++
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			locks = append(locks, path)
		}
		if info, err := entry.Info(); err == nil {
			report.Files++
			report.Bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	if !apply {
		report.Status = "ready"
		return report, checkSessionLocksFree(locks)
	}
	// Hold every lock across the rename so no Bee process can start a write
	// in between. flock follows the inode, so the locks stay valid as the
	// files move.
	held := make([]*os.File, 0, len(locks))
	defer func() {
		for _, file := range held {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			_ = file.Close()
		}
	}()
	for _, path := range locks {
		file, err := lockFileNoWait(path)
		if err != nil {
			return report, err
		}
		held = append(held, file)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return report, errors.New("create destination parent")
	}
	if err := os.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return report, errors.New("remove empty destination directory")
	}
	if err := os.Rename(from, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return report, fmt.Errorf("%s and %s are on different filesystems; stop Bee and move the directory by hand (cp -a, then remove the original), nothing was changed", from, to)
		}
		return report, fmt.Errorf("move session store: %w", err)
	}
	if err := os.Chmod(to, 0o700); err != nil {
		return report, errors.New("set session store permissions")
	}
	report.Applied = true
	report.Status = "moved"
	return report, nil
}

func checkSessionLocksFree(locks []string) error {
	for _, path := range locks {
		file, err := lockFileNoWait(path)
		if err != nil {
			return err
		}
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	return nil
}

// lockFileNoWait takes an existing lock file without creating one.
func lockFileNoWait(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open session lock %s", filepath.Base(path))
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s is locked; stop bee-serve, bee-telegram and any open TUI, then retry (nothing was changed)", ErrSessionStoreInUse, filepath.Base(path))
		}
		return nil, fmt.Errorf("lock %s", filepath.Base(path))
	}
	return file, nil
}
