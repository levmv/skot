package ui

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Inline parsing follows the CommonMark delimiter algorithm, so emphasis
// needs flanking delimiters: "snake_case", "2 * 3" and "*.go" stay literal.
// Supported spans are emphasis, strong emphasis, ~~strikethrough~~, code
// spans, inline links and images, autolinks, bare URLs, backslash escapes,
// <br>, and common character references.

type mdStyle uint8

const (
	mdBold mdStyle = 1 << iota
	mdItalic
	mdStrike
	mdCode
	mdLink
	mdAccent
	mdMuted
)

// mdRun is styled inline text and the source range it was rendered from.
// An exact run copies that range verbatim, so it can be split at an offset.
type mdRun struct {
	text     string
	style    mdStyle
	link     string
	src, end int
	exact    bool
}

type mdInlineKind uint8

const (
	inlineText mdInlineKind = iota
	inlineDelimiter
	inlineBracket
	inlineBreak
)

type mdInline struct {
	kind     mdInlineKind
	text     string
	src, end int
	exact    bool
	style    mdStyle
	link     string

	char     byte // delimiter run character
	count    int  // delimiter characters not matched yet
	length   int  // delimiter run length, for the rule of three
	canOpen  bool
	canClose bool
	// leading marks an opener at the start of a word, where models put
	// emphasis, unlike the stars in "2*3" or "*.go".
	leading bool

	active bool // bracket that can still start a link
}

type mdInlineParser struct {
	text       string
	segments   []mdSegment
	nodes      []mdInline
	delimiters []int // indices of delimiter nodes
	brackets   []int // indices of brackets not closed yet
	// open means the text is still streaming: unfinished spans render as if
	// already closed instead of flashing their markers.
	open bool
	// table unescapes \| inside code spans, where GFM tables need it too.
	table bool
	// shown is the source offset before any inline syntax held back while
	// streaming. Only text before this offset can be committed to history.
	shown int
}

// mdSegment maps an offset in the joined text of a block to the source.
type mdSegment struct{ at, src int }

// parseInline parses the lines of one paragraph, heading, or table cell. Each
// source line gets its own row: models break lines deliberately.
func parseInline(lines []mdLine, open, table bool) ([][]mdRun, int) {
	parser := mdInlineParser{open: open, table: table}
	var text strings.Builder
	for index, line := range lines {
		if index > 0 {
			text.WriteByte('\n')
		}
		parser.segments = append(parser.segments, mdSegment{at: text.Len(), src: line.start})
		text.WriteString(strings.TrimRight(line.text, " \t"))
	}
	parser.text = text.String()
	parser.scan()
	parser.processEmphasis(-1)
	if open {
		parser.closeUnfinished()
	}
	return parser.rows(), parser.shown
}

func (p *mdInlineParser) source(at int) int {
	index := sort.Search(len(p.segments), func(index int) bool { return p.segments[index].at > at }) - 1
	if index < 0 {
		return at
	}
	return p.segments[index].src + at - p.segments[index].at
}

func (p *mdInlineParser) scan() {
	text, end := p.text, len(p.text)
	if p.open {
		end = unfinishedInlineEnd(text)
	}
	p.shown = p.source(end)
	plain := 0
	flush := func(at int) {
		if at > plain {
			p.addText(plain, at, text[plain:at])
		}
		plain = at
	}
	for at := 0; at < end; {
		next := at
		switch text[at] {
		case '\\':
			switch {
			case at+1 < end && text[at+1] == '\n':
				flush(at)
				p.nodes = append(p.nodes, mdInline{kind: inlineBreak})
				next = at + 2
			case at+1 < end && isASCIIPunct(text[at+1]):
				flush(at)
				p.addText(at+1, at+2, text[at+1:at+2])
				next = at + 2
			}
		case '\n':
			flush(at)
			p.nodes = append(p.nodes, mdInline{kind: inlineBreak})
			next = at + 1
		case '`':
			flush(at)
			next = p.codeSpan(at, end)
		case '*', '_', '~':
			flush(at)
			next = p.delimiter(at, end)
		case '[':
			flush(at)
			p.addBracket(at, 1)
			next = at + 1
		case '!':
			if at+1 < end && text[at+1] == '[' {
				flush(at)
				p.addBracket(at, 2)
				next = at + 2
			}
		case ']':
			flush(at)
			next = p.closeBracket(at, end)
		case '<':
			next = p.angle(at, end, flush)
		case '&':
			if value, n := characterReference(text[at:end]); n > 0 {
				flush(at)
				p.addText(at, at+n, value)
				next = at + n
			}
		case 'h', 'H', 'w', 'W':
			if n := bareURLEnd(text, at, end, len(p.brackets) > 0); n > at {
				flush(at)
				url := text[at:n]
				target := url
				if !strings.Contains(url, "://") {
					target = "http://" + url
				}
				p.addLink(at, n, url, target)
				next = n
			}
		}
		if next == at {
			at++
			continue
		}
		at, plain = next, next
	}
	flush(end)
}

