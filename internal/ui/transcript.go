package ui

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
)

type screenBlockKind uint8

const (
	screenBlockSystem screenBlockKind = iota
	screenBlockScopeChange
	screenBlockUser
	screenBlockAssistant
	screenBlockTool
	screenBlockError
	screenBlockChangeSummary
	screenBlockDuration
)

type screenBlock struct {
	kind       screenBlockKind
	text       string
	attemptID  string
	duration   time.Duration
	tool       *toolBlock
	completion *processResultMeta
}

type toolBlock struct {
	name              string
	rawArguments      string
	resultText        string
	callIDs           []string
	done              bool
	failed            bool
	startedAt         time.Time
	elapsed           time.Duration
	group             *toolGroupMeta
	fileChange        *fileChangeMeta
	process           *processResultMeta
	output            string
	superseded        bool
	shell             *shellMeta
	collapsed         bool
	collapsedDuration time.Duration
}

type toolGroupMeta struct {
	key  string
	dir  string
	item string
}

type shellMeta struct {
	private bool
}

type renderedScreenBlock struct {
	end int
}

// transcriptState owns the presentation state derived from session history
// and live agent events. Terminal I/O and Agent calls remain with screenModel;
// cache invalidation stays local to this state.
type transcriptState struct {
	blocks         []screenBlock
	currentAttempt string
	root           string
	// preserveToolTailHeight asks the next rendered refresh to retain the
	// physical rows released when a live tool tail becomes stable.
	preserveToolTailHeight bool

	renderCache      []renderedScreenBlock
	renderCacheLines []string
	renderCacheWidth int
	renderDirtyFrom  int
	lines            []string
	dirty            bool
	dirtyFrom        int
}

func (m *screenModel) loadSessionHistory() error {
	m.composer.resetHistory()
	state, err := m.agent.State(m.ctx)
	if err != nil {
		return err
	}
	m.refreshSessionStatus()
	for index := 0; index < len(state.Items); index++ {
		item := state.Items[index]
		switch item.Kind {
		case model.ItemUserText:
			m.composer.remember(item.Text)
			if call, result, ok := recordedShellItems(state.Items, index); ok {
				m.addToolCall(call)
				m.transcript.markLastToolAsShell(false)
				m.finishTool(result)
				index += 2
				continue
			}
			m.addBlock(screenBlockUser, item.Text)
		case model.ItemBoundaryText:
			if strings.TrimSpace(item.Text) != "" {
				m.addBoundaryEvent(item.Text, item.Details)
			}
		case model.ItemAssistantText:
			if strings.TrimSpace(item.Text) != "" {
				m.addBlock(screenBlockAssistant, item.Text)
			}
		case model.ItemToolCall:
			if item.ToolCall != nil {
				m.addToolCall(*item.ToolCall)
			}
		case model.ItemToolResult:
			if item.ToolResult != nil {
				m.finishTool(*item.ToolResult)
			}
		}
	}
	m.operation.changedPaths = nil
	return nil
}

func (m *screenModel) resetTranscript() {
	m.renderer.Invalidate()
	m.clearTranscript()
}

func (m *screenModel) continueTranscriptBelow() {
	m.renderer.AppendFrameAfter(len(m.transcript.lines))
	m.clearTranscript()
}

func (m *screenModel) clearTranscript() {
	m.frameRowFloor = 0
	m.transcript.clear()
	m.operation.changedPaths = nil
}

func (m *screenModel) addBlock(kind screenBlockKind, text string) {
	m.transcript.addBlock(kind, text)
}

func (m *screenModel) appendBlock(block screenBlock) {
	m.transcript.appendBlock(block)
}

func (transcript *transcriptState) clear() {
	transcript.blocks = nil
	transcript.lines = nil
	transcript.renderCache = nil
	transcript.renderCacheLines = nil
	transcript.renderCacheWidth = 0
	transcript.renderDirtyFrom = 0
	transcript.preserveToolTailHeight = false
	transcript.dirty = true
	transcript.dirtyFrom = 0
}

func (transcript *transcriptState) addBlock(kind screenBlockKind, text string) {
	transcript.appendBlock(screenBlock{kind: kind, text: sanitizeTerminalText(text)})
}

