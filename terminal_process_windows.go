//go:build windows

package bee

import (
	"errors"
	"os"
)

func signalProcessGroup(pid int, signal os.Signal) error {
	return errors.New("terminal process-group signals are unsupported on Windows")
}
func killProcessGroup(pid int) error {
	return errors.New("terminal process-group cleanup is unsupported on Windows")
}
