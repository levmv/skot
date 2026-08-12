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
	return Replay(records)
}
