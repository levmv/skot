package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type tuiCommand struct {
	name        string
	aliases     []string
	description string
	usage       string
	minArgs     int
	maxArgs     int
	duringTurn  bool
	run         func(*screenModel, string, []string) tea.Cmd
}

var tuiCommands []tuiCommand

// Typing "/" shows commands; /help covers keyboard shortcuts and shell input.
// Only advertise keys the current terminal can distinguish.
func (m screenModel) tuiCommandHelp() string {
	displayLines := make([]string, 0, 2)
	if direct := m.directDisplayShortcutLabel(); direct != "" {
		displayLines = append(displayLines, fmt.Sprintf("  %-24s select compact/detailed/full", direct))
	} else {
		displayLines = append(displayLines, fmt.Sprintf("  %-24s choose transcript detail", "/display"))
	}
	if relative := m.relativeDisplayShortcutLabel(); relative != "" {
		displayLines = append(displayLines, fmt.Sprintf("  %-24s increase/decrease detail", relative))
	}
	displayKeys := strings.Join(displayLines, "\n")
	keymap := m.keymap
	return fmt.Sprintf(`Keys
  %-24s send
  %-24s insert a newline
  %-24s accept the suggestion
  %-24s cycle filesystem scope
%s
  %-24s walk input history
  %-24s start, end, back, forward
  %-24s erase to start, to end, word
  %-24s interrupt; apply pending steer
  %-24s exit when the input is empty

Shell
  ! command                keeps the result in context
  !! command               private, keeps nothing

While Skot is working
  %-24s steer at next model request
  %-24s edit the last pending steer

Type / for commands.`,
		keymap.helpFor(actionConfirm),
		keymap.helpFor(actionInsertNewline),
		keymap.helpFor(actionAcceptSuggestion),
		keymap.helpFor(actionCycleScope),
		displayKeys,
		"up/down, "+keymap.helpFor(actionHistoryPrevious)+"/"+keymap.helpFor(actionHistoryNext),
		"ctrl+a/e/b/f",
		"ctrl+u/k/w",
		keymap.helpFor(actionCancel),
		keymap.helpFor(actionDeleteOrExit),
		keymap.helpFor(actionConfirm),
		keymap.helpFor(actionRestoreQueuedInput),
	)
}

func init() {
	// Assigning the function-valued table here avoids a package initialization
	// cycle through command handlers which refresh suggestions from this table.
	tuiCommands = []tuiCommand{
		{name: "/help", description: "show keys", usage: "/help", duringTurn: true, run: runHelpCommand},
		{name: "/clear", description: "start a new session", usage: "/clear", duringTurn: true, run: runClearCommand},
		{name: "/resume", description: "choose or resume a previous session", usage: "/resume [id-or-prefix]", maxArgs: 1, run: runResumeCommand},
		{name: "/login", description: "sign in to a provider or service", usage: "/login [provider]", maxArgs: 1, duringTurn: true, run: runLoginCommand},
		{name: "/model", description: "list or switch models", usage: "/model [provider/model [api]]", maxArgs: 2, duringTurn: true, run: runModelCommand},
		{name: "/tools", description: "show or switch the active tool set", usage: "/tools [name]", maxArgs: 1, duringTurn: true, run: runToolsCommand},
		{name: "/scope", description: "show or switch filesystem scope", usage: "/scope [workspace|machine]", maxArgs: 1, duringTurn: true, run: runScopeCommand},
		{name: "/theme", description: "show or switch the terminal theme", usage: "/theme [auto|light|dark]", maxArgs: 1, duringTurn: true, run: runThemeCommand},
		{name: "/display", description: "show or switch transcript detail", usage: "/display [compact|detailed|full]", maxArgs: 1, duringTurn: true, run: runDisplayCommand},
		{name: "/context", description: "show context budget", usage: "/context", duringTurn: true, run: runContextCommand},
		{name: "/compact", description: "compact older context", usage: "/compact", run: runCompactCommand},
		// Logout sits with exit rather than next to login: it is rare, and the
		// suggestion list is ordered by how often a command is reached for.
		{name: "/logout", description: "remove stored credentials", usage: "/logout [provider]", maxArgs: 1, duringTurn: true, run: runLogoutCommand},
		{name: "/exit", aliases: []string{"/quit", "/q"}, description: "exit Skot", usage: "/exit", duringTurn: true, run: runExitCommand},
	}
}