func (transcript *transcriptState) appendBlock(block screenBlock) {
	if endsToolTail(block) {
		transcript.collapseToolTail()
	}
	transcript.markBlockDirty(len(transcript.blocks))
	transcript.blocks = append(transcript.blocks, block)
}

func endsToolTail(block screenBlock) bool {
	return block.kind != screenBlockTool && (block.kind == screenBlockDuration || strings.TrimSpace(block.text) != "")
}

// modelOwnedToolBlock excludes explicit user shell escapes: they are local
// work, not agent trace, and keep their complete output in every profile.
func modelOwnedToolBlock(block screenBlock) bool {
	return block.kind == screenBlockTool && block.tool != nil && block.tool.shell == nil
}

// collapseToolTail commits model-owned activity when the next meaningful
// transcript block arrives. Compact display can then replace that stable
// history with a summary while leaving the current tool tail observable.
func (transcript *transcriptState) collapseToolTail() {
	start, end, ok := transcript.modelOwnedToolTail()
	if !ok {
		return
	}
	transcript.preserveToolTailHeight = true
	transcript.markBlockDirty(start)
	for index := start; index <= end; index++ {
		transcript.blocks[index].tool.collapsed = true
	}
	earliest := transcript.blocks[start].tool.startedAt
	for index := start + 1; index <= end; index++ {
		startedAt := transcript.blocks[index].tool.startedAt
		if !startedAt.IsZero() && (earliest.IsZero() || startedAt.Before(earliest)) {
			earliest = startedAt
		}
	}
	if !earliest.IsZero() {
		transcript.blocks[end].tool.collapsedDuration = max(time.Duration(0), time.Since(earliest))
	}
}

func (transcript *transcriptState) modelOwnedToolTail() (int, int, bool) {
	end := len(transcript.blocks) - 1
	if end < 0 || !modelOwnedToolBlock(transcript.blocks[end]) {
		return 0, 0, false
	}
	start := end
	for start > 0 && modelOwnedToolBlock(transcript.blocks[start-1]) {
		start--
	}
	return start, end, true
}

// foldOldestToolTailBlock advances the compact prefix by one source block.
// The renderer still expands failures and running work, so folding never hides
// the part of a tool that currently needs attention.
func (transcript *transcriptState) foldOldestToolTailBlock() bool {
	start, end, ok := transcript.modelOwnedToolTail()
	if !ok {
		return false
	}
	for index := start; index <= end; index++ {
		if transcript.blocks[index].tool.collapsed {
			continue
		}
		transcript.markBlockDirty(start)
		transcript.blocks[index].tool.collapsed = true
		return true
	}
	return false
}

func (transcript *transcriptState) markBlockDirty(index int) {
	index = max(0, index)
	if index < transcript.renderDirtyFrom {
		transcript.renderDirtyFrom = index
	}
}

func (m *screenModel) addToolCall(call model.ToolCall) {
	m.addToolCallAt(call, time.Time{})
}

func (m *screenModel) addToolCallAt(call model.ToolCall, startedAt time.Time) {
	m.transcript.addToolCallAt(call, startedAt)
	var args jobDisplayArgs
	if call.Name != "job" || !decodeToolDisplayArgs(call.RawArguments, &args) || args.JobID == "" {
		return
	}
	if command := m.resolveJobCommand(args.JobID); command != "" {
		m.transcript.blocks[len(m.transcript.blocks)-1].text = describeJobCall(args, command)
	}
}

func (transcript *transcriptState) addToolCallAt(call model.ToolCall, startedAt time.Time) {
	display := describeToolCall(call.Name, call.RawArguments, transcript.root)
	var group *toolGroupMeta
	if display.GroupKey != "" {
		group = &toolGroupMeta{key: display.GroupKey, dir: display.GroupDir, item: display.GroupItem}
	}
	if len(transcript.blocks) > 0 && group != nil {
		lastIndex := len(transcript.blocks) - 1
		last := transcript.blocks[lastIndex]
		if last.kind == screenBlockTool && last.tool != nil && last.tool.group != nil && last.tool.group.key == group.key {
			transcript.markBlockDirty(transcript.toolPresentationGroupStart(lastIndex))
		}
	}
	transcript.appendBlock(screenBlock{
		kind: screenBlockTool,
		text: sanitizeTerminalText(display.Text),
		tool: &toolBlock{
			name: sanitizeTerminalText(call.Name), callIDs: []string{call.ID},
			rawArguments: call.RawArguments, group: group, startedAt: startedAt,
		},
	})
}

