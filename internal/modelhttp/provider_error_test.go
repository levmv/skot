package modelhttp

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestDecodeProviderErrorFallbacks(t *testing.T) {
	tests := []struct {
		name, body, want, notWant string
	}{
		{name: "empty structured message", body: `{"error":{"message":"","type":"request_error"}}`, want: "): Bad Request", notWant: `{"error":`},
		{name: "plain body", body: "upstream proxy failed", want: "upstream proxy failed"},
		{name: "status text", want: "): " + http.StatusText(http.StatusBadRequest)},
		{name: "unknown metadata shape", body: `{"error":{"message":"opaque detail","type":"request_error","metadata":["gateway detail"]}}`, want: "opaque detail", notWant: `{"error":`},
		{name: "unknown metadata field shape", body: `{"error":{"message":"opaque detail","type":"request_error","metadata":{"error_type":{"nested":true}}}}`, want: "opaque detail", notWant: `{"error":`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				Status: "400 Bad Request", StatusCode: http.StatusBadRequest,
				Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body)),
			}
			err := DecodeProviderError("provider", "model", "API", response)
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want %q", err, test.want)
			}
			if test.notWant != "" && strings.Contains(err.Error(), test.notWant) {
				t.Fatalf("error = %q, do not want %q", err, test.notWant)
			}
		})
	}
}

func TestNewProviderErrorClassifiesCallerActionWithoutParsingMessage(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantKind  agent.ProviderErrorKind
		wantText  string
		retryable bool
	}{
		{name: "invalid credential", status: http.StatusUnauthorized, wantKind: agent.ProviderErrorAuthentication, wantText: "rejected the credential"},
		{name: "quota exhausted", status: http.StatusPaymentRequired, wantKind: agent.ProviderErrorQuota, wantText: "quota is exhausted"},
		{name: "access denied", status: http.StatusForbidden, wantKind: agent.ProviderErrorPermission, wantText: "denied access"},
		{name: "temporary rate limit", status: http.StatusTooManyRequests, wantKind: agent.ProviderErrorRateLimit, wantText: "temporarily rate limited", retryable: true},
		{name: "bad request", status: http.StatusBadRequest, wantKind: agent.ProviderErrorRequest, wantText: "rejected the request"},
		{name: "request too large", status: http.StatusRequestEntityTooLarge, wantKind: agent.ProviderErrorRequestTooLarge, wantText: "rejected the oversized request"},
		{name: "service failure", status: http.StatusBadGateway, wantKind: agent.ProviderErrorUnavailable, wantText: "temporarily unavailable", retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := NewProviderError(ProviderErrorDetails{
				Provider: "opencode-go", Model: "candidate", StatusCode: test.status,
				Status: http.StatusText(test.status), Message: "opaque upstream prose",
				Code: "ROUTE_CODE", RetryAfter: 3 * time.Second,
			})
			var providerErr *agent.ProviderError
			if !errors.Is(err, agent.ErrProviderFailure) || !errors.As(err, &providerErr) {
				t.Fatalf("error = %v", err)
			}
			if got := errors.Is(err, agent.ErrModelRequestTooLarge); got != (test.wantKind == agent.ProviderErrorRequestTooLarge) {
				t.Fatalf("request-too-large classification = %v", got)
			}
			if providerErr.Kind != test.wantKind || providerErr.Code != "route_code" || providerErr.Type != "" ||
				providerErr.Retryable != test.retryable || providerErr.RetryAfter != 3*time.Second {
				t.Fatalf("metadata = %#v", providerErr)
			}
			if !strings.Contains(err.Error(), test.wantText) || !strings.Contains(err.Error(), "opaque upstream prose") {
				t.Fatalf("text = %q", err)
			}
		})
	}
}

