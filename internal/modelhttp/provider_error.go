package modelhttp

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	modelapi "github.com/levmv/skot/model"
)

// ProviderErrorDetails is the protocol-neutral classification input shared by
// HTTP and in-band provider failures.
type ProviderErrorDetails struct {
	Provider        string
	Model           string
	Status          string
	StatusCode      int
	Message         string
	Code            string
	Type            string
	RetryAfter      time.Duration
	ResponseProfile modelapi.ResponseProfile
	// InBandStatusCode is a protocol-defined upstream status used only for
	// classification. StatusCode is reserved for an actual HTTP error response.
	InBandStatusCode int
}

// ProviderErrorEnvelope is the common structured error object used by the
// supported model protocols and compatible gateways.
type ProviderErrorEnvelope struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitzero"`
	// Compatible gateways use unrelated metadata shapes, so keep the value raw.
	// A nonempty string metadata.error_type takes precedence over Type.
	Metadata jsontext.Value `json:"metadata,omitzero"`
	// Google-compatible endpoints put typed google.rpc details here.
	Details jsontext.Value `json:"details,omitzero"`
}

func (envelope *ProviderErrorEnvelope) messageText() string {
	if envelope == nil {
		return ""
	}
	return strings.TrimSpace(envelope.Message)
}

func (envelope *ProviderErrorEnvelope) errorType() string {
	if envelope == nil {
		return ""
	}
	var metadata struct {
		ErrorType string `json:"error_type,omitempty"`
	}
	if jsonv2.Unmarshal(envelope.Metadata, &metadata) == nil && strings.TrimSpace(metadata.ErrorType) != "" {
		return strings.TrimSpace(metadata.ErrorType)
	}
	return strings.TrimSpace(envelope.Type)
}

// NewProviderEnvelopeError converts an in-band provider error, which has no
// HTTP status, into the same classified error used for HTTP responses.
func NewProviderEnvelopeError(provider, model string, profile modelapi.ResponseProfile, envelope *ProviderErrorEnvelope) error {
	code := ""
	if envelope != nil {
		code = ErrorCode(envelope.Code)
	}
	return NewProviderError(ProviderErrorDetails{
		Provider:        provider,
		Model:           model,
		Message:         envelope.messageText(),
		Code:            code,
		Type:            envelope.errorType(),
		ResponseProfile: profile,
	})
}

