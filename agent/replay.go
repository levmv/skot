package agent

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/levmv/skot/model"
)

type PendingTool struct {
	RunID string
	Call  model.ToolCall
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
	// InputInstructions survive compaction at their original user authority.
	InputInstructions  []Instruction
	InputReadOnlyPaths []string
}

type ConversationEntry struct {
	Sequence uint64
	Time     time.Time
	Item     model.Item
}

type State struct {
	SchemaVersion    int
	SessionID        string
	Workspace        string
	Selection        model.ReplayContext
	Configured       *EffectiveConfigSnapshot
	Items            []model.Item
	Blocks           []ConversationBlock
	Compaction       *ContextCompactedRecord
	CompactionCount  int
	ToolPruning      *ToolResultsPrunedRecord
	ToolPruningCount int
	ImageDelivery    ImageDeliveryObservedRecord
	// Usage sums accepted model responses and committed compactions.
	// Use Runtime.Usage or ReplayUsage for accounting across every attempt.
	Usage         model.TokenCounts
	ActiveRuns    []string
	PendingTools  []PendingTool
	DeliveredJobs map[string]struct{}
	DetachedJobs  []string
	LastSequence  uint64
	// InputContextClearedThrough affects model projection and resource requests,
	// never the historical transcript in Items or Blocks.
	InputContextClearedThrough uint64
	// lastRequiredSequence advances only for records which participate in the
	// semantic session projection. Auxiliary diagnostics leave it unchanged.
	lastRequiredSequence uint64
}

func (state State) hasUnfinishedWork() bool {
	return len(state.ActiveRuns) != 0 || len(state.PendingTools) != 0
}

func (state State) VerbatimItems() []model.Item {
	return state.verbatimItemsFromSequence(state.firstVerbatimSequence(), true)
}

// modelItems returns retained user instructions followed by the verbatim tail,
// without product-only details. Requests and context estimates share this source.
func (state State) modelItems() []model.Item {
	var inputs []uint64
	if state.Compaction != nil {
		inputs = state.Compaction.RetainedInputSequences
	}
	return state.modelItemsFromSequence(state.firstVerbatimSequence(), inputs)
}

func (state State) firstVerbatimSequence() uint64 {
	if state.Compaction != nil {
		return state.Compaction.FirstVerbatimSequence
	}
	return 0
}

func (state State) modelItemsFromSequence(firstSequence uint64, retainedInputs []uint64) []model.Item {
	// Retain the latest version of each source unless it is already in the tail.
	// Compaction requests restrict Blocks to the prefix being summarized.
	var items []model.Item
	seen := make(map[string]bool)
	for _, block := range slices.Backward(state.Blocks) {
		if block.StartSequence <= state.InputContextClearedThrough {
			continue
		}
		var retained []Instruction
		for _, instruction := range slices.Backward(block.InputInstructions) {
			if instruction.Text == "" {
				continue
			}
			if instruction.Key != "" {
				if seen[instruction.Key] {
					continue
				}
				seen[instruction.Key] = true
			}
			if block.StartSequence < firstSequence {
				retained = append(retained, instruction)
			}
		}
		if len(retained) != 0 {
			slices.Reverse(retained)
			text := (Input{Instructions: retained}).instructionsText()
			items = append(items, model.Item{Kind: model.ItemUserText, Text: text})
		}
	}
	slices.Reverse(items)
	for _, block := range state.Blocks {
		if slices.Contains(retainedInputs, block.StartSequence) {
			items = append(items, model.Item{Kind: model.ItemUserText, Text: InputText(block.Entries[0].Item)})
		}
	}
	return append(items, state.verbatimItemsFromSequence(firstSequence, false)...)
}

func (state State) verbatimItemsFromSequence(firstSequence uint64, includeDetails bool) []model.Item {
	var items []model.Item
	for _, block := range state.Blocks {
		if firstSequence != 0 && block.EndSequence < firstSequence {
			continue
		}
		for _, entry := range block.Entries {
			if entry.Sequence < firstSequence {
				continue
			}
			item := cloneItemForProjection(entry.Item, includeDetails)
			if !includeDetails && block.StartSequence <= state.InputContextClearedThrough &&
				entry.Sequence == block.StartSequence && len(block.InputInstructions) != 0 {
				item.Text = InputText(entry.Item)
			}
			if item.Kind == model.ItemToolResult && item.ToolResult != nil && state.ToolPruning != nil && entry.Sequence <= state.ToolPruning.ThroughSequence {
				item.ToolResult.Content = pruneToolResult(item.ToolResult.Content, state.ToolPruning.HeadBytes, state.ToolPruning.TailBytes)
			}
			items = append(items, item)
		}
	}
	return items
}

func validateProviderOwnership(item model.Item, selection model.ReplayContext) error {
	if item.Kind == model.ItemReasoning {
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
		result := model.ToolResult{
			CallID:  pending.Call.ID,
			Content: model.TextContent(fmt.Sprintf("tool %s outcome is unknown after an interrupted session; the call was not replayed", pending.Call.Name)),
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
