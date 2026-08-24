package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (runtime *Runtime) RunShell(ctx context.Context, command string) (ToolResult, error) {
	return runtime.runUserShell(ctx, command, true)
}

func (runtime *Runtime) RunPrivateShell(ctx context.Context, command string) (ToolResult, error) {
	return runtime.runUserShell(ctx, command, false)
}

func (runtime *Runtime) runUserShell(ctx context.Context, command string, journaled bool) (ToolResult, error) {
	if !runtime.runMu.TryLock() {
		return ToolResult{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()

	command = strings.TrimSpace(command)
	if command == "" {
		return ToolResult{}, errors.New("shell command is required")
	}
	if runtime.userShell == nil {
		return ToolResult{}, errors.New("user shell is unavailable")
	}
	if !journaled {
		return runtime.executeUserShell(ctx, "", command)
	}

	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read journal: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return ToolResult{}, err
	}
	runtime.publishSessionStatus(live.state)
	if live.state.hasUnfinishedWork() {
		return ToolResult{}, unfinishedWorkError("running a shell command")
	}
	if err := runtime.prepareSession(ctx, live); err != nil {
		return ToolResult{}, err
	}

	runID, err := newID("run")
	if err != nil {
		return ToolResult{}, err
	}
	callID, err := newID("call")
	if err != nil {
		return ToolResult{}, err
	}
	responseID, err := newID("response")
	if err != nil {
		return ToolResult{}, err
	}
	arguments, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: runtime.sanitize(command)})
	if err != nil {
		return ToolResult{}, fmt.Errorf("encode shell command: %w", err)
	}

	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunStarted, RunStartedRecord{RunID: runID}); err != nil {
		return ToolResult{}, err
	}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: "!" + runtime.sanitize(command)}); err != nil {
		return ToolResult{}, err
	}
	call := ToolCall{ID: callID, Name: "bash", RawArguments: string(arguments)}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordModelResponse, ModelResponseRecord{
		RunID:   runID,
		Backend: live.state.Selection.Backend,
		Model:   live.state.Selection.Model,
		Epoch:   live.state.Selection.Epoch,
		Items: []Item{{
			Kind:       ItemToolCall,
			ResponseID: responseID,
			ToolCall:   &call,
		}},
		StopReason: "user_shell",
	}); err != nil {
		return ToolResult{}, err
	}

	result, runErr := runtime.executeUserShell(ctx, callID, command)
	journalCtx := context.WithoutCancel(ctx)
	if _, err := appendRecordAndApply(journalCtx, runtime.journal, live, RecordToolResult, ToolResultRecord{RunID: runID, Result: result}); err != nil {
		// Leave the run and call unfinished so ordinary reconciliation can mark
		// the outcome unknown. Recording a finish without the result would make
		// the journaled sequence internally contradictory.
		return result, errors.Join(runErr, err)
	}
	status := RunCompleted
	if ctx.Err() != nil {
		status = RunCancelled
		runErr = errors.Join(runErr, ctx.Err())
	} else if runErr != nil {
		status = RunFailed
	}
	finished := RunFinishedRecord{RunID: runID, Status: status}
	if runErr != nil {
		runErr = sanitizeError(runErr, runtime.sanitize)
		finished.Error = runErr.Error()
	}
	if _, err := appendRecordAndApply(journalCtx, runtime.journal, live, RecordRunFinished, finished); err != nil {
		runErr = errors.Join(runErr, err)
	}
	runtime.publishSessionStatus(live.state)
	return result, runErr
}

func (runtime *Runtime) executeUserShell(ctx context.Context, callID, command string) (ToolResult, error) {
	output, err := runtime.userShell(ctx, command)
	details, detailErr := runtime.sanitizeToolDetails(output.Details)
	if detailErr != nil {
		err = errors.Join(err, fmt.Errorf("invalid shell output: %w", detailErr))
		details = nil
	}
	result := ToolResult{CallID: callID, Content: runtime.sanitizeContent(output.Content), Details: details, Error: err != nil}
	if err != nil && strings.TrimSpace(result.Content.Text()) == "" {
		result.Content = TextContent(runtime.sanitize(err.Error()))
	}
	return result, sanitizeError(err, runtime.sanitize)
}