// DecodeProviderError classifies a bounded HTTP error body even when reading it
// failed. The caller owns reading, closing, and exposing the original body.
func DecodeProviderError(provider, model string, response *http.Response, body []byte, readErr error) *modelapi.ProviderError {
	var envelope struct {
		Error *ProviderErrorEnvelope `json:"error"`
	}
	message, code, errorType := "", "", ""
	retryAfter := ParseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	structured := jsonv2.Unmarshal(body, &envelope) == nil && envelope.Error != nil
	if structured {
		message = envelope.Error.messageText()
		code = ErrorCode(envelope.Error.Code)
		errorType = envelope.Error.errorType()
		retryAfter = max(retryAfter, GoogleRetryDelay(envelope.Error.Details))
	}
	if message == "" && !structured {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	classified := NewProviderError(ProviderErrorDetails{
		Provider: provider, Model: model, Status: response.Status, StatusCode: response.StatusCode,
		Message: message, Code: code, Type: errorType,
		RetryAfter: retryAfter,
	})
	if readErr != nil {
		classified.Cause = fmt.Errorf("%w (read body: %w)", classified.Cause, readErr)
	}
	return classified
}

// UnsupportedCompletionReasonError reports a non-retryable protocol mismatch.
func UnsupportedCompletionReasonError(provider, reason string) error {
	return &modelapi.ProviderError{
		Cause: modelapi.MarkProviderFailure(fmt.Errorf(
			"%s reported unsupported completion reason %q", strings.TrimSpace(provider), reason)),
	}
}

// NewProviderError classifies failures by HTTP status and structured code/type.
// The message is preserved for display, not used to decide whether to retry.
func NewProviderError(details ProviderErrorDetails) *modelapi.ProviderError {
	provider := strings.TrimSpace(details.Provider)
	model := strings.TrimSpace(details.Model)
	status := strings.TrimSpace(details.Status)
	if status == "" && details.StatusCode > 0 {
		status = fmt.Sprintf("%d %s", details.StatusCode, http.StatusText(details.StatusCode))
	}
	message := strings.TrimSpace(details.Message)
	if message == "" {
		message = strings.TrimSpace(http.StatusText(details.StatusCode))
		if message == "" {
			message = "unknown error"
		}
	}
	code := strings.ToLower(strings.TrimSpace(details.Code))
	errorType := strings.ToLower(strings.TrimSpace(details.Type))
	statusForPolicy := details.StatusCode
	if statusForPolicy == 0 {
		statusForPolicy = details.InBandStatusCode
	}
	// OpenRouter's numeric in-band code is an upstream HTTP status. Use it for
	// recovery without reporting a transport failure; other providers' numeric
	// codes have no shared meaning.
	// https://openrouter.ai/docs/api_reference/errors-and-debugging#mid-stream-errors
	if statusForPolicy == 0 && details.ResponseProfile == modelapi.ResponseProfileOpenRouter {
		if status, err := strconv.Atoi(code); err == nil && status >= 400 && status < 600 {
			statusForPolicy = status
		}
	}
	kind := classifyProviderError(statusForPolicy, code, errorType)
	summary := providerErrorSummary(provider, model, kind)
	var cause error
	if status == "" {
		cause = modelapi.MarkProviderFailure(fmt.Errorf("%s: %s", summary, message))
	} else {
		cause = modelapi.MarkProviderFailure(fmt.Errorf("%s (HTTP %s): %s", summary, status, message))
	}
	// A statusless, unclassified in-band error has no structured signal that
	// retrying unchanged is futile. A recognized kind supplies its own policy.
	retryable := statusForPolicy == http.StatusRequestTimeout ||
		(statusForPolicy == 0 && kind == "") ||
		kind == modelapi.ProviderErrorRateLimit || kind == modelapi.ProviderErrorUnavailable
	return &modelapi.ProviderError{
		Cause: cause, StatusCode: details.StatusCode, Kind: kind, Code: code, Type: errorType,
		Retryable: retryable, RetryAfter: details.RetryAfter,
	}
}

func classifyProviderError(status int, code, errorType string) modelapi.ProviderErrorKind {
	// A specific code wins over a general type. These structured signals are
	// shared by compatible APIs; connection names and message prose are not policy.
	for _, signal := range []string{code, errorType} {
		switch signal {
		case "request_too_large", "context_length_exceeded":
			return modelapi.ProviderErrorRequestTooLarge
		case "insufficient_quota", "usage_limit_reached":
			return modelapi.ProviderErrorQuota
		case "usage_not_included":
			return modelapi.ProviderErrorSubscription
		case "rate_limit_exceeded":
			return modelapi.ProviderErrorRateLimit
		}
	}
	if code == "invalid_request_error" || errorType == "invalid_request_error" {
		return modelapi.ProviderErrorRequest
	}
	// Anthropic's context-window overflow shares the generic 400
	// invalid_request_error used by ordinary invalid requests. Treating that pair
	// as oversized would compact unrelated failures, and provider prose is not a
	// stable routing signal.
	switch status {
	case http.StatusRequestEntityTooLarge:
		return modelapi.ProviderErrorRequestTooLarge
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return modelapi.ProviderErrorRequest
	case http.StatusUnauthorized:
		return modelapi.ProviderErrorAuthentication
	case http.StatusPaymentRequired:
		return modelapi.ProviderErrorQuota
	case http.StatusForbidden:
		return modelapi.ProviderErrorPermission
	case http.StatusTooManyRequests:
		return modelapi.ProviderErrorRateLimit
	default:
		if status >= 500 {
			return modelapi.ProviderErrorUnavailable
		}
		return ""
	}
}

func providerErrorSummary(provider, model string, kind modelapi.ProviderErrorKind) string {
	target := provider
	if target == "" {
		target = "provider"
	}
	if model != "" {
		target += " model " + strconv.Quote(model)
	}
	switch kind {
	case modelapi.ProviderErrorAuthentication:
		return target + " rejected the credential"
	case modelapi.ProviderErrorPermission:
		return target + " denied access"
	case modelapi.ProviderErrorSubscription:
		return target + " is not included in the current subscription"
	case modelapi.ProviderErrorQuota:
		return target + " quota is exhausted"
	case modelapi.ProviderErrorRateLimit:
		return target + " is temporarily rate limited"
	case modelapi.ProviderErrorRequest:
		return target + " rejected the request"
	case modelapi.ProviderErrorRequestTooLarge:
		return target + " rejected the oversized request"
	case modelapi.ProviderErrorUnavailable:
		return target + " is temporarily unavailable"
	default:
		return target + " request failed"
	}
}

// ErrorCode normalizes the string-or-number code shape used by compatible
// provider error envelopes. Composite values are deliberately ignored.
func ErrorCode(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case jsonv1.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}
