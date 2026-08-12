package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type tuiCommand struct {
	name        string
	aliases     []string
	synopsis    string
	description string
	usage       string
	minArgs     int
	maxArgs     int
	duringTurn  bool
	run         func(*screenModel, string, []string) tea.Cmd
}

var tuiCommands []tuiCommand
var tuiCommandHelp string

func init() {
	// Assigning the function-valued table here avoids a package initialization
	// cycle through command handlers which refresh suggestions from this table.
	tuiCommands = []tuiCommand{
		{name: "/help", synopsis: "/help", description: "show commands and keyboard shortcuts", usage: "/help", run: runHelpCommand},
		{name: "/clear", synopsis: "/clear", description: "start a new session", usage: "/clear", run: runClearCommand},
		{name: "/resume", synopsis: "/resume [id-or-prefix]", description: "choose or resume a previous session", usage: "/resume [id-or-prefix]", maxArgs: 1, run: runResumeCommand},
		{name: "/login", synopsis: "/login [provider]", description: "store a provider or service API key", usage: "/login [provider]", maxArgs: 1, run: runLoginCommand},
		{name: "/logout", synopsis: "/logout [provider]", description: "remove a stored API key", usage: "/logout [provider]", maxArgs: 1, run: runLogoutCommand},
		{name: "/model", synopsis: "/model [provider/model]", description: "list or switch models", usage: "/model [provider/model]", maxArgs: 1, run: runModelCommand},
		{name: "/profile", synopsis: "/profile [name]", description: "show or switch the tool profile", usage: "/profile [name]", maxArgs: 1, run: runProfileCommand},
		{name: "/sandbox", synopsis: "/sandbox [auto|workspace|masked|off]", description: "show or switch model filesystem isolation", usage: "/sandbox [auto|workspace|masked|off]", maxArgs: 1, duringTurn: true, run: runSandboxCommand},
		{name: "/context", synopsis: "/context", description: "show context budget", usage: "/context", run: runContextCommand},
		{name: "/compact", synopsis: "/compact", description: "compact older context", usage: "/compact", run: runCompactCommand},
		{name: "/exit", aliases: []string{"/quit", "/q"}, synopsis: "/exit", description: "exit Skot", usage: "/exit", run: runExitCommand},
	}
	synopses := make([]string, 0, len(tuiCommands))
	for _, command := range tuiCommands {
		synopses = append(synopses, command.synopsis)
	}
	tuiCommandHelp = "Commands: " + strings.Join(synopses, " · ") + "\n\nEnter sends · Shift/Alt+Enter or Ctrl+J inserts a newline · ! runs shell in context · !! runs private shell · Esc cancels · Alt+Up recalls queued input · Ctrl+C exits"
}

var sandboxPickerItems = []pickerItem{
	{value: "auto", label: "auto", description: "workspace on a host, masked in a container"},
	{value: "workspace", label: "workspace", description: "workspace and tool home only; protected paths stay hidden"},
	{value: "masked", label: "masked", description: "ambient filesystem except protected paths"},
	{value: "off", label: "off", description: "ambient model process authority"},
}

func (m *screenModel) syncCommandSuggestions() {
	value := strings.ToLower(strings.TrimLeft(m.composer.value(), " \t"))
	var candidates []string
	switch {
	case strings.HasPrefix(value, "/profile "):
		for _, profile := range m.agent.Profiles() {
			candidates = append(candidates, "/profile "+profile)
		}
	case strings.HasPrefix(value, "/sandbox "):
		for _, item := range sandboxPickerItems {
			candidates = append(candidates, "/sandbox "+item.value)
		}
	case strings.HasPrefix(value, "/model "):
		for _, model := range m.knownModels {
			candidates = append(candidates, "/model "+model)
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
	return !m.operation.isTurn() && m.loginProvider == "" && !m.operation.isMaintenance() && !m.picker.active() && strings.HasPrefix(value, "/") && m.composer.hasSuggestions()
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
		detail := candidate
		if description := commandDescription(candidate); description != "" {
			detail = fmt.Sprintf("%-10s %s", candidate, description)
		}
		lines = append(lines, marker+strings.Repeat(" ", transcriptGutter-1)+truncateANSI(m.mutedStyle.Render(detail), m.contentWidth()))
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
	m.startProviderLogin(args[0], "", "")
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
	effort := ""
	if strings.EqualFold(args[0], m.agent.CurrentModel()) {
		effort = m.agent.CurrentReasoningEffort()
	}
	m.selectModel(args[0], effort)
	return nil
}

func runProfileCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openProfilePicker()
		return nil
	}
	if err := m.agent.SwitchProfile(m.ctx, args[0]); err != nil {
		m.addBlock(screenBlockError, "profile: "+err.Error())
		return nil
	}
	m.acceptCommand(input)
	m.addBlock(screenBlockSystem, "profile: "+m.agent.CurrentProfile())
	return nil
}

