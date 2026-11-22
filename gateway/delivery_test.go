package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type nonFlushingResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (writer *nonFlushingResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *nonFlushingResponseWriter) WriteHeader(status int) { writer.status = status }
func (writer *nonFlushingResponseWriter) Write(payload []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.body.Write(payload)
}

func TestDeliveryWriterCompressesAndConditionallyServesHTML(t *testing.T) {
	body := strings.Repeat("<html><body>bee static shell</body></html>", 80)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	writer := &deliveryWriter{dst: response, request: request}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "private, no-cache")
	_, _ = writer.Write([]byte(body))
	writer.finish()
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("compressed response = %d headers=%v", response.Code, response.Header())
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if string(decoded) != body {
		t.Fatalf("decoded body mismatch: %d", len(decoded))
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	conditional := httptest.NewRequest(http.MethodGet, "/", nil)
	conditional.Header.Set("If-None-Match", etag)
	conditionalResponse := httptest.NewRecorder()
	conditionalWriter := &deliveryWriter{dst: conditionalResponse, request: conditional}
	conditionalWriter.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = conditionalWriter.Write([]byte(body))
	conditionalWriter.finish()
	if conditionalResponse.Code != http.StatusNotModified || conditionalResponse.Body.Len() != 0 {
		t.Fatalf("conditional response = %d body=%d", conditionalResponse.Code, conditionalResponse.Body.Len())
	}
}

func TestDeliveryWriterExcludesSSEAndRanges(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run/events", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	writer := &deliveryWriter{dst: response, request: request}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = writer.Write([]byte("data: {}\n\n"))
	writer.finish()
	if response.Header().Get("Content-Encoding") != "" || response.Header().Get("Vary") != "" {
		t.Fatalf("SSE was transformed: %v", response.Header())
	}

	rangeRequest := httptest.NewRequest(http.MethodGet, "/api/artifacts/id/download", nil)
	rangeResponse := httptest.NewRecorder()
	rangeWriter := &deliveryWriter{dst: rangeResponse, request: rangeRequest}
	rangeWriter.Header().Set("Content-Type", "text/css")
	rangeWriter.Header().Set("Content-Range", "bytes 0-3/4")
	_, _ = rangeWriter.Write([]byte("body"))
	rangeWriter.finish()
	if rangeResponse.Header().Get("Content-Encoding") != "" {
		t.Fatal("range response was compressed")
	}
}

func TestDeliveryWriterRespectsDisabledGzipQuality(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0, br;q=1")
	response := httptest.NewRecorder()
	writer := &deliveryWriter{dst: response, request: request}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write([]byte(strings.Repeat("compressible bee ", 100)))
	writer.finish()
	if response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip was used despite q=0: %v", response.Header())
	}
}

func TestGatewayRunEventsPreservesUnderlyingFlusherCapability(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.mu.Lock()
	service.runs["non-flushing"] = newRunRecord("non-flushing", runCtx, cancel)
	service.mu.Unlock()

	writer := &nonFlushingResponseWriter{}
	service.Handler().ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/api/runs/non-flushing/events", nil))
	if writer.status != http.StatusInternalServerError || !strings.Contains(writer.body.String(), "streaming is not supported") {
		t.Fatalf("non-flushing SSE writer = status %d body %q", writer.status, writer.body.String())
	}
}
