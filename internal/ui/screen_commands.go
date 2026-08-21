package ui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
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

// tuiCommandHelp deliberately omits the command list: typing "/" already shows
// it as a vertical menu with descriptions, and that menu also completes the
// command. Help covers what the menu cannot — keys, shell escapes, and what
// input does while a turn is running.
const tuiCommandHelp = `Keys
  enter                    send
  shift/alt+enter, ctrl+j  insert a newline
  tab                      accept the suggestion
  up/down, ctrl+p/ctrl+n   walk input history
  ctrl+a/e/b/f             start, end, back, forward
  ctrl+u/k/w               erase to start, to end, word
  esc                      cancel the running turn
  ctrl+c                   cancel the turn, else exit
  ctrl+d                   exit when the input is empty

Shell
  ! command                keeps the result in context
  !! command               private, keeps nothing

While Skot is working
  enter                    queue for the next turn
  alt+up                   take the last one back

Type / for commands.`

func init() {
	// Assigning the function-valued table here avoids a package initialization
	// cycle through command handlers which refresh suggestions from this table.
	tuiCommands = []tuiCommand{
		{name: "/help", description: "show keys", usage: "/help", run: runHelpCommand},
		{name: "/clear", description: "start a new session", usage: "/clear", run: runClearCommand},
		{name: "/resume", description: "choose or resume a previous session", usage: "/resume [id-or-prefix]", maxArgs: 1, run: runResumeCommand},
		{name: "/login", description: "store a provider or service API key", usage: "/login [provider]", maxArgs: 1, run: runLoginCommand},
		{name: "/model", description: "list or switch models", usage: "/model [provider/model [api]]", maxArgs: 2, run: runModelCommand},
		{name: "/tools", description: "show or switch the active tool set", usage: "/tools [name]", maxArgs: 1, run: runToolsCommand},
		{name: "/scope", description: "show or switch filesystem scope", usage: "/scope [auto|workspace|machine]", maxArgs: 1, duringTurn: true, run: runScopeCommand},
		{name: "/theme", description: "show or switch the terminal theme", usage: "/theme [auto|light|dark]", maxArgs: 1, duringTurn: true, run: runThemeCommand},
		{name: "/context", description: "show context budget", usage: "/context", run: runContextCommand},
		{name: "/compact", description: "compact older context", usage: "/compact", run: runCompactCommand},
		// Logout sits with exit rather than next to login: it is rare, and the
		// suggestion list is ordered by how often a command is reached for.
		{name: "/logout", description: "remove a stored API key", usage: "/logout [provider]", maxArgs: 1, run: runLogoutCommand},
		{name: "/exit", aliases: []string{"/quit", "/q"}, description: "exit Skot", usage: "/exit", run: runExitCommand},
	}
}

var scopePickerItems = []pickerItem{
	{value: "auto", label: "auto", description: "workspace on a host, machine inside a container"},
	{value: "workspace", label: "workspace", description: "keep model-owned file access in the workspace"},
	{value: "machine", label: "machine", description: "allow model-owned file access outside the workspace"},
}

var themePickerItems = []pickerItem{
	{value: ThemeAuto, label: ThemeAuto, description: "detect the terminal background"},
	{value: ThemeLight, label: ThemeLight, description: "colors for a light background"},
	{value: ThemeDark, label: ThemeDark, description: "colors for a dark background"},
}

func (m *screenModel) syncCommandSuggestions() {
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
	return m.loginProvider == "" && !m.operation.isMaintenance() && !m.picker.active() && strings.HasPrefix(value, "/") && m.composer.hasSuggestions()
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
		if m.operation.isTurn() {
			m.addBlock(screenBlockError, "commands are unavailable while Skot is working; wait or cancel the turn")
		} else {
			m.addBlock(screenBlockError, "unknown command: "+input)
		}
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
	m.addBlock(screenBlockSystem, tuiCommandHelp)
	return nil
}

func runClearCommand(m *screenModel, input string, _ []string) tea.Cmd {
	m.acceptCommand(input)
	m.clearSession()
	return nil
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
	m.startProviderLogin(args[0], modelSelection{}, pickerState{})
	return nil
}

func runLogoutCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openLogoutPicker()
		return nil
	}
	m.acceptCommand(input)
	m.logoutProvider(args[0])
	return nil
}

func runModelCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openModelPicker()
		return nil
	}
	m.acceptCommand(input)
	selection := modelSelection{uri: args[0]}
	if len(args) > 1 {
		selection.api = args[1]
	}
	if strings.EqualFold(selection.uri, m.agent.CurrentModel()) {
		selection.effort = m.agent.CurrentReasoningEffort()
	}
	m.selectModel(selection, pickerState{})
	return nil
}

// formatSettingChange reports where a setting landed, naming where it came from
// when that differs. The arrow is reserved for an actual transition: re-selecting
// the current value is not a live transition, and claiming a change that did
// not happen is worse than saying nothing.
func formatSettingChange(name, before, after string) string {
	if before == "" || before == after {
		return name + ": " + after
	}
	return name + ": " + before + " → " + after
}

// toolSetChangeNotice names the cost that follows a tool set change. The tool
// list is part of the cached prefix, so replacing it restarts the cache; the
// picker warns before the fact, and this covers `/tools <name>`, which never
// opens the picker. A no-op switch costs nothing and so says nothing.
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
	before := m.agent.CurrentToolSet()
	switchErr := m.agent.SwitchToolSet(m.ctx, args[0])
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		m.addBlock(screenBlockError, "tools: "+switchErr.Error())
		return nil
	}
	m.acceptCommand(input)
	m.refreshSessionStatus()
	m.addBlock(screenBlockSystem, toolSetChangeNotice(before, m.agent.CurrentToolSet()))
	if switchErr != nil {
		m.addBlock(screenBlockError, "tools: "+switchErr.Error())
	}
	return nil
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
	before := m.theme
	command, err := m.switchTerminalTheme(args[0])
	if err != nil && !preferenceAppliedDespiteError(err) {
		m.addBlock(screenBlockError, "theme: "+err.Error())
		return nil
	}
	m.acceptCommand(input)
	m.addBlock(screenBlockSystem, formatSettingChange("theme", before, m.theme))
	if err != nil {
		m.addBlock(screenBlockError, "theme: "+err.Error())
	}
	return command
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
	m.quitting = true
	return tea.Quit
}

func (m *screenModel) openModelPicker() {
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "model: "+err.Error())
		return
	}
	credentials := make(map[string]ProviderStatus, len(m.providers))
	for _, status := range m.providers {
		credentials[status.Name] = status
	}
	current := m.agent.CurrentModel()
	currentEffort := m.agent.CurrentReasoningEffort()
	var currentItems, availableItems, loginItems []pickerItem
	var unavailable []ModelChoice
	for _, choice := range m.modelChoices {
		if choice.Unavailable {
			unavailable = append(unavailable, choice)
			continue
		}
		model := choice.URI
		status := credentials[modelProvider(model)]
		efforts := orderedReasoningEfforts(choice.ReasoningEfforts)
		// A route the session is not on starts at the provider default, which
		// sits in the middle of the ladder rather than at its first rung.
		selected := ""
		if strings.EqualFold(model, current) {
			selected = currentEffort
		}
		effortIndex := slices.Index(efforts, selected)
		if effortIndex < 0 {
			effortIndex = max(0, slices.Index(efforts, ""))
		}
		description := ""
		loginRequired := status.Source == "none"
		if loginRequired {
			description = "login required"
		}
		item := pickerItem{
			value: model, label: model,
			description:  description,
			activeDetail: modelChoiceActiveDetail(choice),
			dimmed:       loginRequired,
			efforts:      efforts, effortIndex: effortIndex,
		}
		switch {
		case strings.EqualFold(model, current):
			currentItems = append(currentItems, item)
		case loginRequired:
			loginItems = append(loginItems, item)
		default:
			availableItems = append(availableItems, item)
		}
	}
	items := make([]pickerItem, 0, len(currentItems)+len(availableItems)+len(loginItems)+2)
	items = append(items, currentItems...)
	items = append(items, availableItems...)
	items = append(items, loginItems...)
	if len(unavailable) != 0 {
		known := fmt.Sprintf("%d known routes", len(unavailable))
		if len(unavailable) == 1 {
			known = "1 known route"
		}
		items = append(items, pickerItem{
			label: "Unavailable routes…", description: known,
			details: unavailableModelDetails(unavailable),
		})
	}
	items = append(items, pickerItem{label: "Enter model URI…", description: "provider/model", custom: true})
	m.openPicker(pickerModel, items, markCurrentPickerItem(items, current))
}

