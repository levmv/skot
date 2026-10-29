package agent

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/levmv/skot/model"
)

const (
	defaultPrunedToolHeadBytes = 2 * 1024
	defaultPrunedToolTailBytes = 2 * 1024
)

func (runtime *Runtime) pruneOldToolResultsOnce(ctx context.Context, state State, spec runRequestSpec, report ContextReport) (Record, ContextReport, error) {
	boundaries := state.contextBoundaries()
	// Preserve the same recent tail as compaction before considering savings.
	targetEnd := runtime.compactionTargetEnd(state, spec, boundaries)
	if targetEnd < 0 {
		return Record{}, report, nil
	}
	payload := ToolResultsPrunedRecord{
		ThroughSequence: boundaries[targetEnd].through,
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
	if !toolResultPruningIsDecisive(report, prunedReport) {
		return Record{}, report, nil
	}
	if err := validateToolPruningBoundary(state, payload, state.LastSequence+1); err != nil {
		return Record{}, report, err
	}
	record, err := appendRecord(ctx, runtime.journal, RecordToolResultsPruned, payload)
	if err != nil {
		return Record{}, report, err
	}
	return record, prunedReport, nil
}

// Pruning changes the cached prefix just like compaction. Select it only when
// tool output dominates enough to leave a compact-sized projection. Under
// known token pressure it must also make the next request fit; a provider byte
// rejection can only be confirmed by retrying the reduced request.
func toolResultPruningIsDecisive(before, after ContextReport) bool {
	savedHistory := before.HistoryTokens - after.HistoryTokens
	if savedHistory <= 0 || savedHistory < after.HistoryTokens {
		return false
	}
	compactHistory := compactionVerbatimTokenBudget(before) + compactionCheckpointTokenBudget
	if after.HistoryTokens > compactHistory {
		return false
	}
	return before.Window == 0 || before.TotalInputTokens <= before.InputLimit || after.TotalInputTokens <= after.InputLimit
}

func validateToolPruningBoundary(state State, payload ToolResultsPrunedRecord, recordSequence uint64) error {
	if payload.ThroughSequence == 0 || payload.ThroughSequence >= recordSequence || payload.HeadBytes <= 0 || payload.TailBytes <= 0 {
		return fmt.Errorf("invalid tool-result pruning at sequence %d", recordSequence)
	}
	found := false
	for _, boundary := range state.contextBoundaries() {
		if boundary.through == payload.ThroughSequence {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("tool-result pruning at sequence %d does not follow a completed older exchange", recordSequence)
	}
	if state.ToolPruning != nil && payload.ThroughSequence <= state.ToolPruning.ThroughSequence {
		return fmt.Errorf("tool-result pruning at sequence %d does not advance", recordSequence)
	}
	return nil
}

func pruneToolResult(content model.Content, headBytes, tailBytes int) model.Content {
	text := content.Text()
	headEnd, tailStart, omissionMarker, textPruned := toolResultTextPruning(text, headBytes, tailBytes)
	if !textPruned && !content.HasImage() {
		return content
	}
	projected := make(model.Content, 0, len(content)+1)
	// Treat text parts as one pruning budget, then map the retained head and
	// tail back onto their original positions so image markers remain ordered.
	textOffset := 0
	omissionWritten := false
	for _, part := range content {
		switch part.Kind {
		case model.ContentPartText:
			if !textPruned {
				projected = append(projected, part)
				continue
			}
			partStart := textOffset
			partEnd := partStart + len(part.Text)
			if partStart < headEnd {
				keep := min(partEnd, headEnd) - partStart
				projected = appendTextContentPart(projected, part.Text[:keep])
			}
			omittedStart := max(partStart, headEnd)
			omittedEnd := min(partEnd, tailStart)
			if omittedStart < omittedEnd && !omissionWritten {
				projected = appendTextContentPart(projected, omissionMarker)
				omissionWritten = true
			}
			if partEnd > tailStart {
				keep := max(partStart, tailStart) - partStart
				projected = appendTextContentPart(projected, part.Text[keep:])
			}
			textOffset = partEnd
		case model.ContentPartImage:
			if part.Image == nil {
				continue
			}
			projected = append(projected, model.ContentPart{Kind: model.ContentPartText, Text: fmt.Sprintf(
				"\n[image pruned: %s, %dx%d; full image remains in session journal]\n",
				part.Image.MediaType, part.Image.Width, part.Image.Height,
			)})
		}
	}
	// Pruning leaves only text. Coalesce parts so splitting a retained text
	// segment around an omission cannot exceed Content's part limit.
	return model.TextContent(projected.Text())
}

func toolResultTextPruning(content string, headBytes, tailBytes int) (headEnd, tailStart int, marker string, ok bool) {
	if headBytes <= 0 || tailBytes <= 0 || len(content) <= headBytes+tailBytes {
		return 0, 0, "", false
	}
	headEnd = min(headBytes, len(content))
	for headEnd > 0 && !utf8.ValidString(content[:headEnd]) {
		headEnd--
	}
	tailStart = max(headEnd, len(content)-tailBytes)
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	omitted := tailStart - headEnd
	if omitted <= 0 {
		return 0, 0, "", false
	}
	marker = fmt.Sprintf("\n[… %d bytes omitted from old tool result; full output remains in session journal …]\n", omitted)
	if headEnd+len(marker)+len(content)-tailStart >= len(content) {
		return 0, 0, "", false
	}
	return headEnd, tailStart, marker, true
}

func appendTextContentPart(content model.Content, text string) model.Content {
	if text == "" {
		return content
	}
	return append(content, model.ContentPart{Kind: model.ContentPartText, Text: text})
}
