//go:build !linux

package bee

import (
	"io"
	"time"
)

type tuiTerminal struct {
	output io.Writer
}

func newTUITerminal(io.Reader, io.Writer) (*tuiTerminal, bool, error) {
	return nil, false, nil
}

func (*tuiTerminal) enter() error                       { return nil }
func (*tuiTerminal) leave() error                       { return nil }
func (*tuiTerminal) size() (int, int, error)            { return 80, 24, nil }
func (*tuiTerminal) read(time.Duration) ([]byte, error) { return nil, io.EOF }
func (terminal *tuiTerminal) write(value string) error {
	_, err := io.WriteString(terminal.output, value)
	return err
}
