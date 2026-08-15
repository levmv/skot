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

// SessionStatus is the coherent, presentation-neutral status of one journal
// projection. Runtime publishes complete values at operation boundaries so
// observers never have to replay the journal or assemble a partial view.
type SessionStatus struct {
	ContextReport ContextReport
	Usage         ModelUsage
}

func (runtime *Runtime) SessionStatus() SessionStatus {
	runtime.statusMu.RLock()
	defer runtime.statusMu.RUnlock()
	return runtime.sessionStatus
}

// publishSessionStatus always uses the canonical next-request projection:
// tools are included and only still-queued input is treated as pending.
func (runtime *Runtime) publishSessionStatus(state State) {
	runtime.storeSessionStatus(state.LastSequence, runtime.calculateSessionStatus(state))
}

func (runtime *Runtime) calculateSessionStatus(state State) SessionStatus {
	return SessionStatus{
		ContextReport: runtime.contextReport(state, runtime.QueuedInputs()...),
		Usage:         state.Usage,
	}
}

func (runtime *Runtime) storeSessionStatus(sequence uint64, status SessionStatus) {
	runtime.statusMu.Lock()
	defer runtime.statusMu.Unlock()
	if sequence < runtime.statusSequence {
		return
	}
	runtime.statusSequence = sequence
	runtime.sessionStatus = status
}

func (runtime *Runtime) prepareContext(ctx context.Context, state State, pendingInput string, emit EmitFunc) (State, ContextReport, error) {
	return runtime.prepareContextForRequest(ctx, state, pendingInput, true, emit)
}

func (runtime *Runtime) prepareContextForRequest(ctx context.Context, state State, pendingInput string, includeTools bool, emit EmitFunc) (State, ContextReport, error) {
	report := runtime.contextReportForRequest(state, includeTools, pendingInput)
	if report.Window == 0 || report.TotalInputTokens <= report.InputLimit {
		return state, report, nil
	}
	prunedState, prunedReport, pruningSequence, err := runtime.tryPruneToolResults(ctx, state, pendingInput, includeTools)
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
	report = runtime.contextReportForRequest(state, includeTools, pendingInput)
	if report.TotalInputTokens > report.InputLimit {
		return state, report, fmt.Errorf("estimated model input remains %d tokens after compaction (limit %d, window %d)", report.TotalInputTokens, report.InputLimit, report.Window)
	}
	return state, report, nil
}

func (runtime *Runtime) contextReport(state State, pendingInputs ...string) ContextReport {
	return runtime.contextReportForRequest(state, true, pendingInputs...)
}

func (runtime *Runtime) contextReportForRequest(state State, includeTools bool, pendingInputs ...string) ContextReport {
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
	if includeTools {
		for _, tool := range runtime.tools {
			report.ToolTokens += estimateTextTokens(tool.Spec.Name) + estimateTextTokens(tool.Spec.Description) + estimateTextTokens(string(tool.Spec.InputSchema)) + perToolDefinitionTokens
		}
	}
	if state.Compaction != nil {
		report.SummaryTokens = estimateTextTokens("Conversation summary:\n"+state.Compaction.Summary) + perMessageTokens
	}
	// Project pending input with its history: current-turn routes drop the
	// previous turn's reasoning once the next user message is appended.
	items := state.verbatimModelItems()
	pending := 0
	for _, pendingInput := range pendingInputs {
		if pendingInput != "" {
			items = append(items, Item{Kind: ItemUserText, Text: pendingInput})
			pending++
		}
	}
	projected := runtime.projectModelItems(items, ProviderContext{
		Backend: runtime.modelInfo.Backend,
		Epoch:   state.Selection.Epoch,
	})
	// Projection may remove reasoning only, so pending users remain the suffix.
	boundary := max(0, len(projected)-pending)
	report.HistoryTokens = estimateItemsTokens(projected[:boundary])
	report.PendingTokens = estimateItemsTokens(projected[boundary:])
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
