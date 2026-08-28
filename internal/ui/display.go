package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
)

const (
	DisplayCompact  = state.DisplayCompact
	DisplayDetailed = state.DisplayDetailed
	DisplayFull     = state.DisplayFull
)

func normalizeDisplayProfile(value string) (string, error) {
	return state.NormalizeDisplayProfile(value)
}

func (m *screenModel) switchTranscriptDisplay(value string) error {
	profile, err := normalizeDisplayProfile(value)
	if err != nil {
		return err
	}
	changed := profile != m.displayProfile
	switchErr := m.agent.SwitchDisplayProfile(profile)
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		return switchErr
	}
	m.displayProfile = profile
	if !changed {
		return switchErr
	}
	m.frameRowFloor = 0
	// A profile changes the presentation of history which may already be in
	// terminal scrollback. The transcript cache and terminal frame therefore
	// become stale as one unit.
	m.transcript.invalidate()
	m.renderer.Invalidate()
	return switchErr
}

func (m *screenModel) switchTranscriptDisplayFromKey(profile string) {
	if profile == m.displayProfile {
		return
	}
	err := m.switchTranscriptDisplay(profile)
	if err != nil && !preferenceAppliedDespiteError(err) {
		m.addBlock(screenBlockError, "display: "+err.Error())
	} else {
		m.addBlock(screenBlockSystem, m.displayNotice(m.displayProfile))
		if err != nil {
			m.addBlock(screenBlockError, "display: "+err.Error())
		}
	}
	m.refreshTranscript()
}

func (m screenModel) displayNotice(profile string) string {
	notice := "display: " + profile
	if hint := m.displayShortcutHint(); hint != "" {
		notice += " · " + hint
	}
	return notice
}

func (m screenModel) displayShortcutHint() string {
	var hints []string
	if direct := m.directDisplayShortcutLabel(); direct != "" {
		hints = append(hints, direct+" select")
	}
	if relative := m.relativeDisplayShortcutLabel(); relative != "" {
		hints = append(hints, relative+" adjust")
	}
	return strings.Join(hints, " · ")
}

func (m screenModel) directDisplayShortcutLabel() string {
	if !m.keyboard.supportsKeyDisambiguation() {
		return ""
	}
	compact := m.keymap.helpFor(actionDisplayCompact)
	detailed := strings.TrimPrefix(m.keymap.helpFor(actionDisplayDetailed), "ctrl+")
	full := strings.TrimPrefix(m.keymap.helpFor(actionDisplayFull), "ctrl+")
	if compact == "" || detailed == "" || full == "" {
		return ""
	}
	return strings.Join([]string{compact, detailed, full}, "/")
}

func (m screenModel) relativeDisplayShortcutLabel() string {
	more := m.keymap.helpFor(actionDisplayMore)
	less := strings.TrimPrefix(m.keymap.helpFor(actionDisplayLess), "ctrl+")
	if more == "" || less == "" {
		return ""
	}
	return more + "/" + less
}

func (m screenModel) moreDetailShortcutLabel() string {
	if more := m.keymap.helpFor(actionDisplayMore); more != "" {
		return more
	}
	if m.keyboard.supportsKeyDisambiguation() {
		if detailed := m.keymap.helpFor(actionDisplayDetailed); detailed != "" {
			return detailed
		}
	}
	return "/display"
}

func (m *screenModel) shiftTranscriptDisplay(delta int) {
	profiles := []string{DisplayCompact, DisplayDetailed, DisplayFull}
	for index, profile := range profiles {
		if profile != m.displayProfile {
			continue
		}
		target := min(max(0, index+delta), len(profiles)-1)
		m.switchTranscriptDisplayFromKey(profiles[target])
		return
	}
}

// fitCompactToolTail keeps every row that may still be rewritten inside the
// physical viewport. Once an older row would cross that boundary, it joins the
// cumulative summary before the overflowing frame reaches the terminal. This
// preserves native scrollback while leaving the newest activity observable.
func (m *screenModel) fitCompactToolTail() {
	if m.frameRowFloor > 0 && m.baseInlineFrameRows() >= m.frameRowFloor {
		m.frameRowFloor = 0
	}
	if m.displayProfile != DisplayCompact || m.height <= 0 {
		return
	}
	for {
		start, _, ok := m.transcript.modelOwnedToolTail()
		if !ok {
			return
		}
		startLine := 0
		if start > 0 {
			startLine = m.transcript.renderCache[start-1].end
		}
		mutableRows := len(m.transcript.lines) - startLine + len(m.inlineFrame().dynamic)
		if mutableRows <= m.height || !m.transcript.foldOldestToolTailBlock() {
			return
		}
		m.transcript.refresh(m.contentWidth(), m.renderBlockLinesAt)
	}
}

