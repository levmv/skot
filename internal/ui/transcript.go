package ui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

type screenBlockKind uint8

const (
	screenBlockSystem screenBlockKind = iota
	screenBlockUser
	screenBlockAssistant
	screenBlockTool
	screenBlockError
	screenBlockDuration
)

type screenBlock struct {
	kind      screenBlockKind
	text      string
	attemptID string
	duration  time.Duration
	tool      *toolBlock
}

type toolBlock struct {
	name       string
	callIDs    []string
	done       bool
	failed     bool
	startedAt  time.Time
	elapsed    time.Duration
	group      *toolGroupMeta
	fileChange *fileChangeMeta
	process    *processResultMeta
	output     string
	superseded bool
	shell      *shellMeta
}

type toolGroupMeta struct {
	key   string
	dir   string
	items string
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
		case agent.ItemUserText:
			m.composer.remember(item.Text)
			if call, result, ok := recordedShellItems(state.Items, index); ok {
				m.addToolCall(call)
				m.transcript.markLastToolAsShell(false)
				m.finishTool(result)
				index += 2
				continue
			}
			m.addBlock(screenBlockUser, item.Text)
		case agent.ItemBoundaryText:
			if strings.TrimSpace(item.Text) != "" {
				m.addBlock(screenBlockSystem, item.Text)
			}
		case agent.ItemAssistantText:
			if strings.TrimSpace(item.Text) != "" {
				m.addBlock(screenBlockAssistant, item.Text)
			}
		case agent.ItemToolCall:
			if item.ToolCall != nil {
				m.addToolCall(*item.ToolCall)
			}
		case agent.ItemToolResult:
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
	transcript.dirty = true
	transcript.dirtyFrom = 0
}

func (transcript *transcriptState) addBlock(kind screenBlockKind, text string) {
	transcript.appendBlock(screenBlock{kind: kind, text: sanitizeTerminalText(text)})
}

func (transcript *transcriptState) appendBlock(block screenBlock) {
	transcript.markBlockDirty(len(transcript.blocks))
	transcript.blocks = append(transcript.blocks, block)
}

func (transcript *transcriptState) markBlockDirty(index int) {
	index = max(0, index)
	if index < transcript.renderDirtyFrom {
		transcript.renderDirtyFrom = index
	}
}

func (m *screenModel) addToolCall(call agent.ToolCall) {
	m.transcript.addToolCallAt(call, time.Time{})
}

func (m *screenModel) addToolCallAt(call agent.ToolCall, startedAt time.Time) {
	m.transcript.addToolCallAt(call, startedAt)
}

