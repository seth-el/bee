package bee

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

type SessionFileLock struct {
	file *os.File
}

// tryAcquireSessionFileLock acquires an advisory lock without waiting. It is
// used for turn-level leases, where callers need a stable busy response rather
// than an unbounded queue behind a model/tool run.
func tryAcquireSessionFileLock(path string, busy error) (*SessionFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open session lock")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("set session lock permissions")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, busy
		}
		return nil, errors.New("acquire session lock")
	}
	return &SessionFileLock{file: file}, nil
}

func acquireSessionFileLock(path string) (*SessionFileLock, error) {
	return acquireSessionFileLockContext(context.Background(), path)
}

func acquireSessionFileLockContext(ctx context.Context, path string) (*SessionFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open session lock")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("set session lock permissions")
	}
	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &SessionFileLock{file: file}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, errors.New("acquire session lock")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (lock *SessionFileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if err != nil {
		return errors.New("release session lock")
	}
	return closeErr
}
