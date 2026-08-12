package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxCompactionInputBytes         = 256 * 1024
	maxCompactionSummaryBytes       = 128 * 1024
	maxCompactionTextBytes          = 32 * 1024
	maxCompactionToolResultBytes    = 8 * 1024
	maxCompactionToolArgumentsBytes = 4 * 1024
)

const compactionSystemInstructions = `Summarize an older segment of an agent session for reliable continuation. Preserve the objective and acceptance criteria, user constraints, decisions and reasons, modified files and current state, commands and tests with their results, errors and abandoned approaches, outstanding work and the next step, and explicit uncertainty. Be compact but concrete. Do not claim work was done unless the transcript says it was.`

var errNoCompactionBoundary = errors.New("no completed older conversation block is available to compact")

// compactionPlan is tied to an exact journal state. Input contains the prior
// rolling summary plus every newly covered complete conversation block.
type compactionPlan struct {
	SessionID              string
	BaseSequence           uint64
	CoveredThroughSequence uint64
	FirstVerbatimSequence  uint64
	Input                  string
}

// Compact generates and commits one rolling summary. It uses the same model
// backend but no tools, and it never records provisional summarizer output.
func (runtime *Runtime) Compact(ctx context.Context, keepVerbatimBlocks int) (ContextCompactedRecord, error) {
	if !runtime.runMu.TryLock() {
		return ContextCompactedRecord{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return ContextCompactedRecord{}, fmt.Errorf("read journal for compaction: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return ContextCompactedRecord{}, err
	}
	if live.state.hasUnfinishedWork() {
		return ContextCompactedRecord{}, errors.New("cannot compact unfinished work")
	}
	if err := runtime.prepareSession(ctx, live); err != nil {
		return ContextCompactedRecord{}, err
	}
	compaction, _, err := runtime.compactLocked(ctx, live.state, keepVerbatimBlocks, nil)
	return compaction, err
}

func (runtime *Runtime) compactLocked(ctx context.Context, state State, keepVerbatimBlocks int, emit EmitFunc) (ContextCompactedRecord, Record, error) {
	if state.Configured == nil {
		return ContextCompactedRecord{}, Record{}, errors.New("session has no effective configuration")
	}
	plan, err := planCompaction(state, keepVerbatimBlocks)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	response, err := runtime.completeRequest(ctx, "", ModelRequest{
		SessionID:     state.SessionID,
		ProviderEpoch: state.Selection.Epoch,
		Instructions:  state.Configured.ModelContext.CompactionInstructions,
		Items:         []Item{{Kind: ItemUserText, Text: plan.Input}},
	}, emit)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("summarize context: %w", sanitizeError(err, runtime.sanitize))
	}
	response = runtime.sanitizeModelResponse(response)
	if isIncompleteStopReason(response.StopReason) {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction model returned an incomplete summary: stop reason %s", response.StopReason)
	}
	for _, item := range response.Items {
		if item.Kind != ItemAssistantText && item.Kind != ItemReasoning {
			return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction model returned unsupported item kind %q", item.Kind)
		}
	}
	summary := responseText(response.Items)
	if strings.TrimSpace(summary) == "" {
		return ContextCompactedRecord{}, Record{}, errors.New("compaction model returned an empty summary")
	}
	return commitCompaction(ctx, runtime.journal, plan, summary, response.Usage)
}

// planCompaction selects the oldest complete blocks while retaining at least
// keepVerbatimBlocks recent blocks verbatim. It never splits a block merely to
// make the summarizer input fit.
func planCompaction(state State, keepVerbatimBlocks int) (compactionPlan, error) {
	if state.SessionID == "" {
		return compactionPlan{}, errors.New("session is not initialized")
	}
	if len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0 {
		return compactionPlan{}, errors.New("cannot compact unfinished work")
	}
	if keepVerbatimBlocks < 1 {
		return compactionPlan{}, errors.New("at least one verbatim block is required")
	}
	activeStart := 0
	var input strings.Builder
	if state.Compaction != nil {
		activeStart = blockIndexAtSequence(state.Blocks, state.Compaction.FirstVerbatimSequence)
		if activeStart < 0 {
			return compactionPlan{}, errors.New("current compaction boundary is missing from replayed blocks")
		}
		input.WriteString("Previous rolling summary:\n")
		input.WriteString(state.Compaction.Summary)
		input.WriteString("\n\nNewly covered conversation blocks:\n")
	} else {
		input.WriteString("Conversation blocks to summarize:\n")
	}
	targetEnd := len(state.Blocks) - keepVerbatimBlocks
	if targetEnd <= activeStart {
		return compactionPlan{}, errNoCompactionBoundary
	}
	compactedEnd := activeStart
	for index := activeStart; index < targetEnd; index++ {
		blockText := formatCompactionBlock(state.Blocks[index], state.ToolPruning)
		if input.Len()+len(blockText) > maxCompactionInputBytes {
			break
		}
		input.WriteString(blockText)
		compactedEnd = index + 1
	}
	if compactedEnd == activeStart {
		return compactionPlan{}, errors.New("no complete conversation block fits in the bounded compaction input")
	}
	return compactionPlan{
		SessionID:              state.SessionID,
		BaseSequence:           state.LastSequence,
		CoveredThroughSequence: state.Blocks[compactedEnd-1].EndSequence,
		FirstVerbatimSequence:  state.Blocks[compactedEnd].StartSequence,
		Input:                  input.String(),
	}, nil
}

// commitCompaction appends a rolling summary only if the journal still has the
// exact state for which the plan was constructed.
func commitCompaction(ctx context.Context, journal Journal, plan compactionPlan, summary string, usage ModelUsage) (ContextCompactedRecord, Record, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ContextCompactedRecord{}, Record{}, errors.New("compaction summary is empty")
	}
	if len(summary) > maxCompactionSummaryBytes {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("compaction summary exceeds %d bytes", maxCompactionSummaryBytes)
	}
	records, err := journal.Records(ctx)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, fmt.Errorf("read journal before compaction: %w", err)
	}
	state, err := Replay(records)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	if state.SessionID != plan.SessionID || state.LastSequence != plan.BaseSequence {
		return ContextCompactedRecord{}, Record{}, errors.New("session changed while compaction summary was being prepared")
	}
	payload := ContextCompactedRecord{
		CoveredThroughSequence: plan.CoveredThroughSequence,
		FirstVerbatimSequence:  plan.FirstVerbatimSequence,
		Summary:                summary,
		Usage:                  usage,
	}
	if err := validateCompactionBoundary(state, payload, state.LastSequence+1, len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0); err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	record, err := appendRecord(ctx, journal, RecordContextCompacted, payload)
	if err != nil {
		return ContextCompactedRecord{}, Record{}, err
	}
	return payload, record, nil
}

