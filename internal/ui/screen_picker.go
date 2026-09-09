package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func appendDescription(description, part string) string {
	description = strings.TrimSpace(description)
	part = strings.TrimSpace(part)
	if description == "" {
		return part
	}
	if part == "" {
		return description
	}
	return description + " · " + part
}

func (m *screenModel) openToolSetPicker() {
	toolSets := m.agent.ToolSets()
	items := make([]pickerItem, 0, len(toolSets))
	for _, toolSet := range toolSets {
		items = append(items, pickerItem{value: toolSet, label: toolSet, description: toolSetDescription(m.agent.ToolSetTools(toolSet))})
	}
	m.openPicker(pickerToolSet, items, markCurrentPickerItem(items, m.agent.CurrentToolSet()))
}

func toolSetDescription(tools []string) string {
	if len(tools) == 0 {
		return "no model tools"
	}
	return strings.Join(tools, ", ")
}

func (m *screenModel) openThemePicker() {
	items := append([]pickerItem(nil), themePickerItems...)
	m.openPicker(pickerTheme, items, markCurrentPickerItem(items, m.theme))
}

func (m *screenModel) openDisplayPicker() {
	items := append([]pickerItem(nil), displayPickerItems...)
	m.openPicker(pickerDisplay, items, markCurrentPickerItem(items, m.displayProfile))
}

func markCurrentPickerItem(items []pickerItem, current string) int {
	selected := 0
	for index := range items {
		items[index].current = strings.EqualFold(items[index].value, current)
		if items[index].current {
			selected = index
		}
	}
	return selected
}

func (m *screenModel) openPicker(kind pickerKind, items []pickerItem, selected int) {
	m.picker = pickerState{kind: kind, items: items, index: min(max(0, selected), len(items)-1)}
	m.composer.reset()
	m.syncCommandSuggestions()
}

type pickerNote struct {
	text    string
	warning bool
}

// pickerNoteFor supplies the hint shown below a menu.
func (m screenModel) pickerNoteFor(kind pickerKind) pickerNote {
	switch kind {
	case pickerToolSet:
		return pickerNote{text: "switching resets the prompt cache, so the next message costs full price", warning: true}
	case pickerModelAPI:
		return pickerNote{text: "a wrong protocol fails on the first request", warning: true}
	case pickerScope:
		return pickerNote{text: "Shift+Tab cycles scope without opening this menu"}
	case pickerDisplay:
		if hint := m.displayShortcutHint(); hint != "" {
			return pickerNote{text: "Anytime: " + hint}
		}
	}
	return pickerNote{}
}

// currentPickerMark flags the active row. Its width is reserved on every row of
// an aligned picker so the description column does not depend on which row is
// current.
const currentPickerMark = "  ✓"

// Align descriptions in fixed menus. Search menus use variable spacing so
// filtering does not shift the description column.
func pickerAlignsDescriptions(kind pickerKind) bool {
	switch kind {
	case pickerToolSet, pickerScope, pickerTheme, pickerDisplay:
		return true
	default:
		return false
	}
}

func (picker pickerState) descriptionColumn() int {
	if !pickerAlignsDescriptions(picker.kind) {
		return 0
	}
	column := 0
	for _, item := range picker.items {
		// Managed path rows keep their own width: a long path would otherwise
		// push the descriptions of the rows being compared off the line.
		if item.filesystemPath != notFilesystemPath {
			continue
		}
		column = max(column, visibleLen(sanitizeTerminalText(item.label)))
	}
	return column + visibleLen(currentPickerMark)
}

func pickerNavigationFor(kind pickerKind) pickerNavigation {
	// Fixed, non-destructive lists use number shortcuts; models use search and
	// logout keeps only arrows.
	switch kind {
	case pickerModel:
		return navigationSearch
	case pickerModelAPI, pickerToolSet, pickerScope, pickerTheme, pickerDisplay, pickerLogin, pickerSession:
		return navigationNumbers
	default:
		// Logout keeps arrows only: it is the one destructive picker, and rare
		// enough that a shortcut is worth less than a stray digit costs.
		return navigationArrows
	}
}

func (picker pickerState) visibleIndices() []int {
	indices := make([]int, 0, len(picker.items))
	var terms []string
	if pickerNavigationFor(picker.kind) == navigationSearch {
		terms = strings.Fields(strings.ToLower(picker.query))
	}
	for index, item := range picker.items {
		if len(terms) == 0 || item.custom || pickerItemMatches(item, terms) {
			indices = append(indices, index)
		}
	}
	return indices
}

func pickerItemMatches(item pickerItem, terms []string) bool {
	haystack := strings.ToLower(strings.Join([]string{item.label, item.value, item.details}, " "))
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// appendQuery takes both typed keys and pasted text, so it flattens a paste
// into a single searchable line instead of carrying newlines into the query.
func (picker *pickerState) appendQuery(text string) {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, sanitizeTerminalText(text))
	if text == "" {
		return
	}
	picker.query += text
	picker.reconcileSelection()
}

func (picker *pickerState) reconcileSelection() {
	visible := picker.visibleIndices()
	if len(visible) == 0 {
		picker.index = -1
		return
	}
	if slices.Contains(visible, picker.index) {
		return
	}
	picker.index = visible[0]
}

