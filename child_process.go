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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProcessChildRunner is the process-backed implementation of ChildExecutor.
// It launches exactly one child per Run call; it is not a scheduler.
type ProcessChildRunner struct {
	Application *Application
	Executor    *Executor
	Config      ChildProcessConfig
	mu          sync.Mutex
	running     bool
}

// ChildExecutor is the common one-child boundary used by in-process and
// process-backed runners.
type ChildExecutor interface {
	Run(context.Context, ChildTask) (ChildCompletionPacket, error)
}

func NewProcessChildRunner(app *Application, config ChildProcessConfig) (*ProcessChildRunner, error) {
	if app == nil || app.Provider == nil {
		return nil, errors.New("process child runner application provider is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ProcessChildRunner{Application: app, Executor: app.Executor, Config: config}, nil
}

func (runner *ProcessChildRunner) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return runner.RunObserved(ctx, task, nil)
}

func (runner *ProcessChildRunner) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (packet ChildCompletionPacket, runErr error) {
	packet = ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID}
	if runner == nil || runner.Application == nil || runner.Application.Provider == nil {
		return packet, errors.New("process child runner application provider is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if NetworkOffFromContext(ctx) {
		// The child's tools run in another process, which can't see this
		// session's /network off (G11): fail closed.
		return packet, fmt.Errorf("%w; process-isolated children can't honor it", ErrNetworkOff)
	}
	if err := runner.Config.Validate(); err != nil {
		return packet, err
	}
	if err := task.Validate(); err != nil {
		return packet, err
	}
	if err := task.Scope.Validate(); err != nil {
		return packet, fmt.Errorf("child scope: %w", err)
	}
	runner.mu.Lock()
	if runner.running {
		runner.mu.Unlock()
		return packet, errors.New("process child runner already has a child in progress")
	}
	runner.running = true
	runner.mu.Unlock()
	notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "started"})
	defer func() {
		runner.mu.Lock()
		runner.running = false
		runner.mu.Unlock()
		status := packet.Status
		if status == "" {
			status = ChildFailed
		}
		info := string(status)
		if runErr != nil {
			info = runErr.Error()
		}
		notifyChild(observer, ChildObservation{TaskID: task.TaskID, ParentID: task.ParentID, Type: "completed", Info: info, Completion: &packet})
	}()
	if err := runner.validateTask(task); err != nil {
		return packet, err
	}
	workingDir, err := runner.workingDir(task.Scope)
	if err != nil {
		return packet, err
	}

	maxOutput := task.Budget.maxOutput(task.Scope)
	if scopeLimit := task.Scope.maxOutput(); scopeLimit < maxOutput {
		maxOutput = scopeLimit
	}
	packet = ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID}
	transport, err := runner.startProcess(workingDir, task)
	if err != nil {
		return packet, err
	}
	transport.setEventHandler(func(event ChildEvent) {
		if event.TaskID != "" && event.TaskID != task.TaskID {
			transport.fail(fmt.Errorf("child event task id %q does not match %q", event.TaskID, task.TaskID))
			return
		}
		observation := event.Observation()
		if observation.TaskID == "" {
			observation.TaskID = task.TaskID
		}
		if observation.ParentID == "" {
			observation.ParentID = task.ParentID
		}
		notifyChild(observer, observation)
	})
	defer transport.close(ctx)

	runCtx := ctx
	cancelRun := func() {}
	childTimeout := task.Budget.timeout(task.Scope)
	if scopeTimeout := task.Scope.timeout(); scopeTimeout < childTimeout {
		childTimeout = scopeTimeout
	}
	if childTimeout > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, childTimeout)
	}
	defer cancelRun()
	transport.setRequestHandler(func(req childRPCRequest) {
		runner.handleChildRequest(transport, runCtx, task, req, maxOutput)
	})

	initCtx, cancelInit := context.WithTimeout(runCtx, runner.Config.startupTimeout())
	initResult, err := transport.request(initCtx, ChildMethodInitialize, ChildInitializeParams{
		Protocol: ChildProtocolVersion,
		Host:     "bee",
		Version:  "6B",
		Task:     task,
		Config:   childRuntimeConfig(runner.Application.ChildConfigOrMain(), runner.Application.RequirePlan),
	})
	cancelInit()
	if err != nil {
		status := ChildTimedOut
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			status = ChildMalformed
		}
		return runner.failurePacket(task, status, "child initialize failed: "+err.Error()), nil
	}
	var initialized ChildInitializeResult
	if err := decodeChildResult(initResult, &initialized); err != nil {
		return runner.failurePacket(task, ChildMalformed, err.Error()), nil
	}
	if err := initialized.Validate(task); err != nil {
		return runner.failurePacket(task, ChildMalformed, err.Error()), nil
	}

	taskID, err := transport.nextRequestID()
	if err != nil {
		return packet, err
	}
	taskWait, err := transport.register(taskID)
	if err != nil {
		return packet, err
	}
	if err := transport.writeRequest(childRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%q", taskID)), Method: ChildMethodTask}); err != nil {
		transport.remove(taskID)
		status := ChildFailed
		select {
		case <-transport.done:
			status = ChildMalformed
		default:
		}
		return runner.failurePacket(task, status, "send child task: "+err.Error()), nil
	}
	requestTimeout := runner.Config.requestTimeout()
	requestCtx, cancelRequest := contextWithOptionalTimeout(runCtx, requestTimeout)
	defer cancelRequest()
	var response childRPCResponse
	select {
	case response = <-taskWait:
	case <-requestCtx.Done():
		_ = transport.cancelRequest(taskID)
		status := ChildCancelled
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			status = ChildTimedOut
		}
		transport.terminate()
		return runner.failurePacket(task, status, requestCtx.Err().Error()), nil
	case <-transport.done:
		return runner.failurePacket(task, ChildMalformed, transport.failureString()), nil
	}
	if response.Error != nil {
		status := ChildFailed
		if response.Error.Code == ChildRPCInvalidParams || (response.Error.Code == ChildRPCInternalError && transport.failed()) {
			status = ChildMalformed
		}
		return runner.failurePacket(task, status, response.Error.Error()), nil
	}
	var completion ChildCompletionPacket
	if err := decodeChildResult(response, &completion); err != nil {
		return runner.failurePacket(task, ChildMalformed, err.Error()), nil
	}
	if completion.TaskID != task.TaskID || completion.ParentID != task.ParentID {
		return runner.failurePacket(task, ChildMalformed, "child completion identity does not match task"), nil
	}
	if err := completion.Validate(); err != nil {
		return runner.failurePacket(task, ChildMalformed, "validate child completion packet: "+err.Error()), nil
	}
	return completion, nil
}

