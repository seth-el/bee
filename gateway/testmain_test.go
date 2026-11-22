package gateway

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "bee-gateway-test-home-")
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
	code := m.Run()
	if err := os.RemoveAll(home); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "remove isolated Bee test home:", err)
		code = 1
	}
	os.Exit(code)
}
