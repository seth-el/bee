package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seth-el/bee"
)

func TestNormalizeAttachmentUpdateSelectsPhotoAndPreservesCaption(t *testing.T) {
	config := testConfig(t)
	update := Update{UpdateID: 901, Message: &Message{
		MessageID: 12, From: &User{ID: 7}, Chat: Chat{ID: 42}, Caption: "caption only",
		Photo: []PhotoSize{{FileID: "small", Width: 10, Height: 10, FileSize: 20}, {FileID: "large", Width: 100, Height: 80, FileSize: 200}},
	}}
	normalized, err := NormalizeAttachmentUpdate(config, update)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Ignored || normalized.Message.Text != "caption only" || len(normalized.RawAttachments) != 1 {
		t.Fatalf("normalized=%#v raw=%#v", normalized.Message, normalized.RawAttachments)
	}
	if normalized.RawAttachments[0].FileID != "large" || normalized.RawAttachments[0].Kind != AttachmentKindPhoto {
		t.Fatalf("selected photo=%#v", normalized.RawAttachments[0])
	}
	if strings.Contains(normalized.Message.Attachments[0].Reference, "large") || strings.Contains(normalized.Message.Attachments[0].Reference, "/") {
		t.Fatalf("reference leaked provider value: %#v", normalized.Message.Attachments[0])
	}
}

func TestNormalizeAttachmentOnlyDoesNotFabricateText(t *testing.T) {
	config := testConfig(t)
	normalized, err := NormalizeAttachmentUpdate(config, Update{UpdateID: 902, Message: &Message{
		MessageID: 13, From: &User{ID: 7}, Chat: Chat{ID: 42}, Document: &Document{FileID: "doc", FileName: "report.pdf", MimeType: "application/pdf", FileSize: 8},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Ignored || normalized.Message.Text != "" || len(normalized.Message.Attachments) != 1 {
		t.Fatalf("normalized=%#v", normalized.Message)
	}
}

func TestNormalizeDefaultPollerIgnoresCaptionOnlyUnsupportedMedia(t *testing.T) {
	config := testConfig(t)
	normalized, err := NormalizeUpdate(config, Update{UpdateID: 904, Message: &Message{
		MessageID: 15, From: &User{ID: 7}, Chat: Chat{ID: 42}, Caption: "caption on a document",
		Document: &Document{FileID: "doc", FileName: "report.pdf", MimeType: "application/pdf", FileSize: 8},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !normalized.Ignored || normalized.Reason != "unsupported or media-only message" {
		t.Fatalf("normalized=%#v", normalized)
	}
}

func TestTelegramChannelSendsOutboundAttachmentsInOrderAndCleans(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{1}, 16)...)
	server, _ := attachmentFileServer(pdf, "application/pdf", "docs/a.pdf")
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, _ := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"))
	staged, err := store.Stage(context.Background(), client, TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: int64(len(pdf))})
	if err != nil {
		t.Fatal(err)
	}
	ref := staged.Reference()
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body struct {
				ChatID int64 `json:"chat_id"`
				Reply  int64 `json:"reply_to_message_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.ChatID != 42 || body.Reply != 9 {
				t.Fatalf("metadata chat=%d reply=%d", body.ChatID, body.Reply)
			}
		} else {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "" {
				t.Fatalf("attachment metadata chat=%q reply=%q", r.FormValue("chat_id"), r.FormValue("reply_to_message_id"))
			}
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":10}}`)
	}))
	defer api.Close()
	client, _ = NewClient(api.URL, "123:secret-token", api.Client())
	channel, err := NewTelegramChannel(client, Config{ChannelID: "telegram", AccountID: "test-account", ExpectedBotID: 99, ExpectedBotUsername: "bee_test_bot", TokenFile: filepath.Join(t.TempDir(), "token"), APIBaseURL: api.URL, SessionRoot: filepath.Join(t.TempDir(), "sessions"), AllowedChats: []int64{42}, AllowedSenders: []int64{7}}, ChannelOptions{AttachmentStore: store, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Send(context.Background(), bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-p42", ReplyTo: "tg-msg-p9", Text: "caption", Attachments: []bee.AttachmentRef{ref}}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/sendMessage") || !strings.HasSuffix(paths[1], "/sendDocument") {
		t.Fatalf("paths=%#v", paths)
	}
	if _, err := os.Stat(staged.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact remains: %v", err)
	}
}

func TestTelegramChannelRetriesOutboundAttachment(t *testing.T) {
	payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{2}, 8)...)
	server, _ := attachmentFileServer(payload, "application/pdf", "docs/a.pdf")
	defer server.Close()
	stageClient, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, _ := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"))
	artifact, err := store.Stage(context.Background(), stageClient, TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendDocument") {
			http.NotFound(w, r)
			return
		}
		sends.Add(1)
		if sends.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"ok":false,"error_code":503}`)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := r.FormFile("document")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		if !bytes.Equal(data, payload) {
			t.Fatalf("retry payload changed")
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":11}}`)
	}))
	defer api.Close()
	client, _ := NewClient(api.URL, "123:secret-token", api.Client())
	channel, _ := NewTelegramChannel(client, Config{ChannelID: "telegram", AccountID: "test-account", ExpectedBotID: 99, ExpectedBotUsername: "bee_test_bot", TokenFile: filepath.Join(t.TempDir(), "token"), APIBaseURL: api.URL, SessionRoot: filepath.Join(t.TempDir(), "sessions"), AllowedChats: []int64{42}, AllowedSenders: []int64{7}}, ChannelOptions{AttachmentStore: store, MaxAttempts: 2, RetryDelay: 0, Sleep: func(context.Context, time.Duration) error { return nil }})
	if err := channel.Send(context.Background(), bee.OutboundMessage{ChannelID: "telegram", AccountID: "test-account", ConversationID: "tg-chat-p42", Attachments: []bee.AttachmentRef{artifact.Reference()}}); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 2 {
		t.Fatalf("sends=%d", sends.Load())
	}
}

