package bee

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Session history export: a redacted copy of every session transcript,
// meant to be committed to git (G6). Raw transcripts keep secret-like tool
// arguments and output verbatim by design, so they are never committed;
// this copy is derived from them, can be regenerated at any time (a pattern
// added later applies by re-exporting), and is what versioning sees.
//
// Redaction has two layers:
//  1. Exact known secret values: every file under the secrets directory and
//     every token/key/secret/password file named by a JSON config directly
//     in Bee's home (config.json, adapter configs). This catches secrets of
//     any shape.
//  2. Secret-shaped strings: key=value and key: value assignments, Bearer
//     tokens, private-key blocks, common token prefixes, and values under
//     secret-named JSON keys (also inside JSON-encoded tool arguments).
//
// A secret that is in neither layer still gets through, e.g. a password
// typed into chat that is not stored under secrets/.

const (
	// HistoryRedactionMarker replaces exact known secret values.
	HistoryRedactionMarker  = "[REDACTED:secret]"
	maxKnownSecretFileBytes = 64 << 10
	minKnownSecretBytes     = 8
	maxHistoryLineBytes     = 64 << 20
)

// SessionHistoryReport summarizes one export. It never contains secret
// values.
type SessionHistoryReport struct {
	Source       string `json:"source"`
	Output       string `json:"output"`
	Sessions     int    `json:"sessions"`
	Written      int    `json:"written"`
	Unchanged    int    `json:"unchanged"`
	Removed      int    `json:"removed"`
	KnownSecrets int    `json:"known_secrets"`
	Redactions   int    `json:"redactions"`
}

// DefaultBeeHistoryRoot is where the redacted history lives: ~/.bee/history,
// outside every agent write root.
func DefaultBeeHistoryRoot() (string, error) {
	home, err := DefaultBeeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "history"), nil
}

// ExportSessionHistory writes a redacted copy of every transcript in source
// to <output>/sessions/<id>.jsonl, rewriting only files whose redacted
// content changed and removing copies of sessions that no longer exist.
func ExportSessionHistory(source, output string) (SessionHistoryReport, error) {
	report := SessionHistoryReport{Source: source, Output: output}
	if !filepath.IsAbs(source) || !filepath.IsAbs(output) {
		return report, errors.New("history source and output must be absolute paths")
	}
	source, output = filepath.Clean(source), filepath.Clean(output)
	guarded := append([]string{}, SessionStoreRoots()...)
	guarded = append(guarded, source)
	if secrets, err := DefaultBeeSecretsRoot(); err == nil {
		guarded = append(guarded, secrets)
	}
	for _, root := range guarded {
		if output == root || scopeWithin(root, output) || scopeWithin(output, root) {
			return report, fmt.Errorf("history output must not overlap %s", root)
		}
	}
	if err := rejectBeeSymlinkAncestors(output); err != nil {
		return report, fmt.Errorf("validate history output: %w", err)
	}
	redactor, err := newHistoryRedactor()
	if err != nil {
		return report, err
	}
	report.KnownSecrets = len(redactor.known)
	sessionsOut := filepath.Join(output, "sessions")
	if err := os.MkdirAll(sessionsOut, 0o700); err != nil {
		return report, errors.New("create history output")
	}
	_ = os.Chmod(output, 0o700)
	_ = os.Chmod(sessionsOut, 0o700)
	entries, err := os.ReadDir(source)
	if err != nil {
		return report, errors.New("read session store")
	}
	present := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || validateSessionID(SessionID(strings.TrimSuffix(name, ".jsonl"))) != nil {
			continue
		}
		present[name] = true
		report.Sessions++
		redacted, count, err := redactor.transcript(filepath.Join(source, name))
		if err != nil {
			return report, fmt.Errorf("redact %s: %w", name, err)
		}
		report.Redactions += count
		target := filepath.Join(sessionsOut, name)
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, redacted) {
			report.Unchanged++
			continue
		}
		if err := writeFileAtomic(target, redacted, 0o600); err != nil {
			return report, fmt.Errorf("write %s: %w", name, err)
		}
		report.Written++
	}
	outEntries, err := os.ReadDir(sessionsOut)
	if err != nil {
		return report, errors.New("read history output")
	}
	for _, entry := range outEntries {
		if strings.HasSuffix(entry.Name(), ".jsonl") && !present[entry.Name()] {
			if err := os.Remove(filepath.Join(sessionsOut, entry.Name())); err != nil {
				return report, errors.New("remove stale history copy")
			}
			report.Removed++
		}
	}
	return report, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

