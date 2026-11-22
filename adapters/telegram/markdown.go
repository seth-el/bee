package telegram

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Telegram renders a small HTML subset (parse_mode HTML): b, i, s, code,
// pre (with a copy button in current clients), a, blockquote, tg-spoiler.
// The model writes Markdown, so replies are converted to that subset.
// Anything the converter does not recognize is sent as escaped literal
// text, never as markup, and every chunk keeps a plain-text twin that is
// sent instead if Telegram refuses the HTML.

// FormattedChunk is one Telegram message: HTML to send, and the plain text
// to send instead if Telegram rejects the HTML.
type FormattedChunk struct {
	HTML  string
	Plain string
}

type mdBlockKind int

const (
	mdLine mdBlockKind = iota
	mdCode
	mdQuote
)

type mdBlock struct {
	kind  mdBlockKind
	lang  string
	lines []string
}

var (
	mdFenceOpen   = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})\\s*([^`\\s]*)")
	mdHeading     = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
	mdRule        = regexp.MustCompile(`^\s{0,3}([-*_])(?:\s*[-*_]){2,}\s*$`)
	mdBullet      = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdTaskBox     = regexp.MustCompile(`^\[([ xX])\]\s+`)
	mdTableRule   = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	mdLanguageTag = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,32}$`)
)

// DefaultTableWidth is the widest table, in monospace columns, shown as an
// aligned grid. A phone in portrait fits about this much of a code block
// before Telegram wraps lines, which scrambles the columns.
const DefaultTableWidth = 36

// MarkdownOptions tunes FormatMarkdown. Zero values take the defaults.
type MarkdownOptions struct {
	MaxBytes   int
	TableWidth int
}

// MarkdownChunks is FormatMarkdown with the default table width.
func MarkdownChunks(text string, maxBytes int) []FormattedChunk {
	return FormatMarkdown(text, MarkdownOptions{MaxBytes: maxBytes})
}

// FormatMarkdown converts Markdown to Telegram HTML split into messages of
// at most MaxBytes each (HTML and plain alike). Code blocks, quotes and
// tables that cross a split are closed and reopened in the next message.
func FormatMarkdown(text string, options MarkdownOptions) []FormattedChunk {
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxTelegramTextBytes
	}
	if maxBytes < 64 {
		maxBytes = 64
	}
	var chunks []FormattedChunk
	var html, plain strings.Builder
	flush := func() {
		h := strings.Trim(html.String(), "\n")
		p := strings.Trim(plain.String(), "\n")
		if strings.TrimSpace(p) != "" && strings.TrimSpace(h) != "" {
			chunks = append(chunks, FormattedChunk{HTML: h, Plain: p})
		}
		html.Reset()
		plain.Reset()
	}
	for _, block := range parseMarkdownBlocks(text, options.TableWidth) {
		for _, unit := range fitBlock(block, maxBytes) {
			sep := 0
			if html.Len() > 0 {
				sep = 1
			}
			if html.Len()+sep+len(unit.HTML) > maxBytes || plain.Len()+sep+len(unit.Plain) > maxBytes {
				flush()
				sep = 0
			}
			if sep == 1 {
				html.WriteByte('\n')
				plain.WriteByte('\n')
			}
			html.WriteString(unit.HTML)
			plain.WriteString(unit.Plain)
		}
	}
	flush()
	return chunks
}

// RenderTelegramHTML converts Markdown to Telegram HTML without splitting.
func RenderTelegramHTML(text string) string {
	var parts []string
	for _, block := range parseMarkdownBlocks(text, 0) {
		parts = append(parts, renderBlock(block).HTML)
	}
	return strings.Trim(strings.Join(parts, "\n"), "\n")
}

