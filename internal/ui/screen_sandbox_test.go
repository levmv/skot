package ui

import (
	"strings"
	"testing"
)

func TestScopeCommandRunsAsCancellableMaintenance(t *testing.T) {
	fake := &fakeAgent{
		scope: "machine", scopeSummary: "scope: machine",
		scopeNotice: "A protected path is inside the workspace",
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/scope auto")
	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationScope || model.operation.cancel == nil || !model.scope.pending || model.composer.value() != "" {
		t.Fatalf("cmd=%v operation=%#v scope=%#v input=%q", cmd, model.operation, model.scope, model.composer.value())
	}
	rawMessage := cmd()
	message, ok := rawMessage.(scopeDoneMsg)
	if !ok {
		t.Fatalf("message = %T", rawMessage)
	}
	model.finishScopeSwitch(message)
	if model.operation.kind != operationNone || model.scope.pending || fake.scope != "auto" || fake.scopeSummary != "scope: workspace (auto)" {
		t.Fatalf("operation=%#v state=%#v scope=%q scopeSummary=%q", model.operation, model.scope, fake.scope, fake.scopeSummary)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; !strings.Contains(got, "filesystem scope: machine → auto") ||
		!strings.Contains(got, "warning: A protected path is inside the workspace") {
		t.Fatalf("result block = %q", got)
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
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; strings.Contains(got, "warning:") {
		t.Fatalf("result block = %q", got)
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
	if !message.concurrent {
		t.Fatalf("message = %#v", message)
	}
	model.finishScopeSwitch(message)
	if model.operation.kind != operationTurn || model.scope.pending || fake.scope != "workspace" {
		t.Fatalf("operation=%#v state=%#v scope=%q", model.operation, model.scope, fake.scope)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; !strings.Contains(got, "new processes use this scope") {
		t.Fatalf("result block = %q", got)
	}
}

func TestScopeCommandShowsCurrentEffectiveBoundary(t *testing.T) {
	fake := &fakeAgent{scope: "auto", scopeSummary: "scope: machine (auto, docker)"}
	model := testScreenModel(t, fake)
	model.composer.setValue("/scope")
	model, _ = model.submitInput()
	if model.picker.kind != pickerScope || len(model.picker.items) != 3 || model.picker.index != 0 || !model.picker.items[0].current {
		t.Fatalf("scope picker = %#v", model.picker)
	}
	frame := model.inlineFrame()
	if frame.cursor != nil || !strings.Contains(strings.Join(frame.dynamic, "\n"), "auto") {
		t.Fatalf("picker frame = %#v", frame)
	}
}

func TestSystemScopeSummaryUsesMutedStyle(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model := testScreenModel(t, &fakeAgent{})
	rendered := strings.Join(model.renderBlockLines(screenBlock{
		kind: screenBlockSystem,
		text: "scope: machine · protected paths: 2",
	}), "\n")
	if !strings.Contains(rendered, model.mutedStyle.Render("scope: machine · protected paths: 2")) {
		t.Fatalf("scope summary is not muted: %q", rendered)
	}
}