func (transcript *transcriptState) markLastToolAsShell(private bool) {
	if len(transcript.blocks) == 0 {
		return
	}
	block := &transcript.blocks[len(transcript.blocks)-1]
	if block.kind == screenBlockTool && block.tool != nil {
		block.tool.shell = &shellMeta{private: private}
	}
}

func (m *screenModel) finishTool(result model.ToolResult) {
	for _, path := range m.transcript.finishTool(result) {
		m.operation.changedPaths = appendUniquePath(m.operation.changedPaths, path)
	}
}

func (transcript *transcriptState) finishTool(result model.ToolResult) []string {
	var changedPaths []string
	for index := range slices.Backward(transcript.blocks) {
		block := &transcript.blocks[index]
		if block.kind != screenBlockTool || block.tool == nil {
			continue
		}
		tool := block.tool
		callIndex := toolCallIndex(tool.callIDs, result.CallID)
		if callIndex < 0 {
			continue
		}
		transcript.markBlockDirty(transcript.toolPresentationGroupStart(index))
		tool.callIDs = append(tool.callIDs[:callIndex], tool.callIDs[callIndex+1:]...)
		tool.done = len(tool.callIDs) == 0
		// Presentation groups use the longest member duration, so each source
		// call keeps only the time it actually spent running.
		if tool.done && !tool.startedAt.IsZero() {
			tool.elapsed = max(tool.elapsed, time.Since(tool.startedAt))
		}
		failed := result.Error || result.Unknown
		tool.failed = tool.failed || failed
		tool.resultText = displayableToolResult(result.Content)
		recognizedDetail := false
		for _, detail := range result.Details {
			if change, ok := agent.FileChangeFromDetail(detail); ok {
				recognizedDetail = true
				tool.fileChange = &change
				block.text = sanitizeTerminalText(strings.TrimSpace(change.Operation + "  " + change.Path))
				if path, include := changedFilePath(change); include {
					changedPaths = appendUniquePath(changedPaths, path)
				}
			}
			if process, ok := agent.ProcessResultFromDetail(detail); ok {
				recognizedDetail = true
				tool.process = &process
				tool.output = processOutputFromContent(result.Content.Text())
				tool.elapsed = time.Duration(process.DurationMillis) * time.Millisecond
				tool.failed = process.Status != agent.ProcessCompleted && process.Status != agent.ProcessRunning
				var args jobDisplayArgs
				if tool.name == "job" && process.Command != "" && decodeToolDisplayArgs(tool.rawArguments, &args) {
					block.text = describeJobCall(args, process.Command)
				}
			}
		}
		if failed && !recognizedDetail && strings.TrimSpace(result.Content.Text()) != "" {
			block.text += ": " + compactSingleLine(sanitizeTerminalText(result.Content.Text()), 180)
		}
		for _, part := range result.Content {
			if part.Kind == model.ContentPartImage && part.Image != nil {
				block.text += fmt.Sprintf("  [%s %d×%d]", part.Image.MediaType, part.Image.Width, part.Image.Height)
			}
		}
		return changedPaths
	}
	text := "tool result"
	if result.CallID != "" {
		text += " " + result.CallID
	}
	transcript.appendBlock(screenBlock{
		kind: screenBlockTool,
		text: sanitizeTerminalText(text),
		tool: &toolBlock{done: true, failed: result.Error || result.Unknown, resultText: displayableToolResult(result.Content)},
	})
	return changedPaths
}

