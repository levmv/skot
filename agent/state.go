package agent

import (
	"context"
	"fmt"
)

// State returns the journaled session state reconstructed from its records.
// Transient queued input is intentionally not included.
func (runtime *Runtime) State(ctx context.Context) (State, error) {
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return State{}, fmt.Errorf("read journal: %w", err)
	}
	state, err := Replay(records)
	if err != nil {
		return State{}, err
	}
	// Preserve State's non-blocking snapshot behavior during an active run. The
	// status projection calls the model adapter, so publish it only when runMu
	// proves that no model request is executing; the sequence guard rejects this
	// replay if an operation completed between Records and TryLock.
	if runtime.runMu.TryLock() {
		runtime.configMu.RLock()
		status := runtime.calculateSessionStatus(state)
		runtime.configMu.RUnlock()
		runtime.storeSessionStatus(state.LastSequence, status)
		runtime.runMu.Unlock()
	}
	return state, nil
}
