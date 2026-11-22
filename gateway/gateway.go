package gateway

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seth-el/bee"
)

const (
	DefaultListenAddr   = "127.0.0.1:18790"
	DefaultSessionTitle = "New conversation"
	MaxJSONBodyBytes    = 64 << 10
	MaxRunRecords       = 256
	// MaxRunEvents bounds ordinary per-run events. The cursor-aware replay
	// contract reports a gap when this bounded buffer rolls over.
	MaxRunEvents = 1024
	// MaxRunTerminalEvents reserves one bounded slot for each terminal type.
	MaxRunTerminalEvents = 2
	// MaxRunRecordEvents is the total retained event cap, including terminals.
	MaxRunRecordEvents = MaxRunEvents + MaxRunTerminalEvents
	MaxRunEventBytes   = 128 << 10
	SSEKeepalive       = 15 * time.Second
	SSEMaxDuration     = 10 * time.Minute
	// MaxConfiguredAvatarBytes bounds the two optional operator-supplied image
	// files retained in memory and served through fixed routes.
	MaxConfiguredAvatarBytes = 2 << 20
)

var ErrRunCapacity = errors.New("gateway run capacity is full")

type configuredAvatar struct {
	data        []byte
	contentType string
}

var supportedAvatarContentTypes = map[string]struct{}{
	"image/gif":  {},
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

//go:embed index.html
var indexPage []byte

//go:embed assets/bee-mascot.png
var beeMascotAsset []byte

//go:embed assets/bee-mascot-home.png
var beeMascotHomeAsset []byte

//go:embed assets/bee-mascot.svg
var beeMascotSVGAsset []byte

// Config describes the web-only Bee gateway composition.
type Config struct {
	BeeConfig    bee.Config
	Profile      string
	ListenAddr   string
	SessionStore *bee.SessionStore
	ScopeRoot    string
	WriteRoot    string
	FullMachine  bool
	Policy       bee.AssistantPolicyMode
}

// Service owns one general assistant and one HTTP server. Telegram is a
// separate future adapter and is intentionally not part of this lifecycle.
type Service struct {
	app        *bee.Application
	store      *bee.SessionStore
	server     *http.Server
	listen     net.Listener
	listenAddr string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu            sync.Mutex
	started       bool
	ready         bool
	stopped       bool
	serveErr      error
	runs          map[string]*runRecord
	runSlots      chan struct{}
	notifications *bee.NotificationInboxStore
	userAvatar    *configuredAvatar
	beeAvatar     *configuredAvatar
}

// New composes exactly one general assistant and prepares the local gateway.
func New(config Config) (*Service, error) {
	if config.ListenAddr == "" {
		config.ListenAddr = DefaultListenAddr
	}
	if err := validateListenAddr(config.ListenAddr); err != nil {
		return nil, err
	}

	store := config.SessionStore
	var err error
	if store == nil {
		store, err = bee.NewDefaultSessionStore()
		if err != nil {
			return nil, fmt.Errorf("gateway session store: %w", err)
		}
	}
	app, err := ComposeAssistant(config.BeeConfig, config.Profile, store, bee.AssistantCompositionOptions{
		ScopeRoot:         config.ScopeRoot,
		WriteRoot:         config.WriteRoot,
		FullMachine:       config.FullMachine,
		Policy:            config.Policy,
		EnableMediaSearch: config.BeeConfig.Web.MediaSearchEnabled,
	})
	if err != nil {
		return nil, err
	}
	service, err := newService(app, store, config.ListenAddr)
	if err != nil {
		_ = app.Close(context.Background())
		return nil, err
	}
	return service, nil
}

// NewForApplication creates a gateway around an already-composed application.
// It is useful for focused tests and for embedders that must control
// composition. Production callers should normally use New.
func NewForApplication(app *bee.Application, listenAddr string) (*Service, error) {
	if app == nil {
		return nil, errors.New("gateway application is required")
	}
	if app.Store == nil {
		return nil, errors.New("gateway application session store is required")
	}
	if app.NotificationStore == nil {
		jobStorePath := ""
		if app.JobStore != nil {
			jobStorePath = app.JobStore.Path()
		}
		if jobStorePath == "" {
			jobStorePath = filepath.Join(app.Store.Root(), "jobs.json")
		}
		notificationStore, err := bee.NewNotificationInboxStore(bee.NotificationStorePathForJobStore(jobStorePath))
		if err != nil {
			return nil, fmt.Errorf("gateway notifications: %w", err)
		}
		app.NotificationStore = notificationStore
	}
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	if err := validateListenAddr(listenAddr); err != nil {
		return nil, err
	}
	return newService(app, app.Store, listenAddr)
}

func newService(app *bee.Application, store *bee.SessionStore, listenAddr string) (*Service, error) {
	if app == nil || store == nil {
		return nil, errors.New("gateway application and session store are required")
	}
	if err := app.Config.Web.Validate(); err != nil {
		return nil, fmt.Errorf("gateway web config: %w", err)
	}
	userAvatar, err := loadConfiguredAvatar("user_avatar_file", app.Config.Web.UserAvatarFile)
	if err != nil {
		return nil, err
	}
	beeAvatar, err := loadConfiguredAvatar("bee_avatar_file", app.Config.Web.BeeAvatarFile)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{app: app, store: store, notifications: app.NotificationStore, listenAddr: listenAddr, userAvatar: userAvatar, beeAvatar: beeAvatar, ctx: ctx, cancel: cancel, runs: make(map[string]*runRecord), runSlots: make(chan struct{}, app.Config.Web.EffectiveMaxConcurrentRuns())}
	var policy bee.GeneralAssistantPolicy
	if app.Executor != nil {
		policy, _ = app.Executor.Policy.(bee.GeneralAssistantPolicy)
	}
	if policy.Mode != bee.AssistantPolicyLocked && app.Executor != nil {
		// Trusted mode still has two deliberately approval-only activation tools:
		// skill_enable and plugin_enable. Installing the broker here lets the web
		// UI review those requests without turning every ordinary mutation into a
		// prompted-policy approval.
		if err := app.EnableApprovalBroker(); err != nil {
			cancel()
			return nil, fmt.Errorf("gateway approval broker: %w", err)
		}
	}
	service.server = &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	return service, nil
}

// ComposeAssistant is the shared general-assistant composition helper used by
// the web gateway and the separate compatibility Telegram executable.
func ComposeAssistant(config bee.Config, profile string, store *bee.SessionStore, options bee.AssistantCompositionOptions) (*bee.Application, error) {
	options.Store = store
	return bee.ComposeGeneralAssistant(config, profile, options)
}

// App returns the single composed Application.
func (service *Service) App() *bee.Application {
	if service == nil {
		return nil
	}
	return service.app
}

// Addr returns the bound address after Start, or the configured address before
// Start.
func (service *Service) Addr() string {
	if service == nil {
		return ""
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.listen != nil {
		return service.listen.Addr().String()
	}
	return service.listenAddr
}

// Start binds the configured HTTP listener and starts extensions. It is
// idempotent after a successful start. The default remains loopback-only;
// callers may explicitly opt into an unspecified host such as 0.0.0.0.
func (service *Service) Start(ctx context.Context) error {
	if service == nil {
		return errors.New("gateway service is nil")
	}
	if ctx == nil {
		return errors.New("gateway start context is nil")
	}
	service.mu.Lock()
	if service.started {
		service.mu.Unlock()
		return nil
	}
	if service.stopped {
		service.mu.Unlock()
		return errors.New("gateway service is stopped")
	}
	service.mu.Unlock()
	if err := service.app.StartExtensions(ctx); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", service.listenAddr)
	if err != nil {
		_ = service.app.CloseExtensions(context.Background())
		return fmt.Errorf("gateway listen: %w", err)
	}
	service.mu.Lock()
	service.listen = listener
	service.started = true
	service.ready = true
	service.mu.Unlock()

	service.wg.Add(1)
	go func() {
		defer service.wg.Done()
		err := service.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			service.mu.Lock()
			service.serveErr = err
			service.ready = false
			service.mu.Unlock()
		}
	}()
	return nil
}

// Stop gracefully shuts down HTTP, active runs, and configured extensions. It
// is safe to call more than once.
func (service *Service) Stop(ctx context.Context) error {
	if service == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("gateway stop context is nil")
	}
	service.mu.Lock()
	if service.stopped {
		service.mu.Unlock()
		return nil
	}
	service.stopped = true
	service.ready = false
	server := service.server
	service.mu.Unlock()
	service.cancel()
	var first error
	if server != nil {
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			first = err
		}
	}
	service.wg.Wait()
	if err := service.app.Close(ctx); err != nil && first == nil {
		first = err
	}
	return first
}