// unfinishedInlineEnd drops trailing characters whose meaning depends on what
// follows: a delimiter run can still open or close emphasis, "!" can still
// start an image, a backslash can still escape, and "&am" can still become
// "&amp;".
func unfinishedInlineEnd(text string) int {
	end := len(text)
	for end > 0 && strings.IndexByte("*_~", text[end-1]) >= 0 {
		end--
	}
	if end > 0 && text[end-1] == '!' {
		end--
	}
	if end > 0 && text[end-1] == '\\' {
		end--
	}
	name := strings.LastIndexByte(text[:end], '&')
	if name >= 0 && end-name <= 32 && strings.TrimLeft(text[name+1:end], "#0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
		end = name
	}
	return end
}

func (p *mdInlineParser) addText(from, to int, text string) {
	p.nodes = append(p.nodes, mdInline{
		kind: inlineText, text: text, src: p.source(from), end: p.source(to), exact: text == p.text[from:to],
	})
}

func (p *mdInlineParser) addLink(from, to int, text, destination string) {
	p.nodes = append(p.nodes, mdInline{
		kind: inlineText, text: text, style: mdLink, link: hyperlinkTarget(destination),
		src: p.source(from), end: p.source(to), exact: text == p.text[from:to],
	})
}

func (p *mdInlineParser) codeSpan(at, end int) int {
	text := p.text
	length := runLength(text, at, end, '`')
	if close := matchingBackticks(text, at+length, end, length); close >= 0 {
		p.addCode(at+length, close)
		return close + length
	}
	if p.open && !strings.Contains(text[at:end], "\n") {
		p.addCode(at+length, end)
		return end
	}
	p.addText(at, at+length, text[at:at+length])
	return at + length
}

func (p *mdInlineParser) addCode(from, to int) {
	code := p.text[from:to]
	if len(code) >= 2 && strings.ContainsRune(" \n", rune(code[0])) && strings.ContainsRune(" \n", rune(code[len(code)-1])) && strings.Trim(code, " \n") != "" {
		from, to = from+1, to-1
	}
	add := func(from, to int, text string) {
		if text != "" {
			p.addText(from, to, text)
			p.nodes[len(p.nodes)-1].style = mdCode
		}
	}
	// Keep each replacement separate from literal code so a continuation
	// can resume on either side without replaying the whole span.
	start := from
	for at := from; at < to; at++ {
		next, value := at, ""
		switch {
		case p.text[at] == '\n':
			next, value = at+1, " "
		case p.table && p.text[at] == '\\' && at+1 < to && p.text[at+1] == '|':
			next, value = at+2, "|"
		}
		if next > at {
			add(start, at, p.text[start:at])
			add(at, next, value)
			start, at = next, next-1
		}
	}
	add(start, to, p.text[start:to])
}

