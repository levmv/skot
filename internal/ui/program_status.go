package ui

import (
	"context"
	"errors"
	"io"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
)

type programStatus struct {
	state string
	kind  string
}

// OSC 7501 reports replace the root record, so every report repeats app.
// Only fixed values belong here: conversation and credential text stay out.
// https://www.superlogical.com/rex/docs/build/program-status
type programStatusReporter struct {
	out  io.Writer
	last programStatus
}

func (reporter *programStatusReporter) report(status programStatus) error {
	if reporter == nil || reporter.last == status {
		return nil
	}
	sequence := "\x1b]7501;app=skot:state=" + status.state
	if status.kind != "" {
		sequence += ":kind=" + status.kind
	}
	if _, err := io.WriteString(reporter.out, sequence+"\x1b\\"); err != nil {
		return err
	}
	reporter.last = status
	return nil
}

func programResultState(err error, success string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "idle"
	case err != nil:
		return "error"
	default:
		return success
	}
}

func shellProgramResultState(result model.ToolResult, err error) string {
	state := "done"
	if result.Error {
		state = "error"
	}
	// Non-zero exits and timeouts are structured process results, not tool
	// errors. Cancellation still takes precedence over the process outcome.
	for _, detail := range result.Details {
		if process, ok := agent.ProcessResultFromDetail(detail); ok && process.Status != agent.ProcessCompleted {
			state = "error"
			break
		}
	}
	return programResultState(err, state)
}

func (m screenModel) terminalProgramStatus() programStatus {
	if m.quitting {
		return programStatus{state: programResultState(m.exitErr, "clear")}
	}
	if m.operation.kind != operationNone || m.credentialOperation.kind != operationNone ||
		m.scope.pending || m.pendingSessionAction != sessionActionNone {
		return programStatus{state: "working"}
	}
	if m.loginProvider != "" || m.picker.active() && m.picker.kind == pickerLogin && m.picker.startupLogin {
		return programStatus{state: "blocked", kind: "auth"}
	}
	if m.modelContextSelection.uri != "" || m.pathPrompt != notFilesystemPath ||
		m.picker.active() && m.picker.kind == pickerModelAPI {
		return programStatus{state: "blocked", kind: "question"}
	}
	if m.programOutcome != "" {
		return programStatus{state: m.programOutcome}
	}
	return programStatus{state: "idle"}
}
