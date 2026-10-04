package model

import (
	"errors"
	"fmt"
	"strings"
)

// APIRequiredError means model selection needs an explicit protocol because
// neither the model catalog nor the provider supplies one.
type APIRequiredError struct {
	URI string
}

func (failure *APIRequiredError) Error() string {
	return fmt.Sprintf(
		"model %q has no known protocol; specify its API",
		strings.TrimSpace(failure.URI),
	)
}

// IsAPIRequired reports whether err requests an explicit model protocol.
func IsAPIRequired(err error) bool {
	_, ok := errors.AsType[*APIRequiredError](err)
	return ok
}

// ContextWindowRequiredError reports an undeclared route whose context
// window could not be discovered.
type ContextWindowRequiredError struct {
	URI string
}

func (failure *ContextWindowRequiredError) Error() string {
	return fmt.Sprintf("model %q needs a context window", strings.TrimSpace(failure.URI))
}

// IsContextWindowRequired reports whether err requests an explicit context window.
func IsContextWindowRequired(err error) bool {
	_, ok := errors.AsType[*ContextWindowRequiredError](err)
	return ok
}