func (picker *pickerState) moveSelection(delta int) {
	visible := picker.visibleIndices()
	if len(visible) == 0 {
		picker.index = -1
		return
	}
	position := 0
	for index, itemIndex := range visible {
		if itemIndex == picker.index {
			position = index
			break
		}
	}
	position = min(max(0, position+delta), len(visible)-1)
	picker.index = visible[position]
}

func (picker pickerState) numberSelectionEnabled() bool {
	count := picker.numberedRows()
	return pickerNavigationFor(picker.kind) == navigationNumbers && count > 0 && count <= 9
}

// numberedRows counts the leading rows a digit may select. Managed path rows
// follow the scopes and are never numbered: their order changes with the policy,
// and a digit must keep meaning the same scope.
func (picker pickerState) numberedRows() int {
	for index, item := range picker.items {
		if item.filesystemPath != notFilesystemPath {
			return index
		}
	}
	return len(picker.items)
}

func (m screenModel) handlePickerKey(message tea.KeyPressMsg) (screenModel, tea.Cmd) {
	key := message.Key()
	action := m.keymap.actionFor(message)
	digit := pickerDigit(message)
	navigation := pickerNavigationFor(m.picker.kind)
	switch {
	case action == actionCancel || action == actionInterrupt:
		kind, pending := m.picker.kind, m.picker.pendingModel
		m.closePicker()
		if kind == pickerModelAPI {
			m.cancelModelAPIChoice(pending)
			m.refreshTranscript()
		}
		return m, nil
	case isUpKey(message):
		m.picker.moveSelection(-1)
		return m, nil
	case isDownKey(message):
		m.picker.moveSelection(1)
		return m, nil
	case m.picker.kind == pickerModel && (key.Code == tea.KeyLeft || key.Code == tea.KeyKpLeft):
		m.cycleModelEffort(-1)
		return m, nil
	case m.picker.kind == pickerModel && (key.Code == tea.KeyRight || key.Code == tea.KeyKpRight):
		m.cycleModelEffort(1)
		return m, nil
	case navigation == navigationSearch && editorControlIs(message, 'u'):
		m.picker.query = ""
		m.picker.reconcileSelection()
		return m, nil
	case navigation == navigationSearch && key.Code == tea.KeyBackspace:
		runes := []rune(m.picker.query)
		if len(runes) != 0 {
			m.picker.query = string(runes[:len(runes)-1])
			m.picker.reconcileSelection()
		}
		return m, nil
	case navigation == navigationSearch && key.Text != "" && key.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModMeta|tea.ModHyper|tea.ModSuper) == 0:
		m.picker.appendQuery(key.Text)
		return m, nil
	case m.picker.kind == pickerScope && key.Mod == 0 &&
		(key.Code == tea.KeyBackspace || key.Text == "d") && m.picker.selectedItem().managedPath():
		// A row the session does not own is refused by the application, which
		// owns the wording; the row's hint only says which ones can go.
		return m, m.startFilesystemPathRemoval(m.picker.selectedItem())
	case m.picker.numberSelectionEnabled() && digit > 0:
		if digit <= m.picker.numberedRows() {
			m.picker.index = digit - 1
			return m.selectPickerItem()
		}
		return m, nil
	case action == actionConfirm:
		return m.selectPickerItem()
	default:
		return m, nil
	}
}

func (picker pickerState) selectedItem() pickerItem {
	if picker.index < 0 || picker.index >= len(picker.items) {
		return pickerItem{}
	}
	return picker.items[picker.index]
}

func pickerDigit(message tea.KeyPressMsg) int {
	key := message.Key()
	if key.Mod&(tea.ModShift|tea.ModAlt|tea.ModCtrl|tea.ModMeta|tea.ModHyper|tea.ModSuper) != 0 {
		return 0
	}
	if len(key.Text) == 1 && key.Text[0] >= '1' && key.Text[0] <= '9' {
		return int(key.Text[0] - '0')
	}
	// Keypad digits arrive as their own contiguous codes and carry no text.
	if key.Code >= tea.KeyKp1 && key.Code <= tea.KeyKp9 {
		return int(key.Code-tea.KeyKp1) + 1
	}
	return 0
}

