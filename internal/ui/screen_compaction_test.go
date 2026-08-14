package ui

import (
	"strings"
	"testing"

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
	if !strings.Contains(got, "context: 12k / 80k") || !strings.Contains(got, "history 12k") {
		t.Fatalf("context report = %q", got)
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
