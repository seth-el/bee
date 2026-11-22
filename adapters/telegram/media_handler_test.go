package telegram

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/seth-el/bee"
)

func TestMediaHandlerStagesPhotoForRouterAndCleansUp(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	payload := encoded.Bytes()
	server, _ := attachmentFileServer(payload, "image/png", "photos/a.png")
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, err := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	media := &MediaHandler{Config: config.Normalized(), Client: client, Attachments: store}
	var got bee.InboundMessage
	var read bee.ChannelFile
	media.Handler = bee.ChannelHandlerFunc(func(ctx context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		got = message
		var err error
		read, err = media.ReadAttachment(ctx, message.Attachments[0])
		return bee.ChannelRouteResult{}, err
	})
	update := Update{UpdateID: 5, Message: &Message{MessageID: 9, From: &User{ID: 7}, Chat: Chat{ID: 42}, Caption: "what's this?", Photo: []PhotoSize{{FileID: "doc", FileUniqueID: "u1", Width: 4, Height: 4, FileSize: int64(len(payload))}}}}
	if _, err := media.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if got.Text != "what's this?" || len(got.Attachments) != 1 {
		t.Fatalf("message = %#v", got)
	}
	if !bytes.Equal(read.Data, payload) || read.MediaType != "image/png" {
		t.Fatalf("read %d bytes as %q", len(read.Data), read.MediaType)
	}
	if entries, _ := os.ReadDir(store.Root()); len(entries) != 0 {
		t.Fatalf("staging not cleaned: %d entries", len(entries))
	}
	// Plain text still routes normally.
	got = bee.InboundMessage{}
	media.Handler = bee.ChannelHandlerFunc(func(_ context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		got = message
		return bee.ChannelRouteResult{}, nil
	})
	if _, err := media.HandleUpdate(context.Background(), Update{UpdateID: 6, Message: &Message{MessageID: 10, From: &User{ID: 7}, Chat: Chat{ID: 42}, Text: "hi"}}); err != nil || got.Text != "hi" {
		t.Fatalf("text = %q, %v", got.Text, err)
	}
	// Unknown senders' photos are ignored before any download.
	got = bee.InboundMessage{}
	if _, err := media.HandleUpdate(context.Background(), Update{UpdateID: 7, Message: &Message{MessageID: 11, From: &User{ID: 666}, Chat: Chat{ID: 666}, Photo: []PhotoSize{{FileID: "doc", Width: 4, Height: 4}}}}); err != nil || got.Text != "" || got.MessageID != "" {
		t.Fatalf("unknown sender routed: %#v, %v", got, err)
	}
}
