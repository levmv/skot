package ui

import (
	"strings"
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
		{name: "compact display", key: tea.KeyPressMsg{Code: '1', Mod: tea.ModCtrl}, action: actionDisplayCompact},
		{name: "detailed display", key: tea.KeyPressMsg{Code: '2', Mod: tea.ModCtrl}, action: actionDisplayDetailed},
		{name: "full display", key: tea.KeyPressMsg{Code: '3', Mod: tea.ModCtrl}, action: actionDisplayFull},
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

func TestDisplayArrowBindingsAreNotAssignedOnMacOS(t *testing.T) {
	more := tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl}
	less := tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl}
	if got := newDefaultKeyMapFor("linux").actionFor(more); got != actionDisplayMore {
		t.Fatalf("linux Ctrl+Up action = %q", got)
	}
	if got := newDefaultKeyMapFor("linux").actionFor(less); got != actionDisplayLess {
		t.Fatalf("linux Ctrl+Down action = %q", got)
	}
	if got := newDefaultKeyMapFor("darwin").actionFor(more); got != actionNone {
		t.Fatalf("macOS Ctrl+Up action = %q", got)
	}
	if got := newDefaultKeyMapFor("darwin").actionFor(less); got != actionNone {
		t.Fatalf("macOS Ctrl+Down action = %q", got)
	}
}

func TestDisplayDirectKeysRequireKeyboardEnhancements(t *testing.T) {
	fake := &fakeAgent{displayProfile: DisplayDetailed}
	model := testScreenModel(t, fake)
	model.keymap = newDefaultKeyMapFor("linux")
	compact := tea.KeyPressMsg{Code: '1', Mod: tea.ModCtrl}

	model, _ = model.handleKey(compact)
	if model.displayProfile != DisplayDetailed || fake.displayProfile != DisplayDetailed {
		t.Fatalf("unconfirmed Ctrl+1 changed display to %q", model.displayProfile)
	}
	model.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	model, _ = model.handleKey(compact)
	if model.displayProfile != DisplayCompact || fake.displayProfile != DisplayCompact {
		t.Fatalf("confirmed Ctrl+1 left display at %q", model.displayProfile)
	}

	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	if model.displayProfile != DisplayDetailed {
		t.Fatalf("Ctrl+Up display = %q", model.displayProfile)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	if model.displayProfile != DisplayFull {
		t.Fatalf("second Ctrl+Up display = %q", model.displayProfile)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	if model.displayProfile != DisplayFull {
		t.Fatalf("Ctrl+Up wrapped past full to %q", model.displayProfile)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	if model.displayProfile != DisplayDetailed {
		t.Fatalf("Ctrl+Down display = %q", model.displayProfile)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: '3', Mod: tea.ModCtrl})
	if model.displayProfile != DisplayFull {
		t.Fatalf("Ctrl+3 display = %q", model.displayProfile)
	}
}

func TestDisplayHelpMatchesPlatformBindings(t *testing.T) {
	linux := testScreenModel(t, &fakeAgent{})
	linux.keymap = newDefaultKeyMapFor("linux")
	legacyLinux := linux.tuiCommandHelp()
	if strings.Contains(legacyLinux, "ctrl+1/2/3") || !strings.Contains(legacyLinux, "/display") || !strings.Contains(legacyLinux, "ctrl+up/down") {
		t.Fatalf("legacy Linux display help = %q", legacyLinux)
	}
	linux.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	if enhanced := linux.tuiCommandHelp(); !strings.Contains(enhanced, "ctrl+1/2/3") || strings.Contains(enhanced, "/display") {
		t.Fatalf("enhanced Linux display help = %q", enhanced)
	}

	macOS := testScreenModel(t, &fakeAgent{})
	macOS.keymap = newDefaultKeyMapFor("darwin")
	legacyMacOS := macOS.tuiCommandHelp()
	if strings.Contains(legacyMacOS, "ctrl+1/2/3") || strings.Contains(legacyMacOS, "ctrl+up/down") || !strings.Contains(legacyMacOS, "/display") {
		t.Fatalf("legacy macOS display help = %q", legacyMacOS)
	}
	macOS.keyboard.record(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyDisambiguateEscapeCodes})
	if enhanced := macOS.tuiCommandHelp(); !strings.Contains(enhanced, "ctrl+1/2/3") || strings.Contains(enhanced, "ctrl+up/down") {
		t.Fatalf("enhanced macOS display help = %q", enhanced)
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
