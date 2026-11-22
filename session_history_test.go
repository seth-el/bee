package bee

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportSessionHistoryRedactsBothLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	t.Setenv("BEE_SESSION_ROOT", "")
	secrets := filepath.Join(home, "secrets")
	tokenDir := filepath.Join(home, "workspace", "secrets")
	for _, dir := range []string{secrets, tokenDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	serveToken := "serve-token-0123456789abcdef"
	odd := "plainwordsecret!" // matches no pattern; only layer 1 can catch it
	botToken := "987654321:AAbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(secrets, "serve.token"), serveToken+"\n")
	mustWrite(filepath.Join(secrets, "env"), "ODD_PASSWORD=\""+odd+"\"\n")
	mustWrite(filepath.Join(tokenDir, "bot-token"), botToken+"\n")
	mustWrite(filepath.Join(home, "telegram-adapter.json"), `{"token_file": "`+filepath.Join(tokenDir, "bot-token")+`"}`)

	source := filepath.Join(home, "state", "sessions")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []map[string]any{
		{"type": "message", "message": map[string]any{"role": "user", "content": "my token is " + serveToken + " and " + odd}},
		{"type": "tool_call", "arguments": `{"command":"curl -H 'Authorization: Bearer abcdefghijklmnop1234' https://x","password":"hunter2hunter2","max_tokens":4096}`},
		{"type": "tool_result", "content": "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3Nza\n-----END OPENSSH PRIVATE KEY-----\napi_key=sk-live-zzzzzzzzzzzzzzzz bot " + botToken},
		{"type": "usage", "usage": map[string]any{"prompt_tokens": 1234, "completion_tokens": 56}, "ratio": 0.125},
	}
	var raw strings.Builder
	for _, line := range lines {
		encoded, _ := json.Marshal(line)
		raw.Write(encoded)
		raw.WriteByte('\n')
	}
	raw.WriteString(`{"type":"message","partial":`) // write in progress, no newline
	mustWrite(filepath.Join(source, "s1.jsonl"), raw.String())
	mustWrite(filepath.Join(source, "s1.jsonl.lock"), "")
	mustWrite(filepath.Join(source, "s1.jsonl.search.idx"), serveToken)

	out := filepath.Join(home, "history")
	report, err := ExportSessionHistory(source, out)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 1 || report.Written != 1 || report.KnownSecrets < 3 || report.Redactions < 5 {
		t.Fatalf("report = %#v", report)
	}
	encodedReport, _ := json.Marshal(report)
	data, err := os.ReadFile(filepath.Join(out, "sessions", "s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, leaked := range []string{serveToken, odd, botToken, "hunter2hunter2", "abcdefghijklmnop1234", "AAAAB3Nza", "sk-live-zzzz"} {
		if strings.Contains(text, leaked) || strings.Contains(string(encodedReport), leaked) {
			t.Fatalf("leaked %q:\n%s", leaked, text)
		}
	}
	for _, kept := range []string{`\"max_tokens\":4096`, `"prompt_tokens":1234`, `"ratio":0.125`, "my token is", "curl -H", HistoryRedactionMarker} {
		if !strings.Contains(text, kept) {
			t.Fatalf("missing %q:\n%s", kept, text)
		}
	}
	if strings.Count(text, "\n") != 4 || strings.Contains(text, "partial") {
		t.Fatalf("partial trailing line exported:\n%s", text)
	}
	if entries, _ := os.ReadDir(filepath.Join(out, "sessions")); len(entries) != 1 {
		t.Fatalf("sidecars exported: %v", entries)
	}
	if info, _ := os.Stat(filepath.Join(out, "sessions", "s1.jsonl")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}

	again, err := ExportSessionHistory(source, out)
	if err != nil || again.Written != 0 || again.Unchanged != 1 {
		t.Fatalf("second export = %#v, %v", again, err)
	}
	if err := os.Remove(filepath.Join(source, "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if gone, err := ExportSessionHistory(source, out); err != nil || gone.Removed != 1 {
		t.Fatalf("removed = %#v, %v", gone, err)
	}
}

func TestExportSessionHistoryRefusesOverlappingOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	source := filepath.Join(home, "state", "sessions")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{source, filepath.Join(source, "history"), filepath.Join(home, "state"), filepath.Join(home, "secrets", "h")} {
		if _, err := ExportSessionHistory(source, out); err == nil {
			t.Fatalf("output %s accepted", out)
		}
	}
}

func TestRedactFileForHistoryJSONAndText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	known := "known-secret-value-42"
	if err := os.WriteFile(filepath.Join(home, "secrets", "k"), []byte(known), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "conversations.json")
	ghp := "ghp_" + strings.Repeat("a", 36)
	doc := `[{"name":"chat","messages":[{"text":"here is ` + known + ` and ` + ghp + `","count":3}]}]`
	if err := os.WriteFile(in, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "conversations.redacted.json")
	count, err := RedactFileForHistory(in, out)
	if err != nil || count < 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	data, _ := os.ReadFile(out)
	if strings.Contains(string(data), known) || strings.Contains(string(data), ghp) || !strings.Contains(string(data), `"count":3`) {
		t.Fatalf("redacted = %s", data)
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("line one\npassword=hunter2hunter2\napiKey : a1b2c3d4e5f6a7b8c9d0\nPASSWORD = correcthorse99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RedactFileForHistory(text, text+".out"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(text + ".out"); strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "a1b2c3d4") || strings.Contains(string(data), "correcthorse") || !strings.HasPrefix(string(data), "line one\n") {
		t.Fatalf("text redaction = %q", data)
	}
	if _, err := RedactFileForHistory(in, in); err == nil {
		t.Fatal("in-place redaction accepted")
	}
}
