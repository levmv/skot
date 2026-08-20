package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/levmv/skot/agent"
)

type processResultMeta = agent.ProcessResult

const (
	maxModelProcessPreviewLines = 6
	processOutputIndentWidth    = 2
)

func (m screenModel) renderProcessResultLines(block screenBlock) []string {
	tool := block.tool
	result := *tool.process
	lines := m.renderToolSummaryLines(m.toolMarker(tool.failed), block.text, processStatusText(result))
	output := tool.output
	userOwned := tool.shell != nil || result.UserInitiated
	_, _, outputIndent, _ := toolCommandPrefix(block.text)
	if output == "" && !userOwned && result.Status != agent.ProcessCompleted && result.Status != agent.ProcessRunning {
		output = result.FailureTail
	}
	if userOwned {
		lines = append(lines, m.renderFullProcessOutput(output, outputIndent)...)
	} else {
		lines = append(lines, m.renderModelProcessPreview(output, outputIndent)...)
	}
	return lines
}

func (m screenModel) renderFullProcessOutput(output string, indent int) []string {
	output = sanitizeTerminalText(output)
	if output == "" {
		return nil
	}
	var lines []string
	for _, outputLine := range strings.Split(output, "\n") {
		lines = append(lines, m.renderProcessOutputLine(outputLine, indent, true)...)
	}
	return lines
}

func (m screenModel) renderModelProcessPreview(output string, indent int) []string {
	preview := processOutputPreviewLines(sanitizeTerminalText(output), maxModelProcessPreviewLines)
	lines := make([]string, 0, len(preview))
	for _, outputLine := range preview {
		lines = append(lines, m.renderProcessOutputLine(outputLine, indent, false)...)
	}
	return lines
}

func (m screenModel) renderProcessOutputLine(outputLine string, indent int, wrap bool) []string {
	width := max(1, m.contentWidth()-indent)
	prefix := strings.Repeat(" ", indent)
	if wrap {
		return m.hangingLines(" ", prefix, prefix, m.mutedStyle.Render(outputLine))
	}
	return []string{m.marked(" ", prefix+m.mutedStyle.Render(ansi.Truncate(outputLine, width, "…")))}
}

func processOutputPreviewLines(output string, limit int) []string {
	output = strings.TrimRight(output, "\n")
	if output == "" || limit <= 0 {
		return nil
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= limit {
		return lines
	}
	if limit == 1 {
		return []string{omittedOutputLabel(len(lines))}
	}
	head := (limit - 1) / 2
	tail := limit - head - 1
	omitted := len(lines) - head - tail
	preview := make([]string, 0, limit)
	preview = append(preview, lines[:head]...)
	preview = append(preview, omittedOutputLabel(omitted))
	preview = append(preview, lines[len(lines)-tail:]...)
	return preview
}

func omittedOutputLabel(lines int) string {
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	return fmt.Sprintf("… +%d %s", lines, unit)
}

func processStatusText(result processResultMeta) string {
	parts := []string{formatDuration(time.Duration(result.DurationMillis) * time.Millisecond)}
	if result.Status == agent.ProcessRunning {
		parts = append(parts, "running")
		if result.JobID != "" {
			parts = append(parts, result.JobID)
		}
		return strings.Join(parts, " · ")
	}
	switch result.Status {
	case agent.ProcessTimedOut:
		parts = append(parts, "timed out")
	case agent.ProcessKilled:
		parts = append(parts, "killed")
	case agent.ProcessFailed:
		if result.ExitCode == nil {
			parts = append(parts, "failed")
		}
	case agent.ProcessNotStarted:
		parts = append(parts, "not started")
	case agent.ProcessAbandoned:
		parts = append(parts, "abandoned")
	case agent.ProcessUnknown:
		parts = append(parts, "status unavailable")
	}
	if result.ManagedProcesses > 1 {
		parts = append(parts, fmt.Sprintf("%d managed processes", result.ManagedProcesses))
	}
	if result.ExitCode != nil && *result.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", *result.ExitCode))
	}
	if result.OutputBytes > 0 {
		parts = append(parts, formatByteCount(result.OutputBytes))
	}
	if result.DiscardedBytes > 0 {
		parts = append(parts, fmt.Sprintf("%s discarded", formatByteCount(result.DiscardedBytes)))
	}
	return strings.Join(parts, " · ")
}