func parseMarkdownBlocks(text string, tableWidth int) []mdBlock {
	if tableWidth <= 0 {
		tableWidth = DefaultTableWidth
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var blocks []mdBlock
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if match := mdFenceOpen.FindStringSubmatch(line); match != nil {
			fence := match[1]
			lang := match[2]
			if !mdLanguageTag.MatchString(lang) {
				lang = ""
			}
			var body []string
			i++
			for ; i < len(lines); i++ {
				trimmed := strings.TrimSpace(lines[i])
				if strings.HasPrefix(trimmed, fence[:3]) && strings.Trim(trimmed, fence[:1]) == "" && len(trimmed) >= len(fence) {
					break
				}
				body = append(body, lines[i])
			}
			blocks = append(blocks, mdBlock{kind: mdCode, lang: lang, lines: body})
			continue
		}
		if isTableRow(line) && i+1 < len(lines) && mdTableRule.MatchString(lines[i+1]) && strings.Contains(lines[i+1], "-") {
			rows := []string{line}
			j := i + 1
			for ; j < len(lines) && isTableRow(lines[j]); j++ {
				rows = append(rows, lines[j])
			}
			blocks = append(blocks, tableBlocks(rows, tableWidth)...)
			i = j - 1
			continue
		}
		if quoted, ok := quoteContent(line); ok {
			body := []string{quoted}
			for i+1 < len(lines) {
				next, ok := quoteContent(lines[i+1])
				if !ok {
					break
				}
				body = append(body, next)
				i++
			}
			blocks = append(blocks, mdBlock{kind: mdQuote, lines: body})
			continue
		}
		blocks = append(blocks, mdBlock{kind: mdLine, lines: []string{line}})
	}
	return blocks
}

func isTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2
}

func quoteContent(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || !strings.HasPrefix(trimmed, ">") {
		return "", false
	}
	content := strings.TrimPrefix(trimmed, ">")
	return strings.TrimPrefix(content, " "), true
}

// tableBlocks lays out a Markdown table. Telegram cannot draw tables, so a
// table that fits a phone screen becomes an aligned monospace grid; one
// that is wider, or holds emoji or wide characters (which break monospace
// alignment), becomes one short record per row, which always reads
// cleanly.
func tableBlocks(rows []string, width int) []mdBlock {
	header := splitTableCells(rows[0])
	body := make([][]string, 0, len(rows)-2)
	for _, row := range rows[2:] {
		body = append(body, splitTableCells(row))
	}
	columns := len(header)
	for _, row := range body {
		columns = max(columns, len(row))
	}
	plain := make([][]string, 0, len(body)+1)
	for _, row := range append([][]string{header}, body...) {
		cells := make([]string, columns)
		for column := range cells {
			if column < len(row) {
				cells[column] = plainInline(row[column])
			}
		}
		plain = append(plain, cells)
	}
	widths := make([]int, columns)
	grid := true
	for _, row := range plain {
		for column, cell := range row {
			if !monospaceSafe(cell) {
				grid = false
			}
			widths[column] = max(widths[column], utf8.RuneCountInString(cell))
		}
	}
	total := 3 * (columns - 1)
	for _, w := range widths {
		total += w
	}
	if grid && total <= width {
		return []mdBlock{{kind: mdCode, lines: gridLines(plain, widths)}}
	}
	return recordBlocks(header, body, columns)
}

func gridLines(rows [][]string, widths []int) []string {
	out := make([]string, 0, len(rows)+1)
	for index, row := range rows {
		var line strings.Builder
		for column, cell := range row {
			if column > 0 {
				line.WriteString(" │ ")
			}
			line.WriteString(cell)
			if column < len(row)-1 {
				line.WriteString(strings.Repeat(" ", widths[column]-utf8.RuneCountInString(cell)))
			}
		}
		out = append(out, strings.TrimRight(line.String(), " "))
		if index == 0 {
			parts := make([]string, len(widths))
			for column, w := range widths {
				parts[column] = strings.Repeat("─", max(w, 1))
			}
			out = append(out, strings.Join(parts, "─┼─"))
		}
	}
	return out
}

// recordBlocks shows each row as its own small block: the first cell in
// bold, then "Header: value" for the rest. Two-column tables put the value
// on the same line. Cells keep their inline formatting.
func recordBlocks(header []string, body [][]string, columns int) []mdBlock {
	labels := make([]string, columns)
	for column := range labels {
		if column < len(header) {
			labels[column] = plainInline(header[column])
		}
	}
	cell := func(row []string, column int) string {
		if column < len(row) {
			return strings.TrimSpace(row[column])
		}
		return ""
	}
	var blocks []mdBlock
	line := func(text string) { blocks = append(blocks, mdBlock{kind: mdLine, lines: []string{text}}) }
	for index, row := range body {
		title := cell(row, 0)
		if strings.HasPrefix(title, "**") && strings.HasSuffix(title, "**") && len(title) > 4 {
			title = title[2 : len(title)-2]
		}
		if title != "" {
			title = "**" + title + "**"
		}
		switch {
		case columns == 1:
			line(cell(row, 0))
			continue
		case columns == 2:
			value := cell(row, 1)
			if title == "" {
				line(value)
			} else if value == "" {
				line(title)
			} else {
				line(title + " — " + value)
			}
			continue
		}
		if index > 0 {
			line("")
		}
		if title != "" {
			line(title)
		}
		for column := 1; column < columns; column++ {
			value := cell(row, column)
			if value == "" {
				continue
			}
			if labels[column] != "" {
				value = labels[column] + ": " + value
			}
			line(value)
		}
	}
	return blocks
}

