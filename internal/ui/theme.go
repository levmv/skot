package ui

import (
	"image/color"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/levmv/skot/internal/state"
)

const (
	ThemeAuto  = state.ThemeAuto
	ThemeLight = state.ThemeLight
	ThemeDark  = state.ThemeDark
)

type terminalPalette struct {
	muted          color.Color
	accent         color.Color
	error          color.Color
	success        color.Color
	code           color.Color
	userForeground color.Color
	userBackground color.Color
}

func terminalPaletteFor(dark bool) terminalPalette {
	if dark {
		return terminalPalette{
			muted:          lipgloss.ANSIColor(8),
			accent:         lipgloss.ANSIColor(14),
			error:          lipgloss.ANSIColor(9),
			success:        lipgloss.ANSIColor(10),
			code:           lipgloss.ANSIColor(11),
			userForeground: lipgloss.ANSIColor(14),
			userBackground: lipgloss.ANSIColor(8),
		}
	}
	return terminalPalette{
		muted:          lipgloss.ANSIColor(8),
		accent:         lipgloss.ANSIColor(6),
		error:          lipgloss.ANSIColor(1),
		success:        lipgloss.ANSIColor(2),
		code:           lipgloss.ANSIColor(5),
		userForeground: lipgloss.Color("242"),
		userBackground: lipgloss.Color("255"),
	}
}

func normalizeTheme(value string) (string, error) {
	return state.NormalizeTheme(value)
}

func (m *screenModel) switchTerminalTheme(value string) (tea.Cmd, error) {
	theme, err := normalizeTheme(value)
	if err != nil {
		return nil, err
	}
	if err := m.agent.SwitchTheme(theme); err != nil {
		return nil, err
	}
	m.theme = theme
	m.themeQuery++
	if theme == ThemeAuto {
		// Establish the documented fallback before waiting for OSC 11, so a
		// missing response cannot leave the previously selected dark palette.
		m.applyTerminalTheme(false)
		m.themePending = true
		return queryTerminalTheme(m.themeQuery), nil
	}
	m.themePending = false
	m.applyTerminalTheme(theme == ThemeDark)
	return nil, nil
}

func (m *screenModel) applyTerminalTheme(dark bool) {
	m.darkTheme = dark
	palette := terminalPaletteFor(dark)
	m.mutedStyle = lipgloss.NewStyle()
	m.accentStyle = lipgloss.NewStyle()
	m.errorStyle = lipgloss.NewStyle()
	m.successStyle = lipgloss.NewStyle()
	m.userGutterStyle = lipgloss.NewStyle()
	if m.useStyle {
		m.mutedStyle = m.mutedStyle.Foreground(palette.muted)
		m.accentStyle = m.accentStyle.Foreground(palette.accent).Bold(true)
		m.errorStyle = m.errorStyle.Foreground(palette.error)
		m.successStyle = m.successStyle.Foreground(palette.success)
		m.userGutterStyle = m.userGutterStyle.Foreground(palette.userForeground).Background(palette.userBackground).Bold(true)
	}
	m.markdown = newMarkdownRenderer(m.useStyle, palette)
	// Transcript lines are cached after styling, so a palette change must
	// invalidate the cache before the first themed frame is rendered.
	m.transcript.invalidate()
}
