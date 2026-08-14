package modelhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
)

// ProviderErrorDetails is the protocol-neutral part of an HTTP provider
// failure. Adapters remain responsible for decoding their wire envelope.
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
	if status == "" {
		status = fmt.Sprintf("%d %s", details.StatusCode, http.StatusText(details.StatusCode))
	}
	message := strings.TrimSpace(details.Message)
	if message == "" {
		message = http.StatusText(details.StatusCode)
	}
	code := strings.ToLower(strings.TrimSpace(details.Code))
	errorType := strings.ToLower(strings.TrimSpace(details.Type))
	kind := classifyProviderError(provider, details.StatusCode, code, errorType)
	cause := agent.MarkProviderFailure(fmt.Errorf(
		"%s (HTTP %s): %s",
		providerErrorSummary(provider, model, kind), status, message,
	))
	retryable := details.StatusCode == http.StatusRequestTimeout ||
		kind == agent.ProviderErrorRateLimit || kind == agent.ProviderErrorUnavailable
	return &agent.ProviderError{
		Cause: cause, StatusCode: details.StatusCode, Kind: kind, Code: code, Type: errorType,
		Retryable: retryable, RetryAfter: details.RetryAfter,
	}
}

func classifyProviderError(provider string, status int, code, errorType string) agent.ProviderErrorKind {
	// DeepSeek's stable quota signal wins over HTTP 429, which otherwise means
	// a temporary rate limit. Do not add message fragments here: prose is not a
	// routing contract.
	if strings.EqualFold(strings.TrimSpace(provider), "deepseek") &&
		(code == "insufficient_quota" || errorType == "insufficient_quota") {
		return agent.ProviderErrorQuota
	}
	switch status {
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
