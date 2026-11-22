package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRPCServerStreamsEventsAndValidatesMethods(t *testing.T) {
	provider := &scriptedToolProvider{responses: []Response{{Message: Message{Role: RoleAssistant, Content: "rpc"}}}}
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApplication(DefaultConfig(), provider, store)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"run\",\"params\":{\"prompt\":\"hi\"}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown\"}\n")
	var output bytes.Buffer
	if err := (&RPCServer{Handlers: &HarnessHandlers{Application: app}, Input: input, Output: &output}).Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) < 5 {
		t.Fatalf("lines=%q", lines)
	}
	foundEvent, foundRunResponse, foundMethodError := false, false, false
	for _, line := range lines {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		if value["method"] == "event" {
			foundEvent = true
		}
		if value["id"] == float64(1) && value["result"] != nil {
			foundRunResponse = true
		}
		if value["id"] == float64(2) && value["error"] != nil {
			foundMethodError = true
		}
	}
	if !foundEvent || !foundRunResponse || !foundMethodError {
		t.Fatalf("output=%s", output.String())
	}
}