var scopePickerItems = []pickerItem{
	{value: "workspace", label: "workspace", description: "keep model-owned file access in the workspace"},
	{value: "machine", label: "machine", description: "allow model-owned file access outside the workspace"},
}

func nextScope(current string) string {
	for index, item := range scopePickerItems {
		if strings.EqualFold(item.value, current) {
			return scopePickerItems[(index+1)%len(scopePickerItems)].value
		}
	}
	return scopePickerItems[0].value
}

var themePickerItems = []pickerItem{
	{value: ThemeAuto, label: ThemeAuto, description: "detect the terminal background"},
	{value: ThemeLight, label: ThemeLight, description: "colors for a light background"},
	{value: ThemeDark, label: ThemeDark, description: "colors for a dark background"},
}

var displayPickerItems = []pickerItem{
	{value: DisplayCompact, label: DisplayCompact, description: "show live work, then fold it into a compact conversation"},
	{value: DisplayDetailed, label: DisplayDetailed, description: "keep tool activity and turn details in the transcript"},
	{value: DisplayFull, label: DisplayFull, description: "show each tool call without UI preview limits"},
}

func (m *screenModel) syncCommandSuggestions() {
	if m.pathPrompt != notFilesystemPath {
		m.composer.setSuggestionCandidates(pathCompletionCandidates(&m.pathCompletion, m.config.Root, m.composer.value()))
		return
	}
	if m.modelContextSelection.uri != "" {
		m.composer.setSuggestionCandidates(nil)
		return
	}
	value := strings.ToLower(strings.TrimLeft(m.composer.value(), " \t"))
	var candidates []string
	switch {
	case strings.HasPrefix(value, "/tools "):
		for _, toolSet := range m.agent.ToolSets() {
			candidates = append(candidates, "/tools "+toolSet)
		}
	case strings.HasPrefix(value, "/scope "):
		for _, item := range scopePickerItems {
			candidates = append(candidates, "/scope "+item.value)
		}
	case strings.HasPrefix(value, "/theme "):
		for _, item := range themePickerItems {
			candidates = append(candidates, "/theme "+item.value)
		}
	case strings.HasPrefix(value, "/display "):
		for _, item := range displayPickerItems {
			candidates = append(candidates, "/display "+item.value)
		}
	case strings.HasPrefix(value, "/model "):
		for _, choice := range m.modelChoices {
			if choice.Unavailable {
				continue
			}
			candidates = append(candidates, "/model "+choice.URI)
		}
	case strings.HasPrefix(value, "/login "), strings.HasPrefix(value, "/logout "):
		command := "/login "
		if strings.HasPrefix(value, "/logout ") {
			command = "/logout "
		}
		for _, provider := range m.providers {
			candidates = append(candidates, command+provider.Name)
		}
	default:
		for _, command := range tuiCommands {
			candidates = append(candidates, command.name)
		}
	}
	m.composer.setSuggestionCandidates(candidates)
}

func (m screenModel) commandSuggestionsVisible() bool {
	value := strings.TrimSpace(m.composer.value())
	naming := m.pathPrompt != notFilesystemPath || strings.HasPrefix(value, "/")
	return m.loginProvider == "" && !m.maintenanceOperation().isMaintenance() && !m.picker.active() && naming && m.composer.hasSuggestions()
}

func (m screenModel) currentCommandSuggestion() string {
	return m.composer.currentSuggestion()
}

func (m *screenModel) moveCommandSuggestion(delta int) {
	m.composer.moveSuggestion(delta)
}

