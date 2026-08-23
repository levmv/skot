package ui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/app"
)

func TestScopeCommandRunsAsCancellableMaintenance(t *testing.T) {
	fake := &fakeAgent{
		scope: "machine", scopeSummary: "scope: machine",
		scopeNotice: "A protected path is inside the workspace",
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/scope workspace")
	model, cmd := model.submitInput()
	maintenance := model.maintenanceOperation()
	if cmd == nil || model.operation.kind != operationNone || maintenance.kind != operationScope || maintenance.cancel == nil || !model.scope.pending || model.composer.value() != "" {
		t.Fatalf("cmd=%v operation=%#v maintenance=%#v scope=%#v input=%q", cmd, model.operation, maintenance, model.scope, model.composer.value())
	}
	rawMessage := cmd()
	message, ok := rawMessage.(scopeDoneMsg)
	if !ok {
		t.Fatalf("message = %T", rawMessage)
	}
	model.finishScopeSwitch(message)
	if model.maintenanceOperation().isMaintenance() || model.scope.pending || fake.scope != "workspace" || fake.scopeSummary != "scope: workspace" {
		t.Fatalf("operation=%#v state=%#v scope=%q scopeSummary=%q", model.operation, model.scope, fake.scope, fake.scopeSummary)
	}
	block := model.transcript.blocks[len(model.transcript.blocks)-1]
	if block.kind != screenBlockScopeChange || block.text != "filesystem scope: machine → workspace\nwarning: A protected path is inside the workspace" {
		t.Fatalf("result block = %#v", block)
	}
}

func TestScopeCommandDoesNotRepeatNoticeWithoutSwitch(t *testing.T) {
	fake := &fakeAgent{
		scope: "workspace", scopeSummary: "scope: workspace",
		scopeNotice: "A protected path is inside the workspace",
	}
	model := testScreenModel(t, fake)
	cmd := model.startScopeSwitch("workspace")
	rawMessage := cmd()
	message, ok := rawMessage.(scopeDoneMsg)
	if !ok {
		t.Fatalf("message = %T", rawMessage)
	}
	model.finishScopeSwitch(message)
	if got := model.transcript.blocks[len(model.transcript.blocks)-1]; got.kind != screenBlockScopeChange || got.text != "filesystem scope: workspace" {
		t.Fatalf("result block = %#v", got)
	}
}

func TestScopeChangeKeepsOnlyAdditionalSummary(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.scope = scopeSwitchState{pending: true, previous: "workspace"}
	model.finishScopeSwitch(scopeDoneMsg{
		scope: "machine", summary: "scope: machine · protected paths: /private, /secrets",
	})
	if got := model.transcript.blocks[len(model.transcript.blocks)-1]; got.kind != screenBlockScopeChange ||
		got.text != "filesystem scope: workspace → machine\nprotected paths: /private, /secrets" {
		t.Fatalf("result block = %#v", got)
	}
}

func TestScopeCommandCanSwitchDuringTurn(t *testing.T) {
	fake := &fakeAgent{scope: "machine", scopeSummary: "scope: machine"}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationTurn}
	model.composer.setValue("/scope workspace")
	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationTurn || !model.scope.pending {
		t.Fatalf("cmd=%v operation=%#v scope=%#v", cmd, model.operation, model.scope)
	}
	rawMessage := cmd()
	message, ok := rawMessage.(scopeDoneMsg)
	if !ok {
		t.Fatalf("message = %T", rawMessage)
	}
	model.finishScopeSwitch(message)
	if model.operation.kind != operationTurn || model.scope.pending || fake.scope != "workspace" {
		t.Fatalf("operation=%#v state=%#v scope=%q", model.operation, model.scope, fake.scope)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1]; got.kind != screenBlockScopeChange || got.text != "filesystem scope: machine → workspace" {
		t.Fatalf("result block = %#v", got)
	}
}