func TestAttachmentArtifactStageValidatesChecksumContainmentAndCleanup(t *testing.T) {
	payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x01}, 32)...)
	server, counts := attachmentFileServer(payload, "application/pdf", "docs/report.pdf")
	defer server.Close()
	client, err := NewClient(server.URL, "123:secret-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "attachments")
	store, err := NewAttachmentArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Stage(context.Background(), client, TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "report.pdf", ContentType: "application/pdf", SizeBytes: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	ref := artifact.Reference()
	if strings.Contains(ref.Reference, "doc") || strings.Contains(ref.Reference, "/") || !strings.HasPrefix(ref.Reference, "tg-attachment-v1-") {
		t.Fatalf("opaque ref=%#v", ref)
	}
	if ref.SHA256 == "" || ref.SizeBytes != int64(len(payload)) || ref.Filename != "report.pdf" {
		t.Fatalf("ref=%#v", ref)
	}
	if !strings.HasPrefix(artifact.Path(), root+string(os.PathSeparator)) {
		t.Fatalf("path escaped root: %q", artifact.Path())
	}
	resolved, err := store.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	file, err := resolved.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := artifact.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact remains: %v", err)
	}
	if counts.getFile.Load() != 1 || counts.download.Load() != 1 {
		t.Fatalf("network counts getFile=%d download=%d", counts.getFile.Load(), counts.download.Load())
	}
}

func TestAttachmentArtifactStagesPhotoAndAudioVariants(t *testing.T) {
	photo := append([]byte("\xff\xd8\xff"), bytes.Repeat([]byte{0x03}, 12)...)
	audio := append([]byte("ID3"), bytes.Repeat([]byte{0x04}, 12)...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			var body struct {
				FileID string `json:"file_id"`
			}
			// getFile is JSON POST; the request body is intentionally bounded and
			// carries no secret beyond the fake's test token.
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.FileID == "photo" {
				fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"photo","file_size":%d,"file_path":"photos/a.jpg"}}`, len(photo))
			} else {
				fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"audio","file_size":%d,"file_path":"audio/a.mp3"}}`, len(audio))
			}
		case strings.HasSuffix(r.URL.Path, "/photos/a.jpg"):
			_, _ = w.Write(photo)
		case strings.HasSuffix(r.URL.Path, "/audio/a.mp3"):
			_, _ = w.Write(audio)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, _ := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"))
	photoArtifact, err := store.Stage(context.Background(), client, TelegramAttachment{Kind: AttachmentKindPhoto, FileID: "photo", SizeBytes: int64(len(photo)), Width: 20, Height: 10})
	if err != nil || !strings.HasPrefix(photoArtifact.Reference().ContentType, "image/") {
		t.Fatalf("photo artifact=%#v err=%v", photoArtifact, err)
	}
	defer photoArtifact.Cleanup()
	audioArtifact, err := store.Stage(context.Background(), client, TelegramAttachment{Kind: AttachmentKindAudio, FileID: "audio", ContentType: "audio/mpeg", SizeBytes: int64(len(audio))})
	if err != nil || audioArtifact.Reference().ContentType != "audio/mpeg" {
		t.Fatalf("audio artifact=%#v err=%v", audioArtifact, err)
	}
	defer audioArtifact.Cleanup()
}

