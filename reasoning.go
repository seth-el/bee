package bee

import "unicode/utf8"

const (
	MaxReasoningBytes         = 256 << 10
	MaxReasoningRunEventBytes = 64 << 10
)

// boundedText retains a UTF-8-safe prefix while continuing to count all input
// bytes. It is used at stream ingestion boundaries so provider output cannot
// grow Bee's in-memory reasoning state without bound.
type boundedText struct {
	buf           []byte
	limit         int
	originalBytes int
	truncated     bool
}

func newBoundedText(limit int) *boundedText {
	if limit < 0 {
		limit = 0
	}
	return &boundedText{limit: limit}
}

func (text *boundedText) Append(value string) {
	_ = text.AppendRetained(value)
}

// AppendRetained appends a bounded UTF-8-safe prefix and returns only the
// newly retained portion. Callers can therefore bound live event delivery as
// well as durable/in-memory accumulation while still counting all source bytes.
func (text *boundedText) AppendRetained(value string) string {
	if text == nil || value == "" {
		return ""
	}
	before := len(text.buf)
	text.originalBytes += len(value)
	if before >= text.limit {
		text.truncated = true
		return ""
	}
	remaining := text.limit - before
	if len(value) <= remaining {
		text.buf = append(text.buf, value...)
		return string(text.buf[before:])
	}
	cut := remaining
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	text.buf = append(text.buf, value[:cut]...)
	text.truncated = true
	return string(text.buf[before:])
}

func (text *boundedText) String() string {
	if text == nil {
		return ""
	}
	return string(text.buf)
}

func (text *boundedText) OriginalBytes() int {
	if text == nil {
		return 0
	}
	return text.originalBytes
}

func (text *boundedText) Truncated() bool {
	return text != nil && text.truncated
}