func TestPendingScopeOwnsMaintenanceUIAfterConcurrentTurnEnds(t *testing.T) {
	fake := &fakeAgent{scope: "machine", scopeSummary: "scope: machine"}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationTurn, startedAt: time.Now()}
	command := model.startScopeSwitch("workspace")

	model, _ = model.update(agentDoneMsg{})
	maintenance := model.maintenanceOperation()
	if !model.scope.pending || model.operation.kind != operationNone || maintenance.kind != operationScope || maintenance.cancel == nil {
		t.Fatalf("operation=%#v maintenance=%#v scope=%#v", model.operation, maintenance, model.scope)
	}
	model.composer.setValue("/clear")
	model, blocked := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, BaseCode: tea.KeyEnter})
	if blocked != nil || model.composer.value() != "/clear" || fake.clearID != "" {
		t.Fatalf("pending scope accepted maintenance: command=%v input=%q clear=%q", blocked, model.composer.value(), fake.clearID)
	}

	message, ok := command().(scopeDoneMsg)
	if !ok {
		t.Fatalf("message = %T", message)
	}
	model, _ = model.update(message)
	if model.maintenanceOperation().isMaintenance() || model.scope.pending || fake.scope != "workspace" {
		t.Fatalf("operation=%#v scope=%#v current=%q", model.operation, model.scope, fake.scope)
	}
}

func TestShiftTabCyclesScopeDuringTurnWithoutChangingDraft(t *testing.T) {
	fake := &fakeAgent{scope: "workspace", scopeSummary: "scope: workspace"}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationTurn}
	model.composer.setValue("keep this draft")
	shiftTab := tea.KeyPressMsg{Code: tea.KeyTab, BaseCode: tea.KeyTab, Mod: tea.ModShift}

	for _, want := range []string{"machine", "workspace"} {
		var command tea.Cmd
		model, command = model.handleKey(shiftTab)
		if command == nil || !model.scope.pending || model.operation.kind != operationTurn {
			t.Fatalf("switch to %q: command=%v operation=%#v scope=%#v", want, command, model.operation, model.scope)
		}
		message, ok := command().(scopeDoneMsg)
		if !ok {
			t.Fatalf("switch to %q returned an unexpected message", want)
		}
		model.finishScopeSwitch(message)
		if fake.scope != want || model.scope.pending {
			t.Fatalf("switch to %q: scope=%q state=%#v", want, fake.scope, model.scope)
		}
	}
	if got := model.composer.value(); got != "keep this draft" {
		t.Fatalf("draft = %q", got)
	}
}

func TestScopeCommandShowsCurrentBoundary(t *testing.T) {
	fake := &fakeAgent{scope: "machine", scopeSummary: "scope: machine"}
	model := testScreenModel(t, fake)
	model.composer.setValue("/scope")
	model, _ = model.submitInput()
	// The picker opens on the scope in use, wherever that sits in the list.
	// Two scopes plus the row which opens each path prompt.
	if model.picker.kind != pickerScope || len(model.picker.items) != 4 {
		t.Fatalf("scope picker = %#v", model.picker)
	}
	if selected := model.picker.items[model.picker.index]; selected.value != "machine" || !selected.current {
		t.Fatalf("scope picker selection = %#v", selected)
	}
	frame := model.inlineFrame()
	rendered := strings.Join(frame.dynamic, "\n")
	if frame.cursor != nil || !strings.Contains(rendered, "machine") || !strings.Contains(rendered, "Shift+Tab") {
		t.Fatalf("picker frame = %#v", frame)
	}
}

func TestScopeChangeUsesNormalHeadlineAndMutedDetails(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model := testScreenModel(t, &fakeAgent{})
	headline := "filesystem scope: workspace → machine"
	detail := "protected paths: /private, /secrets"
	rendered := strings.Join(model.renderBlockLines(screenBlock{
		kind: screenBlockScopeChange,
		text: headline + "\n" + detail,
	}), "\n")
	if !strings.Contains(rendered, headline) || strings.Contains(rendered, model.mutedStyle.Render(headline)) ||
		!strings.Contains(rendered, model.mutedStyle.Render(detail)) {
		t.Fatalf("scope change styles = %q", rendered)
	}
}

func scopePickerTestAgent() *fakeAgent {
	return &fakeAgent{
		scope: "workspace", scopeSummary: "scope: workspace",
		addedPaths: []FilesystemPath{
			{Path: "/workspaces/shared", Origin: app.FilesystemPathRemembered},
			{Path: "/tmp/generated", Origin: app.FilesystemPathInvocation},
		},
		protectedPaths: []FilesystemPath{{Path: "/home/user/.env", Origin: app.FilesystemPathSettings}},
	}
}

