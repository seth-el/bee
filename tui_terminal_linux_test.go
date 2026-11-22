//go:build linux

package bee

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/creack/pty"
)

type tuiPTYProvider struct {
	calls atomic.Int32
}

func (*tuiPTYProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, errors.New("unexpected non-streaming request")
}

func (provider *tuiPTYProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (Response, error) {
	content := fmt.Sprintf("streamed PTY answer %d\n\n```go\nfmt.Println(\"ok\")\n```", provider.calls.Add(1))
	if err := onDelta(content); err != nil {
		return Response{}, err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: content}, Usage: Usage{TotalTokens: 7}}, nil
}

func TestRunTUIUsesPersistentComposerOnPTY(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 12, Cols: 60}); err != nil {
		t.Fatal(err)
	}
	var original syscall.Termios
	if err := tuiIoctl(slave.Fd(), uintptr(syscall.TCGETS), unsafe.Pointer(&original)); err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), &tuiPTYProvider{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunTUI(ctx, app, slave, slave)
	}()

	initialOutput := readPTYUntil(t, master, 4*time.Second, func(value string) bool {
		return strings.Contains(value, "\x1b[?1049h") && strings.Contains(value, " context ")
	})
	if !strings.Contains(initialOutput, "\x1b[11;2H") {
		t.Fatalf("initial composer was not ready: %q", initialOutput)
	}
	if _, err := master.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	output := readPTYUntil(t, master, 4*time.Second, func(value string) bool {
		answer := strings.LastIndex(value, "streamed PTY answer 1")
		return answer >= 0 && strings.Contains(value[answer:], " Enter send ")
	})
	allOutput := initialOutput + output
	for _, want := range []string{"\x1b[?1049h", "\x1b[?7l", "\x1b[10;2H", "\x1b[11;2H", "\x1b[12;2H"} {
		if !strings.Contains(allOutput, want) {
			t.Fatalf("PTY output missing %q: %q", want, allOutput)
		}
	}
	visible := tuiSafeText(output)
	for _, want := range []string{"streamed PTY answer 1", "┌─ go", "│ fmt.Println(\"ok\")", "Enter send · Ctrl+J newline"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("visible PTY output missing %q: %q", want, visible)
		}
	}
	for _, mouseCapture := range []string{"\x1b[?1000h", "\x1b[?1006h"} {
		if strings.Contains(allOutput, mouseCapture) {
			t.Fatalf("PTY enabled mouse capture %q and prevents text selection", mouseCapture)
		}
	}
	if _, err := master.Write([]byte("again\r")); err != nil {
		t.Fatal(err)
	}
	secondOutput := readPTYUntil(t, master, 4*time.Second, func(value string) bool {
		answer := strings.LastIndex(value, "streamed PTY answer 2")
		return answer >= 0 && strings.Contains(value[answer:], " Enter send ")
	})
	for _, row := range []string{"\x1b[11;2H", "\x1b[12;2H"} {
		if !strings.Contains(secondOutput, row) {
			t.Fatalf("composer did not recover after the first response; missing %q: %q", row, secondOutput)
		}
	}
	if _, err := master.Write([]byte("/exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("TUI did not exit after /exit")
	}
	var restored syscall.Termios
	if err := tuiIoctl(slave.Fd(), uintptr(syscall.TCGETS), unsafe.Pointer(&restored)); err != nil {
		t.Fatal(err)
	}
	if restored != original {
		t.Fatal("TUI did not restore the original terminal mode")
	}
}

func readPTYUntil(t *testing.T, master *os.File, timeout time.Duration, done func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	if err := master.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	defer master.SetReadDeadline(time.Time{})
	var output strings.Builder
	buffer := make([]byte, 4096)
	for {
		count, err := master.Read(buffer)
		if count > 0 {
			output.Write(buffer[:count])
			if done(output.String()) {
				return output.String()
			}
		}
		if err != nil {
			t.Fatalf("reading PTY output: %v (output %q)", err, output.String())
		}
	}
}