func (runner *ProcessChildRunner) validateTask(task ChildTask) error {
	base := runner.Executor
	if base == nil {
		base = runner.Application.Executor
	}
	if len(task.AllowedTools) == 0 {
		return nil
	}
	if base == nil || base.Registry == nil {
		return errors.New("process child allowed tools require a parent executor")
	}
	if !processExecutorPolicySupported(base) {
		return errors.New("process child requires the built-in parent policy; custom policy serialization is deferred")
	}
	if base.Auditor != nil {
		return errors.New("process child requires no parent auditor; audit brokering is deferred")
	}
	if err := base.Scope.Validate(); err != nil {
		return fmt.Errorf("parent executor scope: %w", err)
	}
	if !scopeWithin(base.Scope.Root, task.Scope.Root) {
		return errors.New("child scope must remain within parent executor scope")
	}
	if task.Scope.timeout() > base.Scope.timeout() {
		return errors.New("child scope timeout must not exceed parent executor timeout")
	}
	if task.Scope.maxOutput() > base.Scope.maxOutput() {
		return errors.New("child scope output limit must not exceed parent executor output limit")
	}
	if !allowlistSubset(task.Scope.EnvAllowlist, base.Scope.EnvAllowlist) {
		return errors.New("child scope environment allowlist must remain within parent allowlist")
	}
	for _, name := range task.AllowedTools {
		if _, ok := base.Registry.Lookup(name); !ok {
			return fmt.Errorf("child tool %q is not registered in parent executor", name)
		}
		if !isBuiltInChildTool(name) {
			return fmt.Errorf("process child tool %q is unsupported; custom and extension tool brokers are deferred", name)
		}
	}
	return nil
}

