package agent

import (
	"context"
	"fmt"

	"github.com/levmv/skot/model"
)

const inputContextClearedText = "The user cleared the attached supplementary instructions and their resource access. Continue the conversation without applying them unless the user attaches them again."

// ClearInputContext removes delivered and queued instructions and resource
// requests from future calls, preserving the conversation and journal.
func (runtime *Runtime) ClearInputContext(ctx context.Context) error {
	if !runtime.runMu.TryLock() {
		return ErrRunActive
	}
	defer runtime.runMu.Unlock()
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return err
	}
	live, err := reduceRecords(records)
	if err != nil {
		return err
	}
	if live.state.hasUnfinishedWork() {
		return unfinishedWorkError("clearing attached instructions")
	}
	if live.state.hasInputContext() {
		if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordInputContextCleared, struct{}{}); err != nil {
			return err
		}
	}
	runtime.queueMu.Lock()
	for index := range runtime.pendingInputs {
		runtime.pendingInputs[index].Instructions = nil
		runtime.pendingInputs[index].ReadOnlyPaths = nil
	}
	runtime.queueMu.Unlock()
	runtime.publishSessionStatus(live.state)
	return nil
}

func (state State) hasInputContext() bool {
	for _, block := range state.Blocks {
		if block.StartSequence > state.InputContextClearedThrough && block.RunID != "" &&
			(len(block.InputInstructions) != 0 || len(block.InputReadOnlyPaths) != 0) {
			return true
		}
	}
	return false
}

func (reducer *stateReducer) applyInputContextCleared(record Record) error {
	if _, err := record.decode[struct{}](); err != nil {
		return err
	}
	state := &reducer.state
	if state.hasUnfinishedWork() {
		return fmt.Errorf("input context cleared while work was unfinished at sequence %d", record.Sequence)
	}
	state.InputContextClearedThrough = record.Sequence - 1
	display, _ := NewDetail(inputTextDetailKind, "Attached instructions cleared.")
	item := model.Item{Kind: model.ItemUserText, Text: inputContextClearedText, Details: []model.Detail{display}}
	state.Items = append(state.Items, item)
	state.Blocks = append(state.Blocks, ConversationBlock{
		StartSequence: record.Sequence, EndSequence: record.Sequence, Status: RunCompleted,
		Entries: []ConversationEntry{{Sequence: record.Sequence, Time: record.Time, Item: item}},
		// Retain this small notice through compaction too: an older summary may
		// still mention the removed instructions. A later clear replaces it.
		InputInstructions: []Instruction{{Text: inputContextClearedText}},
	})
	return nil
}
