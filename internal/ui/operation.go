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
)

// activeOperation is the screen's single exclusive cancellable-operation slot.
// A scope switch lives separately because it may overlap a model turn; the
// screen projects it into this slot's maintenance view once no turn is active.
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

func (operation activeOperation) label() string {
	switch operation.kind {
	case operationShell:
		return "Running shell"
	case operationScope:
		return "Checking filesystem scope"
	case operationCompaction:
		return "Compacting context"
	default:
		return "Working"
	}
}

// maintenanceOperation is the single source of truth for whether the UI is
// occupied by maintenance. Scope switching is concurrent only with a turn: if
// that turn finishes first, the still-pending switch immediately becomes the
// maintenance owner without an event-order-dependent state transfer.
func (m screenModel) maintenanceOperation() activeOperation {
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
