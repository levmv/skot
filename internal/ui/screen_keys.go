package ui

import tea "charm.land/bubbletea/v2"

func keyIsCtrl(message tea.KeyPressMsg, base rune) bool {
	key := message.Key()
	if key.Mod&tea.ModCtrl == 0 {
		return false
	}
	return keyLayoutBase(message) == base
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

func isEscapeKey(message tea.KeyPressMsg) bool {
	return message.Key().Code == tea.KeyEscape || message.String() == "esc"
}

func isEnterKey(message tea.KeyPressMsg) bool {
	code := message.Key().Code
	return code == tea.KeyEnter || code == tea.KeyKpEnter
}

func isInterruptKey(message tea.KeyPressMsg) bool {
	return keyIsCtrl(message, 'c')
}

func isNewlineShortcut(message tea.KeyPressMsg) bool {
	key := message.Key()
	keyString := message.String()
	modifiedEnter := isEnterKey(message) && (key.Mod&(tea.ModShift|tea.ModAlt) != 0 || keyString == "shift+enter" || keyString == "alt+enter")
	return modifiedEnter || keyIsCtrl(message, 'j')
}

func isShiftTab(message tea.KeyPressMsg) bool {
	key := message.Key()
	return key.Code == tea.KeyTab && key.Mod&tea.ModShift != 0 || message.String() == "shift+tab"
}

func isAltUp(message tea.KeyPressMsg) bool {
	key := message.Key()
	return isUpKey(message) && (key.Mod&tea.ModAlt != 0 || message.String() == "alt+up")
}

func isUpKey(message tea.KeyPressMsg) bool {
	code := message.Key().Code
	return code == tea.KeyUp || code == tea.KeyKpUp
}

func isDownKey(message tea.KeyPressMsg) bool {
	code := message.Key().Code
	return code == tea.KeyDown || code == tea.KeyKpDown
}
