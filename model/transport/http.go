package transport

import (
	"net/http"
	"net/url"
	"time"

	"github.com/levmv/skot/internal/modelhttp"
)

// HTTPClientConfig configures Skot's HTTP client for model and image requests.
type HTTPClientConfig struct {
	// Proxy selects a proxy for each request. Nil connects directly, ignoring
	// proxy environment variables. Use http.ProxyURL for a fixed proxy or
	// http.ProxyFromEnvironment to opt into environment settings.
	// A supplied function must be safe for concurrent use.
	Proxy func(*http.Request) (*url.URL, error)
	// MaxIdleConnsPerHost limits idle connections retained per host. Zero uses
	// http.DefaultMaxIdleConnsPerHost (2); negative disables idle reuse.
	// The client retains at most 100 idle connections across all hosts.
	MaxIdleConnsPerHost int
	// MaxConnsPerHost limits connections per host, including those dialing,
	// active, or idle. Calls wait for a connection when the limit is reached.
	// Zero or negative means no limit. This is not an HTTP/2 stream limit.
	MaxConnsPerHost int
	// ResponseHeaderTimeout bounds waiting for headers after sending the
	// request. Zero selects five minutes; negative disables this timeout.
	// Bound the whole call, including its body, with a context or Client.Timeout.
	ResponseHeaderTimeout time.Duration
}

// NewHTTPClient creates a reusable client for the HTTPClient fields in
// provider, transport, and images configurations. It follows only same-origin
// redirects that preserve the request method and body.
//
// Model and image calls using this client classify connection failures before
// sending as model.ErrRequestNotSent. Replacing or wrapping client.Transport
// loses that classification. Direct client.Do calls return ordinary HTTP errors.
//
// Reuse a client across calls to share its connection pool. Configure client
// fields before use; call client.CloseIdleConnections when retiring the client.
func NewHTTPClient(config HTTPClientConfig) *http.Client {
	return modelhttp.NewClient(modelhttp.ClientConfig{
		Proxy:                 config.Proxy,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
		MaxConnsPerHost:       config.MaxConnsPerHost,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
	})
}
