package modelhttp

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

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
			if providerErr.Kind != test.wantKind || providerErr.Code != "route_code" ||
				providerErr.Retryable != test.retryable || providerErr.RetryAfter != 3*time.Second {
				t.Fatalf("metadata = %#v", providerErr)
			}
			if !strings.Contains(err.Error(), test.wantText) || !strings.Contains(err.Error(), "opaque upstream prose") {
				t.Fatalf("text = %q", err)
			}
		})
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

func TestProviderCodeRefinementCanTurnHTTP429QuotaIntoNonRetryableFailure(t *testing.T) {
	original := providerErrorKindsByCode
	providerErrorKindsByCode = map[string]map[string]agent.ProviderErrorKind{
		"test": {"quota_exhausted": agent.ProviderErrorQuota},
	}
	t.Cleanup(func() { providerErrorKindsByCode = original })
	err := NewProviderError(ProviderErrorDetails{
		Provider: "test", Model: "model", StatusCode: http.StatusTooManyRequests,
		Code: "quota_exhausted", Message: "limit reached",
	})
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorQuota || providerErr.Retryable {
		t.Fatalf("metadata = %#v", providerErr)
	}
}
