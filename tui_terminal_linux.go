//go:build linux

package bee

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

type tuiTerminal struct {
	input    *os.File
	output   *os.File
	original syscall.Termios
	entered  bool
}

type tuiWindowSize struct {
	rows uint16
	cols uint16
	x    uint16
	y    uint16
}

func (terminal *tuiTerminal) eraseByte() byte {
	return terminal.original.Cc[syscall.VERASE]
}

func newTUITerminal(input io.Reader, output io.Writer) (*tuiTerminal, bool, error) {
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	if !inputOK || !outputOK || inputFile == nil || outputFile == nil {
		return nil, false, nil
	}
	var termios syscall.Termios
	if err := tuiIoctl(inputFile.Fd(), uintptr(syscall.TCGETS), unsafe.Pointer(&termios)); err != nil {
		return nil, false, nil
	}
	var size tuiWindowSize
	if err := tuiIoctl(outputFile.Fd(), uintptr(syscall.TIOCGWINSZ), unsafe.Pointer(&size)); err != nil {
		return nil, false, nil
	}
	return &tuiTerminal{input: inputFile, output: outputFile, original: termios}, true, nil
}

func (terminal *tuiTerminal) enter() error {
	raw := terminal.original
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := tuiIoctl(terminal.input.Fd(), uintptr(syscall.TCSETS), unsafe.Pointer(&raw)); err != nil {
		return err
	}
	terminal.entered = true
	// Mouse reporting deliberately remains disabled so the terminal retains
	// native drag-selection and copy behavior. Page Up/Page Down provide TUI
	// transcript scrolling without stealing ordinary mouse input.
	if _, err := io.WriteString(terminal.output, "\x1b[?1049h\x1b[?2004h\x1b[?1000l\x1b[?1006l\x1b[?7l\x1b[?25l\x1b[2J\x1b[H"); err != nil {
		_ = tuiIoctl(terminal.input.Fd(), uintptr(syscall.TCSETS), unsafe.Pointer(&terminal.original))
		terminal.entered = false
		return err
	}
	return nil
}

func (terminal *tuiTerminal) leave() error {
	if terminal == nil || !terminal.entered {
		return nil
	}
	terminal.entered = false
	_, writeErr := io.WriteString(terminal.output, "\x1b[?25h\x1b[?7h\x1b[?1000l\x1b[?1006l\x1b[?2004l\x1b[?1049l")
	restoreErr := tuiIoctl(terminal.input.Fd(), uintptr(syscall.TCSETS), unsafe.Pointer(&terminal.original))
	return errors.Join(writeErr, restoreErr)
}

func (terminal *tuiTerminal) size() (int, int, error) {
	var size tuiWindowSize
	if err := tuiIoctl(terminal.output.Fd(), uintptr(syscall.TIOCGWINSZ), unsafe.Pointer(&size)); err != nil {
		return 0, 0, err
	}
	return int(size.cols), int(size.rows), nil
}

func (terminal *tuiTerminal) read(timeout time.Duration) ([]byte, error) {
	fd := int(terminal.input.Fd())
	var readSet syscall.FdSet
	index := fd / 64
	if index < 0 || index >= len(readSet.Bits) {
		return nil, errors.New("tui input file descriptor is outside select range")
	}
	readSet.Bits[index] |= int64(1) << uint(fd%64)
	timeval := syscall.NsecToTimeval(timeout.Nanoseconds())
	ready, err := syscall.Select(fd+1, &readSet, nil, nil, &timeval)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, nil
		}
		return nil, err
	}
	if ready == 0 {
		return nil, nil
	}
	buffer := make([]byte, 256)
	count, err := terminal.input.Read(buffer)
	if count > 0 {
		return buffer[:count], nil
	}
	return nil, err
}

func (terminal *tuiTerminal) write(value string) error {
	_, err := io.WriteString(terminal.output, value)
	return err
}

func tuiIoctl(fd uintptr, request uintptr, value unsafe.Pointer) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, request, uintptr(value), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
