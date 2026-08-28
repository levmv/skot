package ui

import (
	"runtime"

	tea "charm.land/bubbletea/v2"
)

// actionID names behavior owned by Skot rather than the physical key which
// currently invokes it. Textarea-native movement and editing stay with the
// textarea; only shortcuts interpreted by the surrounding application belong
// here.
type actionID string

const (
	actionNone               actionID = ""
	actionInterrupt          actionID = "app.interrupt"
	actionCancel             actionID = "app.cancel"
	actionConfirm            actionID = "app.confirm"
	actionInsertNewline      actionID = "input.newline"
	actionDeleteOrExit       actionID = "input.delete-or-exit"
	actionAcceptSuggestion   actionID = "input.accept-suggestion"
	actionHistoryPrevious    actionID = "input.history-previous"
	actionHistoryNext        actionID = "input.history-next"
	actionCycleScope         actionID = "scope.next"
	actionRestoreQueuedInput actionID = "queue.restore-last"
	actionDisplayCompact     actionID = "display.compact"
	actionDisplayDetailed    actionID = "display.detailed"
	actionDisplayFull        actionID = "display.full"
	actionDisplayMore        actionID = "display.more"
	actionDisplayLess        actionID = "display.less"
)

type keyStroke struct {
	code rune
	mod  tea.KeyMod
}

type keyBinding struct {
	action actionID
	keys   []keyStroke
	help   string
}

// keyMap is deliberately a small ordered table rather than input dispatch
// itself. It keeps action handlers independent of terminal key encodings.
type keyMap struct {
	bindings []keyBinding
}

func newDefaultKeyMap() keyMap {
	return newDefaultKeyMapFor(runtime.GOOS)
}

func newDefaultKeyMapFor(goos string) keyMap {
	bindings := []keyBinding{
		{action: actionInterrupt, keys: []keyStroke{{code: 'c', mod: tea.ModCtrl}}, help: "ctrl+c"},
		{action: actionCancel, keys: []keyStroke{{code: tea.KeyEscape}}, help: "esc"},
		{action: actionConfirm, keys: []keyStroke{{code: tea.KeyEnter}}, help: "enter"},
		{action: actionInsertNewline, keys: []keyStroke{
			{code: tea.KeyEnter, mod: tea.ModShift},
			{code: tea.KeyEnter, mod: tea.ModAlt},
			{code: 'j', mod: tea.ModCtrl},
		}, help: "shift/alt+enter, ctrl+j"},
		{action: actionDeleteOrExit, keys: []keyStroke{{code: 'd', mod: tea.ModCtrl}}, help: "ctrl+d"},
		{action: actionAcceptSuggestion, keys: []keyStroke{{code: tea.KeyTab}}, help: "tab"},
		{action: actionHistoryPrevious, keys: []keyStroke{{code: 'p', mod: tea.ModCtrl}}, help: "ctrl+p"},
		{action: actionHistoryNext, keys: []keyStroke{{code: 'n', mod: tea.ModCtrl}}, help: "ctrl+n"},
		{action: actionCycleScope, keys: []keyStroke{{code: tea.KeyTab, mod: tea.ModShift}}, help: "shift+tab"},
		{action: actionRestoreQueuedInput, keys: []keyStroke{{code: tea.KeyUp, mod: tea.ModAlt}}, help: "alt+up"},
		{action: actionDisplayCompact, keys: []keyStroke{{code: '1', mod: tea.ModCtrl}}, help: "ctrl+1"},
		{action: actionDisplayDetailed, keys: []keyStroke{{code: '2', mod: tea.ModCtrl}}, help: "ctrl+2"},
		{action: actionDisplayFull, keys: []keyStroke{{code: '3', mod: tea.ModCtrl}}, help: "ctrl+3"},
	}
	if goos != "darwin" {
		bindings = append(bindings,
			keyBinding{action: actionDisplayMore, keys: []keyStroke{{code: tea.KeyUp, mod: tea.ModCtrl}}, help: "ctrl+up"},
			keyBinding{action: actionDisplayLess, keys: []keyStroke{{code: tea.KeyDown, mod: tea.ModCtrl}}, help: "ctrl+down"},
		)
	}
	return keyMap{bindings: bindings}
}

func (keymap keyMap) actionFor(message tea.KeyPressMsg) actionID {
	pressed := normalizeKeyStroke(message)
	for _, binding := range keymap.bindings {
		for _, candidate := range binding.keys {
			if candidate == pressed {
				return binding.action
			}
		}
	}
	return actionNone
}

func (keymap keyMap) helpFor(action actionID) string {
	for _, binding := range keymap.bindings {
		if binding.action == action {
			return binding.help
		}
	}
	return ""
}

func normalizeKeyStroke(message tea.KeyPressMsg) keyStroke {
	key := message.Key()
	code := key.Code
	if key.Mod != 0 {
		if base := keyLayoutBase(message); base != 0 {
			code = base
		}
	}
	switch code {
	case tea.KeyKpEnter:
		code = tea.KeyEnter
	case tea.KeyKpUp:
		code = tea.KeyUp
	case tea.KeyKpDown:
		code = tea.KeyDown
	case tea.KeyKpLeft:
		code = tea.KeyLeft
	case tea.KeyKpRight:
		code = tea.KeyRight
	}
	modifiers := key.Mod & (tea.ModShift | tea.ModAlt | tea.ModCtrl | tea.ModMeta | tea.ModHyper | tea.ModSuper)
	return keyStroke{code: code, mod: modifiers}
}

func keyLayoutBase(message tea.KeyPressMsg) rune {
	key := message.Key()
	for _, candidate := range []rune{key.BaseCode, key.Code, key.ShiftedCode} {
		if mapped := layoutBaseRune(candidate); mapped != 0 {
			return mapped
		}
	}
	for _, candidate := range key.Text {
		return layoutBaseRune(candidate)
	}
	return 0
}

func isUpKey(message tea.KeyPressMsg) bool {
	code := message.Key().Code
	return code == tea.KeyUp || code == tea.KeyKpUp
}

func isDownKey(message tea.KeyPressMsg) bool {
	code := message.Key().Code
	return code == tea.KeyDown || code == tea.KeyKpDown
}

func editorControlCode(message tea.KeyPressMsg) (rune, bool) {
	if message.Key().Mod&tea.ModCtrl == 0 {
		return 0, false
	}
	code := keyLayoutBase(message)
	return code, code != 0 && (code == 'a' || code == 'e' || code == 'b' || code == 'f' || code == 'u' || code == 'k' || code == 'w')
}

func editorControlIs(message tea.KeyPressMsg, want rune) bool {
	code, ok := editorControlCode(message)
	return ok && code == want
}
