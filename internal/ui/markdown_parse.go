package ui

import (
	"slices"
	"strconv"
	"strings"
)

// The block parser follows the CommonMark container model for the constructs
// models actually write: paragraphs, ATX headings, fenced code, thematic
// breaks, block quotes, lists, and GFM tables. Indented code blocks, setext
// headings, HTML blocks, and link reference definitions are not recognized;
// their text renders as ordinary paragraphs. A "---" line is always a rule:
// models mean a separator far more often than a setext heading.

type mdKind uint8

const (
	mdRoot mdKind = iota
	mdParagraph
	mdHeading
	mdCodeBlock
	mdRule
	mdTable
	mdQuote
	mdList
	mdItem
)

// mdLine is block content after container markers. Offsets index the source,
// so rendering can resume exactly after output that is already printed.
type mdLine struct {
	text  string
	start int // source offset of text
	line  int // source offset of the line holding text
}

type mdBlock struct {
	kind     mdKind
	parent   *mdBlock
	children []*mdBlock
	lines    []mdLine
	start    int  // source offset of the line that opened the block
	blank    bool // a blank line separates the block from its previous sibling
	// open marks the paragraph or heading whose last line is still streaming.
	open bool

	level int // heading

	fence       byte // fenced code
	fenceLen    int
	fenceIndent int

	ordered bool // list
	delim   byte // list: bullet character, or the '.' or ')' after a number
	number  int  // list: number of the first item

	indent int  // item: content column within the parent container
	task   byte // item: 0, or ' ' / 'x' for a task box

	align []mdAlign // table
}

// mdDocument is parsed Markdown. While text streams, shown can stop short of
// the source: a trailing line whose meaning depends on text that has not
// arrived yet is withheld instead of being drawn as raw syntax first.
type mdDocument struct {
	blocks []*mdBlock
	shown  int
}

func parseMarkdown(source string, streaming bool) mdDocument {
	if !streaming {
		return mdDocument{blocks: parseMarkdownBlocks(source).root.children, shown: len(source)}
	}
	cut := len(source)
	tail := strings.LastIndexByte(source, '\n') + 1
	if tail < len(source) && pendingBlockMarker(source[tail:]) {
		cut = tail
	}
	parser := parseMarkdownBlocks(source[:cut])
	if start, ok := parser.pendingTable(tail, cut > tail); ok {
		cut = start
		parser = parseMarkdownBlocks(source[:cut])
	}
	if last := parser.last; cut > tail && last != nil {
		switch last.kind {
		case mdParagraph, mdHeading:
			last.open = true
		case mdCodeBlock:
			if len(last.lines) > 0 {
				if line := last.lines[len(last.lines)-1]; line.line == tail {
					cut = line.start + len(strings.TrimRight(line.text, " \t"))
				}
			}
		}
	}
	return mdDocument{blocks: parser.root.children, shown: cut}
}

type mdParser struct {
	root *mdBlock
	tip  *mdBlock // innermost open container
	leaf *mdBlock // open paragraph, fenced code, or table
	// last is the leaf that received the most recent non-blank line.
	last  *mdBlock
	blank bool
}

func parseMarkdownBlocks(source string) *mdParser {
	root := &mdBlock{kind: mdRoot}
	parser := &mdParser{root: root, tip: root}
	for start := 0; start < len(source); {
		end := strings.IndexByte(source[start:], '\n')
		if end < 0 {
			end = len(source)
		} else {
			end += start
		}
		parser.addLine(source[start:end], start)
		start = end + 1
	}
	return parser
}

func (p *mdParser) addLine(line string, lineStart int) {
	rest, at := line, lineStart
	matched := p.root
	for _, container := range p.openContainers() {
		n, ok := p.continues(container, rest)
		if !ok {
			break
		}
		rest, at = rest[n:], at+n
		matched = container
	}
	if matched == p.tip && p.leaf != nil && p.leaf.kind == mdCodeBlock {
		p.codeLine(rest, at, lineStart)
		return
	}
	if matched != p.tip {
		// A paragraph absorbs a lazy continuation line that omits the
		// markers of its containers.
		if p.leaf != nil && p.leaf.kind == mdParagraph && !isBlankLine(rest) && !startsMarkdownBlock(rest) {
			p.paragraphLine(rest, at, lineStart)
			return
		}
		p.tip, p.leaf = matched, nil
	}
	p.openBlocks(rest, at, lineStart)
}

