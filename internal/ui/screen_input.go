package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m screenModel) handleKey(msg tea.KeyPressMsg) (screenModel, tea.Cmd) {
	if m.picker.active() {
		return m.handlePickerKey(msg)
	}
	if m.loginProvider != "" {
		return m.handleLoginKey(msg)
	}
	key := msg.Key()
	keyString := msg.String()
	hasCtrl := key.Mod&tea.ModCtrl != 0
	baseKey := keyLayoutBase(msg)
	if m.operation.isMaintenance() {
		if isInterruptKey(msg) || isEscapeKey(msg) {
			if m.operation.cancel != nil {
				m.operation.cancel()
			}
		}
		return m, nil
	}

	switch {
	case isInterruptKey(msg):
		if m.operation.isTurn() {
			m.cancelTurn(false)
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case keyIsCtrl(msg, 'd'):
		if !m.operation.isTurn() && strings.TrimSpace(m.composer.value()) == "" {
			m.quitting = true
			return m, tea.Quit
		}
		return m.updateInput(msg)
	case isNewlineShortcut(msg):
		m.composer.insertString("\n")
		m.syncCommandSuggestions()
		return m, nil
	case isEnterKey(msg):
		return m.submitInput()
	case isAltUp(msg):
		if m.operation.isTurn() && m.composer.value() == "" {
			if input, ok := m.agent.PopQueued(); ok {
				m.composer.setValue(input)
				m.composer.cursorEnd()
				m.addBlock(screenBlockSystem, "queued input returned to editor")
				m.refreshTranscript()
			}
		}
		return m, nil
	case isEscapeKey(msg):
		if m.operation.isTurn() {
			m.cancelTurn(true)
		}
		return m, nil
	case m.composer.historyAtEnd() && m.commandSuggestionsVisible() && ((!hasCtrl && isUpKey(msg)) || keyIsCtrl(msg, 'p')):
		m.moveCommandSuggestion(-1)
		return m, nil
	case m.composer.historyAtEnd() && m.commandSuggestionsVisible() && ((!hasCtrl && isDownKey(msg)) || keyIsCtrl(msg, 'n')):
		m.moveCommandSuggestion(1)
		return m, nil
	case m.commandSuggestionsVisible() && (key.Code == tea.KeyTab || keyString == "tab"):
		m.composer.setValue(m.currentCommandSuggestion())
		m.composer.cursorEnd()
		m.syncCommandSuggestions()
		return m, nil
	case !hasCtrl && isUpKey(msg):
		if !m.composer.atFirstVisualLine() || (m.composer.historyAtEnd() && strings.TrimSpace(m.composer.value()) != "") {
			return m.updateInput(msg)
		}
		m.historyPrevious()
		return m, nil
	case !hasCtrl && isDownKey(msg):
		if !m.composer.atLastVisualLine() {
			return m.updateInput(msg)
		}
		m.historyNext()
		return m, nil
	case keyIsCtrl(msg, 'p'):
		m.historyPrevious()
		return m, nil
	case keyIsCtrl(msg, 'n'):
		m.historyNext()
		return m, nil
	case hasCtrl && strings.ContainsRune("ukwaebf", baseKey):
		return m.updateInput(editorCtrlKey(baseKey))
	}
	return m.updateInput(msg)
}

func (m screenModel) updateInput(msg tea.Msg) (screenModel, tea.Cmd) {
	var cmd tea.Cmd
	if m.loginProvider != "" {
		m.secret, cmd = m.secret.Update(msg)
	} else {
		cmd = m.composer.update(msg)
		m.syncCommandSuggestions()
	}
	return m, cmd
}

func (m screenModel) handleLoginKey(message tea.KeyPressMsg) (screenModel, tea.Cmd) {
	switch {
	case isEscapeKey(message):
		returnPicker := m.loginReturn
		m.cancelLogin()
		if returnPicker.active() {
			m.picker = returnPicker
			return m, nil
		}
		m.addBlock(screenBlockSystem, "login cancelled")
		m.refreshTranscript()
		return m, nil
	case isInterruptKey(message):
		m.cancelLogin()
		m.addBlock(screenBlockSystem, "login cancelled")
		m.refreshTranscript()
		return m, nil
	case isEnterKey(message):
		return m.submitInput()
	default:
		return m.updateInput(message)
	}
}

func editorCtrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Mod: tea.ModCtrl, Code: code, BaseCode: code}
}

