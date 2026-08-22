package ui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type scopeSwitchState struct {
	pending   bool
	startedAt time.Time
	cancel    context.CancelFunc
	// previous is the scope in force when the switch started, kept so the
	// result can name where it came from once the async switch lands.
	previous string
}

func (m *screenModel) startScopeSwitch(scope string) tea.Cmd {
	if m.scope.pending {
		m.addBlock(screenBlockError, "scope: a switch is already in progress")
		return nil
	}
	operationCtx, cancel := context.WithCancel(m.ctx)
	previous := m.agent.CurrentScope()
	m.scope = scopeSwitchState{pending: true, startedAt: time.Now(), cancel: cancel, previous: previous}
	return func() tea.Msg {
		err := m.agent.SwitchScope(operationCtx, scope)
		current := m.agent.CurrentScope()
		notice := ""
		if (err == nil || preferenceAppliedDespiteError(err)) && current != previous {
			notice = m.agent.ScopeNotice()
		}
		return scopeDoneMsg{
			scope:   current,
			summary: m.agent.ScopeSummary(),
			notice:  notice,
			err:     err,
		}
	}
}

func (m *screenModel) finishScopeSwitch(message scopeDoneMsg) {
	if m.scope.cancel != nil {
		m.scope.cancel()
	}
	previous := m.scope.previous
	m.scope = scopeSwitchState{}
	if message.err != nil && !preferenceAppliedDespiteError(message.err) {
		m.addBlock(screenBlockError, "scope: "+message.err.Error())
		return
	}
	text := formatSettingChange("filesystem scope", previous, message.scope)
	if detail := scopeSummaryDetail(message.scope, message.summary); detail != "" {
		text += "\n" + detail
	}
	if message.notice != "" {
		text += "\nwarning: " + message.notice
	}
	m.addBlock(screenBlockScopeChange, text)
	if message.err != nil {
		m.addBlock(screenBlockError, "scope: "+message.err.Error())
	}
}

// scopeSummaryDetail removes the headline already carried by the transition.
// ScopeSummary is also used on startup and therefore remains self-contained;
// the switch result needs only its optional policy details.
func scopeSummaryDetail(scope, summary string) string {
	summary = strings.TrimSpace(summary)
	headline := "scope: " + strings.TrimSpace(scope)
	if summary == headline {
		return ""
	}
	if detail, ok := strings.CutPrefix(summary, headline+" · "); ok {
		return strings.TrimSpace(detail)
	}
	return summary
}
