package ui

import "strings"

type mdAlign uint8

const (
	mdAlignLeft mdAlign = iota
	mdAlignCenter
	mdAlignRight
)

// splitTableRow splits a GFM table row into trimmed cells. Escaped pipes and
// pipes inside code spans stay in their cell: models write `a|b` expecting
// code, not a new column.
func splitTableRow(row mdLine) []mdLine {
	text := row.text
	end := len(strings.TrimRight(text, " \t"))
	_, start := leadingIndent(text)
	start = min(start, end)
	if start < end && text[start] == '|' {
		start++
	}
	if end > start && text[end-1] == '|' && !escapedAt(text, end-1) {
		end--
	}
	var cells []mdLine
	cell := func(from, to int) {
		_, n := leadingIndent(text[from:to])
		from += n
		to = from + len(strings.TrimRight(text[from:to], " \t"))
		cells = append(cells, mdLine{text: text[from:to], start: row.start + from, line: row.line})
	}
	cellStart := start
	for index := start; index < end; index++ {
		switch text[index] {
		case '\\':
			index++
		case '`':
			length := runLength(text, index, end, '`')
			if close := matchingBackticks(text, index+length, end, length); close >= 0 {
				index = close
			}
			index += length - 1
		case '|':
			cell(cellStart, index)
			cellStart = index + 1
		}
	}
	cell(cellStart, end)
	return cells
}

func tableDelimiterRow(text string) ([]mdAlign, bool) {
	if !strings.Contains(text, "|") {
		return nil, false
	}
	cells := splitTableRow(mdLine{text: text})
	align := make([]mdAlign, len(cells))
	for index, cell := range cells {
		left, right := strings.HasPrefix(cell.text, ":"), strings.HasSuffix(cell.text, ":")
		dashes := strings.TrimSuffix(strings.TrimPrefix(cell.text, ":"), ":")
		if dashes == "" || strings.Trim(dashes, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			align[index] = mdAlignCenter
		case right:
			align[index] = mdAlignRight
		}
	}
	return align, true
}

func escapedAt(text string, at int) bool {
	backslashes := 0
	for at > 0 && text[at-1] == '\\' {
		backslashes++
		at--
	}
	return backslashes%2 == 1
}

// table renders a GFM table with box-drawing borders, fitted to the
// available width by wrapping its widest columns first.
func (w *markdownWriter) table(table *mdBlock, indent *markdownIndent) {
	columns := len(table.align)
	rows := make([][][]string, 0, len(table.lines)-1)
	for index, line := range table.lines {
		if index == 1 {
			continue
		}
		var base mdStyle
		if index == 0 {
			base = mdBold | mdAccent
		}
		cells := splitTableRow(line)
		row := make([][]string, columns)
		for column := range row {
			if column < len(cells) {
				for _, runs := range w.renderer.inline(cells[column:column+1], false, true) {
					row[column] = append(row[column], w.renderer.paintRuns(runs, base))
				}
			}
		}
		rows = append(rows, row)
	}
	widths := make([]int, columns)
	for _, row := range rows {
		for column, lines := range row {
			for _, line := range lines {
				widths[column] = max(widths[column], visibleLen(line))
			}
		}
	}
	widths = fitTableWidths(widths, w.contentWidth(indent))

	rendered := make([][]string, len(rows))
	separate := false
	for index, row := range rows {
		rendered[index] = w.tableRow(row, widths, table.align)
		separate = separate || index > 0 && len(rendered[index]) > 1
	}
	w.row(indent, table.start, w.tableBorder(widths, "┌", "┬", "┐"))
	for _, line := range rendered[0] {
		w.row(indent, table.start, line)
	}
	last := table.lines[1].line
	w.row(indent, last, w.tableBorder(widths, "├", "┼", "┤"))
	for index, lines := range rendered[1:] {
		last = table.lines[index+2].line
		// Rule between rows only when a wrapped cell would blur them.
		if index > 0 && separate {
			w.row(indent, last, w.tableBorder(widths, "├", "┼", "┤"))
		}
		for _, line := range lines {
			w.row(indent, last, line)
		}
	}
	w.row(indent, last, w.tableBorder(widths, "└", "┴", "┘"))
}

func (w *markdownWriter) tableRow(row [][]string, widths []int, align []mdAlign) []string {
	wrapped := make([][]string, len(widths))
	height := 1
	for column, width := range widths {
		for _, line := range row[column] {
			wrapped[column] = append(wrapped[column], wrapDisplayLine(line, width)...)
		}
		height = max(height, len(wrapped[column]))
	}
	bar := w.renderer.paint("│", mdMuted)
	lines := make([]string, height)
	for index := range lines {
		var line strings.Builder
		line.WriteString(bar)
		for column, width := range widths {
			cell := ""
			if index < len(wrapped[column]) {
				cell = wrapped[column][index]
			}
			pad := max(0, width-visibleLen(cell))
			left := 0
			switch align[column] {
			case mdAlignRight:
				left = pad
			case mdAlignCenter:
				left = pad / 2
			}
			line.WriteString(" " + strings.Repeat(" ", left) + cell + strings.Repeat(" ", pad-left) + " ")
			line.WriteString(bar)
		}
		lines[index] = line.String()
	}
	return lines
}

func (w *markdownWriter) tableBorder(widths []int, left, middle, right string) string {
	var border strings.Builder
	border.WriteString(left)
	for index, width := range widths {
		if index > 0 {
			border.WriteString(middle)
		}
		border.WriteString(strings.Repeat("─", width+2))
	}
	border.WriteString(right)
	return w.renderer.paint(border.String(), mdMuted)
}

// fitTableWidths shrinks the widest columns until the table, including its
// borders, fits maxWidth. Columns keep at least three characters of width.
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
