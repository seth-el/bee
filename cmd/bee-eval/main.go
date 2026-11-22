package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	bee "github.com/seth-el/bee"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "mcp-stub" {
		return runMCPStub()
	}
	if len(args) == 0 || args[0] != "run" || len(args) > 2 {
		return errors.New("usage: bee-eval run [fixture-directory]")
	}
	root := filepath.Join("testdata", "eval")
	if len(args) == 2 {
		root = args[1]
	}
	paths, err := bee.GoldenFixturePaths(root)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	for _, path := range paths {
		scenario, err := bee.LoadGoldenScenario(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		actual, err := bee.RunGoldenScenario(context.Background(), scenario, executable)
		if err != nil {
			return fmt.Errorf("%s: run: %w", path, err)
		}
		if err := bee.CompareGoldenTrace(scenario.Expected, actual); err != nil {
			return fmt.Errorf("%s (%s): trace mismatch\n%w", path, scenario.Name, err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "PASS %s\n", scenario.Name)
	}
	return nil
}

func runMCPStub() error {
	reader := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	for reader.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      string          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(reader.Bytes(), &request); err != nil {
			return err
		}
		switch request.Method {
		case "initialize":
			if err := writer.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{
					"protocolVersion": bee.MCPProtocolVersion,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "bee-eval", "version": "1"},
				},
			}); err != nil {
				return err
			}
		case "notifications/initialized":
		case "tools/list":
			if err := writer.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"tools": []any{
					map[string]any{
						"name":        "echo",
						"description": "Echo bounded deterministic fixture text.",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]string{"type": "string"}}, "additionalProperties": false},
					},
				}},
			}); err != nil {
				return err
			}
		case "tools/call":
			var input struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			if err := json.Unmarshal(request.Params, &input); err != nil {
				return err
			}
			if input.Name != "echo" {
				return fmt.Errorf("unsupported fixture tool %q", input.Name)
			}
			if err := writer.Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"content": []any{map[string]string{"type": "text", "text": input.Arguments["text"]}}},
			}); err != nil {
				return err
			}
		default:
			if request.ID != "" {
				if err := writer.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unsupported fixture method"}}); err != nil {
					return err
				}
			}
		}
	}
	return reader.Err()
}
