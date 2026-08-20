package agent

import (
	"fmt"
	"strings"
)

// stateReducer is the single journal-record fold used by both Replay and a
// live run. Keeping its indexes private prevents callers from constructing
// state which could not have come from accepted records.
type stateReducer struct {
	state      State
	active     map[string]struct{}
	blockByRun map[string]int
}

// Replay derives the complete journaled session state using the same transition
// function used to advance a live run after each successful journal append.
func Replay(records []Record) (State, error) {
	reducer, err := reduceRecords(records)
	if err != nil {
		return State{}, err
	}
	return reducer.state, nil
}

func reduceRecords(records []Record) (*stateReducer, error) {
	reducer := &stateReducer{
		state:      State{DeliveredJobs: make(map[string]struct{})},
		active:     make(map[string]struct{}),
		blockByRun: make(map[string]int),
	}
	for _, record := range records {
		if err := reducer.apply(record); err != nil {
			return nil, err
		}
	}
	return reducer, nil
}

func reducerFromState(state State) *stateReducer {
	if state.DeliveredJobs == nil {
		state.DeliveredJobs = make(map[string]struct{})
	}
	reducer := &stateReducer{
		state:      state,
		active:     make(map[string]struct{}, len(state.ActiveRuns)),
		blockByRun: make(map[string]int, len(state.ActiveRuns)),
	}
	for _, runID := range state.ActiveRuns {
		reducer.active[runID] = struct{}{}
	}
	for index, block := range state.Blocks {
		if _, active := reducer.active[block.RunID]; active {
			reducer.blockByRun[block.RunID] = index
		}
	}
	return reducer
}

func (reducer *stateReducer) apply(record Record) error {
	if record.Sequence == 0 || record.Sequence <= reducer.state.LastSequence {
		return fmt.Errorf("record sequence %d is not strictly increasing", record.Sequence)
	}
	if reducer.state.LastSequence == 0 && record.Kind != RecordSessionStarted {
		return fmt.Errorf("first journal record must be %q, got %q at sequence %d", RecordSessionStarted, record.Kind, record.Sequence)
	}
	if err := reducer.applyRecord(record); err != nil {
		return err
	}
	reducer.state.LastSequence = record.Sequence
	if !isAuxiliaryRecordKind(record.Kind) {
		reducer.state.lastRequiredSequence = record.Sequence
	}
	return nil
}

func (reducer *stateReducer) applyRecord(record Record) error {
	switch record.Kind {
	case RecordSessionStarted:
		return reducer.applySessionStarted(record)
	case RecordModelSelected:
		return reducer.applyModelSelected(record)
	case RecordSessionConfigured:
		return reducer.applySessionConfigured(record)
	case RecordRunStarted:
		return reducer.applyRunStarted(record)
	case RecordRunInputAdded:
		return reducer.applyRunInputAdded(record)
	case RecordModelResponse:
		return reducer.applyModelResponse(record)
	case RecordToolResult:
		return reducer.applyToolResult(record)
	case RecordBoundaryEvent:
		return reducer.applyBoundaryEvent(record)
	case RecordRunFinished:
		return reducer.applyRunFinished(record)
	case RecordContextCompacted:
		return reducer.applyContextCompacted(record)
	case RecordToolResultsPruned:
		return reducer.applyToolResultsPruned(record)
	default:
		if isAuxiliaryRecordKind(record.Kind) {
			// Auxiliary records are semantic leaves: ignoring one may change only
			// LastSequence, and no required record may reference or depend on it.
			return nil
		}
		return fmt.Errorf("unknown record kind %q at sequence %d", record.Kind, record.Sequence)
	}
}

func isAuxiliaryRecordKind(kind RecordKind) bool {
	return strings.HasPrefix(string(kind), auxiliaryRecordKindPrefix)
}

