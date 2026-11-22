package bee

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type statusBrowserBackend struct {
	fakeBrowserBackend
	status int
}

func (b *statusBrowserBackend) Navigate(context.Context, BrowserNavigateRequest) (BrowserNavigateResult, error) {
	return BrowserNavigateResult{}, &CamofoxError{Status: b.status, Class: "http status"}
}

func TestBrowserErrorMappingIsSafeAndActionable(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{{401, "authentication"}, {403, "authentication"}, {409, "page changed"}, {422, "page changed"}, {429, "busy"}, {500, "unavailable"}} {
		backend := &statusBrowserBackend{status: tc.status}
		tool, _ := NewBrowserTool(backend)
		ctx := withArtifactContext(context.Background(), SessionID("errors"), "run-errors")
		_, _ = tool.Execute(ctx, json.RawMessage(`{"action":"open","url":"http://127.0.0.1/app"}`))
		out, err := tool.Execute(ctx, json.RawMessage(`{"action":"navigate","url":"http://127.0.0.1/next"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ToLower(out), tc.want) || strings.Contains(out, "target") || strings.Contains(out, "http status") {
			t.Fatalf("status=%d output=%s", tc.status, out)
		}
	}
}