func (transcript *transcriptState) addToolCallAt(call agent.ToolCall, startedAt time.Time) {
	display := describeToolCall(call.Name, call.RawArguments)
	if display.GroupKey != "" && len(transcript.blocks) > 0 {
		last := &transcript.blocks[len(transcript.blocks)-1]
		if last.kind == screenBlockTool && last.tool != nil && !last.tool.failed && last.tool.group != nil && last.tool.group.key == display.GroupKey {
			transcript.markBlockDirty(len(transcript.blocks) - 1)
			items := append(splitCompactToolItems(last.tool.group.items), display.GroupItem)
			last.text = sanitizeTerminalText(formatReadGroup(last.tool.group.dir, items))
			last.tool.group.items = strings.Join(items, compactToolItemSeparator)
			last.tool.callIDs = append(last.tool.callIDs, call.ID)
			last.tool.done = false
			if last.tool.startedAt.IsZero() {
				last.tool.startedAt = startedAt
			}
			return
		}
	}
	var group *toolGroupMeta
	if display.GroupKey != "" {
		group = &toolGroupMeta{key: display.GroupKey, dir: display.GroupDir, items: display.GroupItem}
	}
	transcript.appendBlock(screenBlock{
		kind: screenBlockTool,
		text: sanitizeTerminalText(display.Text),
		tool: &toolBlock{
			name: sanitizeTerminalText(call.Name), callIDs: []string{call.ID},
			group: group, startedAt: startedAt,
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

func (m *screenModel) finishTool(result agent.ToolResult) {
	for _, path := range m.transcript.finishTool(result) {
		m.operation.changedPaths = appendUniquePath(m.operation.changedPaths, path)
	}
}

func (transcript *transcriptState) finishTool(result agent.ToolResult) []string {
	var changedPaths []string
	for index := len(transcript.blocks) - 1; index >= 0; index-- {
		block := &transcript.blocks[index]
		if block.kind != screenBlockTool || block.tool == nil {
			continue
		}
		tool := block.tool
		callIndex := toolCallIndex(tool.callIDs, result.CallID)
		if callIndex < 0 {
			continue
		}
		transcript.markBlockDirty(index)
		tool.callIDs = append(tool.callIDs[:callIndex], tool.callIDs[callIndex+1:]...)
		tool.done = len(tool.callIDs) == 0
		if tool.done && !tool.startedAt.IsZero() {
			tool.elapsed = max(time.Duration(0), time.Since(tool.startedAt))
		}
		failed := result.Error || result.Unknown
		tool.failed = tool.failed || failed
		recognizedDetail := false
		for _, detail := range result.Details {
			if change, ok := workspacetools.FileChangeMetaFromDetail(detail); ok {
				recognizedDetail = true
				tool.fileChange = &change
				block.text = sanitizeTerminalText(strings.TrimSpace(change.Operation + "  " + change.Path))
				if path, include := changedFilePath(change); include {
					changedPaths = appendUniquePath(changedPaths, path)
				}
			}
			if process, ok := workspacetools.ProcessResultFromDetail(detail); ok {
				recognizedDetail = true
				tool.process = &process
				tool.output = processOutputFromContent(result.Content)
				tool.elapsed = time.Duration(process.DurationMillis) * time.Millisecond
				tool.failed = process.Status != workspacetools.ProcessCompleted && process.Status != workspacetools.ProcessRunning
			}
		}
		if failed && !recognizedDetail && strings.TrimSpace(result.Content) != "" {
			block.text += ": " + compactSingleLine(sanitizeTerminalText(result.Content), 180)
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
		tool: &toolBlock{done: true, failed: result.Error || result.Unknown},
	})
	return changedPaths
}

func recordedShellItems(items []agent.Item, index int) (agent.ToolCall, agent.ToolResult, bool) {
	if index < 0 || index+2 >= len(items) || items[index].Kind != agent.ItemUserText {
		return agent.ToolCall{}, agent.ToolResult{}, false
	}
	command, private, shell := shellEscapeCommand(items[index].Text)
	callItem := items[index+1]
	resultItem := items[index+2]
	if !shell || private || command == "" || callItem.Kind != agent.ItemToolCall || callItem.ToolCall == nil ||
		callItem.ToolCall.Name != "bash" || resultItem.Kind != agent.ItemToolResult || resultItem.ToolResult == nil ||
		resultItem.ToolResult.CallID != callItem.ToolCall.ID {
		return agent.ToolCall{}, agent.ToolResult{}, false
	}
	var arguments struct {
		Command string `json:"command"`
	}
	if !decodeToolDisplayArgs(callItem.ToolCall.RawArguments, &arguments) || strings.TrimSpace(arguments.Command) != command {
		return agent.ToolCall{}, agent.ToolResult{}, false
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
	m.transcript.refresh(m.contentWidth(), m.renderBlockLines)
}

func (transcript *transcriptState) refresh(width int, renderBlock func(screenBlock) []string) {
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

func (transcript *transcriptState) renderLinesFromDirty(width int, renderBlock func(screenBlock) []string) ([]string, int) {
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
		lines := renderBlock(transcript.blocks[index])
		// Blocks declare the air they want on both sides, so neighbours that
		// both want it would double-space. Drop the leading blank where the
		// previous block already ended in one, or at the top of the transcript.
		if len(lines) > 0 && isBlankTranscriptLine(lines[0]) && transcriptEndsBlank(transcript.renderCacheLines) {
			lines = lines[1:]
		}
		transcript.renderCacheLines = append(transcript.renderCacheLines, lines...)
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
		return m.padded(m.wrappedMarked(" ", m.renderSystemText(block.text)))
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
			return m.renderFileChangeLines(block.text, *tool.fileChange, m.toolMarker(tool.failed))
		}
		detail := ""
		if tool.done && tool.elapsed > 0 {
			detail = formatDuration(tool.elapsed)
		}
		return m.renderToolSummaryLines(m.toolMarker(tool.failed), block.text, detail)
	case screenBlockError:
		return m.renderErrorNotice(block.text)
	case screenBlockDuration:
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
	const (
		sandboxOff = "sandbox: off"
		off        = "off"
	)
	var rendered strings.Builder
	for {
		index := strings.Index(line, sandboxOff)
		if index < 0 {
			rendered.WriteString(m.mutedStyle.Render(line))
			return rendered.String()
		}
		offIndex := index + len(sandboxOff) - len(off)
		rendered.WriteString(m.mutedStyle.Render(line[:offIndex]))
		rendered.WriteString(m.errorStyle.Render(off))
		line = line[index+len(sandboxOff):]
	}
}

func (m screenModel) renderAssistantBlock(text string) []string {
	lines := []string{m.marked(" ", "")}
	marked := false
	for _, rendered := range m.markdown.renderLinesAtWidth(text, m.contentWidth()) {
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
	// A muted bar down the whole message stays findable when scrolling back
	// without the visual weight of a filled gutter.
	bar := m.userBarStyle.Render(userBarMarker)
	lines := []string{m.marked(" ", "")}
	lines = append(lines, m.wrappedMarkedWithContinuation(bar, bar, text)...)
	return append(lines, m.marked(" ", ""))
}

func (m screenModel) wrappedMarked(marker, text string) []string {
	return m.wrappedMarkedWithContinuation(marker, " ", text)
}

func (m screenModel) wrappedMarkedWithContinuation(marker, continuation, text string) []string {
	width := m.contentWidth()
	var lines []string
	marked := false
	for _, sourceLine := range strings.Split(text, "\n") {
		wrapped := lipgloss.Wrap(sourceLine, width, "")
		if wrapped == "" {
			wrapped = " "
		}
		for _, line := range strings.Split(wrapped, "\n") {
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
	wrapped := wrapDisplayLine(body, max(1, m.contentWidth()-indent))
	lines := make([]string, 0, len(wrapped))
	for index, line := range wrapped {
		lineMarker := " "
		prefix := strings.Repeat(" ", indent)
		if index == 0 {
			lineMarker = marker
			prefix = m.accentStyle.Render(label) + " "
		}
		lines = append(lines, m.marked(lineMarker, prefix+line))
	}
	return lines
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