func (m screenModel) selectedInput() string {
	if m.loginProvider != "" {
		return strings.TrimSpace(m.secret.Value())
	}
	value := strings.TrimSpace(m.composer.value())
	if m.commandSuggestionsVisible() {
		suggestion := m.currentCommandSuggestion()
		if strings.HasPrefix(strings.ToLower(suggestion), strings.ToLower(value)) {
			return suggestion
		}
	}
	return value
}

func (m screenModel) renderCommandSuggestions() []string {
	if !m.commandSuggestionsVisible() {
		return nil
	}
	limit := min(7, max(0, m.height-m.composer.height()-5))
	if limit == 0 {
		return nil
	}
	suggestions, selected := m.composer.suggestionWindow(limit)
	lines := make([]string, 0, len(suggestions))
	for index, candidate := range suggestions {
		marker := " "
		if index == selected {
			marker = userMarker
		}
		label := sanitizeTerminalText(candidate)
		if index == selected {
			label = m.accentStyle.Render(label)
		}
		if description := commandDescription(candidate); description != "" {
			if pad := 10 - visibleLen(label); pad > 0 {
				label += strings.Repeat(" ", pad)
			}
			label += m.mutedStyle.Render(" " + sanitizeTerminalText(description))
		}
		lines = append(lines, marker+strings.Repeat(" ", transcriptGutter-1)+label)
	}
	return lines
}

func commandDescription(candidate string) string {
	name, _, _ := strings.Cut(candidate, " ")
	for _, command := range tuiCommands {
		if command.name == name {
			return command.description
		}
	}
	return ""
}

func (m *screenModel) dispatchCommand(input string) (tea.Cmd, bool) {
	if !strings.HasPrefix(input, "/") {
		return nil, false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return nil, false
	}
	var selected *tuiCommand
	for index := range tuiCommands {
		if tuiCommands[index].matches(fields[0]) {
			selected = &tuiCommands[index]
			break
		}
	}
	if selected == nil {
		if !looksLikeCommandName(fields[0]) {
			return nil, false
		}
		m.addBlock(screenBlockError, "unknown command: "+input)
		m.refreshTranscript()
		return nil, true
	}
	if m.operation.kind == operationCompaction {
		m.addBlock(screenBlockError, "commands are unavailable while compacting; wait or cancel compaction")
		m.refreshTranscript()
		return nil, true
	}
	if m.operation.isTurn() && !selected.duringTurn {
		m.addBlock(screenBlockError, "commands are unavailable while Skot is working; wait or cancel the turn")
		m.refreshTranscript()
		return nil, true
	}
	args := fields[1:]
	if len(args) < selected.minArgs || len(args) > selected.maxArgs {
		m.addBlock(screenBlockError, "usage: "+selected.usage)
		m.refreshTranscript()
		return nil, true
	}
	command := selected.run(m, input, args)
	m.refreshTranscript()
	return command, true
}

// looksLikeCommandName reports whether a leading slash can only have started a
// command name: the slash alone, or the slash and one plain word. Text which a
// slash merely begins, an absolute path above all, is ordinary input and has
// to reach the model instead of failing as a command.
func looksLikeCommandName(field string) bool {
	for _, character := range strings.TrimPrefix(field, "/") {
		named := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character == '-'
		if !named {
			return false
		}
	}
	return true
}

func (command tuiCommand) matches(value string) bool {
	if strings.EqualFold(command.name, value) {
		return true
	}
	for _, alias := range command.aliases {
		if strings.EqualFold(alias, value) {
			return true
		}
	}
	return false
}

func (m *screenModel) acceptCommand(input string) {
	m.composer.reset()
	m.composer.remember(input)
}

func runHelpCommand(m *screenModel, _ string, _ []string) tea.Cmd {
	m.composer.reset()
	m.addBlock(screenBlockSystem, m.tuiCommandHelp())
	return nil
}

func runClearCommand(m *screenModel, input string, _ []string) tea.Cmd {
	m.acceptCommand(input)
	return m.startSessionAction(sessionActionClear)
}

func runResumeCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openSessionPicker()
		return nil
	}
	m.acceptCommand(input)
	return resumeSessionCmd(args[0])
}

func runLoginCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openLoginPicker()
		return nil
	}
	m.acceptCommand(input)
	return m.startProviderLogin(args[0], modelSelection{}, pickerState{})
}

func runLogoutCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openLogoutPicker()
		return nil
	}
	m.acceptCommand(input)
	return m.logoutProvider(args[0])
}

func runModelCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openModelPicker()
		return nil
	}
	m.acceptCommand(input)
	selection := m.modelSelectionForURI(args[0])
	if len(args) > 1 {
		selection.api = args[1]
	}
	if strings.EqualFold(selection.uri, m.agent.CurrentModel()) {
		selection.effort = m.agent.CurrentReasoningEffort()
	}
	return m.selectModel(selection, pickerState{})
}

func formatSettingChange(name, before, after string) string {
	if before == "" || before == after {
		return name + ": " + after
	}
	return name + ": " + before + " → " + after
}

// Include the cache warning for direct /tools commands, which skip the picker.
func toolSetChangeNotice(before, after string) string {
	notice := formatSettingChange("tools", before, after)
	if before != "" && before != after {
		notice += " · prompt cache reset, next message costs full price"
	}
	return notice
}

func runToolsCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openToolSetPicker()
		return nil
	}
	if m.selectToolSet(args[0]) {
		m.acceptCommand(input)
	}
	return nil
}

// selectToolSet reports both the live change and any failure to save it.
// The return value tells command input whether the selection was applied.
func (m *screenModel) selectToolSet(value string) bool {
	before := m.agent.CurrentToolSet()
	switchErr := m.agent.SwitchToolSet(m.ctx, value)
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		m.addBlock(screenBlockError, "tools: "+switchErr.Error())
		return false
	}
	m.refreshSessionStatus()
	notice := toolSetChangeNotice(before, m.agent.CurrentToolSet())
	if m.operation.isTurn() {
		notice += " · applies before the next model request"
	}
	m.addBlock(screenBlockSystem, notice)
	if switchErr != nil {
		m.addBlock(screenBlockError, "tools: "+switchErr.Error())
	}
	return true
}

func runScopeCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openScopePicker()
		return nil
	}
	m.acceptCommand(input)
	return m.startScopeSwitch(args[0])
}

func runThemeCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openThemePicker()
		return nil
	}
	command, applied := m.selectTheme(args[0])
	if applied {
		m.acceptCommand(input)
	}
	return command
}

func (m *screenModel) selectTheme(value string) (tea.Cmd, bool) {
	before := m.theme
	command, err := m.switchTerminalTheme(value)
	if err != nil && !preferenceAppliedDespiteError(err) {
		m.addBlock(screenBlockError, "theme: "+err.Error())
		return nil, false
	}
	m.addBlock(screenBlockSystem, formatSettingChange("theme", before, m.theme))
	if err != nil {
		m.addBlock(screenBlockError, "theme: "+err.Error())
	}
	return command, true
}

func runDisplayCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openDisplayPicker()
		return nil
	}
	if m.selectDisplay(args[0]) {
		m.acceptCommand(input)
	}
	return nil
}

func (m *screenModel) selectDisplay(value string) bool {
	err := m.switchTranscriptDisplay(value)
	if err != nil && !preferenceAppliedDespiteError(err) {
		m.addBlock(screenBlockError, "display: "+err.Error())
		return false
	}
	m.addBlock(screenBlockSystem, m.displayNotice(m.displayProfile))
	if err != nil {
		m.addBlock(screenBlockError, "display: "+err.Error())
	}
	return true
}

func runContextCommand(m *screenModel, input string, _ []string) tea.Cmd {
	m.acceptCommand(input)
	m.refreshSessionStatus()
	m.addBlock(screenBlockSystem, formatContextReport(m.sessionStatus.ContextReport))
	return nil
}

func runCompactCommand(m *screenModel, input string, _ []string) tea.Cmd {
	m.acceptCommand(input)
	return m.startCompaction()
}

func runExitCommand(m *screenModel, _ string, _ []string) tea.Cmd {
	return m.startSessionAction(sessionActionExit)
}
