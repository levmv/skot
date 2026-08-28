package ui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
	"github.com/levmv/skot/internal/toolpolicy"
)

type fakeAgent struct {
	state            agent.State
	queued           []string
	shellResult      agent.ToolResult
	shellErr         error
	shellCommand     string
	shellPrivate     bool
	status           []agent.Detail
	statusFound      bool
	toolSet          string
	toolSets         []string
	toolSetTools     map[string][]string
	toolSetErr       error
	model            string
	modelAPI         string
	reasoningEffort  string
	reasoningEfforts map[string][]string
	modelErr         error
	knownModels      []string
	modelChoices     []ModelChoice
	scope            string
	scopeSummary     string
	addedPaths       []FilesystemPath
	protectedPaths   []FilesystemPath
	removedPath      string
	removeErr        error
	addedPath        string
	addErr           error
	scopeNotice      string
	scopeErr         error
	theme            string
	themeErr         error
	displayProfile   string
	displayErr       error
	contextReport    agent.ContextReport
	compaction       agent.ContextCompactedRecord
	compactionErr    error
	providers        []ProviderStatus
	providerErr      error
	loginProvider    string
	loginToken       string
	loginErr         error
	logoutProvider   string
	logoutErr        error
	sessionID        string
	sessions         []SessionSummary
	sessionErr       error
	clearID          string
	clearErr         error
	resumeID         string
	resumeArg        string
	resumeErr        error
	notices          []string
	resumeNotices    []string
}

func (fake *fakeAgent) Run(context.Context, string, agent.EmitFunc) (agent.RunResult, error) {
	return agent.RunResult{}, nil
}

func (fake *fakeAgent) QueueInput(input string) error {
	fake.queued = append(fake.queued, input)
	return nil
}

func (fake *fakeAgent) ClaimQueued() (string, bool) {
	if len(fake.queued) == 0 {
		return "", false
	}
	input := fake.queued[0]
	fake.queued = fake.queued[1:]
	return input, true
}

func (fake *fakeAgent) PopQueued() (string, bool) {
	if len(fake.queued) == 0 {
		return "", false
	}
	last := len(fake.queued) - 1
	input := fake.queued[last]
	fake.queued = fake.queued[:last]
	return input, true
}

func (fake *fakeAgent) RestoreQueued() []string {
	queued := append([]string(nil), fake.queued...)
	fake.queued = nil
	return queued
}

func (fake *fakeAgent) QueuedInputs() []string {
	return append([]string(nil), fake.queued...)
}

func (fake *fakeAgent) State(context.Context) (agent.State, error) { return fake.state, nil }

func (fake *fakeAgent) RunShell(_ context.Context, command string) (agent.ToolResult, error) {
	fake.shellCommand = command
	fake.shellPrivate = false
	return fake.shellResult, fake.shellErr
}

func (fake *fakeAgent) RunPrivateShell(_ context.Context, command string) (agent.ToolResult, error) {
	fake.shellCommand = command
	fake.shellPrivate = true
	return fake.shellResult, fake.shellErr
}

func (fake *fakeAgent) ToolStatus(string) ([]agent.Detail, bool) {
	return fake.status, fake.statusFound
}

func (fake *fakeAgent) CurrentToolSet() string { return fake.toolSet }

func (fake *fakeAgent) ToolSets() []string {
	if len(fake.toolSets) != 0 {
		return append([]string(nil), fake.toolSets...)
	}
	return []string{toolpolicy.ToolSetReadOnly, toolpolicy.ToolSetEdit, toolpolicy.ToolSetDefault}
}

func (fake *fakeAgent) ToolSetTools(toolSet string) []string {
	return append([]string(nil), fake.toolSetTools[toolSet]...)
}

func (fake *fakeAgent) SwitchToolSet(_ context.Context, toolSet string) error {
	if fake.toolSetErr != nil && !preferenceAppliedDespiteError(fake.toolSetErr) {
		return fake.toolSetErr
	}
	fake.toolSet = toolSet
	return fake.toolSetErr
}

func (fake *fakeAgent) CurrentModel() string { return fake.model }