func TestScopePickerShowsPathsWithoutTakingTheScopeDigits(t *testing.T) {
	model := testScreenModel(t, scopePickerTestAgent())
	model.openScopePicker()

	if len(model.picker.items) != 7 {
		t.Fatalf("scope picker rows = %#v", model.picker.items)
	}
	// Scopes lead the list, so 1 and 2 keep meaning what they always did.
	if model.picker.numberedRows() != 2 || !model.picker.numberSelectionEnabled() {
		t.Fatalf("numbered rows = %d", model.picker.numberedRows())
	}
	model, _ = model.handlePickerKey(tea.KeyPressMsg{Text: "2", Code: '2', BaseCode: '2'})
	if !model.scope.pending {
		t.Fatalf("digit did not start a scope switch: %#v", model.scope)
	}

	model = testScreenModel(t, scopePickerTestAgent())
	model.openScopePicker()
	// Only what departs from an ordinary remembered row is spelled out.
	if got := model.picker.items[2]; got.description != "" || got.activeDetail != "(d or backspace to remove)" {
		t.Fatalf("remembered row = %#v", got)
	}
	rendered := strings.Join(model.renderPicker(), "\n")
	for _, want := range []string{"-add-dir this run", "config.json"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("picker %q is missing %q", rendered, want)
		}
	}
}

