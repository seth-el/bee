package bee

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openAIWebRunLifecycleHeader = "X-Bee-Run-Lifecycle"
	maxOpenAIWebRunRecords      = 128
	maxOpenAIWebRunEvents       = 256
	maxOpenAIWebRunIDBytes      = 128
	openAIWebRunStreamDuration  = 10 * time.Minute
)

type openAIWebRunState string

const (
	openAIWebRunIdle               openAIWebRunState = "idle"
	openAIWebRunRunning            openAIWebRunState = "running"
	openAIWebRunStreaming          openAIWebRunState = "streaming"
	openAIWebRunRunningTool        openAIWebRunState = "running_tool"
	openAIWebRunWaitingForApproval openAIWebRunState = "waiting_for_approval"
	openAIWebRunDone               openAIWebRunState = "done"
	openAIWebRunFailed             openAIWebRunState = "failed"
)

// openAIWebRunEvent is the deliberately content-free lifecycle projection
// exposed to HTTP clients. It never carries prompts, model text, reasoning, tool
// arguments/results, or approval summaries.
type openAIWebRunEvent struct {
	Cursor         uint64            `json:"cursor"`
	RunID          string            `json:"run_id,omitempty"`
	SessionID      SessionID         `json:"session_id,omitempty"`
	State          openAIWebRunState `json:"state"`
	ToolName       string            `json:"tool_name,omitempty"`
	ApprovalStatus ApprovalStatus    `json:"approval_status,omitempty"`
	At             time.Time         `json:"at,omitempty"`
	Terminal       bool              `json:"terminal,omitempty"`
	// Performance is set on the record emitted after each model call: token
	// counts and timing only (G9). The state is unchanged by such a record.
	Performance *openAIWebRunPerformance `json:"performance,omitempty"`
	// Reason classifies a failed state: "model_unreachable" when the model
	// server could not be reached. Clients that don't know it still see
	// "failed".
	Reason string `json:"reason,omitempty"`
	// FallbackProfile is set on the record emitted when the main model was
	// unreachable and another profile is answering. The state is unchanged.
	FallbackProfile string `json:"fallback_profile,omitempty"`
	// NetworkOff is true on every record of a run whose session has its
	// network turned off (/network off).
	NetworkOff bool `json:"network_off,omitempty"`
}

// openAIWebRunPerformance is one model call's numbers. Never content.
type openAIWebRunPerformance struct {
	PromptTokens          int     `json:"prompt_tokens,omitempty"`
	CompletionTokens      int     `json:"completion_tokens,omitempty"`
	CachedTokens          int     `json:"cached_tokens,omitempty"`
	DurationMs            int64   `json:"duration_ms,omitempty"`
	TimeToFirstTokenMs    int64   `json:"time_to_first_token_ms,omitempty"`
	DecodeTokensPerSecond float64 `json:"decode_tokens_per_second,omitempty"`
	ContextUsedTokens     int     `json:"context_used_tokens,omitempty"`
	ContextWindowTokens   int     `json:"context_window_tokens,omitempty"`
}

type openAIWebRunRegistry struct {
	app *Application

	mu             sync.Mutex
	byID           map[string]*openAIWebRunRecord
	byConversation map[string]string
	order          []string
}

type openAIWebRunRecord struct {
	networkOff     bool
	id             string
	conversationID string
	projector      *RunEventDisplayProjector

	mu        sync.Mutex
	events    []openAIWebRunEvent
	next      uint64
	notify    chan struct{}
	done      bool
	failed    bool
	state     openAIWebRunState
	sessionID SessionID
	pending   map[string]struct{}
}

func newOpenAIWebRunRegistry(app *Application) *openAIWebRunRegistry {
	return &openAIWebRunRegistry{
		app:            app,
		byID:           make(map[string]*openAIWebRunRecord),
		byConversation: make(map[string]string),
	}
}

func (registry *openAIWebRunRegistry) publish(conversationID string, event RunEvent) (openAIWebRunEvent, bool) {
	if registry == nil || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(event.RunID) == "" {
		return openAIWebRunEvent{}, false
	}
	registry.mu.Lock()
	record := registry.byID[event.RunID]
	if record == nil {
		if event.Type != RunStart {
			registry.mu.Unlock()
			return openAIWebRunEvent{}, false
		}
		if !registry.makeRoomLocked() {
			registry.mu.Unlock()
			return openAIWebRunEvent{}, false
		}
		record = &openAIWebRunRecord{
			id:             event.RunID,
			conversationID: conversationID,
			projector:      registry.app.NewRunEventDisplayProjector(),
			notify:         make(chan struct{}),
			pending:        make(map[string]struct{}),
		}
		registry.byID[event.RunID] = record
		registry.byConversation[conversationID] = event.RunID
		registry.order = append(registry.order, event.RunID)
	}
	registry.mu.Unlock()
	if record.conversationID != conversationID {
		return openAIWebRunEvent{}, false
	}
	return record.publish(event)
}