// askModelAPI offers the protocols Skot implements for a route it does not
// describe. It appears only after a URI has been entered and only for the
// gateways which serve more than one protocol, so an ordinary selection never
// meets it.
func (m *screenModel) askModelAPI(selection modelSelection) {
	items := modelAPIPickerItems(m.modelChoices, modelProvider(selection.uri))
	m.addBlock(screenBlockSystem, selection.uri+" is not in Skot's model list; choose the API it speaks")
	m.openPicker(pickerModelAPI, items, 0)
	m.picker.pendingModel = selection
}

// cancelModelAPIChoice leaves the selection unchanged and names the typed form
// of the same answer, so declining the list is not a dead end.
func (m *screenModel) cancelModelAPIChoice(selection modelSelection) {
	m.addBlock(screenBlockError, fmt.Sprintf(
		"model: %s needs the API it speaks; retry with /model %s %s",
		selection.uri, selection.uri, modelAPIChatCompletions,
	))
}

// The protocol names a user types or picks. They are the same vocabulary the
// -model-api flag documents, which is why the frontend spells them out rather
// than deriving them.
const (
	modelAPIChatCompletions   = "chat_completions"
	modelAPIResponses         = "responses"
	modelAPIAnthropicMessages = "anthropic_messages"
)

func modelAPIPickerItems(choices []ModelChoice, provider string) []pickerItem {
	items := make([]pickerItem, 0, 3)
	for _, protocol := range []string{modelAPIChatCompletions, modelAPIResponses, modelAPIAnthropicMessages} {
		items = append(items, pickerItem{
			value: protocol, label: modelProtocolLabel(protocol),
			description: modelAPIExamples(choices, provider, protocol),
		})
	}
	return items
}