func (m screenModel) renderBlockLinesAt(index int, block screenBlock) []string {
	if m.displayProfile == DisplayFull && modelOwnedToolBlock(block) {
		return m.renderFullToolBlock(block)
	}
	if m.displayProfile == DisplayCompact && compactLiveBash(block) {
		return m.renderCompactLiveTool(block)
	}
	if m.displayProfile == DisplayCompact && index >= 0 && compactCollapsedTool(block) {
		var lines []string
		if index == 0 || !compactCollapsedTool(m.transcript.blocks[index-1]) {
			end := m.compactToolGroupEnd(index)
			lines = append(lines, m.renderCompactToolSummary(index, end)...)
		}
		if compactToolStaysExpanded(block) {
			lines = append(lines, m.renderCompactLiveTool(block)...)
		}
		return lines
	}
	if index >= 0 && toolPresentationGroupMember(block) {
		start, end := m.toolPresentationGroup(index)
		if index != start {
			return nil
		}
		return m.renderToolPresentationGroup(start, end)
	}
	return m.renderBlockLines(block)
}

func (m screenModel) toolPresentationGroup(index int) (int, int) {
	start, end := index, index
	member := m.transcript.blocks[index]
	key := member.tool.group.key
	for start > 0 {
		previous := m.transcript.blocks[start-1]
		if !toolPresentationGroupMember(previous) || previous.tool.group.key != key {
			break
		}
		if m.displayProfile == DisplayCompact && previous.tool.collapsed != member.tool.collapsed {
			break
		}
		start--
	}
	for end+1 < len(m.transcript.blocks) {
		next := m.transcript.blocks[end+1]
		if !toolPresentationGroupMember(next) || next.tool.group.key != key {
			break
		}
		if m.displayProfile == DisplayCompact && next.tool.collapsed != member.tool.collapsed {
			break
		}
		end++
	}
	return start, end
}

func toolPresentationGroupMember(block screenBlock) bool {
	if block.kind != screenBlockTool || block.tool == nil || block.tool.group == nil || block.tool.failed {
		return false
	}
	group := block.tool.group
	plain := sanitizeTerminalText(formatReadGroup(group.dir, []string{group.item}))
	return block.text == plain
}

func (m screenModel) renderToolPresentationGroup(start, end int) []string {
	block := m.transcript.blocks[start]
	tool := *block.tool
	block.tool = &tool
	items := make([]string, 0, end-start+1)
	tool.done = true
	tool.failed = false
	tool.elapsed = 0
	for index := start; index <= end; index++ {
		member := m.transcript.blocks[index].tool
		items = append(items, member.group.item)
		tool.done = tool.done && member.done
		tool.failed = tool.failed || member.failed
		tool.elapsed = max(tool.elapsed, member.elapsed)
	}
	block.text = sanitizeTerminalText(formatReadGroup(tool.group.dir, items))
	return m.renderBlockLines(block)
}

func compactCollapsedTool(block screenBlock) bool {
	return modelOwnedToolBlock(block) && block.tool.collapsed
}

func compactLiveBash(block screenBlock) bool {
	return modelOwnedToolBlock(block) && !block.tool.collapsed && block.tool.name == "bash"
}

func (m screenModel) compactToolGroupEnd(index int) int {
	end := index
	for end+1 < len(m.transcript.blocks) && compactCollapsedTool(m.transcript.blocks[end+1]) {
		end++
	}
	return end
}

func (m screenModel) renderCompactToolSummary(start, end int) []string {
	tools := 0
	changedPaths := make(map[string]struct{})
	for index := start; index <= end; index++ {
		tool := m.transcript.blocks[index].tool
		tools++
		if tool.fileChange != nil {
			if path := strings.TrimSpace(tool.fileChange.Path); path != "" {
				changedPaths[path] = struct{}{}
			}
		}
	}

	summary := fmt.Sprintf("Used %d %s", tools, plural(tools, "tool", "tools"))
	if changed := len(changedPaths); changed > 0 {
		summary += fmt.Sprintf(" · changed %d %s", changed, plural(changed, "file", "files"))
	}
	if duration := formatCollapsedToolDuration(m.transcript.blocks[end].tool.collapsedDuration); duration != "" {
		summary += " · " + duration
	}
	styled := m.summaryStyle.Render(summary)
	if m.firstCompactToolSummary(start) {
		styled += m.mutedStyle.Render("  (" + m.moreDetailShortcutLabel() + " for more detail)")
	}
	return m.wrappedMarked(m.mutedStyle.Render("•"), styled)
}

