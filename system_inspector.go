package bee

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	MaxProcessListResults  = 64
	MaxInspectorOutput     = 24 << 10
	MaxServiceLogLines     = 100
	MaxServiceLogLineBytes = 2048
	MaxSystemCommandOutput = 128 << 10
	MaxServiceSinceMinutes = 1440
)

type ProcessSummary struct {
	PID          int    `json:"pid"`
	PPID         int    `json:"ppid,omitempty"`
	UID          int    `json:"uid"`
	Comm         string `json:"comm"`
	State        string `json:"state,omitempty"`
	Threads      int    `json:"threads,omitempty"`
	RSSBytes     uint64 `json:"rss_bytes,omitempty"`
	VirtualBytes uint64 `json:"virtual_bytes,omitempty"`
	Cgroup       string `json:"cgroup,omitempty"`
}

type ProcessListRequest struct {
	Limit int
	Comm  string
}
type ProcessInfoRequest struct{ PID int }
type ServiceSummary struct {
	Unit        string `json:"unit"`
	LoadState   string `json:"load_state,omitempty"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`
	Description string `json:"description,omitempty"`
}
type ServiceStatus struct {
	Unit       string            `json:"unit"`
	Properties map[string]string `json:"properties"`
}
type ServiceLogEntry struct {
	Timestamp  string `json:"timestamp,omitempty"`
	Priority   string `json:"priority,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	PID        string `json:"pid,omitempty"`
	Message    string `json:"message,omitempty"`
}
type ServiceLogsRequest struct {
	Unit         string
	Lines        int
	SinceMinutes int
}

type SystemInspector interface {
	ProcessList(context.Context, ProcessListRequest) ([]ProcessSummary, error)
	ProcessInfo(context.Context, ProcessInfoRequest) (ProcessSummary, error)
	ServiceList(context.Context) ([]ServiceSummary, error)
	ServiceStatus(context.Context, string) (ServiceStatus, error)
	ServiceLogs(context.Context, ServiceLogsRequest) ([]ServiceLogEntry, error)
}

type SystemCommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

var ErrSystemCommandOutputTruncated = errors.New("system command output exceeded the safety limit")

type systemCommandOutputBudget struct {
	remaining int
	truncated bool
}

type systemBoundedCommandBuffer struct {
	data   bytes.Buffer
	budget *systemCommandOutputBudget
}

func (buffer *systemBoundedCommandBuffer) Write(data []byte) (int, error) {
	if buffer.budget == nil || buffer.budget.remaining <= 0 {
		if buffer.budget != nil {
			buffer.budget.truncated = true
		}
		return len(data), nil
	}
	if len(data) > buffer.budget.remaining {
		_, _ = buffer.data.Write(data[:buffer.budget.remaining])
		buffer.budget.remaining = 0
		buffer.budget.truncated = true
		return len(data), nil
	}
	buffer.budget.remaining -= len(data)
	return buffer.data.Write(data)
}

type execSystemCommandRunner struct{}

const trustedSystemPath = "/usr/bin:/bin"

func trustedSystemCommandPath(name string) string {
	for _, root := range []string{"/usr/bin", "/bin"} {
		path := filepath.Join(root, name)
		if err := validateTrustedExecutable(path); err == nil {
			return path
		}
	}
	return ""
}

func validateTrustedExecutable(path string) error {
	clean := filepath.Clean(path)
	if clean != filepath.Join("/usr/bin", filepath.Base(clean)) && clean != filepath.Join("/bin", filepath.Base(clean)) {
		return errors.New("system command is outside trusted roots")
	}
	if err := rejectBeeSymlinkAncestors(clean); err != nil {
		return err
	}
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("system command path is not root-owned")
		}
		if info.Mode()&0o022 != 0 {
			return errors.New("system command path is user-writable")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("system command is not a trusted executable")
	}
	return nil
}

func systemdCommandEnv() []string {
	env := make([]string, 0, 8)
	env = append(env, "PATH="+trustedSystemPath)
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0", "SYSTEMD_LESS=", "LC_ALL=C", "LANG=C", "LANGUAGE=C", "TERM=dumb")
	return env
}

func (execSystemCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = systemdCommandEnv()
	budget := &systemCommandOutputBudget{remaining: MaxSystemCommandOutput}
	stdout := &systemBoundedCommandBuffer{budget: budget}
	stderr := &systemBoundedCommandBuffer{budget: budget}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if budget.truncated {
			return nil, ErrSystemCommandOutputTruncated
		}
		return nil, errors.New("system command failed")
	}
	if budget.truncated {
		return nil, ErrSystemCommandOutputTruncated
	}
	return stdout.data.Bytes(), nil
}

var _ io.Writer = (*systemBoundedCommandBuffer)(nil)

type LinuxSystemInspector struct {
	ProcRoot       string
	UID            int
	Runner         SystemCommandRunner
	Systemd        bool
	SystemctlPath  string
	JournalctlPath string
}

func (inspector *LinuxSystemInspector) ServiceInspectionAvailable() bool {
	return inspector != nil && inspector.Systemd
}

func NewLinuxSystemInspector() (*LinuxSystemInspector, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("system inspector is Linux-only")
	}
	uid := os.Getuid()
	root := "/proc"
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("proc unavailable: %w", err)
	}
	systemctlPath := trustedSystemCommandPath("systemctl")
	journalctlPath := trustedSystemCommandPath("journalctl")
	return &LinuxSystemInspector{ProcRoot: root, UID: uid, Runner: execSystemCommandRunner{}, Systemd: systemctlPath != "" && journalctlPath != "", SystemctlPath: systemctlPath, JournalctlPath: journalctlPath}, nil
}
func (inspector *LinuxSystemInspector) procRoot() string {
	if strings.TrimSpace(inspector.ProcRoot) == "" {
		return "/proc"
	}
	return inspector.ProcRoot
}
func (inspector *LinuxSystemInspector) ownUID() int {
	if inspector.UID < 0 {
		return os.Getuid()
	}
	return inspector.UID
}
func (inspector *LinuxSystemInspector) ProcessList(ctx context.Context, req ProcessListRequest) ([]ProcessSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Limit < 0 || req.Limit > MaxProcessListResults {
		return nil, errors.New("process list limit is out of bounds")
	}
	limit := req.Limit
	if limit == 0 {
		limit = MaxProcessListResults
	}
	entries, err := os.ReadDir(inspector.procRoot())
	if err != nil {
		return nil, fmt.Errorf("read proc: %w", err)
	}
	pids := make([]int, 0, limit)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	out := make([]ProcessSummary, 0, limit)
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) >= limit {
			break
		}
		summary, e := inspector.readProcess(ctx, pid)
		if e != nil {
			continue
		}
		if req.Comm != "" && !strings.Contains(strings.ToLower(summary.Comm), strings.ToLower(req.Comm)) {
			continue
		}
		out = append(out, summary)
	}
	return out, nil
}
func (inspector *LinuxSystemInspector) ProcessInfo(ctx context.Context, req ProcessInfoRequest) (ProcessSummary, error) {
	if req.PID <= 0 || req.PID > 4194304 {
		return ProcessSummary{}, errors.New("pid is invalid")
	}
	return inspector.readProcess(ctx, req.PID)
}
func readPrivateInspectorFile(path string, maxBytes int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("inspector file is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, errors.New("inspector file exceeds bounds")
	}
	return data, nil
}

func (inspector *LinuxSystemInspector) readProcess(ctx context.Context, pid int) (ProcessSummary, error) {
	if err := ctx.Err(); err != nil {
		return ProcessSummary{}, err
	}
	dir := filepath.Join(inspector.procRoot(), strconv.Itoa(pid))
	if err := rejectBeeSymlinkAncestors(dir); err != nil {
		return ProcessSummary{}, err
	}
	data, err := readPrivateInspectorFile(filepath.Join(dir, "status"), 16<<10)
	if err != nil {
		return ProcessSummary{}, err
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := strings.Cut(line, ":")
		if ok {
			fields[key] = strings.TrimSpace(val)
		}
	}
	uid, err := parseFirstInt(fields["Uid"])
	if err != nil || uid != inspector.ownUID() {
		return ProcessSummary{}, errors.New("process is not owned by current user")
	}
	pidField, pidErr := parseFirstInt(fields["Pid"])
	if pidErr != nil || pidField != pid {
		return ProcessSummary{}, errors.New("proc status pid does not match requested pid")
	}
	summary := ProcessSummary{PID: pidField, UID: uid, Comm: truncateUTF8(fields["Name"], 128), State: truncateUTF8(fields["State"], 64)}
	summary.PPID, _ = parseFirstInt(fields["PPid"])
	summary.Threads, _ = parseFirstInt(fields["Threads"])
	summary.RSSBytes = parseMemoryKB(fields["VmRSS"])
	summary.VirtualBytes = parseMemoryKB(fields["VmSize"])
	summary.Cgroup = readCgroupSummary(filepath.Join(dir, "cgroup"))
	return summary, nil
}
func parseFirstInt(value string) (int, error) {
	for _, part := range strings.Fields(value) {
		if n, e := strconv.Atoi(part); e == nil {
			return n, nil
		}
	}
	return 0, errors.New("integer missing")
}
func parseMemoryKB(value string) uint64 {
	n, e := parseFirstInt(value)
	if e != nil || n < 0 {
		return 0
	}
	return uint64(n) * 1024
}
func readCgroupSummary(path string) string {
	data, err := readPrivateInspectorFile(path, 4096)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	return truncateUTF8(line, 256)
}

func validateServiceUnit(unit string) error {
	if unit == "" || len(unit) > 128 || !strings.HasSuffix(unit, ".service") || strings.HasPrefix(unit, "-") || strings.ContainsAny(unit, "/\\*?[]{}\x00\r\n\t ") {
		return errors.New("service unit must be a literal .service name")
	}
	for _, r := range unit {
		if !(r == 'a' || r == 'b' || r == 'c' || r == 'd' || r == 'e' || r == 'f' || r == 'g' || r == 'h' || r == 'i' || r == 'j' || r == 'k' || r == 'l' || r == 'm' || r == 'n' || r == 'o' || r == 'p' || r == 'q' || r == 'r' || r == 's' || r == 't' || r == 'u' || r == 'v' || r == 'w' || r == 'x' || r == 'y' || r == 'z' || r == 'A' || r == 'B' || r == 'C' || r == 'D' || r == 'E' || r == 'F' || r == 'G' || r == 'H' || r == 'I' || r == 'J' || r == 'K' || r == 'L' || r == 'M' || r == 'N' || r == 'O' || r == 'P' || r == 'Q' || r == 'R' || r == 'S' || r == 'T' || r == 'U' || r == 'V' || r == 'W' || r == 'X' || r == 'Y' || r == 'Z' || r == '0' || r == '1' || r == '2' || r == '3' || r == '4' || r == '5' || r == '6' || r == '7' || r == '8' || r == '9' || r == '-' || r == '_' || r == '.' || r == '@') {
			return errors.New("service unit contains unsafe characters")
		}
	}
	return nil
}
func boundedInspectorCommandOutput(out []byte) ([]byte, error) {
	if len(out) > MaxSystemCommandOutput {
		return nil, ErrSystemCommandOutputTruncated
	}
	return out, nil
}

func (inspector *LinuxSystemInspector) command(ctx context.Context, args ...string) ([]byte, error) {
	if !inspector.Systemd {
		return nil, errors.New("systemd user services are unavailable")
	}
	runner := inspector.Runner
	if runner == nil {
		runner = execSystemCommandRunner{}
	}
	name := inspector.SystemctlPath
	if name == "" {
		if isNativeSystemCommandRunner(runner) {
			return nil, errors.New("trusted systemctl executable is unavailable")
		}
		name = "systemctl"
	}
	out, err := runner.Run(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("systemctl: %w", err)
	}
	return boundedInspectorCommandOutput(out)
}
func (inspector *LinuxSystemInspector) ServiceList(ctx context.Context) ([]ServiceSummary, error) {
	out, err := inspector.command(ctx, "--user", "--no-pager", "--no-legend", "--plain", "list-units", "--type=service", "--all")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	services := make([]ServiceSummary, 0, MaxProcessListResults)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if validateServiceUnit(fields[0]) != nil {
			continue
		}
		services = append(services, ServiceSummary{Unit: fields[0], LoadState: fields[1], ActiveState: fields[2], SubState: fields[3], Description: truncateUTF8(strings.Join(fields[4:], " "), 256)})
		if len(services) >= MaxProcessListResults {
			break
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Unit < services[j].Unit })
	return services, nil
}

var allowedServiceProperties = []string{"Id", "Description", "LoadState", "ActiveState", "SubState", "MainPID", "ExecMainStartTimestamp", "MemoryCurrent", "TasksCurrent", "ControlGroup", "NRestarts", "ExecMainStatus", "ExecMainCode", "ActiveEnterTimestamp", "CPUUsageNSec"}

func (inspector *LinuxSystemInspector) ServiceStatus(ctx context.Context, unit string) (ServiceStatus, error) {
	if err := validateServiceUnit(unit); err != nil {
		return ServiceStatus{}, err
	}
	args := []string{"--user", "--no-pager", "--no-legend", "show", unit}
	for _, p := range allowedServiceProperties {
		args = append(args, "--property="+p)
	}
	out, err := inspector.command(ctx, args...)
	if err != nil {
		return ServiceStatus{}, err
	}
	allowed := map[string]bool{}
	for _, p := range allowedServiceProperties {
		allowed[p] = true
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] {
			continue
		}
		if looksLikeSecret(val) {
			val = "[REDACTED]"
		}
		props[key] = truncateUTF8(val, 512)
	}
	return ServiceStatus{Unit: unit, Properties: props}, nil
}
func (inspector *LinuxSystemInspector) ServiceLogs(ctx context.Context, req ServiceLogsRequest) ([]ServiceLogEntry, error) {
	if err := validateServiceUnit(req.Unit); err != nil {
		return nil, err
	}
	if req.Lines < 0 || req.Lines > MaxServiceLogLines {
		return nil, errors.New("service log line count is out of bounds")
	}
	if req.SinceMinutes < 0 || req.SinceMinutes > MaxServiceSinceMinutes {
		return nil, errors.New("service log since_minutes is out of bounds")
	}
	lines := req.Lines
	if lines == 0 {
		lines = MaxServiceLogLines
	}
	args := []string{"--user", "--no-pager", "--no-hostname", "-u", req.Unit, "-n", strconv.Itoa(lines), "--output=json"}
	if req.SinceMinutes > 0 {
		args = append(args, "--since", fmt.Sprintf("%d minutes ago", req.SinceMinutes))
	}
	runner := inspector.Runner
	if runner == nil {
		runner = execSystemCommandRunner{}
	}
	if !inspector.Systemd {
		return nil, errors.New("systemd user services are unavailable")
	}
	name := inspector.JournalctlPath
	if name == "" {
		if isNativeSystemCommandRunner(runner) {
			return nil, errors.New("trusted journalctl executable is unavailable")
		}
		name = "journalctl"
	}
	out, err := runner.Run(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	out, err = boundedInspectorCommandOutput(out)
	if err != nil {
		return nil, err
	}
	result := make([]ServiceLogEntry, 0, lines)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		entry := ServiceLogEntry{Timestamp: stringField(raw, "__REALTIME_TIMESTAMP"), Priority: stringField(raw, "PRIORITY"), Identifier: stringField(raw, "SYSLOG_IDENTIFIER"), PID: stringField(raw, "_PID"), Message: truncateUTF8(stringField(raw, "MESSAGE"), MaxServiceLogLineBytes)}
		if looksLikeSecret(entry.Message) {
			entry.Message = "[REDACTED]"
		}
		result = append(result, entry)
		if len(result) >= lines {
			break
		}
	}
	return result, nil
}

func isNativeSystemCommandRunner(runner SystemCommandRunner) bool {
	switch runner.(type) {
	case execSystemCommandRunner, *execSystemCommandRunner:
		return true
	default:
		return false
	}
}
func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

// Model-facing bounded tools.
type processListArgs struct {
	Limit int    `json:"limit,omitempty"`
	Comm  string `json:"comm,omitempty"`
}
type processInfoArgs struct {
	PID int `json:"pid"`
}
type serviceUnitArgs struct {
	Unit string `json:"unit"`
}
type serviceLogsArgs struct {
	Unit         string `json:"unit"`
	Lines        int    `json:"lines,omitempty"`
	SinceMinutes int    `json:"since_minutes,omitempty"`
}

type processListResult struct {
	Items     []ProcessSummary `json:"items,omitempty"`
	Truncated bool             `json:"truncated"`
}
type serviceListResult struct {
	Items     []ServiceSummary `json:"items,omitempty"`
	Truncated bool             `json:"truncated"`
}
type serviceLogsResult struct {
	Entries   []ServiceLogEntry `json:"entries,omitempty"`
	Truncated bool              `json:"truncated"`
}

func boundedProcessListJSON(items []ProcessSummary) (string, error) {
	normalized := make([]ProcessSummary, len(items))
	for i, item := range items {
		item.Comm = truncateUTF8(item.Comm, 128)
		item.State = truncateUTF8(item.State, 64)
		item.Cgroup = truncateUTF8(item.Cgroup, 256)
		normalized[i] = item
	}
	for count := len(normalized); count >= 0; count-- {
		data, err := json.Marshal(processListResult{Items: normalized[:count], Truncated: count < len(normalized)})
		if err != nil {
			return "", err
		}
		if len(data) <= MaxInspectorOutput {
			return string(data), nil
		}
	}
	return "", errors.New("process list output cannot fit safety bound")
}

func boundedServiceListJSON(items []ServiceSummary) (string, error) {
	normalized := make([]ServiceSummary, len(items))
	for i, item := range items {
		item.Unit = truncateUTF8(item.Unit, 128)
		item.LoadState = truncateUTF8(item.LoadState, 64)
		item.ActiveState = truncateUTF8(item.ActiveState, 64)
		item.SubState = truncateUTF8(item.SubState, 64)
		item.Description = truncateUTF8(item.Description, 256)
		normalized[i] = item
	}
	for count := len(normalized); count >= 0; count-- {
		data, err := json.Marshal(serviceListResult{Items: normalized[:count], Truncated: count < len(normalized)})
		if err != nil {
			return "", err
		}
		if len(data) <= MaxInspectorOutput {
			return string(data), nil
		}
	}
	return "", errors.New("service list output cannot fit safety bound")
}

func boundedServiceLogsJSON(entries []ServiceLogEntry) (string, error) {
	normalized := make([]ServiceLogEntry, len(entries))
	for i, entry := range entries {
		entry.Timestamp = truncateUTF8(entry.Timestamp, 64)
		entry.Priority = truncateUTF8(entry.Priority, 32)
		entry.Identifier = truncateUTF8(entry.Identifier, 128)
		entry.PID = truncateUTF8(entry.PID, 32)
		entry.Message = truncateUTF8(entry.Message, MaxServiceLogLineBytes)
		normalized[i] = entry
	}
	for count := len(normalized); count >= 0; count-- {
		data, err := json.Marshal(serviceLogsResult{Entries: normalized[:count], Truncated: count < len(normalized)})
		if err != nil {
			return "", err
		}
		if len(data) <= MaxInspectorOutput {
			return string(data), nil
		}
	}
	return "", errors.New("service logs output cannot fit safety bound")
}

type ProcessListTool struct{ Inspector SystemInspector }

func (ProcessListTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "process_list", Description: "Read-only inspection of processes owned by the current user on Linux; no environment, command lines, credentials, or file contents.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"},"comm":{"type":"string"}},"additionalProperties":false}`)}
}
func (t ProcessListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Inspector == nil {
		return "", errors.New("process_list inspector is not configured")
	}
	var in processListArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("process_list arguments: %w", err)
	}
	if err := validateText("process filter", in.Comm, 128, false); err != nil {
		return "", err
	}
	out, err := t.Inspector.ProcessList(ctx, ProcessListRequest{Limit: in.Limit, Comm: in.Comm})
	if err != nil {
		return "", err
	}
	return boundedProcessListJSON(out)
}

