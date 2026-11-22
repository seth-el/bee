package bee

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
)

const (
	MaxOpenAIWebCommandBodyBytes = 4 << 10
	MaxOpenAIWebCommandNameBytes = 32
	MaxOpenAIWebCommandArgsBytes = 2 << 10
)

type openAIWebCommandRequest struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

func (handler *OpenAIHTTPHandler) handleCommand(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "commands do not accept query parameters")
		return
	}

	input, ok := decodeOpenAIWebCommand(writer, request)
	if !ok {
		return
	}
	name := CommandName(input.Name)
	if reason := openAIWebCommandUnavailable(name, input.Args); reason != "" {
		writeOpenAIWebCommandError(writer, name, reason)
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
	var claimedSession SessionID
	defer func() { handler.releaseConversation(conversationID, claimedSession) }()

	sessionID, err := handler.lookupSession(request.Context(), openAIRouteKey(conversationID))
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "load command session mapping")
		return
	}
	if sessionID == "" {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "conversation does not select a Bee session")
		return
	}
	if err := handler.claimSession(conversationID, sessionID); err != nil {
		writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
		return
	}
	claimedSession = sessionID

	turnLease, err := handler.handlers.Application.Store.acquireTurnLease(sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionBusy):
			writeOpenAIError(writer, http.StatusConflict, "conflict", ErrSessionBusy.Error())
		case errors.Is(err, ErrSessionNotFound):
			writeOpenAIError(writer, http.StatusNotFound, "not_found", "session not found")
		default:
			writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "inspect command session availability")
		}
		return
	}
	defer turnLease.Close()

	commandInput := "/" + input.Name
	if input.Args != "" {
		commandInput += " " + input.Args
	}
	commandContext, cancel := context.WithTimeout(request.Context(), handler.requestTimeout)
	defer cancel()
	gatewayReady := true
	model := ""
	if len(handler.modelIDs) > 0 {
		model = handler.modelIDs[0]
	}
	result := DispatchCommand(commandContext, commandInput, CommandContext{
		Application:  handler.handlers.Application,
		SessionID:    sessionID,
		Model:        model,
		GatewayReady: &gatewayReady,
	})
	event := localCommandEvent(result)
	if event.Name == "" {
		event.Name = name
	}
	if result.CommandError() != nil {
		writeOpenAIJSON(writer, http.StatusBadRequest, event)
		return
	}
	writeOpenAIJSON(writer, http.StatusOK, event)
}

func decodeOpenAIWebCommand(writer http.ResponseWriter, request *http.Request) (openAIWebCommandRequest, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxOpenAIWebCommandBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input openAIWebCommandRequest
	if err := decoder.Decode(&input); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "command body must be one bounded JSON object with name and optional args")
		return openAIWebCommandRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "command body must contain one JSON object")
		return openAIWebCommandRequest{}, false
	}
	if input.Name == "" || input.Name != strings.TrimSpace(input.Name) || len(input.Name) > MaxOpenAIWebCommandNameBytes || !validOpenAIWebCommandName(input.Name) {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "command name must be a lowercase name without a leading slash")
		return openAIWebCommandRequest{}, false
	}
	input.Args = strings.TrimSpace(input.Args)
	if len(input.Args) > MaxOpenAIWebCommandArgsBytes {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "command arguments are too long")
		return openAIWebCommandRequest{}, false
	}
	return input, true
}

func validOpenAIWebCommandName(name string) bool {
	for index, value := range name {
		if value > unicode.MaxASCII || (!unicode.IsLower(value) && !unicode.IsDigit(value) && !(value == '-' && index > 0)) {
			return false
		}
	}
	return true
}

// openAIWebCommandUnavailable is deliberately narrower than isKnownCommand.
// Session routing, active-run control, media, and host speech remain owned by
// their dedicated adapters instead of being approximated by this endpoint.
func openAIWebCommandUnavailable(name CommandName, args string) string {
	switch name {
	case CommandStatus, CommandHelp, CommandSubagents, CommandTasks:
		if args == "" {
			return ""
		}
	case CommandPlan:
		return ""
	case CommandTools, CommandThinking, CommandToolLimit:
		return ""
	case CommandCompact:
		if args == "" {
			return ""
		}
	case CommandContinue:
		return ""
	case CommandInbox, CommandNetwork:
		return ""
	default:
		if isKnownCommand(string(name)) {
			return "command /" + string(name) + " is not available through the HTTP command endpoint"
		}
		return "unknown command /" + string(name)
	}
	return "command /" + string(name) + " does not accept arguments through the HTTP command endpoint"
}

func writeOpenAIWebCommandError(writer http.ResponseWriter, name CommandName, message string) {
	writeOpenAIJSON(writer, http.StatusBadRequest, LocalCommandEvent{
		Name: name,
		Kind: commandKindError,
		Text: boundedCommandText(message),
	})
}