func (fake *fakeAgent) ModelChoices() []ModelChoice {
	if len(fake.modelChoices) != 0 {
		choices := append([]ModelChoice(nil), fake.modelChoices...)
		for index := range choices {
			choices[index].ReasoningEfforts = append([]string(nil), choices[index].ReasoningEfforts...)
		}
		return choices
	}
	models := append([]string{fake.model}, fake.knownModels...)
	choices := make([]ModelChoice, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, uri := range models {
		key := strings.ToLower(strings.TrimSpace(uri))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		choices = append(choices, ModelChoice{URI: uri, ReasoningEfforts: fake.ReasoningEfforts(uri)})
	}
	return choices
}

func (fake *fakeAgent) SwitchModel(_ context.Context, model, effort, api string) error {
	changed := fake.model != model || fake.reasoningEffort != effort || fake.modelAPI != api
	err := fake.modelErr
	if app.IsModelAPIRequired(err) {
		// The refusal stands until the caller supplies the protocol, exactly as
		// the application refuses an undeclared mixed-protocol route.
		if api == "" {
			return err
		}
		err = nil
	}
	if err != nil && !preferenceAppliedDespiteError(err) {
		return err
	}
	fake.model = model
	fake.modelAPI = api
	fake.reasoningEffort = effort
	if changed {
		fake.state.ImageDelivery = agent.ImageDeliveryObservedRecord{}
	}
	return err
}

func (fake *fakeAgent) CurrentReasoningEffort() string { return fake.reasoningEffort }

func (fake *fakeAgent) ReasoningEfforts(uri string) []string {
	if efforts := fake.reasoningEfforts[uri]; len(efforts) != 0 {
		return append([]string(nil), efforts...)
	}
	return []string{"", "high"}
}

func (fake *fakeAgent) CurrentScope() string { return fake.scope }

func (fake *fakeAgent) ScopeSummary() string { return fake.scopeSummary }

func (fake *fakeAgent) ScopeNotice() string { return fake.scopeNotice }

func (fake *fakeAgent) SwitchScope(_ context.Context, policy string) error {
	if fake.scopeErr != nil && !preferenceAppliedDespiteError(fake.scopeErr) {
		return fake.scopeErr
	}
	fake.scope = policy
	fake.scopeSummary = "scope: " + policy
	return fake.scopeErr
}

func (fake *fakeAgent) FilesystemPaths() (added, protected []FilesystemPath) {
	return append([]FilesystemPath(nil), fake.addedPaths...), append([]FilesystemPath(nil), fake.protectedPaths...)
}

func (fake *fakeAgent) AddDirectory(_ context.Context, path string) error {
	if fake.addErr != nil {
		return fake.addErr
	}
	fake.addedPath = path
	fake.addedPaths = append(fake.addedPaths, FilesystemPath{Path: path})
	return nil
}

func (fake *fakeAgent) ProtectPath(_ context.Context, path string) error {
	if fake.addErr != nil {
		return fake.addErr
	}
	fake.addedPath = path
	fake.protectedPaths = append(fake.protectedPaths, FilesystemPath{Path: path})
	return nil
}

func (fake *fakeAgent) RemoveAddedDirectory(_ context.Context, path string) error {
	if fake.removeErr != nil {
		return fake.removeErr
	}
	fake.removedPath = path
	fake.addedPaths = slices.DeleteFunc(fake.addedPaths, func(entry FilesystemPath) bool { return entry.Path == path })
	return nil
}

func (fake *fakeAgent) UnprotectPath(_ context.Context, path string) error {
	if fake.removeErr != nil {
		return fake.removeErr
	}
	fake.removedPath = path
	fake.protectedPaths = slices.DeleteFunc(fake.protectedPaths, func(entry FilesystemPath) bool { return entry.Path == path })
	return nil
}

func (fake *fakeAgent) CurrentTheme() string { return fake.theme }

func (fake *fakeAgent) SwitchTheme(theme string) error {
	if fake.themeErr != nil && !preferenceAppliedDespiteError(fake.themeErr) {
		return fake.themeErr
	}
	fake.theme = theme
	return fake.themeErr
}

func (fake *fakeAgent) CurrentDisplayProfile() string { return fake.displayProfile }

func (fake *fakeAgent) SwitchDisplayProfile(profile string) error {
	if fake.displayErr != nil && !preferenceAppliedDespiteError(fake.displayErr) {
		return fake.displayErr
	}
	fake.displayProfile = profile
	return fake.displayErr
}

func (fake *fakeAgent) SessionStatus() agent.SessionStatus {
	return agent.SessionStatus{
		ContextReport: fake.contextReport,
		Usage:         fake.state.Usage,
		ImageDelivery: fake.state.ImageDelivery.Status,
	}
}

func (fake *fakeAgent) Compact(context.Context) (agent.ContextCompactedRecord, error) {
	return fake.compaction, fake.compactionErr
}

func (fake *fakeAgent) ProviderStatuses() ([]ProviderStatus, error) {
	return append([]ProviderStatus(nil), fake.providers...), fake.providerErr
}

func (fake *fakeAgent) Login(_ context.Context, provider, token string) error {
	fake.loginProvider = provider
	fake.loginToken = token
	return fake.loginErr
}

func (fake *fakeAgent) Logout(_ context.Context, provider string) error {
	fake.logoutProvider = provider
	return fake.logoutErr
}

func (fake *fakeAgent) SessionID() string { return fake.sessionID }

func (fake *fakeAgent) StartupNotices() []string {
	return append([]string(nil), fake.notices...)
}

func (fake *fakeAgent) ListSessions() ([]SessionSummary, error) {
	return append([]SessionSummary(nil), fake.sessions...), fake.sessionErr
}

func (fake *fakeAgent) ClearSession(context.Context) (string, error) {
	if fake.clearErr != nil {
		return "", fake.clearErr
	}
	fake.sessionID = fake.clearID
	fake.state = agent.State{}
	return fake.clearID, nil
}

func (fake *fakeAgent) ResumeSession(_ context.Context, value string) (string, error) {
	fake.resumeArg = value
	if fake.resumeErr != nil {
		return "", fake.resumeErr
	}
	fake.sessionID = fake.resumeID
	fake.notices = append(fake.notices, fake.resumeNotices...)
	return fake.resumeID, nil
}

func testScreenModel(t *testing.T, fake *fakeAgent) screenModel {
	t.Helper()
	if fake.theme == "" {
		fake.theme = ThemeLight
	}
	if fake.displayProfile == "" {
		fake.displayProfile = DisplayDetailed
	}
	model, err := newScreenModel(context.Background(), fake, Config{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	model.refreshTranscript()
	return model
}

func TestHelpKeepsItsColumnsOnANarrowTerminal(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(60, 40)
	// Help is a two column layout, and the transcript wraps a long line back to
	// the left margin, which would break the columns rather than shorten them.
	for line := range strings.SplitSeq(model.tuiCommandHelp(), "\n") {
		if len(line) > model.contentWidth() {
			t.Fatalf("help line of %d columns does not fit %d: %q", len(line), model.contentWidth(), line)
		}
	}
	model.composer.setValue("/help")
	model, _ = model.submitInput()
	model.refreshTranscript()
	if !strings.Contains(strings.Join(model.transcript.lines, "\n"), "Type / for commands.") {
		t.Fatalf("help transcript = %q", model.transcript.lines)
	}
}

func TestExitCommandAliasesQuit(t *testing.T) {
	for _, input := range []string{"/exit", "/quit", "/q", "/Q"} {
		model := testScreenModel(t, &fakeAgent{})
		command, handled := model.dispatchCommand(input)
		if !handled || command == nil || !model.quitting {
			t.Fatalf("command %q: handled=%v command=%v quitting=%v", input, handled, command != nil, model.quitting)
		}
	}
}

func TestSubmitWhileWorkingQueuesInput(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.operation.kind = operationTurn
	model.composer.setValue("follow up")
	blocksBefore := len(model.transcript.blocks)

	model, _ = model.submitInput()
	if len(fake.queued) != 1 || fake.queued[0] != "follow up" {
		t.Fatalf("queued = %#v", fake.queued)
	}
	if model.composer.value() != "" {
		t.Fatalf("input was not cleared: %q", model.composer.value())
	}
	if got := len(model.transcript.blocks); got != blocksBefore {
		t.Fatalf("transcript blocks = %d, want %d", got, blocksBefore)
	}
	if got := strings.TrimSpace(model.queuedLine()); got != "queued: follow up" {
		t.Fatalf("queued line = %q", got)
	}
}

func TestLeadingPathIsOrdinaryInputWhileWorking(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.operation.kind = operationTurn
	blocksBefore := len(model.transcript.blocks)
	input := "/etc/hosts is the file to read"
	model.composer.setValue(input)

	model, _ = model.submitInput()
	if len(fake.queued) != 1 || fake.queued[0] != input {
		t.Fatalf("queued = %#v", fake.queued)
	}
	if got := len(model.transcript.blocks); got != blocksBefore {
		t.Fatalf("transcript blocks = %d, want %d", got, blocksBefore)
	}
}

// A running turn is the harder state: it used to reject every slash input,
// including paths, and it still refuses a command it cannot run now.
func TestSlashInputIsRefusedOnlyAsACommandName(t *testing.T) {
	for _, testCase := range []struct{ input, want string }{
		{input: "/tmp/notes.md read this"},
		{input: "/build.sh"},
		{input: "/mdoel", want: "unknown command: /mdoel"},
		{input: "/", want: "unknown command: /"},
		{input: "/clear", want: "commands are unavailable while Skot is working"},
	} {
		model := testScreenModel(t, &fakeAgent{})
		model.operation.kind = operationTurn
		_, handled := model.dispatchCommand(testCase.input)
		if handled != (testCase.want != "") {
			t.Fatalf("input %q: handled = %v", testCase.input, handled)
		}
		if testCase.want == "" {
			continue
		}
		last := model.transcript.blocks[len(model.transcript.blocks)-1]
		if last.kind != screenBlockError || !strings.Contains(last.text, testCase.want) {
			t.Fatalf("input %q block = %#v", testCase.input, last)
		}
	}
}

func TestCommandMenuRemainsVisibleWhileWorking(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationTurn
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "/", Code: '/', BaseCode: '/'})

	if model.composer.value() != "/" || !model.commandSuggestionsVisible() {
		t.Fatalf("input = %q, command menu visible = %v", model.composer.value(), model.commandSuggestionsVisible())
	}
	if rendered := strings.Join(model.renderCommandSuggestions(), "\n"); !strings.Contains(rendered, "/help") {
		t.Fatalf("command menu = %q", rendered)
	}
}

func TestCommandMenuUsesPlainLabelsAndAccentsTheSelection(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model := testScreenModel(t, &fakeAgent{})
	model.composer.setValue("/")
	model.syncCommandSuggestions()

	rendered := model.renderCommandSuggestions()
	if len(rendered) < 2 || !strings.Contains(rendered[0], model.accentStyle.Render("/help")) ||
		!strings.Contains(rendered[0], model.mutedStyle.Render(" show keys")) ||
		!strings.Contains(rendered[1], "/clear") || strings.Contains(rendered[1], model.mutedStyle.Render("/clear")) {
		t.Fatalf("command menu = %#v", rendered)
	}

	model.moveCommandSuggestion(1)
	rendered = model.renderCommandSuggestions()
	if strings.Contains(rendered[0], model.accentStyle.Render("/help")) ||
		!strings.Contains(rendered[1], model.accentStyle.Render("/clear")) {
		t.Fatalf("moved command selection = %#v", rendered)
	}
}

func TestQueuedLineShowsLatestInputAndCount(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{queued: []string{"first", "latest"}})
	if got := strings.TrimSpace(model.queuedLine()); got != "queued 2 · latest: latest" {
		t.Fatalf("queued line = %q", got)
	}
}