func (p *mdInlineParser) delimiter(at, end int) int {
	text := p.text
	char := text[at]
	next := at + runLength(text, at, end, char)
	length := next - at
	// A single tilde is literal: models write "~5 min" far more often than
	// they mean strikethrough.
	if char == '~' && length != 2 {
		p.addText(at, next, text[at:next])
		return next
	}
	before, after := ' ', ' '
	if at > 0 {
		before, _ = utf8.DecodeLastRuneInString(text[:at])
	}
	if next < len(text) {
		after, _ = utf8.DecodeRuneInString(text[next:])
	}
	left := !unicode.IsSpace(after) && (!isPunctuation(after) || unicode.IsSpace(before) || isPunctuation(before))
	right := !unicode.IsSpace(before) && (!isPunctuation(before) || unicode.IsSpace(after) || isPunctuation(after))
	canOpen, canClose := left, right
	if char == '_' {
		canOpen = left && (!right || isPunctuation(before))
		canClose = right && (!left || isPunctuation(after))
	}
	leading := left && !right && (!isPunctuation(after) || strings.ContainsRune("*_~`[(\"'«“‘", after))
	p.delimiters = append(p.delimiters, len(p.nodes))
	p.nodes = append(p.nodes, mdInline{
		kind: inlineDelimiter, text: text[at:next], src: p.source(at), end: p.source(next), exact: true,
		char: char, count: length, length: length, canOpen: canOpen, canClose: canClose, leading: leading,
	})
	return next
}

// processEmphasis pairs delimiter runs after node bottom.
func (p *mdInlineParser) processEmphasis(bottom int) {
	first := sort.SearchInts(p.delimiters, bottom+1)
	for position := first; position < len(p.delimiters); position++ {
		c := p.delimiters[position]
		closer := &p.nodes[c]
		for closer.canClose && closer.count > 0 {
			o := p.opener(first, position)
			if o < 0 {
				break
			}
			opener := &p.nodes[o]
			n, style := 1, mdItalic
			switch {
			case closer.char == '~':
				n, style = 2, mdStrike
			case opener.count >= 2 && closer.count >= 2:
				n, style = 2, mdBold
			}
			opener.count -= n
			closer.count -= n
			for k := o + 1; k < c; k++ {
				p.nodes[k].style |= style
				// Delimiters inside a pair can no longer match outside it.
				p.nodes[k].canOpen, p.nodes[k].canClose = false, false
			}
		}
	}
}

// opener finds the opener for the closer at delimiters[position], searching
// no earlier than delimiters[first].
func (p *mdInlineParser) opener(first, position int) int {
	closer := p.nodes[p.delimiters[position]]
	for candidate := position - 1; candidate >= first; candidate-- {
		o := p.delimiters[candidate]
		opener := p.nodes[o]
		if opener.char != closer.char || !opener.canOpen || opener.count == 0 {
			continue
		}
		// The rule of three keeps "*foo**bar*" from pairing the wrong runs.
		if (opener.canClose || closer.canOpen) && (opener.length+closer.length)%3 == 0 && (opener.length%3 != 0 || closer.length%3 != 0) {
			continue
		}
		return o
	}
	return -1
}

func (p *mdInlineParser) addBracket(at, n int) {
	p.brackets = append(p.brackets, len(p.nodes))
	p.nodes = append(p.nodes, mdInline{
		kind: inlineBracket, text: p.text[at : at+n], src: p.source(at), end: p.source(at + n), exact: true, active: true,
	})
}

type linkState uint8

const (
	linkNone linkState = iota
	linkComplete
	// linkPending is a streamed link whose destination has not arrived.
	linkPending
)

func (p *mdInlineParser) closeBracket(at, end int) int {
	if len(p.brackets) == 0 {
		p.addText(at, at+1, "]")
		return at + 1
	}
	opener := p.brackets[len(p.brackets)-1]
	if p.open && at+1 == len(p.text) && p.nodes[opener].active {
		// Only the next character tells link text from "[WIP]" or "arr[0]".
		// The opener stays unclosed, so closeUnfinished holds it back.
		p.addText(at, at+1, "]")
		return at + 1
	}
	p.brackets = p.brackets[:len(p.brackets)-1]
	destination, next, state := "", at+1, linkNone
	if p.nodes[opener].active {
		destination, next, state = p.linkDestination(at+1, end)
	}
	if state == linkNone {
		p.nodes[opener].kind = inlineText
		p.addText(at, at+1, "]")
		return at + 1
	}
	label := p.link(opener, destination)
	if state == linkPending {
		p.shown = min(p.shown, p.source(at))
	}
	if state == linkComplete && destination != "" && !sameLinkText(label, destination) {
		// Terminals without hyperlink support still show where a link goes.
		p.nodes = append(p.nodes, mdInline{
			kind: inlineText, text: " (" + destination + ")", style: mdMuted, link: hyperlinkTarget(destination),
			src: p.source(next - 1), end: p.source(next),
		})
	}
	return next
}

