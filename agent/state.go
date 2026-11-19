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
	live, err := foldRecords(records, true)
	if err != nil {
		return State{}, err
	}
	// Preserve State's non-blocking snapshot behavior during an active run. The
	// status projection calls the model backend, so publish it only when runMu
	// proves that no model request is executing; the sequence guard rejects this
	// replay if an operation completed between Records and TryLock.
	if runtime.runMu.TryLock() {
		runtime.configMu.RLock()
		projected := live.state
		runtime.restoreContextSamples(&projected)
		status := runtime.calculateSessionStatus(projected, nil)
		runtime.configMu.RUnlock()
		runtime.lastContextSamples = projected.contextSamples
		runtime.storeSessionStatus(live.state.LastSequence, status)
		runtime.runMu.Unlock()
	}
	state := live.state
	state.contextSamples = [2]contextSample{}
	state.responseTokens = nil
	return state, nil
}
