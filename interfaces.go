package bee

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RunPrint renders only human-readable assistant text and terminal errors.
func RunPrint(ctx context.Context, app *Application, request RunRequest, output io.Writer) error {
	_, err := RunPrintWithSession(ctx, app, request, output, false)
	return err
}

// RunPrintWithSession renders a human-readable run and returns its durable
// session ID. When announceSession is true, a stable session line is emitted
// before assistant output so a caller can resume the conversation without
// scraping model text.
func RunPrintWithSession(ctx context.Context, app *Application, request RunRequest, output io.Writer, announceSession bool) (SessionID, error) {
	return RunPrintWithSessionObserved(ctx, app, request, output, announceSession, nil)
}

// RunPrintWithSessionObserved is the terminal-friendly print renderer with an
// optional bounded event observer. The observer sees the same provider-neutral
// events as the other adapters, but the renderer still emits assistant text
// only. It is intentionally separate from RunPrintWithSession so existing
// print, JSON, RPC, and redirected-output contracts remain unchanged.
func RunPrintWithSessionObserved(ctx context.Context, app *Application, request RunRequest, output io.Writer, announceSession bool, observe func(RunEvent)) (SessionID, error) {
	if output == nil {
		return "", errors.New("print output is required")
	}
	events, err := app.Run(ctx, request)
	if err != nil {
		return "", err
	}
	var sessionID SessionID
	var runErr error
	sawDelta := false
	for event := range events {
		if observe != nil {
			observe(event)
		}
		switch event.Type {
		case RunSession:
			sessionID = event.SessionID
			if announceSession && sessionID != "" {
				if _, err := fmt.Fprintf(output, "session: %s\n", sessionID); err != nil {
					return sessionID, err
				}
			}
		case RunAssistantDelta:
			if _, err := io.WriteString(output, event.Delta); err != nil {
				return sessionID, err
			}
			sawDelta = true
		case RunAssistant:
			if !sawDelta && event.Message != nil {
				if _, err := io.WriteString(output, event.Message.Content); err != nil {
					return sessionID, err
				}
			}
			sawDelta = false
		case RunLocalCommand:
			if event.Command != nil {
				if _, err := io.WriteString(output, event.Command.Text); err != nil {
					return sessionID, err
				}
			}
		case RunFailure:
			runErr = formatRunError(event)
		}
	}
	return sessionID, runErr
}

// RunJSON writes an NDJSON event stream. Every stdout line is a JSON object.
func RunJSON(ctx context.Context, app *Application, request RunRequest, output io.Writer) error {
	events, err := app.Run(ctx, request)
	if err != nil {
		return err
	}
	return EncodeEvents(output, events)
}

// RunTUI presents a compact Codex-style terminal interface. Real terminals
// get a scrollable transcript and persistent composer; redirected input or
// output retains the line-oriented ANSI transcript for compatibility.
func RunTUI(ctx context.Context, app *Application, input io.Reader, output io.Writer) error {
	return RunTUIWithSessionAndModel(ctx, app, input, output, "", "")
}

func RunTUIWithSession(ctx context.Context, app *Application, input io.Reader, output io.Writer, initialSessionID SessionID) error {
	return RunTUIWithSessionAndModel(ctx, app, input, output, initialSessionID, "")
}

// RunTUIWithSessionAndModel runs the TUI with an optional durable session and
// explicit per-run model override. The legacy RunTUIWithSession wrapper
// remains available for callers using the configured model.
func RunTUIWithSessionAndModel(ctx context.Context, app *Application, input io.Reader, output io.Writer, initialSessionID SessionID, model string) error {
	if input == nil || output == nil {
		return errors.New("tui input and output are required")
	}
	if app == nil {
		return errors.New("tui application is required")
	}
	if terminal, ok, err := newTUITerminal(input, output); err != nil {
		return err
	} else if ok {
		return runFullscreenTUI(ctx, app, terminal, initialSessionID, model)
	}
	return runLineTUI(ctx, app, input, output, initialSessionID, model)
}

