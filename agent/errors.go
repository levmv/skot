package agent

import (
	"errors"
	"fmt"
)

var (
	// ErrModelRequestBudget reports that one logical model request used all of
	// its wall-clock allowance, including attempts and retry delays.
	ErrModelRequestBudget = errors.New("model request retry budget exhausted")
	// ErrModelUnavailable reports a selected model with no executable backend.
	ErrModelUnavailable = errors.New("model unavailable")
)

type modelUnavailableError struct{ model string }

func (err modelUnavailableError) Error() string {
	return fmt.Sprintf("model %q is unavailable", err.model)
}

func (err modelUnavailableError) Is(target error) bool { return target == ErrModelUnavailable }
