//go:build windows

package bee

import (
	"errors"
	"os/exec"
)

func configureChildProcessGroup(command *exec.Cmd) error {
	return nil
}

func killChildProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return errors.New("child process-group cleanup is unsupported: Windows is outside Bee's project scope")
}