func runLineTUI(ctx context.Context, app *Application, input io.Reader, output io.Writer, initialSessionID SessionID, model string) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	if app.Executor != nil {
		previousApproval := app.Executor.Approval
		app.Executor.Approval = ApprovalFunc(func(approvalCtx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
			if err := approvalCtx.Err(); err != nil {
				return ApprovalDenied, err
			}
			if _, err := fmt.Fprintf(output, "\nApproval required: %s\n%s\nargs: %s\nApprove? [y/N]: ", request.ToolName, request.Summary, boundedAuditArgs(request.Args)); err != nil {
				return ApprovalDenied, err
			}
			if !scanner.Scan() {
				if scanner.Err() != nil {
					return ApprovalDenied, scanner.Err()
				}
				return ApprovalDenied, nil
			}
			answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if answer == "y" || answer == "yes" {
				return ApprovalApproved, nil
			}
			return ApprovalDenied, nil
		})
		defer func() { app.Executor.Approval = previousApproval }()
	}
	sessionID := initialSessionID
	if err := renderTUIHeader(output, app, sessionID, model); err != nil {
		return err
	}
	for {
		if err := renderTUIPrompt(output); err != nil {
			return err
		}
		if !scanner.Scan() {
			if scanner.Err() != nil {
				return scanner.Err()
			}
			_, _ = io.WriteString(output, "\n")
			return nil
		}
		if _, err := io.WriteString(output, "\n"); err != nil {
			return err
		}
		prompt := strings.TrimSpace(scanner.Text())
		switch prompt {
		case "":
			continue
		case "/exit":
			return nil
		}
		command := DispatchCommand(ctx, prompt, CommandContext{Application: app, SessionID: sessionID, Model: model})
		if command.Handled {
			if command.Resume != nil && command.Resume.Selected != nil {
				sessionID = command.Resume.Selected.ID
			}
			if command.NewSession != nil {
				sessionID = command.NewSession.Session.ID
			}
			if command.Text != "" {
				if err := renderTUICommand(output, command.Text); err != nil {
					return err
				}
			} else if _, err := io.WriteString(output, "\n"); err != nil {
				return err
			}
			continue
		}
		events, err := app.Run(ctx, RunRequest{Prompt: prompt, SessionID: sessionID, Model: model})
		if err != nil {
			return err
		}
		var renderErr error
		sessionID, renderErr = renderTUIEventsFromSessionWithVisibility(output, events, sessionID, app.DisplayVisibility())
		if renderErr != nil {
			return renderErr
		}
	}
}

func RenderTUIEvents(output io.Writer, events <-chan RunEvent) error {
	_, err := renderTUIEvents(output, events)
	return err
}

func renderTUIEvents(output io.Writer, events <-chan RunEvent) (SessionID, error) {
	return renderTUIEventsFromSession(output, events, "")
}

// RPCServer is a local line-oriented JSON-RPC 2.0 adapter. Run events are
// emitted as notifications followed by one response for the request ID.
type RPCServer struct {
	Application *Application
	Handlers    *HarnessHandlers
	Input       io.Reader
	Output      io.Writer
}

// HarnessHandlers is the transport-neutral request layer shared by RPC and
// HTTP adapters. One Run call consumes exactly one complete Application turn,
// including every provider/tool iteration, while optionally exposing the
// provider-neutral event stream to the transport.
type HarnessHandlers struct {
	Application *Application
}

// HarnessRunResult is the terminal projection of one complete harness turn.
// Response is the final agent response; local commands that do not produce a
// Response are projected into Message instead.
type HarnessRunResult struct {
	RunID     string
	SessionID SessionID
	Model     string
	Message   Message
	Response  *Response
	Usage     Usage
}

