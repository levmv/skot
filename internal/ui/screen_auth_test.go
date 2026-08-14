package ui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

func TestMissingStartupCredentialOpensLoginPicker(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/model",
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "none", Description: "model provider", CredentialURL: "https://example.test/keys"},
			{Name: "openai", Source: "environment override", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	if model.picker.kind != pickerLogin || len(model.picker.items) != 1 || model.picker.items[0].value != "deepseek" {
		t.Fatalf("startup login picker = %#v", model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if model.picker.active() || model.loginProvider != "deepseek" || model.secret.EchoMode != textinput.EchoPassword {
		t.Fatalf("login editor = picker %#v provider %q echo %v", model.picker, model.loginProvider, model.secret.EchoMode)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; !strings.Contains(got, "input is hidden") || !strings.Contains(got, "https://example.test/keys") {
		t.Fatalf("login prompt = %q", got)
	}
}

func TestLoginSecretNeverEntersTranscriptOrHistory(t *testing.T) {
	fake := &fakeAgent{
		model:     "deepseek/model",
		providers: []ProviderStatus{{Name: "deepseek", Source: "none", Description: "model provider"}},
	}
	model := testScreenModel(t, fake)
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	model.secret.SetValue("super-secret-key")
	if rendered := strings.Join(model.markedEditorLines(), "\n"); strings.Contains(rendered, "super-secret-key") {
		t.Fatalf("secret rendered in cleartext: %q", rendered)
	}
	model, _ = model.submitInput()
	if fake.loginProvider != "deepseek" || fake.loginToken != "super-secret-key" || model.loginProvider != "" {
		t.Fatalf("login = provider %q token %q active %q", fake.loginProvider, fake.loginToken, model.loginProvider)
	}
	if len(model.composer.history) != 0 || strings.Contains(strings.Join(model.transcript.lines, "\n"), "super-secret-key") {
		t.Fatalf("secret leaked: history=%#v transcript=%q", model.composer.history, model.transcript.lines)
	}
}

func TestLoginAndLogoutRespectCredentialSources(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/model",
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
			{Name: "openai", Source: "environment override", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/login openai")
	model, _ = model.submitInput()
	if model.loginProvider != "" || !strings.Contains(model.transcript.blocks[len(model.transcript.blocks)-1].text, "environment override") {
		t.Fatalf("environment login state = %q / %#v", model.loginProvider, model.transcript.blocks[len(model.transcript.blocks)-1])
	}
	model.composer.setValue("/logout")
	model, _ = model.submitInput()
	if model.picker.kind != pickerLogout || len(model.picker.items) != 1 || model.picker.items[0].value != "deepseek" {
		t.Fatalf("logout picker = %#v", model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.logoutProvider != "deepseek" {
		t.Fatalf("logout provider = %q", fake.logoutProvider)
	}
}

func TestStartupLoginPickerCanSwitchToConfiguredProvider(t *testing.T) {
	fake := &fakeAgent{
		model:       "deepseek/model",
		knownModels: []string{"deepseek/model", "openrouter/free", "openai/gpt-5"},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "none", Description: "model provider"},
			{Name: "openrouter", Source: "environment override", Description: "model provider"},
			{Name: "openai", Source: "none", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	if model.picker.kind != pickerLogin || !model.picker.startupLogin || len(model.picker.items) != 3 || model.picker.index != 0 {
		t.Fatalf("startup picker = %#v", model.picker)
	}
	if !strings.Contains(model.picker.items[1].description, "environment credential") || model.picker.items[1].modelURI != "openrouter/free" {
		t.Fatalf("OpenRouter item = %#v", model.picker.items[1])
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyDown, BaseCode: tea.KeyDown})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.model != "openrouter/free" || model.loginProvider != "" {
		t.Fatalf("model=%q login=%q", fake.model, model.loginProvider)
	}
}

func TestStartupLoginToAlternativeProviderFinishesModelSwitch(t *testing.T) {
	fake := &fakeAgent{
		model:       "deepseek/model",
		knownModels: []string{"deepseek/model", "openrouter/free"},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "none", Description: "model provider"},
			{Name: "openrouter", Source: "none", Description: "model provider", CredentialURL: "https://example.test/openrouter"},
		},
	}
	model := testScreenModel(t, fake)
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyDown, BaseCode: tea.KeyDown})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if model.loginProvider != "openrouter" || model.loginModel != "openrouter/free" || model.secret.EchoMode != textinput.EchoPassword {
		t.Fatalf("pending login: provider=%q model=%q", model.loginProvider, model.loginModel)
	}
	model.secret.SetValue("openrouter-secret")
	model, _ = model.submitInput()
	if fake.loginProvider != "openrouter" || fake.loginToken != "openrouter-secret" || fake.model != "openrouter/free" {
		t.Fatalf("login/switch: provider=%q token=%q model=%q", fake.loginProvider, fake.loginToken, fake.model)
	}
	if fake.model != "openrouter/free" || model.loginProvider != "" || model.loginModel != "" {
		t.Fatalf("screen state: model=%q login=%q pending=%q", fake.model, model.loginProvider, model.loginModel)
	}
}

func TestModelPickerShowsCredentialStateAndDefersMissingLogin(t *testing.T) {
	fake := &fakeAgent{
		model:       "deepseek/model",
		knownModels: []string{"deepseek/model", "openrouter/free"},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
			{Name: "openrouter", Source: "none", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	if model.picker.kind != pickerModel || len(model.picker.items) != 3 || model.picker.items[0].description != "" || model.picker.items[1].description != "login required" {
		t.Fatalf("model picker = %#v", model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyDown, BaseCode: tea.KeyDown})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.model != "deepseek/model" || model.loginProvider != "openrouter" || model.loginModel != "openrouter/free" {
		t.Fatalf("deferred selection: model=%q login=%q pending=%q", fake.model, model.loginProvider, model.loginModel)
	}
}

func TestModelPickerShowsCuratedRouteFacts(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/model",
		modelChoices: []ModelChoice{
			{URI: "deepseek/model"},
			{
				URI: "opencode-go/gpt-5.6-luna", Name: "GPT 5.6 Luna", Protocol: "responses",
				ContextWindow: 922_000,
			},
		},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
			{Name: "opencode-go", Source: "none", Description: "OpenCode Go subscription"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	item := model.picker.items[1]
	if item.label != "GPT 5.6 Luna" || !strings.Contains(item.description, "opencode-go/gpt-5.6-luna") ||
		!strings.Contains(item.description, "922K context") || !strings.Contains(item.description, "login required") ||
		strings.Contains(item.description, "~922K") || strings.Contains(item.description, "responses") {
		t.Fatalf("OpenCode choice = %#v", item)
	}
}

func TestModelPickerMarksEstimatedFallbackWithoutRepeatingProtocol(t *testing.T) {
	description := modelChoiceDescription(ModelChoice{
		Protocol:      "chat_completions",
		ContextWindow: 128 * 1024, ContextWindowEstimated: true,
	})
	if description != "~131K context" || strings.Contains(description, "chat completions") {
		t.Fatalf("estimated description = %q", description)
	}
}

func TestModelPickerKeepsUnavailableRoutesInDetails(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/model",
		modelChoices: []ModelChoice{
			{URI: "deepseek/model"},
			{
				URI: "opencode-go/minimax-m3", Name: "MiniMax M3", Protocol: "anthropic_messages",
				Unavailable: true,
			},
		},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
			{Name: "opencode-go", Source: "auth store", Description: "OpenCode Go subscription"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	if len(model.picker.items) != 3 || model.picker.items[1].label != "Unavailable routes…" || model.picker.items[1].value != "" {
		t.Fatalf("model picker = %#v", model.picker)
	}
	model.picker.index = 1
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	details := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(details, "MiniMax M3 (opencode-go/minimax-m3)") ||
		!strings.Contains(details, "anthropic messages") || fake.model != "deepseek/model" {
		t.Fatalf("unavailable details = %q, model = %q", details, fake.model)
	}
}

func TestModelPickerCyclesAndPersistsReasoningEffort(t *testing.T) {
	uri := "deepseek/model"
	fake := &fakeAgent{
		model:            uri,
		knownModels:      []string{uri},
		reasoningEfforts: map[string][]string{uri: {"", "high"}},
		providers:        []ProviderStatus{{Name: "deepseek", Source: "auth store", Description: "model provider"}},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	if lines := strings.Join(model.renderPicker(), "\n"); !strings.Contains(lines, "effort: default") {
		t.Fatalf("default effort is not visible: %q", lines)
	}
	model, _ = model.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyRight, BaseCode: tea.KeyRight})
	if lines := strings.Join(model.renderPicker(), "\n"); !strings.Contains(lines, "effort: high") {
		t.Fatalf("high effort is not visible: %q", lines)
	}
	model, _ = model.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.reasoningEffort != "high" {
		t.Fatalf("effort = %q", fake.reasoningEffort)
	}
}
