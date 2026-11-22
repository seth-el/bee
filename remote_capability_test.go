package bee

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRemoteCapabilityGrantBindsExactDynamicTargetAndPersistsSessionScope(t *testing.T) {
	session := SessionID("session-remote")
	grant := RemoteCapabilityGrant{SessionID: session, Host: "203.0.113.123", User: "alex", Port: 22, AllowSSH: true, AllowCommands: true, AllowElevated: true, AllowStdinScripts: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := grant.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := grant.permits(commandRequest{Target: CommandTargetSSH, Host: "203.0.113.123", User: "alex", Port: 22, Elevated: true, Stdin: "#!/bin/sh\necho ok"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, request := range []commandRequest{{Target: CommandTargetSSH, Host: "203.0.113.123", User: "alex", Port: 22}, {Target: CommandTargetSSH, Host: "203.0.113.123", User: "alex", Port: 22, Elevated: true}} {
		if err := grant.permits(request, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for _, mismatch := range []commandRequest{{Target: CommandTargetSSH, Host: "203.0.113.124", User: "alex", Port: 22}, {Target: CommandTargetSSH, Host: "203.0.113.123", User: "root", Port: 22}, {Target: CommandTargetSSH, Host: "203.0.113.123", User: "alex", Port: 2200}} {
		if err := grant.permits(mismatch, time.Now().UTC()); err == nil {
			t.Fatalf("mismatch permitted: %#v", mismatch)
		}
	}
}

func TestRemoteCapabilityGrantExpiresRevokesAndRejectsUnsafeRemoteArgs(t *testing.T) {
	grant := RemoteCapabilityGrant{SessionID: "session-remote", Host: "vm.example", User: "alex", Port: 22, AllowSSH: true, AllowCommands: true, CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ExpiresAt: time.Now().UTC().Add(-time.Hour)}
	if err := grant.ValidateShape(); err == nil {
		t.Fatal("expired grant shape unexpectedly invalid")
	}
	if grant.active(time.Now().UTC()) {
		t.Fatal("expired grant active")
	}
	for _, value := range []string{"echo x && id", "$(id)", "bad\ncommand", "a|b"} {
		if err := validateRemoteArg(value); err == nil {
			t.Fatalf("unsafe arg accepted: %q", value)
		}
	}
}

func TestRunCommandSSHDefaultsOnlyFromActiveGrant(t *testing.T) {
	root := t.TempDir()
	logPath := root + "/ssh-args.log"
	sshPath := root + "/ssh"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	scope := ExecutionScope{Root: root, RemoteGrant: &RemoteCapabilityGrant{SessionID: "session-remote", Host: "203.0.113.123", User: "alex", Port: 2222, AllowSSH: true, AllowCommands: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}}
	if _, err := (RunCommandTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"target":"ssh","argv":["hostname"]}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "alex@203.0.113.123") || !strings.Contains(string(data), "2222") {
		t.Fatalf("ssh args=%q", data)
	}
	if _, err := (RunCommandTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"target":"ssh","argv":["hostname"]}`)); err == nil {
		t.Fatal("SSH without active grant unexpectedly succeeded")
	}
}

func TestRunCommandTrustedForegroundSSHUsesExplicitTargetWithoutGrant(t *testing.T) {
	root := t.TempDir()
	logPath := root + "/ssh-args.log"
	sshPath := root + "/ssh"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	scope := ExecutionScope{Root: root, TrustedForeground: true}
	output, err := (RunCommandTool{Scope: scope}).Execute(context.Background(), json.RawMessage(`{"target":"ssh","host":"vm.example","user":"testuser","port":2200,"argv":["hostname"],"elevated":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if output != "" {
		t.Fatalf("unexpected output=%q", output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "2200", "testuser@vm.example", "sudo", "-n", "hostname"} {
		if !strings.Contains(args, want) {
			t.Fatalf("ssh args=%q missing %q", args, want)
		}
	}
}

func TestRunCommandSSHRequiresTrustedForegroundOrGrant(t *testing.T) {
	root := t.TempDir()
	if _, err := (RunCommandTool{Scope: ExecutionScope{Root: root}}).Execute(context.Background(), json.RawMessage(`{"target":"ssh","host":"vm.example","user":"testuser","argv":["hostname"]}`)); err == nil {
		t.Fatal("SSH without trusted scope or grant unexpectedly succeeded")
	}
}

func TestRunCommandRemoteRequiresGrantAndRedactsStdinAudit(t *testing.T) {
	scope := ExecutionScope{Root: t.TempDir(), RemoteGrant: &RemoteCapabilityGrant{SessionID: "session-remote", Host: "203.0.113.123", User: "alex", Port: 22, AllowLocalHost: true, AllowSSH: true, AllowCommands: true, AllowElevated: true, AllowStdinScripts: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}}
	registry, err := NewRegistry(RunCommandTool{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	var audited AuditEvent
	executor := Executor{Registry: registry, Scope: scope, Policy: DefaultCodingPolicy{}, Auditor: AuditorFunc(func(_ context.Context, event AuditEvent) error { audited = event; return nil })}
	_, err = executor.Execute(context.Background(), "run_command", json.RawMessage(`{"target":"local","host":"203.0.113.123","user":"alex","argv":["cat"],"stdin":"SECRET SCRIPT"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(audited.Args), "SECRET SCRIPT") {
		t.Fatal("stdin leaked into audit")
	}
}
