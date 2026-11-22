package bee

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxOpenAIWebImageBytes            int64 = 8 << 20
	MaxOpenAIWebImageUploadOverhead         = 64 << 10
	MaxOpenAIWebImagePreviewDimension       = 512
	MaxOpenAIWebImagePreviewPixels    int64 = MaxOpenAIWebImagePreviewDimension * MaxOpenAIWebImagePreviewDimension
	MaxOpenAIWebImagePreviewBytes     int64 = 2 << 20
	MaxOpenAIWebAttachmentIDBytes           = 128
)

var errOpenAIWebImageTooLarge = errors.New("image exceeds 8388608 byte limit")

type openAIWebAttachment struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size,omitempty"`
	Preview   string `json:"preview_url"`
}

func webAttachmentProjection(attachment Attachment) openAIWebAttachment {
	return openAIWebAttachment{
		ID: attachment.ID, Filename: attachment.Filename, MediaType: attachment.MediaType,
		Size: attachment.Size, Preview: "/api/attachments/" + attachment.ID + "/preview",
	}
}

func validOpenAIWebAttachmentID(id string) bool {
	if len(id) == 0 || len(id) > MaxOpenAIWebAttachmentIDBytes || validateAttachmentID(id) != nil {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func validOpenAIWebAttachmentFilename(filename string) bool {
	if !utf8.ValidString(filename) || strings.IndexFunc(filename, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
		return false
	}
	_, err := cleanArtifactFilename(filename)
	return err == nil
}

func projectOpenAIWebAttachments(refs []AttachmentRef) []openAIWebAttachment {
	attachments := make([]openAIWebAttachment, 0, minInt(len(refs), MaxProviderImageAttachments))
	for _, ref := range refs {
		mediaType := strings.ToLower(strings.TrimSpace(ref.MediaType))
		if ref.Kind != "image" && !strings.HasPrefix(mediaType, "image/") {
			continue
		}
		if len(attachments) >= MaxProviderImageAttachments {
			break
		}
		if !validOpenAIWebAttachmentID(ref.ID) || !validOpenAIWebAttachmentFilename(ref.Filename) {
			continue
		}
		if _, ok := supportedProviderImageMediaTypes[mediaType]; !ok {
			continue
		}
		attachments = append(attachments, openAIWebAttachment{
			ID: ref.ID, Filename: ref.Filename, MediaType: mediaType,
			Preview: "/api/attachments/" + ref.ID + "/preview",
		})
	}
	if len(attachments) == 0 {
		return nil
	}
	return attachments
}

func (handler *OpenAIHTTPHandler) handleWebSessionAttachments(writer http.ResponseWriter, request *http.Request, sessionID SessionID) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if request.URL.RawQuery != "" {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "attachment upload does not accept query parameters")
		return
	}
	if handler.handlers.Application.AttachmentStore == nil {
		writeOpenAIError(writer, http.StatusNotImplemented, "server_error", "attachment storage is unavailable")
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
	claimed := false
	defer func() {
		if claimed {
			handler.releaseConversation(conversationID, sessionID)
		} else {
			handler.releaseConversation(conversationID, "")
		}
	}()
	if err := handler.claimSession(conversationID, sessionID); err != nil {
		writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
		return
	}
	claimed = true
	if err := handler.requireWebSessionBinding(request, conversationID, sessionID); err != nil {
		writeOpenAIError(writer, http.StatusConflict, "conflict", err.Error())
		return
	}
	lease, err := handler.handlers.Application.Store.acquireTurnLease(sessionID)
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
	defer lease.Close()

	filename, declaredType, data, err := readOpenAIWebImageUpload(writer, request)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOpenAIWebImageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeOpenAIError(writer, status, "invalid_request_error", err.Error())
		return
	}
	mediaType, err := validateOpenAIWebImage(request.Context(), declaredType, data)
	if err != nil {
		writeOpenAIError(writer, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	attachment, err := handler.handlers.Application.AttachmentStore.Create(request.Context(), sessionID, filename, mediaType, data)
	if err != nil {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "store attachment")
		return
	}
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writeOpenAIJSON(writer, http.StatusCreated, webAttachmentProjection(attachment))
}

func readOpenAIWebImageUpload(writer http.ResponseWriter, request *http.Request) (string, string, []byte, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxOpenAIWebImageBytes+MaxOpenAIWebImageUploadOverhead)
	reader, err := request.MultipartReader()
	if err != nil {
		return "", "", nil, errors.New("request must be multipart/form-data with one file field")
	}
	part, err := reader.NextPart()
	if err != nil {
		return "", "", nil, errors.New("multipart field file is required")
	}
	defer part.Close()
	if part.FormName() != "file" || strings.TrimSpace(part.FileName()) == "" {
		return "", "", nil, errors.New("multipart field file with a filename is required")
	}
	if !validOpenAIWebAttachmentFilename(part.FileName()) {
		return "", "", nil, errors.New("image filename must be a safe bounded basename")
	}
	data, err := io.ReadAll(io.LimitReader(part, MaxOpenAIWebImageBytes+1))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return "", "", nil, errOpenAIWebImageTooLarge
		}
		return "", "", nil, errors.New("read image upload")
	}
	if int64(len(data)) > MaxOpenAIWebImageBytes {
		return "", "", nil, errOpenAIWebImageTooLarge
	}
	if len(data) == 0 {
		return "", "", nil, errors.New("image is empty")
	}
	if extra, extraErr := reader.NextPart(); extraErr != io.EOF {
		if extra != nil {
			_ = extra.Close()
		}
		if extraErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(extraErr, &tooLarge) {
				return "", "", nil, errOpenAIWebImageTooLarge
			}
		}
		return "", "", nil, errors.New("attachment upload accepts exactly one file field")
	}
	return part.FileName(), part.Header.Get("Content-Type"), data, nil
}