func TestQueuedLineOffsetsEditorCursor(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{queued: []string{"follow up"}})
	model.operation.kind = operationTurn
	model.composer.setValue("draft")
	frame := model.inlineFrame()

	editorRow := -1
	for index, line := range frame.dynamic {
		if strings.Contains(line, "draft") {
			editorRow = index
			break
		}
	}
	if editorRow < 0 {
		t.Fatalf("editor row not found in %#v", frame.dynamic)
	}
	if frame.cursor == nil || frame.cursor.Position.Y != len(frame.transcript)+editorRow {
		t.Fatalf("cursor = %#v, want row %d", frame.cursor, len(frame.transcript)+editorRow)
	}
}

func TestAltUpRecallsNewestQueuedInput(t *testing.T) {
	fake := &fakeAgent{queued: []string{"first", "latest"}}
	model := testScreenModel(t, fake)
	model.operation.kind = operationTurn

	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp, BaseCode: tea.KeyUp, Mod: tea.ModAlt})
	if model.composer.value() != "latest" {
		t.Fatalf("input = %q", model.composer.value())
	}
	if len(fake.queued) != 1 || fake.queued[0] != "first" {
		t.Fatalf("remaining queue = %#v", fake.queued)
	}
	if got := strings.TrimSpace(model.queuedLine()); got != "queued: first" {
		t.Fatalf("queued line = %q", got)
	}
}

func TestAltUpDoesNotOverwriteDraft(t *testing.T) {
	fake := &fakeAgent{queued: []string{"queued"}}
	model := testScreenModel(t, fake)
	model.operation.kind = operationTurn
	model.composer.setValue("draft")

	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyUp, BaseCode: tea.KeyUp, Mod: tea.ModAlt})
	if model.composer.value() != "draft" || len(fake.queued) != 1 {
		t.Fatalf("draft = %q, queue = %#v", model.composer.value(), fake.queued)
	}
}

func TestWorkingCommandsAreNotQueued(t *testing.T) {
	for _, input := range []string{"/model openai/gpt", "! echo no"} {
		t.Run(input[:1], func(t *testing.T) {
			fake := &fakeAgent{}
			model := testScreenModel(t, fake)
			model.operation.kind = operationTurn
			model.composer.setValue(input)

			model, _ = model.submitInput()
			if len(fake.queued) != 0 {
				t.Fatalf("queue = %#v", fake.queued)
			}
			if model.composer.value() != input {
				t.Fatalf("input = %q", model.composer.value())
			}
		})
	}
}