func (p *mdParser) openContainers() []*mdBlock {
	var chain []*mdBlock
	for block := p.tip; block != p.root; block = block.parent {
		chain = append(chain, block)
	}
	slices.Reverse(chain)
	return chain
}

// continues reports whether line continues an open container and how many
// bytes of container markers it starts with.
func (p *mdParser) continues(container *mdBlock, line string) (int, bool) {
	switch container.kind {
	case mdQuote:
		return quoteMarker(line)
	case mdItem:
		width, n := leadingIndent(line)
		switch {
		case n == len(line):
			return 0, true
		case width >= container.indent:
			return indentBytes(line, container.indent), true
		case p.leaf != nil && p.leaf.kind == mdCodeBlock && containsBlock(container, p.leaf):
			// Models often dedent lines inside a fence nested in a list
			// item. Keep them in the fence rather than ending the item.
			return n, true
		}
		return 0, false
	}
	// A list continues through its items.
	return 0, true
}

func (p *mdParser) codeLine(rest string, at, lineStart int) {
	code := p.leaf
	p.last, p.blank = code, false
	if closesFence(rest, code) {
		p.leaf = nil
		return
	}
	n := indentBytes(rest, code.fenceIndent)
	code.lines = append(code.lines, mdLine{text: rest[n:], start: at + n, line: lineStart})
}

func (p *mdParser) openBlocks(rest string, at, lineStart int) {
	for {
		width, n := leadingIndent(rest)
		if n == len(rest) {
			p.leaf = nil
			p.blank = true
			return
		}
		body, bodyAt := rest[n:], at+n
		if fence, length, ok := fenceOpening(body); ok {
			code := p.add(mdCodeBlock, lineStart)
			code.fence, code.fenceLen, code.fenceIndent = fence, length, width
			p.leaf, p.last = code, code
			return
		}
		// Indented code blocks are not supported, so deep indentation only
		// matters where it continues a paragraph.
		if width < 4 || p.leaf == nil || p.leaf.kind != mdParagraph {
			if body[0] == '>' {
				k := 1
				if k < len(body) && (body[k] == ' ' || body[k] == '\t') {
					k++
				}
				p.tip = p.add(mdQuote, lineStart)
				rest, at = body[k:], bodyAt+k
				continue
			}
			if level, text, offset, ok := atxHeading(body); ok {
				heading := p.add(mdHeading, lineStart)
				heading.level = level
				heading.lines = []mdLine{{text: text, start: bodyAt + offset, line: lineStart}}
				p.last = heading
				return
			}
			if isThematicBreak(body) {
				p.last = p.add(mdRule, lineStart)
				return
			}
			if item, content, ok := p.listItem(body, width, lineStart); ok {
				rest, at = body[content:], bodyAt+content
				if isBlankLine(rest) {
					p.last = item
					return
				}
				continue
			}
		}
		if p.leaf != nil && p.leaf.kind == mdTable && strings.Contains(body, "|") {
			p.leaf.lines = append(p.leaf.lines, mdLine{text: body, start: bodyAt, line: lineStart})
			p.last = p.leaf
			return
		}
		if p.leaf != nil && p.leaf.kind == mdParagraph && p.startTable(body, bodyAt, lineStart) {
			return
		}
		p.paragraphLine(body, bodyAt, lineStart)
		return
	}
}

// add appends a block to the innermost open container. It closes the open
// leaf, and a list when the new block is not one of its items.
func (p *mdParser) add(kind mdKind, lineStart int) *mdBlock {
	for p.tip.kind == mdList && kind != mdItem {
		p.tip = p.tip.parent
	}
	block := &mdBlock{kind: kind, parent: p.tip, start: lineStart, blank: p.blank && len(p.tip.children) > 0}
	p.tip.children = append(p.tip.children, block)
	p.leaf, p.blank = nil, false
	return block
}