func (transcript transcriptState) toolPresentationGroupStart(index int) int {
	if index < 0 || index >= len(transcript.blocks) {
		return max(0, index)
	}
	block := transcript.blocks[index]
	if block.kind != screenBlockTool || block.tool == nil || block.tool.group == nil {
		return index
	}
	key := block.tool.group.key
	for index > 0 {
		previous := transcript.blocks[index-1]
		if previous.kind != screenBlockTool || previous.tool == nil || previous.tool.group == nil || previous.tool.group.key != key {
			break
		}
		index--
	}
	return index
}

func displayableToolResult(content model.Content) string {
	var result strings.Builder
	endsWithNewline := true
	for _, part := range content {
		switch {
		case part.Kind == model.ContentPartText:
			result.WriteString(part.Text)
			if part.Text != "" {
				endsWithNewline = strings.HasSuffix(part.Text, "\n")
			}
		case part.Kind == model.ContentPartImage && part.Image != nil:
			if result.Len() > 0 && !endsWithNewline {
				result.WriteString("  ")
			}
			fmt.Fprintf(&result, "[%s %d×%d]", part.Image.MediaType, part.Image.Width, part.Image.Height)
			endsWithNewline = false
		}
	}
	return result.String()
}

func recordedShellItems(items []model.Item, index int) (model.ToolCall, model.ToolResult, bool) {
	if index < 0 || index+2 >= len(items) || items[index].Kind != model.ItemUserText {
		return model.ToolCall{}, model.ToolResult{}, false
	}
	command, private, shell := shellEscapeCommand(items[index].Text)
	callItem := items[index+1]
	resultItem := items[index+2]
	if !shell || private || command == "" || callItem.Kind != model.ItemToolCall || callItem.ToolCall == nil ||
		callItem.ToolCall.Name != "bash" || resultItem.Kind != model.ItemToolResult || resultItem.ToolResult == nil ||
		resultItem.ToolResult.CallID != callItem.ToolCall.ID {
		return model.ToolCall{}, model.ToolResult{}, false
	}
	var arguments struct {
		Command string `json:"command"`
	}
	if !decodeToolDisplayArgs(callItem.ToolCall.RawArguments, &arguments) || strings.TrimSpace(arguments.Command) != command {
		return model.ToolCall{}, model.ToolResult{}, false
	}
	return *callItem.ToolCall, *resultItem.ToolResult, true
}

func toolCallIndex(callIDs []string, callID string) int {
	for index, candidate := range callIDs {
		if candidate == callID {
			return index
		}
	}
	return -1
}

func (m *screenModel) refreshTranscript() {
	preserveHeight := m.displayProfile == DisplayCompact && m.transcript.preserveToolTailHeight &&
		m.renderer != nil && m.renderer.started && !m.renderer.invalidated &&
		m.renderer.previousWidth == m.width && m.renderer.previousHeight == m.height
	previousFrameRows := 0
	if preserveHeight {
		previousFrameRows = len(m.renderer.previousTranscript) + len(m.renderer.previousDynamic)
	}
	m.transcript.preserveToolTailHeight = false
	m.transcript.refresh(m.contentWidth(), m.renderBlockLinesAt)
	m.fitCompactToolTail()
	if preserveHeight && previousFrameRows > m.baseInlineFrameRows() {
		m.frameRowFloor = max(m.frameRowFloor, previousFrameRows)
	}
}

func (transcript *transcriptState) refresh(width int, renderBlock func(int, screenBlock) []string) {
	lines, dirtyFrom := transcript.renderLinesFromDirty(width, renderBlock)
	if dirtyFrom == len(transcript.lines) && len(lines) == len(transcript.lines) {
		return
	}
	dirtyFrom = min(dirtyFrom, len(transcript.lines), len(lines))
	transcript.lines = append(transcript.lines[:dirtyFrom], lines[dirtyFrom:]...)
	if !transcript.dirty || dirtyFrom < transcript.dirtyFrom {
		transcript.dirtyFrom = dirtyFrom
	}
	transcript.dirty = true
}

