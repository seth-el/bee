package bee

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// handleNotifications serves the job notification inbox to owner clients
// over HTTP with the same bounded projection and query contract as the legacy
// gateway: `limit` (1-50) and a `before` sequence cursor. The projection
// omits delivery keys, attempt IDs, and private failure details.
func (handler *OpenAIHTTPHandler) handleNotifications(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	store := handler.handlers.Application.NotificationStore
	if store == nil {
		writeOpenAIError(writer, http.StatusServiceUnavailable, "server_error", "notification inbox is unavailable")
		return
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification query is invalid")
		return
	}
	for key, values := range query {
		if (key != "limit" && key != "before") || len(values) != 1 {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification query accepts one limit and one before parameter")
			return
		}
	}
	limit := NotificationMaxListLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > NotificationMaxListLimit {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification limit is invalid")
			return
		}
		limit = parsed
	}
	var before uint64
	if raw := query.Get("before"); raw != "" {
		parsed, parseErr := strconv.ParseUint(raw, 10, 64)
		if parseErr != nil || parsed == 0 {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification cursor is invalid")
			return
		}
		before = parsed
	}
	result, err := store.ListBefore(request.Context(), limit, before)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "read notification inbox")
		return
	}
	writeOpenAIJSON(writer, http.StatusOK, result)
}

// handleNotificationPath serves POST /api/notifications/<id>/read, which is
// idempotent and only marks the item read; job state never changes here.
func (handler *OpenAIHTTPHandler) handleNotificationPath(writer http.ResponseWriter, request *http.Request, suffix string) {
	writer.Header().Set("Cache-Control", "no-store")
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "read" {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "notification route not found")
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification read does not accept query parameters")
		return
	}
	store := handler.handlers.Application.NotificationStore
	if store == nil {
		writeOpenAIError(writer, http.StatusServiceUnavailable, "server_error", "notification inbox is unavailable")
		return
	}
	item, err := store.MarkRead(request.Context(), parts[0])
	if err != nil {
		if errors.Is(err, ErrNotificationNotFound) {
			writeOpenAIError(writer, http.StatusNotFound, "not_found", "notification not found")
			return
		}
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "notification id is invalid")
		return
	}
	writeOpenAIJSON(writer, http.StatusOK, notificationItemView(item))
}