func (p *mdParser) paragraphLine(rest string, at, lineStart int) {
	_, n := leadingIndent(rest)
	if p.leaf == nil || p.leaf.kind != mdParagraph {
		p.leaf = p.add(mdParagraph, lineStart)
	}
	p.leaf.lines = append(p.leaf.lines, mdLine{text: rest[n:], start: at + n, line: lineStart})
	p.last = p.leaf
}

// listItem opens an item, and a list unless the item continues the open one.
// It returns the offset of the item content within body.
func (p *mdParser) listItem(body string, width, lineStart int) (*mdBlock, int, bool) {
	ordered, delim, number, marker, ok := listMarker(body)
	if !ok {
		return nil, 0, false
	}
	spaces, n := leadingIndent(body[marker:])
	indent := width + marker + spaces
	switch {
	case marker+n == len(body):
		indent = width + marker + 1
	case spaces > 4:
		indent, n = width+marker+1, 1
	}
	if list := p.tip; list.kind != mdList || list.ordered != ordered || list.delim != delim {
		list = p.add(mdList, lineStart)
		list.ordered, list.delim, list.number = ordered, delim, number
		p.tip = list
	}
	item := p.add(mdItem, lineStart)
	item.indent = indent
	p.tip = item
	content := marker + n
	if task, k := taskBox(body[content:]); task != 0 {
		item.task = task
		content += k
	}
	return item, content, true
}

// startTable turns the last paragraph line into a table header when body is
// a matching delimiter row.
func (p *mdParser) startTable(body string, at, lineStart int) bool {
	paragraph := p.leaf
	header := paragraph.lines[len(paragraph.lines)-1]
	align, ok := tableDelimiterRow(body)
	if !ok || !strings.Contains(header.text, "|") || len(splitTableRow(header)) != len(align) {
		return false
	}
	paragraph.lines = paragraph.lines[:len(paragraph.lines)-1]
	table := &mdBlock{
		kind: mdTable, parent: p.tip, start: header.line, align: align,
		lines: []mdLine{header, {text: body, start: at, line: lineStart}},
	}
	if len(paragraph.lines) == 0 {
		table.blank = paragraph.blank
		p.tip.children[len(p.tip.children)-1] = table
	} else {
		p.tip.children = append(p.tip.children, table)
	}
	p.leaf, p.last = table, table
	return true
}

// pendingTable finds trailing lines that streaming must hold back: a table
// row still being written, or pipe lines that become a table header once the
// delimiter row arrives. partial means the line at tail has no newline yet.
func (p *mdParser) pendingTable(tail int, partial bool) (int, bool) {
	switch last := p.last; {
	case last == nil:
	case last.kind == mdTable:
		if row := last.lines[len(last.lines)-1]; partial && len(last.lines) > 2 && row.line == tail {
			return tail, true
		}
	case last.kind == mdParagraph && last == p.leaf:
		cut := -1
		for index := len(last.lines) - 1; index >= 0 && strings.HasPrefix(last.lines[index].text, "|"); index-- {
			cut = last.lines[index].line
		}
		if cut >= 0 {
			return cut, true
		}
	}
	return 0, false
}

// pendingBlockMarker reports whether an unfinished line holds only block
// syntax so far, such as "##", "-", "12." or "> - [", whose meaning depends
// on the text still to come.
func pendingBlockMarker(line string) bool {
	text := strings.TrimRight(strings.TrimLeft(line, " \t>"), " \t")
	switch {
	case text == "", strings.Trim(text, "-*+_ \t") == "", strings.Trim(text, "`") == "", strings.Trim(text, "~") == "":
		return true
	case strings.Trim(text, "#") == "":
		return len(text) <= 6
	}
	if _, _, _, marker, ok := listMarker(text); ok {
		rest := strings.TrimLeft(text[marker:], " \t")
		switch rest {
		case "[", "[ ]", "[x", "[X", "[x]", "[X]":
			return true
		}
		return pendingBlockMarker(rest)
	}
	return len(text) <= 9 && strings.Trim(text, "0123456789") == ""
}