func (m screenModel) firstCompactToolSummary(start int) bool {
	for index := 0; index < start; index++ {
		if compactCollapsedTool(m.transcript.blocks[index]) {
			return false
		}
	}
	return true
}

func compactToolStaysExpanded(block screenBlock) bool {
	tool := block.tool
	return tool != nil && (!tool.done || tool.failed || tool.process != nil && tool.process.Status == agent.ProcessRunning)
}

func formatCollapsedToolDuration(duration time.Duration) string {
	if duration <= 0 {
		return ""
	}
	if duration < time.Second {
		return "<1s"
	}
	return formatToolDuration(duration)
}

func (m screenModel) renderCompactLiveTool(block screenBlock) []string {
	if block.tool == nil || block.tool.name != "bash" {
		return m.renderBlockLines(block)
	}
	copy := block
	limit := min(80, max(1, m.contentWidth()))
	detail := ""
	if block.tool.process != nil {
		detail = processStatusText(*block.tool.process)
	} else if block.tool.done {
		detail = formatToolDuration(block.tool.elapsed)
	}
	if detail != "" {
		limit = min(limit, max(1, m.contentWidth()-visibleLen(detail)-2))
	}
	copy.text = truncateToolDisplay(block.text, limit)
	return m.renderBlockLines(copy)
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func (m screenModel) renderFullToolBlock(block screenBlock) []string {
	tool := block.tool
	copy := block
	copy.text = fullToolCallText(block.text, tool)

	var lines []string
	switch {
	case tool.process != nil:
		lines = m.renderProcessResultLines(copy)
	case tool.fileChange != nil:
		lines = m.renderFileChangeBlock(copy)
		lines = append(lines, m.renderFullToolResult(tool.resultText)...)
	default:
		detail := ""
		if tool.done {
			detail = formatToolDuration(tool.elapsed)
		}
		lines = m.renderToolSummaryLines(m.toolMarker(tool.failed), copy.text, detail)
		lines = append(lines, m.renderFullToolResult(tool.resultText)...)
	}
	return lines
}

func fullToolCallText(fallback string, tool *toolBlock) string {
	name := compactSingleLine(tool.name, 0)
	if name == "" {
		name = strings.TrimSpace(sanitizeTerminalText(fallback))
		if name == "" {
			name = "tool"
		}
	}
	arguments := strings.TrimSpace(sanitizeTerminalText(tool.rawArguments))
	if arguments == "" {
		return name
	}
	return name + "  " + arguments
}

func (m screenModel) renderFullToolResult(result string) []string {
	return m.renderFullProcessOutput(result, processOutputIndentWidth)
}

func (m screenModel) renderFileChangeBlock(block screenBlock) []string {
	tool := block.tool
	change := *tool.fileChange
	if m.displayProfile == DisplayCompact && tool.collapsed && !tool.failed {
		change.Hunks = nil
		change.Truncated = false
	} else if m.displayProfile == DisplayCompact {
		change = limitFileChangeForDisplay(change, 12)
	}
	return m.renderFileChangeLines(block.text, change, m.toolMarker(tool.failed))
}

func limitFileChangeForDisplay(change fileChangeMeta, lineLimit int) fileChangeMeta {
	if lineLimit <= 0 {
		change.Hunks = nil
		change.Truncated = len(change.Hunks) > 0 || change.Truncated
		return change
	}
	remaining := lineLimit
	var hunks []agent.FileDiffHunk
	truncated := change.Truncated
	for hunkIndex, hunk := range change.Hunks {
		if remaining == 0 {
			truncated = true
			break
		}
		copy := hunk
		if len(copy.Lines) > remaining {
			copy.Lines = append([]agent.FileDiffLine(nil), copy.Lines[:remaining]...)
			truncated = true
		} else {
			copy.Lines = append([]agent.FileDiffLine(nil), copy.Lines...)
		}
		hunks = append(hunks, copy)
		remaining -= len(copy.Lines)
		if hunkIndex+1 < len(change.Hunks) && remaining == 0 {
			truncated = true
		}
	}
	change.Hunks = hunks
	change.Truncated = truncated
	return change
}