func runSandboxCommand(m *screenModel, input string, args []string) tea.Cmd {
	if len(args) == 0 {
		m.composer.remember(input)
		m.openSandboxPicker()
		return nil
	}
	m.acceptCommand(input)
	return m.startSandboxSwitch(args[0])
}

func runContextCommand(m *screenModel, input string, _ []string) tea.Cmd {
	report, err := m.agent.ContextReport(m.ctx)
	if err != nil {
		m.addBlock(screenBlockError, "context: "+err.Error())
		return nil
	}
	m.acceptCommand(input)
	m.addBlock(screenBlockSystem, formatContextReport(report))
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
	items := make([]pickerItem, 0, len(m.knownModels)+1)
	for _, model := range m.knownModels {
		status := credentials[modelProvider(model)]
		efforts := m.agent.ReasoningEfforts(model)
		if len(efforts) == 0 {
			efforts = []string{""}
		}
		effortIndex := 0
		if strings.EqualFold(model, current) {
			for index, effort := range efforts {
				if effort == currentEffort {
					effortIndex = index
					break
				}
			}
		}
		description := ""
		if status.Source == "none" {
			description = "login required"
		}
		item := pickerItem{
			value: model, label: model,
			description: description,
			source:      status.Source, credentialURL: status.CredentialURL,
			efforts: append([]string(nil), efforts...), effortIndex: effortIndex,
		}
		items = append(items, item)
	}
	items = append(items, pickerItem{label: "Enter model URI…", description: "provider/model", custom: true})
	m.openPicker(pickerModel, items, markCurrentPickerItem(items, current))
}

func (m *screenModel) openProfilePicker() {
	profiles := m.agent.Profiles()
	items := make([]pickerItem, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, pickerItem{value: profile, label: profile, description: profileDescription(profile)})
	}
	m.openPicker(pickerProfile, items, markCurrentPickerItem(items, m.agent.CurrentProfile()))
}

func profileDescription(profile string) string {
	switch profile {
	case "read-only":
		return "read and search only"
	case "edit":
		return "read and write, no process execution"
	case "full":
		return "files and process execution"
	default:
		return "custom tool profile"
	}
}

