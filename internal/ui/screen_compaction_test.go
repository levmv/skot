package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/levmv/skot/agent"
)

func TestContextCommandShowsBudgetBreakdown(t *testing.T) {
	fake := &fakeAgent{contextReport: agent.ContextReport{
		Window: 100_000, InputLimit: 80_000, TotalInputTokens: 12_500, AvailableInputTokens: 67_500,
		InstructionTokens: 100, ToolTokens: 400, HistoryTokens: 12_000,
	}}
	model := testScreenModel(t, fake)
	model.composer.setValue("/context")
	model, _ = model.submitInput()
	got := model.transcript.blocks[len(model.transcript.blocks)-1].text
	if !strings.Contains(got, "context: 12.5k / 80k") || !strings.Contains(got, "history 12k") {
		t.Fatalf("context report = %q", got)
	}
}

func TestCompactionAcceptsInputForTheNextTurn(t *testing.T) {
	fake := &fakeAgent{}
	model := testScreenModel(t, fake)
	model.operation = activeOperation{kind: operationCompaction}

	model, _ = model.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n', BaseCode: 'n'})
	if model.composer.value() != "n" {
		t.Fatalf("input during compaction = %q", model.composer.value())
	}
	if dynamic, editorStart := model.baseInlineDynamic(); editorStart < 0 || !strings.Contains(strings.Join(dynamic, "\n"), "n") {
		t.Fatalf("editor during compaction: start=%d lines=%q", editorStart, dynamic)
	}

	model.composer.setValue("follow up")
	model, command := model.submitInput()
	if command != nil || model.operation.kind != operationCompaction {
		t.Fatalf("submit command=%v operation=%#v", command, model.operation)
	}
	if len(fake.queued) != 1 || fake.queued[0] != "follow up" || model.composer.value() != "" {
		t.Fatalf("queue=%#v input=%q", fake.queued, model.composer.value())
	}

	command = model.finishCompaction(compactionDoneMsg{})
	if command == nil || model.operation.kind != operationTurn || len(fake.queued) != 0 {
		t.Fatalf("finish command=%v operation=%#v queue=%#v", command, model.operation, fake.queued)
	}
	last := model.transcript.blocks[len(model.transcript.blocks)-1]
	if last.kind != screenBlockUser || last.text != "follow up" {
		t.Fatalf("last block = %#v", last)
	}
}

func TestCompactCommandRunsAsCancellableMaintenance(t *testing.T) {
	fake := &fakeAgent{
		compaction:    agent.ContextCompactedRecord{CoveredThroughSequence: 42},
		contextReport: agent.ContextReport{TotalInputTokens: 900},
	}
	model := testScreenModel(t, fake)
	model.composer.setValue("/compact")
	model, cmd := model.submitInput()
	if cmd == nil || model.operation.kind != operationCompaction || model.operation.cancel == nil {
		t.Fatalf("cmd=%v operation=%#v", cmd, model.operation)
	}
	message, ok := cmd().(compactionDoneMsg)
	if !ok {
		t.Fatalf("message type is not compactionDoneMsg")
	}
	model.finishCompaction(message)
	if model.operation.kind != operationNone {
		t.Fatalf("operation = %#v", model.operation)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; strings.Contains(got, "sequence 42") || !strings.Contains(got, "context compacted") || !strings.Contains(got, "900 estimated") {
		t.Fatalf("compaction result = %q", got)
	}
}
