package app

import (
	"context"

	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

type processExternalWork struct {
	processes *workspacetools.ProcessManager
	await     bool
}

func (work processExternalWork) Status(id string) ([]agent.Detail, bool) {
	return work.processes.StatusDetails(id)
}

func (work processExternalWork) PendingEvents(sessionID string) []agent.BoundaryEvent {
	completions := work.processes.PendingCompletionEvents(sessionID)
	events := make([]agent.BoundaryEvent, 0, len(completions))
	for _, completion := range completions {
		events = append(events, agent.BoundaryEvent{
			JobID: completion.JobID, FinishedAt: completion.FinishedAt, Content: completion.Content,
		})
	}
	return events
}

func (work processExternalWork) EventCommitted(jobID string) {
	work.processes.MarkCompletionDelivered(jobID)
}

func (work processExternalWork) ToolResultCommitted(result agent.ToolResult) {
	for _, detail := range result.Details {
		process, ok := workspacetools.ProcessResultFromDetail(detail)
		if ok && process.JobID != "" && process.Status != workspacetools.ProcessRunning {
			work.processes.MarkCompletionDelivered(process.JobID)
		}
	}
}

func (work processExternalWork) Await(ctx context.Context, sessionID string) (bool, error) {
	if !work.await {
		return false, nil
	}
	return work.processes.AwaitRequiredJobs(ctx, sessionID)
}

func (work processExternalWork) DetachedJobs(sessionID string) []string {
	return work.processes.DetachedJobs(sessionID)
}
