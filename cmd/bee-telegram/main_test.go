package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seth-el/bee"
)

func TestComposeTelegramAssistantUsesGeneralComposition(t *testing.T) {
	store, err := bee.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := bee.DefaultConfig()
	memoryRoot := t.TempDir()
	if err := os.Chmod(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	config.Assistant.MemoryRoot = memoryRoot
	config.Assistant.JobStorePath = filepath.Join(t.TempDir(), "jobs.json")

	app, err := composeTelegramAssistant(config, "", store)
	if err != nil {
		t.Fatal(err)
	}
	if app.Store != store {
		t.Fatal("Telegram composition did not preserve the supplied session store")
	}
	if app.Executor == nil || app.MemoryService == nil || app.JobStore == nil {
		t.Fatalf("Telegram composition is incomplete: executor=%v memory=%v jobs=%v", app.Executor != nil, app.MemoryService != nil, app.JobStore != nil)
	}
	if _, ok := app.Executor.Registry.Lookup("read_file"); !ok {
		t.Fatal("Telegram composition is missing the general assistant tool registry")
	}
}
