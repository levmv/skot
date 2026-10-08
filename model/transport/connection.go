// Package transport sends prepared model requests and reads SSE.
// provider.Open supplies a configured Connection; callers choose request fields,
// limits, and retries. Chat Completions responses can use chatcompletions.Observer.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// Nil uses a client with a five-minute response-header timeout.
	// Bound body reading with a context deadline or Client.Timeout.
	HTTPClient *http.Client
	Authorizer Authorizer
	Header     http.Header
}

// Connection is immutable and supports concurrent calls. Its Authorizer and
// HTTPClient must also be safe for concurrent use.
type Connection struct {
	api, provider, apiModel string
	endpoint                string
	client                  *http.Client
	authorizer              Authorizer
	header                  http.Header
}

// New copies Header and limits redirects to the same origin and method,
// preserving any stricter HTTPClient.CheckRedirect policy.
func New(config Config) (*Connection, error) {
	config.API = strings.TrimSpace(config.API)
	config.Provider = strings.TrimSpace(config.Provider)
	config.APIModel = strings.TrimSpace(config.APIModel)
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	switch config.API {
	case "chat_completions", "responses", "anthropic_messages":
	default:
		return nil, model.MarkInvalidRequest(fmt.Errorf("unsupported model API %q", config.API))
	}
	if config.Provider == "" || config.APIModel == "" {
		return nil, model.MarkInvalidRequest(errors.New("provider and API model are required"))
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, model.MarkInvalidRequest(errors.New("endpoint must be an absolute HTTP or HTTPS URL"))
	}
	if config.Authorizer == nil {
		return nil, model.MarkInvalidRequest(errors.New("authorizer is required"))
	}
	header := make(http.Header)
	replaceHeaders(header, config.Header)
	return &Connection{
		api: config.API, provider: config.Provider, apiModel: config.APIModel,
		endpoint: config.Endpoint, client: modelhttp.ModelClient(config.HTTPClient),
		authorizer: config.Authorizer, header: header,
	}, nil
}

// API reports the route's protocol. Post does not translate between protocols.
func (c *Connection) API() string      { return c.api }
func (c *Connection) Provider() string { return c.provider }
func (c *Connection) APIModel() string { return c.apiModel }

// Endpoint returns the full generation URL, including the API path, without
// credentials, query, or fragment. model.Info.Endpoint reports the base URL.
func (c *Connection) Endpoint() string { return modelhttp.PublicEndpoint(c.endpoint) }

// Post makes one Client.Do call without changing body or retrying. Call headers
// replace route headers; Content-Type is application/json, User-Agent defaults
// to Skot, and authorization runs last. Headers are not filtered; pass only
// those intended for the upstream provider. Keep body unchanged until the
// response is closed. Close every returned response body or transfer it to
// EventStream.
//
// Errors before Do carry model.ErrRequestNotSent; its absence does not prove
// delivery. Non-2xx returns both response and *model.ProviderError. Its in-memory
// Body ends with ErrBodyTruncated or the original read error if incomplete.
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, model.MarkInvalidRequest(fmt.Errorf("create %s request: %w", c.provider, err))
	}
	request.Header = c.header.Clone()
	replaceHeaders(request.Header, header)
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Skot")
	}
	if err := c.authorizer.Authorize(ctx, request); err != nil {
		err = fmt.Errorf("authorize %s request: %w", c.provider, err)
		if !errors.Is(err, model.ErrProviderFailure) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = model.MarkInvalidRequest(err)
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started = true
	response, err = c.client.Do(request)
	if err != nil {
		return response, fmt.Errorf("%s request: %w", c.provider, err)
	}
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
