package modelhttp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

// ProviderErrorDetails is the protocol-neutral classification input shared by
// HTTP and in-band provider failures.
type ProviderErrorDetails struct {
	Provider   string
	Model      string
	Status     string
	StatusCode int
	Message    string
	Code       string
	Type       string
	RetryAfter time.Duration
}

// ProviderErrorEnvelope is the common structured error object used by the
// supported model protocols and compatible gateways.
type ProviderErrorEnvelope struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
	// Compatible gateways use unrelated metadata shapes, so keep the value raw.
	// ErrorType recognizes OpenRouter's string metadata.error_type when present.
	Metadata json.RawMessage `json:"metadata,omitempty"`
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
	if json.Unmarshal(envelope.Metadata, &metadata) == nil && strings.TrimSpace(metadata.ErrorType) != "" {
		return strings.TrimSpace(metadata.ErrorType)
	}
	return strings.TrimSpace(envelope.Type)
}

// NewProviderEnvelopeError converts an in-band provider error, which has no
// HTTP status, into the same classified error used for HTTP responses.
func NewProviderEnvelopeError(provider, model string, envelope *ProviderErrorEnvelope) error {
	code := ""
	if envelope != nil {
		code = ErrorCode(envelope.Code)
	}
	return NewProviderError(ProviderErrorDetails{
		Provider: provider,
		Model:    model,
		Message:  envelope.messageText(),
		Code:     code,
		Type:     envelope.errorType(),
	})
}

// DecodeProviderError reads the common error envelope used by Skot's HTTP model
// adapters. A structured message wins; an unrecognized bounded body is the
// fallback, while an empty recognized envelope falls back to HTTP status text.
func DecodeProviderError(provider, model, label string, response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, productlimits.MaxModelCompletionBytes+1))
	if err != nil {
		return fmt.Errorf("%s %s returned HTTP %s (read body: %w)", provider, label, response.Status, err)
	}
	if len(body) > productlimits.MaxModelCompletionBytes {
		body = body[:productlimits.MaxModelCompletionBytes]
	}
	var envelope struct {
		Error *ProviderErrorEnvelope `json:"error"`
	}
	message, code, errorType := "", "", ""
	structured := json.Unmarshal(body, &envelope) == nil && envelope.Error != nil
	if structured {
		message = envelope.Error.messageText()
		code = ErrorCode(envelope.Error.Code)
		errorType = envelope.Error.errorType()
	}
	if message == "" && !structured {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return NewProviderError(ProviderErrorDetails{
		Provider: provider, Model: model, Status: response.Status, StatusCode: response.StatusCode,
		Message: message, Code: code, Type: errorType,
		RetryAfter: ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	})
}

// UnsupportedCompletionReasonError reports a non-retryable protocol mismatch.
// Its empty provider-error kind lets diagnostics add a compatibility hint for
// unverified routes.
func UnsupportedCompletionReasonError(provider, reason string) error {
	return &agent.ProviderError{
		Cause: agent.MarkProviderFailure(fmt.Errorf(
			"%s reported unsupported completion reason %q", strings.TrimSpace(provider), reason)),
	}
}

// NewProviderError classifies a decoded provider response without scraping its
// prose. Exact provider code/type values may refine the conservative status
// mapping after a live baseline establishes their meaning.
func NewProviderError(details ProviderErrorDetails) error {
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
	kind := classifyProviderError(provider, details.StatusCode, code, errorType)
	summary := providerErrorSummary(provider, model, kind)
	var cause error
	if status == "" {
		cause = agent.MarkProviderFailure(fmt.Errorf("%s: %s", summary, message))
	} else {
		cause = agent.MarkProviderFailure(fmt.Errorf("%s (HTTP %s): %s", summary, status, message))
	}
	// A statusless, unclassified in-band error has no structured signal that
	// retrying unchanged is futile. A recognized kind supplies its own policy.
	retryable := details.StatusCode == http.StatusRequestTimeout ||
		(details.StatusCode == 0 && kind == "") ||
		kind == agent.ProviderErrorRateLimit || kind == agent.ProviderErrorUnavailable
	return &agent.ProviderError{
		Cause: cause, StatusCode: details.StatusCode, Kind: kind, Code: code, Type: errorType,
		Retryable: retryable, RetryAfter: details.RetryAfter,
	}
}

func classifyProviderError(provider string, status int, code, errorType string) agent.ProviderErrorKind {
	// NewProviderError normalizes both structured signals before classification.
	if isRequestTooLargeSignal(code) || isRequestTooLargeSignal(errorType) {
		return agent.ProviderErrorRequestTooLarge
	}
	if code == "invalid_request_error" || errorType == "invalid_request_error" {
		return agent.ProviderErrorRequest
	}
	// DeepSeek's stable quota signal wins over HTTP 429, which otherwise means
	// a temporary rate limit. Do not add message fragments here: prose is not a
	// routing contract.
	if strings.EqualFold(strings.TrimSpace(provider), "deepseek") &&
		(code == "insufficient_quota" || errorType == "insufficient_quota") {
		return agent.ProviderErrorQuota
	}
	// Anthropic's context-window overflow shares the generic 400
	// invalid_request_error used by ordinary invalid requests. Treating that pair
	// as oversized would compact unrelated failures, and provider prose is not a
	// stable routing signal.
	switch status {
	case http.StatusRequestEntityTooLarge:
		return agent.ProviderErrorRequestTooLarge
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return agent.ProviderErrorRequest
	case http.StatusUnauthorized:
		return agent.ProviderErrorAuthentication
	case http.StatusPaymentRequired:
		return agent.ProviderErrorQuota
	case http.StatusForbidden:
		return agent.ProviderErrorPermission
	case http.StatusTooManyRequests:
		return agent.ProviderErrorRateLimit
	default:
		if status >= 500 {
			return agent.ProviderErrorUnavailable
		}
		return ""
	}
}

func isRequestTooLargeSignal(value string) bool {
	switch value {
	case "request_too_large", "context_length_exceeded":
		return true
	default:
		return false
	}
}

func providerErrorSummary(provider, model string, kind agent.ProviderErrorKind) string {
	target := provider
	if target == "" {
		target = "provider"
	}
	if model != "" {
		target += " model " + strconv.Quote(model)
	}
	switch kind {
	case agent.ProviderErrorAuthentication:
		return target + " rejected the credential"
	case agent.ProviderErrorPermission:
		return target + " denied access"
	case agent.ProviderErrorSubscription:
		return target + " is not included in the current subscription"
	case agent.ProviderErrorQuota:
		return target + " quota is exhausted"
	case agent.ProviderErrorRateLimit:
		return target + " is temporarily rate limited"
	case agent.ProviderErrorRequest:
		return target + " rejected the request"
	case agent.ProviderErrorRequestTooLarge:
		return target + " rejected the oversized request"
	case agent.ProviderErrorUnavailable:
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
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}
