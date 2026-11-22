package bee

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	tuiItalic     = "\x1b[3m"
	tuiUnderline  = "\x1b[4m"
	tuiStrike     = "\x1b[9m"
	tuiCodeText   = tuiGreen
	tuiInlineCode = tuiGreen + "\x1b[48;5;238m"
)

type tuiMarkdownSpan struct {
	text  string
	style string
}

type tuiMarkdownLine struct {
	marker      string
	markerStyle string
	spans       []tuiMarkdownSpan
	preserve    bool
	table       *tuiMarkdownTable
}

type tuiMarkdownTableAlignment uint8

const (
	tuiMarkdownAlignLeft tuiMarkdownTableAlignment = iota
	tuiMarkdownAlignCenter
	tuiMarkdownAlignRight
)

type tuiMarkdownTable struct {
	header     []string
	alignments []tuiMarkdownTableAlignment
	rows       [][]string
}

type tuiStyledRune struct {
	value rune
	style string
}

func tuiMarkdownVisualLines(text string, width int) []tuiVisualLine {
	logical := tuiParseMarkdown(text)
	var visual []tuiVisualLine
	first := true
	for _, line := range logical {
		assistantPrefix := "  "
		if first {
			assistantPrefix = "• "
		}
		if line.table != nil {
			wrapped := tuiRenderMarkdownTable(*line.table, width, first)
			visual = append(visual, wrapped...)
			first = false
			continue
		}
		wrapped := tuiWrapMarkdownLine(line, assistantPrefix, width)
		visual = append(visual, wrapped...)
		first = false
	}
	if len(visual) == 0 {
		return []tuiVisualLine{{kind: tuiBlockAssistant, text: "• ", rendered: tuiGreen + "• " + tuiReset}}
	}
	return visual
}

func tuiParseMarkdown(text string) []tuiMarkdownLine {
	rawLines := strings.Split(text, "\n")
	lines := make([]tuiMarkdownLine, 0, len(rawLines))
	var fence string
	for index := 0; index < len(rawLines); index++ {
		raw := rawLines[index]
		trimmed := strings.TrimSpace(raw)
		if fence != "" {
			if tuiClosesFence(trimmed, fence) {
				lines = append(lines, tuiMarkdownLine{marker: "└─", markerStyle: tuiGreen, preserve: true})
				fence = ""
				continue
			}
			lines = append(lines, tuiMarkdownLine{
				marker:      "│ ",
				markerStyle: tuiGreen,
				spans:       []tuiMarkdownSpan{{text: raw, style: tuiCodeText}},
				preserve:    true,
			})
			continue
		}
		if opening, language, ok := tuiOpensFence(trimmed); ok {
			fence = opening
			if language == "" {
				language = "code"
			}
			lines = append(lines, tuiMarkdownLine{
				marker:      "┌─ ",
				markerStyle: tuiGreen,
				spans:       []tuiMarkdownSpan{{text: language, style: tuiGreen + tuiBold}},
				preserve:    true,
			})
			continue
		}
		if !strings.HasPrefix(raw, "    ") {
			if table, consumed, ok := tuiMarkdownTableAt(rawLines, index); ok {
				lines = append(lines, tuiMarkdownLine{table: &table})
				index += consumed - 1
				continue
			}
		}
		if trimmed == "" {
			lines = append(lines, tuiMarkdownLine{})
			continue
		}
		if _, heading, ok := tuiMarkdownHeading(trimmed); ok {
			lines = append(lines, tuiMarkdownLine{spans: tuiParseInlineMarkdown(heading, tuiGreen+tuiBold)})
			continue
		}
		if tuiMarkdownRule(trimmed) {
			lines = append(lines, tuiMarkdownLine{spans: []tuiMarkdownSpan{{text: "────────────────", style: tuiGreen}}})
			continue
		}
		if strings.HasPrefix(raw, "    ") {
			lines = append(lines, tuiMarkdownLine{
				marker:      "│ ",
				markerStyle: tuiGreen,
				spans:       []tuiMarkdownSpan{{text: strings.TrimPrefix(raw, "    "), style: tuiCodeText}},
				preserve:    true,
			})
			continue
		}
		indent, content := tuiMarkdownIndent(raw)
		if strings.HasPrefix(content, ">") {
			quoted := strings.TrimSpace(strings.TrimPrefix(content, ">"))
			lines = append(lines, tuiMarkdownLine{
				marker:      indent + "│ ",
				markerStyle: tuiGreen,
				spans:       tuiParseInlineMarkdown(quoted, tuiGreen+tuiItalic),
			})
			continue
		}
		if marker, item, ok := tuiMarkdownListItem(content); ok {
			lines = append(lines, tuiMarkdownLine{
				marker:      indent + marker,
				markerStyle: tuiGreen,
				spans:       tuiParseInlineMarkdown(item, tuiGreen),
			})
			continue
		}
		lines = append(lines, tuiMarkdownLine{spans: tuiParseInlineMarkdown(strings.TrimSpace(raw), tuiGreen)})
	}
	return lines
}