// splitTableCells splits a table row on unescaped pipes outside code spans.
func splitTableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, `\|`) {
		row = row[:len(row)-1]
	}
	var cells []string
	var current strings.Builder
	inCode := 0
	for i := 0; i < len(row); i++ {
		c := row[i]
		switch {
		case c == '\\' && i+1 < len(row) && row[i+1] == '|':
			current.WriteByte('|')
			i++
			continue
		case c == '`':
			run := runLength(row, i, '`')
			if inCode == 0 {
				inCode = run
			} else if run == inCode {
				inCode = 0
			}
			current.WriteString(row[i : i+run])
			i += run - 1
			continue
		case c == '|' && inCode == 0:
			cells = append(cells, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}
		current.WriteByte(c)
	}
	return append(cells, strings.TrimSpace(current.String()))
}

var mdInlineLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)\s]*\)`)

// plainInline drops inline Markdown markers, keeping link labels (for
// table cells shown monospace, and header labels).
func plainInline(text string) string {
	text = mdInlineLink.ReplaceAllString(text, "$1")
	for _, marker := range []string{"**", "__", "~~", "`"} {
		text = strings.ReplaceAll(text, marker, "")
	}
	return strings.TrimSpace(text)
}

// monospaceSafe reports whether text lines up in a monospace block: no
// emoji, CJK or other wide characters, and no combining or joining marks.
func monospaceSafe(text string) bool {
	for _, r := range text {
		switch {
		case r < 0x1100:
		case unicode.Is(unicode.Mn, r), r == 0x200D, r >= 0xFE00 && r <= 0xFE0F:
			return false
		case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
			r >= 0xFFE0 && r <= 0xFFE6, r >= 0x2600 && r <= 0x27BF, r >= 0x2B00 && r <= 0x2BFF,
			r >= 0x1F000:
			return false
		}
	}
	return true
}

// fitBlock renders a block, splitting it until each piece fits maxBytes.
func fitBlock(block mdBlock, maxBytes int) []FormattedChunk {
	unit := renderBlock(block)
	if len(unit.HTML) <= maxBytes && len(unit.Plain) <= maxBytes {
		return []FormattedChunk{unit}
	}
	first, second, ok := splitBlock(block)
	if !ok {
		escaped := escapeHTML(unit.Plain)
		return []FormattedChunk{{HTML: escaped, Plain: unit.Plain}}
	}
	return append(fitBlock(first, maxBytes), fitBlock(second, maxBytes)...)
}

func splitBlock(block mdBlock) (mdBlock, mdBlock, bool) {
	if len(block.lines) > 1 {
		half := len(block.lines) / 2
		return mdBlock{kind: block.kind, lang: block.lang, lines: block.lines[:half]},
			mdBlock{kind: block.kind, lang: block.lang, lines: block.lines[half:]}, true
	}
	if len(block.lines) == 0 {
		return mdBlock{}, mdBlock{}, false
	}
	line := block.lines[0]
	if utf8.RuneCountInString(line) < 2 {
		return mdBlock{}, mdBlock{}, false
	}
	cut := splitPoint(line, block.kind == mdCode)
	return mdBlock{kind: block.kind, lang: block.lang, lines: []string{line[:cut]}},
		mdBlock{kind: block.kind, lang: block.lang, lines: []string{strings.TrimLeft(line[cut:], " ")}}, true
}