// link styles the nodes after opener as link text and returns that text.
func (p *mdInlineParser) link(opener int, destination string) string {
	image := p.nodes[opener].text == "!["
	p.nodes[opener].kind, p.nodes[opener].text = inlineText, ""
	p.processEmphasis(opener)
	target := hyperlinkTarget(destination)
	var label strings.Builder
	for index := opener + 1; index < len(p.nodes); index++ {
		node := &p.nodes[index]
		node.style |= mdLink
		node.canOpen, node.canClose = false, false
		if target != "" {
			node.link = target
		}
		switch node.kind {
		case inlineText:
			label.WriteString(node.text)
		case inlineDelimiter:
			label.WriteString(strings.Repeat(string(node.char), node.count))
		}
	}
	if !image {
		// Links cannot contain other links.
		for _, index := range p.brackets {
			if p.nodes[index].text == "[" {
				p.nodes[index].active = false
			}
		}
	}
	return label.String()
}

// linkDestination parses an inline link destination such as (url "title")
// at offset at, just after the closing bracket.
func (p *mdInlineParser) linkDestination(at, end int) (string, int, linkState) {
	text := p.text
	unfinished := func() (string, int, linkState) {
		if p.open {
			return "", end, linkPending
		}
		return "", at, linkNone
	}
	// Streamed text held back right after "]" is never the "(" of a link.
	if at >= end || text[at] != '(' {
		return "", at, linkNone
	}
	index := skipLinkSpace(text, at+1, end)
	var destination string
	if index < end && text[index] == '<' {
		close := strings.IndexAny(text[index+1:end], ">\n")
		if close < 0 {
			return unfinished()
		}
		if text[index+1+close] != '>' {
			return "", at, linkNone
		}
		destination = text[index+1 : index+1+close]
		index += close + 2
	} else {
		start, depth := index, 0
	scan:
		for ; index < end; index++ {
			switch char := text[index]; {
			case char == '\\' && index+1 < end && isASCIIPunct(text[index+1]):
				index++
			case char == '(':
				depth++
			case char == ')':
				if depth == 0 {
					break scan
				}
				depth--
			case char == ' ' || char == '\t' || char == '\n':
				break scan
			}
		}
		destination = text[start:index]
	}
	index = skipLinkSpace(text, index, end)
	if index < end && (text[index] == '"' || text[index] == '\'' || text[index] == '(') {
		closer := text[index]
		if closer == '(' {
			closer = ')'
		}
		close := strings.IndexByte(text[index+1:end], closer)
		if close < 0 {
			return unfinished()
		}
		index = skipLinkSpace(text, index+close+2, end)
	}
	if index >= end {
		return unfinished()
	}
	if text[index] != ')' {
		return "", at, linkNone
	}
	return unescapeMarkdown(destination), index + 1, linkComplete
}

// angle handles text starting with '<': an autolink or a <br> line break.
// Other HTML stays literal.
func (p *mdInlineParser) angle(at, end int, flush func(int)) int {
	close := strings.IndexAny(p.text[at+1:end], "<>\n")
	if close < 0 && p.open {
		// Hold back an autolink or <br> still being written; "a < b" with
		// a space after the bracket stays literal right away.
		if rest := p.text[at:end]; !strings.ContainsAny(rest, " \t") || strings.HasPrefix(strings.ToLower(rest), "<br") {
			flush(at)
			p.shown = min(p.shown, p.source(at))
			return end
		}
	}
	if close < 0 || p.text[at+1+close] != '>' {
		return at
	}
	close += at + 1
	inner := p.text[at+1 : close]
	switch {
	case isLineBreakTag(inner):
		// Spaces around <br> would leave blanks at the edges of rows.
		flush(len(strings.TrimRight(p.text[:at], " \t")))
		p.nodes = append(p.nodes, mdInline{kind: inlineBreak})
		return end - len(strings.TrimLeft(p.text[close+1:end], " \t"))
	case strings.ContainsAny(inner, " \t"):
		return at
	case isAutolinkURL(inner):
		flush(at)
		p.addLink(at+1, close, inner, inner)
	case isEmailAddress(inner):
		flush(at)
		p.addLink(at+1, close, inner, "mailto:"+inner)
	default:
		return at
	}
	return close + 1
}

