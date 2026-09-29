package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestInteractiveCleanupCanBeInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- waitForCleanup(ctx, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cleanup interruption = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interruption kept waiting for application cleanup")
	}
}

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