// modelAPIExamples names routes of the same gateway which already speak a
// protocol. A model is usually recognizable by the company it keeps, and these
// are the only evidence Skot has to offer.
func modelAPIExamples(choices []ModelChoice, provider, protocol string) string {
	var names []string
	for _, choice := range choices {
		if choice.Unavailable || choice.ProtocolExplicit || choice.Protocol != protocol {
			continue
		}
		if modelProvider(choice.URI) != provider {
			continue
		}
		if _, model, ok := strings.Cut(choice.URI, "/"); ok {
			names = append(names, model)
		}
		if len(names) == 2 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "like " + strings.Join(names, ", ")
}

// orderedReasoningEfforts puts the provider default in the middle of the
// ladder. Efforts are declared weakest first, and the empty one means "let the
// provider decide" — a point the scale cannot locate. At the front it made ←/→
// read backwards: one step right off the default landed on the weakest level.
func orderedReasoningEfforts(efforts []string) []string {
	explicit := make([]string, 0, len(efforts))
	hasDefault := false
	for _, effort := range efforts {
		if effort == "" {
			hasDefault = true
			continue
		}
		explicit = append(explicit, effort)
	}
	if len(explicit) == 0 {
		return []string{""}
	}
	if !hasDefault {
		return explicit
	}
	middle := len(explicit) / 2
	ordered := make([]string, 0, len(explicit)+1)
	ordered = append(ordered, explicit[:middle]...)
	ordered = append(ordered, "")
	return append(ordered, explicit[middle:]...)
}

func unavailableModelDetails(choices []ModelChoice) string {
	lines := []string{"Unavailable model routes:"}
	for _, choice := range choices {
		label := choice.URI
		if name := strings.TrimSpace(choice.Name); name != "" {
			label = name + " (" + choice.URI + ")"
		}
		description := modelChoiceDiagnosticDescription(choice)
		if description != "" {
			label += " · " + description
		}
		if reason := strings.TrimSpace(choice.UnavailableReason); reason != "" {
			label += " · " + reason
		}
		lines = append(lines, "- "+label)
	}
	return strings.Join(lines, "\n")
}

func modelChoiceDescription(choice ModelChoice) string {
	if choice.ContextWindowEstimated {
		if choice.ContextWindow > 0 {
			return "~" + formatModelTokenCount(choice.ContextWindow) + " context"
		}
		return "context unknown"
	}
	if choice.ContextWindow > 0 {
		return formatModelTokenCount(choice.ContextWindow) + " context"
	}
	return ""
}

// modelChoiceActiveDetail names the protocol only for a route whose protocol
// the user chose. For every other row it is a reviewed fact the user cannot act
// on, and it belongs in diagnostics rather than beside the selection.
func modelChoiceActiveDetail(choice ModelChoice) string {
	description := modelChoiceDescription(choice)
	if !choice.ProtocolExplicit {
		return description
	}
	return appendDescription(description, modelProtocolLabel(choice.Protocol))
}

func modelProtocolLabel(protocol string) string {
	return strings.ReplaceAll(strings.TrimSpace(protocol), "_", " ")
}

func modelChoiceDiagnosticDescription(choice ModelChoice) string {
	description := modelChoiceDescription(choice)
	if protocol := modelProtocolLabel(choice.Protocol); protocol != "" {
		description = appendDescription(protocol, description)
	}
	return description
}

func formatModelTokenCount(tokens int) string {
	if tokens < 1_000_000 {
		return fmt.Sprintf("%dK", (tokens+500)/1000)
	}
	millions := float64(tokens) / 1_000_000
	value := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", millions), "0"), ".")
	return value + "M"
}

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

func (m *screenModel) openScopePicker() {
	items := append([]pickerItem(nil), scopePickerItems...)
	m.openPicker(pickerScope, items, markCurrentPickerItem(items, m.agent.CurrentScope()))
}

func (m *screenModel) openThemePicker() {
	items := append([]pickerItem(nil), themePickerItems...)
	m.openPicker(pickerTheme, items, markCurrentPickerItem(items, m.theme))
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

// pickerNoteFor is a caveat about the act of choosing, shown once under the
// rows. It lives here rather than in a row description because it is true of
// every row except the current one, and the descriptions are already carrying
// what distinguishes the rows from each other.
func pickerNoteFor(kind pickerKind) string {
	switch kind {
	case pickerToolSet:
		return "switching resets the prompt cache, so the next message costs full price"
	case pickerModelAPI:
		return "a wrong protocol fails on the first request"
	}
	return ""
}

// currentPickerMark flags the active row. Its width is reserved on every row of
// an aligned picker so the description column does not depend on which row is
// current.
const currentPickerMark = "  ✓"

// pickerAlignsDescriptions reports whether descriptions share a column. It pays
// off where the labels are short and the descriptions invite comparison: the
// tool sets are nested, so a common left edge shows what each step adds.
// Search-filtered pickers are excluded, since the column would move while typing.
func pickerAlignsDescriptions(kind pickerKind) bool {
	switch kind {
	case pickerToolSet, pickerScope, pickerTheme:
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
	case pickerModelAPI, pickerToolSet, pickerScope, pickerTheme, pickerLogin, pickerSession:
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
	for _, index := range visible {
		if index == picker.index {
			return
		}
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
	return pickerNavigationFor(picker.kind) == navigationNumbers && len(picker.items) > 0 && len(picker.items) <= 9
}

func (m screenModel) handlePickerKey(message tea.KeyPressMsg) (screenModel, tea.Cmd) {
	key := message.Key()
	digit := pickerDigit(message)
	navigation := pickerNavigationFor(m.picker.kind)
	switch {
	case isEscapeKey(message) || isInterruptKey(message):
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
	case navigation == navigationSearch && keyIsCtrl(message, 'u'):
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
	case m.picker.numberSelectionEnabled() && digit > 0:
		if digit <= len(m.picker.items) {
			m.picker.index = digit - 1
			return m.selectPickerItem()
		}
		return m, nil
	case isEnterKey(message):
		return m.selectPickerItem()
	default:
		return m, nil
	}
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
		m.selectModel(modelSelection{uri: item.value, effort: selectedModelEffort(item)}, picker)
	case pickerModelAPI:
		selection := picker.pendingModel
		selection.api = item.value
		m.switchModel(selection)
	case pickerToolSet:
		before := m.agent.CurrentToolSet()
		switchErr := m.agent.SwitchToolSet(m.ctx, item.value)
		if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
			m.addBlock(screenBlockError, "tools: "+switchErr.Error())
		} else {
			m.refreshSessionStatus()
			m.addBlock(screenBlockSystem, toolSetChangeNotice(before, m.agent.CurrentToolSet()))
			if switchErr != nil {
				m.addBlock(screenBlockError, "tools: "+switchErr.Error())
			}
		}
	case pickerScope:
		return m, m.startScopeSwitch(item.value)
	case pickerTheme:
		before := m.theme
		command, err := m.switchTerminalTheme(item.value)
		if err != nil && !preferenceAppliedDespiteError(err) {
			m.addBlock(screenBlockError, "theme: "+err.Error())
		} else {
			m.addBlock(screenBlockSystem, formatSettingChange("theme", before, m.theme))
			if err != nil {
				m.addBlock(screenBlockError, "theme: "+err.Error())
			}
		}
		m.refreshTranscript()
		return m, command
	case pickerLogin:
		pendingModel := ""
		if picker.startupLogin {
			pendingModel = item.modelURI
		}
		m.startProviderLogin(item.value, modelSelection{uri: pendingModel}, pickerState{})
	case pickerLogout:
		m.logoutProvider(item.value)
	case pickerSession:
		return m, resumeSessionCmd(item.value)
	}
	m.refreshTranscript()
	return m, nil
}

func (m *screenModel) refreshProviderStatuses() error {
	providers, err := m.agent.ProviderStatuses()
	if err != nil {
		return err
	}
	m.providers = append(m.providers[:0], providers...)
	m.syncCommandSuggestions()
	return nil
}

func (m *screenModel) openStartupLoginPicker() {
	currentModel := m.agent.CurrentModel()
	for _, choice := range m.modelChoices {
		if strings.EqualFold(choice.URI, currentModel) && choice.Unavailable {
			m.addBlock(screenBlockError, fmt.Sprintf("model %q is unavailable; choose another with /model", currentModel))
			return
		}
	}
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "credentials: "+err.Error())
		return
	}
	provider := modelProvider(currentModel)
	currentMissing := false
	for _, status := range m.providers {
		if status.Name == provider && status.Source == "none" {
			currentMissing = true
			break
		}
	}
	if !currentMissing {
		return
	}
	items := make([]pickerItem, 0, len(m.providers))
	selected := 0
	for _, status := range m.providers {
		modelURI := firstProviderModel(m.modelChoices, status.Name)
		current := status.Name == provider
		if current {
			modelURI = currentModel
			selected = len(items)
		}
		if modelURI == "" {
			continue
		}
		description := credentialSourceDescription(status.Source) + " · " + modelURI
		items = append(items, pickerItem{
			value: status.Name, label: status.Name, description: description, current: current,
			modelURI: modelURI,
		})
	}
	if len(items) == 0 {
		m.addBlock(screenBlockError, "credentials: no model providers are available")
		return
	}
	m.openPicker(pickerLogin, items, selected)
	m.picker.startupLogin = true
}

