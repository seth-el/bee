package bee

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type imageSidecarCaptureProvider struct {
	mu          sync.Mutex
	requests    []Request
	response    string
	err         error
	supportsImg bool
}

func (provider *imageSidecarCaptureProvider) Complete(_ context.Context, request Request) (Response, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	err := provider.err
	response := provider.response
	provider.mu.Unlock()
	if err != nil {
		return Response{}, err
	}
	return Response{Message: Message{Role: RoleAssistant, Content: response}, Model: request.Model}, nil
}

func (provider *imageSidecarCaptureProvider) Stream(_ context.Context, request Request, onDelta StreamFunc) (Response, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	err := provider.err
	response := provider.response
	provider.mu.Unlock()
	if err != nil {
		return Response{}, err
	}
	if onDelta != nil && response != "" {
		if err := onDelta(response); err != nil {
			return Response{}, err
		}
	}
	return Response{Message: Message{Role: RoleAssistant, Content: response}, Model: request.Model}, nil
}

func (provider *imageSidecarCaptureProvider) SupportsImageInput() bool { return provider.supportsImg }

func (provider *imageSidecarCaptureProvider) snapshotRequests() []Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	result := make([]Request, len(provider.requests))
	copy(result, provider.requests)
	return result
}

func sidecarTestConfig(enabled bool) Config {
	config := DefaultConfig()
	config.Specialist.ImageSidecarEnabled = enabled
	for index := range config.Profiles {
		if config.Profiles[index].Name == "local-child-8082" {
			config.Profiles[index].Provider.ImageInput = true
		}
	}
	return config
}

