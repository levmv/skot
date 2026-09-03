package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m screenModel) handleKey(msg tea.KeyPressMsg) (screenModel, tea.Cmd) {
	if m.picker.active() {
		return m.handlePickerKey(msg)
	}
	action := m.keymap.actionFor(msg)
	if m.loginProvider != "" {
		return m.handleLoginKey(msg, action)
	}
	key := msg.Key()
	hasCtrl := key.Mod&tea.ModCtrl != 0
	if maintenance := m.maintenanceOperation(); maintenance.isMaintenance() {
		if action == actionInterrupt || action == actionCancel {
			if maintenance.cancel != nil {
				maintenance.cancel()
			}
			return m, nil
		}
		if maintenance.kind != operationCompaction {
			return m, nil
		}
		// Manual compaction does not use the composer, so it can collect the
		// next turn while the context summary is being prepared. Keep shortcuts
		// which would start another operation disabled until compaction finishes.
		if action == actionCycleScope || action == actionDeleteOrExit && strings.TrimSpace(m.composer.value()) == "" {
			return m, nil
		}
	}

	switch {
	case action == actionDisplayCompact || action == actionDisplayDetailed || action == actionDisplayFull:
		// Legacy terminal encodings cannot distinguish Ctrl+digits safely. A
		// positive enhancement report makes these direct bindings unambiguous.
		if !m.keyboard.supportsKeyDisambiguation() {
			return m, nil
		}
		profile := DisplayCompact
		if action == actionDisplayDetailed {
			profile = DisplayDetailed
		} else if action == actionDisplayFull {
			profile = DisplayFull
		}
		m.switchTranscriptDisplayFromKey(profile)
		return m, nil
	case action == actionDisplayMore:
		m.shiftTranscriptDisplay(1)
		return m, nil
	case action == actionDisplayLess:
		m.shiftTranscriptDisplay(-1)
		return m, nil
	case action == actionInterrupt:
		if m.operation.isTurn() {
			m.cancelTurn()
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case action == actionCycleScope:
		command := m.startScopeSwitch(nextScope(m.agent.CurrentScope()))
		m.refreshTranscript()
		return m, command
	case action == actionDeleteOrExit:
		if !m.operation.isTurn() && strings.TrimSpace(m.composer.value()) == "" {
			m.quitting = true
			return m, tea.Quit
		}
		return m.updateInput(msg)
	case action == actionInsertNewline:
		m.composer.insertString("\n")
		m.syncCommandSuggestions()
		return m, nil
	case action == actionConfirm:
		return m.submitInput()
	case action == actionRestoreQueuedInput:
		if m.operation.acceptsQueuedInput() && m.composer.value() == "" {
			if input, ok := m.agent.PopQueued(); ok {
				m.composer.setValue(input)
				m.composer.cursorEnd()
				m.addBlock(screenBlockSystem, "pending steer returned to editor")
				m.refreshTranscript()
			}
		}
		return m, nil
	case action == actionCancel:
		if m.modelContextSelection.uri != "" {
			m.cancelModelContextChoice()
			m.refreshTranscript()
			return m, nil
		}
		if m.pathPrompt != notFilesystemPath {
			m.closePathPrompt()
			return m, nil
		}
		if m.operation.isTurn() {
			m.cancelTurn()
		}
		return m, nil
	case m.composer.historyAtEnd() && m.commandSuggestionsVisible() && ((!hasCtrl && isUpKey(msg)) || action == actionHistoryPrevious):
		m.moveCommandSuggestion(-1)
		return m, nil
	case m.composer.historyAtEnd() && m.commandSuggestionsVisible() && ((!hasCtrl && isDownKey(msg)) || action == actionHistoryNext):
		m.moveCommandSuggestion(1)
		return m, nil
	case m.commandSuggestionsVisible() && action == actionAcceptSuggestion:
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
	case action == actionHistoryPrevious:
		m.historyPrevious()
		return m, nil
	case action == actionHistoryNext:
		m.historyNext()
		return m, nil
	default:
		if code, ok := editorControlCode(msg); ok {
			return m.updateInput(editorCtrlKey(code))
		}
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

func (m screenModel) handleLoginKey(message tea.KeyPressMsg, action actionID) (screenModel, tea.Cmd) {
	switch action {
	case actionCancel:
		returnPicker := m.loginReturn
		m.cancelLogin()
		if returnPicker.active() {
			m.picker = returnPicker
			return m, nil
		}
		m.addBlock(screenBlockSystem, "login cancelled")
		m.refreshTranscript()
		return m, nil
	case actionInterrupt:
		m.cancelLogin()
		m.addBlock(screenBlockSystem, "login cancelled")
		m.refreshTranscript()
		return m, nil
	case actionConfirm:
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
	if m.modelContextSelection.uri != "" {
		selection := m.modelContextSelection
		contextWindow, err := parseModelTokenCount(input)
		if err != nil {
			m.addBlock(screenBlockError, "model: "+err.Error())
			m.refreshTranscript()
			return m, nil
		}
		m.modelContextSelection = modelSelection{}
		m.composer.reset()
		m.syncCommandSuggestions()
		selection.contextWindow = contextWindow
		m.selectModel(selection, pickerState{})
		m.refreshTranscript()
		return m, nil
	}
	if m.pathPrompt != notFilesystemPath {
		kind := m.pathPrompt
		m.pathPrompt = notFilesystemPath
		m.pathCompletion.reset()
		m.composer.reset()
		m.syncCommandSuggestions()
		if input == "" {
			// An empty line is the other way out of the prompt.
			m.openScopePicker()
			return m, nil
		}
		command := m.startFilesystemPathAddition(kind, input)
		m.refreshTranscript()
		return m, command
	}
	if input == "" {
		return m, nil
	}
	if command, handled := m.dispatchCommand(input); handled {
		return m, command
	}
	if m.operation.acceptsQueuedInput() && strings.HasPrefix(input, "!") {
		m.addBlock(screenBlockError, "shell escapes are unavailable while Skot is working; wait or cancel the current operation")
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
	if m.operation.acceptsQueuedInput() {
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

func (m *screenModel) cancelTurn() {
	if m.operation.cancel != nil {
		m.operation.cancel()
	}
	m.addBlock(screenBlockSystem, "cancelling current turn")
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