func (m screenModel) submitInput() (screenModel, tea.Cmd) {
	input := m.selectedInput()
	if m.loginProvider != "" {
		provider := m.loginProvider
		pending := m.loginSelection
		m.cancelLogin()
		if input == "" {
			m.addBlock(screenBlockError, "login: API key is required")
		} else if err := m.agent.Login(m.ctx, provider, input); err != nil {
			m.addBlock(screenBlockError, "login: "+err.Error())
		} else {
			m.addBlock(screenBlockSystem, "logged in to "+provider)
			m.refreshProviderStatuses()
			if pending.uri != "" {
				m.switchModel(pending)
			}
		}
		m.refreshTranscript()
		return m, nil
	}
	if input == "" {
		return m, nil
	}
	if command, handled := m.dispatchCommand(input); handled {
		return m, command
	}
	if m.operation.isTurn() && strings.HasPrefix(input, "!") {
		m.addBlock(screenBlockError, "shell escapes are unavailable while Skot is working; wait or cancel the turn")
		m.refreshTranscript()
		return m, nil
	}
	if command, private, shell := shellEscapeCommand(input); shell {
		if command == "" {
			m.addBlock(screenBlockError, "shell command is required")
			m.refreshTranscript()
			return m, nil
		}
		m.composer.reset()
		m.composer.remember(input)
		cmd := m.startShell(command, private)
		m.refreshTranscript()
		return m, cmd
	}

	m.composer.reset()
	m.composer.remember(input)
	if m.operation.isTurn() {
		if err := m.agent.QueueInput(input); err != nil {
			m.addBlock(screenBlockError, "queue input: "+err.Error())
			m.composer.setValue(input)
			m.composer.cursorEnd()
		}
		m.refreshTranscript()
		return m, nil
	}

	m.addBlock(screenBlockUser, input)
	cmd := m.startTurn(input)
	m.refreshTranscript()
	return m, cmd
}

func (m *screenModel) cancelLogin() {
	m.loginProvider = ""
	m.loginSelection = modelSelection{}
	m.loginReturn = pickerState{}
	m.secret.Reset()
	m.syncCommandSuggestions()
}

func shellEscapeCommand(input string) (command string, private, shell bool) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "!") {
		return "", false, false
	}
	private = strings.HasPrefix(input, "!!")
	prefix := "!"
	if private {
		prefix = "!!"
	}
	return strings.TrimSpace(strings.TrimPrefix(input, prefix)), private, true
}

func shellCommandDisplay(command string, private bool) string {
	if private {
		return "!! " + compactCommand(command, 180)
	}
	return "$ " + compactCommand(command, 180)
}

func (m *screenModel) cancelTurn(restoreQueued bool) {
	if m.operation.cancel != nil {
		m.operation.cancel()
	}
	if restoreQueued {
		restored := m.agent.RestoreQueued()
		if draft := strings.TrimSpace(m.composer.value()); draft != "" {
			restored = append(restored, draft)
		}
		if len(restored) != 0 {
			m.composer.setValue(strings.Join(restored, "\n"))
			m.composer.cursorEnd()
			m.syncCommandSuggestions()
			m.addBlock(screenBlockSystem, "cancelled; queued input restored to editor")
		} else {
			m.addBlock(screenBlockSystem, "cancelling current turn")
		}
	} else {
		m.addBlock(screenBlockSystem, "cancelling current turn")
	}
	m.refreshTranscript()
}

func (m *screenModel) historyPrevious() {
	if m.composer.historyPrevious() {
		m.syncCommandSuggestions()
	}
}

func (m *screenModel) historyNext() {
	if m.composer.historyNext() {
		m.syncCommandSuggestions()
	}
}
