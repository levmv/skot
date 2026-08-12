package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestExitCodeForClassifiesCallerAction(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: exitOK},
		{name: "unclassified", err: errors.New("broken invariant"), want: exitFailure},
		{name: "configuration", err: agent.MarkInvalidRequest(errors.New("bad model")), want: exitConfig},
		{name: "incomplete", err: agent.RunIncompleteError{StopReason: "length"}, want: exitConfig},
		{name: "fatal tool", err: fmt.Errorf("configured executable disappeared: %w", agent.ErrToolFatal), want: exitConfig},
		{name: "provider", err: agent.MarkProviderFailure(errors.New("unavailable")), want: exitProvider},
		{name: "interrupted", err: fmt.Errorf("run: %w", context.Canceled), want: exitInterrupted},
		{name: "interruption wins", err: errors.Join(agent.MarkProviderFailure(errors.New("unavailable")), context.Canceled), want: exitInterrupted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCodeFor(test.err); got != test.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}
