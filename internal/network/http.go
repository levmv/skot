// Package network configures application-owned outbound HTTP clients.
package network

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Proxy uses an explicit URL for HTTP and HTTPS while preserving NO_PROXY and
// the standard loopback bypass. Empty keeps net/http's environment behavior.
func Proxy(raw string) (func(*http.Request) (*url.URL, error), error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return http.ProxyFromEnvironment, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("invalid proxy URL: expected an absolute URL without a path, query, or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("invalid proxy URL: scheme must be http, https, socks5, or socks5h")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errors.New("invalid proxy URL: port must be between 1 and 65535")
		}
	}
	// Match net/http's uppercase-first precedence. x/net's FromEnvironment can
	// differ from the version vendored by the Go toolchain.
	noProxy := os.Getenv("NO_PROXY")
	if noProxy == "" {
		noProxy = os.Getenv("no_proxy")
	}
	// The proxy URL is explicit trusted configuration, not CGI environment input.
	config := httpproxy.Config{
		HTTPProxy: parsed.String(), HTTPSProxy: parsed.String(),
		NoProxy: noProxy,
	}
	proxy := config.ProxyFunc()
	return func(request *http.Request) (*url.URL, error) { return proxy(request.URL) }, nil
}

// NewHTTPClient owns a transport with ordinary HTTP redirect behavior. Callers
// retain their operation-specific deadlines and redirect restrictions.
func NewHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: proxy,
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}
