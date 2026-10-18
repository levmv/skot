package modelhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/levmv/skot/model"
)

// ErrBodyTruncated means an HTTP error body exceeded the route's read limit.
var ErrBodyTruncated = errors.New("provider error body exceeds the local output limit")

// Authorizer is transport.Authorizer, declared here to avoid an import cycle.
type Authorizer interface {
	Authorize(context.Context, *http.Request) error
}

// Route holds the immutable HTTP settings shared by JSON POST APIs.
// Construct it with NewRoute so the client and headers are owned copies.
type Route struct {
	Provider, Model string
	Endpoint        string
	Authorizer      Authorizer
	// Encode, when set, may replace the body after call headers are merged,
	// updating those headers to describe it.
	Encode func(context.Context, http.Header, []byte) ([]byte, error)
	// MaxErrorBodyBytes bounds the in-memory body of a non-2xx response.
	MaxErrorBodyBytes int

	client        *http.Client
	ownsTransport bool
	header        http.Header
}

// NewRoute applies the model redirect policy and copies header.
func NewRoute(route Route, client *http.Client, header http.Header) Route {
	route.client = ModelClient(client)
	route.ownsTransport = BuiltinTransport(route.client.Transport) != nil
	route.header = make(http.Header)
	ReplaceHeaders(route.header, header)
	return route
}

// Client returns the redirect-restricted client used by Post.
func (route Route) Client() *http.Client { return route.client }

// ReplaceHeaders copies each source header over the target's values.
func ReplaceHeaders(target, source http.Header) {
	for name, values := range source {
		target[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

// Post implements transport.Connection.Post for any route; its documentation
// describes the header, authorization, send-state, and error-body contract.
func Post(ctx context.Context, route Route, body []byte, header http.Header) (response *http.Response, returnErr error) {
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
	requestHeader := route.header.Clone()
	ReplaceHeaders(requestHeader, header)
	if route.Encode != nil {
		var err error
		body, err = route.Encode(ctx, requestHeader, body)
		if err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, route.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, model.MarkInvalidRequest(fmt.Errorf("create %s request: %w", route.Provider, err))
	}
	request.Header = requestHeader
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Skot")
	}
	if route.Authorizer != nil {
		if err := route.Authorizer.Authorize(ctx, request); err != nil {
			err = fmt.Errorf("authorize %s request: %w", route.Provider, err)
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
	if route.ownsTransport {
		// Redirects reuse this context. Our transport reports GotConn before
		// writing any request, after proxy CONNECT and TLS have succeeded.
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
		}))
	}
	started = true
	response, err = route.client.Do(request)
	if err != nil {
		if route.ownsTransport && !connected.Load() {
			// Backends skip provider classification for unsent requests, but a
			// connection failure still is one.
			err = model.MarkRequestNotSent(model.MarkProviderFailure(err))
		}
		return response, fmt.Errorf("%s request: %w", route.Provider, err)
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancelRequest}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	limit := route.MaxErrorBodyBytes
	data, readErr := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	_ = response.Body.Close()
	if len(data) > limit {
		data = data[:limit]
		readErr = ErrBodyTruncated
	}
	response.Body = io.NopCloser(&errorBody{reader: bytes.NewReader(data), err: readErr})
	return response, DecodeProviderError(route.Provider, route.Model, response, data, readErr)
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
