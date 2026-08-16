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

// activeOperation is the screen's single cancellable-operation slot. A model
// turn and maintenance work cannot own independent cancellation state.
type activeOperation struct {
	kind          operationKind
	startedAt     time.Time
	cancel        context.CancelFunc
	events        chan tea.Msg
	renderPending bool
	changedPaths  []string
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

func (operation *activeOperation) clear() {
	*operation = activeOperation{}
}
