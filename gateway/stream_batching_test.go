package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

type rapidStreamProvider struct {
	chunks []string
}

func (provider rapidStreamProvider) Complete(context.Context, bee.Request) (bee.Response, error) {
	return bee.Response{}, errors.New("complete not used")
}

func (provider rapidStreamProvider) Stream(_ context.Context, _ bee.Request, onDelta bee.StreamFunc) (bee.Response, error) {
	var content strings.Builder
	for _, chunk := range provider.chunks {
		if err := onDelta(chunk); err != nil {
			return bee.Response{}, err
		}
		content.WriteString(chunk)
	}
	return bee.Response{Message: bee.Message{Role: bee.RoleAssistant, Content: content.String()}}, nil
}

func TestGatewayRapidProviderStreamBatchesSSEEventsWithoutChangingText(t *testing.T) {
	chunks := make([]string, 96)
	for index := range chunks {
		chunks[index] = string(rune('a' + (index % 26)))
	}
	service := newTestService(t, rapidStreamProvider{chunks: chunks})
	defer service.Stop(context.Background())

	create := httptest.NewRecorder()
	service.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create session = %d %s", create.Code, create.Body.String())
	}
	var metadata bee.SessionMetadata
	if err := json.Unmarshal(create.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	message := httptest.NewRecorder()
	service.Handler().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/sessions/"+string(metadata.ID)+"/messages", strings.NewReader(`{"prompt":"rapid"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("start run = %d %s", message.Code, message.Body.String())
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(message.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	stream := httptest.NewRecorder()
	service.Handler().ServeHTTP(stream, httptest.NewRequest(http.MethodGet, "/api/runs/"+started.RunID+"/events", nil))
	if stream.Code != http.StatusOK {
		t.Fatalf("stream = %d %s", stream.Code, stream.Body.String())
	}
	body := stream.Body.String()
	deltaEvents := strings.Count(body, `"type":"assistant_delta"`)
	t.Logf("rapid provider chunks=%d retained assistant_delta SSE events=%d total SSE bytes=%d", len(chunks), deltaEvents, len(body))
	if deltaEvents >= len(chunks) {
		t.Fatalf("assistant delta SSE events = %d for %d provider chunks; batching did not reduce events", deltaEvents, len(chunks))
	}
	var reconstructed string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event bee.RunEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == bee.RunAssistantDelta {
				reconstructed += event.Delta
			}
		}
	}
	var want strings.Builder
	for _, chunk := range chunks {
		want.WriteString(chunk)
	}
	if reconstructed != want.String() {
		t.Fatalf("batched assistant text changed: got %q want %q", reconstructed, want.String())
	}
	if !strings.Contains(body, `"type":"run_end"`) {
		t.Fatalf("stream missing terminal event: %s", body)
	}
}
