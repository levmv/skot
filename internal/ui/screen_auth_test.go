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

func TestUnavailableCurrentModelShowsErrorInsteadOfLoginPicker(t *testing.T) {
	fake := &fakeAgent{
		model: "opencode-go/removed-model",
		modelChoices: []ModelChoice{
			{URI: "opencode-go/removed-model", Unavailable: true},
			{URI: "deepseek/model"},
		},
		providers: []ProviderStatus{
			{Name: "opencode-go", Source: "none", Description: "OpenCode Go subscription"},
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	if model.picker.active() {
		t.Fatalf("unavailable model opened startup picker: %#v", model.picker)
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockError || last.text != `model "opencode-go/removed-model" is unavailable; choose another with /model` {
		t.Fatalf("unavailable model error = %#v", last)
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
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "1", Code: '1', BaseCode: '1'})
	if fake.logoutProvider != "" || !model.picker.active() {
		t.Fatalf("logout accepted a numeric shortcut: provider=%q picker=%#v", fake.logoutProvider, model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.logoutProvider != "deepseek" {
		t.Fatalf("logout provider = %q", fake.logoutProvider)
	}
}

func TestLoginPickerSeparatesModelProvidersFromToolServices(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/model",
		providers: []ProviderStatus{
			{Name: "tavily", Source: "none", Description: "web search", ToolService: true},
			{Name: "deepseek", Source: "auth store", Description: "model provider"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/login")
	model, _ = model.submitInput()

	if len(model.picker.items) != 2 || model.picker.items[0].value != "deepseek" ||
		model.picker.items[1].value != "tavily" || !model.picker.items[1].dividerBefore {
		t.Fatalf("login picker groups = %#v", model.picker.items)
	}
	rendered := model.renderPicker()
	modelLine, dividerLine, toolLine := -1, -1, -1
	for index, line := range rendered {
		switch {
		case strings.Contains(line, "deepseek"):
			modelLine = index
		case strings.Contains(line, "──"):
			dividerLine = index
		case strings.Contains(line, "tavily"):
			toolLine = index
		}
	}
	if modelLine < 0 || dividerLine <= modelLine || toolLine <= dividerLine {
		t.Fatalf("login picker lines = %#v", rendered)
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
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "2", Code: '2', BaseCode: '2'})
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
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "openrouter", Code: 'o', BaseCode: 'o'})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.model != "deepseek/model" || model.loginProvider != "openrouter" || model.loginModel != "openrouter/free" {
		t.Fatalf("deferred selection: model=%q login=%q pending=%q", fake.model, model.loginProvider, model.loginModel)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape, BaseCode: tea.KeyEscape})
	if model.loginProvider != "" || model.picker.kind != pickerModel || model.picker.query != "openrouter" || model.picker.items[model.picker.index].value != "openrouter/free" {
		t.Fatalf("restored model picker = %#v, login=%q", model.picker, model.loginProvider)
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
	// The split matters, not the styling: what a route always says (a barrier
	// to using it) against what only the selected route says (a reminder).
	item := model.picker.items[1]
	if item.label != "opencode-go/gpt-5.6-luna" || !item.dimmed || item.description != "login required" ||
		item.activeDetail != "922K context" || strings.Contains(item.activeDetail, "~922K") ||
		strings.Contains(item.activeDetail, "responses") {
		t.Fatalf("OpenCode choice = %#v", item)
	}
}

func TestModelPickerFiltersRoutesAsTheQueryIsTyped(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/deepseek-v4-flash",
		modelChoices: []ModelChoice{
			{URI: "deepseek/deepseek-v4-flash", Name: "DeepSeek V4 Flash"},
			{URI: "opencode-go/gpt-5.6-luna", Name: "OpenCode Go · GPT 5.6 Luna"},
			{URI: "opencode-go/kimi-k3", Name: "OpenCode Go · Kimi K3"},
		},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store"},
			{Name: "opencode-go", Source: "auth store"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	// One key at a time, the way a terminal delivers them: the query narrows on
	// every keystroke, including the digits.
	for _, r := range "opencode-go 5.6" {
		model, _ = model.handleKey(tea.KeyPressMsg{Text: string(r), Code: r, BaseCode: r})
	}
	visible := model.picker.visibleIndices()
	if model.picker.query != "opencode-go 5.6" || len(visible) != 2 || model.picker.items[visible[0]].value != "opencode-go/gpt-5.6-luna" || !model.picker.items[visible[1]].custom {
		t.Fatalf("filtered model picker = %#v, visible=%#v", model.picker, visible)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.model != "opencode-go/gpt-5.6-luna" {
		t.Fatalf("selected model = %q", fake.model)
	}
}

func TestModelPickerSearchAcceptsPastedText(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/flash",
		modelChoices: []ModelChoice{
			{URI: "deepseek/flash", Name: "DeepSeek V4 Flash"},
			{URI: "opencode-go/kimi-k3", Name: "OpenCode Go · Kimi K3"},
		},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store"},
			{Name: "opencode-go", Source: "auth store"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	updated, _ := model.Update(tea.PasteMsg{Content: "opencode-go/kimi-k3\n"})
	model = updated.(screenModel)
	visible := model.picker.visibleIndices()
	if model.picker.query != "opencode-go/kimi-k3 " || len(visible) != 2 || model.picker.items[visible[0]].value != "opencode-go/kimi-k3" {
		t.Fatalf("pasted query = %q, visible = %#v", model.picker.query, visible)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.model != "opencode-go/kimi-k3" {
		t.Fatalf("selected model = %q", fake.model)
	}
}

func TestModelPickerPlacesDefaultEffortMidLadderAndClampsAtBothEnds(t *testing.T) {
	if got := orderedReasoningEfforts([]string{"", "none", "low", "medium", "high", "xhigh", "max"}); strings.Join(got, ",") != "none,low,medium,,high,xhigh,max" {
		t.Fatalf("seven step ladder = %#v", got)
	}
	// Nothing to place before the default when there is a single explicit step.
	if got := orderedReasoningEfforts([]string{"", "high"}); strings.Join(got, ",") != ",high" {
		t.Fatalf("two step ladder = %#v", got)
	}
	if got := orderedReasoningEfforts(nil); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty ladder = %#v", got)
	}
	fake := &fakeAgent{
		model: "opencode-go/deepseek-v4-flash",
		modelChoices: []ModelChoice{
			{
				URI: "opencode-go/deepseek-v4-flash", Name: "OpenCode Go · DeepSeek V4 Flash",
				ContextWindow: 1_000_000, ReasoningEfforts: []string{"", "low", "high", "max"},
			},
			{
				URI: "opencode-go/gpt-5.6-luna", Name: "OpenCode Go · GPT 5.6 Luna",
				ContextWindow: 922_000, ReasoningEfforts: []string{"", "none", "low", "medium", "high", "xhigh", "max"},
			},
		},
		providers: []ProviderStatus{{Name: "opencode-go", Source: "auth store"}},
	}
	model := testScreenModel(t, fake)
	model.resize(80, 24)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	if got := model.picker.items[0].efforts; strings.Join(got, ",") != "low,,high,max" {
		t.Fatalf("effort ladder = %#v", got)
	}
	// A route the session is not on starts at the default, which is no longer
	// the first rung.
	if got := selectedModelEffort(model.picker.items[1]); got != "" {
		t.Fatalf("effort of an unselected route = %q", got)
	}
	// Width, not wording: the hint used to be truncated away entirely on an
	// 80 column terminal, which is where most of these rows are read.
	if rendered := strings.Join(model.renderPicker(), "\n"); !strings.Contains(rendered, "effort") {
		t.Fatalf("effort hint at 80 columns = %q", rendered)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyRight, BaseCode: tea.KeyRight})
	if got := selectedModelEffort(model.picker.items[0]); got != "high" {
		t.Fatalf("right from the default effort = %q", got)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft, BaseCode: tea.KeyLeft})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft, BaseCode: tea.KeyLeft})
	if got := selectedModelEffort(model.picker.items[0]); got != "low" {
		t.Fatalf("left from the default effort = %q", got)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft, BaseCode: tea.KeyLeft})
	if got := selectedModelEffort(model.picker.items[0]); got != "low" {
		t.Fatalf("effort before the first step = %q", got)
	}
	for range 5 {
		model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyRight, BaseCode: tea.KeyRight})
	}
	if got := selectedModelEffort(model.picker.items[0]); got != "max" {
		t.Fatalf("effort past the last step = %q", got)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if fake.reasoningEffort != "max" {
		t.Fatalf("selected effort = %q", fake.reasoningEffort)
	}
}

