package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	if model.picker.kind != pickerScope || len(model.picker.items) != 2 {
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
