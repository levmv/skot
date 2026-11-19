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

func (runtime *Runtime) pruneOldToolResultsOnce(ctx context.Context, state State, spec runRequestSpec, report ContextReport) (Record, error) {
	boundaries := state.contextBoundaries()
	// Preserve the same recent tail as compaction before considering savings.
	targetEnd := runtime.compactionTargetEnd(state, spec, boundaries)
	if targetEnd < 0 {
		return Record{}, nil
	}
	return runtime.pruneToolResultsThroughOnce(ctx, state, spec, report, boundaries[targetEnd].through, false)
}

func (runtime *Runtime) pruneLatestToolResultsOnce(ctx context.Context, state State, spec runRequestSpec, report ContextReport) (Record, error) {
	return runtime.pruneToolResultsThroughOnce(ctx, state, spec, report, state.latestTextToolPruningSequence(), true)
}

func (runtime *Runtime) pruneToolResultsThroughOnce(ctx context.Context, state State, spec runRequestSpec, report ContextReport, through uint64, emergency bool) (Record, error) {
	if through == 0 {
		return Record{}, nil
	}
	payload := ToolResultsPrunedRecord{
		ThroughSequence: through,
		HeadBytes:       defaultPrunedToolHeadBytes,
		TailBytes:       defaultPrunedToolTailBytes,
	}
	if state.ToolPruning != nil {
		if payload.ThroughSequence <= state.ToolPruning.ThroughSequence {
			return Record{}, nil
		}
		payload.HeadBytes = state.ToolPruning.HeadBytes
		payload.TailBytes = state.ToolPruning.TailBytes
	}
	projected := state
	projected.ToolPruning = &payload
	projected.contextSamples = [2]contextSample{}
	prunedReport := runtime.requestReport(projected, spec)
	// The decision credits only projected savings. The committed pruning
	// will discard the anchor, regardless of how much content was shortened.
	prunedReport.TotalInputTokens = report.TotalInputTokens - (report.localInputTokens() - prunedReport.localInputTokens())
	if emergency {
		// Shortening unseen output is a last resort. Commit only a reduction
		// that fits the known window, even if it exceeds the ordinary soft target.
		if report.Window == 0 || prunedReport.TotalInputTokens >= report.TotalInputTokens || prunedReport.TotalInputTokens > prunedReport.InputLimit {
			return Record{}, nil
		}
	} else if !toolResultPruningIsDecisive(report, prunedReport) {
		return Record{}, nil
	}
	if err := validateToolPruningBoundary(state, payload, state.LastSequence+1); err != nil {
		return Record{}, err
	}
	record, err := appendRecord(ctx, runtime.journal, RecordToolResultsPruned, payload)
	if err != nil {
		return Record{}, err
	}
	return record, nil
}

func (state State) latestTextToolPruningSequence() uint64 {
	_, through := state.contextBoundariesWithLatest()
	if through == 0 {
		return 0
	}
	first := state.firstVerbatimSequence()
	if state.ToolPruning != nil {
		if through <= state.ToolPruning.ThroughSequence {
			return 0
		}
		first = max(first, state.ToolPruning.ThroughSequence+1)
	}
	// The existing projection also removes images. Do not extend it over any
	// image still retained for the model, including images in mixed results.
	for _, block := range state.Blocks {
		for _, entry := range block.Entries {
			if entry.Sequence >= first && entry.Sequence <= through && entry.Item.ToolResult != nil && entry.Item.ToolResult.Content.HasImage() {
				return 0
			}
		}
	}
	return through
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
	if !found && payload.ThroughSequence != state.latestTextToolPruningSequence() {
		return fmt.Errorf("tool-result pruning at sequence %d does not follow an eligible completed exchange", recordSequence)
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
	marker = fmt.Sprintf("\n[… %d bytes omitted from tool result; some content may be unread. Re-read needed source portions; full output remains in session journal …]\n", omitted)
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
