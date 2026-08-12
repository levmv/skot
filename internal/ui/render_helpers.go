package ui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func visibleLen(text string) int {
	return lipgloss.Width(text)
}

func truncateANSI(text string, width int) string {
	if width <= 0 || visibleLen(text) <= width {
		return text
	}
	return ansi.Truncate(text, width, "")
}
