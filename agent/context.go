package agent

import (
	"context"
	"fmt"
	"unicode/utf8"
)

const (
	minContextReserve       = 8 * 1024
	maxContextReserve       = 32 * 1024
	perMessageTokens        = 6
	perToolResultTokens     = 8
	perToolCallTokens       = 10
	perToolDefinitionTokens = 12
)

// ContextReport is a tokenizer-independent estimate of the next model input.
// Window zero means that the configured backend did not declare one.
type ContextReport struct {
	Window               int
	InputLimit           int
	InstructionTokens    int
	ToolTokens           int
	SummaryTokens        int
	HistoryTokens        int
	PendingTokens        int
	TotalInputTokens     int
	AvailableInputTokens int
	CompactionCount      int
	ToolPruningCount     int
}

func (runtime *Runtime) ContextReport(ctx context.Context) (ContextReport, error) {
	if !runtime.runMu.TryLock() {
		return ContextReport{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return ContextReport{}, fmt.Errorf("read journal for context report: %w", err)
	}
	state, err := Replay(records)
	if err != nil {
		return ContextReport{}, err
	}
	return runtime.contextReport(state, runtime.QueuedInputs()...), nil
}

func (runtime *Runtime) prepareContext(ctx context.Context, state State, pendingInput string, emit EmitFunc) (State, ContextReport, error) {
	report := runtime.contextReport(state, pendingInput)
	if report.Window == 0 || report.TotalInputTokens <= report.InputLimit {
		return state, report, nil
	}
	prunedState, prunedReport, pruningSequence, err := runtime.tryPruneToolResults(ctx, state, pendingInput)
	if err != nil {
		return state, report, fmt.Errorf("prune old tool results: %w", err)
	}
	if pruningSequence != 0 {
		emitEvent(emit, Event{Sequence: pruningSequence, Kind: EventToolResultsPruned, Text: "pruned old tool results"})
		return prunedState, prunedReport, nil
	}
	emitEvent(emit, Event{Kind: EventStatus, Text: "compacting context"})
	_, compactionRecord, err := runtime.compactLocked(ctx, state, 1, emit)
	if err != nil {
		return state, report, fmt.Errorf("estimated model input is %d tokens (limit %d) and automatic compaction failed: %w", report.TotalInputTokens, report.InputLimit, err)
	}
	emitEvent(emit, Event{Sequence: compactionRecord.Sequence, Kind: EventContextCompacted, Text: "context compacted"})
	live := reducerFromState(state)
	if err := live.apply(compactionRecord); err != nil {
		return State{}, ContextReport{}, err
	}
	state = live.state
	report = runtime.contextReport(state, pendingInput)
	if report.TotalInputTokens > report.InputLimit {
		return state, report, fmt.Errorf("estimated model input remains %d tokens after compaction (limit %d, window %d)", report.TotalInputTokens, report.InputLimit, report.Window)
	}
	return state, report, nil
}

func (runtime *Runtime) contextReport(state State, pendingInputs ...string) ContextReport {
	report := ContextReport{
		Window:           runtime.modelInfo.ContextWindow,
		CompactionCount:  state.CompactionCount,
		ToolPruningCount: state.ToolPruningCount,
	}
	if report.Window > 0 {
		reserve := min(max(report.Window/5, minContextReserve), maxContextReserve)
		report.InputLimit = max(1, report.Window-reserve)
	}
	report.InstructionTokens = estimateTextTokens(runtime.instructions)
	if runtime.instructions != "" {
		report.InstructionTokens += perMessageTokens
	}
	for _, tool := range runtime.tools {
		report.ToolTokens += estimateTextTokens(tool.Spec.Name) + estimateTextTokens(tool.Spec.Description) + estimateTextTokens(string(tool.Spec.InputSchema)) + perToolDefinitionTokens
	}
	if state.Compaction != nil {
		report.SummaryTokens = estimateTextTokens("Conversation summary:\n"+state.Compaction.Summary) + perMessageTokens
	}
	items := projectOwnedModelItems(state.verbatimModelItems(), ProviderContext{
		Backend: runtime.modelInfo.Backend,
		Epoch:   state.Selection.Epoch,
	})
	report.HistoryTokens = estimateItemsTokens(items)
	for _, pendingInput := range pendingInputs {
		if pendingInput != "" {
			report.PendingTokens += estimateTextTokens(pendingInput) + perMessageTokens
		}
	}
	report.TotalInputTokens = report.InstructionTokens + report.ToolTokens + report.SummaryTokens + report.HistoryTokens + report.PendingTokens
	if report.Window > 0 {
		report.AvailableInputTokens = max(0, report.InputLimit-report.TotalInputTokens)
	}
	return report
}

func estimateItemsTokens(items []Item) int {
	tokens := 0
	for _, item := range items {
		switch item.Kind {
		case ItemUserText, ItemBoundaryText, ItemAssistantText, ItemReasoning:
			// Opaque provider bytes count against the adapter's encoded request
			// limit, but ciphertext size is not a defensible token estimate.
			tokens += estimateTextTokens(item.Text) + perMessageTokens
		case ItemToolCall:
			if item.ToolCall != nil {
				tokens += estimateTextTokens(item.ToolCall.Name) + estimateTextTokens(item.ToolCall.RawArguments) + perToolCallTokens
			}
		case ItemToolResult:
			if item.ToolResult != nil {
				tokens += estimateTextTokens(item.ToolResult.Content) + estimateTextTokens(item.ToolResult.CallID) + perToolResultTokens
			}
		}
	}
	return tokens
}

// estimateTextTokens intentionally favors a stable conservative estimate over
// a tokenizer dependency. ASCII prose/code averages about four bytes per
// token. Counting non-ASCII UTF-8 bytes in groups of three avoids treating
// every Cyrillic rune as a whole token while remaining conservative for CJK
// and other commonly three-byte scripts.
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := 0
	nonASCIIBytes := 0
	for _, runeValue := range text {
		if runeValue < utf8.RuneSelf {
			ascii++
		} else {
			nonASCIIBytes += utf8.RuneLen(runeValue)
		}
	}
	return (ascii+3)/4 + (nonASCIIBytes+2)/3
}
