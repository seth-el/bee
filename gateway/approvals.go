package gateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/seth-el/bee"
)

func (service *Service) handleApprovals(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if service == nil || service.app == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"approvals": []bee.PendingApproval{}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": service.app.PendingApprovals()})
}

func (service *Service) handleApprovalDecision(writer http.ResponseWriter, request *http.Request, id string) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if service == nil || service.app == nil || strings.TrimSpace(id) == "" || strings.Contains(id, "/") {
		notFound(writer)
		return
	}
	var input struct {
		Decision bee.ApprovalDecision `json:"decision"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	if !input.Decision.ValidOwnerDecision() {
		writeError(writer, http.StatusBadRequest, errors.New("decision must be approved, approved_session, or denied"))
		return
	}
	if err := service.app.DecideApproval(id, input.Decision); err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"id": id, "decision": input.Decision})
}