func (m *screenModel) refreshProcessResults() bool {
	viewportTop := 0
	viewportKnown := m.renderer != nil
	if viewportKnown {
		viewportTop = m.renderer.previousViewportTop
	}
	return m.transcript.refreshProcessResults(m.agent.ToolStatus, viewportTop, viewportKnown)
}

func (transcript *transcriptState) refreshProcessResults(status func(string) ([]agent.Detail, bool), viewportTop int, viewportKnown bool) bool {
	changed := false
	blockCount := len(transcript.blocks)
	for index := range blockCount {
		block := &transcript.blocks[index]
		if block.tool == nil || block.tool.process == nil || block.tool.superseded || block.tool.process.Status != agent.ProcessRunning || block.tool.process.JobID == "" {
			continue
		}
		tool := block.tool
		details, ok := status(tool.process.JobID)
		if !ok {
			if !transcript.blockAboveViewport(index, viewportTop, viewportKnown) {
				unknown := *tool.process
				unknown.Status = agent.ProcessUnknown
				unknown.FailureTail = "job state is unavailable in this Skot process"
				transcript.markBlockDirty(index)
				tool.process = &unknown
				tool.failed = true
				changed = true
			} else {
				tool.superseded = true
			}
			continue
		}
		for _, detail := range details {
			result, ok := agent.ProcessResultFromDetail(detail)
			if !ok {
				continue
			}
			if !transcript.blockAboveViewport(index, viewportTop, viewportKnown) {
				if *tool.process != result {
					transcript.markBlockDirty(index)
					tool.process = &result
					tool.elapsed = time.Duration(result.DurationMillis) * time.Millisecond
					tool.failed = result.Status != agent.ProcessCompleted && result.Status != agent.ProcessRunning
					changed = true
				}
				continue
			}
			if result.Status == agent.ProcessRunning {
				continue
			}
			tool.superseded = true
			if transcript.processCompletionRepresented(result.JobID) {
				continue
			}
			copy := *block
			copyTool := *tool
			copy.tool = &copyTool
			copyTool.process = &result
			copyTool.superseded = false
			copyTool.elapsed = time.Duration(result.DurationMillis) * time.Millisecond
			copyTool.failed = result.Status != agent.ProcessCompleted
			transcript.appendBlock(copy)
			changed = true
		}
	}
	return changed
}

func (m screenModel) hasRunningProcesses() bool {
	return m.transcript.hasRunningProcesses()
}

func (transcript transcriptState) hasRunningProcesses() bool {
	for _, block := range transcript.blocks {
		if block.tool != nil && block.tool.process != nil && !block.tool.superseded && block.tool.process.Status == agent.ProcessRunning && block.tool.process.JobID != "" {
			return true
		}
	}
	return false
}

func (transcript transcriptState) blockAboveViewport(index, viewportTop int, viewportKnown bool) bool {
	if !viewportKnown || index < 0 || index >= len(transcript.renderCache) {
		return false
	}
	return transcript.renderCache[index].end <= viewportTop
}

func (transcript transcriptState) processCompletionRepresented(jobID string) bool {
	for _, block := range transcript.blocks {
		if block.tool != nil && block.tool.process != nil && block.tool.process.JobID == jobID && block.tool.process.Status != agent.ProcessRunning {
			return true
		}
	}
	return false
}

func formatByteCount(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

func processOutputFromContent(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if _, output, found := strings.Cut(content, "\n\n"); found {
		return strings.TrimSuffix(output, "\n")
	}
	return ""
}