func TestToolSetCommandShowsAndSwitchesToolSet(t *testing.T) {
	fake := &fakeAgent{toolSet: toolpolicy.ToolSetEdit, toolSets: []string{
		toolpolicy.ToolSetDefault, toolpolicy.ToolSetEdit, toolpolicy.ToolSetReadOnly, "review",
	}, toolSetTools: map[string][]string{
		toolpolicy.ToolSetReadOnly: {"read", "grep"},
		toolpolicy.ToolSetEdit:     {"bash", "job"},
		toolpolicy.ToolSetDefault:  {"read", "write", "bash", "job"},
		"review":                   {"read", "custom"},
	}}
	model := testScreenModel(t, fake)
	model.composer.setValue("/tools")
	model, _ = model.submitInput()
	if model.picker.kind != pickerToolSet || len(model.picker.items) != 4 || model.picker.items[3].value != "review" || model.picker.index != 1 || !model.picker.items[1].current {
		t.Fatalf("tool set picker = %#v", model.picker)
	}
	if got := model.picker.items[1].description; got != "bash, job" {
		t.Fatalf("customized edit description = %q", got)
	}
	if !model.picker.numberSelectionEnabled() {
		t.Fatalf("tool set picker without digit shortcuts = %#v", model.picker)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Text: "1", Code: '1', BaseCode: '1'})
	if fake.toolSet != toolpolicy.ToolSetDefault || model.composer.value() != "" {
		t.Fatalf("tool set = %q, input = %q", fake.toolSet, model.composer.value())
	}
}

func TestModelCommandShowsAndSwitchesModel(t *testing.T) {
	fake := &fakeAgent{
		model: "deepseek/old", knownModels: []string{"deepseek/old", "openrouter/recent"},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/model")
	model, _ = model.submitInput()
	if model.picker.kind != pickerModel || len(model.picker.items) != 3 || model.picker.index != 0 || !model.picker.items[0].current || !model.picker.items[2].custom {
		t.Fatalf("model picker = %#v", model.picker)
	}
	model.picker.index = 2
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if model.picker.active() || model.composer.value() != "/model " || !model.commandSuggestionsVisible() {
		t.Fatalf("custom model entry = picker %#v input %q suggestions %#v", model.picker, model.composer.value(), model.composer.suggestions)
	}

	model.composer.setValue("/model openrouter/new/model")
	model, _ = model.submitInput()
	if fake.model != "openrouter/new/model" || model.composer.value() != "" {
		t.Fatalf("model = %q, input = %q", fake.model, model.composer.value())
	}
	if transcript := strings.Join(model.transcript.lines, "\n"); strings.Contains(transcript, "unverified") {
		t.Fatalf("model switch leaked internal compatibility status: %q", transcript)
	}
}

func TestCurrentPickerSelectionReportsPreferencePersistenceFailure(t *testing.T) {
	preferenceError := func(setting string) error {
		return &app.PreferenceNotPersistedError{Setting: setting, Err: errors.New("disk full")}
	}
	assertReported := func(t *testing.T, model screenModel) {
		t.Helper()
		if model.picker.active() {
			t.Fatalf("picker remained active: %#v", model.picker)
		}
		blocks := model.transcript.blocks
		if len(blocks) < 2 || blocks[len(blocks)-1].kind != screenBlockError ||
			!strings.Contains(blocks[len(blocks)-1].text, "is active for this session but was not saved") {
			t.Fatalf("preference failure was not reported: %#v", blocks)
		}
	}

	t.Run("model", func(t *testing.T) {
		fake := &fakeAgent{
			model: "deepseek/current", reasoningEffort: "high",
			modelErr:  preferenceError("model"),
			providers: []ProviderStatus{{Name: "deepseek", Source: "auth store"}},
		}
		model := testScreenModel(t, fake)
		model.openModelPicker()
		model, command := model.selectPickerItem()
		if command != nil || fake.model != "deepseek/current" || fake.reasoningEffort != "high" {
			t.Fatalf("command=%v model=%q effort=%q", command, fake.model, fake.reasoningEffort)
		}
		assertReported(t, model)
	})

	t.Run("tool set", func(t *testing.T) {
		fake := &fakeAgent{toolSet: toolpolicy.ToolSetEdit, toolSetErr: preferenceError("tool set")}
		model := testScreenModel(t, fake)
		model.openToolSetPicker()
		model, command := model.selectPickerItem()
		if command != nil || fake.toolSet != toolpolicy.ToolSetEdit {
			t.Fatalf("command=%v tool set=%q", command, fake.toolSet)
		}
		assertReported(t, model)
	})

	t.Run("scope", func(t *testing.T) {
		fake := &fakeAgent{
			scope: "workspace", scopeSummary: "scope: workspace",
			scopeErr: preferenceError("filesystem scope"),
		}
		model := testScreenModel(t, fake)
		model.openScopePicker()
		model, command := model.selectPickerItem()
		if command == nil {
			t.Fatal("current scope selection did not start preference retry")
		}
		message, ok := command().(scopeDoneMsg)
		if !ok {
			t.Fatalf("scope result has unexpected type")
		}
		model.finishScopeSwitch(message)
		if fake.scope != "workspace" {
			t.Fatalf("scope = %q", fake.scope)
		}
		assertReported(t, model)
	})

	t.Run("theme", func(t *testing.T) {
		fake := &fakeAgent{theme: ThemeDark, themeErr: preferenceError("theme")}
		model := testScreenModel(t, fake)
		model.openThemePicker()
		model, command := model.selectPickerItem()
		if command != nil || fake.theme != ThemeDark || !model.darkTheme {
			t.Fatalf("command=%v theme=%q dark=%v", command, fake.theme, model.darkTheme)
		}
		assertReported(t, model)
	})
}

func TestCommandSuggestionsCompleteAndSelectArguments(t *testing.T) {
	fake := &fakeAgent{toolSet: toolpolicy.ToolSetDefault}
	model := testScreenModel(t, fake)
	model.composer.setValue("/tools e")
	model.syncCommandSuggestions()
	if !model.commandSuggestionsVisible() || model.currentCommandSuggestion() != "/tools edit" {
		t.Fatalf("suggestions = %#v", model.composer.suggestions)
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyTab, BaseCode: tea.KeyTab})
	if model.composer.value() != "/tools edit" {
		t.Fatalf("completed input = %q", model.composer.value())
	}
	model, _ = model.submitInput()
	if fake.toolSet != toolpolicy.ToolSetEdit {
		t.Fatalf("tool set = %q", fake.toolSet)
	}
}