func validateOpenAIWebImage(ctx context.Context, declared string, data []byte) (string, error) {
	declaredType, _, err := mime.ParseMediaType(strings.TrimSpace(declared))
	if err != nil || declaredType == "" {
		return "", errors.New("image Content-Type is required and must be valid")
	}
	if _, ok := supportedProviderImageMediaTypes[declaredType]; !ok {
		return "", fmt.Errorf("image Content-Type %q is not allowed", declaredType)
	}
	detected := strings.Split(http.DetectContentType(data), ";")[0]
	if _, ok := supportedProviderImageMediaTypes[detected]; !ok {
		return "", errors.New("uploaded bytes are not a supported PNG, JPEG, GIF, or WebP image")
	}
	if detected != declaredType {
		return "", fmt.Errorf("declared image Content-Type %q does not match detected type %q", declaredType, detected)
	}
	if err := acquireImageProcessing(ctx); err != nil {
		return "", err
	}
	defer releaseImageProcessing()
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", errors.New("image metadata is malformed")
	}
	decodedType, err := imageFormatMediaType(format)
	if err != nil || decodedType != detected {
		return "", errors.New("image encoding does not match its detected media type")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxSpecialistImageSourceDimension || config.Height > MaxSpecialistImageSourceDimension || int64(config.Width)*int64(config.Height) > MaxSpecialistImageSourcePixels {
		return "", fmt.Errorf("image dimensions exceed the %d pixel safety limit", MaxSpecialistImageSourcePixels)
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", errors.New("image payload is malformed")
	}
	return detected, nil
}

func (handler *OpenAIHTTPHandler) requireWebSessionBinding(request *http.Request, conversationID string, sessionID SessionID) error {
	if _, err := handler.handlers.Application.Resume(sessionID); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return errors.New("session not found")
		}
		return errors.New("load session")
	}
	mapped, err := handler.lookupSession(request.Context(), openAIRouteKey(conversationID))
	if err != nil {
		return errors.New("load conversation session mapping")
	}
	if mapped != sessionID {
		return errors.New("X-Session-Id does not select the attachment session")
	}
	return nil
}