// splitPoint picks a cut near the middle of line: a space for prose, any
// rune boundary for code.
func splitPoint(line string, code bool) int {
	middle := len(line) / 2
	for middle > 0 && !utf8.RuneStart(line[middle]) {
		middle--
	}
	if !code {
		if space := strings.LastIndexByte(line[:middle], ' '); space > len(line)/4 {
			return space
		}
		if space := strings.IndexByte(line[middle:], ' '); space >= 0 && middle+space < len(line)*3/4 {
			return middle + space
		}
	}
	if middle == 0 {
		_, size := utf8.DecodeRuneInString(line)
		middle = size
	}
	return middle
}

func renderBlock(block mdBlock) FormattedChunk {
	switch block.kind {
	case mdCode:
		body := strings.Join(block.lines, "\n")
		fence := "```" + block.lang + "\n" + body + "\n```"
		if strings.TrimSpace(body) == "" {
			return FormattedChunk{}
		}
		if block.lang != "" {
			return FormattedChunk{HTML: `<pre><code class="language-` + escapeHTML(block.lang) + `">` + escapeHTML(body) + "</code></pre>", Plain: fence}
		}
		return FormattedChunk{HTML: "<pre>" + escapeHTML(body) + "</pre>", Plain: fence}
	case mdQuote:
		rendered := make([]string, len(block.lines))
		plain := make([]string, len(block.lines))
		for index, line := range block.lines {
			rendered[index] = renderLine(line)
			plain[index] = "> " + line
		}
		return FormattedChunk{HTML: "<blockquote>" + strings.Join(rendered, "\n") + "</blockquote>", Plain: strings.Join(plain, "\n")}
	default:
		line := strings.Join(block.lines, " ")
		return FormattedChunk{HTML: renderLine(line), Plain: line}
	}
}

func renderLine(line string) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	if match := mdHeading.FindStringSubmatch(line); match != nil {
		return "<b>" + renderInline(match[1], tagBold) + "</b>"
	}
	if mdRule.MatchString(line) {
		return "──────────"
	}
	if match := mdBullet.FindStringSubmatch(line); match != nil {
		rest := match[2]
		marker := "• "
		if box := mdTaskBox.FindStringSubmatch(rest); box != nil {
			marker = "☐ "
			if box[1] != " " {
				marker = "☑ "
			}
			rest = rest[len(box[0]):]
		}
		return match[1] + marker + renderInline(rest, 0)
	}
	return renderInline(line, 0)
}

type inlineTag uint8

const (
	tagBold inlineTag = 1 << iota
	tagItalic
	tagStrike
	tagSpoiler
	tagLink
)

var inlineDelimiters = []struct {
	marker string
	tag    inlineTag
	html   string
}{
	{"**", tagBold, "b"},
	{"__", tagBold, "b"},
	{"~~", tagStrike, "s"},
	{"||", tagSpoiler, "tg-spoiler"},
	{"*", tagItalic, "i"},
	{"_", tagItalic, "i"},
}

func renderInline(text string, active inlineTag) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		if c == '\\' && i+1 < len(text) && strings.IndexByte("\\`*_{}[]()#+-.!|~>", text[i+1]) >= 0 {
			out.WriteString(escapeHTML(text[i+1 : i+2]))
			i += 2
			continue
		}
		if c == '`' {
			if content, end, ok := codeSpan(text, i); ok {
				out.WriteString("<code>" + escapeHTML(content) + "</code>")
				i = end
				continue
			}
			run := runLength(text, i, '`')
			out.WriteString(text[i : i+run])
			i += run
			continue
		}
		if active&tagLink == 0 && (c == '[' || (c == '!' && i+1 < len(text) && text[i+1] == '[')) {
			start := i
			if c == '!' {
				start++
			}
			if label, target, end, ok := parseLink(text, start); ok {
				if label == "" {
					label = target
				}
				out.WriteString(`<a href="` + escapeHTML(target) + `">` + renderInline(label, active|tagLink) + "</a>")
				i = end
				continue
			}
		}
		if handled, end := renderEmphasis(&out, text, i, active); handled {
			i = end
			continue
		}
		out.WriteString(escapeHTML(text[i : i+1]))
		i++
	}
	return out.String()
}

