package ui

import (
	"fmt"
	"strings"

	workspacetools "github.com/levmv/skot/tools"
)

type fileChangeMeta = workspacetools.FileChangeMeta

func (m screenModel) renderToolDisplay(text string) string {
	name, arguments := splitToolDisplay(text)
	return m.accentStyle.Render(name) + arguments
}

func (m screenModel) renderFileChangeLines(summary string, change fileChangeMeta, marker string) []string {
	stats := make([]string, 0, 2)
	if change.Additions > 0 {
		stats = append(stats, m.successStyle.Render(fmt.Sprintf("+%d", change.Additions)))
	}
	if change.Deletions > 0 {
		stats = append(stats, m.errorStyle.Render(fmt.Sprintf("−%d", change.Deletions)))
	}
	name, arguments := splitToolDisplay(summary)
	styledSummary := m.accentStyle.Render(name)
	if arguments = strings.TrimSpace(arguments); arguments != "" {
		styledSummary += " " + arguments
	}
	if len(stats) > 0 {
		styledSummary += " (" + strings.Join(stats, " ") + ")"
	} else {
		styledSummary += " (" + m.mutedStyle.Render("no changes") + ")"
	}
	lines := m.wrappedMarked(marker, styledSummary)

	numberWidth := diffNumberWidth(change)
	for hunkIndex, hunk := range change.Hunks {
		if hunkIndex > 0 {
			separator := m.mutedStyle.Render(fmt.Sprintf("%*s", numberWidth, "⋮"))
			lines = append(lines, m.marked(" ", separator))
		}
		for _, line := range hunk.Lines {
			lines = append(lines, m.renderFileDiffLine(line, numberWidth)...)
		}
	}
	if change.Truncated {
		detail := "diff preview limited"
		if change.TotalHunks > len(change.Hunks) {
			detail = fmt.Sprintf("diff preview limited · %d hunks total", change.TotalHunks)
		}
		lines = append(lines, m.wrappedMarked("…", m.mutedStyle.Render(detail))...)
	}
	return lines
}

func (m screenModel) renderFileDiffLine(line workspacetools.FileDiffLine, numberWidth int) []string {
	number := diffDisplayLineNumber(line)
	sign := " "
	if line.Kind == "add" {
		sign = m.successStyle.Render("+")
	} else if line.Kind == "delete" {
		sign = m.errorStyle.Render("−")
	}

	gutter := m.mutedStyle.Render(fmt.Sprintf("%*s", numberWidth, diffLineNumber(number))) + " " + sign + "  "
	indent := strings.Repeat(" ", visibleLen(gutter))
	content := sanitizeTerminalText(line.Text)
	if line.NoNewline {
		content += "  [no newline]"
	}
	wrapped := wrapDisplayLine(content, max(1, m.contentWidth()-visibleLen(gutter)))
	lines := make([]string, 0, len(wrapped))
	for index, part := range wrapped {
		prefix := indent
		if index == 0 {
			prefix = gutter
		}
		if line.Kind != "add" {
			part = m.mutedStyle.Render(part)
		}
		lines = append(lines, m.marked(" ", prefix+part))
	}
	return lines
}

func diffNumberWidth(change fileChangeMeta) int {
	width := 1
	for _, hunk := range change.Hunks {
		for _, line := range hunk.Lines {
			width = max(width, len(diffLineNumber(diffDisplayLineNumber(line))))
		}
	}
	return width
}

func diffDisplayLineNumber(line workspacetools.FileDiffLine) int {
	if line.Kind == "delete" {
		return line.OldLine
	}
	return line.NewLine
}

func diffLineNumber(value int) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