func startsMarkdownBlock(line string) bool {
	width, n := leadingIndent(line)
	if width >= 4 || n == len(line) {
		return false
	}
	body := line[n:]
	_, _, fence := fenceOpening(body)
	_, _, _, heading := atxHeading(body)
	_, _, _, _, item := listMarker(body)
	return fence || heading || item || body[0] == '>' || isThematicBreak(body)
}

func fenceOpening(body string) (byte, int, bool) {
	marker := body[0]
	if marker != '`' && marker != '~' {
		return 0, 0, false
	}
	length := runLength(body, 0, len(body), marker)
	if length < 3 || marker == '`' && strings.IndexByte(body[length:], '`') >= 0 {
		return 0, 0, false
	}
	return marker, length, true
}

func closesFence(line string, code *mdBlock) bool {
	body := strings.TrimLeft(line, " \t")
	length := runLength(body, 0, len(body), code.fence)
	return length >= code.fenceLen && strings.TrimSpace(body[length:]) == ""
}

func atxHeading(body string) (level int, text string, offset int, ok bool) {
	for level < len(body) && body[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level < len(body) && body[level] != ' ' && body[level] != '\t' {
		return 0, "", 0, false
	}
	_, n := leadingIndent(body[level:])
	offset = level + n
	text = strings.TrimRight(body[offset:], " \t")
	// A closing run of '#' is not content.
	if trimmed := strings.TrimRight(text, "#"); trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t") {
		text = strings.TrimRight(trimmed, " \t")
	}
	return level, text, offset, true
}

func isThematicBreak(body string) bool {
	marker := body[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	count := 0
	for index := 0; index < len(body); index++ {
		switch body[index] {
		case marker:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

// listMarker parses a bullet ("-", "*", "+") or an ordered marker ("1.",
// "1)") followed by whitespace or the end of the line.
func listMarker(body string) (ordered bool, delim byte, number, length int, ok bool) {
	switch body[0] {
	case '-', '*', '+':
		delim, length = body[0], 1
	default:
		digits := len(body) - len(strings.TrimLeft(body, "0123456789"))
		if digits == 0 || digits > 9 || digits == len(body) || body[digits] != '.' && body[digits] != ')' {
			return false, 0, 0, 0, false
		}
		number, _ = strconv.Atoi(body[:digits])
		ordered, delim, length = true, body[digits], digits+1
	}
	if length < len(body) && body[length] != ' ' && body[length] != '\t' {
		return false, 0, 0, 0, false
	}
	return ordered, delim, number, length, true
}

func taskBox(content string) (byte, int) {
	if len(content) < 3 || content[0] != '[' || content[2] != ']' || len(content) > 3 && content[3] != ' ' && content[3] != '\t' {
		return 0, 0
	}
	switch content[1] {
	case ' ':
		return ' ', min(len(content), 4)
	case 'x', 'X':
		return 'x', min(len(content), 4)
	}
	return 0, 0
}

func quoteMarker(line string) (int, bool) {
	width, n := leadingIndent(line)
	if width > 3 || n == len(line) || line[n] != '>' {
		return 0, false
	}
	n++
	if n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return n, true
}

func containsBlock(container, block *mdBlock) bool {
	for ; block != nil; block = block.parent {
		if block == container {
			return true
		}
	}
	return false
}

// leadingIndent measures leading spaces and tabs, with tab stops every four
// columns.
func leadingIndent(text string) (width, n int) {
	for ; n < len(text); n++ {
		switch text[n] {
		case ' ':
			width++
		case '\t':
			width += 4 - width%4
		default:
			return width, n
		}
	}
	return width, n
}

// indentBytes returns how many bytes of leading whitespace fill up to
// columns.
func indentBytes(text string, columns int) int {
	width, n := 0, 0
	for n < len(text) && width < columns {
		switch text[n] {
		case ' ':
			width++
		case '\t':
			width += 4 - width%4
		default:
			return n
		}
		n++
	}
	return n
}

func isBlankLine(text string) bool {
	_, n := leadingIndent(text)
	return n == len(text)
}

func runLength(text string, at, end int, char byte) int {
	n := at
	for n < end && text[n] == char {
		n++
	}
	return n - at
}
