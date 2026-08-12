package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

type sandboxSwitchState struct {
	pending bool
	cancel  context.CancelFunc
}

func (m *screenModel) startSandboxSwitch(policy string) tea.Cmd {
	if m.sandbox.pending {
		m.addBlock(screenBlockError, "sandbox: a policy switch is already in progress")
		return nil
	}
	operationCtx, cancel := context.WithCancel(m.ctx)
	concurrent := m.operation.isTurn()
	m.sandbox = sandboxSwitchState{pending: true, cancel: cancel}
	if !concurrent {
		m.operation = activeOperation{kind: operationSandbox, startedAt: time.Now(), cancel: cancel}
	}
	return func() tea.Msg {
		err := m.agent.SwitchSandbox(operationCtx, policy)
		return sandboxDoneMsg{
			policy:     m.agent.CurrentSandbox(),
			summary:    m.agent.SecuritySummary(),
			concurrent: concurrent,
			err:        err,
		}
	}
}

func (m *screenModel) finishSandboxSwitch(message sandboxDoneMsg) {
	if m.sandbox.cancel != nil {
		m.sandbox.cancel()
	}
	m.sandbox = sandboxSwitchState{}
	if !message.concurrent {
		m.operation.clear()
	}
	if message.err != nil {
		m.addBlock(screenBlockError, "sandbox: "+message.err.Error())
		return
	}
	text := "sandbox policy: " + message.policy + "\n" + message.summary
	if message.concurrent {
		text += "\nnew processes use this policy; already running processes retain their launch policy"
	}
	m.addBlock(screenBlockSystem, text)
}