func TestScopePickerRemovesRememberedPath(t *testing.T) {
	fake := scopePickerTestAgent()
	model := testScreenModel(t, fake)
	model.openScopePicker()
	model.picker.index = 2

	model, command := model.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if command == nil {
		t.Fatal("backspace did not start a removal")
	}
	message, ok := command().(scopeDoneMsg)
	if !ok {
		t.Fatal("removal result has unexpected type")
	}
	model.finishScopeSwitch(message)
	if fake.removedPath != "/workspaces/shared" {
		t.Fatalf("removed path = %q", fake.removedPath)
	}
	if len(model.picker.items) != 6 || !model.picker.active() {
		t.Fatalf("picker after removal = %#v", model.picker.items)
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockScopeChange || !strings.Contains(last.text, "removed added directory: /workspaces/shared") {
		t.Fatalf("removal block = %#v", last)
	}

	model = testScreenModel(t, scopePickerTestAgent())
	model.openScopePicker()
	model.picker.index = 2
	if _, command = model.handlePickerKey(tea.KeyPressMsg{Text: "d", Code: 'd', BaseCode: 'd'}); command == nil {
		t.Fatal("d did not start a removal")
	}
}

func TestScopePickerKeepsPathsTheSessionDoesNotOwn(t *testing.T) {
	fake := scopePickerTestAgent()
	fake.removeErr = errors.New("/tmp/generated was given with -add-dir for this run")
	model := testScreenModel(t, fake)
	model.openScopePicker()
	model.picker.index = 3

	model, command := model.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if command == nil {
		t.Fatal("removal was not attempted")
	}
	message, ok := command().(scopeDoneMsg)
	if !ok {
		t.Fatal("removal result has unexpected type")
	}
	model.finishScopeSwitch(message)
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockError || !strings.Contains(last.text, "-add-dir for this run") {
		t.Fatalf("refusal block = %#v", last)
	}
	if len(model.picker.items) != 7 {
		t.Fatalf("refused row left the picker: %#v", model.picker.items)
	}
}

func TestScopePickerEnterLeavesPathRowsAlone(t *testing.T) {
	fake := scopePickerTestAgent()
	model := testScreenModel(t, fake)
	model.openScopePicker()
	model.picker.index = 2

	model, command := model.selectPickerItem()
	if command != nil || !model.picker.active() || fake.removedPath != "" {
		t.Fatalf("enter acted on a path row: command=%v picker=%v removed=%q", command != nil, model.picker.active(), fake.removedPath)
	}
}

func TestScopePickerAddRowCollectsAPathAndAddsIt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared-library"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := scopePickerTestAgent()
	fake.scopeNotice = "added directory contains a protected path"
	model := testScreenModel(t, fake)
	model.config.Root = root
	model.openScopePicker()
	model.picker.index = 4 // the row which closes the added section

	model, command := model.selectPickerItem()
	if command != nil || model.pathPrompt != addedDirectoryRow || model.picker.active() {
		t.Fatalf("add row did not open the prompt: prompt=%d picker=%v", model.pathPrompt, model.picker.active())
	}
	// The prompt names itself in the dynamic area, so reopening it never piles
	// lines up in the transcript.
	frame := strings.Join(model.inlineFrame().dynamic, "\n")
	if !strings.Contains(frame, "directory to add · tab completes · esc returns to /scope") {
		t.Fatalf("prompt frame = %q", frame)
	}

	// Typing offers real directories, and tab accepts the highlighted one.
	model.composer.setValue("shared")
	model.syncCommandSuggestions()
	if !model.commandSuggestionsVisible() || model.currentCommandSuggestion() != "shared-library"+string(filepath.Separator) {
		t.Fatalf("completion = %q visible=%v", model.currentCommandSuggestion(), model.commandSuggestionsVisible())
	}
	model, _ = model.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := model.composer.value(); got != "shared-library"+string(filepath.Separator) {
		t.Fatalf("tab left %q", got)
	}

	model, command = model.submitInput()
	if command == nil {
		t.Fatal("submitting the prompt did not start an addition")
	}
	message, ok := command().(scopeDoneMsg)
	if !ok {
		t.Fatal("addition result has unexpected type")
	}
	model.finishScopeSwitch(message)
	if fake.addedPath != "shared-library"+string(filepath.Separator) {
		t.Fatalf("added path = %q", fake.addedPath)
	}
	if model.pathPrompt != notFilesystemPath || model.composer.value() != "" {
		t.Fatalf("prompt outlived the submission: prompt=%d value=%q", model.pathPrompt, model.composer.value())
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockScopeChange || !strings.Contains(last.text, "added directory: shared-library") ||
		!strings.Contains(last.text, "warning: "+fake.scopeNotice) {
		t.Fatalf("addition block = %#v", last)
	}
	// A path typed from the menu belongs to the menu: it reopens on the section
	// which was just edited.
	if !model.picker.active() || model.picker.kind != pickerScope {
		t.Fatal("a successful addition did not return to the menu")
	}
	if selected := model.picker.selectedItem(); !selected.addRow || selected.filesystemPath != addedDirectoryRow {
		t.Fatalf("selection after adding = %#v", selected)
	}
}

func TestScopePickerAddPromptSurvivesARefusedPath(t *testing.T) {
	fake := scopePickerTestAgent()
	fake.addErr = errors.New("/nope is already inside the workspace")
	model := testScreenModel(t, fake)
	model.openScopePicker()
	model.picker.index = 4

	model, _ = model.selectPickerItem()
	model.composer.setValue("nope")
	model, command := model.submitInput()
	if command == nil {
		t.Fatal("submitting the prompt did not start an addition")
	}
	message, ok := command().(scopeDoneMsg)
	if !ok {
		t.Fatal("addition result has unexpected type")
	}
	model.finishScopeSwitch(message)

	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockError || !strings.Contains(last.text, "already inside the workspace") {
		t.Fatalf("refusal block = %#v", last)
	}
	// The prompt keeps collecting, with the refused path ready to edit.
	if model.pathPrompt != addedDirectoryRow || model.composer.value() != "nope" || model.picker.active() {
		t.Fatalf("prompt after refusal: prompt=%d value=%q picker=%v",
			model.pathPrompt, model.composer.value(), model.picker.active())
	}
}

func TestScopePickerAddPromptReturnsOnEscape(t *testing.T) {
	fake := scopePickerTestAgent()
	model := testScreenModel(t, fake)
	model.openScopePicker()
	model.picker.index = 6 // the row which closes the protected section

	model, _ = model.selectPickerItem()
	if model.pathPrompt != protectedPathRow {
		t.Fatalf("prompt = %d", model.pathPrompt)
	}
	model.composer.setValue("secrets")
	model, command := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if command != nil || model.pathPrompt != notFilesystemPath || fake.addedPath != "" {
		t.Fatalf("escape added something: prompt=%d added=%q", model.pathPrompt, fake.addedPath)
	}
	if !model.picker.active() || model.picker.kind != pickerScope || model.composer.value() != "" {
		t.Fatalf("escape did not return to the menu: picker=%v value=%q", model.picker.active(), model.composer.value())
	}
}

func TestScopeChangeGivesEachPolicyFactItsOwnLine(t *testing.T) {
	model := testScreenModel(t, &fakeAgent{})
	model.finishScopeSwitch(scopeDoneMsg{
		scope:   "workspace",
		summary: "scope: workspace · added paths: /shared · protected paths: /private",
	})
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	want := "added paths: /shared\nprotected paths: /private"
	if last.kind != screenBlockScopeChange || !strings.Contains(last.text, want) {
		t.Fatalf("scope change block = %q", last.text)
	}
}