func TestEscapeCancelsAndRestoresQueuedInput(t *testing.T) {
	fake := &fakeAgent{queued: []string{"one", "two"}}
	model := testScreenModel(t, fake)
	model.operation.kind = operationTurn
	cancelled := false
	model.operation.cancel = func() { cancelled = true }

	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape, BaseCode: tea.KeyEscape})
	if !cancelled {
		t.Fatal("turn was not cancelled")
	}
	if got := model.composer.value(); got != "one\ntwo" {
		t.Fatalf("restored input = %q", got)
	}
	if len(fake.queued) != 0 {
		t.Fatalf("queue = %#v", fake.queued)
	}
	if got := model.queuedLine(); got != "" {
		t.Fatalf("stale queued line = %q", got)
	}
}

func TestRetryNoticeReplacesDiscardedPartialAndSummarizesRecovery(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "partial"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "stream failed"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})

	for _, block := range model.transcript.blocks {
		if block.kind == screenBlockAssistant && strings.Contains(block.text, "partial") {
			t.Fatalf("partial assistant block survived: %#v", block)
		}
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "retrying in 1s: stream failed (partial response removed)" {
		t.Fatalf("status = %q", got)
	}

	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-2"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-2", Text: "done", Status: agent.RunCompleted})
	if got := model.transcript.blocks[len(model.transcript.blocks)-2]; got.kind != screenBlockSystem || got.text != "model recovered after 1 retry: stream failed (partial response removed)" {
		t.Fatalf("recovery notice = %#v", got)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1]; got.kind != screenBlockAssistant || got.text != "done" {
		t.Fatalf("final response = %#v", got)
	}
}

func TestRepeatedRetryNoticesUseOneTransientBlock(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.applyAgentEvent(agent.Event{
		Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "provider is temporarily unavailable",
	})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})
	blocks := len(model.transcript.blocks)
	model.applyAgentEvent(agent.Event{
		Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-2", Text: "provider is temporarily unavailable",
	})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-2", Text: "retrying in 2s"})

	if len(model.transcript.blocks) != blocks {
		t.Fatalf("retry blocks = %d, want %d", len(model.transcript.blocks), blocks)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "retrying in 2s after 2 model errors: provider is temporarily unavailable" {
		t.Fatalf("status = %q", got)
	}
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-3"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-3", Text: "done", Status: agent.RunCompleted})
	if got := model.transcript.blocks[len(model.transcript.blocks)-2].text; got != "model recovered after 2 retries: provider is temporarily unavailable" {
		t.Fatalf("recovery status = %q", got)
	}
}

func TestFinalModelFailureReplacesRetryNoticeWithOneError(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationTurn
	initialBlocks := len(model.transcript.blocks)
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "temporarily unavailable"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-2"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-2", Text: "partial"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-2", Text: "temporarily unavailable"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-2", Status: agent.RunFailed})

	model, _ = model.update(agentDoneMsg{err: errors.New("temporarily unavailable")})
	if len(model.transcript.blocks) != initialBlocks+1 || model.transcript.blocks[initialBlocks].kind != screenBlockError || model.transcript.blocks[initialBlocks].text != "error: temporarily unavailable (partial response removed)" {
		t.Fatalf("final transcript = %#v", model.transcript.blocks)
	}
}

func TestBlankStreamedDeltaIsDiscardedWithoutANotice(t *testing.T) {
	// Providers routinely open a response with a whitespace-only delta. Nothing
	// was legible on screen, so nothing needs explaining away.
	model := testScreenModel(t, &fakeAgent{})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "\n\n"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})

	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "retrying in 1s: overloaded" {
		t.Fatalf("status = %q", got)
	}
	for _, block := range model.transcript.blocks {
		if block.kind == screenBlockAssistant {
			t.Fatalf("blank attempt block survived: %#v", block)
		}
	}
}

func TestRemovalNoticeSurvivesLaterAttemptsThatStreamNothing(t *testing.T) {
	// The retry group shares one rewritten block, so the notice describes the
	// group rather than the newest attempt.
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationTurn
	initialBlocks := len(model.transcript.blocks)
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "partial"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-2"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-2", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-2", Text: "retrying in 2s"})

	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "retrying in 2s after 2 model errors: overloaded (partial response removed)" {
		t.Fatalf("status = %q", got)
	}

	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-3"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-3", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-3", Status: agent.RunFailed})
	model, _ = model.update(agentDoneMsg{err: errors.New("overloaded")})
	if len(model.transcript.blocks) != initialBlocks+1 {
		t.Fatalf("final transcript = %#v", model.transcript.blocks)
	}
	if got := model.transcript.blocks[initialBlocks]; got.kind != screenBlockError || got.text != "error: overloaded (partial response removed)" {
		t.Fatalf("final error = %#v", got)
	}
}

func TestReportedRemovalIsNotRepeatedByALaterFailure(t *testing.T) {
	// The recovery line already told the user why the text went. A second group
	// that removes nothing must not inherit the explanation.
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationTurn
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "partial"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelRetryScheduled, AttemptID: "attempt-1", Text: "retrying in 1s"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-2"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventToolStarted, AttemptID: "attempt-2"})

	recovery := model.transcript.blocks[len(model.transcript.blocks)-1].text
	if recovery != "model recovered after 1 retry: overloaded (partial response removed)" {
		t.Fatalf("recovery = %q", recovery)
	}

	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-3"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-3", Text: "overloaded"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-3", Status: agent.RunFailed})
	model, _ = model.update(agentDoneMsg{err: errors.New("overloaded")})

	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockError || last.text != "error: overloaded" {
		t.Fatalf("final error = %#v", last)
	}
}

func TestInterruptExplainsRemovedTextAndStaysSilentOtherwise(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.operation.kind = operationTurn
	initialBlocks := len(model.transcript.blocks)
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "half an answer"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "context canceled"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-1", Status: agent.RunCancelled})

	model, _ = model.update(agentDoneMsg{err: context.Canceled})
	if got := model.transcript.blocks[initialBlocks]; got.kind != screenBlockSystem || got.text != "interrupted (partial response removed)" {
		t.Fatalf("interrupt notice = %#v", got)
	}

	quiet := testScreenModel(t, &fakeAgent{})
	quiet.operation.kind = operationTurn
	quietBlocks := len(quiet.transcript.blocks)
	quiet.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	quiet.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1", Text: "context canceled"})
	quiet.applyAgentEvent(agent.Event{Kind: agent.EventRunFinished, AttemptID: "attempt-1", Status: agent.RunCancelled})

	quiet, _ = quiet.update(agentDoneMsg{err: context.Canceled})
	if len(quiet.transcript.blocks) != quietBlocks {
		t.Fatalf("silent interrupt added %#v", quiet.transcript.blocks[quietBlocks:])
	}
}

