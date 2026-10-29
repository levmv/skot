package ui

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type markdownRenderer struct {
	useStyle bool
	// Code shares the accent colour with headings and tool names: all of
	// them mark literal or structural text rather than prose.
	accent color.Color
	muted  color.Color
}

func newMarkdownRenderer(useStyle bool, palette terminalPalette) markdownRenderer {
	return markdownRenderer{useStyle: useStyle, accent: palette.accent, muted: palette.muted}
}

type markdownOptions struct {
	width int // columns available; 0 when unknown
	// noWrap leaves prose wrapping to the terminal so copied lines stay
	// intact. Tables and rules still fit width.
	noWrap bool
	// from is the source offset where output starts. Earlier source has
	// already been printed and only supplies parser context; a resize can
	// split a fence, a span, or a line at any byte.
	from int
	// streaming means the source may still grow. Syntax whose meaning
	// depends on text still to come is withheld or rendered as if closed,
	// so markers never flash as raw text.
	streaming bool
}

// render returns terminal lines for source. Without styling, inline markup
// stays as written: emphasis markers are then the only sign of emphasis.
func (renderer markdownRenderer) render(source string, options markdownOptions) []string {
	document := parseMarkdown(sanitizeTerminalText(source), options.streaming)
	writer := markdownWriter{renderer: renderer, options: options}
	writer.blocks(document.blocks, nil, 0)
	return writer.lines
}

// markdownShown returns how much of a streaming source the renderer shows.
// Withheld text has not been printed yet, so it must not count as printed.
func markdownShown(source string) int {
	document := parseMarkdown(source, true)
	for blocks := document.blocks; len(blocks) > 0; {
		last := blocks[len(blocks)-1]
		if last.open {
			_, shown := parseInline(last.lines, true, false)
			return min(document.shown, shown)
		}
		blocks = last.children
	}
	return document.shown
}

// renderAnswerMarkdown leaves prose wrapping to the terminal so copied lines
// stay intact; only tables and rules are fitted to width. Without a known
// width they keep their natural size.
func renderAnswerMarkdown(text string, width int) string {
	// A one-shot run cannot ask the terminal for its background without reading
	// stdin. The light palette's accent also stays legible on dark backgrounds.
	renderer := newMarkdownRenderer(true, terminalPaletteFor(false))
	return strings.Join(renderer.render(text, markdownOptions{width: width, noWrap: true}), "\n")
}

func (renderer markdownRenderer) inline(lines []mdLine, open, table bool) [][]mdRun {
	if renderer.useStyle {
		rows, _ := parseInline(lines, open, table)
		return rows
	}
	rows := make([][]mdRun, len(lines))
	for index, line := range lines {
		text := strings.TrimRight(line.text, " \t")
		run := mdRun{text: text, src: line.start, end: line.start + len(text), exact: true}
		rows[index] = markdownTabRuns(run, false)
	}
	return rows
}

func (renderer markdownRenderer) paint(text string, style mdStyle) string {
	return renderer.paintLink(text, style, "")
}

func (renderer markdownRenderer) paintRuns(runs []mdRun, base mdStyle) string {
	var out strings.Builder
	for _, run := range runs {
		out.WriteString(renderer.paintLink(run.text, base|run.style, run.link))
	}
	return out.String()
}

func (renderer markdownRenderer) paintLink(text string, style mdStyle, link string) string {
	if !renderer.useStyle || text == "" {
		return text
	}
	var sgr ansi.Style
	if style&mdBold != 0 {
		sgr = sgr.Bold()
	}
	if style&mdItalic != 0 {
		sgr = sgr.Italic(true)
	}
	if style&mdStrike != 0 {
		sgr = sgr.Strikethrough(true)
	}
	if style&mdLink != 0 {
		sgr = sgr.Underline(true)
	}
	switch {
	case style&mdMuted != 0:
		sgr = sgr.ForegroundColor(renderer.muted)
	case style&(mdCode|mdAccent) != 0:
		sgr = sgr.ForegroundColor(renderer.accent)
	}
	text = sgr.Styled(text)
	if link != "" {
		// The shared ID keeps all rows of a wrapped link together.
		text = ansi.SetHyperlink(link, "id=skot") + text + ansi.ResetHyperlink()
	}
	return text
}

