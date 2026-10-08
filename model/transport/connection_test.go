package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/levmv/skot/model"
)

func TestPostPreservesBodyAndIsolatesHeaders(t *testing.T) {
	body := []byte(`{"model":"caller-model","extra":9007199254740993,"nullable":null}`)
	route := http.Header{"X-Title": {"route"}, "X-Route": {"original"}, "Authorization": {"route-key"}}
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("body = %s, %v", got, err)
		}
		session := r.Header.Get("X-Session-ID")
		if r.Header.Get("X-Title") != session || r.Header.Get("X-Route") != "original" ||
			r.Header.Get("Authorization") != "Bearer "+session || r.Header.Get("X-Opencode-Session") != session ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "caller" ||
			r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers = %#v", r.Header)
		}
		w.Header().Set("X-Request-ID", session)
		_, _ = io.WriteString(w, `{"extension":true}`)
	}))
	connection := testConnection(t, Config{
		Endpoint: server.URL, HTTPClient: server.Client(), Header: route,
		Authorizer: AuthorizerFunc(func(_ context.Context, r *http.Request) error {
			session := r.Header.Get("X-Session-ID")
			r.Header.Set("Authorization", "Bearer "+session)
			r.Header.Set("X-Opencode-Session", session)
			return nil
		}),
	})
	route.Set("X-Route", "changed after New")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			header := http.Header{
				"X-Session-Id": {fmt.Sprint(i)}, "x-title": {fmt.Sprint(i)},
				"Authorization": {"caller-key"}, "Content-Type": {"text/plain"},
				"User-Agent": {"caller"}, "Accept": {"application/json"},
			}
			original := header.Clone()
			response, err := connection.Post(t.Context(), body, header)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			got, err := io.ReadAll(response.Body)
			if err != nil || string(got) != `{"extension":true}` || response.Header.Get("X-Request-ID") != fmt.Sprint(i) {
				t.Errorf("response = %s, %v, %#v", got, err, response.Header)
			}
			if !reflect.DeepEqual(header, original) {
				t.Errorf("input header changed: %#v", header)
			}
		})
	}
	wg.Wait()
	if string(body) != `{"model":"caller-model","extra":9007199254740993,"nullable":null}` {
		t.Fatalf("input body changed: %s", body)
	}
}

func TestPostDistinguishesPreDispatchAndNetworkFailures(t *testing.T) {
	refresh := model.MarkProviderFailure(errors.New("refresh unavailable"))
	for _, test := range []struct {
		name                       string
		cancelBefore, cancelInAuth bool
		authErr                    error
		invalid                    bool
	}{
		{name: "cancelled before authorization", cancelBefore: true},
		{name: "cancelled during authorization", cancelInAuth: true},
		{name: "authorization cancellation", authErr: context.Canceled},
		{name: "temporary refresh failure", authErr: refresh},
		{name: "invalid credential", authErr: errors.New("credential missing"), invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, authorizations := 0, 0
			connection := testConnection(t, Config{
				HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("unexpected dispatch")
				})},
				Authorizer: AuthorizerFunc(func(context.Context, *http.Request) error {
					authorizations++
					if test.cancelInAuth {
						cancel()
					}
					return test.authErr
				}),
			})
			if test.cancelBefore {
				cancel()
			}
			response, err := connection.Post(ctx, []byte(`{}`), nil)
			cause := test.authErr
			if test.cancelBefore || test.cancelInAuth {
				cause = context.Canceled
			}
			if response != nil || calls != 0 || !errors.Is(err, model.ErrRequestNotSent) || !errors.Is(err, cause) ||
				errors.Is(err, model.ErrInvalidRequest) != test.invalid || (test.cancelBefore && authorizations != 0) {
				t.Fatalf("response=%v error=%v calls=%d authorizations=%d", response, err, calls, authorizations)
			}
		})
	}
	t.Run("network failure", func(t *testing.T) {
		failure := io.ErrUnexpectedEOF
		calls := 0
		connection := testConnection(t, Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, failure
		})}})
		_, err := connection.Post(t.Context(), nil, nil)
		if calls != 1 || !errors.Is(err, failure) || errors.Is(err, model.ErrRequestNotSent) || errors.Is(err, model.ErrProviderFailure) ||
			!strings.HasPrefix(err.Error(), "test request: ") {
			t.Fatalf("error=%v calls=%d", err, calls)
		}
	})
}