// ServeError reports a terminal listener error, if one occurred.
func (service *Service) ServeError() error {
	if service == nil {
		return errors.New("gateway service is nil")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.serveErr
}

// Handler returns the bounded local HTTP API handler.
func (service *Service) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isRunEventsPath(request.URL.Path) || isNotificationEventsPath(request.URL.Path) {
			// Preserve the concrete writer's streaming capability. Wrapping an
			// arbitrary writer in a type that always advertises http.Flusher can
			// make a non-streaming proxy look safe to the SSE handler.
			service.handleHTTP(writer, request)
			return
		}
		started := time.Now()
		delivery := &deliveryWriter{dst: writer, request: request}
		service.handleHTTP(delivery, request)
		delivery.finish()
		status := delivery.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("http method=%s route=%s status=%d bytes=%d duration_ms=%d", request.Method, routeLabel(request.URL.Path), status, delivery.bytesWritten, time.Since(started).Milliseconds())
	})
}

func (service *Service) handleHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/assets/user-avatar" {
		service.serveConfiguredAvatar(writer, request, service.userAvatar)
		return
	}
	if request.URL.Path == "/assets/bee-avatar" {
		service.serveConfiguredAvatar(writer, request, service.beeAvatar)
		return
	}
	if request.URL.Path == "/assets/bee-mascot.svg" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writer.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = writer.Write(beeMascotSVGAsset)
		return
	}
	if request.URL.Path == "/assets/bee-mascot.png" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = writer.Write(beeMascotAsset)
		return
	}
	if request.URL.Path == "/assets/bee-mascot-home.png" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writer.Header().Set("Content-Type", "image/png")
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = writer.Write(beeMascotHomeAsset)
		return
	}
	if request.URL.Path == "/" || request.URL.Path == "/index.html" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Cache-Control", "private, no-cache")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		userAvatarSource := ""
		if service.userAvatar != nil {
			userAvatarSource = "/assets/user-avatar"
		}
		beeAvatarSource := ""
		if service.beeAvatar != nil {
			beeAvatarSource = "/assets/bee-avatar"
		}
		_, _ = writer.Write(renderIndexPage(userAvatarSource, beeAvatarSource, service.app.Config.Web.EffectiveTimeZone()))
		return
	}
	if request.URL.Path == "/healthz" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if request.URL.Path == "/readyz" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.mu.Lock()
		ready := service.ready
		service.mu.Unlock()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(writer, status, map[string]any{"ready": ready})
		return
	}
	if request.URL.Path == "/api/metrics" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writeJSON(writer, http.StatusOK, service.app.Metrics.Snapshot())
		return
	}
	if request.URL.Path == "/api/approvals" {
		service.handleApprovals(writer, request)
		return
	}
	if request.URL.Path == "/api/interaction-policy" {
		service.handleInteractionPolicy(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/approvals/") {
		service.handleApprovalDecision(writer, request, strings.TrimPrefix(request.URL.Path, "/api/approvals/"))
		return
	}
	if request.URL.Path == "/api/notifications" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.handleListNotifications(writer, request)
		return
	}
	if request.URL.Path == "/api/notifications/events" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.handleNotificationEvents(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/notifications/") {
		service.handleNotificationPath(writer, request)
		return
	}
	if request.URL.Path == "/api/sessions" {
		switch request.Method {
		case http.MethodGet:
			service.handleListSessions(writer, request)
		case http.MethodPost:
			service.handleCreateSession(writer, request)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return
	}
	if request.URL.Path == "/api/sessions/search" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.handleSearchSessions(writer, request)
		return
	}
	if request.URL.Path == "/api/projects" {
		switch request.Method {
		case http.MethodGet:
			service.handleListProjects(writer)
		case http.MethodPost:
			service.handleCreateProject(writer, request)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/projects/") {
		service.handleProjectPath(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/sessions/") {
		service.handleSessionPath(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/runs/") {
		service.handleRunPath(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/artifacts/") {
		service.handleArtifactPath(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/attachments/") {
		service.handleAttachmentPath(writer, request)
		return
	}
	notFound(writer)
}

func renderIndexPage(userAvatarSource, beeAvatarSource, timeZone string) []byte {
	payload, err := json.Marshal(struct {
		UserAvatar string `json:"userAvatar,omitempty"`
		BeeAvatar  string `json:"beeAvatar,omitempty"`
		TimeZone   string `json:"timeZone"`
	}{UserAvatar: userAvatarSource, BeeAvatar: beeAvatarSource, TimeZone: timeZone})
	if err != nil {
		payload = []byte(`{"userAvatar":"","beeAvatar":"","timeZone":"UTC"}`)
	}
	return bytes.Replace(indexPage, []byte(`window.__BEE_UI_CONFIG__ = null;`), append([]byte(`window.__BEE_UI_CONFIG__ = `), append(payload, ';')...), 1)
}

func loadConfiguredAvatar(name, path string) (*configuredAvatar, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gateway %s: %w", name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("gateway %s stat: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("gateway %s must reference a regular file", name)
	}
	if info.Size() <= 0 || info.Size() > MaxConfiguredAvatarBytes {
		return nil, fmt.Errorf("gateway %s must be between 1 and %d bytes", name, MaxConfiguredAvatarBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxConfiguredAvatarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gateway %s read: %w", name, err)
	}
	if len(data) == 0 || len(data) > MaxConfiguredAvatarBytes {
		return nil, fmt.Errorf("gateway %s must be between 1 and %d bytes", name, MaxConfiguredAvatarBytes)
	}
	contentType := http.DetectContentType(data)
	if _, ok := supportedAvatarContentTypes[contentType]; !ok {
		return nil, fmt.Errorf("gateway %s has unsupported image MIME %q", name, contentType)
	}
	return &configuredAvatar{data: data, contentType: contentType}, nil
}

func (service *Service) serveConfiguredAvatar(writer http.ResponseWriter, request *http.Request, avatar *configuredAvatar) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if avatar == nil {
		notFound(writer)
		return
	}
	writer.Header().Set("Content-Type", avatar.contentType)
	writer.Header().Set("Cache-Control", "private, no-cache")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = writer.Write(avatar.data)
}

func (service *Service) handleListSessions(writer http.ResponseWriter, request ...*http.Request) {
	started := time.Now()
	list, err := service.app.ListSessions()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if len(request) > 0 && request[0] != nil {
		query := request[0].URL.Query()
		projectID := strings.TrimSpace(query.Get("project_id"))
		unassigned := query.Get("unassigned") == "true"
		if projectID != "" && !unassigned {
			project, projectErr := service.store.GetProject(bee.ProjectID(projectID))
			if projectErr != nil {
				status := http.StatusBadRequest
				if errors.Is(projectErr, bee.ErrProjectNotFound) {
					status = http.StatusNotFound
				}
				writeError(writer, status, projectErr)
				return
			}
			_ = project
		}
		filtered := list[:0]
		for _, session := range list {
			if unassigned && session.ProjectID != "" {
				continue
			}
			if projectID != "" && !unassigned && string(session.ProjectID) != projectID {
				continue
			}
			filtered = append(filtered, session)
		}
		list = filtered
	}
	setServerTiming(writer, "sessions", started)
	writeJSON(writer, http.StatusOK, list)
}

func (service *Service) handleListNotifications(writer http.ResponseWriter, request *http.Request) {
	if service.notifications == nil {
		writeError(writer, http.StatusServiceUnavailable, errors.New("notification inbox is unavailable"))
		return
	}
	limit := bee.NotificationMaxListLimit
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > bee.NotificationMaxListLimit {
			writeError(writer, http.StatusBadRequest, errors.New("notification limit is invalid"))
			return
		}
		limit = parsed
	}
	var before uint64
	if raw := strings.TrimSpace(request.URL.Query().Get("before")); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 {
			writeError(writer, http.StatusBadRequest, errors.New("notification cursor is invalid"))
			return
		}
		before = parsed
	}
	result, err := service.notifications.ListBefore(request.Context(), limit, before)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleNotificationPath(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/notifications/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "read" {
		notFound(writer)
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if service.notifications == nil {
		writeError(writer, http.StatusServiceUnavailable, errors.New("notification inbox is unavailable"))
		return
	}
	item, err := service.notifications.MarkRead(request.Context(), parts[0])
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrNotificationNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	writeJSON(writer, http.StatusOK, bee.NotificationItemView{ID: item.ID, Kind: item.Kind, JobID: item.JobID, State: item.State, Summary: item.Summary, CreatedAt: item.CreatedAt, ReadAt: item.ReadAt, Sequence: item.Sequence})
}

func (service *Service) handleNotificationEvents(writer http.ResponseWriter, request *http.Request) {
	if service.notifications == nil {
		writeError(writer, http.StatusServiceUnavailable, errors.New("notification inbox is unavailable"))
		return
	}
	current, err := service.notifications.List(request.Context(), bee.NotificationMaxListLimit)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	if _, err := io.WriteString(writer, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()
	writeNotificationUpdate := func(result bee.NotificationListResult) bool {
		data, marshalErr := json.Marshal(map[string]any{"type": "notifications", "sequence": result.Sequence, "unread_count": result.UnreadCount, "unread_truncated": result.UnreadTruncated})
		if marshalErr != nil {
			return false
		}
		payload := fmt.Sprintf("id: %d\ndata: %s\n\n", result.Sequence, data)
		n, writeErr := io.WriteString(writer, payload)
		if writeErr != nil || n != len(payload) {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeNotificationUpdate(current) {
		return
	}
	deadline := time.NewTimer(SSEMaxDuration)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	keepalive := time.NewTicker(SSEKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-service.ctx.Done():
			return
		case <-deadline.C:
			return
		case <-keepalive.C:
			if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			next, listErr := service.notifications.List(request.Context(), bee.NotificationMaxListLimit)
			if listErr != nil {
				return
			}
			if next.Sequence != current.Sequence || next.UnreadCount != current.UnreadCount || next.UnreadTruncated != current.UnreadTruncated {
				if !writeNotificationUpdate(next) {
					return
				}
				current = next
			}
		}
	}
}

func (service *Service) handleSearchSessions(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	query := request.URL.Query()
	projectID := strings.TrimSpace(query.Get("project_id"))
	unassigned := query.Get("unassigned") == "true"
	if projectID != "" && !unassigned {
		if _, err := service.store.GetProject(bee.ProjectID(projectID)); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bee.ErrProjectNotFound) {
				status = http.StatusNotFound
			}
			writeError(writer, status, err)
			return
		}
	}
	limit := 0
	if rawLimit := strings.TrimSpace(query.Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 {
			writeError(writer, http.StatusBadRequest, errors.New("limit must be a positive integer"))
			return
		}
		limit = parsed
	}
	results, err := service.store.SearchSessions(bee.SessionSearchOptions{Query: query.Get("q"), ProjectID: bee.ProjectID(projectID), Unassigned: unassigned, Limit: limit})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	setServerTiming(writer, "search", started)
	writeJSON(writer, http.StatusOK, results)
}

type sessionRequest struct {
	Title       string          `json:"title,omitempty"`
	ProjectRoot string          `json:"project_root,omitempty"`
	ProjectID   json.RawMessage `json:"project_id,omitempty"`
}

type sessionUpdateRequest struct {
	Title       *string         `json:"title,omitempty"`
	Pinned      *bool           `json:"pinned,omitempty"`
	ProjectRoot *string         `json:"project_root,omitempty"`
	ProjectID   json.RawMessage `json:"project_id,omitempty"`
}

func (service *Service) handleCreateSession(writer http.ResponseWriter, request *http.Request) {
	var input sessionRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	if len(input.Title) > 512 || len(input.ProjectRoot) > 4096 {
		writeError(writer, http.StatusBadRequest, errors.New("session fields are too large"))
		return
	}
	if strings.TrimSpace(input.Title) == "" {
		input.Title = DefaultSessionTitle
	}
	projectInput, err := decodeProjectIDInput(input.ProjectID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	projectID, projectRoot, err := service.resolveProjectInput(projectInput, input.ProjectRoot)
	if err != nil {
		writeProjectError(writer, err)
		return
	}
	session, err := service.store.Create(bee.SessionMetadata{Title: input.Title, ProjectID: projectID, ProjectRoot: projectRoot})
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusCreated, session.Metadata)
}

func decodeProjectIDInput(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if len(raw) == 0 {
			return nil, nil
		}
		value := ""
		return &value, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("project_id must be a string or null")
	}
	return &value, nil
}

func (service *Service) resolveProjectInput(projectID *string, projectRoot string) (bee.ProjectID, string, error) {
	if projectID == nil || strings.TrimSpace(*projectID) == "" {
		return "", projectRoot, nil
	}
	project, err := service.store.GetProject(bee.ProjectID(strings.TrimSpace(*projectID)))
	if err != nil {
		return "", "", err
	}
	if project.Completed {
		return "", "", bee.ErrProjectCompleted
	}
	if strings.TrimSpace(projectRoot) == "" {
		projectRoot = project.RootPath
	}
	return project.ID, projectRoot, nil
}

func writeProjectError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, bee.ErrProjectNotFound):
		status = http.StatusNotFound
	case errors.Is(err, bee.ErrProjectConflict), errors.Is(err, bee.ErrProjectInUse), errors.Is(err, bee.ErrProjectCompleted):
		status = http.StatusConflict
	}
	writeError(writer, status, err)
}

