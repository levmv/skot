package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

type scopeSwitchState struct {
	pending bool
	cancel  context.CancelFunc
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
	concurrent := m.operation.isTurn()
	previous := m.agent.CurrentScope()
	m.scope = scopeSwitchState{pending: true, cancel: cancel, previous: previous}
	if !concurrent {
		m.operation = activeOperation{kind: operationScope, startedAt: time.Now(), cancel: cancel}
	}
	return func() tea.Msg {
		err := m.agent.SwitchScope(operationCtx, scope)
		current := m.agent.CurrentScope()
		notice := ""
		if err == nil && current != previous {
			notice = m.agent.ScopeNotice()
		}
		return scopeDoneMsg{
			scope:      current,
			summary:    m.agent.ScopeSummary(),
			notice:     notice,
			concurrent: concurrent,
			err:        err,
		}
	}
}

func (m *screenModel) finishScopeSwitch(message scopeDoneMsg) {
	if m.scope.cancel != nil {
		m.scope.cancel()
	}
	previous := m.scope.previous
	m.scope = scopeSwitchState{}
	if !message.concurrent {
		m.operation.clear()
	}
	if message.err != nil {
		m.addBlock(screenBlockError, "scope: "+message.err.Error())
		return
	}
	text := formatSettingChange("filesystem scope", previous, message.scope) + "\n" + message.summary
	if message.notice != "" {
		text += "\nwarning: " + message.notice
	}
	if message.concurrent {
		text += "\nnew tool calls use this scope; work already running is unchanged"
	}
	m.addBlock(screenBlockSystem, text)
}
