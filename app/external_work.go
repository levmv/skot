package app

import (
	"context"
	"sort"

	"github.com/levmv/skot/agent"
	workspacetools "github.com/levmv/skot/tools"
)

type processExternalWork struct {
	processes *workspacetools.ProcessManager
	await     bool
}

// applicationExternalWork presents process jobs and child agents as one
// runtime boundary while preserving their distinct lifecycle policies.
type applicationExternalWork struct {
	processes processExternalWork
	agents    *childSupervisor
}

func (work applicationExternalWork) Status(id string) ([]agent.Detail, bool) {
	if work.agents != nil {
		if details, ok := work.agents.Status(id); ok {
			return details, true
		}
	}
	return work.processes.Status(id)
}

func (work applicationExternalWork) PendingEvents(sessionID string) []agent.BoundaryEvent {
	events := work.processes.PendingEvents(sessionID)
	if work.agents != nil {
		events = append(events, work.agents.PendingEvents(sessionID)...)
	}
	sort.Slice(events, func(left, right int) bool {
		if events[left].FinishedAt.Equal(events[right].FinishedAt) {
			return events[left].JobID < events[right].JobID
		}
		return events[left].FinishedAt.Before(events[right].FinishedAt)
	})
	return events
}

func (work applicationExternalWork) EventCommitted(id string) {
	work.processes.EventCommitted(id)
	if work.agents != nil {
		work.agents.EventCommitted(id)
	}
}

func (work applicationExternalWork) ToolResultCommitted(result agent.ToolResult) {
	work.processes.ToolResultCommitted(result)
	if work.agents != nil {
		work.agents.ToolResultCommitted(result)
	}
}

func (work applicationExternalWork) Await(ctx context.Context, sessionID string) (bool, error) {
	return work.processes.Await(ctx, sessionID)
}

func (work applicationExternalWork) DetachedJobs(sessionID string) []string {
	return work.processes.DetachedJobs(sessionID)
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