func TestAttachmentArtifactRejectsOversizedTruncatedMissingGetFileUnsafeNameAndMagic(t *testing.T) {
	cases := []struct {
		name       string
		payload    []byte
		declared   string
		fileSize   int64
		filePath   string
		raw        TelegramAttachment
		getFileErr bool
		maxBytes   int64
	}{
		{name: "oversized metadata", payload: []byte("%PDF"), declared: "application/pdf", fileSize: 99, filePath: "docs/a.pdf", raw: TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 99}, maxBytes: 8},
		{name: "truncated", payload: []byte("%PDF"), declared: "application/pdf", fileSize: 20, filePath: "docs/a.pdf", raw: TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 20}},
		{name: "missing getFile", payload: nil, declared: "", fileSize: 0, filePath: "", raw: TelegramAttachment{Kind: AttachmentKindDocument, FileID: "missing", Filename: "a.bin"}, getFileErr: true},
		{name: "unsafe filename", payload: []byte("%PDF"), declared: "application/pdf", fileSize: 4, filePath: "docs/a.pdf", raw: TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "../secret", ContentType: "application/pdf", SizeBytes: 4}},
		{name: "bad magic", payload: []byte("not pdf"), declared: "application/pdf", fileSize: 7, filePath: "docs/a.pdf", raw: TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 7}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := attachmentFileServer(testCase.payload, testCase.declared, testCase.filePath)
			defer server.Close()
			client, _ := NewClient(server.URL, "123:secret-token", server.Client())
			store, err := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"), AttachmentArtifactStoreOptions{MaxBytes: testCase.maxBytes})
			if err != nil {
				t.Fatal(err)
			}
			if testCase.getFileErr {
				// The shared fake returns a successful envelope for ordinary IDs;
				// the missing fixture uses an invalid path and therefore fails closed
				// before any media request.
				if _, err := store.Stage(context.Background(), client, testCase.raw); err == nil {
					t.Fatal("expected missing getFile failure")
				}
				return
			}
			if _, err := store.Stage(context.Background(), client, testCase.raw); err == nil {
				t.Fatal("expected staging failure")
			}
			entries, _ := os.ReadDir(store.Root())
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".attachment-") || strings.HasPrefix(entry.Name(), ".attachment-stage-") {
					t.Fatalf("staging cleanup failed: %s", entry.Name())
				}
			}
		})
	}
}

func TestAttachmentAwareHandlerDeduplicatesAndCleansAfterHandlerFailure(t *testing.T) {
	payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0x02}, 8)...)
	server, counts := attachmentFileServer(payload, "application/pdf", "docs/a.pdf")
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	store, _ := NewAttachmentArtifactStore(filepath.Join(t.TempDir(), "attachments"))
	config := testConfig(t)
	var calls atomic.Int32
	handler, err := NewAttachmentAwareHandler(config, bee.ChannelHandlerFunc(func(_ context.Context, message bee.InboundMessage) (bee.ChannelRouteResult, error) {
		call := calls.Add(1)
		if len(message.Attachments) != 1 || strings.Contains(message.Attachments[0].Reference, "doc") || message.Attachments[0].SHA256 == "" {
			t.Fatalf("message attachment=%#v", message.Attachments)
		}
		if _, err := store.Resolve(message.Attachments[0]); err != nil {
			t.Fatalf("resolve staged attachment: %v", err)
		}
		if call == 1 {
			return bee.ChannelRouteResult{}, errors.New("handler failure")
		}
		return bee.ChannelRouteResult{}, nil
	}), client, store)
	if err != nil {
		t.Fatal(err)
	}
	update := Update{UpdateID: 903, Message: &Message{MessageID: 14, From: &User{ID: 7}, Chat: Chat{ID: 42}, Caption: "read", Document: &Document{FileID: "doc", FileName: "a.pdf", MimeType: "application/pdf", FileSize: int64(len(payload))}}}
	if _, err := handler.HandleUpdate(context.Background(), update); err == nil {
		t.Fatal("expected handler failure")
	}
	// Failure is replayable, so it stages once more; after success the same
	// update is a duplicate and does not redownload or invoke Bee again.
	if _, err := handler.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	result, err := handler.HandleUpdate(context.Background(), update)
	if err != nil || !result.Duplicate {
		t.Fatalf("duplicate result=%#v err=%v", result, err)
	}
	if calls.Load() != 2 || counts.getFile.Load() != 2 || counts.download.Load() != 2 {
		t.Fatalf("calls=%d getFile=%d download=%d", calls.Load(), counts.getFile.Load(), counts.download.Load())
	}
	entries, _ := os.ReadDir(store.Root())
	if len(entries) != 0 {
		t.Fatalf("handler cleanup left artifacts: %#v", entries)
	}
}

func TestAttachmentStageCancellationCleansPrivateRoot(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			fmt.Fprint(w, `{"ok":true,"result":{"file_id":"doc","file_size":4,"file_path":"docs/a.pdf"}}`)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:secret-token", server.Client())
	root := filepath.Join(t.TempDir(), "attachments")
	store, _ := NewAttachmentArtifactStore(root)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.Stage(ctx, client, TelegramAttachment{Kind: AttachmentKindDocument, FileID: "doc", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 4})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("root not cleaned: %#v", entries)
	}
}

func TestAttachmentArtifactStoreRejectsSymlinkedAncestors(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachmentArtifactStore(filepath.Join(link, "attachments")); err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlinked root error=%v", err)
	}
}

type attachmentCounts struct{ getFile, download atomic.Int32 }

func attachmentFileServer(payload []byte, mime, path string) (*httptest.Server, *attachmentCounts) {
	counts := &attachmentCounts{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			counts.getFile.Add(1)
			var request struct {
				FileID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.FileID == "missing" {
				fmt.Fprint(w, `{"ok":false,"error_code":404}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"doc","file_size":%d,"file_path":%q}}`, len(payload), path)
		case strings.HasSuffix(r.URL.Path, "/"+path):
			counts.download.Add(1)
			if mime != "" {
				w.Header().Set("Content-Type", mime)
			}
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	})), counts
}
