package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	modelapi "github.com/levmv/skot/model"
)

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

// askModelContextWindow collects the final field of an undeclared model.
func (m *screenModel) askModelContextWindow(selection modelSelection) {
	m.modelContextSelection = selection
	m.composer.reset()
	m.syncCommandSuggestions()
}

func (m screenModel) modelContextPromptLine() string {
	if m.modelContextSelection.uri == "" {
		return ""
	}
	return strings.Repeat(" ", transcriptGutter) + m.mutedStyle.Render(
		"context window · e.g. 128K or 1M · esc cancels",
	)
}

func (m *screenModel) cancelModelContextChoice() {
	if m.modelContextSelection.uri == "" {
		return
	}
	selection := m.modelContextSelection
	m.modelContextSelection = modelSelection{}
	m.composer.reset()
	m.syncCommandSuggestions()
	m.addBlock(screenBlockError, fmt.Sprintf(
		"model: selection cancelled; retry with /model %s %s",
		selection.uri, selection.api,
	))
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
	switch strings.TrimSpace(protocol) {
	case modelAPIChatCompletions:
		return "OpenAI Chat Completions"
	case modelAPIResponses:
		return "Open Responses"
	case modelAPIAnthropicMessages:
		return "Anthropic Messages"
	default:
		return strings.ReplaceAll(strings.TrimSpace(protocol), "_", " ")
	}
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

func parseModelTokenCount(value string) (int, error) {
	original := strings.TrimSpace(value)
	normalized := strings.ToLower(strings.ReplaceAll(original, "_", ""))
	multiplier := int64(1)
	if strings.HasSuffix(normalized, "k") {
		multiplier = 1_000
		normalized = strings.TrimSuffix(normalized, "k")
	} else if strings.HasSuffix(normalized, "m") {
		multiplier = 1_000_000
		normalized = strings.TrimSuffix(normalized, "m")
	}
	amount, err := strconv.ParseInt(normalized, 10, 64)
	maxInt := int64(^uint(0) >> 1)
	if err != nil || amount <= 0 || amount > maxInt/multiplier {
		return 0, fmt.Errorf("invalid context window %q; use a positive token count such as 128K or 1M", original)
	}
	return int(amount * multiplier), nil
}

func (m *screenModel) selectModel(selection modelSelection, returnPicker pickerState) tea.Cmd {
	selection.uri = strings.TrimSpace(selection.uri)
	provider := modelProvider(selection.uri)
	if provider == "" {
		m.switchModel(selection)
		return nil
	}
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "model: "+err.Error())
		return nil
	}
	for _, status := range m.providers {
		if status.Name == provider {
			if strings.EqualFold(selection.uri, m.agent.CurrentModel()) && selection.effort == m.agent.CurrentReasoningEffort() && status.Source != "none" {
				m.switchModel(selection)
				return nil
			}
			return m.startProviderLogin(provider, selection, returnPicker)
		}
	}
	m.switchModel(selection)
	return nil
}

func (m *screenModel) switchModel(selection modelSelection) {
	before := m.agent.CurrentModel()
	switchErr := m.agent.SwitchModelWithContextWindow(
		m.ctx, selection.uri, selection.effort, selection.api, selection.contextWindow,
	)
	if switchErr != nil && !preferenceAppliedDespiteError(switchErr) {
		// A route whose gateway serves several protocols needs one fact Skot
		// does not have. Asking for it here keeps the answer attached to the
		// selection being made instead of to the whole process.
		if selection.api == "" && modelapi.IsAPIRequired(switchErr) {
			m.askModelAPI(selection)
			return
		}
		if modelapi.IsContextWindowRequired(switchErr) {
			m.askModelContextWindow(selection)
			return
		}
		m.addBlock(screenBlockError, "model: "+switchErr.Error())
		return
	}
	current := m.agent.CurrentModel()
	m.refreshSessionStatus()
	m.refreshModelChoices()
	notice := formatSettingChange("model", before, current)
	if m.operation.isTurn() {
		notice += " · applies before the next model request"
	}
	m.addBlock(screenBlockSystem, notice)
	if switchErr != nil {
		m.addBlock(screenBlockError, "model: "+switchErr.Error())
	}
}

func (m screenModel) modelSelectionForURI(uri string) modelSelection {
	selection := modelSelection{uri: strings.TrimSpace(uri)}
	for _, choice := range m.modelChoices {
		if !strings.EqualFold(strings.TrimSpace(choice.URI), selection.uri) {
			continue
		}
		if choice.ProtocolExplicit {
			selection.api = choice.Protocol
		}
		if !choice.ContextWindowEstimated {
			selection.contextWindow = choice.ContextWindow
		}
		break
	}
	return selection
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

func firstProviderModel(choices []ModelChoice, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, choice := range choices {
		if !choice.Unavailable && modelProvider(choice.URI) == provider {
			return choice.URI
		}
	}
	return ""
}

func modelProvider(uri string) string {
	provider, model, ok := strings.Cut(strings.TrimSpace(uri), "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

func (m *screenModel) refreshModelChoices() {
	m.modelChoices = m.agent.ModelChoices()
	m.syncCommandSuggestions()
}