func (service *Service) handleListProjects(writer http.ResponseWriter) {
	projects, err := service.store.ListProjects()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, projects)
}

type projectRequest struct {
	ID              string  `json:"id,omitempty"`
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	RootPath        *string `json:"root_path,omitempty"`
	CreateDirectory bool    `json:"create_directory,omitempty"`
	Completed       *bool   `json:"completed,omitempty"`
}

func (service *Service) handleCreateProject(writer http.ResponseWriter, request *http.Request) {
	var input projectRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	if input.Name == nil {
		writeError(writer, http.StatusBadRequest, errors.New("project name is required"))
		return
	}
	project := bee.Project{Name: *input.Name, ID: bee.ProjectID(strings.TrimSpace(input.ID))}
	if input.Description != nil {
		project.Description = *input.Description
	}
	if input.RootPath != nil {
		project.RootPath = *input.RootPath
	}
	if input.Completed != nil {
		project.Completed = *input.Completed
	}
	created, err := service.store.CreateProjectWithOptions(project, bee.ProjectCreateOptions{CreateDirectory: input.CreateDirectory})
	if err != nil {
		writeProjectError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, created)
}

func (service *Service) handleProjectPath(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/projects/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		notFound(writer)
		return
	}
	id := bee.ProjectID(parts[0])
	switch request.Method {
	case http.MethodGet:
		project, err := service.store.GetProject(id)
		if err != nil {
			writeProjectError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, project)
	case http.MethodPatch, http.MethodPut:
		var input projectRequest
		if err := decodeJSON(writer, request, &input); err != nil {
			return
		}
		updated, err := service.store.UpdateProject(id, bee.ProjectPatch{Name: input.Name, Description: input.Description, RootPath: input.RootPath, Completed: input.Completed})
		if err != nil {
			writeProjectError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, updated)
	case http.MethodDelete:
		if err := service.store.DeleteProject(id); err != nil {
			writeProjectError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"id": id, "deleted": true})
	default:
		methodNotAllowed(writer, http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete)
	}
}

