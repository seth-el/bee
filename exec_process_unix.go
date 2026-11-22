//go:build !windows

package bee

import (
	"os/exec"
	"syscall"
)

func configureCommandProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCommandProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	if err := killProcessGroup(command.Process.Pid); err != nil {
		return err
	}
	return nil
}
