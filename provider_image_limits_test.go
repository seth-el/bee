package bee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type providerImageResolver struct {
	data      []byte
	mediaType string
}

func (resolver providerImageResolver) OpenAttachmentPayload(context.Context, AttachmentRef) (Artifact, io.ReadCloser, error) {
	return Artifact{Filename: "large.png", MediaType: resolver.mediaType, Kind: "image", Size: int64(len(resolver.data))}, io.NopCloser(bytes.NewReader(resolver.data)), nil
}

func TestOpenAIProviderForwardsImageLargerThanFormerProviderCeilings(t *testing.T) {
	imageBytes := bytes.Repeat([]byte{0x5a}, 5<<20)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		messages := payload["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		imagePart := content[0].(map[string]any)
		imageURL := imagePart["image_url"].(map[string]any)["url"].(string)
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, "data:image/png;base64,"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, imageBytes) {
			t.Fatalf("forwarded image length=%d, want=%d", len(decoded), len(imageBytes))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 5, ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetAttachmentResolver(providerImageResolver{data: imageBytes, mediaType: "image/png"})
	response, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Attachments: []AttachmentRef{{Kind: "image", Filename: "large.png", MediaType: "image/png"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("response=%#v", response)
	}
}

func TestOpenAIProviderRetainsImageCountAndTypeProtections(t *testing.T) {
	imageBytes := []byte("image")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatalf("provider request should not be sent for invalid image attachments")
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 5, ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetAttachmentResolver(providerImageResolver{data: imageBytes, mediaType: "image/png"})
	attachments := make([]AttachmentRef, MaxProviderImageAttachments+1)
	for i := range attachments {
		attachments[i] = AttachmentRef{Kind: "image", Filename: "image.png", MediaType: "image/png"}
	}
	if _, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Attachments: attachments}}}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("image count error=%v", err)
	}
	provider.SetAttachmentResolver(providerImageResolver{data: imageBytes, mediaType: "application/octet-stream"})
	if _, err := provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Attachments: []AttachmentRef{{Kind: "image", Filename: "bad.bin", MediaType: "application/octet-stream"}}}}}); err == nil || !strings.Contains(err.Error(), "unsupported image media type") {
		t.Fatalf("image type error=%v", err)
	}
}

func TestOpenAIProviderSendsRequestsLargerThanFormerCap(t *testing.T) {
	const formerCap = 16 << 20
	content := strings.Repeat("x", formerCap+1)
	for _, test := range []struct {
		name   string
		stream bool
	}{
		{name: "complete"},
		{name: "stream", stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if len(body) <= formerCap {
					t.Fatalf("provider request size = %d, want > %d", len(body), formerCap)
				}
				var payload openAIRequest
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Messages) != 1 || payload.Messages[0].Content != content {
					t.Fatalf("request payload content was not preserved: messages=%d content type=%T", len(payload.Messages), payload.Messages[0].Content)
				}
				if test.stream {
					writer.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(writer, "data: {\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer server.Close()

			provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 5}, "")
			if err != nil {
				t.Fatal(err)
			}
			var response Response
			if test.stream {
				response, err = provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: content}}}, nil)
			} else {
				response, err = provider.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: content}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.Message.Content != "ok" {
				t.Fatalf("response content = %q", response.Message.Content)
			}
		})
	}
}

func TestOpenAIProviderImageReadHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatalf("cancelled image request should not reach provider")
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "test-model", TimeoutSeconds: 5, ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetAttachmentResolver(providerImageResolver{data: []byte("image"), mediaType: "image/png"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Attachments: []AttachmentRef{{Kind: "image", Filename: "cancelled.png", MediaType: "image/png"}}}}}); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancellation error=%v", err)
	}
}
