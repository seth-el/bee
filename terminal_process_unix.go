//go:build !windows

package bee

import (
	"os"
	"syscall"
)

func signalProcessGroup(pid int, signal os.Signal) error {
	if pid <= 0 {
		return nil
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	return syscall.Kill(-pid, sig)
}
func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
