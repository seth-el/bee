//go:build !windows

package bee

import (
	"os/exec"
	"syscall"
)

func configureChildProcessGroup(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func killChildProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	// The child and every descendant inherit this dedicated process group.
	// Negative-PID signalling is what prevents a launcher wrapper from
	// surviving cancellation with a still-running grandchild.
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
