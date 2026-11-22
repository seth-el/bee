package bee

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ACPBackend owns only the adapter process lifecycle. Parent planning,
// workspace acquisition, approval, audit, and final integration remain in Bee.
type ACPBackend struct {
	Config           ACPProcessConfig
	BackendName      string
	Limits           CodingBackendLimits
	TransportFactory ACPTransportFactory
	Evidence         CodingEvidenceCollector
}

var _ CodingBackend = (*ACPBackend)(nil)

func NewACPBackend(config ACPProcessConfig) (*ACPBackend, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ACPBackend{Config: config, BackendName: "acp", TransportFactory: func(config ACPProcessConfig) (ACPTransport, error) {
		return NewACPStdioTransport(config)
	}}, nil
}

func (backend *ACPBackend) Run(ctx context.Context, task CodingBackendTask, observer CodingBackendObserver) (completion CodingBackendCompletion, runErr error) {
	completion = CodingBackendCompletion{ChildCompletionPacket: ChildCompletionPacket{TaskID: task.Task.TaskID, ParentID: task.Task.ParentID}, Backend: backend.backendName()}
	if backend == nil {
		return completion, errors.New("ACP backend is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := backend.Config.Validate(); err != nil {
		return completion, err
	}
	effective, err := task.EffectiveTask()
	if err != nil {
		return completion, err
	}
	if err := effective.Scope.Validate(); err != nil {
		return completion, fmt.Errorf("ACP task scope: %w", err)
	}
	resolvedScope, err := effective.Scope.rootPath()
	if err != nil {
		return completion, fmt.Errorf("resolve ACP task scope: %w", err)
	}
	effective.Scope.Root = resolvedScope
	if err := backend.validateWorkingDir(effective.Scope.Root); err != nil {
		return completion, err
	}
	artifactDir, err := childArtifactDir(effective)
	if err != nil {
		return completion, err
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return completion, fmt.Errorf("create ACP artifact directory: %w", err)
	}
	if err := os.Chmod(artifactDir, 0o700); err != nil {
		return completion, fmt.Errorf("secure ACP artifact directory: %w", err)
	}

	started := time.Now()
	codingNotify(observer, CodingBackendEvent{TaskID: effective.TaskID, ParentID: effective.ParentID, Type: "started", Info: backend.backendName()})
	defer func() {
		if completion.Status == "" {
			completion.Status = ChildFailed
		}
		if completion.Summary == "" {
			completion.Summary = "ACP backend completed without a summary"
		}
		codingNotify(observer, CodingBackendEvent{TaskID: effective.TaskID, ParentID: effective.ParentID, SessionID: completion.SessionID, Type: "completed", Info: fmt.Sprintf("%s (%s)", completion.Status, time.Since(started)), Completion: &completion.ChildCompletionPacket})
	}()

	processConfig := backend.Config
	if processConfig.WorkingDir == "" {
		processConfig.WorkingDir = effective.Scope.Root
	}
	factory := backend.TransportFactory
	if factory == nil {
		factory = func(config ACPProcessConfig) (ACPTransport, error) { return NewACPStdioTransport(config) }
	}
	transport, err := factory(processConfig)
	if err != nil {
		return completion, newCodingBackendFailure(BackendFailureUnavailable, err)
	}
	if transport == nil {
		return completion, newCodingBackendFailure(BackendFailureUnavailable, errors.New("ACP transport factory returned nil"))
	}
	// Permission handling is installed only through the transport's optional
	// setter. Unknown/fake transports remain compatible and simply deny agent
	// permission requests. The built-in handler is bounded by both config and
	// task policy and requires the isolated workspace lease.
	handler := NewBoundedACPPermissionHandler(processConfig, task)
	if setter, ok := transport.(interface{ SetACPPermissionHandler(ACPPermissionHandler) }); ok {
		setter.SetACPPermissionHandler(handler)
	} else if setter, ok := transport.(interface{ SetPermissionHandler(ACPPermissionHandler) }); ok {
		setter.SetPermissionHandler(handler)
	}
	defer func() { _ = transport.Close(context.Background()) }()
	if err := transport.Start(ctx); err != nil {
		return completion, newCodingBackendFailure(BackendFailureUnavailable, err)
	}

	maxOutput := effective.Budget.maxOutput(effective.Scope)
	if scopeLimit := effective.Scope.maxOutput(); scopeLimit < maxOutput {
		maxOutput = scopeLimit
	}
	before, snapshotNotes := snapshotScope(effective.Scope.Root)

	initializeCtx, cancelInitialize := boundedACPContext(ctx, effective.Budget.timeout(effective.Scope), backend.Config.startupTimeout())
	initialized, err := transport.Initialize(initializeCtx, ACPInitializeParams{ProtocolVersion: ACPProtocolVersion, ClientCapabilities: ACPClientCapabilities{FS: ACPFileSystemCapabilities{}, Auth: ACPAuthCapabilities{}}, ClientInfo: ACPImplementation{Name: "bee", Version: "dev"}})
	cancelInitialize()
	if err != nil {
		status := acpFailureStatus(ctx, err)
		completion = annotateACPFailure(backend.failureCompletion(effective, status, acpFailureSummary(transport, "ACP initialize failed: "+err.Error(), "")), acpFailureKind(ctx, transport, err))
		return completion, nil
	}

	authCtx, cancelAuth := boundedACPContext(ctx, effective.Budget.timeout(effective.Scope), backend.Config.requestTimeout())
	authSecret, authErr := authenticateACPIfRequired(authCtx, processConfig, transport, initialized)
	cancelAuth()
	if authErr != nil {
		status := acpFailureStatus(ctx, authErr)
		completion = annotateACPFailure(backend.failureCompletion(effective, status, acpFailureSummary(transport, "ACP authentication failed: "+authErr.Error(), authSecret)), acpFailureKind(ctx, transport, authErr))
		return completion, nil
	}

	sessionCtx, cancelSession := boundedACPContext(ctx, effective.Budget.timeout(effective.Scope), backend.Config.requestTimeout())
	session, err := transport.NewSession(sessionCtx, ACPSessionNewParams{TaskID: effective.TaskID, ParentID: effective.ParentID, Cwd: effective.Scope.Root, MCPServers: []any{}, Prompt: effective.Prompt, Plan: append([]string(nil), task.Plan...), AllowedTools: append([]string(nil), effective.AllowedTools...)})
	cancelSession()
	if err != nil {
		status := acpFailureStatus(ctx, err)
		completion = annotateACPFailure(backend.failureCompletion(effective, status, acpFailureSummary(transport, "ACP session creation failed: "+err.Error(), authSecret)), acpFailureKind(ctx, transport, err))
		return completion, nil
	}
	completion.SessionID = session.SessionID

	collector := &acpEventCollector{taskID: effective.TaskID, parentID: effective.ParentID, maxOutput: maxOutput}
	stopUpdates := make(chan struct{})
	updatesDone := make(chan struct{})
	go func() {
		defer close(updatesDone)
		for {
			select {
			case update, ok := <-transport.Updates():
				if !ok {
					return
				}
				update = redactACPUpdate(update, authSecret)
				if update.TaskID != "" && update.TaskID != effective.TaskID {
					collector.setFailure(fmt.Errorf("ACP update task id %q does not match %q", update.TaskID, effective.TaskID))
					return
				}
				if update.ParentID != "" && update.ParentID != effective.ParentID {
					collector.setFailure(fmt.Errorf("ACP update parent id %q does not match %q", update.ParentID, effective.ParentID))
					return
				}
				event := CodingBackendEvent{TaskID: effective.TaskID, ParentID: effective.ParentID, SessionID: update.SessionID, Type: update.Type, Text: truncateText(update.Text, maxOutput), Info: truncateText(update.Info, 4096), Artifact: truncateText(update.Artifact, 4096), Diff: truncateText(update.Diff, maxOutput), Payload: append([]byte(nil), update.Payload...), Completion: update.Completion, At: time.Now().UTC()}
				collector.observe(update, event)
				codingNotify(observer, event)
			case <-stopUpdates:
				return
			}
		}
	}()

	promptCtx, cancelPrompt := boundedACPContext(ctx, effective.Budget.timeout(effective.Scope), backend.Config.requestTimeout())
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-promptCtx.Done():
			if promptCtx.Err() != nil {
				_ = transport.Cancel(context.Background(), session.SessionID)
			}
		case <-cancelWatch:
		}
	}()
	promptResult, promptErr := transport.Prompt(promptCtx, ACPPromptParams{SessionID: session.SessionID, Prompt: []ACPContentBlock{{Type: "text", Text: buildCodingBackendPrompt(effective, task.Plan)}}})
	close(cancelWatch)
	cancelPrompt()
	close(stopUpdates)
	<-updatesDone

	if collector.failure() != nil {
		completion = backend.failureCompletion(effective, ChildMalformed, collector.failure().Error())
		completion.SessionID = session.SessionID
	} else if promptErr != nil {
		status := acpFailureStatus(ctx, promptErr)
		completion = annotateACPFailure(backend.failureCompletion(effective, status, acpFailureSummary(transport, "ACP prompt failed: "+promptErr.Error(), authSecret)), acpFailureKind(ctx, transport, promptErr))
		completion.SessionID = session.SessionID
	} else if promptResult.StopReason == "cancelled" {
		completion = backend.failureCompletion(effective, ChildCancelled, "ACP prompt cancelled")
		completion.SessionID = session.SessionID
	} else if promptResult.StopReason == "refusal" || promptResult.StopReason == "rejected" {
		completion = backend.failureCompletion(effective, ChildFailed, "ACP permission denied by adapter")
		completion.FailureKind = BackendFailurePermission
		completion.ChildCompletionPacket.FailureKind = BackendFailurePermission
		completion.SessionID = session.SessionID
	} else if packet := collector.completion(); packet != nil {
		completion.ChildCompletionPacket = *packet
		completion.SessionID = session.SessionID
	} else {
		summary := redactACPSecret(strings.TrimSpace(promptResult.Summary), authSecret)
		if summary == "" {
			summary = collector.summary()
		}
		if summary == "" {
			summary = "ACP coding backend completed"
		}
		completion.ChildCompletionPacket = ChildCompletionPacket{TaskID: effective.TaskID, ParentID: effective.ParentID, Status: ChildSucceeded, Summary: truncateText(summary, maxOutput), Evidence: collector.evidence(), OutputTruncated: collector.isTruncated()}
		completion.SessionID = session.SessionID
	}
	if completion.TaskID == "" {
		completion.TaskID = effective.TaskID
	}
	if completion.ParentID == "" {
		completion.ParentID = effective.ParentID
	}
	if completion.Status == "" {
		completion.Status = ChildSucceeded
	}
	redactChildCompletionPacket(&completion.ChildCompletionPacket, authSecret)
	if completion.Summary == "" {
		completion.Summary = collector.summary()
	}
	if completion.Summary == "" {
		completion.Summary = "ACP coding backend completed"
	}
	if completion.Status == ChildTimedOut {
		completion.Blockers = appendUnique(completion.Blockers, "ACP wall-clock budget expired")
	}
	if completion.Status == ChildCancelled {
		completion.Blockers = appendUnique(completion.Blockers, "parent cancelled ACP execution")
	}
	completion.ChangedFiles, completion.Uncertainty = diffScope(effective.Scope.Root, before, snapshotNotes)
	completion.Artifacts = appendUniqueList(completion.Artifacts, listArtifacts(artifactDir, effective.Scope.Root))
	completion.Commands = appendUniqueList(completion.Commands, collector.commands())
	completion.Tests = appendUniqueList(completion.Tests, collector.tests())
	completion.Evidence = appendUniqueList(completion.Evidence, collector.evidence())
	// Completion provenance is parent-owned even when an ACP adapter returned a
	// completion-shaped payload. Never accept an adapter-supplied reference.
	completion.Manifest = nil
	if err := completion.Validate(); err != nil {
		completion.Status = ChildMalformed
		completion.Summary = "ACP backend produced an invalid completion packet"
		completion.Blockers = appendUnique(completion.Blockers, err.Error())
	}
	completionPath := filepath.Join(artifactDir, "completion.json")
	if relative, relErr := filepath.Rel(effective.Scope.Root, completionPath); relErr == nil {
		completion.Artifacts = appendUnique(completion.Artifacts, filepath.ToSlash(relative))
	}
	definitions := make([]ToolDefinition, 0, len(effective.AllowedTools))
	for _, name := range normalizedStrings(effective.AllowedTools) {
		definitions = append(definitions, ToolDefinition{Name: name})
	}
	providerIdentity := CompletionProviderIdentity{Type: "acp", Model: backend.backendName()}
	providerFingerprint := fingerprintValue("bee-acp-provider-v1", struct {
		Name   string
		Config ACPProcessConfig
	}{backend.backendName(), backend.Config})
	harnessFingerprint := fingerprintValue("bee-acp-harness-v1", struct {
		ProviderFingerprint string
		Limits              CodingBackendLimits
		Tools               []ToolDefinition
	}{providerFingerprint, backend.Limits, definitions})
	manifestPacket, manifestErr := finalizeDelegatedCompletion(effective, completion.ChildCompletionPacket, artifactDir, Config{}, definitions, false, CompletionManifestACP, harnessFingerprint, providerFingerprint, providerIdentity)
	if manifestErr != nil {
		completion.Uncertainty = appendUnique(completion.Uncertainty, "write completion manifest: "+manifestErr.Error())
	} else {
		completion.ChildCompletionPacket = manifestPacket
	}
	if backend.Evidence != nil {
		evidence, evidenceErr := backend.Evidence.CollectCodingEvidence(ctx, task, completion.ChildCompletionPacket)
		if evidenceErr != nil {
			completion.Blockers = appendUnique(completion.Blockers, evidenceErr.Error())
			completion.Status = ChildFailed
		} else {
			completion.Evidence = appendUniqueList(completion.Evidence, evidence.Notes)
			completion.Artifacts = appendUniqueList(completion.Artifacts, evidence.Artifacts)
			if evidence.DiffArtifact != "" {
				completion.Artifacts = appendUnique(completion.Artifacts, evidence.DiffArtifact)
			}
			if evidence.ProposalArtifact != "" {
				completion.Artifacts = appendUnique(completion.Artifacts, evidence.ProposalArtifact)
			}
			completion.Evidence = appendUniqueList(completion.Evidence, evidence.ChangedFiles)
			completion.Evidence = appendUniqueList(completion.Evidence, evidence.UntrackedFiles)
			completion.Evidence = appendUniqueList(completion.Evidence, evidence.Notes)
			completion.Evidence = uniqueStrings(completion.Evidence)
			completion.BackendEvidence = evidence
		}
	}
	if err := completion.Validate(); err != nil {
		completion.Status = ChildMalformed
		completion.Summary = "ACP backend produced an invalid completion packet"
		completion.Blockers = appendUnique(completion.Blockers, err.Error())
	}
	if err := writeChildPacket(artifactDir, completion.ChildCompletionPacket); err != nil {
		completion.Uncertainty = appendUnique(completion.Uncertainty, err.Error())
	}
	return completion, runErr
}