func newImageSidecarTestApp(t *testing.T, config Config, mainProvider, specialistProvider Provider) (*Application, *Session, *AttachmentStore, *ArtifactStore) {
	t.Helper()
	sessions, err := NewSessionStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "image sidecar"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	transcriptScript := filepath.Join(t.TempDir(), "transcribe.sh")
	if err := os.WriteFile(transcriptScript, []byte("#!/bin/sh\nprintf 'audio transcript\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts, AttachmentStoreOptions{AudioScriptPath: transcriptScript})
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(config, mainProvider, sessions)
	if err != nil {
		t.Fatal(err)
	}
	specialist, err := newConfiguredChildSpecialistService(specialistProvider, "local", "local-child-8082", "http://127.0.0.1:8082", config.Specialist)
	if err != nil {
		t.Fatal(err)
	}
	specialist.SetAttachmentResolver(artifacts)
	app.Specialist = specialist
	app.ArtifactStore = artifacts
	app.AttachmentStore = attachments
	return app, session, attachments, artifacts
}

func runImageSidecarTurn(t *testing.T, app *Application, session *Session, ids []string) {
	t.Helper()
	events, err := app.Run(context.Background(), RunRequest{SessionID: session.ID, Prompt: "describe the attachments", AttachmentIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func requestHasAttachment(request Request, id string) bool {
	for _, message := range request.Messages {
		for _, attachment := range message.Attachments {
			if attachment.ID == id {
				return true
			}
		}
	}
	return false
}

func requestHasImageAttachment(request Request) bool {
	for _, message := range request.Messages {
		for _, attachment := range message.Attachments {
			if attachment.Kind == "image" || strings.HasPrefix(strings.ToLower(attachment.MediaType), "image/") {
				return true
			}
		}
	}
	return false
}

func requestContextContains(request Request, needle string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func TestImageSidecarRoutesImageOnlyToLocalSpecialistAndPreservesDurability(t *testing.T) {
	main := &imageSidecarCaptureProvider{response: "main", supportsImg: true}
	specialist := &imageSidecarCaptureProvider{response: `{"summary":"a diagram","claims":["one claim"]}`, supportsImg: true}
	app, session, attachments, _ := newImageSidecarTestApp(t, sidecarTestConfig(true), main, specialist)
	image, err := attachments.Create(context.Background(), session.ID, "photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	text, err := attachments.Create(context.Background(), session.ID, "notes.txt", "text/plain", []byte("text attachment"))
	if err != nil {
		t.Fatal(err)
	}
	audio, err := attachments.Create(context.Background(), session.ID, "voice.wav", "audio/wav", append([]byte("RIFF"), make([]byte, 40)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachments.ProcessAudio(context.Background(), audio.ID); err != nil {
		t.Fatal(err)
	}
	runImageSidecarTurn(t, app, session, []string{image.ID, text.ID, audio.ID})

	mainRequests := main.snapshotRequests()
	if len(mainRequests) != 1 || requestHasImageAttachment(mainRequests[0]) {
		t.Fatalf("main requests=%#v", mainRequests)
	}
	if !requestHasAttachment(mainRequests[0], text.ID) || !requestHasAttachment(mainRequests[0], audio.ID) {
		t.Fatalf("non-image attachments were not preserved: %#v", mainRequests[0])
	}
	if !requestContextContains(mainRequests[0], "a diagram") || !requestContextContains(mainRequests[0], "UNTRUSTED IMAGE OBSERVATION") || !requestContextContains(mainRequests[0], "text attachment") || !requestContextContains(mainRequests[0], "audio transcript") {
		t.Fatalf("main context=%#v", mainRequests[0].Messages)
	}
	specialistRequests := specialist.snapshotRequests()
	if len(specialistRequests) != 1 || !requestHasAttachment(specialistRequests[0], image.ID) || !requestHasImageAttachment(specialistRequests[0]) {
		t.Fatalf("specialist requests=%#v", specialistRequests)
	}
	stored, err := app.Store.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := stored.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) == 0 || !requestHasAttachment(Request{Messages: durable}, image.ID) {
		t.Fatalf("durable messages lost original image ref: %#v", durable)
	}
}

func TestImageSidecarDisabledPreservesDirectMainImageBehavior(t *testing.T) {
	main := &imageSidecarCaptureProvider{response: "main", supportsImg: true}
	specialist := &imageSidecarCaptureProvider{response: `{"summary":"should not run"}`, supportsImg: true}
	app, session, attachments, _ := newImageSidecarTestApp(t, sidecarTestConfig(false), main, specialist)
	image, err := attachments.Create(context.Background(), session.ID, "photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	runImageSidecarTurn(t, app, session, []string{image.ID})
	requests := main.snapshotRequests()
	if len(requests) != 1 || !requestHasImageAttachment(requests[0]) {
		t.Fatalf("direct image requests=%#v", requests)
	}
	if got := len(specialist.snapshotRequests()); got != 0 {
		t.Fatalf("disabled sidecar made %d specialist calls", got)
	}
}

func TestImageSidecarFailureNeverSendsImageToMainAndInjectsBoundedNote(t *testing.T) {
	main := &imageSidecarCaptureProvider{response: "main", supportsImg: true}
	specialist := &imageSidecarCaptureProvider{err: errors.New("specialist provider failed"), supportsImg: true}
	app, session, attachments, _ := newImageSidecarTestApp(t, sidecarTestConfig(true), main, specialist)
	image, err := attachments.Create(context.Background(), session.ID, "photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	runImageSidecarTurn(t, app, session, []string{image.ID})
	requests := main.snapshotRequests()
	if len(requests) != 1 || requestHasImageAttachment(requests[0]) {
		t.Fatalf("failed sidecar leaked image=%#v", requests)
	}
	if !requestContextContains(requests[0], "image sidecar could not interpret that attachment") {
		t.Fatalf("missing bounded sidecar failure note=%#v", requests[0].Messages)
	}
	if requestContextContains(requests[0], "provider failed") || requestContextContains(requests[0], "photo.png") {
		t.Fatalf("sidecar failure leaked internals=%#v", requests[0].Messages)
	}
}

func TestStripImageAttachmentsPreservesNonImageRefs(t *testing.T) {
	messages := []Message{{Role: RoleUser, Attachments: []AttachmentRef{{ID: "image", Kind: "image"}, {ID: "text", Kind: "text"}, {ID: "audio", MediaType: "audio/wav"}}}}
	stripped := stripImageAttachmentsFromMessages(messages)
	if len(stripped) != 1 || len(stripped[0].Attachments) != 2 || stripped[0].Attachments[0].ID != "text" || stripped[0].Attachments[1].ID != "audio" {
		t.Fatalf("stripped=%#v", stripped)
	}
	if len(messages[0].Attachments) != 3 {
		t.Fatal("strip mutated durable source messages")
	}
}

func TestImageSidecarConfigRejectsImageOutsideRoutedApplicationTurn(t *testing.T) {
	config := sidecarTestConfig(true)
	provider := &imageSidecarCaptureProvider{response: "main", supportsImg: true}
	agent, err := NewAgent(config, provider)
	if err != nil {
		t.Fatal(err)
	}
	image := AttachmentRef{ID: "direct-image", Kind: "image", MediaType: "image/png"}
	if _, err := agent.RunWithAttachments(context.Background(), "direct compatibility turn", []AttachmentRef{image}, "", nil); err == nil {
		t.Fatal("direct Agent path accepted an image without executing the configured sidecar")
	}
	if requests := provider.snapshotRequests(); len(requests) != 0 {
		t.Fatalf("direct Agent path leaked image to provider: %#v", requests)
	}
}