func (reducer *stateReducer) applySessionStarted(record Record) error {
	payload, err := decodeRecord[SessionStartedRecord](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	if record.Sequence != 1 || payload.SessionID == "" || state.SessionID != "" {
		return fmt.Errorf("invalid session start at sequence %d", record.Sequence)
	}
	if payload.SchemaVersion != JournalSchemaVersion {
		return fmt.Errorf("unsupported journal schema version %d at sequence %d; supported version is %d", payload.SchemaVersion, record.Sequence, JournalSchemaVersion)
	}
	state.SchemaVersion = payload.SchemaVersion
	state.SessionID = payload.SessionID
	state.Workspace = payload.Workspace
	return nil
}

func (reducer *stateReducer) applyModelSelected(record Record) error {
	payload, err := decodeRecord[ModelSelectedRecord](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	if state.SessionID == "" || strings.TrimSpace(payload.Backend) == "" || strings.TrimSpace(payload.Provider) == "" ||
		strings.TrimSpace(payload.Model) == "" || strings.TrimSpace(payload.Epoch) == "" {
		return fmt.Errorf("invalid model selection at sequence %d", record.Sequence)
	}
	if state.hasUnfinishedWork() {
		return fmt.Errorf("model changed while a run was active at sequence %d", record.Sequence)
	}
	if payload.Epoch == state.Selection.Epoch {
		return fmt.Errorf("model selection reused epoch %q at sequence %d", payload.Epoch, record.Sequence)
	}
	state.Selection = payload
	return nil
}

func (reducer *stateReducer) applySessionConfigured(record Record) error {
	payload, err := decodeRecord[EffectiveConfigSnapshot](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	if state.SessionID == "" || state.Selection.Epoch == "" {
		return fmt.Errorf("session configured before model selection at sequence %d", record.Sequence)
	}
	if err := validateEffectiveConfigSnapshot(payload); err != nil {
		return fmt.Errorf("invalid session configuration at sequence %d: %w", record.Sequence, err)
	}
	configured := cloneEffectiveConfigSnapshot(payload)
	state.Configured = &configured
	return nil
}

func (reducer *stateReducer) applyRunStarted(record Record) error {
	payload, err := decodeRecord[RunStartedRecord](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	_, alreadyActive := reducer.active[payload.RunID]
	if state.SessionID == "" || state.Selection.Epoch == "" || payload.RunID == "" || alreadyActive {
		return fmt.Errorf("invalid started run %q at sequence %d", payload.RunID, record.Sequence)
	}
	reducer.active[payload.RunID] = struct{}{}
	state.ActiveRuns = append(state.ActiveRuns, payload.RunID)
	return nil
}

func (reducer *stateReducer) applyRunInputAdded(record Record) error {
	payload, err := decodeRecord[RunInputAddedRecord](record)
	if err != nil {
		return err
	}
	if _, active := reducer.active[payload.RunID]; !active {
		return fmt.Errorf("input for inactive run %q at sequence %d", payload.RunID, record.Sequence)
	}
	state := &reducer.state
	item := Item{Kind: ItemUserText, Text: payload.Text}
	state.Items = append(state.Items, item)
	state.Blocks = append(state.Blocks, ConversationBlock{
		RunID:         payload.RunID,
		StartSequence: record.Sequence,
		EndSequence:   record.Sequence,
		Entries:       []ConversationEntry{{Sequence: record.Sequence, Time: record.Time, Item: item}},
	})
	reducer.blockByRun[payload.RunID] = len(state.Blocks) - 1
	return nil
}

func (reducer *stateReducer) applyModelResponse(record Record) error {
	payload, err := decodeRecord[ModelResponseRecord](record)
	if err != nil {
		return err
	}
	if _, active := reducer.active[payload.RunID]; !active {
		return fmt.Errorf("model response for inactive run %q at sequence %d", payload.RunID, record.Sequence)
	}
	state := &reducer.state
	if payload.Backend != state.Selection.Backend || payload.Model != state.Selection.Model || payload.Epoch != state.Selection.Epoch {
		return fmt.Errorf("model response selection mismatch at sequence %d", record.Sequence)
	}
	blockIndex, exists := reducer.blockByRun[payload.RunID]
	if !exists {
		return fmt.Errorf("model response has no user block at sequence %d", record.Sequence)
	}
	for _, item := range payload.Items {
		if err := validateAcceptedItem(item); err != nil {
			return fmt.Errorf("invalid model response at sequence %d: %w", record.Sequence, err)
		}
		if err := validateProviderOwnership(item, state.Selection); err != nil {
			return fmt.Errorf("invalid model response at sequence %d: %w", record.Sequence, err)
		}
		state.Items = append(state.Items, cloneItem(item))
		state.Blocks[blockIndex].Entries = append(state.Blocks[blockIndex].Entries, ConversationEntry{Sequence: record.Sequence, Time: record.Time, Item: cloneItem(item)})
		if item.Kind == ItemToolCall {
			if pendingToolIndex(state.PendingTools, item.ToolCall.ID) >= 0 {
				return fmt.Errorf("duplicate tool call ID %q at sequence %d", item.ToolCall.ID, record.Sequence)
			}
			state.PendingTools = append(state.PendingTools, PendingTool{RunID: payload.RunID, Call: cloneToolCall(*item.ToolCall)})
		}
	}
	state.Blocks[blockIndex].EndSequence = record.Sequence
	state.Usage = state.Usage.Add(payload.Usage)
	return nil
}

func (reducer *stateReducer) applyToolResult(record Record) error {
	payload, err := decodeRecord[ToolResultRecord](record)
	if err != nil {
		return err
	}
	payload.Result.Details, err = normalizeDetails(payload.Result.Details)
	if err != nil {
		return fmt.Errorf("invalid tool result at sequence %d: %w", record.Sequence, err)
	}
	state := &reducer.state
	index := pendingToolIndex(state.PendingTools, payload.Result.CallID)
	if index < 0 {
		return fmt.Errorf("result for unknown tool call %q at sequence %d", payload.Result.CallID, record.Sequence)
	}
	if state.PendingTools[index].RunID != payload.RunID {
		return fmt.Errorf("tool result run mismatch for call %q at sequence %d", payload.Result.CallID, record.Sequence)
	}
	blockIndex, blockExists := reducer.blockByRun[payload.RunID]
	if !blockExists {
		return fmt.Errorf("tool result has no user block at sequence %d", record.Sequence)
	}
	item := Item{Kind: ItemToolResult, ToolResult: cloneToolResult(&payload.Result)}
	state.Items = append(state.Items, item)
	state.Blocks[blockIndex].Entries = append(state.Blocks[blockIndex].Entries, ConversationEntry{Sequence: record.Sequence, Time: record.Time, Item: cloneItem(item)})
	state.Blocks[blockIndex].EndSequence = record.Sequence
	state.PendingTools = append(state.PendingTools[:index], state.PendingTools[index+1:]...)
	return nil
}

func (reducer *stateReducer) applyBoundaryEvent(record Record) error {
	payload, err := decodeRecord[BoundaryEventRecord](record)
	if err != nil {
		return err
	}
	payload.JobID = strings.TrimSpace(payload.JobID)
	payload.Content = strings.TrimSpace(payload.Content)
	_, active := reducer.active[payload.RunID]
	if !active || payload.JobID == "" || payload.Content == "" {
		return fmt.Errorf("invalid boundary event at sequence %d", record.Sequence)
	}
	state := &reducer.state
	if _, exists := state.DeliveredJobs[payload.JobID]; exists {
		return fmt.Errorf("duplicate boundary event for job %q at sequence %d", payload.JobID, record.Sequence)
	}
	blockIndex, exists := reducer.blockByRun[payload.RunID]
	if !exists {
		return fmt.Errorf("boundary event has no user block at sequence %d", record.Sequence)
	}
	item := Item{Kind: ItemBoundaryText, Text: payload.Content}
	state.Items = append(state.Items, item)
	state.Blocks[blockIndex].Entries = append(state.Blocks[blockIndex].Entries, ConversationEntry{Sequence: record.Sequence, Time: record.Time, Item: item})
	state.Blocks[blockIndex].EndSequence = record.Sequence
	state.DeliveredJobs[payload.JobID] = struct{}{}
	state.DetachedJobs = removeJobID(state.DetachedJobs, payload.JobID)
	return nil
}

func (reducer *stateReducer) applyRunFinished(record Record) error {
	payload, err := decodeRecord[RunFinishedRecord](record)
	if err != nil {
		return err
	}
	if !validRunStatus(payload.Status) {
		return fmt.Errorf("invalid run status %q at sequence %d", payload.Status, record.Sequence)
	}
	if err := validateDetachedJobIDs(payload.DetachedJobs); err != nil {
		return fmt.Errorf("invalid detached jobs at sequence %d: %w", record.Sequence, err)
	}
	if _, active := reducer.active[payload.RunID]; !active {
		return fmt.Errorf("finish for inactive run %q at sequence %d", payload.RunID, record.Sequence)
	}
	state := &reducer.state
	for _, pending := range state.PendingTools {
		if pending.RunID == payload.RunID {
			return fmt.Errorf("finish for run %q with pending tool call %q at sequence %d", payload.RunID, pending.Call.ID, record.Sequence)
		}
	}
	if payload.DetachedJobs != nil {
		state.DetachedJobs = append([]string(nil), payload.DetachedJobs...)
	}
	delete(reducer.active, payload.RunID)
	for index, runID := range state.ActiveRuns {
		if runID == payload.RunID {
			state.ActiveRuns = append(state.ActiveRuns[:index], state.ActiveRuns[index+1:]...)
			break
		}
	}
	if blockIndex, exists := reducer.blockByRun[payload.RunID]; exists {
		state.Blocks[blockIndex].EndSequence = record.Sequence
		state.Blocks[blockIndex].Status = payload.Status
		state.Blocks[blockIndex].Error = payload.Error
		state.Blocks[blockIndex].ToolLimitReached = payload.ToolLimitReached
		state.Blocks[blockIndex].DetachedJobs = append([]string(nil), payload.DetachedJobs...)
		delete(reducer.blockByRun, payload.RunID)
	}
	return nil
}

func (reducer *stateReducer) applyContextCompacted(record Record) error {
	payload, err := decodeRecord[ContextCompactedRecord](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	if err := validateCompactionBoundary(*state, payload, record.Sequence, state.hasUnfinishedWork()); err != nil {
		return err
	}
	payload.Summary = strings.TrimSpace(payload.Summary)
	state.Compaction = &payload
	state.CompactionCount++
	state.Usage = state.Usage.Add(payload.Usage)
	return nil
}

func (reducer *stateReducer) applyToolResultsPruned(record Record) error {
	payload, err := decodeRecord[ToolResultsPrunedRecord](record)
	if err != nil {
		return err
	}
	state := &reducer.state
	if err := validateToolPruningBoundary(*state, payload, record.Sequence, state.hasUnfinishedWork()); err != nil {
		return err
	}
	state.ToolPruning = &payload
	state.ToolPruningCount++
	return nil
}

func pendingToolIndex(pending []PendingTool, callID string) int {
	for index := range pending {
		if pending[index].Call.ID == callID {
			return index
		}
	}
	return -1
}

func validateDetachedJobIDs(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id {
			return fmt.Errorf("invalid job id %q", id)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate job id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func removeJobID(ids []string, removed string) []string {
	for index, id := range ids {
		if id == removed {
			return append(ids[:index:index], ids[index+1:]...)
		}
	}
	return ids
}
