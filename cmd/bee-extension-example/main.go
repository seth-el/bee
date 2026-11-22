// bee-extension-example is a tiny stdio extension used for protocol examples
// and smoke tests. It deliberately uses only the Go standard library.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func main() {
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"manifest":     map[string]any{"name": "example", "version": "1.0.0", "protocol": "bee.extension.v1", "description": "Bee extension protocol example"},
				"capabilities": map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo text", "parameters": map[string]any{"type": "object"}, "risk": "low"}}, "notifications": []string{"example/notice"}},
			}})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.Name != "echo" {
				_ = encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32602, "message": "unknown tool"}})
				continue
			}
			var args struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(params.Arguments, &args)
			_ = encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]string{"output": args.Text}})
		case "shutdown":
			_ = encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]bool{"ok": true}})
			return
		case "$/cancelRequest":
			// Cancellation is a notification and needs no response.
		default:
			_ = encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32601, "message": fmt.Sprintf("method %q not found", req.Method)}})
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, err)
	}
}