func TestModelPickerKeepsSearchForCustomModelEntry(t *testing.T) {
	fake := &fakeAgent{
		model:       "deepseek/model",
		knownModels: []string{"deepseek/model"},
		providers:   []ProviderStatus{{Name: "deepseek", Source: "auth store"}},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "other/custom", Code: 'o', BaseCode: 'o'})
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if model.picker.active() || model.composer.value() != "/model other/custom" {
		t.Fatalf("custom model input = %q, picker=%#v", model.composer.value(), model.picker)
	}
}

func TestModelPickerKeepsCurrentFirstAndMovesLoginRequiredRoutesAfterAvailable(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/current",
		modelChoices: []ModelChoice{
			{URI: "deepseek/current"},
			{URI: "openrouter/login-required"},
			{URI: "openai/available"},
		},
		providers: []ProviderStatus{
			{Name: "deepseek", Source: "auth store"},
			{Name: "openrouter", Source: "none"},
			{Name: "openai", Source: "environment override"},
		},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	got := []string{model.picker.items[0].value, model.picker.items[1].value, model.picker.items[2].value}
	if strings.Join(got, ",") != "deepseek/current,openai/available,openrouter/login-required" || !model.picker.items[2].dimmed {
		t.Fatalf("model order = %#v; picker=%#v", got, model.picker)
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
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "minimax", Code: 'm', BaseCode: 'm'})
	if model.picker.items[model.picker.index].label != "Unavailable routes…" {
		t.Fatalf("unavailable route search = %#v", model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	details := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(details, "MiniMax M3 (opencode-go/minimax-m3)") ||
		!strings.Contains(details, "anthropic messages") || fake.model != "deepseek/model" {
		t.Fatalf("unavailable details = %q, model = %q", details, fake.model)
	}
}
