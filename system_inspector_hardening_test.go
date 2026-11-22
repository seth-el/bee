package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeProc(t *testing.T, root string, pid int, uid int, name string, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name: %s\nPid: %d\nPPid: 1\nUid: %d\nThreads: 2\nVmRSS: 4 kB\nVmSize: 8 kB\nState: S (sleeping)\n", name, pid, uid)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFakeProcOwnershipFilteringOrderingAndSafeFiles(t *testing.T) {
	root := t.TempDir()
	uid := os.Getuid()
	writeFakeProc(t, root, 20, uid, "zeta", strings.Repeat("cgroup/", 100))
	writeFakeProc(t, root, 10, uid, "alpha", "0::/user.slice\n")
	writeFakeProc(t, root, 30, uid+1, "foreign", "0::/foreign\n")
	if err := os.Mkdir(filepath.Join(root, "40"), 0o700); err != nil {
		t.Fatal(err)
	}
	inspector := &LinuxSystemInspector{ProcRoot: root, UID: uid}
	items, err := inspector.ProcessList(context.Background(), ProcessListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].PID != 10 || items[1].PID != 20 {
		t.Fatalf("items=%#v", items)
	}
	if len(items[1].Cgroup) > 256 {
		t.Fatalf("cgroup length=%d", len(items[1].Cgroup))
	}
	if _, err := inspector.ProcessInfo(context.Background(), ProcessInfoRequest{PID: 30}); err == nil {
		t.Fatal("foreign process accepted")
	}
	if _, err := inspector.ProcessInfo(context.Background(), ProcessInfoRequest{PID: 40}); err == nil {
		t.Fatal("malformed process accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "10", "status"), []byte("Name: alpha\nPid: 999\nUid: "+fmt.Sprint(uid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.ProcessInfo(context.Background(), ProcessInfoRequest{PID: 10}); err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("pid mismatch err=%v", err)
	}
	if _, err := inspector.ProcessList(context.Background(), ProcessListRequest{Limit: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspector.ProcessList(ctx, ProcessListRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "10", "status")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "10", "status")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.ProcessInfo(context.Background(), ProcessInfoRequest{PID: 10}); err == nil {
		t.Fatal("symlink status accepted")
	}
}

func TestProcessFilterIsBoundedAndRejectsSecretLikeInput(t *testing.T) {
	inspector := hugeInspector{}
	tool := ProcessListTool{Inspector: inspector}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"comm":"`+strings.Repeat("x", 129)+`"}`)); err == nil {
		t.Fatal("oversized process filter accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"comm":"api_key=secret"}`)); err == nil {
		t.Fatal("secret-like process filter accepted")
	}
}

type hugeInspector struct{}

func (hugeInspector) ProcessList(context.Context, ProcessListRequest) ([]ProcessSummary, error) {
	items := make([]ProcessSummary, 100)
	for i := range items {
		items[i] = ProcessSummary{PID: i + 1, Comm: strings.Repeat("x", 10000), Cgroup: strings.Repeat("y", 10000)}
	}
	return items, nil
}
func (hugeInspector) ProcessInfo(context.Context, ProcessInfoRequest) (ProcessSummary, error) {
	return ProcessSummary{PID: 1}, nil
}
func (hugeInspector) ServiceList(context.Context) ([]ServiceSummary, error) {
	items := make([]ServiceSummary, 100)
	for i := range items {
		items[i] = ServiceSummary{Unit: fmt.Sprintf("x-%d.service", i), Description: strings.Repeat("d", 10000)}
	}
	return items, nil
}
func (hugeInspector) ServiceStatus(context.Context, string) (ServiceStatus, error) {
	return ServiceStatus{Properties: map[string]string{"Description": strings.Repeat("d", 10000)}}, nil
}
func (hugeInspector) ServiceLogs(context.Context, ServiceLogsRequest) ([]ServiceLogEntry, error) {
	items := make([]ServiceLogEntry, 100)
	for i := range items {
		items[i] = ServiceLogEntry{PID: fmt.Sprint(i), Message: strings.Repeat("m", 10000)}
	}
	return items, nil
}
func (hugeInspector) ServiceInspectionAvailable() bool { return true }

func TestInspectorToolOutputsAreAlwaysBoundedJSON(t *testing.T) {
	inspector := hugeInspector{}
	for name, tool := range map[string]Tool{"process": ProcessListTool{Inspector: inspector}, "services": ServiceListTool{Inspector: inspector}, "logs": ServiceLogsTool{Inspector: inspector}} {
		args := `{"unit":"x.service"}`
		if name == "process" || name == "services" {
			args = `{}`
		}
		output, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s err=%v", name, err)
		}
		if len(output) > MaxInspectorOutput {
			t.Fatalf("%s output=%d", name, len(output))
		}
		var value any
		if err := json.Unmarshal([]byte(output), &value); err != nil {
			t.Fatalf("%s invalid json: %v", name, err)
		}
	}
}

