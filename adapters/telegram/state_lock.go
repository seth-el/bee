package telegram

import (
	"errors"
	"os"
	"syscall"
)

type StateLock struct{ file *os.File }

func AcquireStateLock(path string) (*StateLock, error) {
	if path == "" {
		return nil, errors.New("state lock path is required")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open state lock")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, errors.New("set state lock permissions")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("state root is already owned")
	}
	return &StateLock{file: file}, nil
}

func (lock *StateLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if err != nil {
		return errors.New("release state lock")
	}
	return closeErr
}