func acpFailureSummary(transport ACPTransport, summary, secret string) string {
	summary = redactACPSecret(summary, secret)
	if transport == nil {
		return summary
	}
	diagnostics := strings.TrimSpace(transport.Diagnostics())
	if diagnostics == "" {
		return summary
	}
	return truncateText(summary+"; adapter diagnostics: "+redactACPSecret(diagnostics, secret), 4096)
}

func authenticateACPIfRequired(ctx context.Context, config ACPProcessConfig, transport ACPTransport, initialized ACPInitializeResult) (string, error) {
	configuredMethod := strings.TrimSpace(config.AuthMethod)
	if configuredMethod == "" && config.AuthEnvVar != "" {
		// Preserve the original explicit API-key configuration spelling.
		configuredMethod = ACPAuthMethodAPIKey
	}
	if configuredMethod == "" && len(initialized.AuthMethods) == 0 {
		return "", nil
	}
	if configuredMethod == "" {
		return "", errors.New("ACP authentication required; configure auth_method")
	}
	advertised := false
	for _, method := range initialized.AuthMethods {
		if strings.EqualFold(strings.TrimSpace(method.ID), configuredMethod) {
			advertised = true
			break
		}
	}
	if !advertised {
		return "", fmt.Errorf("ACP authentication required but configured method %q was not advertised; no supported authentication method was advertised", configuredMethod)
	}
	authenticator, ok := transport.(ACPAuthenticator)
	if !ok {
		return "", errors.New("ACP authentication transport does not support authenticate")
	}
	secret := ""
	var params ACPAuthenticateParams
	switch configuredMethod {
	case ACPAuthMethodAPIKey:
		if config.AuthEnvVar == "" {
			return "", errors.New("ACP authentication required; configure auth_env_var for api-key authentication")
		}
		var ok bool
		secret, ok = os.LookupEnv(config.AuthEnvVar)
		if !ok || strings.TrimSpace(secret) == "" {
			return "", fmt.Errorf("ACP authentication required; configured auth_env_var %q is missing or empty", config.AuthEnvVar)
		}
		params = ACPAuthenticateParams{MethodID: ACPAuthMethodAPIKey, Meta: ACPAuthenticateMeta{APIKey: ACPAPIKeyAuth{APIKey: secret}}}
	default:
		return "", fmt.Errorf("unsupported ACP authentication method %q", configuredMethod)
	}
	if err := authenticator.Authenticate(ctx, params); err != nil {
		return secret, errors.New(redactACPSecret(err.Error(), secret))
	}
	return secret, nil
}

