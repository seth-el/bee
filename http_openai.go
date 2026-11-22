package bee

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultOpenAIServeAddr                  = "127.0.0.1:18791"
	DefaultOpenAIServeMaxConcurrent         = 8
	DefaultOpenAIServeHeartbeatSeconds      = 15
	DefaultOpenAIServeRequestTimeoutSeconds = 30 * 60
	MaxOpenAIRequestBodyBytes               = 1 << 20
	MaxOpenAIConversationIDBytes            = 256
	MaxOpenAIChatMessages                   = 512
	MaxOpenAIServeConcurrent                = 128
	MaxOpenAIServeRequestTimeoutSeconds     = 24 * 60 * 60
	MaxServeBearerTokenBytes                = 4096
)

var (
	errOpenAIConversationBusy = errors.New("conversation already has an active request")
	errOpenAISessionBusy      = errors.New("Bee session already has an active request")
)

func DefaultOpenAIServeConfig() OpenAIServeConfig {
	return OpenAIServeConfig{
		Addr:                  DefaultOpenAIServeAddr,
		TokenFile:             "~/.bee/secrets/serve.token",
		Policy:                AssistantPolicyPrompted,
		ApprovalMode:          ApprovalModeManual,
		MaxConcurrent:         DefaultOpenAIServeMaxConcurrent,
		HeartbeatSeconds:      DefaultOpenAIServeHeartbeatSeconds,
		RequestTimeoutSeconds: DefaultOpenAIServeRequestTimeoutSeconds,
	}
}

func (config OpenAIServeConfig) effective() OpenAIServeConfig {
	defaults := DefaultOpenAIServeConfig()
	if strings.TrimSpace(config.Addr) == "" {
		config.Addr = defaults.Addr
	}
	if strings.TrimSpace(config.TokenFile) == "" {
		config.TokenFile = defaults.TokenFile
	}
	if config.Policy == "" {
		config.Policy = defaults.Policy
	}
	if config.ApprovalMode == "" {
		config.ApprovalMode = defaults.ApprovalMode
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = defaults.MaxConcurrent
	}
	if config.HeartbeatSeconds == 0 {
		config.HeartbeatSeconds = defaults.HeartbeatSeconds
	}
	if config.RequestTimeoutSeconds == 0 {
		config.RequestTimeoutSeconds = defaults.RequestTimeoutSeconds
	}
	return config
}

func (config OpenAIServeConfig) Validate() error {
	config = config.effective()
	if strings.TrimSpace(config.Addr) == "" {
		return errors.New("serve addr is required")
	}
	host, port, err := net.SplitHostPort(config.Addr)
	if err != nil || strings.TrimSpace(port) == "" || strings.ContainsRune(host, '\x00') {
		return fmt.Errorf("serve addr %q must be a host:port", config.Addr)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("serve addr %q has an invalid port", config.Addr)
	}
	if strings.ContainsRune(config.TokenFile, '\x00') {
		return errors.New("serve token_file contains NUL")
	}
	if config.Policy != AssistantPolicyTrusted && config.Policy != AssistantPolicyPrompted && config.Policy != AssistantPolicyLocked {
		return fmt.Errorf("serve policy %q is invalid", config.Policy)
	}
	if err := config.ApprovalMode.Validate(); err != nil {
		return fmt.Errorf("serve %w", err)
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > MaxOpenAIServeConcurrent {
		return fmt.Errorf("serve max_concurrent must be between 1 and %d", MaxOpenAIServeConcurrent)
	}
	if config.HeartbeatSeconds < 1 || config.HeartbeatSeconds > 60 {
		return errors.New("serve heartbeat_seconds must be between 1 and 60")
	}
	if config.RequestTimeoutSeconds < 1 || config.RequestTimeoutSeconds > MaxOpenAIServeRequestTimeoutSeconds {
		return fmt.Errorf("serve request_timeout_seconds must be between 1 and %d", MaxOpenAIServeRequestTimeoutSeconds)
	}
	return nil
}

// LoadOpenAIServeToken reads an owner-only bearer-token file. The token is
// retained only in memory and is never returned by an endpoint.
func LoadOpenAIServeToken(path string) (string, error) {
	resolved, err := ResolveBeeHomePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve serve token file: %w", err)
	}
	if strings.TrimSpace(resolved) == "" {
		return "", errors.New("serve bearer token or token_file is required")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect serve token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("serve token file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("serve token file must be owner-only (0600 or stricter)")
	}
	if info.Size() > MaxServeBearerTokenBytes {
		return "", fmt.Errorf("serve token file exceeds %d bytes", MaxServeBearerTokenBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read serve token file: %w", err)
	}
	return validateOpenAIServeToken(strings.TrimSpace(string(data)))
}

func validateOpenAIServeToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("serve bearer token is required")
	}
	if len(token) > MaxServeBearerTokenBytes {
		return "", fmt.Errorf("serve bearer token exceeds %d bytes", MaxServeBearerTokenBytes)
	}
	if !utf8.ValidString(token) || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", errors.New("serve bearer token contains whitespace or control characters")
	}
	return token, nil
}

