package bee

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const MaxDiagnosticRecordBytes = 16 << 10

type DiagnosticLevel string
type DiagnosticStatus string

const (
	DiagnosticDebug     DiagnosticLevel  = "debug"
	DiagnosticInfo      DiagnosticLevel  = "info"
	DiagnosticWarn      DiagnosticLevel  = "warn"
	DiagnosticError     DiagnosticLevel  = "error"
	DiagnosticStarted   DiagnosticStatus = "started"
	DiagnosticSucceeded DiagnosticStatus = "succeeded"
	DiagnosticFailed    DiagnosticStatus = "failed"
	DiagnosticCanceled  DiagnosticStatus = "canceled"
	DiagnosticSkipped   DiagnosticStatus = "skipped"
)

type Diagnostic struct {
	Time       time.Time         `json:"time"`
	Level      DiagnosticLevel   `json:"level"`
	Component  string            `json:"component"`
	Operation  string            `json:"operation"`
	Status     DiagnosticStatus  `json:"status"`
	ErrorCode  string            `json:"error_code,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}
type DiagnosticSink interface {
	Emit(context.Context, Diagnostic) error
}
type JSONLDiagnosticSink struct {
	mu        sync.Mutex
	writer    io.Writer
	file      *os.File
	closeOnce sync.Once
	closeErr  error
}

var diagnosticKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func NewJSONLDiagnosticSink(path string) (*JSONLDiagnosticSink, error) {
	if path == "" {
		return nil, errors.New("diagnostics path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("create diagnostics directory")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("open diagnostics file")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, errors.New("set diagnostics permissions")
	}
	return &JSONLDiagnosticSink{writer: f, file: f}, nil
}
func NewWriterDiagnosticSink(writer io.Writer) *JSONLDiagnosticSink {
	return &JSONLDiagnosticSink{writer: writer}
}
func (sink *JSONLDiagnosticSink) Close() error {
	if sink == nil || sink.file == nil {
		return nil
	}
	sink.closeOnce.Do(func() {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		sink.closeErr = sink.file.Close()
	})
	return sink.closeErr
}
func (sink *JSONLDiagnosticSink) Emit(ctx context.Context, diagnostic Diagnostic) error {
	if sink == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("diagnostic context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := NormalizeDiagnostic(diagnostic)
	if err != nil {
		return err
	}
	data, err := json.Marshal(normalized)
	if err != nil || len(data) > MaxDiagnosticRecordBytes {
		return errors.New("diagnostic record exceeds bound")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.writer == nil {
		return errors.New("diagnostic sink writer is unavailable")
	}
	_, err = sink.writer.Write(append(data, '\n'))
	return err
}
func NormalizeDiagnostic(d Diagnostic) (Diagnostic, error) {
	if d.Time.IsZero() {
		d.Time = time.Now().UTC()
	}
	d.Time = d.Time.UTC()
	if d.Level == "" {
		d.Level = DiagnosticInfo
	}
	if d.Status == "" {
		d.Status = DiagnosticSucceeded
	}
	if d.Component == "" || d.Operation == "" {
		return Diagnostic{}, errors.New("diagnostic component and operation are required")
	}
	if len(d.Fields) > MaxDiagnosticsFields {
		return Diagnostic{}, errors.New("diagnostic fields exceed bound")
	}
	safe := make(map[string]string, len(d.Fields))
	for k, v := range d.Fields {
		if !diagnosticKeyPattern.MatchString(k) || diagnosticSecretKey(k) || diagnosticUnsafeValue(v) {
			continue
		}
		if len(v) > MaxDiagnosticValueBytes {
			v = v[:MaxDiagnosticValueBytes]
		}
		safe[k] = v
	}
	d.Fields = safe
	keys := make([]string, 0, len(safe))
	for k := range safe {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return d, nil
}
func diagnosticSecretKey(k string) bool {
	l := strings.ToLower(k)
	for _, x := range []string{"token", "secret", "password", "credential", "authorization", "api_key", "apikey"} {
		if strings.Contains(l, x) {
			return true
		}
	}
	return false
}
func diagnosticUnsafeValue(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	return strings.HasPrefix(l, "bearer ") || strings.HasPrefix(l, "basic ") || strings.HasPrefix(l, "sk-") || strings.Contains(l, "token=") || strings.HasPrefix(l, "/") || strings.HasPrefix(l, "~")
}
