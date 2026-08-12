package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

func NormalizeTheme(value string) (string, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "", ThemeAuto:
		return ThemeAuto, nil
	case ThemeLight, ThemeDark:
		return value, nil
	default:
		return "", fmt.Errorf("invalid terminal theme %q; expected auto, light, or dark", value)
	}
}

func (m *screenModel) applyTerminalTheme(dark bool) {
	m.darkTheme = dark
	m.mutedStyle = lipgloss.NewStyle()
	m.accentStyle = lipgloss.NewStyle()
	m.errorStyle = lipgloss.NewStyle()
	m.successStyle = lipgloss.NewStyle()
	if m.useStyle {
		m.mutedStyle = m.mutedStyle.Foreground(lipgloss.ANSIColor(8))
		if dark {
			m.accentStyle = m.accentStyle.Foreground(lipgloss.ANSIColor(14)).Bold(true)
			m.errorStyle = m.errorStyle.Foreground(lipgloss.ANSIColor(9))
			m.successStyle = m.successStyle.Foreground(lipgloss.ANSIColor(10))
		} else {
			m.accentStyle = m.accentStyle.Foreground(lipgloss.ANSIColor(6)).Bold(true)
			m.errorStyle = m.errorStyle.Foreground(lipgloss.ANSIColor(1))
			m.successStyle = m.successStyle.Foreground(lipgloss.ANSIColor(2))
		}
	}
	// Transcript lines are cached after styling, so a palette change must
	// invalidate the cache before the first themed frame is rendered.
	m.transcript.invalidate()
}