func redactACPSecret(value, secret string) string {
	if value == "" || secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}

func redactACPUpdate(update ACPUpdate, secret string) ACPUpdate {
	update.Text = redactACPSecret(update.Text, secret)
	update.Info = redactACPSecret(update.Info, secret)
	update.Artifact = redactACPSecret(update.Artifact, secret)
	update.Diff = redactACPSecret(update.Diff, secret)
	if len(update.Payload) != 0 {
		update.Payload = []byte(redactACPSecret(string(update.Payload), secret))
	}
	redactChildCompletionPacket(update.Completion, secret)
	return update
}

func redactChildCompletionPacket(packet *ChildCompletionPacket, secret string) {
	if packet == nil || secret == "" {
		return
	}
	packet.TaskID = redactACPSecret(packet.TaskID, secret)
	packet.ParentID = redactACPSecret(packet.ParentID, secret)
	packet.Summary = redactACPSecret(packet.Summary, secret)
	packet.ChangedFiles = redactACPStrings(packet.ChangedFiles, secret)
	packet.Commands = redactACPStrings(packet.Commands, secret)
	packet.Tests = redactACPStrings(packet.Tests, secret)
	packet.Evidence = redactACPStrings(packet.Evidence, secret)
	packet.Uncertainty = redactACPStrings(packet.Uncertainty, secret)
	packet.Artifacts = redactACPStrings(packet.Artifacts, secret)
	packet.Questions = redactACPStrings(packet.Questions, secret)
	packet.Blockers = redactACPStrings(packet.Blockers, secret)
	packet.RequestedBackend = redactACPSecret(packet.RequestedBackend, secret)
	packet.EffectiveBackend = redactACPSecret(packet.EffectiveBackend, secret)
	packet.FallbackReason = redactACPSecret(packet.FallbackReason, secret)
	for index := range packet.Attempts {
		packet.Attempts[index].Backend = redactACPSecret(packet.Attempts[index].Backend, secret)
		packet.Attempts[index].AttemptID = redactACPSecret(packet.Attempts[index].AttemptID, secret)
		packet.Attempts[index].WorkspacePath = redactACPSecret(packet.Attempts[index].WorkspacePath, secret)
		packet.Attempts[index].Summary = redactACPSecret(packet.Attempts[index].Summary, secret)
	}
}

