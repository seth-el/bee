package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func scriptedFileServer(t *testing.T, payload string, script ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "download"
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			kind = "getFile"
		}
		mu.Lock()
		step := ""
		for i, s := range script {
			if strings.HasPrefix(s, kind+":") {
				step = strings.TrimPrefix(s, kind+":")
				script = append(script[:i:i], script[i+1:]...)
				break
			}
		}
		seen = append(seen, kind+":"+step)
		mu.Unlock()
		switch step {
		case "504", "502", "400":
			code := map[string]int{"504": 504, "502": 502, "400": 400}[step]
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"x"}`, code)
			return
		case "cut":
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(payload[:len(payload)/2]))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if kind == "getFile" {
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"abc","file_unique_id":"u","file_size":%d,"file_path":"voice/file_1.oga"}}`, len(payload))
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func noDownloadSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var delays []time.Duration
	previous := downloadRetrySleep
	downloadRetrySleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	t.Cleanup(func() { downloadRetrySleep = previous })
	return &delays
}

func TestDownloadFileByIDRetriesTemporaryFailuresIntoCleanFile(t *testing.T) {
	payload := strings.Repeat("OggS-voice-bytes-", 4096)
	for _, tc := range []struct {
		name   string
		script []string
	}{
		{"getFile 504", []string{"getFile:504"}},
		{"download 502", []string{"download:502"}},
		{"connection cut mid-file", []string{"download:cut"}},
		{"two failures", []string{"getFile:504", "download:cut"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delays := noDownloadSleep(t)
			server, seen := scriptedFileServer(t, payload, tc.script...)
			client, _ := NewClient(server.URL, "123:tok", NewHTTPClient())
			path := filepath.Join(t.TempDir(), "voice.oga")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			_, written, err := client.DownloadFileByID(context.Background(), "abc", file, 1<<20)
			file.Close()
			if err != nil {
				t.Fatalf("err = %v (requests %v)", err, seen())
			}
			data, _ := os.ReadFile(path)
			if string(data) != payload || written != int64(len(payload)) {
				t.Fatalf("file has %d bytes, want %d: a retry left partial data behind", len(data), len(payload))
			}
			if len(*delays) != len(tc.script) {
				t.Fatalf("delays = %v for %d failures", *delays, len(tc.script))
			}
		})
	}
}

func TestDownloadFileByIDGivesUpAndSkipsPermanentErrors(t *testing.T) {
	noDownloadSleep(t)
	server, seen := scriptedFileServer(t, "x", "getFile:400")
	client, _ := NewClient(server.URL, "123:tok", NewHTTPClient())
	f, _ := os.Create(filepath.Join(t.TempDir(), "f"))
	defer f.Close()
	if _, _, err := client.DownloadFileByID(context.Background(), "abc", f, 1<<20); err == nil {
		t.Fatal("400 succeeded")
	}
	if got := len(seen()); got != 1 {
		t.Fatalf("a 400 was retried: %v", seen())
	}

	server, seen = scriptedFileServer(t, "x", "getFile:504", "getFile:504", "getFile:504", "getFile:504")
	client, _ = NewClient(server.URL, "123:tok", NewHTTPClient())
	if _, _, err := client.DownloadFileByID(context.Background(), "abc", f, 1<<20); err == nil || !strings.Contains(err.Error(), "504") {
		t.Fatalf("err = %v, want the 504 after the last attempt", err)
	}
	if got := len(seen()); got != downloadAttempts {
		t.Fatalf("attempts = %d, want %d", got, downloadAttempts)
	}
}

func TestDownloadFileByIDTimesOutHungAttempt(t *testing.T) {
	noDownloadSleep(t)
	previous := downloadAttemptTimeout
	downloadAttemptTimeout = 100 * time.Millisecond
	defer func() { downloadAttemptTimeout = previous }()
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			fmt.Fprint(w, `{"ok":true,"result":{"file_id":"abc","file_unique_id":"u","file_size":2,"file_path":"voice/f.oga"}}`)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "123:tok", NewHTTPClient())
	f, _ := os.Create(filepath.Join(t.TempDir(), "f"))
	defer f.Close()
	if _, _, err := client.DownloadFileByID(context.Background(), "abc", f, 1<<20); err != nil {
		t.Fatalf("hung first attempt was not retried: %v", err)
	}
}
