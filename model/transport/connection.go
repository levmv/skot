// Package transport sends prepared model requests and reads SSE.
// provider.Open and provider.New supply a configured Connection; callers choose
// request fields, limits, and retries. The chatcompletions and google packages
// provide observers for raw protocol responses.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/model"
)

// Config describes one route. Endpoint includes the generation API path.
type Config struct {
	API      string
	Provider string
	APIModel string
	Endpoint string
	// ResponseProfile selects response extensions; Provider is only an identity.
	ResponseProfile model.ResponseProfile
	// Nil uses Skot's transport with environment proxy settings and a
	// five-minute response-header timeout. Use [NewHTTPClient] to customize
	// proxy, pool, and timeout settings while retaining classification of
	// connection failures as model.ErrRequestNotSent. With other transports,
	// Skot cannot infer whether a request was sent before an error.
	// Bound body reading with a context deadline or Client.Timeout.
	HTTPClient *http.Client
	// Authorizer runs after route and call headers. Nil leaves authorization
	// to explicitly supplied headers, without looking up credentials.
	Authorizer Authorizer
	Header     http.Header
	// RequestEncoding compresses request bodies. Empty sends them unchanged.
	RequestEncoding RequestEncoding
}

// Connection is immutable and supports concurrent calls. Its Authorizer and
// HTTPClient must also be safe for concurrent use.
type Connection struct {
	api, provider, apiModel string
	route                   modelhttp.Route
	responseProfile         model.ResponseProfile
}

// New copies Header and limits redirects to the same origin and method,
// preserving any stricter HTTPClient.CheckRedirect policy.
func New(config Config) (*Connection, error) {
	config.API = strings.TrimSpace(config.API)
	config.Provider = strings.TrimSpace(config.Provider)
	config.APIModel = strings.TrimSpace(config.APIModel)
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	switch config.API {
	case "chat_completions", "responses", "anthropic_messages", "gemini":
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", config.API))
	}
	if config.Provider == "" || config.APIModel == "" {
		return nil, model.MarkInvalidRequest(errors.New("provider and API model are required"))
	}
	switch config.ResponseProfile {
	case "", model.ResponseProfileOpenRouter:
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported response profile %q", config.ResponseProfile))
	}
	switch config.RequestEncoding {
	case "", RequestEncodingGzip, RequestEncodingZstd:
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported request encoding %q", config.RequestEncoding))
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, model.MarkInvalidRequest(errors.New("endpoint must be an absolute HTTP or HTTPS URL"))
	}
	route := modelhttp.Route{
		Provider: config.Provider, Model: config.APIModel, Endpoint: config.Endpoint,
		Authorizer: config.Authorizer, MaxErrorBodyBytes: productlimits.MaxModelCompletionBytes,
	}
	if encoding := config.RequestEncoding; encoding != "" {
		route.Encode = func(ctx context.Context, header http.Header, body []byte) ([]byte, error) {
			if header.Get("Content-Encoding") != "" {
				return body, nil
			}
			body, err := encodeBody(ctx, encoding, body)
			if err != nil {
				return nil, err
			}
			header.Set("Content-Encoding", string(encoding))
			return body, nil
		}
	}
	return &Connection{
		api: config.API, provider: config.Provider, apiModel: config.APIModel,
		route:           modelhttp.NewRoute(route, config.HTTPClient, config.Header),
		responseProfile: config.ResponseProfile,
	}, nil
}

// API reports the route's protocol. Post does not translate between protocols.
func (c *Connection) API() string      { return c.api }
func (c *Connection) Provider() string { return c.provider }
func (c *Connection) APIModel() string { return c.apiModel }

// ResponseProfile reports the response extensions selected for this connection.
func (c *Connection) ResponseProfile() model.ResponseProfile { return c.responseProfile }

// Endpoint returns the full generation URL, including the API path, without
// credentials, query, or fragment. model.Info.Endpoint reports the base URL.
func (c *Connection) Endpoint() string { return modelhttp.PublicEndpoint(c.route.Endpoint) }

// Post makes one Client.Do call without retrying. Call headers replace route
// headers; Content-Type is application/json, User-Agent defaults to Skot, and
// authorization runs last. Headers are not filtered; pass only those intended
// for the upstream provider. Post compresses body with the configured
// RequestEncoding unless the headers already set Content-Encoding; otherwise
// it sends body unchanged. Keep body unchanged until the response is closed.
// Close every returned response body or transfer it to EventStream. Closing a
// live body also cancels its HTTP request, promptly interrupting blocked reads.
//
// Errors before Do carry model.ErrRequestNotSent. With the built-in transport,
// this also covers connection failures before any request in the redirect chain
// acquired a connection, including DNS, dial, TLS, and proxy CONNECT failures.
// Its absence does not prove delivery. Non-2xx returns both response and
// *model.ProviderError. Its in-memory Body ends with ErrBodyTruncated or the
// original read error if incomplete.
func (c *Connection) Post(ctx context.Context, body []byte, header http.Header) (*http.Response, error) {
	return modelhttp.Post(ctx, c.route, body, header)
}

// ErrBodyTruncated means an HTTP error body exceeded Skot's 16 MiB limit.
// Do not forward that partial body as a complete provider response.
var ErrBodyTruncated = modelhttp.ErrBodyTruncated
