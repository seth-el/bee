package bee

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const MaxApprovalDecisionBodyBytes = 4 << 10

func (handler *OpenAIHTTPHandler) handleApprovals(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeOpenAIJSON(writer, http.StatusOK, map[string]any{"approvals": handler.handlers.Application.PendingApprovals()})
}

func (handler *OpenAIHTTPHandler) handleApprovalDecision(writer http.ResponseWriter, request *http.Request, id string) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "approval request not found")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, MaxApprovalDecisionBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Decision ApprovalDecision `json:"decision"`
	}
	if err := decoder.Decode(&input); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "invalid approval decision")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "approval decision must contain one JSON object")
		return
	}
	if !input.Decision.ValidOwnerDecision() {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "decision must be approved, approved_session, or denied")
		return
	}
	if err := handler.handlers.Application.DecideApproval(id, input.Decision); err != nil {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "approval request not found")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeOpenAIJSON(writer, http.StatusAccepted, map[string]any{"id": id, "decision": input.Decision})
}