// OpenAIHTTPOptions configures the OpenAI-compatible transport around the
// shared harness request handlers.
type OpenAIHTTPOptions struct {
	BearerToken       string
	ModelIDs          []string
	RouteBindings     RouteBindingStore
	MaxConcurrent     int
	HeartbeatInterval time.Duration
	RequestTimeout    time.Duration
}

// OpenAIHTTPHandler implements the authenticated OpenAI-compatible endpoints.
// Active request and session maps are bounded by the admission semaphore.
type OpenAIHTTPHandler struct {
	handlers          *HarnessHandlers
	bearerToken       string
	modelIDs          []string
	routeBindings     RouteBindingStore
	heartbeatInterval time.Duration
	requestTimeout    time.Duration
	runLifecycle      *openAIWebRunRegistry
	slots             chan struct{}

	mu                  sync.Mutex
	activeConversations map[string]struct{}
	activeSessions      map[SessionID]string
}

func NewOpenAIHTTPHandler(handlers *HarnessHandlers, options OpenAIHTTPOptions) (*OpenAIHTTPHandler, error) {
	if handlers == nil || handlers.Application == nil {
		return nil, errors.New("OpenAI HTTP request handlers are required")
	}
	if handlers.Application.Store == nil {
		return nil, errors.New("OpenAI HTTP serving requires a durable session store")
	}
	token, err := validateOpenAIServeToken(options.BearerToken)
	if err != nil {
		return nil, err
	}
	if options.RouteBindings == nil {
		return nil, errors.New("OpenAI HTTP route bindings are required")
	}
	if options.MaxConcurrent == 0 {
		options.MaxConcurrent = DefaultOpenAIServeMaxConcurrent
	}
	if options.MaxConcurrent < 1 || options.MaxConcurrent > MaxOpenAIServeConcurrent {
		return nil, fmt.Errorf("OpenAI HTTP max concurrency must be between 1 and %d", MaxOpenAIServeConcurrent)
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = time.Duration(DefaultOpenAIServeHeartbeatSeconds) * time.Second
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = time.Duration(DefaultOpenAIServeRequestTimeoutSeconds) * time.Second
	}
	models := append([]string(nil), options.ModelIDs...)
	if len(models) == 0 {
		models = []string{openAIModelID(handlers.Application)}
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if strings.TrimSpace(model) == "" || len(model) > 256 || !utf8.ValidString(model) || strings.ContainsRune(model, '\x00') {
			return nil, errors.New("OpenAI HTTP model IDs must be non-empty bounded UTF-8")
		}
		if _, duplicate := seen[model]; duplicate {
			return nil, fmt.Errorf("duplicate OpenAI HTTP model ID %q", model)
		}
		seen[model] = struct{}{}
	}
	if handlers.Application.Executor != nil {
		if err := handlers.Application.EnableApprovalBroker(); err != nil {
			return nil, fmt.Errorf("OpenAI HTTP approval broker: %w", err)
		}
	}
	return &OpenAIHTTPHandler{
		handlers:            handlers,
		bearerToken:         token,
		modelIDs:            models,
		routeBindings:       options.RouteBindings,
		heartbeatInterval:   options.HeartbeatInterval,
		requestTimeout:      options.RequestTimeout,
		runLifecycle:        newOpenAIWebRunRegistry(handlers.Application),
		slots:               make(chan struct{}, options.MaxConcurrent),
		activeConversations: make(map[string]struct{}),
		activeSessions:      make(map[SessionID]string),
	}, nil
}

func openAIModelID(app *Application) string {
	if app != nil && strings.TrimSpace(app.ProviderProfile) != "" {
		return app.ProviderProfile
	}
	if app != nil && strings.TrimSpace(app.Config.ProviderProfile) != "" {
		return app.Config.ProviderProfile
	}
	if app != nil && strings.TrimSpace(app.Config.Provider.Model) != "" {
		return app.Config.Provider.Model
	}
	return "bee"
}

