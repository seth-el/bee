package bee

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxOpenAIWebSessionSummaries bounds the session metadata returned to a web
// client in one response. Application.ListSessions already orders pinned
// sessions first and then newest-first, so truncation retains the most useful
// entries without exposing any session contents.
const (
	MaxOpenAIWebSessionSummaries    = 200
	MaxOpenAIWebSessionBodyBytes    = 4 << 10
	MaxOpenAIWebHistoryMessages     = 200
	MaxOpenAIWebHistoryMessageBytes = 64 << 10
	MaxOpenAIWebHistoryBytes        = 2 << 20
)

type openAIWebSessionBinding struct {
	SessionID      SessionID `json:"session_id"`
	ConversationID string    `json:"conversation_id"`
}

type openAIWebHistoryMessage struct {
	Role             Role                  `json:"role"`
	Content          string                `json:"content"`
	CreatedAt        time.Time             `json:"created_at"`
	Incomplete       bool                  `json:"incomplete,omitempty"`
	ContentTruncated bool                  `json:"content_truncated,omitempty"`
	Attachments      []openAIWebAttachment `json:"attachments,omitempty"`
}

type openAIWebSessionHistory struct {
	SessionID SessionID                 `json:"session_id"`
	Messages  []openAIWebHistoryMessage `json:"messages"`
	HasMore   bool                      `json:"has_more"`
}

func (handler *OpenAIHTTPHandler) handleSessions(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	switch request.Method {
	case http.MethodGet:
		handler.handleListSessions(writer, request)
	case http.MethodPost:
		handler.handleCreateWebSession(writer, request)
	default:
		writer.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (handler *OpenAIHTTPHandler) handleListSessions(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session listing does not accept a request body")
		return
	}
	if request.URL.RawQuery != "" {
		handler.handleSearchWebSessions(writer, request)
		return
	}
	summaries, err := handler.handlers.Application.ListSessions()
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "list sessions")
		return
	}
	if len(summaries) > MaxOpenAIWebSessionSummaries {
		summaries = summaries[:MaxOpenAIWebSessionSummaries]
		writer.Header().Set("X-Bee-Sessions-Truncated", "true")
	}
	if summaries == nil {
		summaries = []SessionSummary{}
	}
	writeOpenAIJSON(writer, http.StatusOK, summaries)
}

func (handler *OpenAIHTTPHandler) handleSearchWebSessions(writer http.ResponseWriter, request *http.Request) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["q"]) != 1 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session search accepts exactly one q parameter")
		return
	}
	term := strings.TrimSpace(query.Get("q"))
	if term == "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session search query is required")
		return
	}
	if !utf8.ValidString(term) || len(term) > MaxSessionSearchQueryBytes || utf8.RuneCountInString(term) > MaxSessionSearchQueryBytes {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session search query is too long")
		return
	}
	results, err := handler.handlers.Application.Store.SearchSessions(SessionSearchOptions{
		Query:               term,
		Limit:               MaxSessionSearchResults,
		DisplayMessagesOnly: true,
		MatchSummaryFields:  true,
	})
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "search sessions")
		return
	}
	if results == nil {
		results = []SessionSearchResult{}
	}
	writeOpenAIJSON(writer, http.StatusOK, results)
}

func (handler *OpenAIHTTPHandler) handleCreateWebSession(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session creation does not accept query parameters")
		return
	}
	if !decodeEmptyOpenAIJSONObject(writer, request) {
		return
	}
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !handler.admitWebSessionOperation(writer, conversationID) {
		return
	}
	var createdSessionID SessionID
	defer func() { handler.releaseConversation(conversationID, createdSessionID) }()

	routeKey := openAIRouteKey(conversationID)
	if existing, lookupErr := handler.lookupSession(request.Context(), routeKey); lookupErr != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load conversation session mapping")
		return
	} else if existing != "" {
		writeOpenAIError(writer, http.StatusConflict, "conflict", "conversation ID already selects a Bee session")
		return
	}

	session, err := handler.handlers.Application.Store.Create(SessionMetadata{Title: "openai:" + conversationID})
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "create session")
		return
	}
	createdSessionID = session.ID
	if err := handler.claimSession(conversationID, session.ID); err != nil {
		_ = handler.handlers.Application.Store.Delete(session.ID)
		writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err := handler.routeBindings.Bind(request.Context(), routeKey, session.ID); err != nil {
		_ = handler.handlers.Application.Store.Delete(session.ID)
		status := http.StatusInternalServerError
		typ := "server_error"
		message := "persist conversation session mapping"
		if errors.Is(err, ErrRouteBindingConflict) {
			status = http.StatusConflict
			typ = "conflict"
			message = "conversation ID already selects a Bee session"
		}
		writeOpenAIError(writer, status, typ, message)
		return
	}
	handler.handlers.Application.beginMoodSession(session.ID)
	writeOpenAIJSON(writer, http.StatusCreated, openAIWebSessionBinding{SessionID: session.ID, ConversationID: conversationID})
}

