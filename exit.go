package main

import (
	"context"
	"errors"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
)

// Exit codes are an unattended contract. They describe the caller action:
// inspect, fix and rerun, retry unchanged, or treat the invocation as
// interrupted.
const (
	exitOK          = 0
	exitFailure     = 1
	exitConfig      = 2
	exitProvider    = 3
	exitInterrupted = 130
)

// waitForCleanup runs cleanup and lets ctx interrupt the wait. After the TUI
// restores the terminal, Ctrl+C cancels ctx through the signal handler.
// Cleanup may still be running when this returns context.Canceled.
func waitForCleanup(ctx context.Context, cleanup func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cleanup() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, context.Canceled) {
		return exitInterrupted
	}
	if errors.Is(err, model.ErrInvalidRequest) || errors.Is(err, agent.ErrRunIncomplete) {
		return exitConfig
	}
	if errors.Is(err, model.ErrProviderFailure) {
		return exitProvider
	}
	return exitFailure
}