func redactACPStrings(values []string, secret string) []string {
	if len(values) == 0 || secret == "" {
		return values
	}
	redacted := make([]string, len(values))
	for index, value := range values {
		redacted[index] = redactACPSecret(value, secret)
	}
	return redacted
}

func annotateACPFailure(completion CodingBackendCompletion, kind CodingBackendFailureKind) CodingBackendCompletion {
	completion.FailureKind = kind
	completion.ChildCompletionPacket.FailureKind = kind
	return completion
}

func acpFailureKind(ctx context.Context, transport ACPTransport, err error) CodingBackendFailureKind {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return BackendFailureTimeout
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return BackendFailureCancelled
		}
	}
	var failure *CodingBackendFailure
	if errors.As(err, &failure) && failure != nil && failure.Kind != "" {
		return failure.Kind
	}
	text := strings.ToLower(errString(err))
	if transport != nil {
		text += " " + strings.ToLower(errString(transport.Failure())) + " " + strings.ToLower(transport.Diagnostics())
	}
	if strings.Contains(text, "permission") || strings.Contains(text, "forbidden") || strings.Contains(text, "refused") || strings.Contains(text, "rejected") {
		return BackendFailurePermission
	}
	if strings.Contains(text, "stdout closed") || strings.Contains(text, "process exited") || strings.Contains(text, "adapter") || strings.Contains(text, "auth") || strings.Contains(text, "credential") || strings.Contains(text, "quota") || strings.Contains(text, "rate limit") || strings.Contains(text, "usage limit") || strings.Contains(text, "unavailable") || strings.Contains(text, "connection") || strings.Contains(text, "eof") {
		return BackendFailureUnavailable
	}
	if strings.Contains(text, "protocol") || strings.Contains(text, "malformed") || strings.Contains(text, "decode") || strings.Contains(text, "invalid") || strings.Contains(text, "json") {
		return BackendFailureMalformed
	}
	return BackendFailureTask
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// CodingEvidence is intentionally kept separate from ChildCompletionPacket in
// the public result; this helper exposes a concise completion accessor for
// callers that only need the existing parent packet.
func (completion CodingBackendCompletion) Packet() ChildCompletionPacket {
	return completion.ChildCompletionPacket
}