func (service *Service) handleSessionPath(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/sessions/"), "/")
	if len(parts) == 1 && parts[0] != "" && (request.Method == http.MethodPatch || request.Method == http.MethodPut) {
		service.handleUpdateSession(writer, request, bee.SessionID(parts[0]))
		return
	}
	if len(parts) == 1 && parts[0] != "" && request.Method == http.MethodGet {
		service.handleSessionHistory(writer, request, bee.SessionID(parts[0]))
		return
	}
	if len(parts) == 1 && parts[0] != "" && request.Method == http.MethodDelete {
		service.handleDeleteSession(writer, bee.SessionID(parts[0]))
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "history" && request.Method == http.MethodGet {
		service.handleSessionHistory(writer, request, bee.SessionID(parts[0]))
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "artifacts" && request.Method == http.MethodGet {
		service.handleSessionArtifacts(writer, bee.SessionID(parts[0]))
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "attachments" {
		if request.Method == http.MethodGet {
			service.handleSessionAttachments(writer, bee.SessionID(parts[0]))
		} else if request.Method == http.MethodPost {
			service.handleUploadAttachment(writer, request, bee.SessionID(parts[0]))
		} else {
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return
	}
	if len(parts) == 3 && parts[0] != "" && parts[1] == "media" && parts[2] == "select" {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		service.handleMediaSelection(writer, request, bee.SessionID(parts[0]))
		return
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] != "messages" || request.Method != http.MethodPost {
		if len(parts) == 2 && (parts[1] == "messages" || parts[1] == "history" || parts[1] == "artifacts") {
			if parts[1] == "messages" {
				methodNotAllowed(writer, http.MethodPost)
			} else {
				methodNotAllowed(writer, http.MethodGet)
			}
		} else if len(parts) == 1 && parts[0] != "" {
			methodNotAllowed(writer, http.MethodGet, http.MethodDelete)
		} else {
			notFound(writer)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "messages" {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
	}
	sessionID := bee.SessionID(parts[0])
	session, err := service.app.Resume(sessionID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	var input messageRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	if len(input.AttachmentIDs) > bee.MaxAttachmentCount {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("at most %d attachments may be sent", bee.MaxAttachmentCount))
		return
	}
	if len(input.Prompt) == 0 && len(input.AttachmentIDs) > 0 {
		input.Prompt = "Please inspect the attached files."
	}
	if len(input.Prompt) == 0 || len(input.Prompt) > bee.MaxChannelTextBytes {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("prompt must be between 1 and %d bytes", bee.MaxChannelTextBytes))
		return
	}
	service.maybeNameSession(session, input.Prompt)
	runID, err := service.startRun(bee.RunRequest{SessionID: sessionID, Prompt: input.Prompt, Model: input.Model, AttachmentIDs: append([]string(nil), input.AttachmentIDs...), CheckpointID: input.CheckpointID, MediaEnabled: true, AutoRetrieveTop: explicitMediaAutoRetrieveIntent(input.Prompt)})
	if err != nil {
		writeRunStartError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_id": runID, "session_id": sessionID, "status": "running"})
}

// explicitMediaAutoRetrieveIntent is intentionally web-gateway-owned. It
// requires an explicit display/attachment verb and a media noun, while broad
// find/search/options/list requests stay in chooser mode.
func explicitMediaAutoRetrieveIntent(prompt string) bool {
	words := strings.Fields(strings.ToLower(prompt))
	if len(words) == 0 {
		return false
	}
	var hasAction, hasStrongAction, hasMedia, hasBroad bool
	for _, raw := range words {
		word := strings.Trim(raw, "\t\n\r .,!?;:()[]{}<>\"'")
		switch word {
		case "show", "display":
			hasAction = true
		case "attach", "attached", "send", "retrieve", "retrieved", "fetch", "fetched", "grab", "download":
			hasAction = true
			hasStrongAction = true
		case "image", "images", "photo", "photos", "picture", "pictures", "video", "videos", "clip", "clips", "media":
			hasMedia = true
		case "find", "search", "searched", "options", "option", "list", "listing":
			hasBroad = true
		}
	}
	if !hasAction || !hasMedia {
		return false
	}
	if hasBroad && !hasStrongAction {
		return false
	}
	return true
}

type mediaSelectionRequest struct {
	CandidateID string `json:"candidate_id"`
	Token       string `json:"token"`
}

func (service *Service) handleMediaSelection(writer http.ResponseWriter, request *http.Request, sessionID bee.SessionID) {
	if service.app.MediaSearch == nil {
		writeError(writer, http.StatusNotFound, errors.New("media search is not enabled"))
		return
	}
	if _, err := service.app.Resume(sessionID); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	var input mediaSelectionRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	selection, err := service.app.MediaSearch.Resolve(sessionID, input.CandidateID, input.Token)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	runID, err := service.startRun(bee.RunRequest{SessionID: sessionID, Prompt: "/" + selection.Kind + " " + selection.URL, MediaEnabled: true})
	if err != nil {
		writeRunStartError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_id": runID, "session_id": sessionID, "status": "running"})
}

func (service *Service) handleUpdateSession(writer http.ResponseWriter, request *http.Request, id bee.SessionID) {
	var input sessionUpdateRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	session, err := service.store.Load(id)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	if input.Title == nil && input.Pinned == nil && input.ProjectRoot == nil && len(input.ProjectID) == 0 {
		writeError(writer, http.StatusBadRequest, errors.New("at least one session field is required"))
		return
	}
	updated := session.Metadata
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			writeError(writer, http.StatusBadRequest, errors.New("title must not be empty"))
			return
		}
		if len(title) > 512 {
			writeError(writer, http.StatusBadRequest, errors.New("title must be at most 512 bytes"))
			return
		}
		updated.Title = title
	}
	if input.Pinned != nil {
		updated.Pinned = *input.Pinned
	}
	if input.ProjectRoot != nil {
		if len(*input.ProjectRoot) > 4096 {
			writeError(writer, http.StatusBadRequest, errors.New("project_root must be at most 4096 bytes"))
			return
		}
		updated.ProjectRoot = *input.ProjectRoot
	}
	if len(input.ProjectID) > 0 {
		projectInput, decodeErr := decodeProjectIDInput(input.ProjectID)
		if decodeErr != nil {
			writeError(writer, http.StatusBadRequest, decodeErr)
			return
		}
		projectID := ""
		if projectInput != nil {
			projectID = strings.TrimSpace(*projectInput)
		}
		if projectID != "" {
			project, projectErr := service.store.GetProject(bee.ProjectID(projectID))
			if projectErr != nil {
				writeProjectError(writer, projectErr)
				return
			}
			if project.Completed {
				writeProjectError(writer, bee.ErrProjectCompleted)
				return
			}
			updated.ProjectID = project.ID
			if input.ProjectRoot == nil || strings.TrimSpace(updated.ProjectRoot) == "" {
				updated.ProjectRoot = project.RootPath
			}
		} else {
			updated.ProjectID = ""
		}
	}
	if err := session.ReplaceMetadata(updated); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, session.Metadata)
}

type sessionHistoryResponse struct {
	Metadata    bee.SessionMetadata   `json:"metadata"`
	Messages    []bee.Message         `json:"messages"`
	Artifacts   []bee.Artifact        `json:"artifacts,omitempty"`
	Attachments []bee.Attachment      `json:"attachments,omitempty"`
	Timeline    []sessionTimelineItem `json:"timeline,omitempty"`
	EventCount  int                   `json:"event_count"`
}

type sessionTimelineResponse struct {
	SessionID   bee.SessionID         `json:"session_id"`
	Metadata    bee.SessionMetadata   `json:"metadata"`
	Timeline    []sessionTimelineItem `json:"timeline"`
	Attachments []bee.Attachment      `json:"attachments,omitempty"`
	EventCount  int                   `json:"event_count"`
	Limit       int                   `json:"limit"`
	Before      string                `json:"before,omitempty"`
	NextBefore  string                `json:"next_before,omitempty"`
	HasMore     bool                  `json:"has_more"`
}

const (
	maxSessionTimelineItems     = 1024
	defaultSessionHistoryWindow = 64
	maxSessionHistoryWindow     = 128
)

// sessionTimelineItem is an explicit display projection of append-only
// message/artifact/reasoning events. Ordinary messages/tools retain their
// existing display projection; private reasoning remains faithful to its
// bounded durable source. The legacy arrays remain in the response for older
// clients.
type sessionTimelineItem struct {
	Seq        uint64                `json:"seq,omitempty"`
	ID         string                `json:"id,omitempty"`
	At         time.Time             `json:"at,omitempty"`
	Type       string                `json:"type"`
	Message    *bee.Message          `json:"message,omitempty"`
	ToolCall   *bee.ToolCall         `json:"tool_call,omitempty"`
	ToolResult *bee.ToolResultEvent  `json:"tool_result,omitempty"`
	Artifact   *bee.Artifact         `json:"artifact,omitempty"`
	Reasoning  *sessionReasoningItem `json:"reasoning,omitempty"`
}

// sessionReasoningItem is an explicit private-owner timeline projection. It
// is never folded into ordinary messages or search results and preserves the
// bounded durable provider reasoning content verbatim.
type sessionReasoningItem struct {
	Seq           uint64           `json:"seq,omitempty"`
	ID            string           `json:"id,omitempty"`
	At            time.Time        `json:"at,omitempty"`
	RunID         string           `json:"run_id,omitempty"`
	Provider      string           `json:"provider,omitempty"`
	Profile       string           `json:"profile,omitempty"`
	Role          bee.ProviderRole `json:"role,omitempty"`
	Model         string           `json:"model,omitempty"`
	Step          int              `json:"step"`
	Content       string           `json:"content,omitempty"`
	Complete      bool             `json:"complete"`
	Incomplete    bool             `json:"incomplete,omitempty"`
	FinishReason  string           `json:"finish_reason,omitempty"`
	ErrorClass    string           `json:"error_class,omitempty"`
	Truncated     bool             `json:"truncated,omitempty"`
	OriginalBytes int              `json:"original_bytes,omitempty"`
}

func projectReasoning(event bee.Event) (sessionReasoningItem, bool) {
	payload, err := bee.DecodeReasoningEvent(event)
	if err != nil {
		return sessionReasoningItem{}, false
	}
	return sessionReasoningItem{Seq: event.Seq, ID: event.ID, At: event.At, RunID: payload.RunID, Provider: payload.Provider, Profile: payload.Profile, Role: payload.Role, Model: payload.Model, Step: payload.Step, Content: payload.Content, Complete: payload.Complete, Incomplete: payload.Incomplete, FinishReason: payload.FinishReason, ErrorClass: payload.ErrorClass, Truncated: payload.Truncated, OriginalBytes: payload.OriginalBytes}, true
}

func optimizedSessionTimelineItem(event bee.Event) (sessionTimelineItem, bool) {
	item := sessionTimelineItem{Seq: event.Seq, ID: event.ID, At: event.At}
	switch event.Type {
	case bee.EventMessage:
		payload, err := bee.DecodeMessageEvent(event)
		if err != nil {
			return sessionTimelineItem{}, false
		}
		message := bee.ProjectMessageForDisplay(payload.Message)
		if (message.Role != bee.RoleUser && message.Role != bee.RoleAssistant) || strings.TrimSpace(message.Content) == "" {
			return sessionTimelineItem{}, false
		}
		item.Type, item.Message = "message", &message
	case bee.EventArtifact:
		payload, err := bee.DecodeArtifactEvent(event)
		if err != nil {
			return sessionTimelineItem{}, false
		}
		artifact := payload.Artifact
		item.Type, item.Artifact = "artifact", &artifact
	case bee.EventReasoning:
		reasoning, ok := projectReasoning(event)
		if !ok {
			return sessionTimelineItem{}, false
		}
		item.Type, item.Reasoning = "reasoning", &reasoning
	default:
		return sessionTimelineItem{}, false
	}
	return item, true
}

func legacySessionTimelineItem(event bee.Event) (sessionTimelineItem, bool) {
	item := sessionTimelineItem{Seq: event.Seq, ID: event.ID, At: event.At}
	switch event.Type {
	case bee.EventMessage:
		payload, err := bee.DecodeMessageEvent(event)
		if err != nil {
			return sessionTimelineItem{}, false
		}
		message := bee.ProjectMessageForDisplay(payload.Message)
		item.Type, item.Message = "message", &message
	case bee.EventArtifact:
		payload, err := bee.DecodeArtifactEvent(event)
		if err != nil {
			return sessionTimelineItem{}, false
		}
		artifact := payload.Artifact
		item.Type, item.Artifact = "artifact", &artifact
	case bee.EventReasoning:
		reasoning, ok := projectReasoning(event)
		if !ok {
			return sessionTimelineItem{}, false
		}
		item.Type, item.Reasoning = "reasoning", &reasoning
	default:
		return sessionTimelineItem{}, false
	}
	return item, true
}

func legacySessionTimelineTail(events []bee.Event, limit int) []sessionTimelineItem {
	if limit <= 0 {
		return []sessionTimelineItem{}
	}
	items := make([]sessionTimelineItem, 0, limit)
	for index := len(events) - 1; index >= 0 && len(items) < limit; index-- {
		if item, ok := legacySessionTimelineItem(events[index]); ok {
			items = append(items, item)
		}
	}
	result := make([]sessionTimelineItem, len(items))
	for index := range items {
		result[len(items)-1-index] = items[index]
	}
	return result
}

func projectSessionTimelineVisibility(items []sessionTimelineItem, visibility bee.DisplayVisibility) []sessionTimelineItem {
	projected := make([]sessionTimelineItem, 0, len(items))
	toolMode := visibility.EffectiveToolMode()
	for _, item := range items {
		if item.Reasoning != nil && !visibility.Thinking {
			continue
		}
		copy := item
		if item.Message != nil {
			if item.Message.Role == bee.RoleTool && toolMode != bee.ToolDisplayFull {
				continue
			}
			message := bee.ProjectMessageForDisplayWithVisibility(*item.Message, visibility)
			copy.Message = &message
		}
		if item.ToolCall != nil {
			if toolMode == bee.ToolDisplayOff {
				continue
			}
			call := bee.ProjectToolCallForDisplayWithVisibility(*item.ToolCall, visibility)
			copy.ToolCall = &call
		}
		if item.ToolResult != nil {
			if toolMode == bee.ToolDisplayOff {
				continue
			}
			result := bee.ProjectToolResultForDisplayWithVisibility(*item.ToolResult, visibility)
			copy.ToolResult = &result
		}
		projected = append(projected, copy)
	}
	return projected
}

func parseSessionHistoryWindow(request *http.Request) (limit int, before uint64, hasBefore bool, err error) {
	query := request.URL.Query()
	limit = defaultSessionHistoryWindow
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxSessionHistoryWindow {
			return 0, 0, false, fmt.Errorf("limit must be between 1 and %d", maxSessionHistoryWindow)
		}
	}
	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		before, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || before == 0 {
			return 0, 0, false, errors.New("before must be a positive event sequence")
		}
		hasBefore = true
	}
	return limit, before, hasBefore, nil
}

func sessionTimelineWindow(events []bee.Event, limit int, before uint64, hasBefore bool) ([]sessionTimelineItem, uint64, bool) {
	if limit <= 0 {
		return []sessionTimelineItem{}, 0, false
	}
	reverseWindow := make([]sessionTimelineItem, 0, limit)
	hasMore := false
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if hasBefore && event.Seq >= before {
			continue
		}
		item, ok := optimizedSessionTimelineItem(event)
		if !ok {
			continue
		}
		if len(reverseWindow) < limit {
			reverseWindow = append(reverseWindow, item)
			continue
		}
		hasMore = true
		break
	}
	window := make([]sessionTimelineItem, len(reverseWindow))
	for index := range reverseWindow {
		window[len(reverseWindow)-1-index] = reverseWindow[index]
	}
	var nextBefore uint64
	if hasMore && len(window) > 0 {
		nextBefore = window[0].Seq
	}
	return window, nextBefore, hasMore
}

func timelineAttachments(items []sessionTimelineItem, attachments []bee.Attachment) []bee.Attachment {
	attachmentIDs := make(map[string]struct{})
	artifactIDs := make(map[string]struct{})
	for _, item := range items {
		if item.Message == nil {
			continue
		}
		for _, ref := range item.Message.Attachments {
			if id := strings.TrimSpace(ref.ID); id != "" {
				attachmentIDs[id] = struct{}{}
			}
			if id := strings.TrimSpace(ref.ArtifactID); id != "" {
				artifactIDs[id] = struct{}{}
			}
		}
	}
	filtered := make([]bee.Attachment, 0, len(attachmentIDs))
	for _, attachment := range attachments {
		_, byAttachment := attachmentIDs[attachment.ID]
		_, byArtifact := artifactIDs[attachment.ArtifactID]
		if byAttachment || byArtifact {
			filtered = append(filtered, attachment)
		}
	}
	return filtered
}

func (service *Service) handleDeleteSession(writer http.ResponseWriter, id bee.SessionID) {
	if err := service.store.Delete(id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, bee.ErrSessionBusy) {
			status = http.StatusConflict
		}
		writeError(writer, status, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (service *Service) handleSessionHistory(writer http.ResponseWriter, request *http.Request, id bee.SessionID) {
	started := time.Now()
	session, err := service.app.Resume(id)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	if strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("view")), "timeline") {
		limit, before, hasBefore, parseErr := parseSessionHistoryWindow(request)
		if parseErr != nil {
			writeError(writer, http.StatusBadRequest, parseErr)
			return
		}
		items, nextBefore, hasMore := sessionTimelineWindow(session.EventsSnapshot(), limit, before, hasBefore)
		items = projectSessionTimelineVisibility(items, service.app.DisplayVisibility())
		var attachments []bee.Attachment
		if service.app.AttachmentStore != nil {
			attachments, err = service.app.AttachmentStore.List(id)
			if err != nil {
				writeError(writer, http.StatusInternalServerError, err)
				return
			}
			if hasBefore {
				attachments = timelineAttachments(items, attachments)
			}
		}
		response := sessionTimelineResponse{SessionID: session.ID, Metadata: session.Metadata, Timeline: items, Attachments: attachments, EventCount: len(session.EventsSnapshot()), Limit: limit, HasMore: hasMore}
		if hasBefore {
			response.Before = strconv.FormatUint(before, 10)
		}
		if hasMore {
			response.NextBefore = strconv.FormatUint(nextBefore, 10)
		}
		setServerTiming(writer, "history", started)
		writeJSON(writer, http.StatusOK, response)
		return
	}
	messages, err := session.Messages()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	visibility := service.app.DisplayVisibility()
	projectedMessages := make([]bee.Message, 0, len(messages))
	for index := range messages {
		if messages[index].Role == bee.RoleTool && visibility.EffectiveToolMode() != bee.ToolDisplayFull {
			continue
		}
		projectedMessages = append(projectedMessages, bee.ProjectMessageForDisplayWithVisibility(messages[index], visibility))
	}
	messages = projectedMessages
	var artifacts []bee.Artifact
	var attachments []bee.Attachment
	if service.app.ArtifactStore != nil {
		artifacts, err = service.app.ArtifactStore.List(id)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
	}
	if service.app.AttachmentStore != nil {
		attachments, err = service.app.AttachmentStore.List(id)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
	}
	events := session.EventsSnapshot()
	timeline := projectSessionTimelineVisibility(legacySessionTimelineTail(events, maxSessionTimelineItems), visibility)
	setServerTiming(writer, "history", started)
	writeJSON(writer, http.StatusOK, sessionHistoryResponse{Metadata: session.Metadata, Messages: messages, Artifacts: artifacts, Attachments: attachments, Timeline: timeline, EventCount: len(events)})
}

func (service *Service) handleSessionArtifacts(writer http.ResponseWriter, id bee.SessionID) {
	if service.app.ArtifactStore == nil {
		writeError(writer, http.StatusNotImplemented, errors.New("artifact store is not configured"))
		return
	}
	if _, err := service.app.Resume(id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	artifacts, err := service.app.ArtifactStore.List(id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, artifacts)
}

func (service *Service) handleSessionAttachments(writer http.ResponseWriter, id bee.SessionID) {
	if service.app.AttachmentStore == nil {
		writeError(writer, http.StatusNotImplemented, errors.New("attachment store is not configured"))
		return
	}
	if _, err := service.app.Resume(id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	attachments, err := service.app.AttachmentStore.List(id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, attachments)
}

func (service *Service) handleUploadAttachment(writer http.ResponseWriter, request *http.Request, id bee.SessionID) {
	if service.app.AttachmentStore == nil {
		writeError(writer, http.StatusNotImplemented, errors.New("attachment store is not configured"))
		return
	}
	if _, err := service.app.Resume(id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrSessionNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, bee.MaxAttachmentBytes+(1<<20))
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("parse attachment upload: %w", err))
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		writeError(writer, http.StatusBadRequest, errors.New("multipart field file is required"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, bee.MaxAttachmentBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if int64(len(data)) > bee.MaxAttachmentBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, errors.New("attachment exceeds upload size limit"))
		return
	}
	if header == nil || strings.TrimSpace(header.Filename) == "" {
		writeError(writer, http.StatusBadRequest, errors.New("attachment filename is required"))
		return
	}
	mediaType := ""
	if header.Header != nil {
		mediaType = header.Header.Get("Content-Type")
	}
	attachment, err := service.app.AttachmentStore.Create(request.Context(), id, header.Filename, mediaType, data)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	if attachment.Kind == "audio" {
		if _, err := service.app.AttachmentStore.UpdateStatus(attachment.ID, "processing", ""); err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
		service.app.AttachmentStore.StartAudioProcessing(service.ctx, attachment.ID)
		attachment.Status = "processing"
	}
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(writer, http.StatusCreated, attachment)
}

func (service *Service) handleAttachmentPath(writer http.ResponseWriter, request *http.Request) {
	if service.app.AttachmentStore == nil {
		writeError(writer, http.StatusNotImplemented, errors.New("attachment store is not configured"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/attachments/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		notFound(writer)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	attachment, err := service.app.AttachmentStore.Get(parts[0])
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrAttachmentNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	writeJSON(writer, http.StatusOK, attachment)
}

func safeContentDisposition(filename string, inline bool) string {
	filename = strings.NewReplacer("\\", "_", "\"", "_", "\r", "_", "\n", "_").Replace(filename)
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = "artifact"
	}
	mode := "attachment"
	if inline {
		mode = "inline"
	}
	return fmt.Sprintf(`%s; filename="%s"`, mode, filename)
}

type artifactByteRange struct {
	start int64
	end   int64
}

func parseArtifactByteRange(value string, size int64) (artifactByteRange, bool) {
	if size < 0 || !strings.HasPrefix(value, "bytes=") {
		return artifactByteRange{}, false
	}
	spec := strings.TrimPrefix(value, "bytes=")
	if spec == "" || strings.Count(spec, "-") != 1 {
		return artifactByteRange{}, false
	}
	dash := strings.IndexByte(spec, '-')
	first, last := spec[:dash], spec[dash+1:]
	if first == "" {
		suffix, ok := parseArtifactByteRangeNumber(last)
		if !ok || suffix == 0 || size == 0 {
			return artifactByteRange{}, false
		}
		if suffix >= size {
			return artifactByteRange{start: 0, end: size - 1}, true
		}
		return artifactByteRange{start: size - suffix, end: size - 1}, true
	}
	start, ok := parseArtifactByteRangeNumber(first)
	if !ok || size == 0 || start >= size {
		return artifactByteRange{}, false
	}
	if last == "" {
		return artifactByteRange{start: start, end: size - 1}, true
	}
	end, ok := parseArtifactByteRangeNumber(last)
	if !ok || end < start {
		return artifactByteRange{}, false
	}
	if end >= size {
		end = size - 1
	}
	return artifactByteRange{start: start, end: end}, true
}

func parseArtifactByteRangeNumber(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	return number, err == nil
}

func (service *Service) handleArtifactPath(writer http.ResponseWriter, request *http.Request) {
	if service.app.ArtifactStore == nil {
		writeError(writer, http.StatusNotImplemented, errors.New("artifact store is not configured"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/artifacts/"), "/")
	if len(parts) < 1 || parts[0] == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "download" && parts[1] != "preview") {
		notFound(writer)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		artifact, err := service.app.ArtifactStore.Get(id)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bee.ErrArtifactNotFound) {
				status = http.StatusNotFound
			}
			writeError(writer, status, err)
			return
		}
		writeJSON(writer, http.StatusOK, artifact)
		return
	}
	if parts[1] == "preview" {
		artifact, data, truncated, err := service.app.ArtifactStore.Preview(id)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bee.ErrArtifactNotFound) {
				status = http.StatusNotFound
			}
			writeError(writer, status, err)
			return
		}
		if artifact.Kind != "text" {
			writeError(writer, http.StatusUnsupportedMediaType, errors.New("binary artifacts do not have an inline preview"))
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Content-Disposition", safeContentDisposition(artifact.Filename, true))
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Bee-Artifact-Truncated", strconv.FormatBool(truncated))
		_, _ = writer.Write(data)
		return
	}
	artifact, file, err := service.app.ArtifactStore.OpenPayload(id)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bee.ErrArtifactNotFound) {
			status = http.StatusNotFound
		}
		writeError(writer, status, err)
		return
	}
	defer file.Close()
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Type", artifact.MediaType)
	writer.Header().Set("Content-Disposition", safeContentDisposition(artifact.Filename, false))
	writer.Header().Set("X-Content-Type-Options", "nosniff")

	rangeValues := request.Header.Values("Range")
	if len(rangeValues) > 0 {
		if len(rangeValues) != 1 {
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", artifact.Size))
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		byteRange, ok := parseArtifactByteRange(rangeValues[0], artifact.Size)
		if !ok {
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", artifact.Size))
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		seeker, ok := file.(io.Seeker)
		if !ok {
			writeError(writer, http.StatusInternalServerError, errors.New("artifact payload is not seekable"))
			return
		}
		if _, err := seeker.Seek(byteRange.start, io.SeekStart); err != nil {
			writeError(writer, http.StatusInternalServerError, errors.New("seek artifact payload"))
			return
		}
		length := byteRange.end - byteRange.start + 1
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.start, byteRange.end, artifact.Size))
		writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = io.CopyN(writer, file, length)
		return
	}

	writer.Header().Set("Content-Length", strconv.FormatInt(artifact.Size, 10))
	_, _ = io.CopyN(writer, file, artifact.Size)
}

func (service *Service) maybeNameSession(session *bee.Session, prompt string) {
	if session == nil || strings.TrimSpace(prompt) == "" || strings.HasPrefix(strings.TrimSpace(prompt), "/") || (session.Metadata.Title != "" && session.Metadata.Title != DefaultSessionTitle) {
		return
	}
	title := strings.Join(strings.Fields(prompt), " ")
	if len(title) > 72 {
		title = title[:69] + "..."
	}
	if title == "" {
		return
	}
	_ = session.UpdateMetadata(bee.SessionMetadata{Title: title})
}

type messageRequest struct {
	Prompt        string   `json:"prompt"`
	Model         string   `json:"model,omitempty"`
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
	CheckpointID  string   `json:"checkpoint_id,omitempty"`
}

func (service *Service) handleRunPath(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/runs/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.handleRunStatus(writer, parts[0])
		return
	}
	if len(parts) != 2 || parts[0] == "" {
		notFound(writer)
		return
	}
	runID, suffix := parts[0], parts[1]
	switch suffix {
	case "events":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		service.handleRunEvents(writer, request, runID)
	case "cancel":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		service.handleRunCancel(writer, runID)
	default:
		notFound(writer)
	}
}

