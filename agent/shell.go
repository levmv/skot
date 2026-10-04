package agent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/levmv/skot/model"
)

func (runtime *Runtime) RunShell(ctx context.Context, command string) (model.ToolResult, error) {
	return runtime.runUserShell(ctx, command, true)
}

func (runtime *Runtime) RunPrivateShell(ctx context.Context, command string) (model.ToolResult, error) {
	return runtime.runUserShell(ctx, command, false)
}

func (runtime *Runtime) runUserShell(ctx context.Context, command string, journaled bool) (model.ToolResult, error) {
	if !runtime.runMu.TryLock() {
		return model.ToolResult{}, ErrRunActive
	}
	defer runtime.runMu.Unlock()

	command = strings.TrimSpace(command)
	if command == "" {
		return model.ToolResult{}, errors.New("shell command is required")
	}
	if runtime.userShell == nil {
		return model.ToolResult{}, errors.New("user shell is unavailable")
	}
	if !journaled {
		return runtime.executeUserShell(ctx, "", command)
	}

	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return model.ToolResult{}, fmt.Errorf("read journal: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return model.ToolResult{}, err
	}
	runtime.publishSessionStatus(live.state)
	if live.state.hasUnfinishedWork() {
		return model.ToolResult{}, unfinishedWorkError("running a shell command")
	}
	if err := runtime.prepareSession(ctx, live); err != nil {
		return model.ToolResult{}, err
	}

	runID, err := newID("run")
	if err != nil {
		return model.ToolResult{}, err
	}
	callID, err := newID("call")
	if err != nil {
		return model.ToolResult{}, err
	}
	responseID, err := newID("response")
	if err != nil {
		return model.ToolResult{}, err
	}
	arguments, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: runtime.sanitize(command)}, json.Deterministic(true))
	if err != nil {
		return model.ToolResult{}, fmt.Errorf("encode shell command: %w", err)
	}

	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunStarted, RunStartedRecord{RunID: runID}); err != nil {
		return model.ToolResult{}, err
	}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordRunInputAdded, RunInputAddedRecord{RunID: runID, Text: "!" + runtime.sanitize(command)}); err != nil {
		return model.ToolResult{}, err
	}
	call := model.ToolCall{ID: callID, Name: "bash", RawArguments: string(arguments)}
	if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordModelResponse, ModelResponseRecord{
		RunID:   runID,
		Backend: live.state.Selection.Backend,
		Model:   live.state.Selection.Model,
		Epoch:   live.state.Selection.Epoch,
		Items: []model.Item{{
			Kind:       model.ItemToolCall,
			ResponseID: responseID,
			ToolCall:   &call,
		}},
		StopReason: "user_shell",
	}); err != nil {
		return model.ToolResult{}, err
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

func (runtime *Runtime) executeUserShell(ctx context.Context, callID, command string) (model.ToolResult, error) {
	output, err := runtime.userShell(ctx, command)
	details, detailErr := runtime.sanitizeOutputDetails(output.Details)
	if detailErr != nil {
		err = errors.Join(err, fmt.Errorf("invalid shell output: %w", detailErr))
		details = nil
	}
	result := model.ToolResult{CallID: callID, Content: runtime.sanitizeContent(output.Content), Details: details, Error: err != nil}
	if err != nil && strings.TrimSpace(result.Content.Text()) == "" {
		result.Content = model.TextContent(runtime.sanitize(err.Error()))
	}
	return result, sanitizeError(err, runtime.sanitize)
}