func TestDurableMaintenanceEventsReachTranscript(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	for _, event := range []agent.Event{
		{Kind: agent.EventBoundaryDelivered, Text: "background work completed", Sequence: 4},
		{Kind: agent.EventContextCompacted, Text: "context compacted", Sequence: 5},
		{Kind: agent.EventToolResultsPruned, Text: "pruned old tool results", Sequence: 6},
	} {
		model.applyAgentEvent(event)
	}
	got := model.transcript.blocks[len(model.transcript.blocks)-3:]
	for index, want := range []string{"background work completed", "context compacted", "pruned old tool results"} {
		if got[index].kind != screenBlockSystem || got[index].text != want {
			t.Fatalf("block %d = %#v, want system %q", index, got[index], want)
		}
	}
}

func TestTurnCompletionRingsOnlyAfterAnUnfocusedQueueDrains(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}

	model, command := model.update(agentDoneMsg{})
	if command != nil {
		t.Fatal("focused turn completion sent a notification")
	}

	model, _ = model.update(tea.BlurMsg{})
	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}
	fake.queued = []string{"continue"}
	model, command = model.update(agentDoneMsg{})
	if command == nil || !model.operation.isTurn() {
		t.Fatalf("queued turn did not continue: command=%v operation=%#v", command, model.operation)
	}

	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}
	model, command = model.update(agentDoneMsg{})
	if command == nil {
		t.Fatal("unfocused final turn completion sent no notification")
	}
	raw, ok := command().(tea.RawMsg)
	if !ok || raw.Msg != terminalBell {
		t.Fatalf("completion notification = %#v", raw)
	}

	model, _ = model.update(tea.FocusMsg{})
	if !model.focused {
		t.Fatal("focus event left the terminal blurred")
	}
}

func TestTextDeltasFlushAtNewlineOrMaxDelay(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeLight}, Config{}, &output)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	model.appendBlock(screenBlock{kind: screenBlockAssistant, text: "before", attemptID: "attempt"})
	model.refreshTranscript()
	if err := model.renderer.RenderFrame(model.inlineFrame(), model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.presented()
	before := strings.Join(model.transcript.lines, "\n")
	output.Reset()

	updated, _ := model.Update(agentEventMsg{event: agent.Event{
		Kind: agent.EventTextDelta, AttemptID: "attempt", Text: " after",
	}})
	got := updated.(screenModel)
	if !got.operation.renderPending {
		t.Fatal("text delta did not schedule a transcript frame")
	}
	if transcript := strings.Join(got.transcript.lines, "\n"); transcript != before {
		t.Fatalf("delta rendered before frame boundary: %q", transcript)
	}
	updated, _ = got.Update(agentEventMsg{event: agent.Event{
		Kind: agent.EventTextDelta, AttemptID: "attempt", Text: " again",
	}})
	got = updated.(screenModel)
	if output.Len() != 0 {
		t.Fatalf("text deltas wrote to the terminal before frame boundary: %q", output.String())
	}

	updated, _ = got.Update(transcriptRenderMsg{})
	got = updated.(screenModel)
	if got.operation.renderPending || !strings.Contains(strings.Join(got.transcript.lines, "\n"), "before after again") {
		t.Fatalf("frame did not flush delta: pending=%v transcript=%q", got.operation.renderPending, got.transcript.lines)
	}
	if output.Len() == 0 {
		t.Fatal("frame boundary did not write accumulated text to the terminal")
	}

	output.Reset()
	updated, _ = got.Update(agentEventMsg{event: agent.Event{
		Kind: agent.EventTextDelta, AttemptID: "attempt", Text: "\nnext line",
	}})
	got = updated.(screenModel)
	if !strings.Contains(strings.Join(got.transcript.lines, "\n"), "next line") {
		t.Fatalf("newline did not flush delta: %q", got.transcript.lines)
	}
	if output.Len() == 0 {
		t.Fatal("newline did not write accumulated text to the terminal")
	}
}

func TestTranscriptTracksDirtyRenderedSuffix(t *testing.T) {
	var transcript transcriptState
	render := func(_ int, block screenBlock) []string { return []string{block.text} }
	transcript.addBlock(screenBlockSystem, "stable prefix")
	transcript.addBlock(screenBlockAssistant, "old tail")
	transcript.refresh(80, render)
	prefixLines := transcript.renderCache[0].end
	transcript.presented()

	transcript.markBlockDirty(1)
	transcript.blocks[1].text = "new tail"
	transcript.refresh(80, render)
	if !transcript.dirty || transcript.dirtyFrom != prefixLines {
		t.Fatalf("dirty suffix starts at %d (changed=%v), want %d", transcript.dirtyFrom, transcript.dirty, prefixLines)
	}
}

