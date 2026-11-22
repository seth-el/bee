package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

type attachmentCaptureProvider struct {
	mu      sync.Mutex
	request bee.Request
	seen    chan struct{}
}

func (provider *attachmentCaptureProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, nil
}
func (provider *attachmentCaptureProvider) Stream(_ context.Context, request bee.Request, onDelta bee.StreamFunc) (bee.Response, error) {
	provider.mu.Lock()
	provider.request = request
	provider.mu.Unlock()
	select {
	case provider.seen <- struct{}{}:
	default:
	}
	_ = onDelta("ok")
	return bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: "ok"}}, nil
}

func TestGatewayAttachmentUploadStatusAndMessageReferences(t *testing.T) {
	provider := &attachmentCaptureProvider{seen: make(chan struct{}, 1)}
	service := newTestService(t, provider)
	defer service.Stop(context.Background())
	artifactStore, err := bee.NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), service.App().Store)
	if err != nil {
		t.Fatal(err)
	}
	attachmentStore, err := bee.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifactStore)
	if err != nil {
		t.Fatal(err)
	}
	service.App().ArtifactStore = artifactStore
	service.App().AttachmentStore = attachmentStore
	session, err := service.store.Create(bee.SessionMetadata{Title: "upload"})
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "../notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("bounded attachment context"))
	_ = writer.Close()
	upload := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(session.ID)+"/attachments", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	service.Handler().ServeHTTP(upload, request)
	if upload.Code != http.StatusCreated || upload.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("upload=%d headers=%v body=%s", upload.Code, upload.Header(), upload.Body.String())
	}
	var attachment bee.Attachment
	if err := json.Unmarshal(upload.Body.Bytes(), &attachment); err != nil {
		t.Fatal(err)
	}
	if attachment.Filename != "notes.txt" || attachment.Status != "uploaded" || attachment.ArtifactID == "" {
		t.Fatalf("attachment=%#v", attachment)
	}

	status := httptest.NewRecorder()
	service.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/attachments/"+attachment.ID, nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), attachment.ID) {
		t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
	}
	list := httptest.NewRecorder()
	service.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/sessions/"+string(session.ID)+"/attachments", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), attachment.ID) {
		t.Fatalf("list=%d body=%s", list.Code, list.Body.String())
	}

	message := httptest.NewRecorder()
	messageRequest := httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(session.ID)+"/messages", strings.NewReader(`{"prompt":"read it","attachment_ids":["`+attachment.ID+`"]}`))
	messageRequest.Header.Set("Content-Type", "application/json")
	service.Handler().ServeHTTP(message, messageRequest)
	if message.Code != http.StatusAccepted {
		t.Fatalf("message=%d body=%s", message.Code, message.Body.String())
	}
	select {
	case <-provider.seen:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not receive attachment turn")
	}
	provider.mu.Lock()
	captured := provider.request
	provider.mu.Unlock()
	var foundRef, foundContext bool
	for _, item := range captured.Messages {
		if item.Role == bee.RoleUser && len(item.Attachments) == 1 && item.Attachments[0].ID == attachment.ID {
			foundRef = true
		}
		if item.Role == bee.RoleSystem && strings.Contains(item.Content, "bounded attachment context") {
			foundContext = true
		}
	}
	if !foundRef || !foundContext {
		t.Fatalf("provider messages missing ref/context: %#v", captured.Messages)
	}
}

func TestGatewayStaticPageAttachmentMarkers(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()
	for _, marker := range []string{"attachment-input", "attachment-chips", "upload-progress", "uploadOne", "watchAttachment", "attachment-card-preview", "attachment_ids"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing attachment marker %q", marker)
		}
	}
}

func TestGatewayStaticPageAttachmentChronologyAndDedupMarkers(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	page := response.Body.String()

	uploadStart := strings.Index(page, "async function uploadFiles(files)")
	watchStart := strings.Index(page, "async function watchAttachment(id")
	if uploadStart < 0 || watchStart <= uploadStart {
		t.Fatal("static page is missing upload/watch function boundaries")
	}
	if strings.Contains(page[uploadStart:watchStart], "addAttachmentCard(") {
		t.Fatal("uploadFiles must keep uploaded attachments in composer chips until send")
	}

	for _, marker := range []string{
		"const pendingAttachments = state.attachments.slice();",
		"for (const attachment of pendingAttachments) addAttachmentCard(attachment);",
		"const attachmentsByID = new Map(",
		"const attachmentsByArtifactID = new Map(",
		"for (const ref of (message.attachments || []))",
		"card.dataset.artifactId = artifactID;",
		"[data-attachment-id], [data-artifact-id]",
		"Older histories may not expose attachment refs",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("static page missing attachment chronology/dedup marker %q", marker)
		}
	}

	userIndex := strings.Index(page, "addMessage('user', submittedText);")
	pendingIndex := strings.Index(page, "for (const attachment of pendingAttachments) addAttachmentCard(attachment);")
	assistantIndex := strings.Index(page, "assistantBubble = addMessage('assistant', 'thinking', true);")
	if userIndex < 0 || pendingIndex < 0 || assistantIndex < 0 || !(userIndex < pendingIndex && pendingIndex < assistantIndex) {
		t.Fatal("send attachment order must be user message, attachments, then assistant bubble")
	}
}