func (handler *OpenAIHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.handlers == nil {
		writeOpenAIError(writer, http.StatusServiceUnavailable, "server_error", "OpenAI HTTP service is unavailable")
		return
	}
	if !validOpenAIBearerToken(request.Header.Get("Authorization"), handler.bearerToken) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="bee"`)
		writeOpenAIError(writer, http.StatusUnauthorized, "invalid_api_key", "invalid bearer token")
		return
	}
	switch request.URL.Path {
	case "/healthz":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writeOpenAIJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case "/v1/models":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		handler.handleModels(writer)
	case "/api/approvals":
		handler.handleApprovals(writer, request)
	case "/api/interaction-policy":
		handler.handleInteractionPolicy(writer, request)
	case "/api/commands":
		handler.handleCommand(writer, request)
	case "/api/runs/current":
		handler.handleCurrentRun(writer, request)
	case "/api/sessions":
		handler.handleSessions(writer, request)
	case "/api/notifications":
		handler.handleNotifications(writer, request)
	case "/v1/chat/completions":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		handler.handleChatCompletions(writer, request)
	default:
		if strings.HasPrefix(request.URL.Path, "/api/attachments/") {
			handler.handleWebAttachmentPath(writer, request, strings.TrimPrefix(request.URL.Path, "/api/attachments/"))
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/notifications/") {
			handler.handleNotificationPath(writer, request, strings.TrimPrefix(request.URL.Path, "/api/notifications/"))
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/sessions/") {
			handler.handleSessionPath(writer, request, strings.TrimPrefix(request.URL.Path, "/api/sessions/"))
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/runs/") {
			handler.handleRunLifecyclePath(writer, request, strings.TrimPrefix(request.URL.Path, "/api/runs/"))
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/approvals/") {
			handler.handleApprovalDecision(writer, request, strings.TrimPrefix(request.URL.Path, "/api/approvals/"))
			return
		}
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func validOpenAIBearerToken(header, expected string) bool {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) == 1
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (handler *OpenAIHTTPHandler) handleModels(writer http.ResponseWriter) {
	models := make([]openAIModel, 0, len(handler.modelIDs))
	for _, id := range handler.modelIDs {
		models = append(models, openAIModel{ID: id, Object: "model", OwnedBy: "bee"})
	}
	writeOpenAIJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": models})
}

type openAIChatRequest struct {
	Model         string              `json:"model"`
	Messages      []openAIChatMessage `json:"messages"`
	Stream        bool                `json:"stream,omitempty"`
	N             int                 `json:"n,omitempty"`
	AttachmentIDs []string            `json:"attachment_ids,omitempty"`
	CheckpointID  string              `json:"checkpoint_id,omitempty"`
}

type openAIChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type openAIChatMessageResponse struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatChoice struct {
	Index        int                       `json:"index"`
	Message      openAIChatMessageResponse `json:"message"`
	FinishReason string                    `json:"finish_reason"`
}

type openAIChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
}

func (handler *OpenAIHTTPHandler) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxOpenAIRequestBodyBytes)
	var input openAIChatRequest
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&input); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "request body must be valid bounded JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "request body has trailing JSON data")
		return
	}
	if input.N != 0 && input.N != 1 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "only n=1 is supported")
		return
	}
	if !validOpenAICheckpointID(input.CheckpointID) {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "checkpoint_id must be one bounded identifier")
		return
	}
	lifecycle, err := openAIWebLifecycleRequested(request)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if lifecycle && !input.Stream {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "run lifecycle recovery requires stream=true")
		return
	}
	if !handler.supportsModel(input.Model) {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "model is not served by this Bee instance")
		return
	}
	prompt, systemPrompt, err := newestOpenAIInput(input.Messages)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := handler.admitConversation(conversationID); err != nil {
		status := http.StatusTooManyRequests
		typ := "rate_limit_error"
		if errors.Is(err, errOpenAIConversationBusy) {
			status = http.StatusConflict
			typ = "conflict"
		}
		writeOpenAIError(writer, status, typ, err.Error())
		return
	}
	var activeSession SessionID
	var activeSessionMu sync.Mutex
	defer func() {
		activeSessionMu.Lock()
		sessionID := activeSession
		activeSessionMu.Unlock()
		handler.releaseConversation(conversationID, sessionID)
	}()

	routeKey := openAIRouteKey(conversationID)
	sessionID, err := handler.lookupSession(request.Context(), routeKey)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if sessionID != "" {
		if err := handler.claimSession(conversationID, sessionID); err != nil {
			writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
			return
		}
		activeSessionMu.Lock()
		activeSession = sessionID
		activeSessionMu.Unlock()
	}
	if input.CheckpointID != "" {
		if sessionID == "" {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "checkpoint continuation requires a selected durable session")
			return
		}
		session, loadErr := handler.handlers.Application.Store.Load(sessionID)
		if loadErr != nil {
			writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load checkpoint session")
			return
		}
		if _, checkpointErr := session.TurnCheckpoint(input.CheckpointID); checkpointErr != nil {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "checkpoint_id does not select a checkpoint in this session")
			return
		}
	}
	if err := handler.validateWebChatAttachments(sessionID, input.AttachmentIDs); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	runRequest := RunRequest{Prompt: prompt, SystemPrompt: systemPrompt, SessionID: sessionID, Title: "openai:" + conversationID, AttachmentIDs: append([]string(nil), input.AttachmentIDs...), CheckpointID: input.CheckpointID, DisableCommands: true}
	bindSession := func(event RunEvent) error {
		if event.SessionID == "" {
			return nil
		}
		activeSessionMu.Lock()
		defer activeSessionMu.Unlock()
		if activeSession != "" {
			if activeSession != event.SessionID {
				return fmt.Errorf("conversation session changed from %q to %q", activeSession, event.SessionID)
			}
			return nil
		}
		if err := handler.claimSession(conversationID, event.SessionID); err != nil {
			return err
		}
		activeSession = event.SessionID
		if err := handler.routeBindings.Bind(request.Context(), routeKey, event.SessionID); err != nil {
			return fmt.Errorf("persist conversation session mapping: %w", err)
		}
		return nil
	}
	runParent := request.Context()
	if lifecycle {
		runParent = context.WithoutCancel(request.Context())
	}
	runCtx, cancel := context.WithTimeout(runParent, handler.requestTimeout)
	defer cancel()
	if input.Stream {
		handler.streamChatCompletion(runCtx, request.Context(), writer, runRequest, input.Model, conversationID, lifecycle, bindSession)
		return
	}
	result, err := handler.handlers.Run(runCtx, runRequest, bindSession)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writer.Header().Set("X-Bee-Session-Id", string(result.SessionID))
	finishReason := "stop"
	if result.Response != nil && strings.TrimSpace(result.Response.FinishReason) != "" {
		finishReason = result.Response.FinishReason
	}
	response := openAIChatResponse{
		ID:      openAICompletionID(result.RunID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   input.Model,
		Choices: []openAIChatChoice{{Index: 0, Message: openAIChatMessageResponse{Role: "assistant", Content: result.Message.Content}, FinishReason: finishReason}},
		Usage:   result.Usage,
	}
	writeOpenAIJSON(writer, http.StatusOK, response)
}

func validOpenAICheckpointID(value string) bool {
	if value == "" {
		return true
	}
	return len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}

func (handler *OpenAIHTTPHandler) supportsModel(model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	for _, candidate := range handler.modelIDs {
		if model == candidate {
			return true
		}
	}
	return false
}

func newestOpenAIInput(messages []openAIChatMessage) (string, string, error) {
	if len(messages) == 0 || len(messages) > MaxOpenAIChatMessages {
		return "", "", fmt.Errorf("messages must contain between 1 and %d items", MaxOpenAIChatMessages)
	}
	var newestUser json.RawMessage
	lastConversationRole := ""
	systems := make([]string, 0, 2)
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			content, err := decodeOpenAIContent(message.Content)
			if err != nil {
				return "", "", fmt.Errorf("%s message content: %w", message.Role, err)
			}
			if strings.TrimSpace(content) != "" {
				systems = append(systems, content)
			}
		case "user":
			newestUser = message.Content
			lastConversationRole = message.Role
		case "assistant", "tool":
			// Durable Bee history is authoritative. Prior client messages are
			// intentionally validated only by the bounded outer JSON envelope and
			// are not replayed into the harness.
			lastConversationRole = message.Role
		default:
			return "", "", fmt.Errorf("unsupported message role %q", message.Role)
		}
	}
	if lastConversationRole != "user" {
		return "", "", errors.New("the last non-system message must be a user message")
	}
	prompt, err := decodeOpenAIContent(newestUser)
	if err != nil {
		return "", "", fmt.Errorf("newest user message content: %w", err)
	}
	if strings.TrimSpace(prompt) == "" {
		return "", "", errors.New("a non-empty user message is required")
	}
	if len(prompt) > MaxChannelTextBytes {
		return "", "", fmt.Errorf("newest user message exceeds %d bytes", MaxChannelTextBytes)
	}
	systemPrompt := strings.Join(systems, "\n\n")
	if len(systemPrompt) > MaxChannelTextBytes {
		return "", "", fmt.Errorf("system prompt exceeds %d bytes", MaxChannelTextBytes)
	}
	return prompt, systemPrompt, nil
}

func decodeOpenAIContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("must be a string or an array of text parts")
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type != "text" && part.Type != "input_text" {
			return "", fmt.Errorf("content part type %q is not supported", part.Type)
		}
		result.WriteString(part.Text)
	}
	return result.String(), nil
}

func validateOpenAIConversationID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("X-Session-Id conversation ID is required")
	}
	if len(value) > MaxOpenAIConversationIDBytes {
		return "", fmt.Errorf("X-Session-Id exceeds %d bytes", MaxOpenAIConversationIDBytes)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("X-Session-Id is invalid")
	}
	return value, nil
}

func openAIRouteKey(conversationID string) SessionRouteKey {
	digest := sha256.Sum256([]byte(conversationID))
	key, _ := NewSessionRouteKey(ChannelID("openai-http"), AccountID("default"), ConversationID(hex.EncodeToString(digest[:])), "")
	return key
}

func (handler *OpenAIHTTPHandler) lookupSession(ctx context.Context, key SessionRouteKey) (SessionID, error) {
	sessionID, err := handler.routeBindings.Lookup(ctx, key)
	if errors.Is(err, ErrRouteBindingNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load conversation session mapping: %w", err)
	}
	if _, err := handler.handlers.Application.Store.Load(sessionID); err != nil {
		return "", fmt.Errorf("load mapped Bee session %q: %w", sessionID, err)
	}
	return sessionID, nil
}

func (handler *OpenAIHTTPHandler) admitConversation(conversationID string) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if _, active := handler.activeConversations[conversationID]; active {
		return errOpenAIConversationBusy
	}
	select {
	case handler.slots <- struct{}{}:
	default:
		return errors.New("server concurrency limit reached")
	}
	handler.activeConversations[conversationID] = struct{}{}
	return nil
}

func (handler *OpenAIHTTPHandler) claimSession(conversationID string, sessionID SessionID) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if activeConversation, active := handler.activeSessions[sessionID]; active && activeConversation != conversationID {
		return errOpenAISessionBusy
	}
	handler.activeSessions[sessionID] = conversationID
	return nil
}

func (handler *OpenAIHTTPHandler) releaseConversation(conversationID string, sessionID SessionID) {
	handler.mu.Lock()
	delete(handler.activeConversations, conversationID)
	if sessionID != "" && handler.activeSessions[sessionID] == conversationID {
		delete(handler.activeSessions, sessionID)
	}
	handler.mu.Unlock()
	<-handler.slots
}

type openAIHTTPStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIHTTPStreamChoice struct {
	Index        int                   `json:"index"`
	Delta        openAIHTTPStreamDelta `json:"delta"`
	FinishReason *string               `json:"finish_reason"`
}

type openAIHTTPStreamChunk struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"`
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []openAIHTTPStreamChoice `json:"choices"`
}

