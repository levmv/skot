package ui

import tea "charm.land/bubbletea/v2"

// terminalKeyboardState records the keyboard protocol state reported after
// Skot requests its desired enhancements. A missing response remains distinct
// from a response reporting zero active flags.
type terminalKeyboardState struct {
	reported    bool
	activeFlags int
}

func (state *terminalKeyboardState) record(message tea.KeyboardEnhancementsMsg) {
	state.reported = true
	state.activeFlags = message.Flags
}