func (m *screenModel) openSandboxPicker() {
	items := append([]pickerItem(nil), sandboxPickerItems...)
	m.openPicker(pickerSandbox, items, markCurrentPickerItem(items, m.agent.CurrentSandbox()))
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

func (m screenModel) handlePickerKey(message tea.KeyPressMsg) (screenModel, tea.Cmd) {
	key := message.Key()
	switch {
	case isEscapeKey(message) || isInterruptKey(message):
		m.closePicker()
		return m, nil
	case isUpKey(message):
		m.picker.index = max(0, m.picker.index-1)
		return m, nil
	case isDownKey(message):
		m.picker.index = min(len(m.picker.items)-1, m.picker.index+1)
		return m, nil
	case m.picker.kind == pickerModel && (key.Code == tea.KeyLeft || key.Code == tea.KeyKpLeft):
		m.cycleModelEffort(-1)
		return m, nil
	case m.picker.kind == pickerModel && (key.Code == tea.KeyRight || key.Code == tea.KeyKpRight):
		m.cycleModelEffort(1)
		return m, nil
	case isEnterKey(message):
		return m.selectPickerItem()
	default:
		return m, nil
	}
}

func (m screenModel) selectPickerItem() (screenModel, tea.Cmd) {
	picker := m.picker
	item := picker.items[picker.index]
	m.closePicker()
	switch picker.kind {
	case pickerModel:
		if item.custom {
			m.composer.setValue("/model ")
			m.composer.cursorEnd()
			m.syncCommandSuggestions()
			return m, nil
		}
		effort := selectedModelEffort(item)
		if item.current && effort == m.agent.CurrentReasoningEffort() && item.source != "none" {
			return m, nil
		}
		m.selectModel(item.value, effort)
	case pickerProfile:
		if item.current {
			return m, nil
		}
		if err := m.agent.SwitchProfile(m.ctx, item.value); err != nil {
			m.addBlock(screenBlockError, "profile: "+err.Error())
		} else {
			m.addBlock(screenBlockSystem, "profile: "+m.agent.CurrentProfile())
		}
	case pickerSandbox:
		if item.current {
			return m, nil
		}
		return m, m.startSandboxSwitch(item.value)
	case pickerLogin:
		pendingModel := ""
		if picker.startupLogin {
			pendingModel = item.modelURI
		}
		m.startProviderLogin(item.value, pendingModel, "")
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
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "credentials: "+err.Error())
		return
	}
	currentModel := m.agent.CurrentModel()
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
		modelURI := firstProviderModel(m.knownModels, status.Name)
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
			source: status.Source, credentialURL: status.CredentialURL, modelURI: modelURI,
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
	items := make([]pickerItem, 0, len(m.providers))
	for _, status := range m.providers {
		description := status.Description + " · " + status.Source
		if status.Source == "none" {
			description = status.Description + " · not configured"
		}
		items = append(items, pickerItem{
			value: status.Name, label: status.Name, description: description,
			source: status.Source, credentialURL: status.CredentialURL,
		})
	}
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

func (m *screenModel) startProviderLogin(provider, pendingModel, pendingEffort string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "login: "+err.Error())
		return
	}
	for _, status := range m.providers {
		if status.Name != provider {
			continue
		}
		pendingModel = strings.TrimSpace(pendingModel)
		if strings.EqualFold(pendingModel, m.agent.CurrentModel()) {
			if pendingEffort == m.agent.CurrentReasoningEffort() {
				pendingModel = ""
			}
		}
		if pendingModel != "" && status.Source != "none" {
			m.switchModel(pendingModel, pendingEffort)
			return
		}
		if status.Source == "environment override" {
			m.addBlock(screenBlockSystem, provider+" is supplied by an environment override")
			return
		}
		m.loginProvider = provider
		m.loginModel = pendingModel
		m.loginEffort = pendingEffort
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

func (m *screenModel) selectModel(uri, effort string) {
	uri = strings.TrimSpace(uri)
	provider := modelProvider(uri)
	if provider == "" {
		m.switchModel(uri, effort)
		return
	}
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "model: "+err.Error())
		return
	}
	for _, status := range m.providers {
		if status.Name == provider {
			if strings.EqualFold(uri, m.agent.CurrentModel()) && effort == m.agent.CurrentReasoningEffort() && status.Source != "none" {
				return
			}
			m.startProviderLogin(provider, uri, effort)
			return
		}
	}
	m.switchModel(uri, effort)
}

func (m *screenModel) switchModel(uri, effort string) {
	if err := m.agent.SwitchModel(m.ctx, uri, effort); err != nil {
		m.addBlock(screenBlockError, "model: "+err.Error())
		return
	}
	current := m.agent.CurrentModel()
	m.rememberKnownModel(current)
	m.addBlock(screenBlockSystem, "model: "+current)
}

func (m *screenModel) cycleModelEffort(delta int) {
	item := &m.picker.items[m.picker.index]
	if len(item.efforts) <= 1 {
		return
	}
	item.effortIndex = (item.effortIndex + delta + len(item.efforts)) % len(item.efforts)
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

func firstProviderModel(models []string, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, uri := range models {
		if modelProvider(uri) == provider {
			return uri
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

func (m *screenModel) rememberKnownModel(model string) {
	models := append([]string{model}, m.knownModels...)
	m.knownModels = m.knownModels[:0]
	seen := make(map[string]struct{}, len(models))
	for _, candidate := range models {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		m.knownModels = append(m.knownModels, candidate)
	}
	m.syncCommandSuggestions()
}

func (m screenModel) renderPicker() []string {
	limit := min(10, max(1, m.height-5))
	selected := m.picker.index
	start := max(0, selected-limit/2)
	if start+limit > len(m.picker.items) {
		start = max(0, len(m.picker.items)-limit)
	}
	end := min(len(m.picker.items), start+limit)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		item := m.picker.items[index]
		marker := " "
		if index == selected {
			marker = userMarker
		}
		label := sanitizeTerminalText(item.label)
		if item.current {
			label = m.accentStyle.Render(label) + m.mutedStyle.Render("  current")
		} else if index == selected {
			label = m.accentStyle.Render(label)
		}
		if item.description != "" {
			label += m.mutedStyle.Render("  " + sanitizeTerminalText(item.description))
		}
		if m.picker.kind == pickerModel && index == selected && len(item.efforts) > 1 {
			effort := selectedModelEffort(item)
			if effort == "" {
				effort = "default"
			}
			label += m.mutedStyle.Render("  effort: " + sanitizeTerminalText(effort) + "  ←/→")
		}
		lines = append(lines, marker+strings.Repeat(" ", transcriptGutter-1)+truncateANSI(label, m.contentWidth()))
	}
	return lines
}
