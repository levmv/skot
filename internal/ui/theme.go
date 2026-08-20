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
	muted   color.Color
	accent  color.Color
	error   color.Color
	success color.Color
	// warning sits between muted and error: it calls attention to a state or a
	// cost without presenting it as a failure.
	warning color.Color
	// userBar marks user messages and should sit below muted text: it is a
	// scanning aid, not something to read, so it fades toward the background
	// rather than away from it.
	userBar color.Color
}

func terminalPaletteFor(dark bool) terminalPalette {
	if dark {
		return terminalPalette{
			muted:   lipgloss.ANSIColor(8),
			accent:  lipgloss.ANSIColor(14),
			error:   lipgloss.ANSIColor(9),
			success: lipgloss.ANSIColor(10),
			warning: lipgloss.ANSIColor(11),
			userBar: lipgloss.Color("238"),
		}
	}
	return terminalPalette{
		muted:   lipgloss.ANSIColor(8),
		accent:  lipgloss.ANSIColor(6),
		error:   lipgloss.ANSIColor(1),
		success: lipgloss.ANSIColor(2),
		warning: lipgloss.ANSIColor(3),
		userBar: lipgloss.Color("250"),
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
	switchErr := m.agent.SwitchTheme(theme)
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		return nil, switchErr
	}
	m.theme = theme
	m.themeQuery++
	if theme == ThemeAuto {
		// Establish the documented fallback before waiting for OSC 11, so a
		// missing response cannot leave the previously selected palette in place.
		m.applyTerminalTheme(true)
		m.themePending = true
		return queryTerminalTheme(m.themeQuery), switchErr
	}
	m.themePending = false
	m.applyTerminalTheme(theme == ThemeDark)
	return nil, switchErr
}

func (m *screenModel) applyTerminalTheme(dark bool) {
	m.darkTheme = dark
	palette := terminalPaletteFor(dark)
	m.mutedStyle = lipgloss.NewStyle()
	m.accentStyle = lipgloss.NewStyle()
	m.errorStyle = lipgloss.NewStyle()
	m.successStyle = lipgloss.NewStyle()
	m.warningStyle = lipgloss.NewStyle()
	m.userBarStyle = lipgloss.NewStyle()
	if m.useStyle {
		m.mutedStyle = m.mutedStyle.Foreground(palette.muted)
		m.accentStyle = m.accentStyle.Foreground(palette.accent).Bold(true)
		m.errorStyle = m.errorStyle.Foreground(palette.error)
		m.successStyle = m.successStyle.Foreground(palette.success)
		m.warningStyle = m.warningStyle.Foreground(palette.warning)
		m.userBarStyle = m.userBarStyle.Foreground(palette.userBar)
	}
	m.markdown = newMarkdownRenderer(m.useStyle, palette)
	// Transcript lines are cached after styling, so a palette change must
	// invalidate the cache before the first themed frame is rendered.
	m.transcript.invalidate()
}