// Run executes one complete harness turn. The observer is synchronous so a
// streaming transport can apply backpressure. Returning an observer error
// cancels the run and returns that transport error.
func (handlers *HarnessHandlers) Run(ctx context.Context, request RunRequest, observe func(RunEvent) error) (HarnessRunResult, error) {
	if handlers == nil || handlers.Application == nil {
		return HarnessRunResult{}, errors.New("request handlers application is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := handlers.Application.Run(runCtx, request)
	if err != nil {
		return HarnessRunResult{}, err
	}
	var result HarnessRunResult
	var runErr error
	var streamed strings.Builder
	for event := range events {
		if event.RunID != "" {
			result.RunID = event.RunID
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
		if event.Model != "" {
			result.Model = event.Model
		}
		switch event.Type {
		case RunAssistantDelta:
			streamed.WriteString(event.Delta)
		case RunAssistant:
			if event.Message != nil {
				result.Message = *event.Message
			}
		case RunLocalCommand:
			if event.Command != nil {
				result.Message = Message{Role: RoleAssistant, Content: event.Command.Text}
			}
		case RunUsage:
			if event.Usage != nil {
				result.Usage.PromptTokens += event.Usage.Usage.PromptTokens
				result.Usage.CompletionTokens += event.Usage.Usage.CompletionTokens
				result.Usage.TotalTokens += event.Usage.Usage.TotalTokens
				result.Usage.CacheReadTokens += event.Usage.Usage.CacheReadTokens
				result.Usage.CacheWriteTokens += event.Usage.Usage.CacheWriteTokens
			}
		case RunFailure:
			if runErr == nil {
				runErr = formatRunError(event)
			}
		case RunEnd:
			if event.Response != nil {
				copy := *event.Response
				result.Response = &copy
				result.Message = copy.Message
			}
			if runErr == nil && event.Error != "" {
				runErr = formatRunError(event)
			}
		}
		if observe != nil {
			if err := observe(event); err != nil {
				cancel()
				return result, err
			}
		}
	}
	if result.Message.Role == "" {
		result.Message.Role = RoleAssistant
	}
	if result.Message.Content == "" && streamed.Len() > 0 {
		result.Message.Content = streamed.String()
	}
	return result, runErr
}

func (server *RPCServer) requestHandlers() (*HarnessHandlers, error) {
	if server == nil {
		return nil, errors.New("rpc server is required")
	}
	if server.Handlers != nil {
		if server.Handlers.Application == nil {
			return nil, errors.New("rpc request handlers application is required")
		}
		return server.Handlers, nil
	}
	if server.Application == nil {
		return nil, errors.New("rpc server application is required")
	}
	return &HarnessHandlers{Application: server.Application}, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcApplicationErr = -32000
)

func (server *RPCServer) Serve(ctx context.Context) error {
	if server == nil || server.Input == nil || server.Output == nil {
		return errors.New("rpc server input and output are required")
	}
	handlers, err := server.requestHandlers()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(server.Input)
	encoder := json.NewEncoder(server.Output)
	for scanner.Scan() {
		line := scanner.Bytes()
		var request rpcRequest
		if err := json.Unmarshal(line, &request); err != nil {
			if err := encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcParseError, Message: "parse error"}}); err != nil {
				return err
			}
			continue
		}
		response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
		if err := validateRPCRequest(request); err != nil {
			response.Error = &rpcError{Code: rpcInvalidRequest, Message: err.Error()}
			if err := encoder.Encode(response); err != nil {
				return err
			}
			continue
		}
		switch request.Method {
		case "run", "continue":
			var params RunRequest
			if err := json.Unmarshal(request.Params, &params); err != nil || strings.TrimSpace(params.Prompt) == "" {
				response.Error = &rpcError{Code: rpcInvalidParams, Message: "prompt is required"}
				if err := encoder.Encode(response); err != nil {
					return err
				}
				continue
			}
			if request.Method == "continue" && params.SessionID == "" {
				response.Error = &rpcError{Code: rpcInvalidParams, Message: "session_id is required"}
				if err := encoder.Encode(response); err != nil {
					return err
				}
				continue
			}
			result, runErr := handlers.Run(ctx, params, func(event RunEvent) error {
				return encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "event", "params": event})
			})
			if runErr != nil {
				response.Error = &rpcError{Code: rpcApplicationErr, Message: runErr.Error()}
			} else {
				response.Result = map[string]any{"run_id": result.RunID, "session_id": result.SessionID}
			}
		case "list_sessions":
			result, err := handlers.Application.ListSessions()
			if err != nil {
				response.Error = &rpcError{Code: rpcApplicationErr, Message: err.Error()}
			} else {
				response.Result = result
			}
		case "resume", "inspect_session":
			var params struct {
				SessionID SessionID `json:"session_id"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil || params.SessionID == "" {
				response.Error = &rpcError{Code: rpcInvalidParams, Message: "session_id is required"}
			} else {
				metadata, err := handlers.Application.InspectSession(params.SessionID)
				if err != nil {
					response.Error = &rpcError{Code: rpcApplicationErr, Message: err.Error()}
				} else {
					response.Result = metadata
				}
			}
		default:
			response.Error = &rpcError{Code: rpcMethodNotFound, Message: "method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func validateRPCRequest(request rpcRequest) error {
	if request.JSONRPC != "2.0" {
		return errors.New("jsonrpc must be 2.0")
	}
	if len(request.ID) == 0 || string(request.ID) == "null" {
		return errors.New("request id is required")
	}
	var value any
	if err := json.Unmarshal(request.ID, &value); err != nil {
		return errors.New("request id must be a string or number")
	}
	if _, ok := value.(string); !ok {
		if _, ok := value.(float64); !ok {
			return errors.New("request id must be a string or number")
		}
	}
	if strings.TrimSpace(request.Method) == "" {
		return errors.New("method is required")
	}
	return nil
}
