package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type webImageCaptureProvider struct {
	requests []Request
}

func (*webImageCaptureProvider) SupportsImageInput() bool { return true }

func (provider *webImageCaptureProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.requests = append(provider.requests, request)
	return Response{Message: Message{Role: RoleAssistant, Content: "I can see it."}, FinishReason: "stop"}, nil
}

func (provider *webImageCaptureProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	response, err := provider.Complete(ctx, request)
	if err == nil && onDelta != nil {
		err = onDelta(response.Message.Content)
	}
	return response, err
}

func newOpenAIAttachmentTestHandler(t *testing.T) (*OpenAIHTTPHandler, *Application, *LocalRouteBindingStore, *webImageCaptureProvider) {
	t.Helper()
	store, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &webImageCaptureProvider{}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(artifacts.Root(), "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	app.ArtifactStore = artifacts
	app.AttachmentStore = attachments
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	return newOpenAITestHandler(t, app, routes, 2, time.Second), app, routes, provider
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 3, 2))
	picture.Set(0, 0, color.RGBA{R: 210, G: 80, B: 140, A: 255})
	if err := png.Encode(&output, picture); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func multipartImageRequest(t *testing.T, target, conversationID, filename, mediaType string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &body)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-Session-Id", conversationID)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func createBoundWebSession(t *testing.T, handler *OpenAIHTTPHandler, conversationID string) openAIWebSessionBinding {
	t.Helper()
	request := authenticatedSessionRequest(http.MethodPost, "/api/sessions", `{}`)
	request.Header.Set("X-Session-Id", conversationID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var binding openAIWebSessionBinding
	if err := json.Unmarshal(response.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestOpenAIHTTPImageAttachmentUploadChatAndHistory(t *testing.T) {
	handler, _, _, provider := newOpenAIAttachmentTestHandler(t)
	const conversationID = "3d9d0fd8-5cb8-4ac4-916d-a5851c3f5eef"
	binding := createBoundWebSession(t, handler, conversationID)
	target := "/api/sessions/" + string(binding.SessionID) + "/attachments"

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, target, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	upload := httptest.NewRecorder()
	handler.ServeHTTP(upload, multipartImageRequest(t, target, conversationID, "sample.png", "image/png", testPNG(t)))
	if upload.Code != http.StatusCreated || upload.Header().Get("Cache-Control") != "no-store" || upload.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("upload status=%d headers=%v body=%s", upload.Code, upload.Header(), upload.Body.String())
	}
	var attachment openAIWebAttachment
	if err := json.Unmarshal(upload.Body.Bytes(), &attachment); err != nil {
		t.Fatal(err)
	}
	if attachment.ID == "" || attachment.Filename != "sample.png" || attachment.MediaType != "image/png" || attachment.Size == 0 || !strings.HasSuffix(attachment.Preview, "/preview") {
		t.Fatalf("attachment=%#v", attachment)
	}

	chatBody, err := json.Marshal(openAIChatRequest{
		Model: "bee-assistant", Messages: []openAIChatMessage{{Role: "user", Content: json.RawMessage(`"Describe this image."`)}}, AttachmentIDs: []string{attachment.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, openAITestRequest(t, string(chatBody), conversationID))
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chat.Code, chat.Body.String())
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests=%d", len(provider.requests))
	}
	var attached Message
	for _, message := range provider.requests[0].Messages {
		if len(message.Attachments) > 0 {
			attached = message
		}
	}
	if len(attached.Attachments) != 1 || attached.Attachments[0].ID != attachment.ID || attached.Attachments[0].MediaType != "image/png" {
		t.Fatalf("provider messages=%#v", provider.requests[0].Messages)
	}

	historyRequest := authenticatedSessionRequest(http.MethodGet, "/api/sessions/"+string(binding.SessionID)+"/history", "")
	historyRequest.Header.Set("X-Session-Id", conversationID)
	history := httptest.NewRecorder()
	handler.ServeHTTP(history, historyRequest)
	if history.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", history.Code, history.Body.String())
	}
	var replay openAIWebSessionHistory
	if err := json.Unmarshal(history.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if len(replay.Messages) != 2 || len(replay.Messages[0].Attachments) != 1 || replay.Messages[0].Attachments[0].ID != attachment.ID {
		t.Fatalf("history=%#v", replay)
	}
	if strings.Contains(history.Body.String(), "iVBOR") {
		t.Fatalf("history embedded attachment bytes: %s", history.Body.String())
	}

	previewRequest := authenticatedSessionRequest(http.MethodGet, attachment.Preview, "")
	previewRequest.Header.Set("X-Session-Id", conversationID)
	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, previewRequest)
	if preview.Code != http.StatusOK || preview.Header().Get("Content-Type") != "image/png" || int64(preview.Body.Len()) > MaxOpenAIWebImagePreviewBytes {
		t.Fatalf("preview status=%d headers=%v bytes=%d body=%s", preview.Code, preview.Header(), preview.Body.Len(), preview.Body.String())
	}
}

