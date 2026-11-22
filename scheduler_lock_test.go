package bee

import (
	"path/filepath"
	"testing"
)

func TestJobStoreLockRejectsConcurrentOwner(t *testing.T) {
	store, err := NewJobStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.acquire()
	if err == nil {
		second.Close()
		t.Fatal("second scheduler owner acquired lock")
	}
}
