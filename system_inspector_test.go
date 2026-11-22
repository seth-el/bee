package bee

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type inspectorCommandStub struct {
	calls  [][]string
	output map[string][]byte
	err    error
}

func (s *inspectorCommandStub) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, append([]string{name}, args...))
	if s.err != nil {
		return nil, s.err
	}
	return s.output[name], nil
}

func TestLinuxSystemInspectorOwnUserProcAndServiceArgv(t *testing.T) {
	if err := validateServiceUnit("bee-gateway.service"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"--all.service", "../x.service", "bee*.service", "bee-gateway"} {
		if err := validateServiceUnit(unit); err == nil {
			t.Fatalf("unsafe unit accepted: %q", unit)
		}
	}
	stub := &inspectorCommandStub{output: map[string][]byte{"systemctl": []byte("bee-gateway.service loaded active running Bee gateway\n"), "journalctl": []byte(`{"__REALTIME_TIMESTAMP":"1","PRIORITY":"6","SYSLOG_IDENTIFIER":"bee","_PID":"12","MESSAGE":"ok"}` + "\n")}}
	inspector := &LinuxSystemInspector{Systemd: true, Runner: stub, ProcRoot: "/proc", UID: -1}
	services, err := inspector.ServiceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Unit != "bee-gateway.service" {
		t.Fatalf("services=%#v", services)
	}
	logs, err := inspector.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "bee-gateway.service", Lines: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Message != "ok" {
		t.Fatalf("logs=%#v", logs)
	}
	if len(stub.calls) < 2 || stub.calls[0][0] != "systemctl" || stub.calls[0][1] != "--user" {
		t.Fatalf("calls=%#v", stub.calls)
	}
}

func TestSystemInspectorUnsupportedAndSecretRedaction(t *testing.T) {
	stub := &inspectorCommandStub{output: map[string][]byte{"journalctl": []byte(`{"MESSAGE":"api_key=secret"}` + "\n")}}
	inspector := &LinuxSystemInspector{Systemd: true, Runner: stub, UID: -1}
	logs, err := inspector.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "bee.service"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Message != "[REDACTED]" {
		t.Fatalf("logs=%#v", logs)
	}
	inspector.Systemd = false
	if _, err := inspector.ServiceList(context.Background()); err == nil || !errors.Is(err, errors.New("systemd user services are unavailable")) {
		if !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("unsupported err=%v", err)
		}
	}
}