func blockIndexAtSequence(blocks []ConversationBlock, sequence uint64) int {
	for index, block := range blocks {
		if block.StartSequence == sequence {
			return index
		}
	}
	return -1
}

func formatCompactionBlock(block ConversationBlock, pruning *ToolResultsPrunedRecord) string {
	var output strings.Builder
	fmt.Fprintf(&output, "\n[conversation block seq=%d..%d]\n", block.StartSequence, block.EndSequence)
	for _, entry := range block.Entries {
		item := entry.Item
		switch item.Kind {
		case ItemUserText:
			output.WriteString("[user] ")
			output.WriteString(truncateCompactionText(item.Text, maxCompactionTextBytes))
			output.WriteByte('\n')
		case ItemBoundaryText:
			output.WriteString("[boundary] ")
			output.WriteString(truncateCompactionText(item.Text, maxCompactionTextBytes))
			output.WriteByte('\n')
		case ItemAssistantText:
			output.WriteString("[assistant] ")
			output.WriteString(truncateCompactionText(item.Text, maxCompactionTextBytes))
			output.WriteByte('\n')
		case ItemReasoning:
			// Provider-owned reasoning is neither semantic history nor safe
			// summarizer input. It remains available in the raw journal.
		case ItemToolCall:
			if item.ToolCall == nil {
				continue
			}
			fmt.Fprintf(&output, "[tool_call] %s", item.ToolCall.Name)
			if arguments := strings.TrimSpace(item.ToolCall.RawArguments); arguments != "" {
				output.WriteString(" args=")
				output.WriteString(truncateCompactionText(arguments, maxCompactionToolArgumentsBytes))
			}
			output.WriteByte('\n')
		case ItemToolResult:
			if item.ToolResult == nil {
				continue
			}
			fmt.Fprintf(&output, "[tool_result call=%s error=%t unknown=%t] ", item.ToolResult.CallID, item.ToolResult.Error, item.ToolResult.Unknown)
			content := item.ToolResult.Content
			if pruning != nil && entry.Sequence <= pruning.ThroughSequence {
				content = pruneToolResult(content, pruning.HeadBytes, pruning.TailBytes)
				output.WriteString(content)
			} else {
				output.WriteString(truncateCompactionText(content, maxCompactionToolResultBytes))
			}
			output.WriteByte('\n')
		}
	}
	if block.Status != "" {
		fmt.Fprintf(&output, "[run_finished status=%s]", block.Status)
		if block.Error != "" {
			output.WriteByte(' ')
			output.WriteString(truncateCompactionText(block.Error, maxCompactionToolResultBytes))
		}
		output.WriteByte('\n')
	}
	return output.String()
}

func truncateCompactionText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "\n[… truncated for compaction input …]"
}
