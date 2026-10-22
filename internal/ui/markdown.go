package ui

import (
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
)

type markdownRenderer struct {
	useStyle    bool
	accentStyle lipgloss.Style
	mutedStyle  lipgloss.Style
	codeStyle   lipgloss.Style
}

func newMarkdownRenderer(useStyle bool, palette terminalPalette) markdownRenderer {
	renderer := markdownRenderer{useStyle: useStyle}
	if useStyle {
		renderer.accentStyle = lipgloss.NewStyle().Foreground(palette.accent)
		renderer.mutedStyle = lipgloss.NewStyle().Foreground(palette.muted)
		// Code shares the accent colour with tool names: both are "this is a
		// literal, not prose". The style stays separate because the roles
		// differ in weight, not in hue.
		renderer.codeStyle = lipgloss.NewStyle().Foreground(palette.accent)
	}
	return renderer
}

func (renderer markdownRenderer) renderMarkdownLines(text string, tableWidth int) []string {
	return renderer.renderMarkdownContinuation("", text, tableWidth)
}

// A committed prefix supplies parser context without contributing output.
// Resizes can split a fence, an inline marker, or a line at any source byte.
func (renderer markdownRenderer) renderMarkdownContinuation(prefix, text string, tableWidth int) []string {
	text = sanitizeTerminalText(text)
	if text == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(text, "\n")
	input := strings.Split(trimmed, "\n")
	var fence markdownFence
	prefix = sanitizeTerminalText(prefix)
	for {
		line, rest, ok := strings.Cut(prefix, "\n")
		if !ok {
			break
		}
		fence.consume(line)
		prefix = rest
	}
	if prefix == "" {
		return renderer.renderLines(input, &fence, max(1, tableWidth))
	}
	// Parse the complete first line, but emit only its unseen source suffix.
	// In particular, a mid-line '#' must not become a heading after a resize.
	line := prefix + input[0]
	var out []string
	if !fence.consume(line) {
		if fence.active() {
			out = append(out, renderer.style(input[0], renderer.codeStyle))
		} else {
			out = append(out, renderer.renderLineFrom(line, len(prefix)))
		}
	}
	return append(out, renderer.renderLines(input[1:], &fence, max(1, tableWidth))...)
}

// renderAnswerMarkdown leaves prose wrapping to the terminal so copied lines
// stay intact; only tables are fitted to width. Without a known width they keep
// their natural size.
func renderAnswerMarkdown(text string, width int) string {
	// A one-shot run cannot ask the terminal for its background without reading
	// stdin. The light palette's accent also stays legible on dark backgrounds.
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	return strings.Join(renderer.renderMarkdownLines(text, width), "\n")
}

type markdownFence struct {
	marker byte
	length int
}

func (fence markdownFence) active() bool { return fence.marker != 0 && fence.length >= 3 }

// consume reports fence delimiter lines, which change context but have no ink.
func (fence *markdownFence) consume(line string) bool {
	if fence.active() {
		if closesMarkdownFence(line, *fence) {
			*fence = markdownFence{}
			return true
		}
	} else if opened, ok := openingMarkdownFence(line); ok {
		*fence = opened
		return true
	}
	return false
}

func openingMarkdownFence(line string) (markdownFence, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return markdownFence{}, false
	}
	marker := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	if length < 3 || marker == '`' && strings.Contains(trimmed[length:], "`") {
		return markdownFence{}, false
	}
	return markdownFence{marker: marker, length: length}, true
}

func closesMarkdownFence(line string, fence markdownFence) bool {
	if !fence.active() {
		return false
	}
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	length := 0
	for length < len(trimmed) && trimmed[length] == fence.marker {
		length++
	}
	return length >= fence.length && strings.TrimSpace(trimmed[length:]) == ""
}

func (renderer markdownRenderer) renderLines(input []string, fence *markdownFence, width int) []string {
	out := make([]string, 0, len(input))
	for index := 0; index < len(input); {
		if fence.consume(input[index]) {
			index++
			continue
		}
		if fence.active() {
			out = append(out, renderer.style(input[index], renderer.codeStyle))
			index++
			continue
		}
		if index+1 < len(input) && isTableSeparatorLine(input[index+1]) && strings.Contains(input[index], "|") {
			end := index + 2
			for end < len(input) && strings.Contains(input[end], "|") && strings.TrimSpace(input[end]) != "" {
				end++
			}
			out = append(out, renderer.renderTable(input[index:end], width)...)
			index = end
			continue
		}
		out = append(out, renderer.renderLine(input[index]))
		index++
	}
	return out
}

func (renderer markdownRenderer) renderLine(line string) string {
	return renderer.renderLineFrom(line, 0)
}

func (renderer markdownRenderer) renderLineFrom(line string, skip int) string {
	if !renderer.useStyle {
		return line[skip:]
	}
	base := lipgloss.NewStyle()
	trimmed := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimmed)]
	if level, text, ok := parseHeading(trimmed); ok {
		if skip > len(indent) {
			content := trimmed[level+1:]
			start := len(indent) + level + 1 + len(content) - len(strings.TrimLeftFunc(content, unicode.IsSpace))
			skip = len(indent) + max(0, skip-start)
		}
		line = indent + text
		base = renderer.accentStyle
		if level < 3 {
			base = base.Bold(true)
		}
	}
	return renderer.renderInlineMarkdownMarkersFrom(line, base, skip)
}

