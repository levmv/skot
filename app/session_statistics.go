package app

import (
	"context"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/state"
)

// SessionStatistics describes one complete journal prefix. ContextReport uses
// its last saved configuration, excluding transient queued input and changes
// to host instructions or settings made since then. Accounting includes all
// recorded model attempts, including failed calls and compaction, but no child
// journals. Cached tokens describe past requests, never current cache residency.
type SessionStatistics struct {
	Model                  string
	ContextReport          *agent.ContextReport
	ContextWindowEstimated bool
	// ContextError explains why the saved route cannot be projected by this
	// build. Accounting remains available. A missing configuration also leaves
	// ContextReport nil, without an error.
	ContextError string
	Accounting   agent.UsageReport
	LastSequence uint64
	// UpdatedAt is the time of the last complete record, not the journal mtime.
	UpdatedAt time.Time
}

// ReadSessionStatistics reads a managed session without taking execution
// ownership, repairing its journal, loading credentials or making network
// requests. All fields refer to the same finite, complete journal prefix.
func ReadSessionStatistics(ctx context.Context, home, id string) (SessionStatistics, error) {
	home, err := state.ResolveHome(home)
	if err != nil {
		return SessionStatistics{}, err
	}
	snapshot, err := session.Inspect(ctx, home, id)
	if err != nil {
		return SessionStatistics{}, err
	}
	accounting, err := agent.ReplayUsage(snapshot.Records, 0)
	if err != nil {
		return SessionStatistics{}, err
	}
	saved := snapshot.State
	result := SessionStatistics{Accounting: accounting, LastSequence: saved.LastSequence}
	if len(snapshot.Records) != 0 {
		result.UpdatedAt = snapshot.Records[len(snapshot.Records)-1].Time
	}
	if saved.Selection.Provider != "" {
		result.Model = saved.Selection.Provider + "/" + saved.Selection.Model
	}
	if saved.Configured == nil {
		return result, ctx.Err()
	}
	result.ContextWindowEstimated = saved.Configured.RuntimePolicy.ContextWindowEstimated
	project, err := modelconfig.SavedItemProjection(saved.Selection)
	if err != nil {
		result.ContextError = err.Error()
	} else {
		report, _ := agent.ContextReportForState(saved, project)
		result.ContextReport = &report
	}
	return result, ctx.Err()
}
