package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
)

type sessionAction uint8

const (
	sessionActionNone sessionAction = iota
	sessionActionClear
	sessionActionExit
)

func (m *screenModel) startSessionAction(action sessionAction) tea.Cmd {
	m.pendingSessionAction = action
	if m.operation.isTurn() {
		m.cancelTurn()
	}
	if m.scope.cancel != nil {
		// Leaving this session also dismisses any path prompt the cancelled
		// change would otherwise restore.
		m.scope.prompt = notFilesystemPath
		m.scope.cancel()
	}
	return m.continueSessionAction()
}

func (m *screenModel) continueSessionAction() tea.Cmd {
	// A scope/path change may outlive the cancelled turn. Both must finish
	// before the session is replaced or the application closes.
	if m.pendingSessionAction == sessionActionNone || m.operation.kind != operationNone || m.scope.pending {
		return nil
	}
	switch m.pendingSessionAction {
	case sessionActionClear:
		return m.startClearSession()
	case sessionActionExit:
		m.pendingSessionAction = sessionActionNone
		return m.quit(nil)
	}
	return nil
}

type sessionClearedMsg struct {
	id      string
	notices []string
	err     error
}

func (m *screenModel) startClearSession() tea.Cmd {
	m.operation = activeOperation{kind: operationClear}
	client, ctx := m.agent, m.ctx
	return func() tea.Msg {
		noticeCount := len(client.StartupNotices())
		id, err := client.ClearSession(ctx)
		var addedNotices []string
		if notices := client.StartupNotices(); noticeCount < len(notices) {
			addedNotices = notices[noticeCount:]
		}
		return sessionClearedMsg{id: id, notices: addedNotices, err: err}
	}
}

func (m *screenModel) finishClearSession(message sessionClearedMsg) {
	m.operation.clear()
	m.pendingSessionAction = sessionActionNone
	if message.err != nil {
		m.addBlock(screenBlockError, "clear session: "+message.err.Error())
		return
	}
	m.resetTranscript()
	m.composer.resetHistory()
	m.refreshModelChoices()
	m.refreshSessionStatus()
	m.addBlock(screenBlockSystem, "new session "+app.ShortSessionID(message.id))
	for _, notice := range message.notices {
		m.addBlock(screenBlockError, "clear warning: "+notice)
	}
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
	elapsed := max(now.Sub(updated), 0)
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
