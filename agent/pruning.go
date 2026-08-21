package agent

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	defaultPrunedToolHeadBytes = 2 * 1024
	defaultPrunedToolTailBytes = 2 * 1024
)

func (runtime *Runtime) pruneOldToolResultsOnce(ctx context.Context, state State, spec runRequestSpec, report ContextReport) (Record, ContextReport, error) {
	if len(state.Blocks) < 2 {
		return Record{}, report, nil
	}
	boundaryIndex := len(state.Blocks) - 2
	if state.hasUnfinishedWork() {
		unfinishedStart, ok := state.firstUnfinishedBlock()
		if !ok {
			return Record{}, report, errors.New("cannot locate unfinished conversation block")
		}
		boundaryIndex = min(boundaryIndex, unfinishedStart-1)
	}
	if boundaryIndex < 0 {
		return Record{}, report, nil
	}
	payload := ToolResultsPrunedRecord{
		ThroughSequence: state.Blocks[boundaryIndex].EndSequence,
		HeadBytes:       defaultPrunedToolHeadBytes,
		TailBytes:       defaultPrunedToolTailBytes,
	}
	if state.ToolPruning != nil {
		if payload.ThroughSequence <= state.ToolPruning.ThroughSequence {
			return Record{}, report, nil
		}
		payload.HeadBytes = state.ToolPruning.HeadBytes
		payload.TailBytes = state.ToolPruning.TailBytes
	}
	projected := state
	projected.ToolPruning = &payload
	prunedReport := runtime.requestReport(projected, spec)
	if prunedReport.TotalInputTokens >= report.TotalInputTokens {
		return Record{}, report, nil
	}
	if err := validateToolPruningBoundary(state, payload, state.LastSequence+1, state.hasUnfinishedWork()); err != nil {
		return Record{}, report, err
	}
	record, err := appendRecord(ctx, runtime.journal, RecordToolResultsPruned, payload)
	if err != nil {
		return Record{}, report, err
	}
	return record, prunedReport, nil
}

func validateToolPruningBoundary(state State, payload ToolResultsPrunedRecord, recordSequence uint64, unfinished bool) error {
	if payload.ThroughSequence == 0 || payload.ThroughSequence >= recordSequence || payload.HeadBytes <= 0 || payload.TailBytes <= 0 {
		return fmt.Errorf("invalid tool-result pruning at sequence %d", recordSequence)
	}
	if unfinished && !boundaryPrecedesUnfinishedWork(state, payload.ThroughSequence) {
		return fmt.Errorf("tool results pruned while work was unfinished at sequence %d", recordSequence)
	}
	boundaryIndex := -1
	for index, block := range state.Blocks {
		if block.EndSequence == payload.ThroughSequence {
			boundaryIndex = index
			break
		}
	}
	if boundaryIndex < 0 || boundaryIndex >= len(state.Blocks)-1 {
		return fmt.Errorf("tool-result pruning at sequence %d does not use an older conversation-block boundary", recordSequence)
	}
	if state.ToolPruning != nil && payload.ThroughSequence <= state.ToolPruning.ThroughSequence {
		return fmt.Errorf("tool-result pruning at sequence %d does not advance", recordSequence)
	}
	return nil
}

func pruneToolResult(content string, headBytes, tailBytes int) string {
	if headBytes <= 0 || tailBytes <= 0 || len(content) <= headBytes+tailBytes {
		return content
	}
	headEnd := min(headBytes, len(content))
	for headEnd > 0 && !utf8.ValidString(content[:headEnd]) {
		headEnd--
	}
	tailStart := max(headEnd, len(content)-tailBytes)
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	omitted := tailStart - headEnd
	if omitted <= 0 {
		return content
	}
	marker := fmt.Sprintf("\n[… %d bytes omitted from old tool result; full output remains in session journal …]\n", omitted)
	pruned := content[:headEnd] + marker + content[tailStart:]
	if len(pruned) >= len(content) {
		return content
	}
	return pruned
}