func (handler *OpenAIHTTPHandler) validateWebChatAttachments(sessionID SessionID, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxProviderImageAttachments {
		return fmt.Errorf("at most %d image attachments may be sent", MaxProviderImageAttachments)
	}
	if sessionID == "" {
		return errors.New("image attachments require an existing bound session")
	}
	store := handler.handlers.Application.AttachmentStore
	if store == nil {
		return errors.New("attachment storage is unavailable")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id != rawID || !validOpenAIWebAttachmentID(id) {
			return errors.New("attachment_ids contains an invalid attachment ID")
		}
		if _, duplicate := seen[id]; duplicate {
			return errors.New("attachment_ids contains a duplicate attachment ID")
		}
		seen[id] = struct{}{}
		attachment, err := store.Get(id)
		if err != nil || attachment.SessionID != sessionID {
			return errors.New("attachment does not belong to the selected session")
		}
		if attachment.Kind != "image" {
			return errors.New("only image attachments are supported")
		}
		if _, ok := supportedProviderImageMediaTypes[attachment.MediaType]; !ok {
			return errors.New("attachment has an unsupported image media type")
		}
	}
	return nil
}

func (handler *OpenAIHTTPHandler) handleWebAttachmentPath(writer http.ResponseWriter, request *http.Request, suffix string) {
	writer.Header().Set("Cache-Control", "private, no-store")
	parts := strings.Split(suffix, "/")
	if len(parts) != 2 || !validOpenAIWebAttachmentID(parts[0]) || parts[1] != "preview" {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if request.URL.RawQuery != "" || request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "attachment preview does not accept query parameters or a request body")
		return
	}
	store := handler.handlers.Application.AttachmentStore
	if store == nil || handler.handlers.Application.ArtifactStore == nil {
		writeOpenAIError(writer, http.StatusNotImplemented, "server_error", "attachment storage is unavailable")
		return
	}
	attachment, err := store.Get(parts[0])
	if err != nil || attachment.Kind != "image" {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	conversationID, err := validateOpenAIConversationID(request.Header.Get("X-Session-Id"))
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := handler.requireWebSessionBinding(request, conversationID, attachment.SessionID); err != nil {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	artifact, payload, err := handler.handlers.Application.ArtifactStore.OpenPayload(attachment.ArtifactID)
	if err != nil {
		writeOpenAIError(writer, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	defer payload.Close()
	data, err := io.ReadAll(io.LimitReader(payload, MaxOpenAIWebImageBytes+1))
	if err != nil || int64(len(data)) > MaxOpenAIWebImageBytes || artifact.SessionID != attachment.SessionID {
		writeOpenAIError(writer, http.StatusInternalServerError, "server_error", "read attachment preview")
		return
	}
	preview, err := renderOpenAIWebImagePreview(request.Context(), data)
	if err != nil {
		writeOpenAIError(writer, http.StatusUnprocessableEntity, "invalid_request_error", "render attachment preview")
		return
	}
	writer.Header().Set("Content-Type", "image/png")
	writer.Header().Set("Content-Length", strconv.Itoa(len(preview)))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(preview)
}

func renderOpenAIWebImagePreview(ctx context.Context, data []byte) ([]byte, error) {
	if err := acquireImageProcessing(ctx); err != nil {
		return nil, err
	}
	defer releaseImageProcessing()
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	width, height := proportionalImageDimensionsWithPixels(decoded.Bounds().Dx(), decoded.Bounds().Dy(), MaxOpenAIWebImagePreviewDimension, MaxOpenAIWebImagePreviewPixels)
	if width != decoded.Bounds().Dx() || height != decoded.Bounds().Dy() {
		decoded = resizeImage(decoded, width, height)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, decoded); err != nil {
		return nil, err
	}
	if int64(output.Len()) > MaxOpenAIWebImagePreviewBytes {
		return nil, errors.New("attachment preview exceeds encoded byte limit")
	}
	return output.Bytes(), nil
}
