// Package modelhttp contains transport facts shared by concrete model
// adapters. It deliberately does not know provider protocols or credentials.
package modelhttp

import (
	"encoding/json/v2"
	"net"
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

// PublicEndpoint canonicalizes an adapter base URL and removes credentials and
// request-only URL data before it is journaled or compared with saved state.
func PublicEndpoint(value string) string {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil || parsed.Host == "" {
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
	return NewClient(ClientConfig{Proxy: http.ProxyFromEnvironment})
}

// ClientConfig holds the settings exposed by transport.HTTPClientConfig.
type ClientConfig struct {
	Proxy                 func(*http.Request) (*url.URL, error)
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	ResponseHeaderTimeout time.Duration
}

// NewClient constructs an owned transport with explicit network settings.
func NewClient(config ClientConfig) *http.Client {
	headerTimeout := config.ResponseHeaderTimeout
	if headerTimeout == 0 {
		headerTimeout = 5 * time.Minute
	}
	// Own the transport rather than clone http.DefaultTransport, which the
	// application may have wrapped or reconfigured. Inferring unsent requests
	// from httptrace hooks needs standard protocol handling.
	return &http.Client{
		CheckRedirect: checkModelRedirect,
		Transport: &builtinTransport{base: &http.Transport{
			Proxy: config.Proxy,
			DialContext: (&net.Dialer{
				Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
			MaxConnsPerHost:       config.MaxConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: max(headerTimeout, 0),
		}},
	}
}

// Keep the transport's provenance when an internal caller copies the client,
// for example to tighten the Codex redirect policy.
// Do not embed *http.Transport: its promoted RegisterProtocol method would let
// callers bypass GotConn without losing our send-state classification.
type builtinTransport struct{ base *http.Transport }

func (transport *builtinTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.base.RoundTrip(request)
}

func (transport *builtinTransport) CloseIdleConnections() {
	transport.base.CloseIdleConnections()
}

// BuiltinTransport returns our standard transport, or nil for caller-supplied
// transports whose protocol handlers and delivery behavior are unknown.
func BuiltinTransport(transport http.RoundTripper) *http.Transport {
	if builtin, ok := transport.(*builtinTransport); ok {
		return builtin.base
	}
	return nil
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