type historyRedactor struct {
	known []string
}

func newHistoryRedactor() (*historyRedactor, error) {
	values := map[string]bool{}
	addValue := func(value string) {
		value = strings.TrimSpace(value)
		if len(value) < minKnownSecretBytes || strings.HasPrefix(value, "-----") || strings.Contains(value, "\n") && len(value) > maxKnownSecretFileBytes {
			return
		}
		values[value] = true
	}
	addFile := func(path string) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxKnownSecretFileBytes {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		text := string(data)
		addValue(text)
		for _, line := range strings.Split(text, "\n") {
			addValue(line)
			if key, value, ok := strings.Cut(line, "="); ok && !strings.ContainsAny(key, " \t\"") {
				addValue(strings.Trim(strings.TrimSpace(value), `"'`))
			}
		}
		var parsed any
		if json.Unmarshal(data, &parsed) == nil {
			walkJSONStrings(parsed, addValue)
		}
	}
	if secrets, err := DefaultBeeSecretsRoot(); err == nil {
		_ = filepath.WalkDir(secrets, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() {
				addFile(path)
			}
			return nil
		})
	}
	if home, err := DefaultBeeHome(); err == nil {
		configs, _ := filepath.Glob(filepath.Join(home, "*.json"))
		for _, config := range configs {
			for _, path := range configSecretFiles(config) {
				addFile(path)
			}
		}
	}
	known := make([]string, 0, len(values))
	for value := range values {
		known = append(known, value)
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(known, func(i, j int) bool {
		if len(known[i]) != len(known[j]) {
			return len(known[i]) > len(known[j])
		}
		return known[i] < known[j]
	})
	return &historyRedactor{known: known}, nil
}

