package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
)

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

func (m *screenModel) logoutProvider(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := m.agent.Logout(m.ctx, provider); err != nil {
		m.addBlock(screenBlockError, "logout: "+err.Error())
		return
	}
	m.addBlock(screenBlockSystem, "logged out of "+provider)
	_ = m.refreshProviderStatuses()
}