func (backend *ACPBackend) backendName() string {
	if backend == nil || strings.TrimSpace(backend.BackendName) == "" {
		return "acp"
	}
	return backend.BackendName
}

func (backend *ACPBackend) validateWorkingDir(scopeRoot string) error {
	workingDir := backend.Config.WorkingDir
	if workingDir == "" {
		return nil
	}
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Errorf("resolve ACP working directory: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve ACP working directory symlinks: %w", err)
	}
	if !scopeWithin(scopeRoot, abs) {
		return errors.New("ACP working directory must remain within task scope")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat ACP working directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("ACP working directory must be a directory")
	}
	return nil
}

func (backend *ACPBackend) failureCompletion(task ChildTask, status ChildStatus, summary string) CodingBackendCompletion {
	if strings.TrimSpace(summary) == "" {
		summary = "ACP backend failed"
	}
	packet := ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: status, Summary: truncateText(summary, task.Budget.maxOutput(task.Scope))}
	packet.FailureKind = codingBackendFailureKind(nil, packet)
	if status == ChildFailed && strings.Contains(strings.ToLower(summary), "permission") {
		packet.FailureKind = BackendFailurePermission
	}
	if err := packet.Validate(); err != nil {
		packet.Status = ChildMalformed
		packet.Summary = "ACP backend produced an invalid completion packet"
		packet.Blockers = append(packet.Blockers, err.Error())
	}
	return CodingBackendCompletion{ChildCompletionPacket: packet, Backend: backend.backendName(), FailureKind: codingBackendFailureKind(nil, packet)}
}