type ProcessInfoTool struct{ Inspector SystemInspector }

func (ProcessInfoTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "process_info", Description: "Read-only inspection of one current-user Linux process by PID; never exposes environment, full command line, credentials, or open files.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"pid":{"type":"integer"}},"required":["pid"],"additionalProperties":false}`)}
}
func (t ProcessInfoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Inspector == nil {
		return "", errors.New("process_info inspector is not configured")
	}
	var in processInfoArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("process_info arguments: %w", err)
	}
	out, err := t.Inspector.ProcessInfo(ctx, ProcessInfoRequest{PID: in.PID})
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(out, MaxInspectorOutput)
}

type ServiceListTool struct{ Inspector SystemInspector }

func (ServiceListTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "service_list", Description: "Read-only listing of bounded user-level systemd .service units on Linux; no mutation or service control.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (t ServiceListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Inspector == nil {
		return "", errors.New("service_list inspector is not configured")
	}
	if err := parseArgs(args, &struct{}{}); err != nil {
		return "", fmt.Errorf("service_list arguments: %w", err)
	}
	out, err := t.Inspector.ServiceList(ctx)
	if err != nil {
		return "", err
	}
	return boundedServiceListJSON(out)
}

type ServiceStatusTool struct{ Inspector SystemInspector }

