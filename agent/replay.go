package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type PendingTool struct {
	RunID string
	Call  ToolCall
}

type ConversationBlock struct {
	RunID            string
	StartSequence    uint64
	EndSequence      uint64
	Status           RunStatus
	Error            string
	ToolLimitReached bool
	DetachedJobs     []string
	Entries          []ConversationEntry
}

type ConversationEntry struct {
	Sequence uint64
	Time     time.Time
	Item     Item
}

type State struct {
	SchemaVersion    int
	SessionID        string
	Workspace        string
	Selection        ModelSelectedRecord
	Configured       *EffectiveConfigSnapshot
	Items            []Item
	Blocks           []ConversationBlock
	Compaction       *ContextCompactedRecord
	CompactionCount  int
	ToolPruning      *ToolResultsPrunedRecord
	ToolPruningCount int
	Usage            ModelUsage
	ActiveRuns       []string
	PendingTools     []PendingTool
	DeliveredJobs    map[string]struct{}
	DetachedJobs     []string
	LastSequence     uint64
	// lastRequiredSequence advances only for records which participate in the
	// semantic session projection. Auxiliary diagnostics leave it unchanged.
	lastRequiredSequence uint64
}

func (state State) hasUnfinishedWork() bool {
	return len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0
}

// firstUnfinishedBlock returns the oldest block owned by an active run. Model
// boundary maintenance may rewrite the projection of older completed blocks,
// but it must never summarize or prune any part of the run in progress.
func (state State) firstUnfinishedBlock() (int, bool) {
	if len(state.ActiveRuns) == 0 {
		return 0, false
	}
	active := make(map[string]struct{}, len(state.ActiveRuns))
	for _, runID := range state.ActiveRuns {
		active[runID] = struct{}{}
	}
	for index, block := range state.Blocks {
		if _, unfinished := active[block.RunID]; unfinished {
			return index, true
		}
	}
	return 0, false
}

func boundaryPrecedesUnfinishedWork(state State, throughSequence uint64) bool {
	if !state.hasUnfinishedWork() {
		return true
	}
	first, ok := state.firstUnfinishedBlock()
	if !ok {
		// An active run without an input block is not a boundary at which context
		// maintenance can prove that it leaves unfinished work untouched.
		return false
	}
	return throughSequence < state.Blocks[first].StartSequence
}

func (state State) VerbatimItems() []Item {
	return state.verbatimItemsFromSequence(state.firstVerbatimSequence(), true)
}

// verbatimModelItems returns an owned projection source without product-only
// details. Model requests and context estimates discard those details, so
// copying potentially large JSON payloads here would be pure overhead.
func (state State) verbatimModelItems() []Item {
	return state.verbatimModelItemsFromSequence(state.firstVerbatimSequence())
}

func (state State) firstVerbatimSequence() uint64 {
	if state.Compaction != nil {
		return state.Compaction.FirstVerbatimSequence
	}
	return 0
}

func (state State) verbatimModelItemsFromSequence(firstSequence uint64) []Item {
	return state.verbatimItemsFromSequence(firstSequence, false)
}

func (state State) verbatimItemsFromSequence(firstSequence uint64, includeDetails bool) []Item {
	var items []Item
	for _, block := range state.Blocks {
		if firstSequence != 0 && block.StartSequence < firstSequence {
			continue
		}
		for _, entry := range block.Entries {
			item := cloneItemForProjection(entry.Item, includeDetails)
			if item.Kind == ItemToolResult && item.ToolResult != nil && state.ToolPruning != nil && entry.Sequence <= state.ToolPruning.ThroughSequence {
				item.ToolResult.Content = pruneToolResult(item.ToolResult.Content, state.ToolPruning.HeadBytes, state.ToolPruning.TailBytes)
			}
			items = append(items, item)
		}
	}
	return items
}

func validateCompactionBoundary(state State, payload ContextCompactedRecord, recordSequence uint64, unfinished bool) error {
	if strings.TrimSpace(payload.Summary) == "" || payload.CoveredThroughSequence == 0 || payload.FirstVerbatimSequence == 0 || payload.CoveredThroughSequence >= payload.FirstVerbatimSequence || payload.FirstVerbatimSequence >= recordSequence {
		return fmt.Errorf("invalid context compaction at sequence %d", recordSequence)
	}
	if unfinished && !boundaryPrecedesUnfinishedWork(state, payload.CoveredThroughSequence) {
		return fmt.Errorf("context compacted while work was unfinished at sequence %d", recordSequence)
	}
	firstIndex := -1
	for index, block := range state.Blocks {
		if block.StartSequence == payload.FirstVerbatimSequence {
			firstIndex = index
			break
		}
	}
	if firstIndex <= 0 || state.Blocks[firstIndex-1].EndSequence != payload.CoveredThroughSequence {
		return fmt.Errorf("context compaction at sequence %d does not use a conversation-block boundary", recordSequence)
	}
	if state.Compaction != nil {
		if payload.FirstVerbatimSequence <= state.Compaction.FirstVerbatimSequence || payload.CoveredThroughSequence <= state.Compaction.CoveredThroughSequence {
			return fmt.Errorf("context compaction at sequence %d does not advance", recordSequence)
		}
	}
	return nil
}

func validateProviderOwnership(item Item, selection ModelSelectedRecord) error {
	if item.Kind == ItemReasoning {
		if item.ProviderContext == nil || item.ProviderContext.Backend != selection.Backend || item.ProviderContext.Epoch != selection.Epoch {
			return fmt.Errorf("reasoning item does not belong to active provider epoch")
		}
	}
	if item.ToolCall != nil {
		for _, reference := range item.ToolCall.ProviderReferences {
			if reference.Backend != selection.Backend || reference.Epoch != selection.Epoch {
				return fmt.Errorf("tool call provider reference does not belong to active provider epoch")
			}
		}
	}
	return nil
}

// Reconcile validates a journal, marks unfinished work as interrupted, and
// returns the resulting state together with the records used to derive it.
func Reconcile(ctx context.Context, journal Journal) (State, []Record, error) {
	records, err := journal.Records(ctx)
	if err != nil {
		return State{}, nil, fmt.Errorf("read journal for reconciliation: %w", err)
	}
	reducer, err := reduceRecords(records)
	if err != nil {
		return State{}, nil, err
	}
	for len(reducer.state.PendingTools) != 0 {
		pending := reducer.state.PendingTools[0]
		result := ToolResult{
			CallID:  pending.Call.ID,
			Content: fmt.Sprintf("tool %s outcome is unknown after an interrupted session; the call was not replayed", pending.Call.Name),
			Error:   true,
			Unknown: true,
		}
		record, err := appendRecordAndApply(ctx, journal, reducer, RecordToolResult, ToolResultRecord{RunID: pending.RunID, Result: result})
		if err != nil {
			return State{}, nil, err
		}
		records = append(records, record)
	}
	for len(reducer.state.ActiveRuns) != 0 {
		runID := reducer.state.ActiveRuns[0]
		record, err := appendRecordAndApply(ctx, journal, reducer, RecordRunFinished, RunFinishedRecord{RunID: runID, Status: RunInterrupted})
		if err != nil {
			return State{}, nil, err
		}
		records = append(records, record)
	}
	return reducer.state, records, nil
}

func unfinishedWorkError(action string) error {
	return fmt.Errorf("session has unfinished work; restart Skot to reconcile it before %s, or use /clear to discard the session", action)
}
