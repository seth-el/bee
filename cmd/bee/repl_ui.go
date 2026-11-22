package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/seth-el/bee"
)

type replStyle struct {
	userPrompt     string
	assistantLabel string
}

type replApprovalPrompt struct {
	request  bee.ApprovalRequest
	decision chan bee.ApprovalDecision
}

type replApprovalBroker struct {
	requests chan *replApprovalPrompt
}

func newREPLApprovalBroker() *replApprovalBroker {
	return &replApprovalBroker{requests: make(chan *replApprovalPrompt)}
}

func (broker *replApprovalBroker) Approve(ctx context.Context, request bee.ApprovalRequest) (bee.ApprovalDecision, error) {
	prompt := &replApprovalPrompt{request: request, decision: make(chan bee.ApprovalDecision, 1)}
	select {
	case broker.requests <- prompt:
	case <-ctx.Done():
		return bee.ApprovalDenied, ctx.Err()
	}
	select {
	case decision := <-prompt.decision:
		return decision, nil
	case <-ctx.Done():
		return bee.ApprovalDenied, ctx.Err()
	}
}

func newREPLStyle(output io.Writer) replStyle {
	if writerIsTerminal(output) {
		return replStyle{
			userPrompt:     "\x1b[36myou>\x1b[0m ",
			assistantLabel: "\x1b[32mbee>\x1b[0m ",
		}
	}
	return replStyle{userPrompt: "you> ", assistantLabel: "bee> "}
}

// replResponseWriter keeps session metadata readable while adding the human
// response label only when assistant text actually begins. This keeps the
// provider-neutral print renderer unchanged for one-shot, JSON, RPC, and TUI
// callers.
type replResponseWriter struct {
	output          io.Writer
	style           replStyle
	announceSession bool
	sessionHandled  bool
	started         bool
	writeMu         *sync.Mutex
}

func (writer *replResponseWriter) write(data []byte) (int, error) {
	if writer.writeMu == nil {
		return writer.output.Write(data)
	}
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	return writer.output.Write(data)
}

func (writer *replResponseWriter) writeString(data string) (int, error) {
	return writer.write([]byte(data))
}

func (writer *replResponseWriter) Write(data []byte) (int, error) {
	if writer == nil || writer.output == nil {
		return 0, io.ErrClosedPipe
	}
	originalLen := len(data)
	if len(data) == 0 {
		return 0, nil
	}
	if !writer.sessionHandled {
		writer.sessionHandled = true
		if writer.announceSession && bytes.HasPrefix(data, []byte("session: ")) {
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
				if _, err := writer.write(data[:newline+1]); err != nil {
					return 0, err
				}
				data = data[newline+1:]
			}
		}
	}
	if len(data) == 0 {
		return originalLen, nil
	}
	if !writer.started {
		if _, err := writer.writeString("\n\n" + writer.style.assistantLabel); err != nil {
			return 0, err
		}
		writer.started = true
	}
	if _, err := writer.write(data); err != nil {
		return 0, err
	}
	return originalLen, nil
}

func (writer *replResponseWriter) Finish() error {
	if writer == nil || !writer.started {
		return nil
	}
	_, err := writer.writeString("\n")
	return err
}

// replStatus is deliberately a small, terminal-facing projection of runtime
// state. It stores counts and token metadata only; it never stores prompt,
// tool-argument, provider-content, or context payload text.
type replStatus struct {
	app              *bee.Application
	sessionID        bee.SessionID
	profile          string
	model            string
	usage            bee.Usage
	usageCalls       int
	usageSeen        bool
	context          bee.ContextDiagnostics
	contextSeen      bool
	runUsage         bee.Usage
	runUsageCalls    int
	commandStatus    *bee.CommandStatusData
	memoryExtraction *bee.MemoryExtractionStatus
}

func newREPLStatus(app *bee.Application, sessionID bee.SessionID, model string) *replStatus {
	status := &replStatus{app: app, sessionID: sessionID, model: model}
	if app != nil {
		status.profile = app.ProviderProfile
		if status.model == "" {
			status.model = app.Config.Provider.Model
		}
	}
	return status
}

