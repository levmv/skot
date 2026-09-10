package ui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

type operationKind uint8

const (
	operationNone operationKind = iota
	operationTurn
	operationShell
	operationScope
	operationCompaction
	operationClear
	operationLogin
	operationLogout
)

// activeOperation tracks work in the screen. Credentials and scope switches
// use separate slots so they can overlap a turn without replacing its events.
type activeOperation struct {
	kind          operationKind
	startedAt     time.Time
	cancel        context.CancelFunc
	events        chan tea.Msg
	renderPending bool
	changedPaths  []string
	modelRetry    modelRetryState
	// partialRemoved records that the transcript is missing streamed text the
	// user had already read, and that no message has explained it yet. It
	// outlives every retry group of the turn: a run that ends badly emits
	// RunFinished before the turn does, tearing the group down first.
	partialRemoved bool
}

// withPartialRemovedNote appends the standing explanation for text that vanished
// from the transcript, so every message that can end a turn words it the same.
func (operation activeOperation) withPartialRemovedNote(text string) string {
	if !operation.partialRemoved {
		return text
	}
	return text + " (partial response removed)"
}

type modelRetryState struct {
	pendingFailure string
	blockIndex     int
	visible        bool
	count          int
	lastFailure    string
}

func (operation activeOperation) isTurn() bool {
	return operation.kind == operationTurn
}

func (operation activeOperation) isMaintenance() bool {
	return operation.kind != operationNone && !operation.isTurn()
}

func (operation activeOperation) acceptsQueuedInput() bool {
	return operation.kind == operationTurn || operation.kind == operationCompaction
}

func (operation activeOperation) label() string {
	switch operation.kind {
	case operationShell:
		return "Running shell"
	case operationScope:
		return "Checking filesystem scope"
	case operationCompaction:
		return "Compacting context"
	case operationClear:
		return "Clearing session"
	case operationLogin:
		return "Signing in"
	case operationLogout:
		return "Signing out"
	default:
		return "Working"
	}
}

// maintenanceOperation selects the task that owns the input area. Credential
// updates take precedence; scope changes own it only when no turn is running.
func (m screenModel) maintenanceOperation() activeOperation {
	if m.credentialOperation.isMaintenance() {
		return m.credentialOperation
	}
	if m.operation.isMaintenance() {
		return m.operation
	}
	if m.scope.pending && !m.operation.isTurn() {
		return activeOperation{
			kind: operationScope, startedAt: m.scope.startedAt, cancel: m.scope.cancel,
		}
	}
	return activeOperation{}
}

func (operation *activeOperation) clear() {
	*operation = activeOperation{}
}