func processExecutorPolicySupported(executor *Executor) bool {
	if executor == nil || executor.Policy == nil {
		return true
	}
	switch executor.Policy.(type) {
	case DefaultCodingPolicy, *DefaultCodingPolicy:
		return true
	default:
		return false
	}
}

func isBuiltInChildTool(name string) bool {
	switch name {
	case "read_file", "list_files", "find_files", "grep", "write_file", "edit_file", "run_command":
		return true
	default:
		return false
	}
}

func (runner *ProcessChildRunner) workingDir(scope ExecutionScope) (string, error) {
	dir := runner.Config.WorkingDir
	if dir == "" {
		dir = scope.Root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve child working directory: %w", err)
	}
	if !scopeWithin(scope.Root, abs) {
		return "", errors.New("child process working directory must remain within child scope")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat child working directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("child process working directory must be a directory")
	}
	return abs, nil
}

func childRuntimeConfig(config Config, requirePlan bool) ChildRuntimeConfig {
	provider := config.Provider
	provider.APIKeyFile = ""
	return ChildRuntimeConfig{Provider: provider, Generation: config.Generation, Agent: config.Agent, RequirePlan: requirePlan}
}

func (runner *ProcessChildRunner) startProcess(workingDir string, task ChildTask) (*childProcessTransport, error) {
	command := exec.Command(runner.Config.Command, runner.Config.Args...)
	command.Dir = workingDir
	setAllowlistedChildEnvironment(command, runner.Config.EnvAllowlist, false)
	if err := configureChildProcessGroup(command); err != nil {
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("child stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("child stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("child stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start child process: %w", err)
	}
	transport := newChildProcessTransport(command, stdin, runner.Config.maxFrameBytes(), runner.Config.maxMessageBytes())
	go transport.readLoop(stdout)
	go transport.readDiagnostics(stderr)
	go func() {
		err := command.Wait()
		transport.processDone <- err
		if err != nil {
			transport.fail(fmt.Errorf("child process exited: %w", err))
		} else {
			transport.fail(errors.New("child process exited"))
		}
	}()
	return transport, nil
}

func (runner *ProcessChildRunner) handleChildRequest(transport *childProcessTransport, parentCtx context.Context, task ChildTask, request childRPCRequest, maxOutput int) {
	id, err := childID(request.ID)
	if err != nil {
		transport.fail(err)
		return
	}
	requestCtx := parentCtx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	switch request.Method {
	case ChildMethodProvider:
		var params struct {
			Request Request `json:"request"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCInvalidParams, Message: err.Error()})
			return
		}
		var deltas []string
		var total int
		provider, providerErr := runner.Application.childProvider()
		if providerErr != nil {
			_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCServerError, Message: providerErr.Error()})
			return
		}
		var lease *LocalChildLease
		if runner.Application.ChildBroker != nil {
			lease, err = runner.Application.ChildBroker.Acquire(requestCtx, LocalChildPurposeChild, LocalChildPriorityNormal)
			if err != nil {
				_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCServerError, Message: err.Error()})
				return
			}
		}
		response, callErr := provider.Stream(requestCtx, params.Request, func(delta string) error {
			if total+len(delta) > maxOutput {
				return errChildOutputBudget
			}
			total += len(delta)
			deltas = append(deltas, delta)
			return nil
		})
		if lease != nil {
			lease.ReleaseError(callErr)
		}
		if callErr != nil {
			_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCServerError, Message: callErr.Error()})
			return
		}
		_ = transport.writeResponse(id, map[string]any{"response": response, "deltas": deltas}, nil)
	case ChildMethodApproval:
		var requestPayload ApprovalRequest
		if err := json.Unmarshal(request.Params, &requestPayload); err != nil {
			_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCInvalidParams, Message: err.Error()})
			return
		}
		decision := ApprovalDenied
		var approvalErr error
		allowed := false
		for _, name := range task.AllowedTools {
			if name == requestPayload.ToolName {
				allowed = true
				break
			}
		}
		if !allowed {
			approvalErr = fmt.Errorf("approval request for tool %q is outside child allowlist", requestPayload.ToolName)
		}
		base := runner.Executor
		if base == nil {
			base = runner.Application.Executor
		}
		if approvalErr == nil {
			if base == nil || base.Registry == nil {
				approvalErr = errors.New("parent executor is not configured")
			} else if tool, ok := base.Registry.Lookup(requestPayload.ToolName); !ok {
				approvalErr = fmt.Errorf("approval request tool %q is not registered in parent", requestPayload.ToolName)
			} else if tool.Definition().Risk != RiskHigh && tool.Definition().Risk != RiskCritical {
				approvalErr = fmt.Errorf("approval request tool %q is not approval-gated", requestPayload.ToolName)
			} else if base.Approval == nil {
				approvalErr = errors.New("parent approval hook is not configured")
			} else {
				decision, approvalErr = base.Approval.Approve(requestCtx, requestPayload)
			}
		}
		if approvalErr != nil {
			_ = transport.writeResponse(id, map[string]any{"decision": decision, "error": approvalErr.Error()}, nil)
			return
		}
		_ = transport.writeResponse(id, map[string]any{"decision": decision}, nil)
	default:
		_ = transport.writeResponse(id, nil, &childRPCError{Code: ChildRPCMethodNotFound, Message: "child request method not found"})
	}
}

func (runner *ProcessChildRunner) failurePacket(task ChildTask, status ChildStatus, summary string) ChildCompletionPacket {
	if strings.TrimSpace(summary) == "" {
		summary = "child process failed"
	}
	packet := ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: status, Summary: truncateText(summary, task.Budget.maxOutput(task.Scope))}
	if err := packet.Validate(); err != nil {
		packet.Status = ChildMalformed
		packet.Summary = "child process produced an invalid completion packet"
		packet.Blockers = []string{err.Error()}
	}
	return packet
}

func decodeChildResult(response childRPCResponse, target any) error {
	if response.Error != nil {
		return response.Error
	}
	if response.Result == nil {
		return errors.New("child response result is required")
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return fmt.Errorf("encode child response result: %w", err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode child response result: %w", err)
	}
	return nil
}

type childProcessTransport struct {
	command       *exec.Cmd
	stdin         io.WriteCloser
	maxFrame      int
	maxMessage    int
	writeMu       sync.Mutex
	pending       *childRPCPending
	nextID        atomic.Uint64
	done          chan struct{}
	doneOnce      sync.Once
	failureMu     sync.RWMutex
	failure       error
	requestMu     sync.RWMutex
	requestHandle func(childRPCRequest)
	eventMu       sync.RWMutex
	eventHandle   func(ChildEvent)
	processDone   chan error
	diagnosticsMu sync.Mutex
	diagnostics   bytes.Buffer
}

func newChildProcessTransport(command *exec.Cmd, stdin io.WriteCloser, maxFrame, maxMessage int) *childProcessTransport {
	return &childProcessTransport{command: command, stdin: stdin, maxFrame: maxFrame, maxMessage: maxMessage, pending: newChildRPCPending(), done: make(chan struct{}), processDone: make(chan error, 1)}
}

func (transport *childProcessTransport) setRequestHandler(handler func(childRPCRequest)) {
	transport.requestMu.Lock()
	transport.requestHandle = handler
	transport.requestMu.Unlock()
}

func (transport *childProcessTransport) setEventHandler(handler func(ChildEvent)) {
	transport.eventMu.Lock()
	transport.eventHandle = handler
	transport.eventMu.Unlock()
}

func (transport *childProcessTransport) nextRequestID() (string, error) {
	id := transport.nextID.Add(1)
	return fmt.Sprintf("%d", id), nil
}

func (transport *childProcessTransport) register(id string) (chan childRPCResponse, error) {
	return transport.pending.add(id)
}

func (transport *childProcessTransport) remove(id string) { _, _ = transport.pending.take(id) }

func (transport *childProcessTransport) request(ctx context.Context, method string, params any) (childRPCResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := transport.nextRequestID()
	if err != nil {
		return childRPCResponse{}, err
	}
	wait, err := transport.register(id)
	if err != nil {
		return childRPCResponse{}, err
	}
	if err := transport.writeRequest(childRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%q", id)), Method: method, Params: mustRawJSON(params)}); err != nil {
		transport.remove(id)
		return childRPCResponse{}, err
	}
	select {
	case response := <-wait:
		return response, nil
	case <-ctx.Done():
		transport.remove(id)
		return childRPCResponse{}, ctx.Err()
	case <-transport.done:
		transport.remove(id)
		return childRPCResponse{}, errors.New(transport.failureString())
	}
}

func (transport *childProcessTransport) writeRequest(request childRPCRequest) error {
	return transport.writeFrame(request)
}

func (transport *childProcessTransport) writeResponse(id string, result any, rpcErr *childRPCError) error {
	return transport.writeFrame(childRPCResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (transport *childProcessTransport) cancelRequest(id string) error {
	return transport.writeFrame(childRPCRequest{JSONRPC: "2.0", Method: ChildMethodCancelRequest, Params: mustRawJSON(map[string]string{"id": id})})
}

func (transport *childProcessTransport) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode child frame: %w", err)
	}
	if len(data) > transport.maxMessage {
		return fmt.Errorf("child frame exceeds max message size (%d bytes)", transport.maxMessage)
	}
	data = append(data, '\n')
	if len(data) > transport.maxFrame {
		return fmt.Errorf("child frame exceeds max frame size (%d bytes)", transport.maxFrame)
	}
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	if transport.stdin == nil {
		return errors.New("child stdin is not available")
	}
	if _, err := transport.stdin.Write(data); err != nil {
		transport.fail(fmt.Errorf("write child frame: %w", err))
		return err
	}
	return nil
}

func (transport *childProcessTransport) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		frame, err := readBoundedFrame(reader, transport.maxFrame, transport.maxMessage, "child")
		if err != nil {
			if !errors.Is(err, io.EOF) {
				transport.fail(fmt.Errorf("child protocol read: %w", err))
			} else {
				transport.fail(errors.New("child stdout closed"))
			}
			return
		}
		var envelope childRPCRequest
		if err := json.Unmarshal(frame, &envelope); err != nil {
			transport.fail(fmt.Errorf("child protocol malformed JSON: %w", err))
			return
		}
		if envelope.JSONRPC != "2.0" {
			transport.fail(errors.New("child protocol jsonrpc must be 2.0"))
			return
		}
		if envelope.Method != "" {
			if envelope.Method == ChildMethodEvent {
				var event ChildEvent
				if err := json.Unmarshal(envelope.Params, &event); err != nil {
					transport.fail(fmt.Errorf("child event malformed: %w", err))
					return
				}
				if strings.TrimSpace(event.Type) == "" {
					transport.fail(errors.New("child event type is required"))
					return
				}
				transport.eventMu.RLock()
				handler := transport.eventHandle
				transport.eventMu.RUnlock()
				if handler != nil {
					handler(event)
				}
				continue
			}
			if envelope.Method == ChildMethodCancelRequest {
				continue
			}
			transport.requestMu.RLock()
			handler := transport.requestHandle
			transport.requestMu.RUnlock()
			if handler == nil {
				transport.fail(errors.New("child request handler is not configured"))
				return
			}
			go handler(envelope)
			continue
		}
		var response childRPCResponse
		if err := json.Unmarshal(frame, &response); err != nil {
			transport.fail(fmt.Errorf("child response malformed: %w", err))
			return
		}
		if response.ID == "" {
			transport.fail(errors.New("child response id is required"))
			return
		}
		if response.Error != nil && response.Result != nil {
			transport.fail(errors.New("child response cannot contain both result and error"))
			return
		}
		wait, ok := transport.pending.take(response.ID)
		if !ok {
			transport.fail(fmt.Errorf("child response has unknown or duplicate id %q", response.ID))
			return
		}
		wait <- response
	}
}

func (transport *childProcessTransport) readDiagnostics(stderr io.Reader) {
	data, _ := io.ReadAll(io.LimitReader(stderr, int64(transport.maxMessage)+1))
	if len(data) > transport.maxMessage {
		data = data[:transport.maxMessage]
	}
	transport.diagnosticsMu.Lock()
	transport.diagnostics.Write(data)
	transport.diagnosticsMu.Unlock()
}

func (transport *childProcessTransport) fail(err error) {
	if transport == nil || err == nil {
		return
	}
	transport.failureMu.Lock()
	if transport.failure != nil {
		transport.failureMu.Unlock()
		return
	}
	transport.failure = err
	transport.failureMu.Unlock()
	transport.pending.failAll(childRPCResponse{Error: &childRPCError{Code: ChildRPCInternalError, Message: err.Error()}})
	transport.doneOnce.Do(func() { close(transport.done) })
}

func (transport *childProcessTransport) failureString() string {
	transport.failureMu.RLock()
	defer transport.failureMu.RUnlock()
	if transport.failure == nil {
		return "child process stopped"
	}
	return transport.failure.Error()
}

func (transport *childProcessTransport) failed() bool {
	if transport == nil {
		return false
	}
	transport.failureMu.RLock()
	defer transport.failureMu.RUnlock()
	return transport.failure != nil
}

func (transport *childProcessTransport) terminate() {
	if transport == nil || transport.command == nil {
		return
	}
	_ = killChildProcessGroup(transport.command)
}

func (transport *childProcessTransport) close(ctx context.Context) {
	if transport == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-transport.processDone:
	case <-time.After(300 * time.Millisecond):
		transport.terminate()
		select {
		case <-transport.processDone:
		case <-time.After(500 * time.Millisecond):
		}
	case <-ctx.Done():
		transport.terminate()
		select {
		case <-transport.processDone:
		case <-time.After(500 * time.Millisecond):
		}
	}
	if transport.stdin != nil {
		_ = transport.stdin.Close()
	}
}

func mustRawJSON(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	return data
}

func readBoundedFrame(reader *bufio.Reader, maxFrame, maxMessage int, label string) ([]byte, error) {
	frame, err := reader.ReadBytes('\n')
	if len(frame) > maxFrame {
		return nil, fmt.Errorf("%s frame exceeds max frame size (%d bytes)", label, maxFrame)
	}
	if len(frame) == 0 {
		return nil, err
	}
	frame = bytes.TrimSuffix(frame, []byte{'\n'})
	frame = bytes.TrimSuffix(frame, []byte{'\r'})
	if len(frame) > maxMessage {
		return nil, fmt.Errorf("%s message exceeds max message size (%d bytes)", label, maxMessage)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return frame, nil
}

// childBrokerProvider keeps provider credentials in the parent process. The
// child sees only normalized requests and receives responses over the broker.
type childBrokerProvider struct {
	server *childStdioServer
}

func (provider *childBrokerProvider) Complete(ctx context.Context, request Request) (Response, error) {
	response, err := provider.Stream(ctx, request, nil)
	return response, err
}

func (provider *childBrokerProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	response, err := provider.server.requestParent(ctx, ChildMethodProvider, map[string]any{"request": request})
	if err != nil {
		return Response{}, err
	}
	var result struct {
		Response Response `json:"response"`
		Deltas   []string `json:"deltas,omitempty"`
	}
	if err := decodeChildResult(response, &result); err != nil {
		return Response{}, err
	}
	for _, delta := range result.Deltas {
		if err := onDelta(delta); err != nil {
			return Response{}, err
		}
	}
	return result.Response, nil
}

// ServeChildStdio runs the `bee child --stdio` mode. It receives only one
// initialize/task sequence and exits after one completion response.
func ServeChildStdio(ctx context.Context, input io.Reader, output io.Writer) error {
	server := newChildStdioServer(ctx, input, output)
	return server.serve()
}

type childStdioServer struct {
	ctx           context.Context
	input         io.Reader
	output        io.Writer
	writeMu       sync.Mutex
	pending       *childRPCPending
	nextID        atomic.Uint64
	maxFrame      int
	maxMsg        int
	initialized   bool
	task          ChildTask
	config        Config
	requirePlan   bool
	cancel        context.CancelFunc
	taskDone      chan childTaskResult
	taskRequestID string
}

type childTaskResult struct {
	packet ChildCompletionPacket
	err    error
}

func newChildStdioServer(ctx context.Context, input io.Reader, output io.Writer) *childStdioServer {
	if ctx == nil {
		ctx = context.Background()
	}
	return &childStdioServer{ctx: ctx, input: input, output: output, pending: newChildRPCPending(), maxFrame: defaultChildMaxFrameBytes, maxMsg: defaultChildMaxMessageSize}
}

func (server *childStdioServer) serve() error {
	if server.input == nil || server.output == nil {
		return errors.New("child stdio input and output are required")
	}
	lines := make(chan []byte)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(server.input)
		scanner.Buffer(make([]byte, 4096), server.maxFrame)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-server.ctx.Done():
				return
			}
		}
		errs <- scanner.Err()
	}()
	for {
		select {
		case line := <-lines:
			if err := server.handleLine(line); err != nil {
				_ = server.writeResponse("", nil, &childRPCError{Code: ChildRPCInvalidRequest, Message: err.Error()})
				return err
			}
		case result := <-server.taskDone:
			return server.finishTask(result)
		case err := <-errs:
			if err != nil {
				return err
			}
			return nil
		case <-server.ctx.Done():
			if server.cancel != nil {
				server.cancel()
			}
			return nil
		}
	}
}

func (server *childStdioServer) handleLine(line []byte) error {
	var request childRPCRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return fmt.Errorf("child protocol malformed JSON: %w", err)
	}
	if request.JSONRPC != "2.0" {
		return errors.New("child request must use jsonrpc 2.0")
	}
	if strings.TrimSpace(request.Method) == "" {
		var response childRPCResponse
		if err := json.Unmarshal(line, &response); err != nil {
			return fmt.Errorf("child response malformed: %w", err)
		}
		return server.routeResponse(response)
	}
	if request.Method == ChildMethodCancelRequest {
		var params struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.ID == "" {
			return errors.New("child cancellation id is required")
		}
		if server.cancel != nil {
			server.cancel()
		}
		return nil
	}
	id, err := childID(request.ID)
	if err != nil {
		return err
	}
	switch request.Method {
	case ChildMethodInitialize:
		if server.initialized {
			return errors.New("child initialize was duplicated")
		}
		var params ChildInitializeParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return err
		}
		if params.Protocol != ChildProtocolVersion {
			return fmt.Errorf("unsupported child protocol %q", params.Protocol)
		}
		if err := params.Task.Validate(); err != nil {
			return err
		}
		config, err := params.Config.fullConfig()
		if err != nil {
			return err
		}
		server.task = params.Task
		server.config = config
		server.requirePlan = params.Config.RequirePlan
		server.maxFrame = defaultChildMaxFrameBytes
		server.maxMsg = defaultChildMaxMessageSize
		server.initialized = true
		return server.writeResponse(id, ChildInitializeResult{Protocol: ChildProtocolVersion, Version: "6B", TaskID: params.Task.TaskID, Features: []string{"provider_broker", "approval_broker", "single_task"}}, nil)
	case ChildMethodTask:
		if !server.initialized {
			return errors.New("child task received before initialize")
		}
		if server.taskDone != nil {
			return errors.New("child task was duplicated")
		}
		server.taskDone = make(chan childTaskResult, 1)
		server.taskRequestID = id
		server.emitEvent(ChildEvent{TaskID: server.task.TaskID, Type: "started"})
		childCtx, cancel := context.WithCancel(server.ctx)
		server.cancel = cancel
		go func() {
			registry, registryErr := NewCodingRegistry(server.task.Scope)
			if registryErr != nil {
				server.taskDone <- childTaskResult{err: registryErr}
				return
			}
			allowed, allowedErr := NewRegistry()
			if allowedErr != nil {
				server.taskDone <- childTaskResult{err: allowedErr}
				return
			}
			for _, name := range server.task.AllowedTools {
				tool, ok := registry.Lookup(name)
				if !ok {
					server.taskDone <- childTaskResult{err: fmt.Errorf("child tool %q is not available in process mode", name)}
					return
				}
				if err := allowed.Register(tool); err != nil {
					server.taskDone <- childTaskResult{err: err}
					return
				}
			}
			executor := &Executor{Registry: allowed, Scope: server.task.Scope, Policy: DefaultCodingPolicy{}, Approval: childApprovalBroker{server: server}}
			if server.requirePlan {
				executor.planGate = &PlanGate{}
			}
			broker := &childBrokerProvider{server: server}
			app := &Application{Config: server.config, Provider: broker, ChildProvider: broker, ChildConfig: server.config, Executor: executor, RequirePlan: server.requirePlan}
			runner, err := NewChildRunner(app)
			if err != nil {
				server.taskDone <- childTaskResult{err: err}
				return
			}
			observer := childObserverFunc(func(observation ChildObservation) {
				server.emitEvent(childEventFromObservation(observation))
			})
			packet, runErr := runner.RunObserved(childCtx, server.task, observer)
			server.taskDone <- childTaskResult{packet: packet, err: runErr}
		}()
		return nil
	case ChildMethodShutdown:
		return server.writeResponse(id, map[string]bool{"ok": true}, nil)
	default:
		return server.writeResponse(id, nil, &childRPCError{Code: ChildRPCMethodNotFound, Message: "child method not found"})
	}
}

func (server *childStdioServer) finishTask(result childTaskResult) error {
	packet := result.packet
	if result.err != nil {
		packet = ChildCompletionPacket{TaskID: server.task.TaskID, ParentID: server.task.ParentID, Status: ChildFailed, Summary: result.err.Error()}
	}
	if err := packet.Validate(); err != nil {
		packet = ChildCompletionPacket{TaskID: server.task.TaskID, ParentID: server.task.ParentID, Status: ChildMalformed, Summary: "child produced an invalid completion packet", Blockers: []string{err.Error()}}
	}
	// This is the sole completion packet on the child wire. The event above is
	// metadata only and never carries an executable instruction.
	return server.writeResponse(server.taskRequestID, packet, nil)
}

func (server *childStdioServer) requestParent(ctx context.Context, method string, params any) (childRPCResponse, error) {
	id := fmt.Sprintf("p-%d", server.nextID.Add(1))
	wait, err := server.pending.add(id)
	if err != nil {
		return childRPCResponse{}, err
	}
	if err := server.writeRequest(childRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%q", id)), Method: method, Params: mustRawJSON(params)}); err != nil {
		server.pending.take(id)
		return childRPCResponse{}, err
	}
	select {
	case response := <-wait:
		return response, nil
	case <-ctx.Done():
		server.pending.take(id)
		return childRPCResponse{}, ctx.Err()
	case <-server.ctx.Done():
		server.pending.take(id)
		return childRPCResponse{}, server.ctx.Err()
	}
}

func (server *childStdioServer) writeRequest(request childRPCRequest) error {
	return server.writeFrame(request)
}
func (server *childStdioServer) writeResponse(id string, result any, rpcErr *childRPCError) error {
	if id == "" {
		id = "0"
	}
	return server.writeFrame(childRPCResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}
func (server *childStdioServer) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > server.maxMsg {
		return fmt.Errorf("child message exceeds max message size (%d bytes)", server.maxMsg)
	}
	data = append(data, '\n')
	if len(data) > server.maxFrame {
		return fmt.Errorf("child frame exceeds max frame size (%d bytes)", server.maxFrame)
	}
	server.writeMu.Lock()
	defer server.writeMu.Unlock()
	_, err = server.output.Write(data)
	return err
}
func (server *childStdioServer) emitEvent(event ChildEvent) {
	_ = server.writeFrame(childRPCRequest{JSONRPC: "2.0", Method: ChildMethodEvent, Params: mustRawJSON(event)})
}

func (server *childStdioServer) routeResponse(response childRPCResponse) error {
	if response.ID == "" {
		return errors.New("child response id is required")
	}
	if response.Error != nil && response.Result != nil {
		return errors.New("child response cannot contain both result and error")
	}
	wait, ok := server.pending.take(response.ID)
	if !ok {
		return fmt.Errorf("child response has unknown or duplicate id %q", response.ID)
	}
	wait <- response
	return nil
}

type childApprovalBroker struct{ server *childStdioServer }

func (approval childApprovalBroker) Approve(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	response, err := approval.server.requestParent(ctx, ChildMethodApproval, request)
	if err != nil {
		return ApprovalDenied, err
	}
	var result struct {
		Decision ApprovalDecision `json:"decision"`
		Error    string           `json:"error,omitempty"`
	}
	if err := decodeChildResult(response, &result); err != nil {
		return ApprovalDenied, err
	}
	if result.Error != "" {
		return result.Decision, errors.New(result.Error)
	}
	if result.Decision != ApprovalApproved && result.Decision != ApprovalDenied {
		return ApprovalDenied, errors.New("parent returned invalid approval decision")
	}
	return result.Decision, nil
}
