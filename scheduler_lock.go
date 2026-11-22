package bee

import (
	"errors"
	"os"
	"syscall"
)

type SchedulerLock struct{ file *os.File }

func acquireSchedulerLock(path string) (*SchedulerLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("open scheduler lock")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, errors.New("set scheduler lock permissions")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("job store is already owned")
	}
	return &SchedulerLock{file: file}, nil
}
func (lock *SchedulerLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if err != nil {
		return errors.New("release scheduler lock")
	}
	return closeErr
}
