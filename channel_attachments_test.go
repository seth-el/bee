package bee

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newChannelImageFixture(t *testing.T, files map[string]ChannelFile) (*ChannelRouter, *Application, *webImageCaptureProvider, *LocalRouteBindingStore) {
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
	channel, err := NewInMemoryChannel("fake")
	if err != nil {
		t.Fatal(err)
	}
	routes, err := NewLocalRouteBindingStore(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewChannelRouter(app, []Channel{channel}, ChannelRouterOptions{
		RouteBindings:  routes,
		ReplyOnFailure: true,
		AttachmentReader: func(_ context.Context, ref AttachmentRef) (ChannelFile, error) {
			file, ok := files[ref.Reference]
			if !ok {
				return ChannelFile{Filename: ref.Filename}, errors.New("gone")
			}
			return file, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return router, app, provider, routes
}

func lastUserMessage(t *testing.T, provider *webImageCaptureProvider) Message {
	t.Helper()
	if len(provider.requests) == 0 {
		t.Fatal("provider was not called")
	}
	messages := provider.requests[len(provider.requests)-1].Messages
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return messages[i]
		}
	}
	t.Fatal("no user message")
	return Message{}
}

func TestChannelImageReachesTheModelWithAndWithoutCaption(t *testing.T) {
	png := testPNG(t)
	router, app, provider, routes := newChannelImageFixture(t, map[string]ChannelFile{
		"img-1": {Filename: "photo", MediaType: "image/png", Data: png},
		"doc-1": {Filename: "notes.pdf", MediaType: "application/pdf", Data: []byte("%PDF-1.4")},
	})
	// Image only, first message on the route: a session is created and bound.
	result, err := router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "a", ConversationID: "c", MessageID: "m1", Attachments: []AttachmentRef{{Reference: "img-1", Filename: "photo", ContentType: "image/png"}}})
	if err != nil || result.Outbound == nil || result.Outbound.Text != "I can see it." {
		t.Fatalf("image-only = %#v, %v", result, err)
	}
	user := lastUserMessage(t, provider)
	if user.Content != ChannelImageOnlyPrompt || len(user.Attachments) != 1 || user.Attachments[0].Kind != "image" {
		t.Fatalf("user message = %#v", user)
	}
	key, _ := NewSessionRouteKey("fake", "a", "c", "")
	if bound, err := routes.Lookup(context.Background(), key); err != nil || bound != result.SessionID {
		t.Fatalf("route bound to %q, %v; session %q", bound, err, result.SessionID)
	}
	if stored, err := app.AttachmentStore.List(result.SessionID); err != nil || len(stored) != 1 || !strings.HasSuffix(stored[0].Filename, ".png") {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	// Caption plus an image and a non-image: the image goes in, the PDF is named.
	result2, err := router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "a", ConversationID: "c", MessageID: "m2", Text: "what is this?", Attachments: []AttachmentRef{{Reference: "img-1", Filename: "photo"}, {Reference: "doc-1", Filename: "notes.pdf"}}})
	if err != nil || result2.SessionID != result.SessionID {
		t.Fatalf("second = %#v, %v", result2, err)
	}
	user = lastUserMessage(t, provider)
	if !strings.HasPrefix(user.Content, "what is this?") || !strings.Contains(user.Content, "notes.pdf (only images") || len(user.Attachments) != 1 {
		t.Fatalf("captioned user message = %q, %d attachments", user.Content, len(user.Attachments))
	}
}

func TestChannelImageFailuresAreNamedNotFatal(t *testing.T) {
	router, _, provider, _ := newChannelImageFixture(t, map[string]ChannelFile{
		"bad": {Filename: "fake.png", MediaType: "image/png", Data: []byte("not a png")},
	})
	result, err := router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "a", ConversationID: "c", MessageID: "m1", Text: "look", Attachments: []AttachmentRef{{Reference: "bad", Filename: "fake.png"}, {Reference: "missing", Filename: "lost.jpg"}}})
	if err != nil || result.Outbound == nil {
		t.Fatalf("result = %#v, %v", result, err)
	}
	user := lastUserMessage(t, provider)
	if len(user.Attachments) != 0 || !strings.Contains(user.Content, "fake.png (") || !strings.Contains(user.Content, "lost.jpg (could not be downloaded)") {
		t.Fatalf("user message = %q", user.Content)
	}
}

func TestChannelWithoutAttachmentReaderStillRejectsMediaOnly(t *testing.T) {
	f := newChannelCommandFixture(t, 0, ChannelRouterOptions{})
	_, err := f.router.Handle(context.Background(), InboundMessage{ChannelID: "fake", AccountID: "account", ConversationID: "chat", MessageID: "m1", Attachments: []AttachmentRef{{Reference: "x", Filename: "x.png"}}})
	if err == nil {
		t.Fatal("media-only message accepted without an attachment reader")
	}
}
