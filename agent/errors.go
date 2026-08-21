package agent

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidRequest marks a request or configuration that cannot succeed
	// when retried unchanged.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrProviderFailure marks a transport, protocol, or provider-side failure
	// whose recovery depends on provider or external state rather than changing
	// the invocation. Provider failures are not necessarily worth retrying
	// immediately; ProviderError carries that separate decision.
	ErrProviderFailure = errors.New("provider failure")
	// ErrModelRequestBudget reports that one logical model request used all of
	// its wall-clock allowance, including attempts and retry delays.
	ErrModelRequestBudget = errors.New("model request retry budget exhausted")
	// ErrModelRequestTooLarge reports that a logical model request must be
	// reduced before another attempt can succeed.
	ErrModelRequestTooLarge = errors.New("model request too large")
	// ErrModelStreamIdle reports that an open model stream produced no payload
	// within its configured idle interval.
	ErrModelStreamIdle = errors.New("model stream idle timeout")
	// ErrModelUnavailable reports that a runtime knows its selected model but has
	// no executable backend for it.
	ErrModelUnavailable = errors.New("model unavailable")
	// ErrToolFatal marks an execution failure the model cannot repair by
	// changing tool arguments, such as a configured executable disappearing
	// after startup validation. Its tool result is journaled before the run
	// stops.
	ErrToolFatal = errors.New("fatal tool execution failure")
)

type modelUnavailableError struct{ model string }

func (err modelUnavailableError) Error() string {
	return fmt.Sprintf("model %q is unavailable", err.model)
}

func (err modelUnavailableError) Is(target error) bool { return target == ErrModelUnavailable }

// ProviderError preserves provider response metadata needed by retry policy.
// Retryable describes immediate retry eligibility, independently from the
// caller-facing ErrProviderFailure classification used for exit status.
type ProviderError struct {
	Cause      error
	StatusCode int
	Kind       ProviderErrorKind
	// Code is the provider's structured error code when one was present in the
	// response. Callers must not infer billing or quota values from Cause text.
	Code string
	// Type is the provider's structured error type when the wire distinguishes it
	// from Code. Classification may use either exact field, never Cause prose.
	Type       string
	Retryable  bool
	RetryAfter time.Duration
}

// ProviderErrorKind describes the recovery action suggested by a structured
// provider failure. The empty value means the response was not specific enough
// to classify without guessing.
type ProviderErrorKind string

const (
	ProviderErrorAuthentication  ProviderErrorKind = "authentication"
	ProviderErrorPermission      ProviderErrorKind = "permission"
	ProviderErrorSubscription    ProviderErrorKind = "subscription"
	ProviderErrorQuota           ProviderErrorKind = "quota"
	ProviderErrorRateLimit       ProviderErrorKind = "rate_limit"
	ProviderErrorRequest         ProviderErrorKind = "request"
	ProviderErrorRequestTooLarge ProviderErrorKind = "request_too_large"
	ProviderErrorUnavailable     ProviderErrorKind = "unavailable"
)

func (err *ProviderError) Error() string {
	if err == nil || err.Cause == nil {
		return "provider request failed"
	}
	return err.Cause.Error()
}

func (err *ProviderError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (err *ProviderError) Is(target error) bool {
	return err != nil && target == ErrModelRequestTooLarge && err.Kind == ProviderErrorRequestTooLarge
}

type classifiedError struct {
	class error
	cause error
}

func (err classifiedError) Error() string { return err.cause.Error() }
func (err classifiedError) Unwrap() error { return err.cause }
func (err classifiedError) Is(target error) bool {
	return target == err.class
}

// MarkInvalidRequest preserves err's text and cause while classifying the
// caller action required to make a retry useful.
func MarkInvalidRequest(err error) error {
	if err == nil || errors.Is(err, ErrInvalidRequest) {
		return err
	}
	return classifiedError{class: ErrInvalidRequest, cause: err}
}

// MarkProviderFailure preserves err's text and cause while classifying it as
// retryable without changing the invocation. Existing classifications win.
func MarkProviderFailure(err error) error {
	if err == nil || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrProviderFailure) {
		return err
	}
	return classifiedError{class: ErrProviderFailure, cause: err}
}