func (m screenModel) selectPickerItem() (screenModel, tea.Cmd) {
	picker := m.picker
	if picker.index < 0 || picker.index >= len(picker.items) {
		return m, nil
	}
	item := picker.items[picker.index]
	if item.addRow {
		m.closePicker()
		m.openPathPrompt(item.filesystemPath)
		m.refreshTranscript()
		return m, nil
	}
	if item.managedPath() {
		// A path row is managed, not chosen; the menu stays open.
		return m, nil
	}
	m.closePicker()
	switch picker.kind {
	case pickerModel:
		if item.details != "" {
			m.addBlock(screenBlockSystem, item.details)
			m.refreshTranscript()
			return m, nil
		}
		if item.custom {
			m.composer.setValue("/model " + strings.TrimSpace(picker.query))
			m.composer.cursorEnd()
			m.syncCommandSuggestions()
			return m, nil
		}
		selection := m.modelSelectionForURI(item.value)
		selection.effort = selectedModelEffort(item)
		command := m.selectModel(selection, picker)
		m.refreshTranscript()
		return m, command
	case pickerModelAPI:
		selection := picker.pendingModel
		selection.api = item.value
		m.switchModel(selection)
	case pickerToolSet:
		m.selectToolSet(item.value)
	case pickerScope:
		return m, m.startScopeSwitch(item.value)
	case pickerTheme:
		command, _ := m.selectTheme(item.value)
		m.refreshTranscript()
		return m, command
	case pickerDisplay:
		m.selectDisplay(item.value)
	case pickerLogin:
		pendingModel := ""
		if picker.startupLogin {
			pendingModel = item.modelURI
		}
		command := m.startProviderLogin(item.value, modelSelection{uri: pendingModel}, pickerState{})
		m.refreshTranscript()
		return m, command
	case pickerLogout:
		command := m.logoutProvider(item.value)
		m.refreshTranscript()
		return m, command
	case pickerSession:
		return m, resumeSessionCmd(item.value)
	}
	m.refreshTranscript()
	return m, nil
}

func (m *screenModel) closePicker() {
	m.picker = pickerState{}
	m.composer.reset()
	m.syncCommandSuggestions()
}

func (m screenModel) renderPicker() []string {
	searchable := pickerNavigationFor(m.picker.kind) == navigationSearch
	note := m.pickerNoteFor(m.picker.kind)
	visible := m.picker.visibleIndices()
	reservedLines := 5
	if searchable {
		reservedLines++
	}
	for _, index := range visible {
		if m.picker.items[index].dividerBefore {
			reservedLines++
			break
		}
	}
	if note.text != "" {
		// The note gets a blank line of its own so it reads as a caveat about
		// the picker rather than as a trailing row.
		reservedLines += 2
	}
	limit := min(10, max(1, m.height-reservedLines))
	selectedPosition := 0
	for position, index := range visible {
		if index == m.picker.index {
			selectedPosition = position
			break
		}
	}
	start := max(0, selectedPosition-limit/2)
	if start+limit > len(visible) {
		start = max(0, len(visible)-limit)
	}
	end := min(len(visible), start+limit)
	lines := make([]string, 0, end-start+1)
	if searchable {
		filter := m.mutedStyle.Render("filter: ")
		if m.picker.query == "" {
			filter += m.mutedStyle.Render("type to search")
		} else {
			filter += m.accentStyle.Render(sanitizeTerminalText(m.picker.query))
		}
		lines = append(lines, strings.Repeat(" ", transcriptGutter)+filter)
	}
	if len(visible) == 0 {
		return append(lines, strings.Repeat(" ", transcriptGutter)+m.mutedStyle.Render("no matches"))
	}
	numbered := m.picker.numberSelectionEnabled()
	descriptionColumn := m.picker.descriptionColumn()
	for position := start; position < end; position++ {
		index := visible[position]
		item := m.picker.items[index]
		if item.dividerBefore && position > start {
			divider := m.mutedStyle.Render(strings.Repeat("─", m.contentWidth()))
			lines = append(lines, strings.Repeat(" ", transcriptGutter)+divider)
		}
		marker := " "
		if index == m.picker.index {
			marker = userMarker
		}
		label := sanitizeTerminalText(item.label)
		switch {
		case index == m.picker.index || item.current:
			label = m.accentStyle.Render(label)
		case item.dimmed:
			label = m.mutedStyle.Render(label)
		}
		if item.current {
			label += m.mutedStyle.Render(currentPickerMark)
		}
		if item.filesystemPath == notFilesystemPath {
			if pad := descriptionColumn - visibleLen(label); pad > 0 {
				label += strings.Repeat(" ", pad)
			}
		}
		description := item.description
		if index == m.picker.index {
			description = appendDescription(description, item.activeDetail)
		}
		if description != "" {
			label += m.mutedStyle.Render("  " + sanitizeTerminalText(description))
		}
		if m.picker.kind == pickerModel && index == m.picker.index && len(item.efforts) > 1 {
			effort := selectedModelEffort(item)
			if effort == "" {
				effort = "default"
			}
			label += m.mutedStyle.Render("  effort: " + sanitizeTerminalText(effort) + "  ←/→")
		}
		shortcut := ""
		availableWidth := m.contentWidth()
		if numbered {
			// Managed path rows keep the digit column empty: their order moves
			// with the policy, so a number there would not stay the same row.
			shortcut = "  "
			if item.filesystemPath == notFilesystemPath {
				shortcut = m.mutedStyle.Render(fmt.Sprintf("%d ", index+1))
			}
			availableWidth = max(1, availableWidth-2)
		}
		lines = append(lines, marker+strings.Repeat(" ", transcriptGutter-1)+shortcut+truncateANSI(label, availableWidth))
	}
	if note.text != "" {
		style := m.mutedStyle
		if note.warning {
			style = m.warningStyle
		}
		lines = append(lines, "", strings.Repeat(" ", transcriptGutter)+style.Render(note.text))
	}
	return lines
}