func TestStructuredContextLimitSignalsClassifyOversizedRequests(t *testing.T) {
	for _, test := range []struct {
		name, code, errorType string
	}{
		{name: "request too large code", code: "REQUEST_TOO_LARGE"},
		{name: "request too large type", errorType: "request_too_large"},
		{name: "context length code", code: "context_length_exceeded"},
		{name: "context length type", errorType: "CONTEXT_LENGTH_EXCEEDED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := NewProviderError(ProviderErrorDetails{
				Provider: "compatible", Model: "model", StatusCode: http.StatusBadRequest,
				Code: test.code, Type: test.errorType, Message: "opaque provider detail",
			})
			var providerErr *agent.ProviderError
			if !errors.Is(err, agent.ErrModelRequestTooLarge) || !errors.As(err, &providerErr) ||
				providerErr.Kind != agent.ProviderErrorRequestTooLarge || providerErr.Retryable {
				t.Fatalf("error/metadata = %v / %#v", err, providerErr)
			}
		})
	}
}

func TestStatuslessInvalidRequestSignalIsNonRetryable(t *testing.T) {
	for _, details := range []ProviderErrorDetails{
		{Code: "INVALID_REQUEST_ERROR"},
		{Type: "invalid_request_error"},
	} {
		details.Provider = "compatible"
		details.Model = "model"
		details.Message = "opaque provider detail"
		err := NewProviderError(details)
		var providerErr *agent.ProviderError
		if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorRequest || providerErr.Retryable {
			t.Fatalf("error/metadata = %v / %#v", err, providerErr)
		}
	}
}

func TestAnthropicGenericBadRequestDoesNotGuessContextOverflow(t *testing.T) {
	err := NewProviderError(ProviderErrorDetails{
		Provider: "anthropic", StatusCode: http.StatusBadRequest,
		Type: "invalid_request_error", Message: "provider prose says the prompt is too long",
	})
	var providerErr *agent.ProviderError
	if errors.Is(err, agent.ErrModelRequestTooLarge) || !errors.As(err, &providerErr) ||
		providerErr.Kind != agent.ProviderErrorRequest {
		t.Fatalf("error/metadata = %v / %#v", err, providerErr)
	}
}

func TestDecodeProviderErrorUsesMetadataErrorType(t *testing.T) {
	response := &http.Response{
		Status: "400 Bad Request", StatusCode: http.StatusBadRequest, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"opaque detail","type":"invalid_request_error","code":"invalid_prompt","metadata":{"error_type":"context_length_exceeded"}}}`)),
	}
	err := DecodeProviderError("openrouter", "model", "API", response)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrModelRequestTooLarge) || !errors.As(err, &providerErr) ||
		providerErr.Kind != agent.ProviderErrorRequestTooLarge || providerErr.Code != "invalid_prompt" ||
		providerErr.Type != "context_length_exceeded" {
		t.Fatalf("error/metadata = %v / %#v", err, providerErr)
	}
}

func TestErrorCodeAcceptsOnlyScalarWireCodes(t *testing.T) {
	if got := ErrorCode(" quota_exhausted "); got != "quota_exhausted" {
		t.Fatalf("string code = %q", got)
	}
	if got := ErrorCode(float64(42)); got != "42" {
		t.Fatalf("numeric code = %q", got)
	}
	if got := ErrorCode(map[string]any{"nested": true}); got != "" {
		t.Fatalf("composite code = %q", got)
	}
}

func TestDeepSeekStructuredQuotaSignalRefinesHTTP429(t *testing.T) {
	for _, test := range []struct {
		name, code, errorType string
	}{
		{name: "code", code: "insufficient_quota"},
		{name: "type", errorType: "INSUFFICIENT_QUOTA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := NewProviderError(ProviderErrorDetails{
				Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: http.StatusTooManyRequests,
				Code: test.code, Type: test.errorType, Message: "opaque provider detail",
			})
			var providerErr *agent.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorQuota || providerErr.Retryable ||
				providerErr.Code != strings.ToLower(test.code) || providerErr.Type != strings.ToLower(test.errorType) {
				t.Fatalf("metadata = %#v", providerErr)
			}
		})
	}
}

func TestDeepSeekUnknownHTTP429SignalRemainsRetryableRateLimit(t *testing.T) {
	err := NewProviderError(ProviderErrorDetails{
		Provider: "deepseek", Model: "deepseek-v4-flash", StatusCode: http.StatusTooManyRequests,
		Code: "requests_too_fast", Type: "rate_limit_error", Message: "opaque provider detail",
	})
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorRateLimit || !providerErr.Retryable {
		t.Fatalf("metadata = %#v", providerErr)
	}
}