func (handler *OpenAIHTTPHandler) streamChatCompletion(ctx, transportCtx context.Context, writer http.ResponseWriter, request RunRequest, model, conversationID string, lifecycle bool, bindSession func(RunEvent) error) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "streaming is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan RunEvent)
	type runResult struct {
		result HarnessRunResult
		err    error
	}
	completed := make(chan runResult, 1)
	go func() {
		result, err := handler.handlers.Run(ctx, request, func(event RunEvent) error {
			if err := bindSession(event); err != nil {
				return err
			}
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		completed <- runResult{result: result, err: err}
	}()

	ticker := time.NewTicker(handler.heartbeatInterval)
	defer ticker.Stop()
	created := time.Now().Unix()
	completionID := "chatcmpl-bee"
	roleSent := false
	sawDelta := false
	writerOpen := true
	var transportDone <-chan struct{}
	if lifecycle {
		transportDone = transportCtx.Done()
	}
	runDone := ctx.Done()
	writeRaw := func(value string) error {
		if !writerOpen {
			return nil
		}
		if _, err := io.WriteString(writer, value); err != nil {
			if lifecycle {
				writerOpen = false
				return nil
			}
			return err
		}
		flusher.Flush()
		return nil
	}
	writeChunk := func(delta openAIHTTPStreamDelta, finishReason *string) error {
		if !writerOpen {
			return nil
		}
		chunk := openAIHTTPStreamChunk{ID: completionID, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []openAIHTTPStreamChoice{{Index: 0, Delta: delta, FinishReason: finishReason}}}
		if err := writeSSEJSON(writer, chunk); err != nil {
			if lifecycle {
				writerOpen = false
				return nil
			}
			return err
		}
		flusher.Flush()
		return nil
	}
	for {
		select {
		case <-runDone:
			if lifecycle {
				handler.runLifecycle.ensureFailed(conversationID)
			}
			if writerOpen {
				_ = writeSSEJSON(writer, openAIErrorEnvelope{Error: openAIError{Message: ctx.Err().Error(), Type: "server_error"}})
				_ = writeRaw("data: [DONE]\n\n")
			}
			if !lifecycle {
				return
			}
			writerOpen = false
			runDone = nil
		case <-transportDone:
			writerOpen = false
			transportDone = nil
		case <-ticker.C:
			if err := writeRaw(": ping\n\n"); err != nil {
				return
			}
		case event := <-events:
			if event.RunID != "" {
				completionID = openAICompletionID(event.RunID)
			}
			if lifecycle {
				if projected, ok := handler.runLifecycle.publish(conversationID, event); ok && writerOpen {
					if err := writeOpenAIWebRunComment(writer, projected); err != nil {
						writerOpen = false
					} else {
						flusher.Flush()
					}
				}
			}
			if !roleSent && event.Type == RunStart {
				if err := writeChunk(openAIHTTPStreamDelta{Role: "assistant"}, nil); err != nil {
					return
				}
				roleSent = true
			}
			if event.Type == RunAssistantDelta && event.Delta != "" {
				if !roleSent {
					if err := writeChunk(openAIHTTPStreamDelta{Role: "assistant"}, nil); err != nil {
						return
					}
					roleSent = true
				}
				if err := writeChunk(openAIHTTPStreamDelta{Content: event.Delta}, nil); err != nil {
					return
				}
				sawDelta = true
			}
		case outcome := <-completed:
			if outcome.result.RunID != "" {
				completionID = openAICompletionID(outcome.result.RunID)
			}
			if outcome.err != nil {
				if lifecycle {
					handler.runLifecycle.ensureFailed(conversationID)
				}
				if writerOpen {
					_ = writeSSEJSON(writer, openAIErrorEnvelope{Error: openAIError{Message: outcome.err.Error(), Type: "server_error"}})
					_ = writeRaw("data: [DONE]\n\n")
				}
				return
			}
			if !roleSent {
				if err := writeChunk(openAIHTTPStreamDelta{Role: "assistant"}, nil); err != nil {
					return
				}
				roleSent = true
			}
			if !sawDelta && outcome.result.Message.Content != "" {
				if err := writeChunk(openAIHTTPStreamDelta{Content: outcome.result.Message.Content}, nil); err != nil {
					return
				}
			}
			finish := "stop"
			if outcome.result.Response != nil && strings.TrimSpace(outcome.result.Response.FinishReason) != "" {
				finish = outcome.result.Response.FinishReason
			}
			if err := writeChunk(openAIHTTPStreamDelta{}, &finish); err != nil {
				return
			}
			_ = writeRaw("data: [DONE]\n\n")
			return
		}
	}
}

func openAICompletionID(runID string) string {
	if strings.TrimSpace(runID) == "" {
		runID = newStableID()
	}
	return "chatcmpl-" + runID
}

func writeSSEJSON(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, "data: "); err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(writer, "\n\n")
	return err
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

func writeOpenAIError(writer http.ResponseWriter, status int, typ, message string) {
	writeOpenAIJSON(writer, status, openAIErrorEnvelope{Error: openAIError{Message: message, Type: typ}})
}

func writeOpenAIJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func methodNotAllowed(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeOpenAIError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
}

// ServeOpenAIHTTP binds the configured address and gracefully drains on
// context cancellation. No write deadline is set because agent/tool turns can
// legitimately be long-lived; requestTimeout and SSE heartbeats bound them.
func ServeOpenAIHTTP(ctx context.Context, addr string, handler http.Handler) error {
	if ctx == nil {
		return errors.New("OpenAI HTTP server context is required")
	}
	if handler == nil {
		return errors.New("OpenAI HTTP handler is required")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down OpenAI HTTP server: %w", err)
		}
		err := <-served
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