// closeUnfinished prepares the line being streamed. A bracket still open
// there may turn out to be "[WIP]" or "arr[0]" rather than link text, so the
// line is held back from it. Spans open before it render as if closed, so
// their markers never flash before the closer arrives. Only openers that
// start a word qualify: a literal "*.go" must not turn italic for a moment.
// Openers on finished lines stay literal; models do not carry emphasis
// across lines.
func (p *mdInlineParser) closeUnfinished() {
	start := 0
	for index := len(p.nodes) - 1; index >= 0; index-- {
		if p.nodes[index].kind == inlineBreak {
			start = index + 1
			break
		}
	}
	for index := start; index < len(p.nodes); index++ {
		if node := p.nodes[index]; node.kind == inlineBracket && node.active {
			p.shown = min(p.shown, node.src)
			p.nodes = p.nodes[:index]
			break
		}
	}
	var open mdStyle
	for index := start; index < len(p.nodes); index++ {
		node := &p.nodes[index]
		node.style |= open
		if node.kind == inlineDelimiter && node.canOpen && node.leading && node.count > 0 {
			switch {
			case node.char == '~':
				open |= mdStrike
			case node.count >= 3:
				open |= mdBold | mdItalic
			case node.count == 2:
				open |= mdBold
			default:
				open |= mdItalic
			}
			node.count = 0
		}
	}
}

func (p *mdInlineParser) rows() [][]mdRun {
	rows := [][]mdRun{nil}
	var run mdRun
	var text strings.Builder
	finish := func() {
		if text.Len() > 0 {
			run.text = text.String()
			rows[len(rows)-1] = append(rows[len(rows)-1], run)
			text.Reset()
		}
	}
	for _, node := range p.nodes {
		value := node.text
		switch node.kind {
		case inlineBreak:
			finish()
			rows = append(rows, nil)
			continue
		case inlineDelimiter:
			value = strings.Repeat(string(node.char), node.count)
		}
		if value == "" {
			continue
		}
		next := mdRun{text: value, style: node.style, link: node.link, src: node.src, end: node.end, exact: node.exact && value == node.text}
		for _, part := range markdownTabRuns(next, false) {
			// A decoded entity or expanded tab is indivisible. Merging it
			// with literal text would lose that text's source positions.
			if text.Len() > 0 && run.exact && part.exact && run.style == part.style && run.link == part.link && run.end == part.src {
				text.WriteString(part.text)
				run.end = part.end
				continue
			}
			finish()
			run = part
			text.WriteString(part.text)
		}
	}
	finish()
	return rows
}

// bareURLEnd returns the end of a GFM extended autolink starting at at, or at
// itself. Trailing punctuation usually ends the sentence, not the URL. Inside
// brackets, "]" ends it too, so that link text can still close.
func bareURLEnd(text string, at, end int, bracketed bool) int {
	if at > 0 {
		if previous, _ := utf8.DecodeLastRuneInString(text[:at]); !unicode.IsSpace(previous) && !strings.ContainsRune("*_~(\"'", previous) {
			return at
		}
	}
	rest := text[at:end]
	prefix := 0
	for _, scheme := range []string{"https://", "http://", "www."} {
		if len(rest) > len(scheme) && strings.EqualFold(rest[:len(scheme)], scheme) {
			prefix = len(scheme)
			break
		}
	}
	if prefix == 0 {
		return at
	}
	stops := "< \t\n"
	if bracketed {
		stops += "]"
	}
	n := prefix
	for n < len(rest) && strings.IndexByte(stops, rest[n]) < 0 {
		n++
	}
	for n > prefix {
		switch char := rest[n-1]; {
		case strings.IndexByte("?!.,:*_~'\";", char) >= 0:
			n--
			continue
		case char == ')' && strings.Count(rest[:n], "(") < strings.Count(rest[:n], ")"):
			n--
			continue
		}
		break
	}
	if n == prefix {
		return at
	}
	return at + n
}