func (ServiceStatusTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "service_status", Description: "Read-only status properties for one literal current-user systemd .service unit on Linux.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"}},"required":["unit"],"additionalProperties":false}`)}
}
func (t ServiceStatusTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Inspector == nil {
		return "", errors.New("service_status inspector is not configured")
	}
	var in serviceUnitArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("service_status arguments: %w", err)
	}
	out, err := t.Inspector.ServiceStatus(ctx, in.Unit)
	if err != nil {
		return "", err
	}
	return marshalBoundedJSON(out, MaxInspectorOutput)
}

type ServiceLogsTool struct{ Inspector SystemInspector }

func (ServiceLogsTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "service_logs", Description: "Read-only bounded logs for one literal current-user systemd .service unit on Linux; messages are redacted when secret-like.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string"},"lines":{"type":"integer","minimum":0,"maximum":100},"since_minutes":{"type":"integer","minimum":1,"maximum":1440}},"required":["unit"],"additionalProperties":false}`)}
}
func (t ServiceLogsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Inspector == nil {
		return "", errors.New("service_logs inspector is not configured")
	}
	var in serviceLogsArgs
	if err := parseArgs(args, &in); err != nil {
		return "", fmt.Errorf("service_logs arguments: %w", err)
	}
	out, err := t.Inspector.ServiceLogs(ctx, ServiceLogsRequest{Unit: in.Unit, Lines: in.Lines, SinceMinutes: in.SinceMinutes})
	if err != nil {
		return "", err
	}
	return boundedServiceLogsJSON(out)
}
