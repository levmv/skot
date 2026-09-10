package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
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

func (m *screenModel) startProviderLogin(provider string, pending modelSelection, returnPicker pickerState) tea.Cmd {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := m.refreshProviderStatuses(); err != nil {
		m.addBlock(screenBlockError, "login: "+err.Error())
		return nil
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
			return nil
		}
		if status.Source == "environment override" {
			m.addBlock(screenBlockSystem, provider+" is supplied by an environment override")
			return nil
		}
		m.loginProvider = provider
		m.loginSelection = pending
		m.loginReturn = returnPicker
		m.secret.Reset()
		m.secret.EchoMode = textinput.EchoPassword
		if status.BrowserLogin {
			return m.startBrowserLogin(provider)
		}
		m.secret.Placeholder = provider + " API key"
		message := "enter " + provider + " API key (input is hidden)"
		if status.Source == "auth store" {
			message = "enter a new " + provider + " API key (input is hidden)"
		}
		if status.CredentialURL != "" {
			message += "; create or manage keys at " + status.CredentialURL
		}
		m.addBlock(screenBlockSystem, message)
		return nil
	}
	m.addBlock(screenBlockError, "login: unsupported provider "+provider)
	return nil
}

type browserLoginDoneMsg struct {
	login app.BrowserLogin
	err   error
}

func (m *screenModel) startBrowserLogin(provider string) tea.Cmd {
	login, err := m.agent.BeginBrowserLogin(m.ctx, provider)
	if err != nil {
		m.cancelLogin()
		m.addBlock(screenBlockError, "login: "+err.Error())
		return nil
	}
	m.browserLogin = login
	m.secret.Placeholder = "paste the full localhost callback URL, or wait for the browser · esc cancels"
	m.addBlock(screenBlockSystem, "open this URL in your browser to sign in with ChatGPT:\n"+login.AuthorizationURL())
	m.addBlock(screenBlockSystem, "waiting for browser login; if the browser cannot connect to localhost, copy its full address and paste it here (input is hidden)")
	return func() tea.Msg { return browserLoginDoneMsg{login: login, err: login.Wait()} }
}

func (m *screenModel) finishBrowserLogin(message browserLoginDoneMsg) tea.Cmd {
	// A cancelled attempt may finish after another login has started. It must
	// neither save credentials nor change the user's pending model selection.
	if m.browserLogin != message.login {
		return nil
	}
	provider, pending := m.loginProvider, m.loginSelection
	err := message.err
	if err == nil {
		return m.startCredentialUpdate(provider, false, pending, message.login.Complete)
	}
	m.cancelLogin()
	if errors.Is(err, context.DeadlineExceeded) {
		m.addBlock(screenBlockError, "login timed out; retry /login "+provider)
	} else {
		m.addBlock(screenBlockError, "login: "+err.Error())
	}
	return nil
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

func (m *screenModel) logoutProvider(provider string) tea.Cmd {
	provider = strings.ToLower(strings.TrimSpace(provider))
	client := m.agent
	return m.startCredentialUpdate(provider, true, modelSelection{}, func(ctx context.Context) error {
		return client.Logout(ctx, provider)
	})
}

type credentialDoneMsg struct {
	provider string
	logout   bool
	pending  modelSelection
	err      error
}

func (m *screenModel) startCredentialUpdate(provider string, logout bool, pending modelSelection, update func(context.Context) error) tea.Cmd {
	m.loginProvider = ""
	m.secret.Reset()
	ctx, cancel := context.WithCancel(m.ctx)
	if login := m.browserLogin; login != nil {
		cancelContext := cancel
		cancel = func() { cancelContext(); login.Close() }
	}
	kind := operationLogin
	if logout {
		kind = operationLogout
	}
	// OAuth refresh holds the process lock while exchanging a rotating token.
	// Even a local credential write can therefore wait and must keep Esc usable.
	m.credentialOperation = activeOperation{kind: kind, startedAt: time.Now(), cancel: cancel}
	return func() tea.Msg {
		return credentialDoneMsg{provider: provider, logout: logout, pending: pending, err: update(ctx)}
	}
}

func (m *screenModel) finishCredentialUpdate(message credentialDoneMsg) {
	m.credentialOperation.cancel()
	m.credentialOperation.clear()
	returnPicker := m.loginReturn
	m.cancelLogin()
	action, success := "login", "logged in to "
	if message.logout {
		action, success = "logout", "logged out of "
	}
	if errors.Is(message.err, context.Canceled) {
		m.addBlock(screenBlockSystem, action+" cancelled")
		if returnPicker.active() {
			m.picker = returnPicker
		}
		return
	}
	if message.err != nil {
		m.addBlock(screenBlockError, action+": "+message.err.Error())
		return
	}
	m.addBlock(screenBlockSystem, success+message.provider)
	_ = m.refreshProviderStatuses()
	if message.pending.uri != "" {
		m.switchModel(message.pending)
	}
}
