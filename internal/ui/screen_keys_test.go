package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestDefaultKeyMapResolvesActionsFromPhysicalBindings(t *testing.T) {
	keymap := newDefaultKeyMap()
	tests := []struct {
		name   string
		key    tea.KeyPressMsg
		action actionID
	}{
		{name: "confirm", key: tea.KeyPressMsg{Code: tea.KeyEnter}, action: actionConfirm},
		{name: "keypad confirm", key: tea.KeyPressMsg{Code: tea.KeyKpEnter}, action: actionConfirm},
		{name: "newline", key: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}, action: actionInsertNewline},
		{name: "scope", key: tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, action: actionCycleScope},
		{name: "queued input", key: tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}, action: actionRestoreQueuedInput},
		{name: "keypad queued input", key: tea.KeyPressMsg{Code: tea.KeyKpUp, Mod: tea.ModAlt}, action: actionRestoreQueuedInput},
		{name: "history", key: tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}, action: actionHistoryPrevious},
		{
			name:   "base layout history",
			key:    tea.KeyPressMsg{Code: 'з', Text: "з", BaseCode: 'p', Mod: tea.ModCtrl},
			action: actionHistoryPrevious,
		},
		{name: "ordinary digit", key: tea.KeyPressMsg{Code: '1', Text: "1"}, action: actionNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := keymap.actionFor(test.key); got != test.action {
				t.Fatalf("action = %q, want %q", got, test.action)
			}
		})
	}
}

func TestScreenRecordsActiveKeyboardProtocolFlags(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	flags := ansi.KittyDisambiguateEscapeCodes | ansi.KittyReportAlternateKeys
	updated, _ := model.update(tea.KeyboardEnhancementsMsg{Flags: flags})

	if !updated.keyboard.reported || updated.keyboard.activeFlags != flags {
		t.Fatalf("keyboard protocol state = %#v", updated.keyboard)
	}
}
