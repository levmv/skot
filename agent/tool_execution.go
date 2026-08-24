package agent

import (
	"context"
	"errors"
)

// Keep one anomalous model response from turning ParallelSafe into unbounded
// process-wide fan-out. The limit is per run; unsafe and unknown tools remain
// serial barriers regardless of the group size.
const maxParallelToolCalls = 4

type toolExecution struct {
	result    ToolResult
	cancelled bool
	fatal     error
}

func (runtime *Runtime) executeToolCalls(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, calls []ToolCall) (bool, error) {
	for start := 0; start < len(calls); {
		if err := ctx.Err(); err != nil {
			return true, nil
		}
		end := start + 1
		if runtime.toolCallParallelSafe(calls[start]) {
			for end < len(calls) && runtime.toolCallParallelSafe(calls[end]) {
				end++
			}
		}
		group := calls[start:end]
		var cancelled bool
		var err error
		if len(group) == 1 {
			cancelled, err = runtime.executeSerialToolCall(ctx, live, emit, runID, group[0])
		} else {
			cancelled, err = runtime.executeParallelToolCalls(ctx, live, emit, runID, group)
		}
		if cancelled || err != nil {
			return cancelled, err
		}
		start = end
	}
	return false, nil
}

func (runtime *Runtime) toolCallParallelSafe(call ToolCall) bool {
	tool, exists := runtime.toolByName[call.Name]
	return exists && tool.Spec.ParallelSafe
}

func (runtime *Runtime) executeSerialToolCall(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, call ToolCall) (bool, error) {
	emitEvent(emit, Event{Kind: EventToolStarted, RunID: runID, Call: cloneToolCallPointer(&call)})
	result, cancelled, fatal := runtime.executeTool(ctx, live.state.SessionID, call)
	if cancelled {
		return true, nil
	}
	if err := runtime.commitToolResult(ctx, live, emit, runID, call, result); err != nil {
		return false, err
	}
	return false, fatal
}

func (runtime *Runtime) executeParallelToolCalls(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, calls []ToolCall) (bool, error) {
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	active := make(chan struct{}, min(maxParallelToolCalls, len(calls)))
	outcomes := make([]<-chan toolExecution, len(calls))
	for index, call := range calls {
		emitEvent(emit, Event{Kind: EventToolStarted, RunID: runID, Call: cloneToolCallPointer(&call)})
		outcome := make(chan toolExecution, 1)
		outcomes[index] = outcome
		go func() {
			select {
			case active <- struct{}{}:
				defer func() { <-active }()
			case <-groupCtx.Done():
				outcome <- toolExecution{cancelled: true}
				return
			}
			result, cancelled, fatal := runtime.executeTool(groupCtx, live.state.SessionID, call)
			outcome <- toolExecution{result: result, cancelled: cancelled, fatal: fatal}
		}()
	}

	for index, outcome := range outcomes {
		execution := <-outcome
		if execution.cancelled {
			cancel()
			waitForToolExecutions(outcomes[index+1:])
			return true, nil
		}
		if err := runtime.commitToolResult(ctx, live, emit, runID, calls[index], execution.result); err != nil {
			cancel()
			waitForToolExecutions(outcomes[index+1:])
			return false, err
		}
		if execution.fatal != nil {
			cancel()
			fatal := execution.fatal
			for remaining := index + 1; remaining < len(outcomes); remaining++ {
				next := <-outcomes[remaining]
				if next.cancelled {
					next.result = ToolResult{
						CallID:  calls[remaining].ID,
						Content: TextContent("tool execution cancelled after a fatal failure in the same parallel group"),
						Error:   true,
					}
				}
				if err := runtime.commitToolResult(ctx, live, emit, runID, calls[remaining], next.result); err != nil {
					return false, errors.Join(fatal, err)
				}
				fatal = errors.Join(fatal, next.fatal)
			}
			return false, fatal
		}
	}
	return false, nil
}

func waitForToolExecutions(outcomes []<-chan toolExecution) {
	for _, outcome := range outcomes {
		<-outcome
	}
}

func (runtime *Runtime) commitToolResult(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, call ToolCall, result ToolResult) error {
	record, err := appendRecordAndApply(ctx, runtime.journal, live, RecordToolResult, ToolResultRecord{RunID: runID, Result: result})
	if err != nil {
		return err
	}
	if runtime.externalWork != nil {
		runtime.externalWork.ToolResultCommitted(*cloneToolResult(&result))
	}
	emitEvent(emit, Event{Sequence: record.Sequence, Kind: EventToolFinished, RunID: runID, Call: cloneToolCallPointer(&call), Result: cloneToolResult(&result)})
	return nil
}

func (runtime *Runtime) commitRejectedToolResult(ctx context.Context, live *stateReducer, emit EmitFunc, runID string, call ToolCall, result ToolResult) error {
	record, err := appendRecordAndApply(ctx, runtime.journal, live, RecordToolResult, ToolResultRecord{RunID: runID, Result: result})
	if err != nil {
		return err
	}
	if runtime.externalWork != nil {
		runtime.externalWork.ToolResultCommitted(*cloneToolResult(&result))
	}
	emitEvent(emit, Event{Sequence: record.Sequence, Kind: EventToolRejected, RunID: runID, Call: cloneToolCallPointer(&call), Result: cloneToolResult(&result)})
	return nil
}