func (status *replStatus) observe(event bee.RunEvent) {
	if status == nil {
		return
	}
	if event.SessionID != "" {
		status.sessionID = event.SessionID
	}
	if event.Profile != "" {
		status.profile = event.Profile
	}
	if event.Model != "" {
		status.model = event.Model
	}
	switch event.Type {
	case bee.RunStart:
		status.runUsage = bee.Usage{}
		status.runUsageCalls = 0
	case bee.RunUsage:
		if event.Usage == nil {
			return
		}
		status.runUsage.PromptTokens += event.Usage.Usage.PromptTokens
		status.runUsage.CompletionTokens += event.Usage.Usage.CompletionTokens
		status.runUsage.TotalTokens += event.Usage.Usage.TotalTokens
		status.runUsage.CacheReadTokens += event.Usage.Usage.CacheReadTokens
		status.runUsage.CacheWriteTokens += event.Usage.Usage.CacheWriteTokens
		status.runUsageCalls++
		status.usage = status.runUsage
		status.usageCalls = status.runUsageCalls
		status.usageSeen = true
	case bee.RunContextDiagnostics:
		if event.ContextDiagnostics == nil {
			return
		}
		status.context = boundedContextDiagnostics(*event.ContextDiagnostics)
		status.contextSeen = true
	case bee.RunMemoryExtraction:
		if event.MemoryExtraction == nil {
			return
		}
		status.memoryExtraction = &bee.MemoryExtractionStatus{Attempted: event.MemoryExtraction.Attempted, Saved: len(event.MemoryExtraction.Saved), Unchanged: len(event.MemoryExtraction.Unchanged), Proposed: len(event.MemoryExtraction.Proposed), Rejected: event.MemoryExtraction.Rejected, Error: event.MemoryExtraction.Error}
	}
}

func boundedContextDiagnostics(diagnostics bee.ContextDiagnostics) bee.ContextDiagnostics {
	const maxSources = 16
	copy := diagnostics
	if len(copy.Sources) > maxSources {
		copy.Sources = append([]bee.ContextSourceDiagnostics(nil), copy.Sources[:maxSources]...)
	}
	return copy
}

func (status *replStatus) render(output io.Writer) error {
	if status == nil || output == nil {
		return nil
	}
	data := bee.CommandStatusData{}
	if status.commandStatus != nil {
		data = *status.commandStatus
	} else {
		command := bee.DispatchCommand(context.Background(), "/status", bee.CommandContext{Application: status.app, SessionID: status.sessionID, Model: status.model})
		if command.Status != nil {
			data = *command.Status
		}
	}
	data.SessionID = status.sessionID
	if status.profile != "" {
		data.Profile = status.profile
	}
	if status.model != "" {
		data.Model = status.model
	}
	if status.usageSeen {
		usage := status.usage
		data.RecentUsage = &usage
		data.RecentProviderCalls = status.usageCalls
	}
	if status.contextSeen {
		diagnostics := boundedContextDiagnostics(status.context)
		data.ContextDiagnostics = &diagnostics
	}
	if status.memoryExtraction != nil {
		copy := *status.memoryExtraction
		data.MemoryExtraction = &copy
	}
	_, err := fmt.Fprintf(output, "\n%s\n\n", bee.FormatCommandStatus(data, 92))
	return err
}

// terminalActivity is intentionally terminal-only and bounded. It renders a
// short spinner label, never provider text, tool arguments, or tool output.
// The first frame is delayed so session metadata and fast provider responses
// are not overwritten by the indicator.
type terminalActivity struct {
	output    io.Writer
	enabled   bool
	writeMu   *sync.Mutex
	mu        sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	displayed bool
	label     string
	frame     int
}

func newTerminalActivity(output io.Writer, writeMu *sync.Mutex) *terminalActivity {
	return &terminalActivity{output: output, enabled: writerIsTerminal(output), writeMu: writeMu}
}

func (activity *terminalActivity) Start(label string) {
	if activity == nil || !activity.enabled {
		return
	}
	activity.Stop()
	activity.mu.Lock()
	activity.label = boundedActivityLabel(label)
	activity.frame = 0
	activity.stop = make(chan struct{})
	activity.done = make(chan struct{})
	stop := activity.stop
	done := activity.done
	activity.mu.Unlock()
	go activity.loop(stop, done)
}

func (activity *terminalActivity) loop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			activity.mu.Lock()
			if activity.stop == nil || activity.stop != stop {
				activity.mu.Unlock()
				return
			}
			frame := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}[activity.frame%10]
			activity.frame++
			activity.displayed = true
			line := "\r\x1b[90m" + frame + " " + activity.label + "\x1b[0m"
			activity.mu.Unlock()
			activity.writeString(line)
		case <-stop:
			return
		}
	}
}

func (activity *terminalActivity) Stop() {
	if activity == nil || !activity.enabled {
		return
	}
	activity.mu.Lock()
	stop := activity.stop
	done := activity.done
	activity.stop = nil
	activity.done = nil
	displayed := activity.displayed
	activity.displayed = false
	activity.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	if displayed {
		activity.writeString("\r\x1b[2K")
	}
}

func (activity *terminalActivity) writeString(value string) {
	if activity.writeMu == nil {
		_, _ = io.WriteString(activity.output, value)
		return
	}
	activity.writeMu.Lock()
	defer activity.writeMu.Unlock()
	_, _ = io.WriteString(activity.output, value)
}

func boundedActivityLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(label))
	if label == "" {
		return "working"
	}
	const limit = 48
	if len(label) > limit {
		return label[:limit] + "..."
	}
	return label
}
