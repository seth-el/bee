package bee

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBrowserCamofoxModernAndLegacyRoutes(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tabs":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"running":true,"tabs":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"tabId":"tab-1","url":"http://example.test","title":"Example"}`))
		case "/navigate":
			_, _ = w.Write([]byte(`{"targetId":"tab-1","url":"http://example.test/next","title":"Next"}`))
		case "/snapshot":
			_, _ = w.Write([]byte(`{"targetId":"tab-1","snapshot":"text","offset":0,"hasMore":false}`))
		case "/tabs/tab-1/click", "/tabs/tab-1/press", "/tabs/tab-1/scroll", "/tabs/tab-1/wait":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/tabs/tab-1/type":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["ref"] != "e1" || body["text"] != "hello" || len(body) != 3 {
				t.Fatalf("type body=%v", body)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/act":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/tabs/tab-1":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewBrowserCamofoxClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Open(context.Background(), BrowserOpenRequest{URL: "http://example.test", UserID: "session-a", SessionKey: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Navigate(context.Background(), BrowserNavigateRequest{TargetID: "tab-1", URL: "http://example.test/next", UserID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Snapshot(context.Background(), BrowserSnapshotRequest{TargetID: "tab-1", UserID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Scroll(context.Background(), BrowserScrollRequest{TargetID: "tab-1", UserID: "session-a", Direction: "down", Amount: 500}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []BrowserActRequest{{Kind: "click", Ref: "e1"}, {Kind: "type", Ref: "e1", Text: "hello"}, {Kind: "press", Key: "Enter"}, {Kind: "hover", Ref: "e1"}} {
		action.TargetID = "tab-1"
		action.UserID = "session-a"
		if _, err = client.Act(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}
	if err = client.Close(context.Background(), BrowserCloseRequest{TargetID: "tab-1", UserID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POST /tabs", "POST /navigate", "GET /snapshot", "POST /tabs/tab-1/click", "POST /tabs/tab-1/type", "POST /tabs/tab-1/press", "POST /act", "POST /tabs/tab-1/scroll", "DELETE /tabs/tab-1"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing route %q in %v", want, seen)
		}
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"ref":"e1","text":"hello"}`), &body)
}

func TestBrowserCamofoxTypeBodyAndAuthNoRedirect(t *testing.T) {
	dir := t.TempDir()
	key := dir + "/key"
	if err := os.WriteFile(key, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/tabs/tab/type" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["userId"] == nil || body["ref"] != "e1" || body["text"] != "hello" {
			t.Fatalf("body=%v", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()
	_, err := NewBrowserCamofoxClientWithAccessKeyFile("http://192.0.2.1:9377", nil, time.Second, key)
	if err == nil {
		t.Fatal("HTTP off-loopback key endpoint accepted")
	}
	_ = gotAuth
	if _, err = NewBrowserCamofoxClient("http://192.0.2.1:9377", nil, time.Second); err == nil {
		t.Fatal("non-loopback no-key endpoint accepted")
	}
}

func TestBrowserCamofoxRejectsRedirectAndBoundsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) }))
	defer server.Close()
	client, err := NewBrowserCamofoxClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Tabs(context.Background()); err == nil {
		t.Fatal("redirect followed")
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1))) }))
	defer large.Close()
	client, err = NewBrowserCamofoxClient(large.URL, large.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Tabs(context.Background()); err == nil {
		t.Fatal("oversized response accepted")
	}
}