func renderEmphasis(out *strings.Builder, text string, i int, active inlineTag) (bool, int) {
	for _, delimiter := range inlineDelimiters {
		marker := delimiter.marker
		if !strings.HasPrefix(text[i:], marker) {
			continue
		}
		open := i + len(marker)
		if open >= len(text) || text[open] == ' ' || text[open] == '\t' {
			return false, i
		}
		single := len(marker) == 1
		if single && text[open] == marker[0] {
			return false, i
		}
		if (single || marker == "__") && i > 0 && isWordByte(text, i-1) {
			return false, i
		}
		close := findCloser(text, open, marker)
		if close < 0 {
			return false, i
		}
		inner := text[open:close]
		end := close + len(marker)
		if active&delimiter.tag != 0 {
			out.WriteString(renderInline(inner, active))
		} else {
			out.WriteString("<" + delimiter.html + ">" + renderInline(inner, active|delimiter.tag) + "</" + delimiter.html + ">")
		}
		return true, end
	}
	return false, i
}

// findCloser finds the closing marker for an emphasis run opened before
// from, skipping code spans. A closer is not preceded by a space, and a
// single-character or underscore closer is not followed by a word
// character, so snake_case and 2*3*4 stay literal.
func findCloser(text string, from int, marker string) int {
	for j := from + 1; j < len(text); j++ {
		if text[j] == '`' {
			if _, end, ok := codeSpan(text, j); ok {
				j = end - 1
				continue
			}
		}
		if !strings.HasPrefix(text[j:], marker) {
			continue
		}
		run := runLength(text, j, marker[0])
		if len(marker) == 1 && run > 1 {
			j += run - 1
			continue
		}
		candidate := j + run - len(marker)
		if text[candidate-1] == ' ' || text[candidate-1] == '\t' {
			j += run - 1
			continue
		}
		after := candidate + len(marker)
		if (len(marker) == 1 || marker == "__") && after < len(text) && isWordByte(text, after) {
			j += run - 1
			continue
		}
		return candidate
	}
	return -1
}

func isWordByte(text string, index int) bool {
	r, _ := utf8.DecodeRuneInString(text[index:])
	if !utf8.RuneStart(text[index]) {
		start := index
		for start > 0 && !utf8.RuneStart(text[start]) {
			start--
		}
		r, _ = utf8.DecodeRuneInString(text[start:])
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func runLength(text string, i int, c byte) int {
	n := 0
	for i+n < len(text) && text[i+n] == c {
		n++
	}
	return n
}

func codeSpan(text string, i int) (string, int, bool) {
	run := runLength(text, i, '`')
	for j := i + run; j < len(text); j++ {
		if text[j] != '`' {
			continue
		}
		closing := runLength(text, j, '`')
		if closing == run {
			content := text[i+run : j]
			if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.TrimSpace(content) != "" {
				content = content[1 : len(content)-1]
			}
			if content == "" {
				return "", 0, false
			}
			return content, j + closing, true
		}
		j += closing - 1
	}
	return "", 0, false
}

// parseLink reads [label](target) at i. Only web, mail and Telegram links
// become anchors; anything else stays literal text.
func parseLink(text string, i int) (string, string, int, bool) {
	if i >= len(text) || text[i] != '[' {
		return "", "", 0, false
	}
	depth := 0
	closeLabel := -1
	for j := i; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				closeLabel = j
			}
		}
		if closeLabel >= 0 {
			break
		}
	}
	if closeLabel < 0 || closeLabel+1 >= len(text) || text[closeLabel+1] != '(' {
		return "", "", 0, false
	}
	parens := 0
	for j := closeLabel + 1; j < len(text); j++ {
		switch text[j] {
		case ' ', '\t':
			return "", "", 0, false
		case '(':
			parens++
		case ')':
			parens--
			if parens == 0 {
				target := strings.TrimSuffix(strings.TrimPrefix(text[closeLabel+2:j], "<"), ">")
				if !safeLinkTarget(target) {
					return "", "", 0, false
				}
				return text[i+1 : closeLabel], target, j + 1, true
			}
		}
	}
	return "", "", 0, false
}

func safeLinkTarget(target string) bool {
	lower := strings.ToLower(target)
	for _, prefix := range []string{"https://", "http://", "mailto:", "tg://"} {
		if strings.HasPrefix(lower, prefix) && len(target) > len(prefix) {
			return true
		}
	}
	return false
}

func escapeHTML(text string) string {
	if !strings.ContainsAny(text, `&<>"`) {
		return text
	}
	var out strings.Builder
	for _, r := range text {
		switch r {
		case '&':
			out.WriteString("&amp;")
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		case '"':
			out.WriteString("&quot;")
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
