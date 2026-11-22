package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seth-el/bee"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "bee:", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output, errorOutput io.Writer) error {
	if len(args) > 0 && args[0] == "version" {
		if len(args) != 1 {
			return errors.New("bee version does not accept arguments")
		}
		_, err := fmt.Fprintln(output, bee.VersionString())
		return err
	}
	if len(args) > 0 && args[0] == "code" {
		return errors.New("the separate `bee code` command was removed; use ordinary `bee` with a prompt")
	}
	if len(args) > 0 && args[0] == "memory" {
		if len(args) > 1 && args[1] == "inventory" {
			return runMemoryInventory(args[2:], output, errorOutput)
		}
		if len(args) > 1 && args[1] == "candidates" {
			return runMemoryCandidates(args[2:], output, errorOutput)
		}
		if len(args) > 1 && args[1] == "review" {
			return runMemoryReview(args[2:], output, errorOutput)
		}
		if len(args) > 1 && args[1] == "import-normalized" {
			return memoryImportNormalized(args[2:], output, errorOutput)
		}
		if len(args) > 1 && args[1] == "semantic" {
			return runMemorySemantic(args[2:], output, errorOutput)
		}
		return runMemory(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "config" {
		return runConfig(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "skill" {
		return runSkills(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "plugin" {
		return runPlugins(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "jobs" {
		return runJobs(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "inbox" {
		return runInbox(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "sessions" {
		return runSessions(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "providers" {
		return runProviders(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "init" {
		return runInit(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "harness" {
		return runHarness(args[1:], output, errorOutput)
	}
	if len(args) > 0 && (args[0] == "health" || args[0] == "readiness") {
		return runHealth(args[0], args[1:], output, errorOutput)
	}
	if len(args) > 0 && args[0] == "maintenance" {
		return runMaintenance(args[1:], output, errorOutput)
	}
	if len(args) == 2 && args[0] == "child" && args[1] == "--stdio" {
		return bee.ServeChildStdio(context.Background(), input, output)
	}
	if len(args) == 1 && args[0] == "help" {
		args = []string{"-h"}
	}
	if suggestion, ok := likelySubcommandTypo(args); ok {
		return fmt.Errorf("unknown command %q (did you mean `bee %s`?); to send it to the model as a prompt instead, run: bee -- %s", args[0], suggestion, strings.Join(args, " "))
	}
	flags := flag.NewFlagSet("bee", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	configPath := flags.String("config", "", "path to the primary JSON config file")
	printExample := flags.Bool("print-example", false, "print an example config and exit")
	initConfig := flags.String("init-config", "", "create an example config at the given path")
	jsonMode := flags.Bool("json", false, "emit newline-delimited JSON run events")
	tuiMode := flags.Bool("tui", false, "run the interactive terminal interface")
	rpcMode := flags.Bool("rpc-stdio", false, "serve local JSON-RPC 2.0 requests on stdin")
	serveMode := flags.Bool("serve", false, "serve the authenticated OpenAI-compatible HTTP API")
	serveAddr := flags.String("serve-addr", "", "HTTP serve address (default from config; loopback by default)")
	serveToken := flags.String("serve-token", "", "HTTP bearer token override (prefer --serve-token-file)")
	serveTokenFile := flags.String("serve-token-file", "", "owner-only HTTP bearer-token file")
	servePolicy := flags.String("serve-policy", "", "HTTP session policy: trusted, prompted, or locked")
	serveMaxConcurrent := flags.Int("serve-max-concurrent", 0, "maximum concurrent HTTP harness turns")
	serveHeartbeat := flags.Int("serve-heartbeat-seconds", 0, "SSE heartbeat interval in seconds")
	serveRequestTimeout := flags.Int("serve-request-timeout-seconds", 0, "maximum duration of one HTTP harness turn")
	sessionID := flags.String("session", "", "continue a durable session: its ID, or \"last\" for the most recently active one")
	listSessions := flags.Bool("list-sessions", false, "list durable sessions as JSON")
	inspectSession := flags.String("inspect-session", "", "inspect durable session metadata")
	resumeSession := flags.String("resume", "", "deprecated alias for --session")
	continuePrompt := flags.String("continue", "", "with --session: resume that session's latest pending checkpoint (an interrupted turn) with this prompt")
	model := flags.String("model", "", "explicit model override for this run")
	profile := flags.String("profile", "", "explicit provider profile; never auto-selects cloud")
	scopeRoot := flags.String("scope-root", "", "personal assistant readable filesystem scope")
	writeRoot := flags.String("write-root", "", "preferred automatically writable filesystem root")
	codingProjectRoot := flags.String("coding-project-root", "", "explicit coding repository/project root (ordinary assistant composition)")
	codingManagedRoot := flags.String("coding-managed-root", "", "parent-owned Bee worktree/artifact root for coding")
	codingBaseCommit := flags.String("coding-base-commit", "", "parent-approved Git base commit for coding worktrees")
	fullMachine := flags.Bool("full-machine", false, "explicitly allow the full machine filesystem scope")
	noLaunchDir := flags.Bool("no-launch-dir", false, "do not make the directory Bee is started from writable")
	policyMode := flags.String("policy", "", "assistant policy: trusted, prompted, or locked")
	planMode := flags.Bool("plan", false, "enable plan mode for this session")
	noPlanMode := flags.Bool("no-plan", false, "disable plan mode for this session")
	remoteFlags := addRemoteGrantFlags(flags)
	flags.Usage = func() {
		fmt.Fprintln(errorOutput, "Usage: bee [flags] [prompt]")
		fmt.Fprintln(errorOutput, "       bee version")
		fmt.Fprintln(errorOutput, "       bee memory <list|show|search> [--memory-root PATH --retention-days N --include-sensitive]")
		fmt.Fprintln(errorOutput, "       bee memory mood-sweep [--memory-root PATH]")
		fmt.Fprintln(errorOutput, "       bee memory doctor [--memory-root PATH --semantic-index PATH]")
		fmt.Fprintln(errorOutput, "       bee memory proposals <list|show|approve|reject> [--memory-root PATH]")
		fmt.Fprintln(errorOutput, "       bee memory <export|import> [--memory-root PATH]")
		fmt.Fprintln(errorOutput, "       bee memory inventory --root LABEL=PATH [--output PATH]")
		fmt.Fprintln(errorOutput, "       bee memory source-archive --root LABEL=PATH [--exclude GLOB] [--name NAME] [--dry-run]")
		fmt.Fprintln(errorOutput, "       bee memory candidates --inventory PATH [--output PATH --report PATH]")
		fmt.Fprintln(errorOutput, "       bee memory review <list|decide|normalize> ...")
		fmt.Fprintln(errorOutput, "       bee memory import-normalized --normalized PATH --report PATH --memory-root PATH --audit-id ID --reason TEXT [--apply]")
		fmt.Fprintln(errorOutput, "       bee memory semantic <rebuild|status|verify> ...")
		fmt.Fprintln(errorOutput, "       bee config <validate|effective|migrate> [--config PATH] [--apply]")
		fmt.Fprintln(errorOutput, "       bee skill <list|inspect|install|test|enable|disable|rollback|remove> ...")
		fmt.Fprintln(errorOutput, "       bee plugin <list|inspect|install|test|enable|disable|rollback|remove> ...")
		fmt.Fprintln(errorOutput, "       bee jobs <create|list|show|approve|cancel|recover|retry|dismiss|run-due> [--store PATH]")
		fmt.Fprintln(errorOutput, "       bee inbox [list] [--all] [--json] | bee inbox ack <id>|--all")
		fmt.Fprintln(errorOutput, "       bee sessions repair --session ID [--root PATH] [--apply]")
		fmt.Fprintln(errorOutput, "       bee providers accept [--config PATH] [--profiles NAME,...] [--allow-cloud-probe]")
		fmt.Fprintln(errorOutput, "       bee init [--home PATH] [--json]")
		fmt.Fprintln(errorOutput, "       bee doctor [--home PATH] [--config PATH] [--json|--strict]")
		fmt.Fprintln(errorOutput, "       bee harness inspect [--config PATH --profile NAME --model MODEL --json --strict]")
		fmt.Fprintln(errorOutput, "       bee health|readiness --ops-config PATH [--json|--human]")
		fmt.Fprintln(errorOutput, "       bee maintenance backup --ops-config PATH --destination PATH [--include-artifacts]")
		fmt.Fprintln(errorOutput, "       bee maintenance restore --ops-config PATH --source PATH [--apply]")
		fmt.Fprintln(errorOutput, "       bee maintenance cleanup --ops-config PATH [--apply]")
		fmt.Fprintln(errorOutput, "       bee maintenance upgrade-check|migrate --ops-config PATH --manifest PATH [--apply]")
		fmt.Fprintln(errorOutput, "       bee child --stdio")
		fmt.Fprintln(errorOutput, "\nWith a prompt, Bee runs one streamed request. Without one, it starts an interactive REPL.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *planMode && *noPlanMode {
		return errors.New("--plan and --no-plan cannot be combined")
	}
	if *printExample {
		if *initConfig != "" || flags.NArg() != 0 {
			return errors.New("--print-example cannot be combined with --init-config or a prompt")
		}
		data, err := bee.ExampleConfigJSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, string(data))
		return err
	}
	if *initConfig != "" {
		if *configPath != "" || flags.NArg() != 0 {
			return errors.New("--init-config cannot be combined with --config or a prompt")
		}
		if err := bee.WriteExampleConfig(*initConfig); err != nil {
			return err
		}
		path := *initConfig
		if path == "" {
			path, _ = bee.DefaultConfigPath()
		}
		_, err := fmt.Fprintf(output, "created example config at %s\n", path)
		return err
	}
	if *rpcMode && (*jsonMode || *tuiMode || *listSessions || *inspectSession != "" || *resumeSession != "" || *continuePrompt != "" || *model != "" || *profile != "" || *planMode || *noPlanMode || *remoteFlags.enable || flags.NArg() != 0) {
		return errors.New("--rpc-stdio cannot be combined with other run modes or a prompt")
	}
	if *serveToken != "" && *serveTokenFile != "" {
		return errors.New("--serve-token and --serve-token-file cannot be combined")
	}
	config, err := bee.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, line := range bee.UnknownConfigFieldWarnings(config) {
		fmt.Fprintln(errorOutput, line)
	}
	serveConfig := config.Serve
	serveEnabled := *serveMode || serveConfig.Enabled
	if *serveAddr != "" {
		serveConfig.Addr = *serveAddr
	}
	if *servePolicy != "" {
		serveConfig.Policy = bee.AssistantPolicyMode(*servePolicy)
	}
	if *serveMaxConcurrent != 0 {
		serveConfig.MaxConcurrent = *serveMaxConcurrent
	}
	if *serveHeartbeat != 0 {
		serveConfig.HeartbeatSeconds = *serveHeartbeat
	}
	if *serveRequestTimeout != 0 {
		serveConfig.RequestTimeoutSeconds = *serveRequestTimeout
	}
	if err := serveConfig.Validate(); err != nil {
		return err
	}
	if serveEnabled && (*jsonMode || *tuiMode || *rpcMode || *listSessions || *inspectSession != "" || *resumeSession != "" || *continuePrompt != "" || *sessionID != "" || *model != "" || *planMode || *noPlanMode || *remoteFlags.enable || flags.NArg() != 0) {
		return errors.New("--serve cannot be combined with another run mode, a session selector, a model override, an SSH grant, or a prompt")
	}
	if serveEnabled && *servePolicy != "" && *policyMode != "" && *servePolicy != *policyMode {
		return errors.New("--serve-policy and --policy must not disagree")
	}
	// Ordinary chat is durable by default. This is the replacement workflow;
	// callers may still use an in-memory Application through the library.
	needsStore := true
	var store *bee.SessionStore
	if needsStore {
		store, err = bee.NewDefaultSessionStore()
		if err != nil {
			return err
		}
	}
	grant, err := remoteFlags.build()
	if err != nil {
		return err
	}
	if err := rejectRemoteGrantForNonTerminal(grant, *jsonMode, *rpcMode, *tuiMode); err != nil {
		return err
	}
	policy := bee.AssistantPolicyMode(*policyMode)
	approvalMode := config.Assistant.ApprovalMode
	if serveEnabled {
		approvalMode = serveConfig.ApprovalMode
	}
	if serveEnabled && policy == "" {
		policy = serveConfig.Policy
	} else if policy == "" {
		policy = config.Assistant.Policy
	}
	if err := validateNonInteractiveApprovalChannel(policy, approvalMode, *jsonMode, *rpcMode); err != nil {
		return err
	}
	var approval bee.ApprovalHook
	var auditor bee.Auditor
	interactiveInput := input
	if policy == bee.AssistantPolicyPrompted {
		auditor = terminalAuditor{output: errorOutput}
	}
	if policy != bee.AssistantPolicyLocked && !*jsonMode && !*rpcMode && !*tuiMode && !serveEnabled {
		// Trusted policy normally does not ask for approvals, but package
		// activation is deliberately approval-only. Keep an interactive hook
		// available for those exceptional tools in one-shot and REPL modes.
		reader := bufio.NewReader(input)
		approval = terminalApproval{input: reader, output: errorOutput}
		interactiveInput = reader
	}
	launchRoot := ""
	if !serveEnabled && !*rpcMode && !*noLaunchDir && *scopeRoot == "" && *writeRoot == "" && !*fullMachine {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			launchRoot = cwd
			if _, reason := bee.EligibleLaunchRoot(cwd, nil); reason != "" && reason != "already writable" && !*jsonMode && !*tuiMode {
				fmt.Fprintf(errorOutput, "bee: not making the launch directory writable (%s); pass --write-root to choose one\n", reason)
			}
		}
	}
	app, err := bee.ComposeGeneralAssistant(config, *profile, bee.AssistantCompositionOptions{Store: store, ScopeRoot: *scopeRoot, WriteRoot: *writeRoot, FullMachine: *fullMachine, LaunchRoot: launchRoot, Policy: policy, ApprovalMode: approvalMode, Approval: approval, Auditor: auditor, CodingProjectRoot: *codingProjectRoot, CodingManagedRoot: *codingManagedRoot, CodingBaseCommit: *codingBaseCommit})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close(context.Background()) }()
	if *planMode || *noPlanMode {
		if _, err := app.SetPlanMode("", *planMode); err != nil {
			return err
		}
	}
	if grant != nil {
		app.Capabilities.RemoteGranted = true
	}
	startExtensions := func(ctx context.Context) error {
		// Locked composition intentionally has no executor and must not launch
		// configured subprocesses merely because a config entry exists.
		if app.Executor == nil {
			return nil
		}
		return app.StartExtensions(ctx)
	}
	if *listSessions {
		if flags.NArg() != 0 {
			return errors.New("--list-sessions cannot be combined with a prompt")
		}
		list, err := app.ListSessions()
		if err != nil {
			return err
		}
		return encodeValue(output, list)
	}
	if *inspectSession != "" {
		if flags.NArg() != 0 {
			return errors.New("--inspect-session cannot be combined with a prompt")
		}
		metadata, err := app.InspectSession(bee.SessionID(*inspectSession))
		if err != nil {
			return err
		}
		return encodeValue(output, metadata)
	}
	selectedSession := *sessionID
	if *resumeSession != "" {
		fmt.Fprintln(errorOutput, "note: --resume is deprecated; use --session ID (or --session last)")
		if selectedSession != "" && selectedSession != *resumeSession {
			return errors.New("--session and --resume identify different sessions")
		}
		selectedSession = *resumeSession
	}
	if selectedSession == bee.LatestSessionSelector {
		latest, err := app.LatestSessionID()
		if err != nil {
			return err
		}
		selectedSession = string(latest)
	}
	prompt := strings.Join(flags.Args(), " ")
	if *continuePrompt != "" {
		if selectedSession == "" {
			return errors.New("--continue requires --session ID")
		}
		prompt = *continuePrompt
	}
	if *jsonMode && prompt == "" {
		return errors.New("--json requires a prompt")
	}
	if *rpcMode {
		if err := startExtensions(context.Background()); err != nil {
			return err
		}
		return (&bee.RPCServer{Application: app, Input: input, Output: output}).Serve(context.Background())
	}
	if serveEnabled {
		token := strings.TrimSpace(*serveToken)
		if token == "" {
			tokenFile := serveConfig.TokenFile
			if *serveTokenFile != "" {
				tokenFile = *serveTokenFile
			}
			token, err = bee.LoadOpenAIServeToken(tokenFile)
			if err != nil {
				return err
			}
		}
		routePath, err := bee.DefaultBeeOpenAIRouteBindingPath()
		if err != nil {
			return err
		}
		routes, err := bee.NewLocalRouteBindingStore(routePath)
		if err != nil {
			return err
		}
		handler, err := bee.NewOpenAIHTTPHandler(&bee.HarnessHandlers{Application: app}, bee.OpenAIHTTPOptions{
			BearerToken:       token,
			RouteBindings:     routes,
			MaxConcurrent:     serveConfig.MaxConcurrent,
			HeartbeatInterval: time.Duration(serveConfig.HeartbeatSeconds) * time.Second,
			RequestTimeout:    time.Duration(serveConfig.RequestTimeoutSeconds) * time.Second,
		})
		if err != nil {
			return err
		}
		if err := startExtensions(context.Background()); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if _, err := fmt.Fprintf(errorOutput, "bee: serving OpenAI-compatible API on %s with %s policy\n", serveConfig.Addr, policy); err != nil {
			return err
		}
		return bee.ServeOpenAIHTTP(ctx, serveConfig.Addr, handler)
	}
	if err := startExtensions(context.Background()); err != nil {
		return err
	}
	request := bee.RunRequest{Prompt: prompt, SessionID: bee.SessionID(selectedSession), Model: *model, RemoteGrant: grant}
	if *continuePrompt != "" {
		checkpoint, checkpointErr := app.LatestPendingTurnCheckpoint(request.SessionID)
		if checkpointErr != nil {
			return checkpointErr
		}
		request.CheckpointID = checkpoint.ID
	}
	if *jsonMode {
		return bee.RunJSON(context.Background(), app, request, output)
	}
	if *tuiMode {
		return bee.RunTUIWithSessionAndModel(context.Background(), app, interactiveInput, output, request.SessionID, request.Model)
	}
	if prompt != "" {
		if err := printGrantNotice(output, grant); err != nil {
			return err
		}
		_, err := bee.RunPrintWithSession(context.Background(), app, request, output, request.SessionID == "")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output)
		return err
	}
	if err := printGrantNotice(output, grant); err != nil {
		return err
	}
	return repl(app, interactiveInput, output, request)
}

func validateNonInteractiveApprovalChannel(policy bee.AssistantPolicyMode, approvalMode bee.ApprovalMode, jsonMode, rpcMode bool) error {
	if policy == bee.AssistantPolicyPrompted && approvalMode != bee.ApprovalModeAuto && (jsonMode || rpcMode) {
		return errors.New("prompted policy requires an interactive approval channel; use approval_mode auto or --policy trusted explicitly for non-interactive runs")
	}
	return nil
}

func repl(app *bee.Application, input io.Reader, output io.Writer, request bee.RunRequest) error {
	sessionID := request.SessionID
	model := request.Model
	style := newREPLStyle(output)
	writeMu := &sync.Mutex{}
	activity := newTerminalActivity(output, writeMu)
	status := newREPLStatus(app, sessionID, model)
	scanner := bufio.NewScanner(input)
	type inputResult struct {
		line string
		err  error
		eof  bool
	}
	inputLines := make(chan inputResult, 16)
	go func() {
		defer close(inputLines)
		for scanner.Scan() {
			inputLines <- inputResult{line: scanner.Text()}
		}
		inputLines <- inputResult{err: scanner.Err(), eof: true}
	}()

	approvalBroker := newREPLApprovalBroker()
	var approvalRequests <-chan *replApprovalPrompt
	if app.Executor != nil {
		previousApproval := app.Executor.Approval
		app.Executor.Approval = approvalBroker
		defer func() { app.Executor.Approval = previousApproval }()
		approvalRequests = approvalBroker.requests
	}

	type runResult struct {
		sessionID bee.SessionID
		err       error
	}
	queuedInput := make([]string, 0, 4)
	inputOpen := true
	var inputErr error
	announced := sessionID != ""
	for {
		if _, err := io.WriteString(output, style.userPrompt); err != nil {
			return err
		}
		var rawPrompt string
		if len(queuedInput) > 0 {
			rawPrompt = queuedInput[0]
			queuedInput = queuedInput[1:]
		} else if inputOpen {
			line, ok := <-inputLines
			if !ok {
				inputOpen = false
			} else if line.eof {
				inputOpen = false
				inputErr = line.err
			} else {
				rawPrompt = line.line
			}
		}
		if !inputOpen && rawPrompt == "" {
			_, _ = io.WriteString(output, "\n")
			return inputErr
		}
		prompt := strings.TrimSpace(rawPrompt)
		if prompt == "" {
			continue
		}
		if prompt == "/exit" {
			activity.Stop()
			return nil
		}
		command := bee.DispatchCommand(context.Background(), prompt, bee.CommandContext{Application: app, SessionID: sessionID, Model: model})
		checkpointID := ""
		if command.Handled {
			if command.Continue != nil {
				prompt = command.Continue.Prompt
				checkpointID = command.Continue.CheckpointID
			} else {
				if command.Resume != nil && command.Resume.Selected != nil {
					sessionID = command.Resume.Selected.ID
					announced = true
				}
				if command.NewSession != nil {
					sessionID = command.NewSession.Session.ID
					status.sessionID = sessionID
					announced = true
				}
				if command.Name == bee.CommandStatus {
					status.commandStatus = command.Status
					if activity.enabled {
						if err := status.render(output); err != nil {
							return err
						}
					}
				} else if command.Text != "" {
					if _, err := fmt.Fprintf(output, "%s\n", command.Text); err != nil {
						return err
					}
				}
				continue
			}
		}

		currentPrompt := prompt
		currentCheckpointID := checkpointID
		for currentPrompt != "" {
			if sessionID == "" && app.Store != nil {
				session, err := app.Store.Create(bee.SessionMetadata{})
				if err != nil {
					return err
				}
				sessionID = session.ID
				status.sessionID = sessionID
			}
			runCtx, cancelRun := context.WithCancel(context.Background())
			runRequest := bee.RunRequest{Prompt: currentPrompt, SessionID: sessionID, Model: model, RemoteGrant: request.RemoteGrant, CheckpointID: currentCheckpointID}
			response := &replResponseWriter{output: output, style: style, announceSession: !announced, writeMu: writeMu}
			done := make(chan runResult, 1)
			activity.Start("provider streaming")
			go func() {
				newSessionID, runErr := bee.RunPrintWithSessionObserved(runCtx, app, runRequest, response, !announced, func(event bee.RunEvent) {
					status.observe(event)
					switch event.Type {
					case bee.RunStart:
						activity.Start("provider streaming")
					case bee.RunAssistantDelta, bee.RunAssistant, bee.RunEnd, bee.RunFailure:
						activity.Stop()
					case bee.RunToolCall:
						activity.Start("tool call")
					case bee.RunToolResult:
						activity.Start("provider streaming")
					case bee.RunProviderOutputRejected:
						activity.Start("provider recovery")
					}
				})
				activity.Stop()
				if finishErr := response.Finish(); runErr == nil {
					runErr = finishErr
				}
				done <- runResult{sessionID: newSessionID, err: runErr}
			}()

			steerPrompt := ""
			var pendingApproval *replApprovalPrompt
			var completed runResult
			for {
				select {
				case approval := <-approvalRequests:
					pendingApproval = approval
					writeMu.Lock()
					choices := "[y/N]"
					if approval.request.SessionGrant != "" {
						choices = "[y/N, s = allow writes under " + approval.request.SessionGrant + " for this session]"
					}
					_, _ = fmt.Fprintf(output, "\nApproval required: %s\n%s\nargs: %s\nApprove? %s: ", approval.request.ToolName, approval.request.Summary, boundedApprovalArgs(approval.request.Args), choices)
					writeMu.Unlock()
				case line, ok := <-inputLines:
					if !ok {
						inputOpen = false
						inputLines = nil
						if pendingApproval != nil {
							pendingApproval.decision <- bee.ApprovalDenied
							pendingApproval = nil
						}
						continue
					}
					if line.eof {
						inputOpen = false
						inputErr = line.err
						inputLines = nil
						if pendingApproval != nil {
							pendingApproval.decision <- bee.ApprovalDenied
							pendingApproval = nil
						}
						continue
					}
					if pendingApproval != nil {
						answer := strings.ToLower(strings.TrimSpace(line.line))
						decision := bee.ApprovalDenied
						if answer == "y" || answer == "yes" {
							decision = bee.ApprovalApproved
						} else if (answer == "s" || answer == "session") && pendingApproval.request.SessionGrant != "" {
							decision = bee.ApprovalApprovedForSession
						}
						pendingApproval.decision <- decision
						pendingApproval = nil
						writeMu.Lock()
						_, _ = fmt.Fprintln(output, decision)
						writeMu.Unlock()
						continue
					}
					fields := strings.Fields(strings.TrimSpace(line.line))
					if len(fields) > 0 && fields[0] == "/steer" {
						activeCommand := bee.DispatchCommand(context.Background(), line.line, bee.CommandContext{Application: app, SessionID: sessionID, Model: model, RunActive: true})
						if activeCommand.Steer != nil && steerPrompt == "" {
							steerPrompt = activeCommand.Steer.Prompt
							writeMu.Lock()
							_, _ = fmt.Fprintln(output, activeCommand.Text)
							writeMu.Unlock()
							cancelRun()
							continue
						}
					}
					queuedInput = append(queuedInput, line.line)
				case completed = <-done:
					cancelRun()
					goto runComplete
				}
			}

		runComplete:
			if completed.sessionID != "" {
				sessionID = completed.sessionID
				status.sessionID = sessionID
			}
			announced = announced || sessionID != ""
			if completed.err != nil && !(steerPrompt != "" && errors.Is(completed.err, context.Canceled)) {
				if _, writeErr := fmt.Fprintf(output, "\nerror: %v\n\n", completed.err); writeErr != nil {
					return writeErr
				}
				currentPrompt = ""
				continue
			}
			if _, err := io.WriteString(output, "\n"); err != nil {
				return err
			}
			currentPrompt = steerPrompt
			currentCheckpointID = ""
		}
	}
}

func encodeValue(output io.Writer, value any) error {
	return json.NewEncoder(output).Encode(value)
}
