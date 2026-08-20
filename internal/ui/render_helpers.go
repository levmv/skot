package ui

import "github.com/charmbracelet/x/ansi"

func visibleLen(text string) int {
	return ansi.StringWidth(text)
}

func truncateANSI(text string, width int) string {
	if width <= 0 || visibleLen(text) <= width {
		return text
	}
	return ansi.Truncate(text, width, "")
}

// hangingLines wraps body after equally wide first-line and continuation
// prefixes. The marker appears only on the first line.
func (m screenModel) hangingLines(marker, firstPrefix, continuationPrefix, body string) []string {
	wrapped := wrapDisplayLine(body, max(1, m.contentWidth()-visibleLen(continuationPrefix)))
	lines := make([]string, 0, len(wrapped))
	for index, line := range wrapped {
		lineMarker, prefix := " ", continuationPrefix
		if index == 0 {
			lineMarker, prefix = marker, firstPrefix
		}
		lines = append(lines, m.marked(lineMarker, prefix+line))
	}
	return lines
}
