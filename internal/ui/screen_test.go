package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
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
	reasoningEffort  string
	reasoningEfforts map[string][]string
	modelErr         error
	knownModels      []string
	modelChoices     []ModelChoice
	sandbox          string
	security         string
	sandboxErr       error
	contextReport    agent.ContextReport
	contextErr       error
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
	if fake.toolSetErr != nil {
		return fake.toolSetErr
	}
	fake.toolSet = toolSet
	return nil
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

func (fake *fakeAgent) SwitchModel(_ context.Context, model, effort string) error {
	if fake.modelErr != nil {
		return fake.modelErr
	}
	fake.model = model
	fake.reasoningEffort = effort
	return nil
}

func (fake *fakeAgent) CurrentReasoningEffort() string { return fake.reasoningEffort }

func (fake *fakeAgent) ReasoningEfforts(uri string) []string {
	if efforts := fake.reasoningEfforts[uri]; len(efforts) != 0 {
		return append([]string(nil), efforts...)
	}
	return []string{"", "high"}
}

func (fake *fakeAgent) CurrentSandbox() string { return fake.sandbox }

func (fake *fakeAgent) SecuritySummary() string { return fake.security }

func (fake *fakeAgent) SwitchSandbox(_ context.Context, policy string) error {
	if fake.sandboxErr != nil {
		return fake.sandboxErr
	}
	fake.sandbox = policy
	effective := policy
	detail := ""
	if policy == "auto" {
		effective = "workspace"
		detail = " (auto)"
	}
	fake.security = "sandbox: " + effective + detail
	return nil
}

func (fake *fakeAgent) ContextReport(context.Context) (agent.ContextReport, error) {
	return fake.contextReport, fake.contextErr
}

func (fake *fakeAgent) Compact(context.Context, int) (agent.ContextCompactedRecord, error) {
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
	for _, line := range strings.Split(tuiCommandHelp, "\n") {
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

	model, _ = model.submitInput()
	if len(fake.queued) != 1 || fake.queued[0] != "follow up" {
		t.Fatalf("queued = %#v", fake.queued)
	}
	if model.composer.value() != "" {
		t.Fatalf("input was not cleared: %q", model.composer.value())
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "queued: follow up" {
		t.Fatalf("status = %q", got)
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
}

func TestDiscardedAttemptRemovesPartialAssistant(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptStarted, AttemptID: "attempt-1"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventTextDelta, AttemptID: "attempt-1", Text: "partial"})
	model.applyAgentEvent(agent.Event{Kind: agent.EventModelAttemptDiscarded, AttemptID: "attempt-1"})

	for _, block := range model.transcript.blocks {
		if block.kind == screenBlockAssistant && strings.Contains(block.text, "partial") {
			t.Fatalf("partial assistant block survived: %#v", block)
		}
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "interrupted response removed" {
		t.Fatalf("status = %q", got)
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

func TestTextDeltasFlushAtFrameBoundary(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.appendBlock(screenBlock{kind: screenBlockAssistant, text: "before", attemptID: "attempt"})
	model.refreshTranscript()
	before := strings.Join(model.transcript.lines, "\n")

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

	updated, _ = got.Update(transcriptRenderMsg{})
	got = updated.(screenModel)
	if got.operation.renderPending || !strings.Contains(strings.Join(got.transcript.lines, "\n"), "before after") {
		t.Fatalf("frame did not flush delta: pending=%v transcript=%q", got.operation.renderPending, got.transcript.lines)
	}
}

func TestTranscriptTracksDirtyRenderedSuffix(t *testing.T) {
	var transcript transcriptState
	render := func(block screenBlock) []string { return []string{block.text} }
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
		{Kind: agent.ItemReasoning, Text: "private summary", ProviderData: []agent.ProviderData{{Kind: "responses.reasoning_item", Data: json.RawMessage(`{"encrypted_content":"opaque-secret"}`)}}},
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

func TestMultilinePasteKeepsRendererRowsAligned(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{}, Config{}, &output)
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