type runStatusResponse struct {
	RunID              string `json:"run_id"`
	State              string `json:"state"`
	Running            bool   `json:"running"`
	Done               bool   `json:"done"`
	LastCursor         int64  `json:"last_cursor"`
	NextCursor         uint64 `json:"next_cursor"`
	OldestCursor       uint64 `json:"oldest_cursor"`
	TerminalObserved   bool   `json:"terminal_observed"`
	TerminalKind       string `json:"terminal_kind,omitempty"`
	TerminalError      string `json:"terminal_error,omitempty"`
	ReplayGap          bool   `json:"replay_gap"`
	Overflow           bool   `json:"overflow"`
	GapStartCursor     uint64 `json:"gap_start_cursor,omitempty"`
	GapEndCursor       uint64 `json:"gap_end_cursor,omitempty"`
	RetainedEventCount int    `json:"retained_event_count"`
}

func (service *Service) handleRunStatus(writer http.ResponseWriter, id string) {
	record, ok := service.run(id)
	if !ok {
		writeError(writer, http.StatusNotFound, errors.New("run not found"))
		return
	}
	writeJSON(writer, http.StatusOK, record.status())
}

func (service *Service) startRun(request bee.RunRequest) (string, error) {
	service.mu.Lock()
	ready := service.ready
	service.mu.Unlock()
	request.GatewayReady = &ready
	select {
	case service.runSlots <- struct{}{}:
	case <-service.ctx.Done():
		return "", context.Canceled
	default:
		service.app.RecordAdmissionRejected()
		return "", ErrRunCapacity
	}
	releaseSlot := true
	defer func() {
		if releaseSlot {
			<-service.runSlots
		}
	}()
	runCtx, runCancel := context.WithCancel(service.ctx)
	events, err := service.app.Run(runCtx, request)
	if err != nil {
		runCancel()
		return "", err
	}
	select {
	case first, ok := <-events:
		if !ok || first.RunID == "" {
			runCancel()
			return "", errors.New("run did not emit a run id")
		}
		record := newRunRecord(first.RunID, runCtx, runCancel, service.app.NewRunEventDisplayProjector())
		record.publish(first)
		service.mu.Lock()
		service.pruneRunsLocked()
		if len(service.runs) >= MaxRunRecords {
			service.mu.Unlock()
			record.cancel()
			return "", errors.New("run capacity exceeded")
		}
		service.runs[first.RunID] = record
		service.mu.Unlock()
		service.wg.Add(1)
		go func() {
			defer service.wg.Done()
			defer runCancel()
			defer func() { <-service.runSlots }()
			for event := range events {
				record.publish(event)
			}
			record.finish()
		}()
		releaseSlot = false
		return first.RunID, nil
	case <-time.After(10 * time.Second):
		runCancel()
		return "", errors.New("timed out waiting for run start")
	case <-service.ctx.Done():
		runCancel()
		return "", context.Canceled
	}
}

