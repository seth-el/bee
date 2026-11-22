package bee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHCommandUsesDynamicTargetAndNonInteractiveSudo(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "ssh-args.log")
	sshPath := filepath.Join(root, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", root+string(os.PathListSeparator)+oldPath)
	scope := ExecutionScope{Root: root, MaxOutputBytes: 4096, RemoteGrant: &RemoteCapabilityGrant{SessionID: "s", Host: "203.0.113.123", User: "testuser", Port: 2222, AllowSSH: true, AllowCommands: true, AllowElevated: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}}
	output, err := executeSSHCommand(context.Background(), runArgs{Host: "203.0.113.123", User: "testuser", Port: 2222, Argv: []string{"systemctl", "restart", "game-server"}, Elevated: true}, scope)
	if err != nil {
		t.Fatal(err)
	}
	_ = output
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(data))
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-o", "BatchMode=yes", "StrictHostKeyChecking=yes", "-p", "2222", "testuser@203.0.113.123", "sudo", "-n", "systemctl", "restart", "game-server"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("ssh args=%q missing %q", joined, expected)
		}
	}
}