func boundedACPContext(parent context.Context, taskTimeout, requestTimeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	limit := requestTimeout
	if taskTimeout > 0 && (limit <= 0 || taskTimeout < limit) {
		limit = taskTimeout
	}
	if limit <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, limit)
}

func acpFailureStatus(parent context.Context, err error) ChildStatus {
	if errors.Is(err, context.DeadlineExceeded) {
		return ChildTimedOut
	}
	if errors.Is(err, context.Canceled) || (parent != nil && errors.Is(parent.Err(), context.Canceled)) {
		return ChildCancelled
	}
	return ChildMalformed
}

func buildCodingBackendPrompt(task ChildTask, plan []string) string {
	var builder strings.Builder
	builder.WriteString(task.Prompt)
	builder.WriteString("\n\nObjective: ")
	builder.WriteString(task.Objective)
	if len(plan) > 0 {
		builder.WriteString("\nApproved plan:\n")
		for _, item := range plan {
			builder.WriteString("- ")
			builder.WriteString(item)
			builder.WriteByte('\n')
		}
	}
	builder.WriteString("Acceptance criteria:\n")
	for _, criterion := range task.AcceptanceCriteria {
		builder.WriteString("- ")
		builder.WriteString(criterion)
		builder.WriteByte('\n')
	}
	builder.WriteString("You have only the parent-approved working directory and harness capabilities. Do not widen scope, request credentials, or merge changes.")
	return builder.String()
}