func (handler *OpenAIHTTPHandler) handleSessionPath(writer http.ResponseWriter, request *http.Request, suffix string) {
	writer.Header().Set("Cache-Control", "no-store")
	parts := strings.Split(suffix, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "attachments" {
		handler.handleWebSessionAttachments(writer, request, SessionID(parts[0]))
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "history" {
		handler.handleWebSessionHistory(writer, request, SessionID(parts[0]))
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] != "select" {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session selection does not accept query parameters")
		return
	}
	if !decodeEmptyOpenAIJSONObject(writer, request) {
		return
	}
	targetID := SessionID(parts[0])
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if conversationID != string(targetID) {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "X-Session-Id must match the selected Bee session ID")
		return
	}
	if _, err := handler.handlers.Application.Resume(targetID); err != nil {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "session not found")
		return
	}
	if !handler.admitWebSessionOperation(writer, conversationID) {
		return
	}
	claimed := false
	defer func() {
		if claimed {
			handler.releaseConversation(conversationID, targetID)
		} else {
			handler.releaseConversation(conversationID, "")
		}
	}()
	if err := handler.claimSession(conversationID, targetID); err != nil {
		writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
		return
	}
	claimed = true
	turnLease, err := handler.handlers.Application.Store.acquireTurnLease(targetID)
	if err != nil {
		if errors.Is(err, ErrSessionBusy) {
			writeOpenAIError(writer, http.StatusConflict, "conflict", ErrSessionBusy.Error())
			return
		}
		if errors.Is(err, ErrSessionNotFound) {
			writeOpenAIError(writer, http.StatusNotFound, "not_found", "session not found")
			return
		}
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "inspect session availability")
		return
	}
	defer turnLease.Close()

	routeKey := openAIRouteKey(conversationID)
	mapped, err := handler.lookupSession(request.Context(), routeKey)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load conversation session mapping")
		return
	}
	if mapped != "" && mapped != targetID {
		writeOpenAIError(writer, http.StatusConflict, "conflict", "conversation ID already selects a different Bee session")
		return
	}
	if mapped == "" {
		if err := handler.routeBindings.Bind(request.Context(), routeKey, targetID); err != nil {
			status := http.StatusInternalServerError
			typ := "server_error"
			message := "persist conversation session mapping"
			if errors.Is(err, ErrRouteBindingConflict) {
				status = http.StatusConflict
				typ = "conflict"
				message = "conversation ID already selects a different Bee session"
			}
			writeOpenAIError(writer, status, typ, message)
			return
		}
	}
	handler.handlers.Application.beginMoodSession(targetID)
	writeOpenAIJSON(writer, http.StatusOK, openAIWebSessionBinding{SessionID: targetID, ConversationID: conversationID})
}

func (handler *OpenAIHTTPHandler) handleWebSessionHistory(writer http.ResponseWriter, request *http.Request, sessionID SessionID) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if request.URL.RawQuery != "" || request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "session history does not accept query parameters or a request body")
		return
	}
	session, err := handler.handlers.Application.Resume(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeOpenAIError(writer, http.StatusNotFound, "not_found", "session not found")
			return
		}
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load session history")
		return
	}
	messages, hasMore, err := projectOpenAIWebHistory(session.EventsSnapshot())
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "project session history")
		return
	}
	writeOpenAIJSON(writer, http.StatusOK, openAIWebSessionHistory{SessionID: sessionID, Messages: messages, HasMore: hasMore})
}

func projectOpenAIWebHistory(events []Event) ([]openAIWebHistoryMessage, bool, error) {
	reversed := make([]openAIWebHistoryMessage, 0, minInt(len(events), MaxOpenAIWebHistoryMessages))
	totalBytes := 0
	hasMore := false
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != EventMessage {
			continue
		}
		payload, err := DecodeMessageEvent(events[index])
		if err != nil {
			return nil, false, err
		}
		if payload.Message.Role != RoleUser && payload.Message.Role != RoleAssistant {
			continue
		}
		projected := ProjectMessageForDisplay(payload.Message)
		attachments := projectOpenAIWebAttachments(projected.Attachments)
		if strings.TrimSpace(projected.Content) == "" && len(attachments) == 0 {
			continue
		}
		content := projected.Content
		contentTruncated := len(content) > MaxOpenAIWebHistoryMessageBytes
		if contentTruncated {
			content = truncateUTF8(content, MaxOpenAIWebHistoryMessageBytes)
		}
		messageBytes := len(content)
		for _, attachment := range attachments {
			messageBytes += len(attachment.ID) + len(attachment.Filename) + len(attachment.MediaType) + len(attachment.Preview)
		}
		if len(reversed) >= MaxOpenAIWebHistoryMessages || (len(reversed) > 0 && totalBytes+messageBytes > MaxOpenAIWebHistoryBytes) {
			hasMore = true
			break
		}
		reversed = append(reversed, openAIWebHistoryMessage{
			Role:             projected.Role,
			Content:          content,
			CreatedAt:        events[index].At,
			Incomplete:       projected.Incomplete,
			ContentTruncated: contentTruncated,
			Attachments:      attachments,
		})
		totalBytes += messageBytes
	}
	messages := make([]openAIWebHistoryMessage, len(reversed))
	for index := range reversed {
		messages[len(reversed)-1-index] = reversed[index]
	}
	return messages, hasMore, nil
}

func (handler *OpenAIHTTPHandler) admitWebSessionOperation(writer http.ResponseWriter, conversationID string) bool {
	if err := handler.admitConversation(conversationID); err != nil {
		status := http.StatusTooManyRequests
		typ := "rate_limit_error"
		if errors.Is(err, errOpenAIConversationBusy) {
			status = http.StatusConflict
			typ = "conflict"
		}
		writeOpenAIError(writer, status, typ, err.Error())
		return false
	}
	return true
}

func decodeEmptyOpenAIJSONObject(writer http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxOpenAIWebSessionBodyBytes)
	decoder := json.NewDecoder(request.Body)
	var input map[string]json.RawMessage
	if err := decoder.Decode(&input); err != nil || input == nil || len(input) != 0 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "request body must be an empty JSON object")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "request body must contain one empty JSON object")
		return false
	}
	return true
}