func (registry *openAIWebRunRegistry) makeRoomLocked() bool {
	for len(registry.order) >= maxOpenAIWebRunRecords {
		removed := false
		for index, id := range registry.order {
			record := registry.byID[id]
			if record == nil || record.isDone() {
				delete(registry.byID, id)
				if record != nil && registry.byConversation[record.conversationID] == id {
					delete(registry.byConversation, record.conversationID)
				}
				registry.order = append(registry.order[:index], registry.order[index+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			return false
		}
	}
	return true
}

func (registry *openAIWebRunRegistry) current(conversationID string) (openAIWebRunEvent, bool) {
	registry.mu.Lock()
	id := registry.byConversation[conversationID]
	record := registry.byID[id]
	registry.mu.Unlock()
	if record == nil {
		return openAIWebRunEvent{}, false
	}
	return record.status()
}

func (registry *openAIWebRunRegistry) record(id, conversationID string) (*openAIWebRunRecord, bool) {
	registry.mu.Lock()
	record := registry.byID[id]
	registry.mu.Unlock()
	return record, record != nil && record.conversationID == conversationID
}

func (registry *openAIWebRunRegistry) ensureFailed(conversationID string) {
	registry.mu.Lock()
	id := registry.byConversation[conversationID]
	record := registry.byID[id]
	registry.mu.Unlock()
	if record == nil || record.isDone() {
		return
	}
	status, _ := record.status()
	_, _ = record.publish(RunEvent{Type: RunFailure, RunID: record.id, SessionID: status.SessionID})
	_, _ = record.publish(RunEvent{Type: RunEnd, RunID: record.id, SessionID: status.SessionID, Error: "run ended before a terminal event was delivered"})
}

func (record *openAIWebRunRecord) publish(event RunEvent) (openAIWebRunEvent, bool) {
	projected, ok := record.projector.Project(event)
	if !ok {
		return openAIWebRunEvent{}, false
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.done {
		return openAIWebRunEvent{}, false
	}
	if projected.SessionID != "" {
		record.sessionID = projected.SessionID
	}
	state := record.state
	toolName := ""
	approvalStatus := ApprovalStatus("")
	terminal := false
	var performance *openAIWebRunPerformance
	reason := ""
	fallbackProfile := ""
	switch projected.Type {
	case RunProviderFallback:
		if projected.ProviderFallback == nil {
			return openAIWebRunEvent{}, false
		}
		fallbackProfile = projected.ProviderFallback.To
	case RunUsage:
		if projected.Usage == nil {
			return openAIWebRunEvent{}, false
		}
		usage := projected.Usage.Usage
		performance = &openAIWebRunPerformance{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, CachedTokens: usage.CacheReadTokens}
		if timing := projected.Usage.Timing; timing != nil {
			performance.DurationMs = timing.DurationMs
			performance.TimeToFirstTokenMs = timing.TimeToFirstTokenMs
			performance.DecodeTokensPerSecond = timing.DecodeTokensPerSecond
			performance.ContextUsedTokens = timing.ContextUsedTokens
			performance.ContextWindowTokens = timing.ContextWindowTokens
		}
	case RunStart:
		state = openAIWebRunRunning
		record.networkOff = projected.NetworkOff
	case RunAssistantDelta:
		if state == openAIWebRunWaitingForApproval || state == openAIWebRunStreaming {
			return openAIWebRunEvent{}, false
		}
		state = openAIWebRunStreaming
	case RunToolCall:
		state = openAIWebRunRunningTool
		if projected.ToolCall != nil {
			toolName = projected.ToolCall.Name
		}
	case RunToolResult:
		if len(record.pending) > 0 {
			state = openAIWebRunWaitingForApproval
		} else {
			state = openAIWebRunRunning
		}
	case RunApproval:
		if projected.Approval == nil {
			return openAIWebRunEvent{}, false
		}
		approvalStatus = projected.Approval.EffectiveStatus()
		id := strings.TrimSpace(projected.Approval.ID)
		if approvalStatus == ApprovalStatusPending {
			if id != "" {
				record.pending[id] = struct{}{}
			}
			state = openAIWebRunWaitingForApproval
		} else {
			delete(record.pending, id)
			if len(record.pending) > 0 {
				state = openAIWebRunWaitingForApproval
			} else {
				state = openAIWebRunRunning
			}
		}
	case RunFailure:
		record.failed = true
		state = openAIWebRunFailed
		reason = projected.ErrorKind
	case RunEnd:
		record.done = true
		terminal = true
		if record.failed || projected.Error != "" {
			state = openAIWebRunFailed
			reason = projected.ErrorKind
		} else {
			state = openAIWebRunDone
		}
	default:
		return openAIWebRunEvent{}, false
	}
	record.state = state
	record.next++
	at := projected.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result := openAIWebRunEvent{
		Cursor:          record.next,
		RunID:           record.id,
		SessionID:       record.sessionID,
		State:           state,
		ToolName:        toolName,
		ApprovalStatus:  approvalStatus,
		At:              at,
		Terminal:        terminal,
		Performance:     performance,
		Reason:          reason,
		FallbackProfile: fallbackProfile,
		NetworkOff:      record.networkOff,
	}
	if len(record.events) == maxOpenAIWebRunEvents {
		copy(record.events, record.events[1:])
		record.events[len(record.events)-1] = result
	} else {
		record.events = append(record.events, result)
	}
	close(record.notify)
	record.notify = make(chan struct{})
	return result, true
}

func (record *openAIWebRunRecord) status() (openAIWebRunEvent, bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if len(record.events) == 0 {
		return openAIWebRunEvent{}, false
	}
	return record.events[len(record.events)-1], true
}

func (record *openAIWebRunRecord) after(after uint64) ([]openAIWebRunEvent, <-chan struct{}, bool, bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	if len(record.events) > 0 {
		oldest := record.events[0].Cursor
		if oldest > 1 && after < oldest-1 {
			return nil, record.notify, record.done, true
		}
	}
	events := make([]openAIWebRunEvent, 0, len(record.events))
	for _, event := range record.events {
		if event.Cursor > after {
			events = append(events, event)
		}
	}
	return events, record.notify, record.done, false
}

func (record *openAIWebRunRecord) isDone() bool {
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.done
}

func openAIWebLifecycleRequested(request *http.Request) (bool, error) {
	raw := strings.TrimSpace(request.Header.Get(openAIWebRunLifecycleHeader))
	if raw == "" {
		return false, nil
	}
	if raw != "1" {
		return false, errors.New("X-Bee-Run-Lifecycle must be 1 when provided")
	}
	return true, nil
}

func writeOpenAIWebRunComment(writer io.Writer, event openAIWebRunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, ": bee-run %s\n\n", data)
	return err
}

func (handler *OpenAIHTTPHandler) handleCurrentRun(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "current run does not accept query parameters")
		return
	}
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	event, ok := handler.runLifecycle.current(conversationID)
	if !ok {
		writeOpenAIJSON(writer, http.StatusOK, openAIWebRunEvent{State: openAIWebRunIdle})
		return
	}
	writeOpenAIJSON(writer, http.StatusOK, event)
}

func (handler *OpenAIHTTPHandler) handleRunLifecyclePath(writer http.ResponseWriter, request *http.Request, suffix string) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || parts[1] != "events" || parts[0] == "" || len(parts[0]) > maxOpenAIWebRunIDBytes {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "run not found")
		return
	}
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	after := uint64(0)
	query, queryErr := url.ParseQuery(request.URL.RawQuery)
	if queryErr != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "run events query is invalid")
		return
	}
	if len(query) > 1 || (len(query) == 1 && (len(query["after"]) != 1 || query.Get("after") == "")) {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "run events accept only one after cursor")
		return
	}
	if raw := query.Get("after"); raw != "" {
		after, err = strconv.ParseUint(raw, 10, 64)
		if err != nil || len(raw) > 20 {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "after must be a non-negative integer")
			return
		}
	}
	record, ok := handler.runLifecycle.record(parts[0], conversationID)
	if !ok {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if _, _, _, gap := record.after(after); gap {
		writeOpenAIError(writer, http.StatusConflict, "conflict", "run lifecycle replay gap; reload durable session history")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "streaming is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(handler.heartbeatInterval)
	defer keepalive.Stop()
	deadline := time.NewTimer(openAIWebRunStreamDuration)
	defer deadline.Stop()
	for {
		events, notify, done, gap := record.after(after)
		if gap {
			return
		}
		for _, event := range events {
			data, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				return
			}
			if _, err := fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", event.Cursor, data); err != nil {
				return
			}
			after = event.Cursor
			flusher.Flush()
		}
		if done {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-notify:
		case <-keepalive.C:
			if _, err := io.WriteString(writer, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-deadline.C:
			return
		}
	}
}