func tuiMarkdownTableAt(lines []string, start int) (tuiMarkdownTable, int, bool) {
	if start < 0 || start+1 >= len(lines) {
		return tuiMarkdownTable{}, 0, false
	}
	header, ok := tuiSplitMarkdownTableRow(lines[start])
	if !ok || len(header) < 2 || len(header) > 16 {
		return tuiMarkdownTable{}, 0, false
	}
	delimiter, ok := tuiSplitMarkdownTableRow(lines[start+1])
	if !ok || len(delimiter) != len(header) {
		return tuiMarkdownTable{}, 0, false
	}
	alignments := make([]tuiMarkdownTableAlignment, len(delimiter))
	for index, cell := range delimiter {
		alignment, ok := tuiMarkdownTableDelimiter(cell)
		if !ok {
			return tuiMarkdownTable{}, 0, false
		}
		alignments[index] = alignment
	}
	table := tuiMarkdownTable{header: header, alignments: alignments}
	consumed := 2
	for start+consumed < len(lines) {
		row, ok := tuiSplitMarkdownTableRow(lines[start+consumed])
		if !ok {
			break
		}
		normalized := make([]string, len(header))
		copy(normalized, row)
		if len(row) > len(header) {
			normalized[len(header)-1] = strings.Join(row[len(header)-1:], " | ")
		}
		table.rows = append(table.rows, normalized)
		consumed++
	}
	return table, consumed, true
}

func tuiSplitMarkdownTableRow(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var cells []string
	var cell strings.Builder
	escaped := false
	inCode := false
	separators := 0
	for _, value := range raw {
		if escaped {
			cell.WriteRune(value)
			escaped = false
			continue
		}
		if value == '\\' {
			cell.WriteRune(value)
			escaped = true
			continue
		}
		if value == '`' {
			inCode = !inCode
			cell.WriteRune(value)
			continue
		}
		if value == '|' && !inCode {
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			separators++
			continue
		}
		cell.WriteRune(value)
	}
	cells = append(cells, strings.TrimSpace(cell.String()))
	if separators == 0 {
		return nil, false
	}
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return cells, len(cells) >= 2
}

func tuiMarkdownTableDelimiter(cell string) (tuiMarkdownTableAlignment, bool) {
	cell = strings.TrimSpace(cell)
	left := strings.HasPrefix(cell, ":")
	right := strings.HasSuffix(cell, ":")
	cell = strings.TrimPrefix(cell, ":")
	cell = strings.TrimSuffix(cell, ":")
	if len(cell) < 3 || strings.Trim(cell, "-") != "" {
		return tuiMarkdownAlignLeft, false
	}
	switch {
	case left && right:
		return tuiMarkdownAlignCenter, true
	case right:
		return tuiMarkdownAlignRight, true
	default:
		return tuiMarkdownAlignLeft, true
	}
}

func tuiOpensFence(trimmed string) (string, string, bool) {
	if len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return "", "", false
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == trimmed[0] {
		count++
	}
	if count < 3 {
		return "", "", false
	}
	return trimmed[:count], strings.TrimSpace(trimmed[count:]), true
}

