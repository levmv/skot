package model

import (
	"errors"
	"time"
)

var (
	// ErrInvalidRequest marks a request or configuration that cannot succeed
	// when retried unchanged.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrRequestNotSent marks a Complete failure before attempting to send the
	// model request. Its absence does not prove delivery or a charge.
	ErrRequestNotSent = errors.New("model request was not sent")
	// ErrOutputLimitUnsupported means this route cannot honor MaxOutputTokens.
	// Complete also classifies this as ErrInvalidRequest.
	ErrOutputLimitUnsupported = errors.New("output token limit is not supported by this route")
	// ErrProviderFailure marks a transport, protocol, or provider-side failure
	// whose recovery depends on provider or external state rather than changing
	// the invocation. Provider failures are not necessarily worth retrying
	// immediately; ProviderError carries that separate decision.
	ErrProviderFailure = errors.New("provider failure")
	// ErrModelRequestTooLarge reports that a logical model request must be
	// reduced before another attempt can succeed.
	ErrModelRequestTooLarge = errors.New("model request too large")
	// ErrModelStreamIdle reports that an open model stream produced no payload
	// within its configured idle interval.
	ErrModelStreamIdle = errors.New("model stream idle timeout")
)

// ProviderError carries structured metadata for retry and fallback decisions.
// Retryable indicates whether an unchanged request is worth retrying immediately.
type ProviderError struct {
	Cause      error
	StatusCode int
	Kind       ProviderErrorKind
	// Code is the provider's structured error code when one was present in the
	// response. Callers must not infer billing or quota values from Cause text.
	Code string
	// Type is the provider's structured error type when distinct from Code.
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

// MarkRequestNotSent preserves err's text and cause while marking a failure
// known to precede sending the model request.
func MarkRequestNotSent(err error) error {
	if err == nil || errors.Is(err, ErrRequestNotSent) {
		return err
	}
	return classifiedError{class: ErrRequestNotSent, cause: err}
}

// MarkProviderFailure preserves err's text and cause while classifying it as
// a provider failure. ProviderError.Retryable separately controls retry policy.
func MarkProviderFailure(err error) error {
	if err == nil || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrProviderFailure) {
		return err
	}
	return classifiedError{class: ErrProviderFailure, cause: err}
}