func (transcript *transcriptState) renderLinesFromDirty(width int, renderBlock func(int, screenBlock) []string) ([]string, int) {
	if transcript.renderCacheWidth != width || len(transcript.renderCache) > len(transcript.blocks) {
		transcript.renderCache = nil
		transcript.renderCacheLines = nil
		transcript.renderCacheWidth = width
		transcript.renderDirtyFrom = 0
	}
	common := min(transcript.renderDirtyFrom, len(transcript.renderCache), len(transcript.blocks))
	lineEnd := 0
	if common > 0 {
		lineEnd = transcript.renderCache[common-1].end
	}
	transcript.renderCache = transcript.renderCache[:common]
	transcript.renderCacheLines = transcript.renderCacheLines[:lineEnd]
	for index := common; index < len(transcript.blocks); index++ {
		lines := renderBlock(index, transcript.blocks[index])
		// Keep the larger of adjacent gaps: two rows around user messages,
		// one around notices. Omit leading padding at the top of the transcript.
		overlap := 0
		for overlap < len(lines) && isBlankTranscriptLine(lines[overlap]) {
			previous := len(transcript.renderCacheLines) - 1 - overlap
			if len(transcript.renderCacheLines) > 0 && (previous < 0 || !isBlankTranscriptLine(transcript.renderCacheLines[previous])) {
				break
			}
			overlap++
		}
		transcript.renderCacheLines = append(transcript.renderCacheLines, lines[overlap:]...)
		transcript.renderCache = append(transcript.renderCache, renderedScreenBlock{end: len(transcript.renderCacheLines)})
	}
	transcript.renderDirtyFrom = len(transcript.blocks)
	return transcript.renderCacheLines, lineEnd
}

// isBlankTranscriptLine reports whether a line carries no ink. Styled gutters
// leave escape sequences behind, so those lines are deliberately not blank:
// the user bar must survive next to an empty message line.
func isBlankTranscriptLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

func transcriptEndsBlank(lines []string) bool {
	return len(lines) == 0 || isBlankTranscriptLine(lines[len(lines)-1])
}

func (transcript *transcriptState) invalidate() {
	transcript.renderDirtyFrom = 0
}

func (transcript *transcriptState) presented() {
	transcript.dirty = false
}

func (m screenModel) renderBlockLines(block screenBlock) []string {
	switch block.kind {
	case screenBlockSystem:
		if block.completion != nil && m.displayProfile != DisplayFull {
			return m.renderProcessCompletion(*block.completion)
		}
		return m.padded(m.wrappedMarked(" ", m.renderSystemText(block.text)))
	case screenBlockScopeChange:
		return m.padded(m.wrappedMarked(" ", m.renderScopeChangeText(block.text)))
	case screenBlockUser:
		return m.renderUserBlock(block.text)
	case screenBlockAssistant:
		return m.renderAssistantBlock(block.text)
	case screenBlockTool:
		if block.tool == nil {
			return m.renderErrorNotice("invalid tool block")
		}
		tool := block.tool
		if tool.process != nil {
			return m.renderProcessResultLines(block)
		}
		if tool.fileChange != nil {
			return m.renderFileChangeBlock(block)
		}
		detail := ""
		if tool.done {
			detail = formatToolDuration(tool.elapsed)
		}
		return m.renderToolSummaryLines(m.toolMarker(tool.failed), block.text, detail)
	case screenBlockError:
		return m.renderErrorNotice(block.text)
	case screenBlockChangeSummary:
		if m.displayProfile == DisplayCompact {
			return nil
		}
		return m.padded(m.wrappedMarked(" ", m.renderSystemText(block.text)))
	case screenBlockDuration:
		if m.displayProfile == DisplayCompact {
			return nil
		}
		return m.padded([]string{m.renderDurationLine(block.duration)})
	default:
		return m.wrappedMarked(" ", block.text)
	}
}

// renderErrorNotice is the padded "!" notice shared by error blocks and by the
// fallback for a tool block that arrived without its tool.
func (m screenModel) renderErrorNotice(text string) []string {
	return m.padded(m.wrappedMarked(m.errorStyle.Render("!"), text))
}

// toolMarker keeps the gutter empty for tool calls: the highlighted tool name
// carries the line on its own, so only failures deserve a mark.
func (m screenModel) toolMarker(failed bool) string {
	if failed {
		return m.errorStyle.Render("×")
	}
	return " "
}

