// Package modelhttp contains transport facts shared by concrete model
// adapters. It deliberately does not know provider protocols or credentials.
package modelhttp

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MarshalRequestJSON encodes an API request deterministically without
// HTML-only escaping or a trailing newline.
func MarshalRequestJSON(value any) ([]byte, error) {
	return json.Marshal(value, json.Deterministic(true))
}

// SetRequestHeaders sets common headers for streaming JSON model requests.
func SetRequestHeaders(header, extra http.Header, sessionID string) {
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	for name, values := range extra {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", "Skot")
	}
	if sessionID != "" {
		header.Set("X-Session-ID", sessionID)
	}
}

// PublicEndpoint canonicalizes an adapter base URL and removes credentials and
// request-only URL data before it is journaled or compared with saved state.
func PublicEndpoint(value string) string {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// DefaultClient allows long generations and follows only same-origin
// redirects that preserve the request method and body.
func DefaultClient() *http.Client {
	client := &http.Client{CheckRedirect: checkModelRedirect}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		cloned.ResponseHeaderTimeout = 5 * time.Minute
		client.Transport = cloned
	}
	return client
}

// ModelClient applies the model redirect policy without mutating a supplied
// client or relaxing its own redirect restrictions.
func ModelClient(client *http.Client) *http.Client {
	if client == nil {
		return DefaultClient()
	}
	cloned := *client
	redirect := client.CheckRedirect
	cloned.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := checkModelRedirect(request, via); err != nil {
			return err
		}
		if redirect != nil {
			if err := redirect(request, via); err != nil {
				return err
			}
		}
		// The caller's hook may have changed the target or method.
		return checkModelRedirect(request, via)
	}
	return &cloned
}

func checkModelRedirect(request *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	original := via[0]
	if request.Method != original.Method ||
		!strings.EqualFold(request.URL.Scheme, original.URL.Scheme) ||
		!strings.EqualFold(request.URL.Hostname(), original.URL.Hostname()) ||
		originPort(request.URL) != originPort(original.URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

func originPort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return "443"
	}
	return "80"
}

// ParseRetryAfter accepts the delay-seconds and HTTP-date forms defined for
// Retry-After. Invalid, non-positive, and elapsed values have no retry delay.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