func tuiClosesFence(trimmed, fence string) bool {
	if len(trimmed) < len(fence) || !strings.HasPrefix(trimmed, fence) {
		return false
	}
	return strings.TrimSpace(trimmed[len(fence):]) == ""
}

func tuiMarkdownHeading(trimmed string) (int, string, bool) {
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(trimmed[level:]), true
}

func tuiMarkdownRule(trimmed string) bool {
	compact := strings.ReplaceAll(trimmed, " ", "")
	if len(compact) < 3 {
		return false
	}
	marker := compact[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	for index := 1; index < len(compact); index++ {
		if compact[index] != marker {
			return false
		}
	}
	return true
}

func tuiMarkdownIndent(raw string) (string, string) {
	count := 0
	for count < len(raw) && raw[count] == ' ' {
		count++
	}
	if count > 6 {
		count = 6
	}
	return strings.Repeat(" ", count), strings.TrimSpace(raw)
}

func tuiMarkdownListItem(content string) (string, string, bool) {
	if len(content) >= 2 && (content[0] == '-' || content[0] == '*' || content[0] == '+') && content[1] == ' ' {
		item := strings.TrimSpace(content[2:])
		switch {
		case strings.HasPrefix(strings.ToLower(item), "[x] "):
			return "☑ ", strings.TrimSpace(item[4:]), true
		case strings.HasPrefix(item, "[ ] "):
			return "☐ ", strings.TrimSpace(item[4:]), true
		default:
			return "• ", item, true
		}
	}
	index := 0
	for index < len(content) && content[index] >= '0' && content[index] <= '9' {
		index++
	}
	if index > 0 && index+1 < len(content) && (content[index] == '.' || content[index] == ')') && content[index+1] == ' ' {
		return content[:index+2], strings.TrimSpace(content[index+2:]), true
	}
	return "", "", false
}

func tuiParseInlineMarkdown(text, inheritedStyle string) []tuiMarkdownSpan {
	var spans []tuiMarkdownSpan
	for len(text) > 0 {
		if text[0] == '\\' && len(text) > 1 {
			_, size := utf8.DecodeRuneInString(text[1:])
			tuiAppendMarkdownSpan(&spans, text[1:1+size], inheritedStyle)
			text = text[1+size:]
			continue
		}
		if strings.HasPrefix(text, "**") || strings.HasPrefix(text, "__") {
			marker := text[:2]
			if end := strings.Index(text[2:], marker); end >= 0 {
				inner := text[2 : 2+end]
				spans = append(spans, tuiParseInlineMarkdown(inner, inheritedStyle+tuiBold)...)
				text = text[2+end+2:]
				continue
			}
		}
		if strings.HasPrefix(text, "~~") {
			if end := strings.Index(text[2:], "~~"); end >= 0 {
				inner := text[2 : 2+end]
				spans = append(spans, tuiParseInlineMarkdown(inner, inheritedStyle+tuiStrike)...)
				text = text[2+end+2:]
				continue
			}
		}
		if text[0] == '`' {
			if end := strings.IndexByte(text[1:], '`'); end >= 0 {
				tuiAppendMarkdownSpan(&spans, text[1:1+end], tuiInlineCode)
				text = text[1+end+1:]
				continue
			}
		}
		if text[0] == '[' {
			if closeLabel := strings.Index(text, "]("); closeLabel > 0 {
				if closeURL := strings.IndexByte(text[closeLabel+2:], ')'); closeURL >= 0 {
					label := text[1:closeLabel]
					url := text[closeLabel+2 : closeLabel+2+closeURL]
					spans = append(spans, tuiParseInlineMarkdown(label, inheritedStyle+tuiUnderline)...)
					if strings.TrimSpace(url) != "" {
						tuiAppendMarkdownSpan(&spans, " ("+url+")", inheritedStyle)
					}
					text = text[closeLabel+2+closeURL+1:]
					continue
				}
			}
		}
		if text[0] == '*' || text[0] == '_' {
			marker := text[0]
			if end := strings.IndexByte(text[1:], marker); end >= 0 {
				inner := text[1 : 1+end]
				spans = append(spans, tuiParseInlineMarkdown(inner, inheritedStyle+tuiItalic)...)
				text = text[1+end+1:]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(text)
		tuiAppendMarkdownSpan(&spans, text[:size], inheritedStyle)
		text = text[size:]
	}
	return spans
}

func tuiAppendMarkdownSpan(spans *[]tuiMarkdownSpan, text, style string) {
	if text == "" {
		return
	}
	if len(*spans) > 0 && (*spans)[len(*spans)-1].style == style {
		(*spans)[len(*spans)-1].text += text
		return
	}
	*spans = append(*spans, tuiMarkdownSpan{text: text, style: style})
}

func tuiRenderMarkdownTable(table tuiMarkdownTable, width int, first bool) []tuiVisualLine {
	columns := len(table.header)
	if columns == 0 {
		return nil
	}
	styledRows := make([][][]tuiStyledRune, len(table.rows)+1)
	naturalWidths := make([]int, columns)
	for rowIndex := range styledRows {
		styledRows[rowIndex] = make([][]tuiStyledRune, columns)
		for column := 0; column < columns; column++ {
			value := table.header[column]
			style := tuiGreen + tuiBold
			if rowIndex > 0 {
				value = table.rows[rowIndex-1][column]
				style = tuiGreen
			}
			styled := tuiMarkdownRunes(tuiParseInlineMarkdown(value, style))
			styledRows[rowIndex][column] = styled
			if cellWidth := tuiStyledMarkdownWidth(styled); cellWidth > naturalWidths[column] {
				naturalWidths[column] = cellWidth
			}
		}
	}
	columnWidths, ok := tuiMarkdownTableColumnWidths(naturalWidths, width)
	if !ok {
		return tuiRenderMarkdownTableStacked(table, width, first)
	}

	var visual []tuiVisualLine
	lineIndex := 0
	appendLine := func(content []tuiStyledRune) {
		prefix := "  "
		if first && lineIndex == 0 {
			prefix = "• "
		}
		visual = append(visual, tuiMarkdownRenderedLine(prefix, prefix, "", "", content))
		lineIndex++
	}
	appendLine(tuiMarkdownTableBorder(columnWidths, '┌', '┬', '┐'))
	for rowIndex, cells := range styledRows {
		for _, content := range tuiMarkdownTableRow(cells, columnWidths, table.alignments) {
			appendLine(content)
		}
		if rowIndex+1 < len(styledRows) {
			appendLine(tuiMarkdownTableBorder(columnWidths, '├', '┼', '┤'))
		}
	}
	appendLine(tuiMarkdownTableBorder(columnWidths, '└', '┴', '┘'))
	return visual
}

func tuiMarkdownTableColumnWidths(natural []int, width int) ([]int, bool) {
	columns := len(natural)
	if columns == 0 || columns > 8 {
		return nil, false
	}
	contentBudget := width - 2 - (3*columns + 1)
	if contentBudget < columns {
		return nil, false
	}
	minimum := make([]int, columns)
	widths := make([]int, columns)
	minimumTotal := 0
	for index, naturalWidth := range natural {
		if naturalWidth < 1 {
			naturalWidth = 1
			natural[index] = naturalWidth
		}
		minimum[index] = naturalWidth
		if minimum[index] > 4 {
			minimum[index] = 4
		}
		widths[index] = minimum[index]
		minimumTotal += minimum[index]
	}
	if minimumTotal > contentBudget {
		return nil, false
	}
	remaining := contentBudget - minimumTotal
	for remaining > 0 {
		grew := false
		for index := range widths {
			if widths[index] >= natural[index] {
				continue
			}
			widths[index]++
			remaining--
			grew = true
			if remaining == 0 {
				break
			}
		}
		if !grew {
			break
		}
	}
	return widths, true
}

func tuiMarkdownTableBorder(widths []int, left, middle, right rune) []tuiStyledRune {
	var line []tuiStyledRune
	line = append(line, tuiStyledRune{value: left, style: tuiGreen})
	for index, width := range widths {
		for count := 0; count < width+2; count++ {
			line = append(line, tuiStyledRune{value: '─', style: tuiGreen})
		}
		separator := middle
		if index == len(widths)-1 {
			separator = right
		}
		line = append(line, tuiStyledRune{value: separator, style: tuiGreen})
	}
	return line
}

func tuiMarkdownTableRow(cells [][]tuiStyledRune, widths []int, alignments []tuiMarkdownTableAlignment) [][]tuiStyledRune {
	wrapped := make([][][]tuiStyledRune, len(widths))
	height := 1
	for index := range widths {
		wrapped[index] = tuiWrapMarkdownTableCell(cells[index], widths[index])
		if len(wrapped[index]) > height {
			height = len(wrapped[index])
		}
	}
	lines := make([][]tuiStyledRune, 0, height)
	for row := 0; row < height; row++ {
		line := []tuiStyledRune{{value: '│', style: tuiGreen}}
		for column, width := range widths {
			var content []tuiStyledRune
			if row < len(wrapped[column]) {
				content = wrapped[column][row]
			}
			contentWidth := tuiStyledMarkdownWidth(content)
			padding := width - contentWidth
			leftPadding := 0
			switch alignments[column] {
			case tuiMarkdownAlignCenter:
				leftPadding = padding / 2
			case tuiMarkdownAlignRight:
				leftPadding = padding
			}
			line = append(line, tuiStyledRune{value: ' ', style: tuiGreen})
			line = append(line, tuiMarkdownSpaces(leftPadding, tuiGreen)...)
			line = append(line, content...)
			line = append(line, tuiMarkdownSpaces(padding-leftPadding, tuiGreen)...)
			line = append(line, tuiStyledRune{value: ' ', style: tuiGreen})
			line = append(line, tuiStyledRune{value: '│', style: tuiGreen})
		}
		lines = append(lines, line)
	}
	return lines
}

func tuiWrapMarkdownTableCell(cell []tuiStyledRune, width int) [][]tuiStyledRune {
	cell = tuiTrimStyledMarkdownSpace(cell)
	if len(cell) == 0 {
		return [][]tuiStyledRune{nil}
	}
	var lines [][]tuiStyledRune
	for len(cell) > 0 {
		fit := 0
		used := 0
		lastSpace := -1
		for fit < len(cell) {
			runeWidth := tuiRuneWidth(cell[fit].value)
			if fit > 0 && used+runeWidth > width {
				break
			}
			used += runeWidth
			if unicode.IsSpace(cell[fit].value) {
				lastSpace = fit
			}
			fit++
			if used >= width {
				break
			}
		}
		if fit >= len(cell) {
			lines = append(lines, tuiTrimStyledMarkdownSpace(cell))
			break
		}
		consume := fit
		if lastSpace > 0 {
			fit = lastSpace
			consume = lastSpace + 1
		}
		lines = append(lines, tuiTrimStyledMarkdownSpace(cell[:fit]))
		cell = tuiTrimStyledMarkdownSpace(cell[consume:])
	}
	return lines
}

func tuiTrimStyledMarkdownSpace(value []tuiStyledRune) []tuiStyledRune {
	start := 0
	for start < len(value) && unicode.IsSpace(value[start].value) {
		start++
	}
	end := len(value)
	for end > start && unicode.IsSpace(value[end-1].value) {
		end--
	}
	return value[start:end]
}

func tuiStyledMarkdownWidth(value []tuiStyledRune) int {
	width := 0
	for _, styled := range value {
		width += tuiRuneWidth(styled.value)
	}
	return width
}

func tuiMarkdownSpaces(count int, style string) []tuiStyledRune {
	if count <= 0 {
		return nil
	}
	spaces := make([]tuiStyledRune, count)
	for index := range spaces {
		spaces[index] = tuiStyledRune{value: ' ', style: style}
	}
	return spaces
}

func tuiRenderMarkdownTableStacked(table tuiMarkdownTable, width int, first bool) []tuiVisualLine {
	var visual []tuiVisualLine
	rows := table.rows
	if len(rows) == 0 {
		rows = [][]string{make([]string, len(table.header))}
	}
	for rowIndex, row := range rows {
		for column, label := range table.header {
			spans := tuiParseInlineMarkdown(label, tuiGreen+tuiBold)
			tuiAppendMarkdownSpan(&spans, ": ", tuiGreen)
			spans = append(spans, tuiParseInlineMarkdown(row[column], tuiGreen)...)
			prefix := "  "
			if first && len(visual) == 0 {
				prefix = "• "
			}
			visual = append(visual, tuiWrapMarkdownLine(tuiMarkdownLine{spans: spans}, prefix, width)...)
		}
		if rowIndex+1 < len(rows) {
			visual = append(visual, tuiWrapMarkdownLine(tuiMarkdownLine{}, "  ", width)...)
		}
	}
	return visual
}

func tuiWrapMarkdownLine(line tuiMarkdownLine, assistantPrefix string, width int) []tuiVisualLine {
	if width < 1 {
		width = 1
	}
	prefix := assistantPrefix + line.marker
	prefixWidth := utf8.RuneCountInString(prefix)
	available := width - prefixWidth
	if available < 1 {
		available = 1
	}
	styled := tuiMarkdownRunes(line.spans)
	if len(styled) == 0 {
		return []tuiVisualLine{tuiMarkdownRenderedLine(prefix, assistantPrefix, line.marker, line.markerStyle, nil)}
	}
	var visual []tuiVisualLine
	first := true
	for len(styled) > 0 {
		count, consumed := tuiMarkdownBreak(styled, available, line.preserve)
		chunk := styled[:count]
		styled = styled[consumed:]
		linePrefix := strings.Repeat(" ", prefixWidth)
		assistantPart := linePrefix
		markerPart := ""
		if first {
			linePrefix = prefix
			assistantPart = assistantPrefix
			markerPart = line.marker
		}
		visual = append(visual, tuiMarkdownRenderedLine(linePrefix, assistantPart, markerPart, line.markerStyle, chunk))
		first = false
	}
	return visual
}

func tuiMarkdownRunes(spans []tuiMarkdownSpan) []tuiStyledRune {
	var runes []tuiStyledRune
	for _, span := range spans {
		for _, value := range []rune(span.text) {
			runes = append(runes, tuiStyledRune{value: value, style: span.style})
		}
	}
	return runes
}

func tuiMarkdownBreak(runes []tuiStyledRune, available int, preserve bool) (int, int) {
	if len(runes) <= available {
		return len(runes), len(runes)
	}
	if preserve {
		return available, available
	}
	minimum := available / 3
	for index := available; index > minimum; index-- {
		if unicode.IsSpace(runes[index-1].value) {
			consumed := index
			for consumed < len(runes) && unicode.IsSpace(runes[consumed].value) {
				consumed++
			}
			if index-1 > 0 {
				return index - 1, consumed
			}
		}
	}
	return available, available
}

func tuiMarkdownRenderedLine(plainPrefix, assistantPart, markerPart, markerStyle string, content []tuiStyledRune) tuiVisualLine {
	var rendered strings.Builder
	if assistantPart == "• " {
		rendered.WriteString(tuiGreen)
		rendered.WriteString(assistantPart)
		rendered.WriteString(tuiReset)
	} else {
		rendered.WriteString(assistantPart)
	}
	if markerPart != "" {
		rendered.WriteString(markerStyle)
		rendered.WriteString(markerPart)
		rendered.WriteString(tuiReset)
	}
	currentStyle := ""
	for _, styled := range content {
		if styled.style != currentStyle {
			if currentStyle != "" {
				rendered.WriteString(tuiReset)
			}
			if styled.style != "" {
				rendered.WriteString(styled.style)
			}
			currentStyle = styled.style
		}
		rendered.WriteRune(styled.value)
	}
	if currentStyle != "" {
		rendered.WriteString(tuiReset)
	}
	plain := plainPrefix
	for _, styled := range content {
		plain += string(styled.value)
	}
	return tuiVisualLine{kind: tuiBlockAssistant, text: plain, rendered: rendered.String()}
}