// markdownIndent is one container's share of the left margin. A list item
// shows its marker on its first line and aligns later lines with its text.
type markdownIndent struct {
	parent      *markdownIndent
	first, rest string
	width       int
	started     bool
}

func (indent *markdownIndent) next() string {
	if indent == nil {
		return ""
	}
	prefix := indent.parent.next()
	if indent.started {
		return prefix + indent.rest
	}
	indent.started = true
	return prefix + indent.first
}

func (indent *markdownIndent) total() int {
	if indent == nil {
		return 0
	}
	return indent.width + indent.parent.total()
}

var markdownBullets = [...]string{"•", "◦", "▪"}

type markdownWriter struct {
	renderer markdownRenderer
	options  markdownOptions
	lines    []string
}

// blocks keeps the source's blank lines between blocks, collapsing runs of
// them, instead of imposing its own spacing.
func (w *markdownWriter) blocks(blocks []*mdBlock, indent *markdownIndent, depth int) {
	for index, block := range blocks {
		if index > 0 && block.blank {
			w.row(indent, block.start, "")
		}
		w.block(block, indent, depth)
	}
}

func (w *markdownWriter) block(block *mdBlock, indent *markdownIndent, depth int) {
	switch block.kind {
	case mdParagraph:
		w.text(indent, w.renderer.inline(block.lines, block.open, false), 0)
	case mdHeading:
		style := mdAccent
		if block.level < 3 {
			style |= mdBold
		}
		rows := w.renderer.inline(block.lines, block.open, false)
		if !w.renderer.useStyle {
			marker := mdRun{text: strings.Repeat("#", block.level) + " ", src: block.start, end: block.lines[0].start}
			rows[0] = append([]mdRun{marker}, rows[0]...)
		}
		w.text(indent, rows, style)
	case mdCodeBlock:
		for _, line := range block.lines {
			w.code(indent, line)
		}
	case mdRule:
		width := w.contentWidth(indent)
		if width == 0 {
			// Without a known width, a short rule still reads as a separator.
			width = 40
		}
		w.row(indent, block.start, w.renderer.paint(strings.Repeat("─", width), mdMuted))
	case mdTable:
		w.table(block, indent)
	case mdQuote:
		bar := w.renderer.paint("│", mdMuted) + " "
		quote := &markdownIndent{parent: indent, first: bar, rest: bar, width: 2}
		w.blocks(block.children, quote, depth)
		if len(block.children) == 0 {
			w.row(quote, block.start, "")
		}
	case mdList:
		w.list(block, indent, depth)
	}
}

// list numbers items from the first marker, as CommonMark does. Each item
// aligns its continuation lines with its own marker, so a tenth item cannot
// shift the items already printed above it.
func (w *markdownWriter) list(list *mdBlock, indent *markdownIndent, depth int) {
	for index, item := range list.children {
		if index > 0 && item.blank {
			w.row(indent, item.start, "")
		}
		marker := w.renderer.paint(markdownBullets[depth%len(markdownBullets)], mdMuted)
		width := 1
		if list.ordered {
			number := strconv.Itoa(list.number+index) + string(list.delim)
			marker, width = number, len(number)
		}
		switch {
		case item.task != 0 && list.ordered:
			marker += " " + w.taskBox(item.task)
			width += 2
		case item.task != 0:
			marker = w.taskBox(item.task)
		}
		child := &markdownIndent{parent: indent, first: marker + " ", rest: strings.Repeat(" ", width+1), width: width + 1}
		w.blocks(item.children, child, depth+1)
		if len(item.children) == 0 {
			w.row(child, item.start, "")
		}
	}
}

func (w *markdownWriter) taskBox(task byte) string {
	if task == 'x' {
		return w.renderer.paint("☑", mdMuted)
	}
	return w.renderer.paint("☐", mdMuted)
}