func TestPostHTTPErrorPreservesClassificationAndBody(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		readErr    error
		kind       model.ProviderErrorKind
		retryable  bool
	}{
		{name: "complete", status: 401, body: `{"error":{"message":"denied"}}`, kind: model.ProviderErrorAuthentication},
		{name: "broken bad request", status: 400, body: `{"error":`, readErr: io.ErrUnexpectedEOF, kind: model.ProviderErrorRequest},
		{name: "broken rate limit", status: 429, body: "partial", readErr: io.ErrUnexpectedEOF, kind: model.ProviderErrorRateLimit, retryable: true},
		{name: "truncated", status: 503, body: strings.Repeat("x", (16<<20)+1), readErr: ErrBodyTruncated, kind: model.ProviderErrorUnavailable, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := test.readErr
			if terminal == ErrBodyTruncated {
				terminal = nil
			}
			var reader io.Reader = strings.NewReader(test.body)
			if terminal != nil {
				reader = io.MultiReader(reader, iotest.ErrReader(terminal))
			}
			body := &trackedBody{ReadCloser: io.NopCloser(reader)}
			calls := 0
			connection := testConnection(t, Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("User-Agent") != "Skot" {
					t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
				}
				return &http.Response{StatusCode: test.status, Header: http.Header{"Retry-After": {"7"}, "X-Request-Id": {"request-1"}}, Body: body}, nil
			})}})
			response, err := connection.Post(t.Context(), nil, nil)
			var providerErr *model.ProviderError
			if !errors.As(err, &providerErr) || providerErr.StatusCode != test.status || providerErr.Kind != test.kind ||
				providerErr.Retryable != test.retryable || providerErr.RetryAfter != 7*time.Second ||
				(test.readErr != nil && !errors.Is(err, test.readErr)) || calls != 1 || body.closes.Load() != 1 {
				t.Fatalf("error=%v metadata=%#v calls=%d closes=%d", err, providerErr, calls, body.closes.Load())
			}
			defer response.Body.Close()
			if response.StatusCode != test.status || response.Header.Get("X-Request-ID") != "request-1" {
				t.Fatalf("response = %#v", response)
			}
			// io.Copy must see the terminal read error too, not just ReadAll.
			var got bytes.Buffer
			_, readErr := io.Copy(&got, response.Body)
			want := test.body[:min(len(test.body), 16<<20)]
			if got.String() != want || !errors.Is(readErr, test.readErr) {
				t.Fatalf("body bytes=%d error=%v, want bytes=%d error=%v", got.Len(), readErr, len(want), test.readErr)
			}
		})
	}
}

func TestPostPreservesCallerRedirectPolicy(t *testing.T) {
	calls := 0
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			_ = r.Body.Close()
			status := http.StatusTemporaryRedirect
			if r.URL.Path == "/target" {
				status = http.StatusOK
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Location": {"/target"}}, Body: io.NopCloser(strings.NewReader("response"))}, nil
		}),
	}
	connection := testConnection(t, Config{HTTPClient: client})
	response, err := connection.Post(t.Context(), []byte("body"), nil)
	if response != nil {
		defer response.Body.Close()
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusTemporaryRedirect || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func testConnection(t *testing.T, config Config) *Connection {
	t.Helper()
	config.API, config.Provider, config.APIModel = "chat_completions", "test", "test-model"
	if config.Endpoint == "" {
		config.Endpoint = "https://example.test/chat/completions"
	}
	if config.Authorizer == nil {
		config.Authorizer = BearerToken("secret")
	}
	connection, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type trackedBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (body *trackedBody) Close() error {
	body.closes.Add(1)
	return body.ReadCloser.Close()
}