func TestOpenAIHTTPImageAttachmentUploadRejectsMalformedSizeAndType(t *testing.T) {
	handler, _, _, _ := newOpenAIAttachmentTestHandler(t)
	const conversationID = "5dde6299-0b23-48ff-bab3-a42bf9505ce5"
	binding := createBoundWebSession(t, handler, conversationID)
	target := "/api/sessions/" + string(binding.SessionID) + "/attachments"
	pngData := testPNG(t)

	tests := []struct {
		name      string
		request   func() *http.Request
		wantCode  int
		wantError string
	}{
		{name: "oversize", request: func() *http.Request {
			return multipartImageRequest(t, target, conversationID, "large.png", "image/png", make([]byte, MaxOpenAIWebImageBytes+1))
		}, wantCode: http.StatusRequestEntityTooLarge, wantError: "exceeds"},
		{name: "disallowed type", request: func() *http.Request {
			return multipartImageRequest(t, target, conversationID, "note.txt", "text/plain", []byte("not an image"))
		}, wantCode: http.StatusUnsupportedMediaType, wantError: "not allowed"},
		{name: "declared mismatch", request: func() *http.Request {
			return multipartImageRequest(t, target, conversationID, "wrong.jpg", "image/jpeg", pngData)
		}, wantCode: http.StatusUnsupportedMediaType, wantError: "does not match"},
		{name: "malformed image", request: func() *http.Request {
			return multipartImageRequest(t, target, conversationID, "broken.png", "image/png", pngData[:20])
		}, wantCode: http.StatusUnsupportedMediaType, wantError: "malformed"},
		{name: "malformed multipart", request: func() *http.Request {
			request := authenticatedSessionRequest(http.MethodPost, target, "not multipart")
			request.Header.Set("X-Session-Id", conversationID)
			request.Header.Set("Content-Type", "application/json")
			return request
		}, wantCode: http.StatusBadRequest, wantError: "multipart"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request())
			if response.Code != test.wantCode || !strings.Contains(response.Body.String(), test.wantError) || !strings.Contains(response.Body.String(), `"error"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestOpenAIHTTPImageAttachmentRequiresSelectedSessionAndValidChatRefs(t *testing.T) {
	handler, _, _, _ := newOpenAIAttachmentTestHandler(t)
	const conversationID = "b80685eb-7f96-4d06-9c46-40686e600fc6"
	binding := createBoundWebSession(t, handler, conversationID)
	target := "/api/sessions/" + string(binding.SessionID) + "/attachments"

	wrongBinding := httptest.NewRecorder()
	handler.ServeHTTP(wrongBinding, multipartImageRequest(t, target, "unmapped-conversation", "sample.png", "image/png", testPNG(t)))
	if wrongBinding.Code != http.StatusConflict {
		t.Fatalf("wrong binding status=%d body=%s", wrongBinding.Code, wrongBinding.Body.String())
	}

	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, openAITestRequest(t, `{"model":"bee-assistant","messages":[{"role":"user","content":"Use the image."}],"attachment_ids":["../bad"]}`, conversationID))
	if chat.Code != http.StatusBadRequest || !strings.Contains(chat.Body.String(), "invalid attachment ID") {
		t.Fatalf("invalid refs status=%d body=%s", chat.Code, chat.Body.String())
	}
}