// renderSystemText styles each line on its own. lipgloss pads every line of a
// multi-line render out to the longest one, which would trail spaces across the
// block and push a highlighted fragment away from the text it belongs to.
func (m screenModel) renderSystemText(text string) string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = m.renderSystemLine(line)
	}
	return strings.Join(lines, "\n")
}

func (m screenModel) renderSystemLine(line string) string {
	style := m.mutedStyle
	if link, err := url.ParseRequestURI(line); err == nil && link.Host != "" && (link.Scheme == "https" || link.Scheme == "http") {
		// Give standalone URLs an explicit target before wrapping. The shared
		// ID keeps all rows of a link together through wrapping and repainting.
		style = style.Hyperlink(line, "id=skot")
	}
	return style.Render(line)
}

// renderScopeChangeText keeps the deliberate transition readable as ordinary
// text while leaving persistent-policy details visually secondary.
func (m screenModel) renderScopeChangeText(text string) string {
	lines := strings.Split(text, "\n")
	for index := 1; index < len(lines); index++ {
		lines[index] = m.mutedStyle.Render(lines[index])
	}
	return strings.Join(lines, "\n")
}

func (m screenModel) renderAssistantBlock(text string) []string {
	lines := []string{m.marked(" ", "")}
	marked := false
	for _, rendered := range m.markdown.renderMarkdownLines(text, m.contentWidth()) {
		for _, line := range wrapDisplayLine(rendered, m.contentWidth()) {
			marker := " "
			if !marked && strings.TrimSpace(line) != "" {
				marker = "•"
				marked = true
			}
			lines = append(lines, m.marked(marker, line))
		}
	}
	return append(lines, m.marked(" ", ""))
}

func (m screenModel) renderUserBlock(text string) []string {
	// Keep the bar on wrapped lines so the whole message reads as one block.
	bar := m.userBarStyle.Render(userBarMarker)
	blank := m.marked(" ", "")
	lines := []string{blank, blank}
	lines = append(lines, m.wrappedMarkedWithContinuation(bar, bar, text)...)
	return append(lines, blank, blank)
}

func (m screenModel) wrappedMarked(marker, text string) []string {
	return m.wrappedMarkedWithContinuation(marker, " ", text)
}

func (m screenModel) wrappedMarkedWithContinuation(marker, continuation, text string) []string {
	width := m.contentWidth()
	var lines []string
	marked := false
	for sourceLine := range strings.SplitSeq(text, "\n") {
		for _, line := range wrapDisplayLine(sourceLine, width) {
			lineMarker := continuation
			if !marked {
				lineMarker = marker
				marked = true
			}
			lines = append(lines, m.marked(lineMarker, line))
		}
	}
	return lines
}

func (m screenModel) renderToolSummaryLines(marker, text, detail string) []string {
	label, body, indent, hanging := toolCommandPrefix(text)
	if !hanging {
		line := m.renderToolDisplay(text)
		if detail != "" {
			line += "  " + m.mutedStyle.Render(detail)
		}
		return m.wrappedMarked(marker, line)
	}
	if detail != "" {
		body += "  " + m.mutedStyle.Render(detail)
	}
	return m.hangingLines(marker, m.accentStyle.Render(label)+" ", strings.Repeat(" ", indent), body)
}

func toolCommandPrefix(text string) (label, body string, indent int, ok bool) {
	switch {
	case strings.HasPrefix(text, "$ "):
		return "$", strings.TrimPrefix(text, "$ "), 2, true
	case strings.HasPrefix(text, "!! "):
		return "!!", strings.TrimPrefix(text, "!! "), 3, true
	default:
		return "", text, processOutputIndentWidth, false
	}
}

// padded surrounds a notice with blank lines. Tool calls deliberately go
// unpadded so that a run of them reads as one dense list.
func (m screenModel) padded(lines []string) []string {
	blank := m.marked(" ", "")
	padded := make([]string, 0, len(lines)+2)
	padded = append(padded, blank)
	padded = append(padded, lines...)
	return append(padded, blank)
}

func (m screenModel) marked(marker, text string) string {
	return marker + strings.Repeat(" ", transcriptGutter-1) + text
}