func codingNotify(observer CodingBackendObserver, event CodingBackendEvent) {
	if observer == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	observer.ObserveCodingBackendEvent(event)
}

type acpEventCollector struct {
	mu              sync.Mutex
	taskID          string
	parentID        string
	maxOutput       int
	text            strings.Builder
	outputBytes     int
	outputTruncated bool
	packet          *ChildCompletionPacket
	failureErr      error
	evidenceValues  []string
	commandValues   []string
	testValues      []string
}

func (collector *acpEventCollector) observe(update ACPUpdate, event CodingBackendEvent) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.failureErr != nil {
		return
	}
	if update.Text != "" {
		remaining := collector.maxOutput - collector.outputBytes
		if remaining <= 0 {
			collector.outputTruncated = true
		} else {
			value := update.Text
			if len(value) > remaining {
				value = value[:remaining]
				collector.outputTruncated = true
			}
			collector.text.WriteString(value)
			collector.outputBytes += len(value)
		}
	}
	if update.Info != "" {
		collector.evidenceValues = appendUnique(collector.evidenceValues, truncateText(update.Info, 4096))
	}
	if update.Artifact != "" {
		collector.evidenceValues = appendUnique(collector.evidenceValues, truncateText(update.Artifact, 4096))
	}
	if update.Diff != "" {
		collector.evidenceValues = appendUnique(collector.evidenceValues, "diff: "+truncateText(update.Diff, collector.maxOutput))
	}
	if update.Completion != nil {
		packet := *update.Completion
		collector.packet = &packet
	}
	if event.Type == "test" && event.Text != "" {
		collector.testValues = appendUnique(collector.testValues, truncateText(event.Text, 4096))
	}
	if event.Type == "command" && event.Text != "" {
		collector.commandValues = appendUnique(collector.commandValues, truncateText(event.Text, 4096))
	}
}

func (collector *acpEventCollector) setFailure(err error) {
	collector.mu.Lock()
	if collector.failureErr == nil {
		collector.failureErr = err
	}
	collector.mu.Unlock()
}

func (collector *acpEventCollector) failure() error {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.failureErr
}

func (collector *acpEventCollector) completion() *ChildCompletionPacket {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.packet == nil {
		return nil
	}
	copy := *collector.packet
	return &copy
}

func (collector *acpEventCollector) summary() string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return strings.TrimSpace(collector.text.String())
}

func (collector *acpEventCollector) evidence() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]string(nil), collector.evidenceValues...)
}

func (collector *acpEventCollector) commands() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]string(nil), collector.commandValues...)
}

func (collector *acpEventCollector) tests() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]string(nil), collector.testValues...)
}

func (collector *acpEventCollector) isTruncated() bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.outputTruncated
}

func appendUniqueList(values, additions []string) []string {
	for _, value := range additions {
		values = appendUnique(values, value)
	}
	return values
}
