package bee

import "net/http"

func (handler *OpenAIHTTPHandler) handleInteractionPolicy(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	var sessionID SessionID
	if externalID := request.Header.Get("X-Session-Id"); externalID != "" {
		conversationID, err := validateOpenAIConversationID(externalID)
		if err != nil {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		sessionID, err = handler.lookupSession(request.Context(), openAIRouteKey(conversationID))
		if err != nil {
			writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load interaction policy session")
			return
		}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeOpenAIJSON(writer, http.StatusOK, handler.handlers.Application.InteractionPolicy(sessionID))
}
