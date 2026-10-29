package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/levmv/skot/model"
)

type contextBoundary struct {
	through uint64
	first   uint64
}

// contextBoundaries cuts only after a complete model response and all its tool
// results. User input and host events belong to the following exchange until a
// model has seen them. Items from one response share a sequence and never split.
// The last exchange always remains verbatim, even when it exceeds the soft tail
// budget. Runs and conversation blocks remain execution/transcript structure.
func (state State) contextBoundaries() []contextBoundary {
	var boundaries []contextBoundary
	pending := make(map[string]struct{})
	var through uint64
	ready := false
	unanswered := false
	for _, block := range state.Blocks {
		for start := 0; start < len(block.Entries); {
			sequence := block.Entries[start].Sequence
			end := start + 1
			for end < len(block.Entries) && block.Entries[end].Sequence == sequence {
				end++
			}
			if ready && through < sequence {
				boundaries = append(boundaries, contextBoundary{through: through, first: sequence})
			}
			for _, entry := range block.Entries[start:end] {
				switch item := entry.Item; item.Kind {
				case model.ItemUserText, model.ItemBoundaryText:
					unanswered = true
				case model.ItemAssistantText, model.ItemReasoning:
					unanswered = false
				case model.ItemToolCall:
					unanswered = false
					pending[item.ToolCall.ID] = struct{}{}
				case model.ItemToolResult:
					delete(pending, item.ToolResult.CallID)
				}
			}
			ready = !unanswered && len(pending) == 0
			through = sequence
			start = end
		}
		// Include durable completion metadata at a whole-block boundary.
		through = max(through, block.EndSequence)
		// Ending a run also closes input blocks abandoned before a response
		// (for example after cancellation or a failed context preflight).
		if !slices.Contains(state.ActiveRuns, block.RunID) && len(pending) == 0 {
			ready = true
		}
	}
	return boundaries
}

// Retain the authored inputs of the run owning the first remaining exchange.
// This includes steering delivered in earlier blocks of that run. References
// are checkpointed explicitly so replay does not change when the run finishes.
// Attached instructions have their own latest-version retention policy.
func (state State) inputsBeforeBoundary(first uint64) []uint64 {
	var runID string
	for _, block := range state.Blocks {
		if block.StartSequence <= first && block.EndSequence >= first {
			runID = block.RunID
			break
		}
	}
	// Notices such as a context clear have no run and are not authored inputs.
	if runID == "" {
		return nil
	}
	var inputs []uint64
	for _, block := range state.Blocks {
		if block.StartSequence >= first {
			break
		}
		if block.RunID == runID {
			inputs = append(inputs, block.StartSequence)
		}
	}
	return inputs
}

// historyBefore is a shallow projection; it never edits journaled blocks.
func (state State) historyBefore(first uint64) State {
	prefix := state
	prefix.Blocks = nil
	for _, block := range state.Blocks {
		if block.StartSequence >= first {
			break
		}
		end := 0
		for end < len(block.Entries) && block.Entries[end].Sequence < first {
			end++
		}
		if end < len(block.Entries) {
			block.Entries = block.Entries[:end]
			block.EndSequence = block.Entries[end-1].Sequence
			// Completion facts belong to the end of the block, outside this prefix.
			block.Status, block.Error, block.ToolLimitReached, block.DetachedJobs = "", "", false, nil
		}
		prefix.Blocks = append(prefix.Blocks, block)
	}
	return prefix
}

func validateContextCheckpoint(state State, payload ContextCompactedRecord, recordSequence uint64) error {
	if strings.TrimSpace(payload.Summary) == "" || payload.CoveredThroughSequence == 0 ||
		payload.CoveredThroughSequence >= payload.FirstVerbatimSequence || payload.FirstVerbatimSequence >= recordSequence {
		return fmt.Errorf("invalid context checkpoint at sequence %d", recordSequence)
	}
	boundary := contextBoundary{through: payload.CoveredThroughSequence, first: payload.FirstVerbatimSequence}
	if !slices.Contains(state.contextBoundaries(), boundary) {
		return fmt.Errorf("context checkpoint at sequence %d does not follow a completed exchange", recordSequence)
	}
	// An omitted list keeps no inputs verbatim outside the tail. Explicit
	// retention must include every earlier input of the run owning the tail.
	if len(payload.RetainedInputSequences) != 0 && !slices.Equal(payload.RetainedInputSequences, state.inputsBeforeBoundary(boundary.first)) {
		return fmt.Errorf("context checkpoint at sequence %d does not preserve the retained task's inputs", recordSequence)
	}
	if state.Compaction != nil && (boundary.first <= state.Compaction.FirstVerbatimSequence || boundary.through <= state.Compaction.CoveredThroughSequence) {
		return fmt.Errorf("context checkpoint at sequence %d does not advance", recordSequence)
	}
	return nil
}
