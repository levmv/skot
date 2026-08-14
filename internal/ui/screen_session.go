package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
)

func (m *screenModel) clearSession() {
	id, err := m.agent.ClearSession(m.ctx)
	if err != nil {
		m.addBlock(screenBlockError, "clear session: "+err.Error())
		return
	}
	m.resetTranscript()
	m.composer.resetHistory()
	m.refreshModelChoices()
	m.addBlock(screenBlockSystem, "new session "+app.ShortSessionID(id))
}

func (m *screenModel) openSessionPicker() {
	summaries, err := m.agent.ListSessions()
	if err != nil {
		m.addBlock(screenBlockError, "list sessions: "+err.Error())
		return
	}
	items := make([]pickerItem, 0, min(20, len(summaries)))
	now := time.Now()
	current := m.agent.SessionID()
	for _, summary := range summaries {
		if summary.ID == current {
			continue
		}
		items = append(items, pickerItem{
			value:       summary.ID,
			label:       sessionDisplayTitle(summary),
			description: relativeSessionTime(now, summary.UpdatedAt),
		})
		if len(items) == 20 {
			break
		}
	}
	if len(items) == 0 {
		m.composer.reset()
		m.addBlock(screenBlockSystem, "no other sessions")
		return
	}
	m.openPicker(pickerSession, items, 0)
}

func (m *screenModel) resumeSession(idOrPrefix string) {
	noticeCount := len(m.agent.StartupNotices())
	id, err := m.agent.ResumeSession(m.ctx, idOrPrefix)
	if err != nil {
		m.addBlock(screenBlockError, "resume session: "+err.Error())
		return
	}
	m.refreshModelChoices()
	m.continueTranscriptBelow()
	m.addBlock(screenBlockSystem, "resumed session "+app.ShortSessionID(id))
	if notices := m.agent.StartupNotices(); noticeCount < len(notices) {
		for _, notice := range notices[noticeCount:] {
			m.addBlock(screenBlockError, "resume warning: "+notice)
		}
	}
	if err := m.loadSessionHistory(); err != nil {
		m.addBlock(screenBlockError, "history: "+err.Error())
	}
	m.openStartupLoginPicker()
}

func resumeSessionCmd(idOrPrefix string) tea.Cmd {
	return func() tea.Msg { return resumeSessionMsg{idOrPrefix: idOrPrefix} }
}

func sessionDisplayTitle(summary SessionSummary) string {
	if title := strings.TrimSpace(summary.Title); title != "" {
		return title
	}
	return "untitled session " + app.ShortSessionID(summary.ID)
}

func relativeSessionTime(now, updated time.Time) string {
	if updated.IsZero() {
		return "unknown time"
	}
	elapsed := now.Sub(updated)
	if elapsed < 0 {
		elapsed = 0
	}
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed/time.Minute))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed/time.Hour))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed/(24*time.Hour)))
	case updated.Local().Year() == now.Local().Year():
		return updated.Local().Format("Jan 2")
	default:
		return updated.Local().Format("Jan 2, 2006")
	}
}
