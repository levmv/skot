// Package modelhttp contains transport facts shared by concrete model
// adapters. It deliberately does not know provider protocols or credentials.
package modelhttp

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

// DefaultClient preserves normal Go transport behavior while allowing long
// model generations after the response headers arrive.
func DefaultClient() *http.Client {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		cloned.ResponseHeaderTimeout = 5 * time.Minute
		return &http.Client{Transport: cloned}
	}
	return &http.Client{}
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