// configSecretFiles finds file paths a JSON config names under keys that
// look like secret files (token_file, api_key_file, ...).
func configSecretFiles(path string) []string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed any
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}
	var files []string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if text, ok := child.(string); ok && strings.HasSuffix(lower, "file") && (strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "credential")) {
					if resolved, err := ResolveBeeHomePath(strings.TrimSpace(text)); err == nil && resolved != "" {
						files = append(files, resolved)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(parsed)
	return files
}

func walkJSONStrings(value any, visit func(string)) {
	switch typed := value.(type) {
	case string:
		visit(typed)
	case map[string]any:
		for _, child := range typed {
			walkJSONStrings(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkJSONStrings(child, visit)
		}
	}
}

// transcript redacts one session file line by line. A trailing line without
// a newline is a write in progress and is left for the next export.
func (r *historyRedactor) transcript(path string) ([]byte, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	var out bytes.Buffer
	total := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if len(line) > maxHistoryLineBytes {
			return nil, 0, errors.New("transcript line too large")
		}
		redacted, count := r.line(bytes.TrimRight(line, "\r\n"))
		total += count
		out.Write(redacted)
		out.WriteByte('\n')
	}
	return out.Bytes(), total, nil
}

func (r *historyRedactor) line(line []byte) ([]byte, int) {
	if len(bytes.TrimSpace(line)) == 0 {
		return line, 0
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err == nil {
		count := 0
		value = r.value(value, &count)
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(value) == nil {
			return bytes.TrimRight(buffer.Bytes(), "\n"), count
		}
	}
	redacted, count := r.text(string(line))
	return []byte(redacted), count
}

func (r *historyRedactor) value(value any, count *int) any {
	switch typed := value.(type) {
	case string:
		text, n := r.text(typed)
		*count += n
		return text
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && isHistorySecretKey(key) && text != "" && !strings.HasPrefix(text, "[REDACTED") {
				typed[key] = "[REDACTED]"
				*count++
				continue
			}
			typed[key] = r.value(child, count)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = r.value(child, count)
		}
		return typed
	}
	return value
}

var (
	historyBearer     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`)
	historyPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// Assignments with spacing or camelCase that the display redactor's exact
	// markers miss: "apiKey : abc...", "password = hunter2...".
	historyLooseAssignment = regexp.MustCompile(`(?i)\b((?:api[_-]?key|password|passwd|secret|access[_-]?token|auth[_-]?token|token)\s*[=:]\s*["']?)([^\s"',;{}\[\]<>]{8,})`)
	historyTokenShape      = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|[0-9]{8,10}:AA[A-Za-z0-9_-]{30,})\b`)
)

// text applies both layers to one string. A string holding a JSON object or
// array (tool arguments) is redacted structurally and re-encoded.
func (r *historyRedactor) text(value string) (string, int) {
	count := 0
	for _, secret := range r.known {
		if n := strings.Count(value, secret); n > 0 {
			value = strings.ReplaceAll(value, secret, HistoryRedactionMarker)
			count += n
		}
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		var parsed any
		if decoder.Decode(&parsed) == nil {
			var extra any
			if decoder.Decode(&extra) == io.EOF {
				before := count
				parsed = r.value(parsed, &count)
				if count > before {
					var buffer bytes.Buffer
					encoder := json.NewEncoder(&buffer)
					encoder.SetEscapeHTML(false)
					if encoder.Encode(parsed) == nil {
						return strings.TrimRight(buffer.String(), "\n"), count
					}
				}
				return value, count
			}
		}
	}
	before := value
	value = historyPrivateKey.ReplaceAllString(value, "[REDACTED:private-key]")
	value = historyBearer.ReplaceAllString(value, "${1}[REDACTED]")
	value = historyTokenShape.ReplaceAllString(value, "[REDACTED]")
	value = historyLooseAssignment.ReplaceAllStringFunc(value, func(match string) string {
		parts := historyLooseAssignment.FindStringSubmatch(match)
		if strings.HasPrefix(strings.ToUpper(parts[2]), "[REDACTED") {
			return match
		}
		return parts[1] + "[REDACTED]"
	})
	value = redactSecretAssignmentsForDisplay(value)
	if value != before {
		count++
	}
	return value, count
}

// isHistorySecretKey matches JSON keys whose values are credentials,
// without catching ordinary fields like max_tokens or sort_key.
func isHistorySecretKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	switch key {
	case "token", "password", "passwd", "secret", "api_key", "apikey", "authorization", "credential", "credentials", "private_key", "client_secret", "access_key", "secret_key", "passphrase", "cookie":
		return true
	}
	for _, suffix := range []string{"_token", "_secret", "_password", "_api_key", "_apikey", "_private_key", "_access_key", "_secret_key"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// MaxRedactFileBytes bounds a file RedactFileForHistory will load.
const MaxRedactFileBytes = 512 << 20

// RedactFileForHistory writes a redacted copy of src to dst with the same
// two layers as the session history export. A file holding one JSON value
// is redacted structurally and re-encoded compactly; JSON Lines files line
// by line; anything else as text, line by line. dst is written atomically
// with mode 0600. It returns the number of redactions, never the values.
func RedactFileForHistory(src, dst string) (int, error) {
	if src == "" || dst == "" {
		return 0, errors.New("input and output paths are required")
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return 0, err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return 0, err
	}
	if srcAbs == dstAbs {
		return 0, errors.New("output must differ from input")
	}
	info, err := os.Stat(srcAbs)
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("input is not a regular file")
	}
	if info.Size() > MaxRedactFileBytes {
		return 0, fmt.Errorf("input is larger than %d MB", MaxRedactFileBytes>>20)
	}
	data, err := os.ReadFile(srcAbs)
	if err != nil {
		return 0, errors.New("read input")
	}
	redactor, err := newHistoryRedactor()
	if err != nil {
		return 0, err
	}
	count := 0
	var out []byte
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		var extra any
		if decoder.Decode(&extra) == io.EOF {
			value = redactor.value(value, &count)
			var buffer bytes.Buffer
			encoder := json.NewEncoder(&buffer)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(value); err != nil {
				return 0, errors.New("encode redacted JSON")
			}
			out = buffer.Bytes()
		}
	}
	if out == nil {
		var buffer bytes.Buffer
		lines := bytes.SplitAfter(data, []byte("\n"))
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			redacted, n := redactor.line(trimmed)
			count += n
			buffer.Write(redacted)
			buffer.Write(line[len(trimmed):])
		}
		out = buffer.Bytes()
	}
	if err := writeFileAtomic(dstAbs, out, 0o600); err != nil {
		return 0, fmt.Errorf("write output: %w", err)
	}
	return count, nil
}
