package agent

import (
	"context"

	"github.com/levmv/skot/model"
)

type deliveredInput struct {
	Text     string
	Sequence uint64
	Details  []model.Detail
}

// QueueInput keeps editor input transient until a model-request boundary.
func (runtime *Runtime) QueueInput(input string) error {
	return runtime.QueueUserInput(Input{Text: input})
}

// QueueUserInput captures a message and its context for the next request boundary.
func (runtime *Runtime) QueueUserInput(input Input) error {
	input, err := runtime.normalizeUserInput(input)
	if err != nil {
		return err
	}
	runtime.queueMu.Lock()
	runtime.pendingInputs = append(runtime.pendingInputs, input)
	runtime.queueMu.Unlock()
	return nil
}

// ClaimQueued removes the oldest input and returns its authored text. Use
// ClaimQueuedInput to preserve captured instructions and resources.
func (runtime *Runtime) ClaimQueued() (string, bool) {
	input, ok := runtime.ClaimQueuedInput()
	return input.Text, ok
}

// ClaimQueuedInput removes the oldest input, preserving its captured context.
func (runtime *Runtime) ClaimQueuedInput() (Input, bool) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return Input{}, false
	}
	input := runtime.pendingInputs[0]
	runtime.pendingInputs = runtime.pendingInputs[1:]
	return input, true
}

// PopQueued removes the newest input and returns its authored text.
// Use PopQueuedInput to preserve captured instructions and resources.
func (runtime *Runtime) PopQueued() (string, bool) {
	input, ok := runtime.PopQueuedInput()
	return input.Text, ok
}

// PopQueuedInput removes the newest input for editor recall.
func (runtime *Runtime) PopQueuedInput() (Input, bool) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return Input{}, false
	}
	last := len(runtime.pendingInputs) - 1
	input := runtime.pendingInputs[last]
	runtime.pendingInputs = runtime.pendingInputs[:last]
	return input, true
}

func (runtime *Runtime) QueuedInputs() []string {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	inputs := make([]string, len(runtime.pendingInputs))
	for index, input := range runtime.pendingInputs {
		inputs[index] = input.Text
	}
	return inputs
}

func (runtime *Runtime) queuedModelInputs(state State) []string {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return nil
	}
	latest := state.latestInstructions()
	inputs := make([]string, len(runtime.pendingInputs))
	for index, input := range runtime.pendingInputs {
		inputs[index] = input.withNewInstructions(latest).modelText()
	}
	return inputs
}

func (runtime *Runtime) deliverQueuedInput(ctx context.Context, reducer *stateReducer, runID string) ([]deliveredInput, error) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return nil, nil
	}
	delivered := make([]deliveredInput, 0, len(runtime.pendingInputs))
	latest := reducer.state.latestInstructions()
	for len(runtime.pendingInputs) != 0 {
		input, err := runtime.normalizeUserInput(runtime.pendingInputs[0])
		if err != nil {
			return delivered, err
		}
		input = input.withNewInstructions(latest)
		record, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: input.Text, Instructions: input.Instructions, ReadOnlyPaths: input.ReadOnlyPaths})
		if err != nil {
			return delivered, err
		}
		delivered = append(delivered, deliveredInput{Text: input.Text, Sequence: record.Sequence, Details: input.details()})
		runtime.pendingInputs = runtime.pendingInputs[1:]
	}
	return delivered, nil
}