func TestSessionHistorySeedsTranscriptAndInputHistory(t *testing.T) {
	fake := &fakeAgent{state: agent.State{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "hello"},
		{Kind: agent.ItemReasoning, Text: "private summary", ProviderData: []agent.ProviderData{{Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"opaque-secret"}`)}}},
		{Kind: agent.ItemBoundaryText, Text: "Background job job-1 completed."},
		{Kind: agent.ItemAssistantText, Text: "hi"},
	}}}
	model := testScreenModel(t, fake)

	if len(model.composer.history) != 1 || model.composer.history[0] != "hello" {
		t.Fatalf("history = %#v", model.composer.history)
	}
	if len(model.transcript.blocks) != 4 || model.transcript.blocks[1].kind != screenBlockUser || model.transcript.blocks[2].kind != screenBlockSystem || model.transcript.blocks[3].kind != screenBlockAssistant {
		t.Fatalf("blocks = %#v", model.transcript.blocks)
	}
	if transcript := strings.Join(model.transcript.lines, "\n"); strings.Contains(transcript, "private summary") || strings.Contains(transcript, "opaque-secret") {
		t.Fatalf("provider state rendered in transcript: %q", transcript)
	}
}

func TestScreenKeepsFullSourceBackedTranscript(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(40, 6)
	model.transcript.blocks = nil
	model.transcript.renderCache = nil
	model.transcript.renderCacheLines = nil
	model.transcript.lines = nil
	model.transcript.renderDirtyFrom = 0
	for index := range 12 {
		model.addBlock(screenBlockSystem, fmt.Sprintf("line %d", index))
	}
	model.refreshTranscript()

	transcript := strings.Join(model.transcript.lines, "\n")
	if !strings.Contains(transcript, "line 0") || !strings.Contains(transcript, "line 11") {
		t.Fatalf("source-backed transcript = %q", transcript)
	}
}

func TestScreenViewDoesNotMaterializeTranscript(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.addBlock(screenBlockAssistant, "large transcript sentinel")
	model.refreshTranscript()

	if view := model.View(); view.Content != "" || view.Cursor != nil {
		t.Fatalf("Bubble Tea view materialized custom frame: %#v", view)
	}
	if got := strings.Join(model.inlineFrame().transcript, "\n"); !strings.Contains(got, "large transcript sentinel") {
		t.Fatalf("inline frame missed transcript: %q", got)
	}
}

func TestLongInputWrapsAndGrowsComposer(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(24, 14)
	model.composer.setValue(strings.Repeat("long message ", 8))

	if model.composer.height() <= 1 {
		t.Fatalf("composer height = %d", model.composer.height())
	}
	for index, line := range model.inlineFrame().dynamic {
		if strings.Contains(line, "\n") {
			t.Fatalf("dynamic row %d contains an embedded newline: %q", index, line)
		}
		if visibleLen(line) >= model.width {
			t.Fatalf("dynamic row %d is too wide (%d): %q", index, visibleLen(line), line)
		}
	}
}

func TestUserMessageGutterContinuesThroughWrappedLines(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeDark}, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(24, 14)
	lines := model.renderBlockLines(screenBlock{kind: screenBlockUser, text: strings.Repeat("wrapped message ", 5)})
	if len(lines) < 4 {
		t.Fatalf("user message did not wrap: %#v", lines)
	}
	bar := model.userBarStyle.Render(userBarMarker)
	for _, index := range []int{1, 2} {
		if !strings.HasPrefix(lines[index], bar) {
			t.Fatalf("user line %d has no styled bar: %q", index, lines[index])
		}
	}
}

func TestFrameKeepsOneBlankLineAboveTheComposer(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(60, 30)
	model.addBlock(screenBlockAssistant, "Done.")
	model.appendBlock(screenBlock{kind: screenBlockDuration, duration: 3 * time.Second})
	model.refreshTranscript()

	frame := model.inlineFrame()
	lines := append(append([]string(nil), frame.transcript...), frame.dynamic...)
	prompt := -1
	for index, line := range lines {
		if strings.HasPrefix(line, userMarker) {
			prompt = index
			break
		}
	}
	if prompt < 2 {
		t.Fatalf("composer not found in frame: %#v", lines)
	}
	// The duration block already ends in a blank line, so the idle working line
	// must not lay down a second one across the transcript/dynamic seam.
	if !isBlankTranscriptLine(lines[prompt-1]) || isBlankTranscriptLine(lines[prompt-2]) {
		t.Fatalf("blank lines above the composer: %#v", lines[:prompt+1])
	}
}

func TestFrameSeparatesWorkingStatusFromComposer(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.clearTranscript()
	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}

	dynamic := model.inlineFrame().dynamic
	working, prompt := -1, -1
	for index, line := range dynamic {
		if strings.Contains(line, "Working (") {
			working = index
		}
		if strings.HasPrefix(line, userMarker) {
			prompt = index
		}
	}
	if working < 0 || prompt != working+2 || !isBlankTranscriptLine(dynamic[working+1]) {
		t.Fatalf("working status and composer are not separated: %#v", dynamic)
	}
}

func TestToolSetPickerAlignsDescriptionsIntoOneColumn(t *testing.T) {
	fake := &fakeAgent{toolSet: "edit", toolSetTools: map[string][]string{
		"read-only": {"read", "grep"},
		"edit":      {"read", "grep", "edit"},
		"default":   {"read", "grep", "edit", "bash"},
	}}
	model := testScreenModel(t, fake)
	model.resize(100, 24)
	model.composer.setValue("/tools")
	model, _ = model.submitInput()

	// The checked row reserves its mark on every row, so the shared column
	// survives the "✓" that only the current tool set carries.
	column := -1
	for _, line := range model.renderPicker() {
		start := strings.Index(line, "read, grep")
		if start < 0 {
			continue
		}
		// The row markers are multi-byte, so measure the display width of the
		// prefix rather than its byte offset.
		start = visibleLen(line[:start])
		if column == -1 {
			column = start
		} else if start != column {
			t.Fatalf("description column %d != %d: %q", start, column, line)
		}
	}
	if column <= 0 {
		t.Fatalf("no tool lists rendered: %#v", model.renderPicker())
	}
}

func TestToolSetPickerWarnsAboutThePromptCache(t *testing.T) {
	fake := &fakeAgent{toolSet: "edit"}
	model := testScreenModel(t, fake)
	model.resize(90, 24)
	model.composer.setValue("/tools")
	model, _ = model.submitInput()
	rendered := model.renderPicker()
	if len(rendered) == 0 || !strings.Contains(rendered[len(rendered)-1], "prompt cache") {
		t.Fatalf("tool set picker has no cache warning: %#v", rendered)
	}
	// The caveat is about swapping the tool list, so other pickers stay quiet.
	model.closePicker()
	model.composer.setValue("/theme")
	model, _ = model.submitInput()
	for _, line := range model.renderPicker() {
		if strings.Contains(line, "prompt cache") {
			t.Fatalf("theme picker carries the tool set caveat: %q", line)
		}
	}
}

func TestSettingNoticeNamesTheTransitionOnlyWhenItChanged(t *testing.T) {
	fake := &fakeAgent{toolSet: "read-only"}
	model := testScreenModel(t, fake)
	model.composer.setValue("/tools edit")
	model, _ = model.submitInput()
	want := "tools: read-only → edit · prompt cache reset, next message costs full price"
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != want {
		t.Fatalf("switch notice = %q", got)
	}
	// Re-selecting the active value is not a transition, so it reports plainly.
	model.composer.setValue("/tools edit")
	model, _ = model.submitInput()
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "tools: edit" {
		t.Fatalf("no-op notice = %q", got)
	}
}

func TestNoticesArePaddedWithoutDoubleBlankLines(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(60, 30)
	model.addBlock(screenBlockUser, "change the tool set")
	model.addToolCall(agent.ToolCall{ID: "c1", Name: "bash", RawArguments: `{"command":"go build"}`})
	model.addToolCall(agent.ToolCall{ID: "c2", Name: "bash", RawArguments: `{"command":"go vet"}`})
	model.addBlock(screenBlockAssistant, "Done.")
	model.appendBlock(screenBlock{kind: screenBlockDuration, duration: 3 * time.Second})
	model.addBlock(screenBlockSystem, "tools: default")
	model.addBlock(screenBlockSystem, "theme: light")
	model.refreshTranscript()

	lines := model.transcript.lines
	if len(lines) == 0 || isBlankTranscriptLine(lines[0]) {
		t.Fatalf("transcript opens on a blank line: %#v", lines)
	}
	for index := 1; index < len(lines); index++ {
		if isBlankTranscriptLine(lines[index]) && isBlankTranscriptLine(lines[index-1]) {
			t.Fatalf("double blank line at %d: %#v", index, lines)
		}
	}
	// Every notice is surrounded by air.
	for _, notice := range []string{"Worked for", "tools: default", "theme: light"} {
		index := -1
		for candidate, line := range lines {
			if strings.Contains(line, notice) {
				index = candidate
				break
			}
		}
		if index <= 0 || !isBlankTranscriptLine(lines[index-1]) ||
			index+1 >= len(lines) || !isBlankTranscriptLine(lines[index+1]) {
			t.Fatalf("notice %q is not padded at %d: %#v", notice, index, lines)
		}
	}
	// Tool calls stay unpadded, so a run of them reads as one dense list.
	for index, line := range lines {
		if !strings.Contains(line, "go build") {
			continue
		}
		if index+1 >= len(lines) || !strings.Contains(lines[index+1], "go vet") {
			t.Fatalf("consecutive tool calls were separated: %#v", lines)
		}
	}
}

func TestMultilinePasteKeepsRendererRowsAligned(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeLight}, Config{}, &output)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(32, 14)
	model.refreshTranscript()
	if err := model.renderer.RenderFrame(model.inlineFrame(), model.width, model.height); err != nil {
		t.Fatal(err)
	}
	model.transcript.dirty = false
	output.Reset()

	updated, _ := model.Update(tea.PasteMsg{Content: "alpha\nbeta\ngamma"})
	got := updated.(screenModel)
	for index, line := range got.inlineFrame().dynamic {
		if strings.Contains(line, "\n") {
			t.Fatalf("dynamic row %d contains an embedded newline: %q", index, line)
		}
	}
	if strings.Contains(output.String(), "\x1b[?1049h") {
		t.Fatalf("paste enabled alternate screen: %q", output.String())
	}
}

func TestHistoryHotkeysUseBaseCodeAndRussianLayout(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.composer.history = []string{"first", "second"}
	model.composer.historyIndex = len(model.composer.history)

	updated, _ := model.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'з', Text: "з", BaseCode: 'p'})
	got := updated.(screenModel)
	if got.composer.value() != "second" {
		t.Fatalf("Ctrl-P by BaseCode restored %q", got.composer.value())
	}

	got.composer.historyIndex = 0
	got.composer.setValue("first")
	updated, _ = got.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'т', Text: "т"})
	got = updated.(screenModel)
	if got.composer.value() != "second" {
		t.Fatalf("Russian Ctrl-N restored %q", got.composer.value())
	}
}

func TestLayoutBaseRuneUsesRussianJCUKENFallback(t *testing.T) {
	for input, want := range map[rune]rune{'З': 'p', 'т': 'n', 'c': 'c', tea.KeyEnter: tea.KeyEnter} {
		if got := layoutBaseRune(input); got != want {
			t.Fatalf("layoutBaseRune(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestArrowHistoryStartsOnlyFromBlankEditor(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(40, 16)
	model.composer.history = []string{"older prompt"}
	model.composer.historyIndex = len(model.composer.history)
	model.composer.setValue("first line\nsecond line")
	model.composer.moveToEnd()

	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got := updated.(screenModel)
	if got.composer.value() != "first line\nsecond line" || got.composer.historyIndex != len(got.composer.history) {
		t.Fatalf("Up inside editor opened history: value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}
	updated, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got = updated.(screenModel)
	if got.composer.value() != "first line\nsecond line" || got.composer.historyIndex != len(got.composer.history) {
		t.Fatalf("Up replaced a non-empty draft: value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}

	got.composer.reset()
	got.composer.historyIndex = len(got.composer.history)
	updated, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got = updated.(screenModel)
	if got.composer.value() != "older prompt" || got.composer.historyIndex != 0 {
		t.Fatalf("blank Up restored value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}
	updated, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	got = updated.(screenModel)
	if got.composer.value() != "" || got.composer.historyIndex != len(got.composer.history) {
		t.Fatalf("Down did not restore blank draft: value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}
}

func TestCtrlPAndCtrlNPreserveCurrentDraft(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.composer.history = []string{"older prompt"}
	model.composer.historyIndex = len(model.composer.history)
	model.composer.setValue("current draft")

	updated, _ := model.Update(editorCtrlKey('p'))
	got := updated.(screenModel)
	if got.composer.value() != "older prompt" || got.composer.saved != "current draft" {
		t.Fatalf("Ctrl-P value=%q saved=%q", got.composer.value(), got.composer.saved)
	}
	updated, _ = got.Update(editorCtrlKey('n'))
	got = updated.(screenModel)
	if got.composer.value() != "current draft" || got.composer.historyIndex != len(got.composer.history) {
		t.Fatalf("Ctrl-N value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}
}

func TestArrowHistoryDoesNotReplaceSoftWrappedDraft(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.resize(20, 16)
	model.composer.history = []string{"older prompt"}
	model.composer.historyIndex = len(model.composer.history)
	model.composer.setValue(strings.Repeat("wrapped ", 8))
	model.composer.moveToEnd()

	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got := updated.(screenModel)
	if got.composer.value() == "older prompt" {
		t.Fatal("Up from wrapped input opened history")
	}
	got.composer.moveToBegin()
	updated, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	got = updated.(screenModel)
	if got.composer.value() != strings.Repeat("wrapped ", 8) || got.composer.historyIndex != len(got.composer.history) {
		t.Fatalf("Up replaced wrapped draft: value=%q index=%d", got.composer.value(), got.composer.historyIndex)
	}
}
