package ui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const composerMaxRows = 8

// composerState owns editable input, input history, and command completion.
// Screen routing decides when input is accepted; editor-specific behavior stays
// behind this type.
type composerState struct {
	editor          textarea.Model
	history         []string
	historyIndex    int
	saved           string
	suggestions     []string
	suggestionIndex int
}

func newComposerState() composerState {
	editor := textarea.New()
	plain := lipgloss.NewStyle()
	state := textarea.StyleState{
		Base: plain, Text: plain, LineNumber: plain, CursorLineNumber: plain,
		CursorLine: plain, EndOfBuffer: plain, Placeholder: plain.Faint(true), Prompt: plain,
	}
	styles := editor.Styles()
	styles.Focused = state
	styles.Blurred = state
	styles.Cursor.Color = nil
	editor.SetStyles(styles)
	editor.Prompt = ""
	editor.Placeholder = ""
	editor.ShowLineNumbers = false
	editor.DynamicHeight = true
	editor.MinHeight = 1
	editor.MaxHeight = composerMaxRows
	editor.MaxWidth = 0
	editor.MaxContentHeight = 10000
	editor.SetHeight(1)
	editor.SetVirtualCursor(false)
	editor.SetWidth(1)
	_ = editor.Focus()
	return composerState{editor: editor}
}

func (composer *composerState) resize(width, screenHeight int) {
	composer.editor.MaxHeight = min(composerMaxRows, max(1, screenHeight-4))
	composer.editor.SetWidth(width)
}

func (composer composerState) value() string { return composer.editor.Value() }

func (composer *composerState) setValue(value string) { composer.editor.SetValue(value) }

func (composer *composerState) reset() { composer.editor.Reset() }

func (composer *composerState) insertString(value string) { composer.editor.InsertString(value) }

func (composer *composerState) cursorEnd() { composer.editor.CursorEnd() }

func (composer *composerState) moveToEnd() { composer.editor.MoveToEnd() }

func (composer *composerState) moveToBegin() { composer.editor.MoveToBegin() }

func (composer composerState) height() int { return composer.editor.Height() }

func (composer composerState) view() string { return composer.editor.View() }

func (composer composerState) cursor() *tea.Cursor { return composer.editor.Cursor() }

func (composer *composerState) update(message tea.Msg) tea.Cmd {
	var command tea.Cmd
	composer.editor, command = composer.editor.Update(message)
	return command
}

func (composer composerState) atFirstVisualLine() bool {
	line := composer.editor.LineInfo()
	return composer.editor.Line() == 0 && line.RowOffset == 0
}

func (composer composerState) atLastVisualLine() bool {
	line := composer.editor.LineInfo()
	return composer.editor.Line() == composer.editor.LineCount()-1 && line.RowOffset >= line.Height-1
}

func (composer *composerState) resetHistory() {
	composer.history = nil
	composer.historyIndex = 0
	composer.saved = ""
}

func (composer *composerState) remember(input string) {
	if strings.TrimSpace(input) == "" {
		return
	}
	if len(composer.history) == 0 || composer.history[len(composer.history)-1] != input {
		composer.history = append(composer.history, input)
	}
	composer.historyIndex = len(composer.history)
	composer.saved = ""
}

func (composer composerState) historyAtEnd() bool {
	return composer.historyIndex >= len(composer.history)
}

func (composer *composerState) historyPrevious() bool {
	if len(composer.history) == 0 {
		return false
	}
	if composer.historyIndex == len(composer.history) {
		composer.saved = composer.value()
	}
	if composer.historyIndex == 0 {
		return false
	}
	composer.historyIndex--
	composer.setValue(composer.history[composer.historyIndex])
	composer.cursorEnd()
	return true
}

func (composer *composerState) historyNext() bool {
	if composer.historyIndex >= len(composer.history) {
		return false
	}
	if composer.historyIndex < len(composer.history)-1 {
		composer.historyIndex++
		composer.setValue(composer.history[composer.historyIndex])
	} else {
		composer.historyIndex = len(composer.history)
		composer.setValue(composer.saved)
	}
	composer.cursorEnd()
	return true
}

func (composer *composerState) setSuggestionCandidates(candidates []string) {
	value := strings.ToLower(strings.TrimLeft(composer.value(), " \t"))
	composer.suggestions = composer.suggestions[:0]
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate), value) {
			composer.suggestions = append(composer.suggestions, candidate)
		}
	}
	if composer.suggestionIndex >= len(composer.suggestions) {
		composer.suggestionIndex = max(0, len(composer.suggestions)-1)
	}
}

func (composer composerState) hasSuggestions() bool { return len(composer.suggestions) != 0 }

func (composer composerState) currentSuggestion() string {
	if len(composer.suggestions) == 0 {
		return ""
	}
	return composer.suggestions[min(max(0, composer.suggestionIndex), len(composer.suggestions)-1)]
}

func (composer *composerState) moveSuggestion(delta int) {
	if len(composer.suggestions) == 0 {
		return
	}
	composer.suggestionIndex = (composer.suggestionIndex + delta + len(composer.suggestions)) % len(composer.suggestions)
}

func (composer composerState) suggestionWindow(limit int) ([]string, int) {
	limit = min(max(0, limit), len(composer.suggestions))
	if limit == 0 {
		return nil, 0
	}
	selected := min(max(0, composer.suggestionIndex), len(composer.suggestions)-1)
	start := max(0, selected-limit/2)
	if start+limit > len(composer.suggestions) {
		start = len(composer.suggestions) - limit
	}
	return composer.suggestions[start : start+limit], selected - start
}
