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
// Window zero means that no context window is known for the selected model.
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
	ImageDelivery ImageDeliveryStatus
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
		ImageDelivery: runtime.visibleImageDelivery(state),
	}
}

func (runtime *Runtime) visibleImageDelivery(state State) ImageDeliveryStatus {
	firstSequence := state.firstVerbatimSequence()
	for _, block := range state.Blocks {
		if firstSequence != 0 && block.StartSequence < firstSequence {
			continue
		}
		for _, entry := range block.Entries {
			if state.ToolPruning != nil && entry.Sequence <= state.ToolPruning.ThroughSequence {
				continue
			}
			if entry.Item.ToolResult != nil && entry.Item.ToolResult.Content.HasImage() {
				return runtime.effectiveImageDelivery(state.ImageDelivery.Status)
			}
		}
	}
	return ImageDeliveryUnknown
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

func (runtime *Runtime) prepareRunRequestContext(ctx context.Context, live *stateReducer, spec runRequestSpec, emit EmitFunc) (ContextReport, error) {
	report := runtime.requestReport(live.state, spec)
	for report.Window > 0 && report.TotalInputTokens > report.InputLimit {
		var err error
		report, err = runtime.shrinkRunRequestOnce(ctx, live, spec, report, emit)
		if err != nil {
			return report, fmt.Errorf("estimated model input is %d tokens (limit %d) and automatic context reduction failed: %w", report.TotalInputTokens, report.InputLimit, err)
		}
	}
	return report, nil
}

// shrinkRunRequestOnce advances durable context maintenance by one step on a
// successful return. Cheap pruning of old tool-result projections gets the
// first chance; if it cannot advance its boundary, the oldest available
// conversation prefix is summarized. The returned report describes the
// request rebuilt from the committed state.
func (runtime *Runtime) shrinkRunRequestOnce(ctx context.Context, live *stateReducer, spec runRequestSpec, report ContextReport, emit EmitFunc) (ContextReport, error) {
	pruningRecord, nextReport, err := runtime.pruneOldToolResultsOnce(ctx, live.state, spec, report)
	if err != nil {
		return report, fmt.Errorf("prune old tool results: %w", err)
	}
	if pruningRecord.Sequence != 0 {
		if err := live.apply(pruningRecord); err != nil {
			return report, fmt.Errorf("apply committed tool-result pruning: %w", err)
		}
		nextReport.ToolPruningCount = live.state.ToolPruningCount
		runtime.publishSessionStatus(live.state)
		emitEvent(emit, Event{Sequence: pruningRecord.Sequence, Kind: EventToolResultsPruned, Text: "pruned old tool results"})
		return nextReport, nil
	}

	_, compactionRecord, err := runtime.compactLocked(ctx, live.state, spec, emit)
	if err != nil {
		return report, fmt.Errorf("compact context: %w", err)
	}
	if err := live.apply(compactionRecord); err != nil {
		return report, fmt.Errorf("apply committed context compaction: %w", err)
	}
	runtime.publishSessionStatus(live.state)
	emitEvent(emit, Event{Sequence: compactionRecord.Sequence, Kind: EventContextCompacted, Text: "context compacted"})
	return runtime.requestReport(live.state, spec), nil
}

func (runtime *Runtime) requestReport(state State, spec runRequestSpec) ContextReport {
	return runtime.contextReportForRequest(state, !spec.omitTools, spec.extraUserText)
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
		report.SummaryTokens = estimateTextTokens(ConversationSummaryPrefix+state.Compaction.Summary) + perMessageTokens
	}
	items, boundary := runtime.projectedItemsForRequest(state, state.verbatimModelItems(), pendingInputs...)
	report.HistoryTokens = estimateItemsTokens(items[:boundary])
	report.PendingTokens = estimateItemsTokens(items[boundary:])
	report.TotalInputTokens = report.InstructionTokens + report.ToolTokens + report.SummaryTokens + report.HistoryTokens + report.PendingTokens
	if report.Window > 0 {
		report.AvailableInputTokens = max(0, report.InputLimit-report.TotalInputTokens)
	}
	return report
}

// projectedItemsForRequest projects pending input together with its history:
// current-turn routes drop the previous turn's reasoning only after the next
// user message has been appended.
func (runtime *Runtime) projectedItemsForRequest(state State, items []Item, pendingInputs ...string) ([]Item, int) {
	pending := 0
	for _, pendingInput := range pendingInputs {
		if pendingInput != "" {
			items = append(items, Item{Kind: ItemUserText, Text: pendingInput})
			pending++
		}
	}
	projected := runtime.projectModelItems(items, ProviderContext{
		Backend: runtime.modelInfo.BackendID,
		Epoch:   state.Selection.Epoch,
	}, state.ImageDelivery.Status)
	// Projection may remove reasoning only, so pending users remain the suffix.
	boundary := max(0, len(projected)-pending)
	return projected, boundary
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
				tokens += estimateContentTokens(item.ToolResult.Content) + estimateTextTokens(item.ToolResult.CallID) + perToolResultTokens
			}
		}
	}
	return tokens
}

func estimateContentTokens(content Content) int {
	tokens := 0
	for _, part := range content {
		switch part.Kind {
		case ContentPartText:
			tokens += estimateTextTokens(part.Text)
		case ContentPartImage:
			if part.Image != nil {
				// Normalization bounds dimensions before content reaches this
				// layer. The 28x28 grid is a stable provider-neutral geometry
				// heuristic; exact visual-token accounting differs by route.
				tokens += ((part.Image.Width + 27) / 28) * ((part.Image.Height + 27) / 28)
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
