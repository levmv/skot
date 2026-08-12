package agent

import "context"

type deliveredInput struct {
	Text     string
	Sequence uint64
}

// QueueInput keeps editor input transient until a model-request boundary.
func (runtime *Runtime) QueueInput(input string) error {
	input, err := normalizeInput(input)
	if err != nil {
		return err
	}
	runtime.queueMu.Lock()
	runtime.pendingInputs = append(runtime.pendingInputs, input)
	runtime.queueMu.Unlock()
	return nil
}

// ClaimQueued removes the oldest undelivered input so it can start a new run.
func (runtime *Runtime) ClaimQueued() (string, bool) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return "", false
	}
	input := runtime.pendingInputs[0]
	runtime.pendingInputs = runtime.pendingInputs[1:]
	return input, true
}

// PopQueued removes the newest undelivered input for editor recall (Alt+Up).
func (runtime *Runtime) PopQueued() (string, bool) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	if len(runtime.pendingInputs) == 0 {
		return "", false
	}
	last := len(runtime.pendingInputs) - 1
	input := runtime.pendingInputs[last]
	runtime.pendingInputs = runtime.pendingInputs[:last]
	return input, true
}

// RestoreQueued returns all undelivered input in FIFO order and clears it.
func (runtime *Runtime) RestoreQueued() []string {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	inputs := append([]string(nil), runtime.pendingInputs...)
	runtime.pendingInputs = nil
	return inputs
}

func (runtime *Runtime) QueuedInputs() []string {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	return append([]string(nil), runtime.pendingInputs...)
}

func (runtime *Runtime) deliverQueuedInput(ctx context.Context, reducer *stateReducer, runID string) ([]deliveredInput, error) {
	runtime.queueMu.Lock()
	defer runtime.queueMu.Unlock()
	delivered := make([]deliveredInput, 0, len(runtime.pendingInputs))
	for len(runtime.pendingInputs) != 0 {
		input := runtime.sanitize(runtime.pendingInputs[0])
		record, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: input})
		if err != nil {
			return delivered, err
		}
		delivered = append(delivered, deliveredInput{Text: input, Sequence: record.Sequence})
		runtime.pendingInputs = runtime.pendingInputs[1:]
	}
	return delivered, nil
}
