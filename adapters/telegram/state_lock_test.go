package telegram

import (
	"path/filepath"
	"testing"
)

func TestStateLockRejectsSecondOwnerAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := AcquireStateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := AcquireStateLock(path); err == nil {
		t.Fatal("second owner unexpectedly acquired lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireStateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
