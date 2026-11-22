package gateway

import (
	"net/http"

	"github.com/seth-el/bee"
)

func (service *Service) handleInteractionPolicy(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	sessionID := bee.SessionID(request.URL.Query().Get("session_id"))
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, service.app.InteractionPolicy(sessionID))
}
