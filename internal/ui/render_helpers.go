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
