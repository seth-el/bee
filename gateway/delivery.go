package gateway

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const minJSONCompressionBytes = 1024

// deliveryWriter keeps ordinary JSON/static responses bounded in memory long
// enough to apply one consistent set of cache and content-encoding headers.
// Streaming SSE and ranged/media payloads switch to passthrough mode before
// their first byte is emitted.
type deliveryWriter struct {
	dst          http.ResponseWriter
	request      *http.Request
	status       int
	wroteHead    bool
	streaming    bool
	body         bytes.Buffer
	bytesWritten int64
}

func (w *deliveryWriter) Header() http.Header { return w.dst.Header() }

func (w *deliveryWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.status = status
	w.wroteHead = true
	if w.streaming {
		w.dst.WriteHeader(status)
	}
}

func (w *deliveryWriter) shouldBuffer() bool {
	if w.request == nil {
		return false
	}
	if strings.HasPrefix(w.request.URL.Path, "/assets/") || w.request.URL.Path == "/" || w.request.URL.Path == "/index.html" {
		return true
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(w.Header().Get("Content-Type"), ";")[0]))
	if contentType == "text/event-stream" || w.Header().Get("Content-Range") != "" || w.Header().Get("Accept-Ranges") != "" {
		return false
	}
	switch contentType {
	case "application/json", "text/html", "text/css", "text/javascript", "application/javascript", "application/x-javascript", "image/svg+xml":
		return true
	default:
		return false
	}
}

func (w *deliveryWriter) switchToStreaming() {
	if w.streaming {
		return
	}
	w.streaming = true
	if !w.wroteHead {
		w.status = http.StatusOK
		w.wroteHead = true
	}
	w.dst.WriteHeader(w.status)
	if w.body.Len() > 0 {
		_, _ = w.dst.Write(w.body.Bytes())
		w.body.Reset()
	}
}

func (w *deliveryWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.status = http.StatusOK
		w.wroteHead = true
	}
	if !w.streaming && !w.shouldBuffer() {
		w.switchToStreaming()
	}
	if w.streaming {
		n, err := w.dst.Write(p)
		w.bytesWritten += int64(n)
		return n, err
	}
	n, err := w.body.Write(p)
	w.bytesWritten += int64(n)
	return n, err
}

func (w *deliveryWriter) Flush() {
	w.switchToStreaming()
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *deliveryWriter) finish() {
	if w.streaming {
		return
	}
	if !w.wroteHead {
		w.status = http.StatusOK
		w.wroteHead = true
	}
	body := w.body.Bytes()
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(w.Header().Get("Content-Type"), ";")[0]))
	compressible := contentType == "text/html" || contentType == "text/css" || contentType == "text/javascript" || contentType == "application/javascript" || contentType == "application/x-javascript" || contentType == "image/svg+xml" || contentType == "application/json"
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if compressible {
		w.Header().Add("Vary", "Accept-Encoding")
	}
	etagHash := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(etagHash[:]) + `"`
	w.Header().Set("ETag", etag)
	if matchesETag(w.request.Header.Get("If-None-Match"), etag) && w.status >= 200 && w.status < 300 {
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Length", "0")
		w.bytesWritten = 0
		w.status = http.StatusNotModified
		w.dst.WriteHeader(http.StatusNotModified)
		return
	}
	if compressible && len(body) >= compressionThreshold(contentType) && acceptsGzip(w.request.Header.Get("Accept-Encoding")) {
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		_, _ = gz.Write(body)
		_ = gz.Close()
		body = compressed.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	} else {
		w.Header().Del("Content-Encoding")
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.bytesWritten = int64(len(body))
	if contentType == "application/json" && w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.dst.WriteHeader(w.status)
	_, _ = io.Copy(w.dst, bytes.NewReader(body))
}

func compressionThreshold(contentType string) int {
	if contentType == "application/json" {
		return minJSONCompressionBytes
	}
	return 256
}

func acceptsGzip(value string) bool {
	wildcard := -1.0
	for _, part := range strings.Split(strings.ToLower(value), ",") {
		fields := strings.Split(part, ";")
		encoding := strings.TrimSpace(fields[0])
		quality := 1.0
		for _, parameter := range fields[1:] {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || strings.TrimSpace(name) != "q" {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		if encoding == "gzip" {
			return quality > 0
		}
		if encoding == "*" {
			wildcard = quality
		}
	}
	return wildcard > 0
}

func matchesETag(value, etag string) bool {
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func routeLabel(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if path == "/" || path == "/index.html" {
		return "/"
	}
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	if parts[0] == "api" && len(parts) >= 2 {
		switch parts[1] {
		case "sessions":
			if len(parts) >= 3 && parts[2] == "search" {
				return "/api/sessions/search"
			}
			if len(parts) >= 4 {
				return "/api/sessions/:id/" + parts[3]
			}
			if len(parts) >= 3 {
				return "/api/sessions/:id"
			}
			return "/api/sessions"
		case "projects":
			if len(parts) >= 3 {
				return "/api/projects/:id"
			}
			return "/api/projects"
		case "runs":
			if len(parts) >= 4 {
				return "/api/runs/:id/" + parts[3]
			}
			return "/api/runs/:id"
		case "artifacts":
			if len(parts) >= 4 {
				return "/api/artifacts/:id/" + parts[3]
			}
			return "/api/artifacts/:id"
		case "attachments":
			return "/api/attachments/:id"
		case "notifications":
			if len(parts) >= 3 && parts[2] == "events" {
				return "/api/notifications/events"
			}
			if len(parts) >= 4 && parts[3] == "read" {
				return "/api/notifications/:id/read"
			}
			return "/api/notifications"
		}
	}
	if strings.HasPrefix(path, "/assets/") {
		return "/assets/:asset"
	}
	return "/other"
}

func isRunEventsPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "api" && parts[1] == "runs" && parts[2] != "" && parts[3] == "events"
}

func isNotificationEventsPath(path string) bool {
	return strings.Trim(path, "/") == "api/notifications/events"
}