func (m *screenModel) openLoginPicker() {
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "login: "+err.Error())
		return
	}
	modelItems := make([]pickerItem, 0, len(m.providers))
	toolItems := make([]pickerItem, 0, len(m.providers))
	for _, status := range m.providers {
		description := status.Description + " · " + status.Source
		if status.Source == "none" {
			description = status.Description + " · not configured"
		}
		item := pickerItem{
			value: status.Name, label: status.Name, description: description,
		}
		if status.ToolService {
			toolItems = append(toolItems, item)
		} else {
			modelItems = append(modelItems, item)
		}
	}
	if len(modelItems) != 0 && len(toolItems) != 0 {
		toolItems[0].dividerBefore = true
	}
	items := append(modelItems, toolItems...)
	if len(items) == 0 {
		m.addBlock(screenBlockError, "login: no providers are available")
		return
	}
	m.openPicker(pickerLogin, items, 0)
}

func (m *screenModel) openLogoutPicker() {
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "logout: "+err.Error())
		return
	}
	var items []pickerItem
	for _, status := range m.providers {
		if status.Source == "auth store" {
			items = append(items, pickerItem{value: status.Name, label: status.Name, description: status.Description})
		}
	}
	if len(items) == 0 {
		m.composer.reset()
		m.addBlock(screenBlockSystem, "no stored provider credentials")
		return
	}
	m.openPicker(pickerLogout, items, 0)
}

func (m *screenModel) startProviderLogin(provider string, pending modelSelection, returnPicker pickerState) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "login: "+err.Error())
		return
	}
	for _, status := range m.providers {
		if status.Name != provider {
			continue
		}
		pending.uri = strings.TrimSpace(pending.uri)
		if strings.EqualFold(pending.uri, m.agent.CurrentModel()) && pending.effort == m.agent.CurrentReasoningEffort() {
			pending.uri = ""
		}
		if pending.uri != "" && status.Source != "none" {
			m.switchModel(pending)
			return
		}
		if status.Source == "environment override" {
			m.addBlock(screenBlockSystem, provider+" is supplied by an environment override")
			return
		}
		m.loginProvider = provider
		m.loginSelection = pending
		m.loginReturn = returnPicker
		m.secret.Reset()
		m.secret.EchoMode = textinput.EchoPassword
		m.secret.Placeholder = provider + " API key"
		message := "enter " + provider + " API key (input is hidden)"
		if status.Source == "auth store" {
			message = "enter a new " + provider + " API key (input is hidden)"
		}
		if status.CredentialURL != "" {
			message += "; create or manage keys at " + status.CredentialURL
		}
		m.addBlock(screenBlockSystem, message)
		return
	}
	m.addBlock(screenBlockError, "login: unsupported provider "+provider)
}

