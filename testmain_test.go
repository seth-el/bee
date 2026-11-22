package bee

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps tests that use Bee's default paths isolated from the
// developer's real profile. Individual tests can still override BEE_HOME with
// t.Setenv when they need to exercise a particular layout.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "bee-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create isolated Bee test home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("BEE_HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "configure isolated Bee test home: %v\n", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "configure isolated user test home: %v\n", err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	// No model servers run under test; doctor's reachability probe is
	// covered by its own test.
	_ = os.Setenv("BEE_DOCTOR_SKIP_PROVIDER_PROBE", "1")

	code := m.Run()
	if err := os.RemoveAll(home); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove isolated Bee test home: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