func TestSystemdExactArgvPropertiesTemplateAndSinceMinutes(t *testing.T) {
	stub := &inspectorCommandStub{output: map[string][]byte{"systemctl": []byte("Id=bee@1.service\nNRestarts=2\nExecMainStatus=0\nIgnored=secret\n"), "journalctl": []byte(`{"MESSAGE":"ok"}` + "\n")}}
	inspector := &LinuxSystemInspector{Systemd: true, Runner: stub, UID: -1}
	if _, err := inspector.ServiceStatus(context.Background(), "bee@1.service"); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "bee@1.service", Lines: 2, SinceMinutes: 15}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stub.calls[0], " "); !strings.Contains(got, "--property=NRestarts") || strings.Contains(got, "Ignored") {
		t.Fatalf("status argv=%#v", stub.calls[0])
	}
	last := stub.calls[len(stub.calls)-1]
	if strings.Join(last, " ") != "journalctl --user --no-pager --no-hostname -u bee@1.service -n 2 --output=json --since 15 minutes ago" {
		t.Fatalf("logs argv=%#v", last)
	}
	if _, err := inspector.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "bee.service", Lines: -1}); err == nil {
		t.Fatal("negative lines accepted")
	}
	if _, err := inspector.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "bee.service", SinceMinutes: 1441}); err == nil {
		t.Fatal("large since_minutes accepted")
	}
	trustedStub := &inspectorCommandStub{output: map[string][]byte{"systemctl": []byte("x.service loaded active running x\n"), "journalctl": []byte(`{"MESSAGE":"ok"}` + "\n")}}
	trusted := &LinuxSystemInspector{Systemd: true, Runner: trustedStub, SystemctlPath: "/usr/bin/systemctl", JournalctlPath: "/usr/bin/journalctl"}
	if _, err := trusted.ServiceList(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := trustedStub.calls[0][0]; got != "/usr/bin/systemctl" {
		t.Fatalf("systemctl argv path=%q", got)
	}
	if _, err := trusted.ServiceLogs(context.Background(), ServiceLogsRequest{Unit: "x.service", Lines: 1}); err != nil {
		t.Fatal(err)
	}
	if got := trustedStub.calls[1][0]; got != "/usr/bin/journalctl" {
		t.Fatalf("journalctl argv path=%q", got)
	}
}

func TestBoundedSystemCommandRunnerEnvTruncationAndCancellation(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/dbus")
	t.Setenv("BEE_SECRET", "must-not-cross-boundary")
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := execSystemCommandRunner{}
	output, err := runner.Run(context.Background(), os.Args[0], "-test.run=TestSystemInspectorHelperEnv/child")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "XDG_RUNTIME_DIR=/run/user/test") || strings.Contains(string(output), "BEE_SECRET") || strings.Contains(string(output), "must-not-cross-boundary") {
		t.Fatalf("env=%q", output)
	}
	if _, err := runner.Run(context.Background(), os.Args[0], "-test.run=TestSystemInspectorHelperLarge/child"); !errors.Is(err, ErrSystemCommandOutputTruncated) {
		t.Fatalf("truncation err=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := runner.Run(ctx, os.Args[0], "-test.run=TestSystemInspectorHelperSleep/child"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel err=%v", err)
	}
}

func TestTrustedSystemCommandsIgnoreMaliciousPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+"/usr/bin:/bin")
	if got := systemdCommandEnv()[0]; got != "PATH="+trustedSystemPath {
		t.Fatalf("command env path=%q", got)
	}
	inspector, err := NewLinuxSystemInspector()
	if err != nil {
		t.Fatal(err)
	}
	if inspector.SystemctlPath != "" && !strings.HasPrefix(inspector.SystemctlPath, "/usr/bin/") && !strings.HasPrefix(inspector.SystemctlPath, "/bin/") {
		t.Fatalf("untrusted systemctl path=%q", inspector.SystemctlPath)
	}
	if inspector.JournalctlPath != "" && !strings.HasPrefix(inspector.JournalctlPath, "/usr/bin/") && !strings.HasPrefix(inspector.JournalctlPath, "/bin/") {
		t.Fatalf("untrusted journalctl path=%q", inspector.JournalctlPath)
	}
}

func TestSystemInspectorHelperEnv(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "/child") {
		return
	}
	for _, key := range []string{"PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "SYSTEMD_PAGER", "SYSTEMD_COLORS", "LC_ALL", "LANG", "LANGUAGE", "TERM", "BEE_SECRET"} {
		if value, ok := os.LookupEnv(key); ok {
			fmt.Printf("%s=%s\n", key, value)
		}
	}
}
func TestSystemInspectorHelperLarge(t *testing.T) {
	if strings.Contains(strings.Join(os.Args, " "), "/child") {
		fmt.Print(strings.Repeat("x", MaxSystemCommandOutput+1))
	}
}
func TestSystemInspectorHelperSleep(t *testing.T) {
	if strings.Contains(strings.Join(os.Args, " "), "/child") {
		time.Sleep(time.Second)
	}
}