func (m *screenModel) selectModel(selection modelSelection, returnPicker pickerState) {
	selection.uri = strings.TrimSpace(selection.uri)
	provider := modelProvider(selection.uri)
	if provider == "" {
		m.switchModel(selection)
		return
	}
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "model: "+err.Error())
		return
	}
	for _, status := range m.providers {
		if status.Name == provider {
			if strings.EqualFold(selection.uri, m.agent.CurrentModel()) && selection.effort == m.agent.CurrentReasoningEffort() && status.Source != "none" {
				m.switchModel(selection)
				return
			}
			m.startProviderLogin(provider, selection, returnPicker)
			return
		}
	}
	m.switchModel(selection)
}

func (m *screenModel) switchModel(selection modelSelection) {
	before := m.agent.CurrentModel()
	switchErr := m.agent.SwitchModel(m.ctx, selection.uri, selection.effort, selection.api)
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		// A route whose gateway serves several protocols needs one fact Skot
		// does not have. Asking for it here keeps the answer attached to the
		// selection being made instead of to the whole process.
		if selection.api == "" && app.IsModelAPIRequired(switchErr) {
			m.askModelAPI(selection)
			return
		}
		m.addBlock(screenBlockError, "model: "+switchErr.Error())
		return
	}
	current := m.agent.CurrentModel()
	m.refreshSessionStatus()
	m.refreshModelChoices()
	m.addBlock(screenBlockSystem, formatSettingChange("model", before, current))
	if switchErr != nil {
		m.addBlock(screenBlockError, "model: "+switchErr.Error())
	}
}

func (m *screenModel) cycleModelEffort(delta int) {
	if m.picker.index < 0 || m.picker.index >= len(m.picker.items) {
		return
	}
	item := &m.picker.items[m.picker.index]
	if len(item.efforts) <= 1 {
		return
	}
	// The ends clamp instead of wrapping: wrapping put the cheapest and the
	// most expensive effort one keypress apart from the starting position.
	item.effortIndex = min(max(0, item.effortIndex+delta), len(item.efforts)-1)
}

func selectedModelEffort(item pickerItem) string {
	if item.effortIndex < 0 || item.effortIndex >= len(item.efforts) {
		return ""
	}
	return item.efforts[item.effortIndex]
}

func credentialSourceDescription(source string) string {
	switch source {
	case "auth store":
		return "stored credential"
	case "environment override":
		return "environment credential"
	case "none":
		return "login required"
	default:
		return "credential status unknown"
	}
}

func firstProviderModel(choices []ModelChoice, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, choice := range choices {
		if !choice.Unavailable && modelProvider(choice.URI) == provider {
			return choice.URI
		}
	}
	return ""
}

func (m *screenModel) logoutProvider(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := m.agent.Logout(m.ctx, provider); err != nil {
		m.addBlock(screenBlockError, "logout: "+err.Error())
		return
	}
	m.addBlock(screenBlockSystem, "logged out of "+provider)
	_ = m.refreshProviderStatuses()
}

func modelProvider(uri string) string {
	provider, model, ok := strings.Cut(strings.TrimSpace(uri), "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

func (m *screenModel) closePicker() {
	m.picker = pickerState{}
	m.composer.reset()
	m.syncCommandSuggestions()
}

func (m *screenModel) refreshModelChoices() {
	m.modelChoices = m.agent.ModelChoices()
	m.syncCommandSuggestions()
}

func (m screenModel) renderPicker() []string {
	searchable := pickerNavigationFor(m.picker.kind) == navigationSearch
	note := pickerNoteFor(m.picker.kind)
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
	if note != "" {
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
		if pad := descriptionColumn - visibleLen(label); pad > 0 {
			label += strings.Repeat(" ", pad)
		}
		description := item.description
		if index == m.picker.index {
			description = appendDescription(item.activeDetail, description)
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
			shortcut = m.mutedStyle.Render(fmt.Sprintf("%d ", index+1))
			availableWidth = max(1, availableWidth-2)
		}
		lines = append(lines, marker+strings.Repeat(" ", transcriptGutter-1)+shortcut+truncateANSI(label, availableWidth))
	}
	if note != "" {
		lines = append(lines, "", strings.Repeat(" ", transcriptGutter)+m.warningStyle.Render(note))
	}
	return lines
}