func writeRunStartError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, bee.ErrSessionBusy) {
		status = http.StatusConflict
	} else if errors.Is(err, ErrRunCapacity) {
		status = http.StatusTooManyRequests
	} else if errors.Is(err, context.Canceled) {
		status = http.StatusServiceUnavailable
	}
	writeError(writer, status, err)
}

func (service *Service) pruneRunsLocked() {
	for len(service.runs) >= MaxRunRecords {
		var oldestID string
		var oldest time.Time
		for id, record := range service.runs {
			if !record.isDone() {
				continue
			}
			record.mu.Lock()
			finishedAt := record.finishedAt
			record.mu.Unlock()
			if oldestID == "" || finishedAt.Before(oldest) {
				oldestID, oldest = id, finishedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(service.runs, oldestID)
	}
}

func (service *Service) run(id string) (*runRecord, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	record, ok := service.runs[id]
	return record, ok
}

func (service *Service) handleRunCancel(writer http.ResponseWriter, id string) {
	record, ok := service.run(id)
	if !ok {
		writeError(writer, http.StatusNotFound, errors.New("run not found"))
		return
	}
	record.cancelRun()
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_id": id, "cancelled": true})
}

func (service *Service) handleRunEvents(writer http.ResponseWriter, request *http.Request, id string) {
	startedAt := time.Now()
	after := ^uint64(0) // no cursor: replay from the beginning
	eventCount := 0
	reason := runEventsReasonSetup
	gap := false
	defer func() {
		if record, ok := service.run(id); ok {
			status := record.status()
			clientGap := gap || (status.ReplayGap && (after == ^uint64(0) || after < status.GapEndCursor))
			logRunEventsTermination(id, reason, displayCursor(after), eventCount, clientGap, time.Since(startedAt))
			return
		}
		cursor := int64(-1)
		if after != ^uint64(0) && after <= uint64(^uint64(0)>>1) {
			cursor = int64(after)
		}
		logRunEventsTermination(id, reason, cursor, eventCount, gap, time.Since(startedAt))
	}()

	record, ok := service.run(id)
	if !ok {
		reason = runEventsReasonInvalidRequest
		writeError(writer, http.StatusNotFound, errors.New("run not found"))
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		reason = runEventsReasonSetup
		writeError(writer, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	query := request.URL.Query()
	if raw := query.Get("after"); raw != "" {
		parsed, err := parseCursor(raw)
		if err != nil {
			reason = runEventsReasonInvalidRequest
			writeError(writer, http.StatusBadRequest, errors.New("event stream cursor must be a non-negative integer"))
			return
		}
		after = parsed
	} else if raw := request.Header.Get("Last-Event-ID"); raw != "" {
		parsed, err := parseCursor(raw)
		if err != nil {
			reason = runEventsReasonInvalidRequest
			writeError(writer, http.StatusBadRequest, errors.New("last event id must be a non-negative integer"))
			return
		}
		after = parsed
	} else if raw := query.Get("from"); raw != "" {
		parsed, err := parseCursor(raw)
		if err != nil {
			reason = runEventsReasonInvalidRequest
			writeError(writer, http.StatusBadRequest, errors.New("event stream offset must be a non-negative integer"))
			return
		}
		if parsed > 0 {
			after = parsed - 1
		}
	}
	if _, _, _, gapFound := record.afterCursor(after); gapFound {
		reason = runEventsReasonReplayGap
		gap = true
		status := record.status()
		writeJSON(writer, http.StatusConflict, map[string]any{"error": "event replay gap", "gap": true, "run_id": id, "requested_after": displayCursor(after), "gap_start_cursor": status.GapStartCursor, "gap_end_cursor": status.GapEndCursor, "oldest_cursor": status.OldestCursor, "next_cursor": status.NextCursor})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	if n, err := io.WriteString(writer, ": connected\n\n"); err != nil || n != len(": connected\n\n") {
		reason = runEventsReasonWriteError
		return
	}
	flusher.Flush()
	deadline := time.NewTimer(SSEMaxDuration)
	defer deadline.Stop()
	keepalive := time.NewTicker(SSEKeepalive)
	defer keepalive.Stop()
	for {
		events, notify, done, gapFound := record.afterCursor(after)
		if gapFound {
			reason = runEventsReasonReplayGap
			gap = true
			status := record.status()
			data, _ := json.Marshal(map[string]any{"type": "run_gap", "run_id": id, "reason": "replay_gap", "gap_start_cursor": status.GapStartCursor, "gap_end_cursor": status.GapEndCursor})
			payload := fmt.Sprintf("id: %d\ndata: %s\n\n", status.GapEndCursor, data)
			if n, err := io.WriteString(writer, payload); err != nil || n != len(payload) {
				reason = runEventsReasonWriteError
				return
			}
			eventCount++
			flusher.Flush()
			return
		}
		for _, retained := range events {
			data, err := json.Marshal(retained.event)
			if err != nil || len(data) > MaxRunEventBytes {
				data, _ = json.Marshal(map[string]any{"type": "run_gap", "run_id": id, "cursor": retained.cursor, "reason": "event_encode_failure"})
			}
			payload := fmt.Sprintf("id: %d\ndata: %s\n\n", retained.cursor, data)
			if n, err := io.WriteString(writer, payload); err != nil || n != len(payload) {
				reason = runEventsReasonWriteError
				return
			}
			eventCount++
			after = retained.cursor
			flusher.Flush()
		}
		if done {
			reason = runEventsReasonRunCompleted
			return
		}
		select {
		case <-request.Context().Done():
			if service.ctx.Err() != nil {
				reason = runEventsReasonServiceShutdown
			} else {
				reason = runEventsReasonClientDisconnect
			}
			return
		case <-service.ctx.Done():
			reason = runEventsReasonServiceShutdown
			return
		case <-notify:
			continue
		case <-keepalive.C:
			if n, err := io.WriteString(writer, ": keepalive\n\n"); err != nil || n != len(": keepalive\n\n") {
				reason = runEventsReasonWriteError
				return
			}
			flusher.Flush()
		case <-deadline.C:
			reason = runEventsReasonMaxDuration
			return
		}
	}
}

const (
	runEventsReasonRunCompleted     = "run_completed"
	runEventsReasonClientDisconnect = "client_disconnect"
	runEventsReasonServiceShutdown  = "service_shutdown"
	runEventsReasonMaxDuration      = "sse_max_duration"
	runEventsReasonWriteError       = "write_error"
	runEventsReasonReplayGap        = "replay_gap"
	runEventsReasonSetup            = "setup_error"
	runEventsReasonInvalidRequest   = "invalid_request"
)

func boundedRunEventsReason(reason string) string {
	switch reason {
	case runEventsReasonRunCompleted,
		runEventsReasonClientDisconnect,
		runEventsReasonServiceShutdown,
		runEventsReasonMaxDuration,
		runEventsReasonWriteError,
		runEventsReasonReplayGap,
		runEventsReasonSetup,
		runEventsReasonInvalidRequest:
		return reason
	default:
		return runEventsReasonSetup
	}
}

func parseCursor(raw string) (uint64, error) {
	if raw == "" || len(raw) > 20 {
		return 0, errors.New("invalid cursor")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, errors.New("invalid cursor")
		}
	}
	return strconv.ParseUint(raw, 10, 64)
}

func displayCursor(cursor uint64) int64 {
	if cursor == ^uint64(0) {
		return -1
	}
	if cursor > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(cursor)
}

func boundedRunEvent(event bee.RunEvent) bee.RunEvent {
	data, err := json.Marshal(event)
	if err == nil && len(data) <= MaxRunEventBytes {
		return event
	}
	if event.Type == bee.RunEnd || event.Type == bee.RunFailure {
		return bee.RunEvent{Type: event.Type, At: event.At, RunID: event.RunID, SessionID: event.SessionID, Error: event.Error}
	}
	return bee.RunEvent{Type: bee.RunEventType("run_gap"), At: event.At, RunID: event.RunID, SessionID: event.SessionID, Error: "event_too_large"}
}

func logRunEventsTermination(id, reason string, cursor int64, eventCount int, gap bool, elapsed time.Duration) {
	if len(id) > 128 {
		id = id[:128]
	}
	id = strings.NewReplacer("\r", "", "\n", "").Replace(id)
	if cursor < -1 {
		cursor = -1
	}
	if eventCount < 0 {
		eventCount = 0
	}
	if elapsed < 0 {
		elapsed = 0
	}
	log.Printf("run_events_terminated run_id=%q cursor=%d event_count=%d gap=%t reason=%s elapsed_ms=%d", id, cursor, eventCount, gap, boundedRunEventsReason(reason), elapsed/time.Millisecond)
}

type runRecord struct {
	id        string
	ctx       context.Context
	cancel    context.CancelFunc
	projector *bee.RunEventDisplayProjector

	mu             sync.Mutex
	events         []retainedRunEvent
	ordinaryEvents int
	failureSeen    bool
	endSeen        bool
	done           bool
	nextCursor     uint64
	oldestCursor   uint64
	overflow       bool
	gapSet         bool
	gapStartCursor uint64
	gapEndCursor   uint64
	terminalKind   string
	terminalError  string
	finishedAt     time.Time
	notify         chan struct{}
	cancelled      bool
}

type retainedRunEvent struct {
	cursor uint64
	event  bee.RunEvent
}

func newRunRecord(id string, ctx context.Context, cancel context.CancelFunc, projectors ...*bee.RunEventDisplayProjector) *runRecord {
	var projector *bee.RunEventDisplayProjector
	if len(projectors) > 0 {
		projector = projectors[0]
	}
	return &runRecord{id: id, ctx: ctx, cancel: cancel, projector: projector, notify: make(chan struct{})}
}

func (record *runRecord) publish(event bee.RunEvent) {
	if record.projector != nil {
		var visible bool
		event, visible = record.projector.Project(event)
		if !visible {
			return
		}
	}
	record.mu.Lock()
	event = boundedRunEvent(event)
	retained := false
	advance := false
	switch event.Type {
	case bee.RunFailure:
		// Reserve one bounded slot for the failure terminal event. Ignore a
		// duplicate publish so retries cannot grow the record or duplicate SSE.
		if !record.failureSeen {
			record.events = append(record.events, retainedRunEvent{cursor: record.nextCursor, event: event})
			record.failureSeen = true
			retained = true
			advance = true
		}
		record.terminalKind = string(bee.RunFailure)
		record.terminalError = event.Error
	case bee.RunEnd:
		// Reserve one bounded slot for the end terminal event. RunEnd still
		// marks the record complete when a producer publishes it twice.
		if !record.endSeen {
			record.events = append(record.events, retainedRunEvent{cursor: record.nextCursor, event: event})
			record.endSeen = true
			retained = true
			advance = true
		}
		record.done = true
		record.finishedAt = time.Now().UTC()
		record.terminalKind = string(bee.RunEnd)
		record.terminalError = event.Error
	default:
		if record.ordinaryEvents < MaxRunEvents {
			record.events = append(record.events, retainedRunEvent{cursor: record.nextCursor, event: event})
			record.ordinaryEvents++
			retained = true
			advance = true
		} else {
			record.markGapLocked(record.nextCursor)
			advance = true
		}
	}
	if advance {
		record.nextCursor++
	}
	if retained && len(record.events) > MaxRunRecordEvents {
		record.markGapLocked(record.events[0].cursor)
		record.events = record.events[1:]
		record.oldestCursor = record.events[0].cursor
		record.overflow = true
	}
	close(record.notify)
	record.notify = make(chan struct{})
	record.mu.Unlock()
}

func (record *runRecord) markGapLocked(cursor uint64) {
	record.overflow = true
	if !record.gapSet {
		record.gapSet = true
		record.gapStartCursor = cursor
	}
	record.gapEndCursor = cursor
}

func (record *runRecord) finish() {
	record.mu.Lock()
	if !record.endSeen {
		event := bee.RunEvent{Type: bee.RunEnd, At: time.Now().UTC(), RunID: record.id, Error: "run ended without a terminal event"}
		record.events = append(record.events, retainedRunEvent{cursor: record.nextCursor, event: event})
		record.nextCursor++
		record.endSeen = true
		record.terminalKind = string(bee.RunEnd)
		record.terminalError = event.Error
	}
	record.done = true
	record.finishedAt = time.Now().UTC()
	close(record.notify)
	record.notify = make(chan struct{})
	record.mu.Unlock()
}

func (record *runRecord) after(index int) ([]bee.RunEvent, <-chan struct{}, bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if index < 0 {
		index = 0
	}
	if index >= len(record.events) {
		return nil, record.notify, record.done
	}
	events := make([]bee.RunEvent, 0, len(record.events)-index)
	for _, retained := range record.events[index:] {
		events = append(events, retained.event)
	}
	return events, record.notify, record.done
}

func (record *runRecord) afterCursor(after uint64) ([]retainedRunEvent, <-chan struct{}, bool, bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.gapSet && (after == ^uint64(0) || after < record.gapEndCursor) {
		return nil, record.notify, record.done, true
	}
	if after != ^uint64(0) && record.oldestCursor > 0 && len(record.events) > 0 && after < record.oldestCursor-1 {
		return nil, record.notify, record.done, true
	}
	start := 0
	for start < len(record.events) && after != ^uint64(0) && record.events[start].cursor <= after {
		start++
	}
	return append([]retainedRunEvent(nil), record.events[start:]...), record.notify, record.done, false
}

func (record *runRecord) status() runStatusResponse {
	record.mu.Lock()
	defer record.mu.Unlock()
	last := int64(-1)
	if record.nextCursor > 0 {
		last = int64(record.nextCursor - 1)
	}
	return runStatusResponse{RunID: record.id, State: map[bool]string{true: "done", false: "running"}[record.done], Running: !record.done, Done: record.done, LastCursor: last, NextCursor: record.nextCursor, OldestCursor: record.oldestCursor, TerminalObserved: record.endSeen || record.failureSeen, TerminalKind: record.terminalKind, TerminalError: record.terminalError, ReplayGap: record.overflow, Overflow: record.overflow, GapStartCursor: record.gapStartCursor, GapEndCursor: record.gapEndCursor, RetainedEventCount: len(record.events)}
}

func (record *runRecord) isDone() bool {
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.done
}

func (record *runRecord) cancelRun() {
	record.mu.Lock()
	record.cancelled = true
	record.mu.Unlock()
	record.cancel()
}

func validateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	if host == "" {
		return errors.New("listen address host is required")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
		return errors.New("listen address must be loopback or an unspecified wildcard host")
	}
	return nil
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, value any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxJSONBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "request body too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(writer, status, errors.New("invalid JSON request body"))
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(writer, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return errors.New("trailing JSON")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	if err == nil {
		err = errors.New("request failed")
	}
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(writer, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}

func notFound(writer http.ResponseWriter) {
	writeError(writer, http.StatusNotFound, errors.New("not found"))
}
