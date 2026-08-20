package main

import (
	"context"
	"errors"

	"github.com/levmv/skot/agent"
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

func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, context.Canceled) {
		return exitInterrupted
	}
	if errors.Is(err, agent.ErrInvalidRequest) || errors.Is(err, agent.ErrRunIncomplete) || errors.Is(err, agent.ErrToolFatal) {
		return exitConfig
	}
	if errors.Is(err, agent.ErrProviderFailure) {
		return exitProvider
	}
	return exitFailure
}
