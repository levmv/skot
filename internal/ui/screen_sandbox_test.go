package ui

import (
	"strings"
	"testing"
)

func TestSandboxCommandRunsAsCancellableMaintenance(t *testing.T) {
	fake := &fakeAgent{sandbox: "off", security: "sandbox: off"}
	model := testScreenModel(t, fake)
	model.composer.setValue("/sandbox auto")
	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationSandbox || model.operation.cancel == nil || !model.sandbox.pending || model.composer.value() != "" {
		t.Fatalf("cmd=%v operation=%#v sandbox=%#v input=%q", cmd, model.operation, model.sandbox, model.composer.value())
	}
	message, ok := cmd().(sandboxDoneMsg)
	if !ok {
		t.Fatalf("message = %T", cmd())
	}
	model.finishSandboxSwitch(message)
	if model.operation.kind != operationNone || model.sandbox.pending || fake.sandbox != "auto" || fake.security != "sandbox: workspace (auto)" {
		t.Fatalf("operation=%#v state=%#v sandbox=%q security=%q", model.operation, model.sandbox, fake.sandbox, fake.security)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; !strings.Contains(got, "sandbox policy: auto") {
		t.Fatalf("result block = %q", got)
	}
}

func TestSandboxCommandCanSwitchPolicyDuringTurn(t *testing.T) {
	fake := &fakeAgent{sandbox: "off", security: "sandbox: off"}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationTurn}
	model.composer.setValue("/sandbox workspace")
	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationTurn || !model.sandbox.pending {
		t.Fatalf("cmd=%v operation=%#v sandbox=%#v", cmd, model.operation, model.sandbox)
	}
	message, ok := cmd().(sandboxDoneMsg)
	if !ok || !message.concurrent {
		t.Fatalf("message = %#v (%T)", message, message)
	}
	model.finishSandboxSwitch(message)
	if model.operation.kind != operationTurn || model.sandbox.pending || fake.sandbox != "workspace" {
		t.Fatalf("operation=%#v state=%#v sandbox=%q", model.operation, model.sandbox, fake.sandbox)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; !strings.Contains(got, "new processes use this policy") {
		t.Fatalf("result block = %q", got)
	}
}

func TestSandboxCommandShowsCurrentEffectiveBoundary(t *testing.T) {
	fake := &fakeAgent{sandbox: "auto", security: "sandbox: masked (auto, docker)"}
	model := testScreenModel(t, fake)
	model.composer.setValue("/sandbox")
	model, _ = model.submitInput()
	if model.picker.kind != pickerSandbox || len(model.picker.items) != 4 || model.picker.index != 0 || !model.picker.items[0].current {
		t.Fatalf("sandbox picker = %#v", model.picker)
	}
	frame := model.inlineFrame()
	if frame.cursor != nil || !strings.Contains(strings.Join(frame.dynamic, "\n"), "auto") {
		t.Fatalf("picker frame = %#v", frame)
	}
}

func TestSystemSecuritySummaryHighlightsOnlySandboxOff(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model := testScreenModel(t, &fakeAgent{})
	rendered := strings.Join(model.renderBlockLines(screenBlock{
		kind: screenBlockSystem,
		text: "sandbox: off · sandbox probe failed",
	}), "\n")
	styledOff := model.errorStyle.Render("off")
	if styledOff == "off" {
		t.Fatal("test error style did not emit color")
	}
	if !strings.Contains(rendered, styledOff) {
		t.Fatalf("sandbox off is not highlighted: %q", rendered)
	}
	if strings.Contains(rendered, model.errorStyle.Render("sandbox")) || strings.Contains(rendered, model.errorStyle.Render("sandbox probe failed")) {
		t.Fatalf("security summary over-highlights the diagnostic: %q", rendered)
	}
}