func (w *markdownWriter) text(indent *markdownIndent, rows [][]mdRun, base mdStyle) {
	width := w.contentWidth(indent)
	for _, row := range rows {
		row, resumed := w.clip(row)
		if resumed {
			// The start of this row is already printed above.
			indent.next()
			if len(row) == 0 {
				continue
			}
		}
		for _, line := range w.wrap(w.renderer.paintRuns(row, base), width) {
			w.emit(indent, line)
		}
	}
}

// code hard-wraps long lines: word wrapping would move or drop the spaces
// that code depends on.
func (w *markdownWriter) code(indent *markdownIndent, line mdLine) {
	text := strings.TrimRight(line.text, " \t")
	run := mdRun{text: text, src: line.start, end: line.start + len(text), exact: true}
	row, resumed := w.clip(markdownTabRuns(run, true))
	if resumed {
		indent.next()
		if len(row) == 0 {
			return
		}
	}
	var plain strings.Builder
	for _, run := range row {
		plain.WriteString(run.text)
	}
	text = plain.String()
	chunks := []string{text}
	if width := w.contentWidth(indent); width > 0 && !w.options.noWrap {
		chunks = strings.Split(ansi.Hardwrap(text, width, true), "\n")
	}
	for _, chunk := range chunks {
		w.emit(indent, w.renderer.paint(chunk, mdCode))
	}
}

// clip drops runs printed before options.from. resumed reports that the row
// started before from, so its first line continues earlier output.
func (w *markdownWriter) clip(row []mdRun) ([]mdRun, bool) {
	from := w.options.from
	if from == 0 || len(row) == 0 || row[0].src >= from {
		return row, false
	}
	var kept []mdRun
	for _, run := range row {
		switch {
		case run.end <= from:
			continue
		case run.src < from && run.exact:
			run.text, run.src = run.text[from-run.src:], from
		}
		kept = append(kept, run)
	}
	return kept, true
}

// row emits a line that belongs to the source at src as a whole, such as a
// rule, a table row, or a blank line.
func (w *markdownWriter) row(indent *markdownIndent, src int, text string) {
	if src < w.options.from {
		indent.next()
		return
	}
	w.emit(indent, text)
}

func (w *markdownWriter) emit(indent *markdownIndent, text string) {
	line := indent.next() + text
	if text == "" {
		// Keep a quote bar on a blank line, but not the space after it.
		line = strings.TrimRight(line, " ")
	}
	w.lines = append(w.lines, line)
}

func (w *markdownWriter) contentWidth(indent *markdownIndent) int {
	if w.options.width <= 0 {
		return 0
	}
	return max(1, w.options.width-indent.total())
}

func (w *markdownWriter) wrap(text string, width int) []string {
	if width <= 0 || w.options.noWrap {
		return []string{text}
	}
	return wrapDisplayLine(text, width)
}

// markdownTabRuns expands tabs without losing the source positions of the
// text around them. Code blocks use tab stops; prose uses four spaces.
func markdownTabRuns(run mdRun, tabStops bool) []mdRun {
	if !strings.Contains(run.text, "\t") {
		return []mdRun{run}
	}
	if !run.exact {
		run.text = strings.ReplaceAll(run.text, "\t", "    ")
		return []mdRun{run}
	}
	var runs []mdRun
	start, column := 0, 0
	for at := 0; at < len(run.text); at++ {
		if run.text[at] != '\t' {
			continue
		}
		part := run
		part.text, part.src, part.end = run.text[start:at], run.src+start, run.src+at
		if part.text != "" {
			runs = append(runs, part)
		}
		column += ansi.StringWidth(part.text)
		spaces := 4
		if tabStops {
			spaces -= column % 4
		}
		part.text, part.src, part.end, part.exact = strings.Repeat(" ", spaces), run.src+at, run.src+at+1, false
		runs = append(runs, part)
		column += spaces
		start = at + 1
	}
	if start < len(run.text) {
		run.text, run.src = run.text[start:], run.src+start
		runs = append(runs, run)
	}
	return runs
}

func wrapDisplayLine(text string, width int) []string {
	width = max(1, width)
	if !strings.Contains(text, "\n") && visibleLen(text) <= width {
		return []string{text}
	}
	wrapped := lipgloss.Wrap(text, width, "")
	if wrapped == "" {
		return []string{""}
	}
	return strings.Split(wrapped, "\n")
}
