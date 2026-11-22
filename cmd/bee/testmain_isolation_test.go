package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// The acceptance tests compile a fresh Bee binary. Preserve Go's normal
	// tool/module caches before HOME is isolated so that hermetic Bee state does
	// not turn those builds into network-dependent cold builds.
	originalHome, _ := os.UserHomeDir()
	if os.Getenv("GOCACHE") == "" {
		if cache, cacheErr := os.UserCacheDir(); cacheErr == nil {
			_ = os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
		}
	}
	if os.Getenv("GOPATH") == "" && originalHome != "" {
		_ = os.Setenv("GOPATH", filepath.Join(originalHome, "go"))
	}
	home, err := os.MkdirTemp("", "bee-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create isolated Bee test home:", err)
		os.Exit(1)
	}
	if err := os.Setenv("BEE_HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "configure isolated Bee test home:", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "configure isolated user test home:", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	_ = os.Setenv("BEE_DOCTOR_SKIP_PROVIDER_PROBE", "1")
	code := m.Run()
	if err := os.RemoveAll(home); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "remove isolated Bee test home:", err)
		code = 1
	}
	os.Exit(code)
}
