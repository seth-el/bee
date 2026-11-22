package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func TestGatewayStaticDeliveryHeadersAndConditionalGet(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("shell delivery = %d headers=%v", response.Code, response.Header())
	}
	if response.Header().Get("ETag") == "" || response.Header().Get("Content-Length") == "" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("shell cache headers = %v", response.Header())
	}
	etag := response.Header().Get("ETag")
	conditional := httptest.NewRequest(http.MethodGet, "/", nil)
	conditional.Header.Set("If-None-Match", etag)
	conditionalResponse := httptest.NewRecorder()
	service.Handler().ServeHTTP(conditionalResponse, conditional)
	if conditionalResponse.Code != http.StatusNotModified || conditionalResponse.Body.Len() != 0 {
		t.Fatalf("conditional shell = %d body=%d", conditionalResponse.Code, conditionalResponse.Body.Len())
	}
}

func TestGatewayTimingHeadersAndRouteLabelsStayBounded(t *testing.T) {
	service := newTestService(t, testProvider{})
	defer service.Stop(context.Background())
	created, err := service.store.Create(bee.SessionMetadata{Title: "timing"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Server-Timing"), "sessions;dur=") {
		t.Fatalf("session timing = %d %q", response.Code, response.Header().Get("Server-Timing"))
	}
	label := routeLabel("/api/sessions/" + string(created.ID) + "/history")
	if label != "/api/sessions/:id/history" || strings.Contains(label, string(created.ID)) {
		t.Fatalf("route label leaked id: %q", label)
	}
}
