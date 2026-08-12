package ui

import (
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The normal terminal buffer is an append-only medium once rows enter
// scrollback: cursor addressing can only edit rows still on screen. Rendering
// therefore follows three rules:
//   - transcript contains source-backed history and changes only at a known
//     dirty suffix;
//   - dynamic contains the editor, picker, spinner, and footer and stays small
//     enough to rewrite in the visible terminal;
//   - changes above the visible viewport require an authoritative rebuild,
//     while routine mutable state must be frozen or appended before that point.
//
// Keeping these rules here is important. Treating transcript and dynamic as
// independent terminal renderers causes stale rows, duplicated output, and
// incorrect cursor coordinates after scrolling or resizing.
type inlineFrame struct {
	transcript          []string
	dynamic             []string
	cursor              *tea.Cursor
	transcriptChanged   bool
	transcriptDirtyFrom int
}

// rendererFrame is a transient view over normalized terminal lines. A changed
// transcript reuses the previous prefix and owns only its newly rendered tail.
type rendererFrame struct {
	transcriptPrefix []string
	transcriptSuffix []string
	dynamic          []string
}

func (f rendererFrame) transcriptLen() int {
	return len(f.transcriptPrefix) + len(f.transcriptSuffix)
}

func (f rendererFrame) len() int {
	return f.transcriptLen() + len(f.dynamic)
}

func (f rendererFrame) line(index int) string {
	if index < len(f.transcriptPrefix) {
		return f.transcriptPrefix[index]
	}
	index -= len(f.transcriptPrefix)
	if index < len(f.transcriptSuffix) {
		return f.transcriptSuffix[index]
	}
	return f.dynamic[index-len(f.transcriptSuffix)]
}

// inlineRenderer owns every row Skot draws. Keeping one source-backed buffer is
// essential: terminal scrollback can be native, or it can be reflowed after a
// resize, but two independent renderers cannot safely own adjacent rows.
type inlineRenderer struct {
	out io.Writer

	previousTranscript []string
	previousDynamic    []string
	previousWidth      int
	previousHeight     int
	// These are logical frame rows, not physical screen coordinates. Subtract
	// previousViewportTop to address a row in the current terminal viewport.
	previousViewportTop int
	hardwareCursorRow   int
	started             bool
	stopped             bool
	invalidated         bool
	appendPending       bool
	appendAfterLines    int
}

func newInlineRenderer(out io.Writer) *inlineRenderer {
	if out == nil {
		out = io.Discard
	}
	return &inlineRenderer{out: out}
}

// Render keeps the renderer independently testable with an ordinary tea.View.
// The application uses RenderFrame so it can preserve the transcript prefix.
func (r *inlineRenderer) Render(view tea.View, width, height int) error {
	return r.RenderFrame(inlineFrame{
		dynamic: strings.Split(view.Content, "\n"),
		cursor:  view.Cursor,
	}, width, height)
}

func (r *inlineRenderer) RenderFrame(frame inlineFrame, width, height int) error {
	if r == nil || r.stopped || width <= 0 || height <= 0 {
		return nil
	}
	if !r.started {
		keyboardFlags := ansi.KittyDisambiguateEscapeCodes | ansi.KittyReportAlternateKeys
		if err := r.write(ansi.SetModeBracketedPaste + ansi.SetModifyOtherKeys2 + ansi.KittyKeyboard(keyboardFlags, 1) + ansi.ResetModeTextCursorEnable); err != nil {
			return err
		}
		r.started = true
	}

	widthChanged := r.previousWidth != 0 && r.previousWidth != width
	heightChanged := r.previousHeight != 0 && r.previousHeight != height
	next, firstPossibleChange := r.prepareFrame(frame, width, widthChanged)
	previous := r.previousFrame()
	if r.invalidated {
		r.invalidated = false
		return r.fullRender(next, frame.cursor, width, height, previous.len() > 0)
	}
	if r.appendPending {
		r.appendPending = false
		return r.appendFrame(next, frame.cursor, width, height, r.appendAfterLines)
	}
	if previous.len() == 0 {
		return r.fullRender(next, frame.cursor, width, height, false)
	}
	if widthChanged || heightChanged {
		return r.fullRender(next, frame.cursor, width, height, true)
	}

	firstChanged, lastChanged := changedLineRange(previous, next, firstPossibleChange)
	if firstChanged < 0 {
		r.adoptFrame(next, width, height, r.hardwareCursorRow, r.previousViewportTop)
		return r.positionCursor(frame.cursor, next.len())
	}
	if firstChanged < r.previousViewportTop {
		// A source change above the viewport can shift the logical row mapping,
		// so a partial rewrite is unsafe. Mutable UI state should normally be
		// frozen or appended before it reaches this fallback.
		return r.fullRender(next, frame.cursor, width, height, true)
	}
	return r.differentialRender(next, frame.cursor, width, height, firstChanged, lastChanged)
}

func (r *inlineRenderer) prepareFrame(frame inlineFrame, width int, widthChanged bool) (rendererFrame, int) {
	dirtyFrom := len(frame.transcript)
	canReuseTranscript := !widthChanged && !frame.transcriptChanged && len(frame.transcript) == len(r.previousTranscript)
	if !canReuseTranscript {
		dirtyFrom = frame.transcriptDirtyFrom
		if widthChanged || !frame.transcriptChanged {
			dirtyFrom = 0
		}
		dirtyFrom = min(max(0, dirtyFrom), len(frame.transcript), len(r.previousTranscript))
	}

	next := rendererFrame{
		transcriptPrefix: r.previousTranscript[:dirtyFrom],
		transcriptSuffix: normalizeRendererLines(frame.transcript[dirtyFrom:], width),
		dynamic:          normalizeRendererLines(frame.dynamic, width),
	}
	return next, dirtyFrom
}

func (r *inlineRenderer) previousFrame() rendererFrame {
	return rendererFrame{
		transcriptPrefix: r.previousTranscript,
		dynamic:          r.previousDynamic,
	}
}

func normalizeRendererLines(lines []string, width int) []string {
	normalized := make([]string, len(lines))
	// Leave the last physical column unused. Writing into it can trigger an
	// implicit terminal wrap and invalidate all logical row accounting.
	limit := max(1, width-1)
	for index, line := range lines {
		if visibleLen(line) > limit {
			line = truncateANSI(line, limit)
		}
		normalized[index] = line + ansi.ResetStyle
	}
	return normalized
}

func changedLineRange(previous, next rendererFrame, start int) (int, int) {
	first, last := -1, -1
	start = min(max(0, start), previous.len(), next.len())
	for index := start; index < max(previous.len(), next.len()); index++ {
		var oldLine, newLine string
		if index < previous.len() {
			oldLine = previous.line(index)
		}
		if index < next.len() {
			newLine = next.line(index)
		}
		if oldLine != newLine {
			if first < 0 {
				first = index
			}
			last = index
		}
	}
	return first, last
}

func (r *inlineRenderer) fullRender(lines rendererFrame, cursor *tea.Cursor, width, height int, clear bool) error {
	var buffer strings.Builder
	buffer.WriteString(ansi.SetModeSynchronizedOutput)
	buffer.WriteString(ansi.ResetModeTextCursorEnable)
	if clear {
		// CSI 2J clears the visible screen and CSI 3J clears scrollback. The
		// latter is intentionally destructive and is reserved for resize,
		// /clear, or the safety fallback for a source change above viewport.
		buffer.WriteString("\x1b[r")
		buffer.WriteString(ansi.ResetStyle)
		buffer.WriteString(ansi.CursorHomePosition)
		buffer.WriteString(ansi.EraseEntireScreen)
		buffer.WriteString(ansi.EraseEntireDisplay)
		buffer.WriteString(ansi.CursorHomePosition)
	} else {
		buffer.WriteByte('\r')
	}
	for index := 0; index < lines.len(); index++ {
		if index > 0 {
			buffer.WriteString("\r\n")
		}
		buffer.WriteString(lines.line(index))
	}
	renderCursorRow := max(0, lines.len()-1)
	viewportTop := max(0, lines.len()-height)
	hardwareCursorRow := appendCursorPosition(&buffer, cursor, lines.len(), renderCursorRow)
	buffer.WriteString(ansi.ResetModeSynchronizedOutput)
	if err := r.write(buffer.String()); err != nil {
		return err
	}

	r.adoptFrame(lines, width, height, hardwareCursorRow, viewportTop)
	return nil
}

func (r *inlineRenderer) differentialRender(lines rendererFrame, cursor *tea.Cursor, width, height, firstChanged, lastChanged int) error {
	previous := r.previousFrame()
	previousViewportTop := r.previousViewportTop
	viewportTop := previousViewportTop
	hardwareCursorRow := r.hardwareCursorRow
	computeLineDiff := func(targetRow int) int {
		// Cursor movement is relative to physical screen rows. Both operands
		// start as logical rows, so translate them through their respective
		// viewport origins before taking the difference.
		currentScreenRow := hardwareCursorRow - previousViewportTop
		targetScreenRow := targetRow - viewportTop
		return targetScreenRow - currentScreenRow
	}

	appended := lines.len() > previous.len()
	appendStart := appended && firstChanged == previous.len() && firstChanged > 0

	if firstChanged >= lines.len() {
		return r.renderDeletion(lines, cursor, width, height, previousViewportTop, computeLineDiff)
	}

	var buffer strings.Builder
	buffer.WriteString(ansi.SetModeSynchronizedOutput)
	buffer.WriteString(ansi.ResetModeTextCursorEnable)
	previousViewportBottom := previousViewportTop + height - 1
	moveTargetRow := firstChanged
	if appendStart {
		moveTargetRow--
	}
	if moveTargetRow > previousViewportBottom {
		currentScreenRow := min(max(0, hardwareCursorRow-previousViewportTop), height-1)
		if moveToBottom := height - 1 - currentScreenRow; moveToBottom > 0 {
			buffer.WriteString(ansi.CursorDown(moveToBottom))
		}
		scroll := moveTargetRow - previousViewportBottom
		buffer.WriteString(strings.Repeat("\r\n", scroll))
		previousViewportTop += scroll
		viewportTop += scroll
		hardwareCursorRow = moveTargetRow
	}
	writeVerticalMove(&buffer, computeLineDiff(moveTargetRow))
	if appendStart {
		buffer.WriteString("\r\n")
	} else {
		buffer.WriteByte('\r')
	}

	renderEnd := min(lastChanged, lines.len()-1)
	for index := firstChanged; index <= renderEnd; index++ {
		if index > firstChanged {
			buffer.WriteString("\r\n")
		}
		buffer.WriteString(ansi.EraseEntireLine)
		buffer.WriteString(lines.line(index))
	}
	finalCursorRow := renderEnd
	if previous.len() > lines.len() {
		if renderEnd < lines.len()-1 {
			moveDown := lines.len() - 1 - renderEnd
			buffer.WriteString(ansi.CursorDown(moveDown))
			finalCursorRow = lines.len() - 1
		}
		extraLines := previous.len() - lines.len()
		for range extraLines {
			buffer.WriteString("\r\n")
			buffer.WriteString(ansi.EraseEntireLine)
		}
		buffer.WriteString(ansi.CursorUp(extraLines))
	}
	viewportTop = max(previousViewportTop, finalCursorRow-height+1)
	hardwareCursorRow = appendCursorPosition(&buffer, cursor, lines.len(), finalCursorRow)
	buffer.WriteString(ansi.ResetModeSynchronizedOutput)
	if err := r.write(buffer.String()); err != nil {
		return err
	}

	r.adoptFrame(lines, width, height, hardwareCursorRow, viewportTop)
	return nil
}

// Invalidate makes the next frame authoritative. This is used when /clear
// replaces the transcript wholesale, so no rows from the previous session can
// remain in terminal scrollback.
func (r *inlineRenderer) Invalidate() {
	if r != nil && !r.stopped {
		r.invalidated = true
		r.appendPending = false
	}
}

// AppendFrameAfter preserves the given prefix as ordinary terminal scrollback
// and starts the next source-backed frame directly below it. A later resize
// still clears the terminal and redraws only the new managed frame.
func (r *inlineRenderer) AppendFrameAfter(lines int) {
	if r != nil && !r.stopped {
		r.invalidated = false
		r.appendPending = true
		r.appendAfterLines = max(0, lines)
	}
}

func (r *inlineRenderer) appendFrame(lines rendererFrame, cursor *tea.Cursor, width, height, preserveLines int) error {
	previousLen := r.previousFrame().len()
	if previousLen == 0 {
		return r.fullRender(lines, cursor, width, height, false)
	}

	preserveLines = min(max(0, preserveLines), previousLen)
	replaceSuffix := preserveLines < previousLen && preserveLines >= r.previousViewportTop
	targetRow := previousLen - 1
	if replaceSuffix {
		targetRow = preserveLines
	}

	var buffer strings.Builder
	buffer.WriteString(ansi.SetModeSynchronizedOutput)
	buffer.WriteString(ansi.ResetModeTextCursorEnable)
	writeVerticalMove(&buffer, targetRow-r.hardwareCursorRow)
	if !replaceSuffix {
		// If the replaceable suffix has already moved into scrollback, it can
		// no longer be edited safely. Preserve the complete frame instead.
		buffer.WriteString("\r\n")
	} else {
		for index := targetRow; index < previousLen; index++ {
			buffer.WriteByte('\r')
			buffer.WriteString(ansi.EraseEntireLine)
			if index+1 < previousLen {
				buffer.WriteString(ansi.CursorDown(1))
			}
		}
		if clearedLines := previousLen - targetRow; clearedLines > 1 {
			buffer.WriteString(ansi.CursorUp(clearedLines - 1))
		}
	}
	buffer.WriteString(ansi.ResetModeSynchronizedOutput)
	if err := r.write(buffer.String()); err != nil {
		return err
	}

	r.forgetFrame()
	return r.fullRender(lines, cursor, width, height, false)
}

func (r *inlineRenderer) renderDeletion(lines rendererFrame, cursor *tea.Cursor, width, height, viewportTop int, lineDiff func(int) int) error {
	previousLen := r.previousFrame().len()
	targetRow := lines.len() - 1
	if targetRow < viewportTop || previousLen-lines.len() > height {
		return r.fullRender(lines, cursor, width, height, true)
	}
	extraLines := previousLen - lines.len()
	var buffer strings.Builder
	buffer.WriteString(ansi.SetModeSynchronizedOutput)
	buffer.WriteString(ansi.ResetModeTextCursorEnable)
	writeVerticalMove(&buffer, lineDiff(targetRow))
	buffer.WriteByte('\r')
	buffer.WriteString(ansi.CursorDown(1))
	for index := range extraLines {
		buffer.WriteByte('\r')
		buffer.WriteString(ansi.EraseEntireLine)
		if index < extraLines-1 {
			buffer.WriteString(ansi.CursorDown(1))
		}
	}
	buffer.WriteString(ansi.CursorUp(extraLines))
	hardwareCursorRow := appendCursorPosition(&buffer, cursor, lines.len(), targetRow)
	buffer.WriteString(ansi.ResetModeSynchronizedOutput)
	if err := r.write(buffer.String()); err != nil {
		return err
	}

	r.adoptFrame(lines, width, height, hardwareCursorRow, viewportTop)
	return nil
}

func (r *inlineRenderer) positionCursor(cursor *tea.Cursor, totalLines int) error {
	var buffer strings.Builder
	hardwareCursorRow := appendCursorPosition(&buffer, cursor, totalLines, r.hardwareCursorRow)
	if err := r.write(buffer.String()); err != nil {
		return err
	}
	r.hardwareCursorRow = hardwareCursorRow
	return nil
}

func appendCursorPosition(buffer *strings.Builder, cursor *tea.Cursor, totalLines, currentRow int) int {
	if cursor == nil || totalLines == 0 {
		buffer.WriteString(ansi.ResetModeTextCursorEnable)
		return currentRow
	}
	targetRow := min(max(0, cursor.Y), totalLines-1)
	writeVerticalMove(buffer, targetRow-currentRow)
	buffer.WriteString(ansi.CursorHorizontalAbsolute(max(0, cursor.X) + 1))
	buffer.WriteString(ansi.SetModeTextCursorEnable)
	return targetRow
}

func (r *inlineRenderer) Stop() error {
	if r == nil || r.stopped {
		return nil
	}
	r.stopped = true
	var buffer strings.Builder
	previous := r.previousFrame()
	if dynamicLen := len(r.previousDynamic); dynamicLen > 0 {
		// Interactive chrome is not part of the transcript. Clear the visible
		// suffix and leave the shell cursor on the first blank row below the
		// transcript instead of preserving the editor and footer in scrollback.
		clearFrom := max(previous.transcriptLen(), r.previousViewportTop)
		if clearFrom < previous.len() {
			writeVerticalMove(&buffer, clearFrom-r.hardwareCursorRow)
			buffer.WriteString(ansi.ResetStyle)
			for index := clearFrom; index < previous.len(); index++ {
				buffer.WriteByte('\r')
				buffer.WriteString(ansi.EraseEntireLine)
				if index+1 < previous.len() {
					buffer.WriteString(ansi.CursorDown(1))
				}
			}
			if clearedLines := previous.len() - clearFrom; clearedLines > 1 {
				buffer.WriteString(ansi.CursorUp(clearedLines - 1))
			}
			buffer.WriteByte('\r')
		}
	} else if previous.len() > 0 {
		targetRow := previous.len() - 1
		writeVerticalMove(&buffer, targetRow-r.hardwareCursorRow)
		buffer.WriteString("\r\n")
	}
	buffer.WriteString("\x1b[0 q")
	buffer.WriteString(ansi.ResetModifyOtherKeys)
	buffer.WriteString(ansi.KittyKeyboard(0, 1))
	buffer.WriteString(ansi.ResetModeBracketedPaste)
	buffer.WriteString(ansi.SetModeTextCursorEnable)
	return r.write(buffer.String())
}

func (r *inlineRenderer) adoptFrame(lines rendererFrame, width, height, cursorRow, viewportTop int) {
	prefixLen := len(lines.transcriptPrefix)
	if prefixLen <= len(r.previousTranscript) {
		r.previousTranscript = append(r.previousTranscript[:prefixLen], lines.transcriptSuffix...)
	} else {
		r.previousTranscript = append(r.previousTranscript[:0], lines.transcriptPrefix...)
		r.previousTranscript = append(r.previousTranscript, lines.transcriptSuffix...)
	}
	r.previousDynamic = append(r.previousDynamic[:0], lines.dynamic...)
	r.previousWidth = width
	r.previousHeight = height
	r.hardwareCursorRow = cursorRow
	r.previousViewportTop = viewportTop
}

func (r *inlineRenderer) forgetFrame() {
	r.previousTranscript = nil
	r.previousDynamic = nil
	r.previousWidth = 0
	r.previousHeight = 0
	r.previousViewportTop = 0
	r.hardwareCursorRow = 0
}

func writeVerticalMove(buffer *strings.Builder, rows int) {
	if rows > 0 {
		buffer.WriteString(ansi.CursorDown(rows))
	} else if rows < 0 {
		buffer.WriteString(ansi.CursorUp(-rows))
	}
}

func (r *inlineRenderer) write(value string) error {
	if value == "" {
		return nil
	}
	_, err := io.WriteString(r.out, value)
	return err
}