func (renderer markdownRenderer) renderTable(lines []string, maxWidth int) []string {
	rows := make([][]string, 0, len(lines)-1)
	for index, line := range lines {
		if index != 1 {
			rows = append(rows, splitTableCells(line))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	widths := make([]int, columns)
	for _, row := range rows {
		for column, cell := range row {
			widths[column] = max(widths[column], renderer.cellWidth(cell))
		}
	}
	widths = fitTableWidths(widths, maxWidth)

	out := make([]string, 0, len(rows)+1)
	for index, row := range rows {
		out = append(out, renderer.renderTableRows(row, widths, index == 0)...)
		if index == 0 {
			out = append(out, renderer.style(renderTableSeparator(widths), renderer.mutedStyle))
		}
	}
	return out
}

func fitTableWidths(widths []int, maxWidth int) []int {
	fitted := append([]int(nil), widths...)
	if maxWidth <= 0 || len(fitted) == 0 {
		return fitted
	}
	available := maxWidth - (3*len(fitted) + 1)
	if available < len(fitted) {
		return fitted
	}
	for totalWidths(fitted) > available {
		widest := -1
		for index, width := range fitted {
			minimum := min(widths[index], 3)
			if width > minimum && (widest < 0 || width > fitted[widest]) {
				widest = index
			}
		}
		if widest < 0 {
			break
		}
		fitted[widest]--
	}
	return fitted
}

func totalWidths(widths []int) int {
	total := 0
	for _, width := range widths {
		total += width
	}
	return total
}

func (renderer markdownRenderer) renderTableRows(row []string, widths []int, header bool) []string {
	wrappedCells := make([][]string, len(widths))
	height := 1
	for index := range widths {
		cell := ""
		if index < len(row) {
			cell = row[index]
		}
		if renderer.useStyle {
			base := lipgloss.NewStyle()
			if header {
				base = renderer.accentStyle.Bold(true)
			}
			cell = renderer.renderInlineMarkdownMarkers(cell, base)
		}
		wrappedCells[index] = wrapDisplayLine(cell, max(1, widths[index]))
		height = max(height, len(wrappedCells[index]))
	}

	lines := make([]string, 0, height)
	for lineIndex := 0; lineIndex < height; lineIndex++ {
		cells := make([]string, len(widths))
		for column := range widths {
			cell := ""
			if lineIndex < len(wrappedCells[column]) {
				cell = wrappedCells[column][lineIndex]
			}
			if pad := widths[column] - visibleLen(cell); pad > 0 {
				padding := strings.Repeat(" ", pad)
				if header {
					padding = renderer.style(padding, renderer.accentStyle.Bold(true))
				}
				cell += padding
			}
			cells[column] = cell
		}
		separator := renderer.style("|", renderer.mutedStyle)
		lines = append(lines, separator+" "+strings.Join(cells, " "+separator+" ")+" "+separator)
	}
	return lines
}

func (renderer markdownRenderer) cellWidth(cell string) int {
	if renderer.useStyle {
		cell = renderer.renderInlineMarkdownMarkers(cell, lipgloss.NewStyle())
	}
	return visibleLen(cell)
}

func (renderer markdownRenderer) style(text string, style lipgloss.Style) string {
	if !renderer.useStyle || text == "" {
		return text
	}
	return style.Render(text)
}

func renderTableSeparator(widths []int) string {
	parts := make([]string, len(widths))
	for index, width := range widths {
		parts[index] = strings.Repeat("-", max(1, width))
	}
	return "| " + strings.Join(parts, " | ") + " |"
}

func isTableSeparatorLine(line string) bool {
	cells := splitTableCells(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		if cell == "" || strings.Trim(cell, "-:") != "" || !strings.Contains(cell, "-") {
			return false
		}
	}
	return true
}

func splitTableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

func parseHeading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(line[level+1:]), true
}

func (renderer markdownRenderer) renderInlineMarkdownMarkers(text string, base lipgloss.Style) string {
	return renderer.renderInlineMarkdownMarkersFrom(text, base, 0)
}

func (renderer markdownRenderer) renderInlineMarkdownMarkersFrom(text string, base lipgloss.Style, skip int) string {
	var builder strings.Builder
	segmentStart := 0
	bold := false
	code := false
	flush := func(end int) {
		start := max(segmentStart, skip)
		if end <= start {
			return
		}
		style := base
		if code {
			style = style.Foreground(renderer.codeStyle.GetForeground())
		}
		if bold {
			style = style.Bold(true)
		}
		builder.WriteString(style.Render(text[start:end]))
	}
	for index := 0; index < len(text); {
		if !code && strings.HasPrefix(text[index:], "**") {
			flush(index)
			bold = !bold
			index += 2
			segmentStart = index
			continue
		}
		if text[index] == '`' {
			flush(index)
			code = !code
			index++
			segmentStart = index
			continue
		}
		index++
	}
	flush(len(text))
	return builder.String()
}

func wrapDisplayLine(text string, width int) []string {
	wrapped := lipgloss.Wrap(text, max(1, width), "")
	if wrapped == "" {
		return []string{""}
	}
	return strings.Split(wrapped, "\n")
}