// matchingBackticks finds the next run of exactly length backticks, which
// closes a code span.
func matchingBackticks(text string, at, end, length int) int {
	for at < end {
		if text[at] != '`' {
			at++
			continue
		}
		n := runLength(text, at, end, '`')
		if n == length {
			return at
		}
		at += n
	}
	return -1
}

func isLineBreakTag(inner string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(inner), "/")), "br")
}

func isAutolinkURL(text string) bool {
	colon := strings.IndexByte(text, ':')
	if colon < 2 || colon > 32 || !isASCIILetter(text[0]) {
		return false
	}
	for index := 1; index < colon; index++ {
		if char := text[index]; !isASCIILetter(char) && !isASCIIDigit(char) && char != '+' && char != '.' && char != '-' {
			return false
		}
	}
	return colon+1 < len(text)
}

func isEmailAddress(text string) bool {
	at := strings.IndexByte(text, '@')
	return at > 0 && strings.Contains(text[at+1:], ".") && !strings.ContainsAny(text, " \t\\") && !strings.HasSuffix(text, ".")
}

// hyperlinkTarget returns destination when terminals can open it as a link.
func hyperlinkTarget(destination string) string {
	if scheme, _, ok := strings.Cut(destination, ":"); ok {
		switch strings.ToLower(scheme) {
		case "http", "https", "mailto", "file", "ftp":
			return destination
		}
	}
	return ""
}

// sameLinkText reports whether link text already shows its destination.
func sameLinkText(label, destination string) bool {
	normalize := func(text string) string {
		for _, prefix := range []string{"mailto:", "https://", "http://"} {
			text = strings.TrimPrefix(text, prefix)
		}
		return strings.TrimSuffix(text, "/")
	}
	return normalize(label) == normalize(destination)
}

func skipLinkSpace(text string, at, end int) int {
	for at < end && (text[at] == ' ' || text[at] == '\t' || text[at] == '\n') {
		at++
	}
	return at
}

func unescapeMarkdown(text string) string {
	if !strings.Contains(text, `\`) {
		return text
	}
	var out strings.Builder
	for index := 0; index < len(text); index++ {
		if text[index] == '\\' && index+1 < len(text) && isASCIIPunct(text[index+1]) {
			index++
		}
		out.WriteByte(text[index])
	}
	return out.String()
}

var characterReferences = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": "\u00a0",
	"copy": "©", "reg": "®", "trade": "™", "hellip": "…", "mdash": "—", "ndash": "–",
	"laquo": "«", "raquo": "»", "larr": "←", "rarr": "→", "uarr": "↑", "darr": "↓", "harr": "↔",
	"times": "×", "divide": "÷", "plusmn": "±", "le": "≤", "ge": "≥", "ne": "≠", "deg": "°",
	"middot": "·", "bull": "•",
}

// characterReference decodes a reference such as "&amp;" or "&#x2192;" at the
// start of text. Control characters stay literal: they never reach the
// terminal from model text.
func characterReference(text string) (string, int) {
	end := strings.IndexByte(text, ';')
	if end < 2 || end > 32 {
		return "", 0
	}
	name := text[1:end]
	if name[0] != '#' {
		value, ok := characterReferences[name]
		if !ok {
			return "", 0
		}
		return value, end + 1
	}
	digits, base := name[1:], 10
	if digits != "" && (digits[0] == 'x' || digits[0] == 'X') {
		digits, base = digits[1:], 16
	}
	value, err := strconv.ParseUint(digits, base, 32)
	if err != nil || value < 0x20 || value >= 0x7f && value <= 0x9f || !utf8.ValidRune(rune(value)) {
		return "", 0
	}
	return string(rune(value)), end + 1
}

func isASCIIPunct(char byte) bool {
	return char >= '!' && char <= '/' || char >= ':' && char <= '@' || char >= '[' && char <= '`' || char >= '{' && char <= '~'
}

func isASCIILetter(char byte) bool { return char|0x20 >= 'a' && char|0x20 <= 'z' }

func isASCIIDigit(char byte) bool { return char >= '0' && char <= '9' }

func isPunctuation(char rune) bool { return unicode.IsPunct(char) || unicode.IsSymbol(char) }
