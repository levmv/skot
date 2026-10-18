// Package transport sends prepared model requests and reads SSE.
// provider.Open and provider.New supply a configured Connection; callers choose
// request fields, limits, and retries. The chatcompletions and google packages
// provide observers for raw protocol responses.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"

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
	// Nil uses Skot's own transport with a five-minute response-header timeout
	// and classification of connection failures as model.ErrRequestNotSent.
	// Supply a client explicitly to use a custom transport; Skot cannot infer
	// whether that transport sent a request before returning an error.
	// Bound body reading with a context deadline or Client.Timeout.
	HTTPClient *http.Client
	// Authorizer runs after route and call headers. Nil leaves authorization
	// to explicitly supplied headers, without looking up credentials.
	Authorizer Authorizer
	Header     http.Header
}

// Connection is immutable and supports concurrent calls. Its Authorizer and
// HTTPClient must also be safe for concurrent use.
type Connection struct {
	api, provider, apiModel string
	endpoint                string
	client                  *http.Client
	ownsTransport           bool
	authorizer              Authorizer
	header                  http.Header
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
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, model.MarkInvalidRequest(errors.New("endpoint must be an absolute HTTP or HTTPS URL"))
	}
	header := make(http.Header)
	replaceHeaders(header, config.Header)
	client := modelhttp.ModelClient(config.HTTPClient)
	return &Connection{
		api: config.API, provider: config.Provider, apiModel: config.APIModel,
		endpoint: config.Endpoint, client: client,
		ownsTransport: modelhttp.BuiltinTransport(client.Transport) != nil,
		authorizer:    config.Authorizer, header: header,
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
func (c *Connection) Endpoint() string { return modelhttp.PublicEndpoint(c.endpoint) }

// Post makes one Client.Do call without changing body or retrying. Call headers
// replace route headers; Content-Type is application/json, User-Agent defaults
// to Skot, and authorization runs last. Headers are not filtered; pass only
// those intended for the upstream provider. Keep body unchanged until the
// response is closed. Close every returned response body or transfer it to
// EventStream. Closing a live body also cancels its HTTP request, promptly
// interrupting blocked reads.
//
// Errors before Do carry model.ErrRequestNotSent. With the built-in transport,
// this also covers connection failures before any request in the redirect chain
// acquired a connection, including DNS, dial, TLS, and proxy CONNECT failures.
// Its absence does not prove delivery. Non-2xx returns both response and
// *model.ProviderError. Its in-memory Body ends with ErrBodyTruncated or the
// original read error if incomplete.
func (c *Connection) Post(ctx context.Context, body []byte, header http.Header) (response *http.Response, returnErr error) {
	started := false
	defer func() {
		if !started {
			returnErr = model.MarkRequestNotSent(returnErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer func() {
		if returnErr != nil {
			cancelRequest()
		}
	}()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, model.MarkInvalidRequest(fmt.Errorf("create %s request: %w", c.provider, err))
	}
	request.Header = c.header.Clone()
	replaceHeaders(request.Header, header)
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Skot")
	}
	if c.authorizer != nil {
		if err := c.authorizer.Authorize(ctx, request); err != nil {
			err = fmt.Errorf("authorize %s request: %w", c.provider, err)
			if !errors.Is(err, model.ErrProviderFailure) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				err = model.MarkInvalidRequest(err)
			}
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var connected atomic.Bool
	if c.ownsTransport {
		// Redirects reuse this context. Our transport reports GotConn before
		// writing any request, after proxy CONNECT and TLS have succeeded.
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
		}))
	}
	started = true
	response, err = c.client.Do(request)
	if err != nil {
		if c.ownsTransport && !connected.Load() {
			// Backends skip provider classification for unsent requests, but a
			// connection failure still is one.
			err = model.MarkRequestNotSent(model.MarkProviderFailure(err))
		}
		return response, fmt.Errorf("%s request: %w", c.provider, err)
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancelRequest}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, productlimits.MaxModelCompletionBytes+1))
	_ = response.Body.Close()
	if len(data) > productlimits.MaxModelCompletionBytes {
		data = data[:productlimits.MaxModelCompletionBytes]
		readErr = ErrBodyTruncated
	}
	response.Body = io.NopCloser(&errorBody{reader: bytes.NewReader(data), err: readErr})
	return response, modelhttp.DecodeProviderError(c.provider, c.apiModel, response, data, readErr)
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *cancelOnCloseBody) Close() error {
	// Cancel before Close to interrupt active reads and prevent net/http from
	// draining a response that may never reach EOF.
	body.cancel()
	return body.ReadCloser.Close()
}

func replaceHeaders(target, source http.Header) {
	for name, values := range source {
		target[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

// ErrBodyTruncated means an HTTP error body exceeded Skot's 16 MiB limit.
// Do not forward that partial body as a complete provider response.
var ErrBodyTruncated = errors.New("provider error body exceeds the local output limit")

type errorBody struct {
	reader *bytes.Reader
	err    error
}

func (body *errorBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	if err == io.EOF && body.err != nil {
		err = body.err
	}
	return n, err
}
