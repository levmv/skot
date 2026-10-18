package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/levmv/skot/internal/modelhttp"
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

func TestPostWithoutAuthorizerUsesOnlyExplicitHeaders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	for _, test := range []struct {
		name, token string
	}{
		{"anonymous", ""},
		{"explicit header", "Bearer caller-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, err := New(Config{
				API: "responses", Provider: "openai", APIModel: "test", Endpoint: "https://example.test/responses",
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if got := request.Header.Get("Authorization"); got != test.token {
						t.Fatalf("authorization = %q, want %q", got, test.token)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			header := make(http.Header)
			if test.token != "" {
				header.Set("Authorization", test.token)
			}
			response, err := connection.Post(t.Context(), []byte(`{}`), header)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
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
	t.Run("custom transport failure", func(t *testing.T) {
		// A custom transport may return a dial-like error after sending.
		failure := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
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

func TestPostMarksConnectionFailuresBeforeSending(t *testing.T) {
	t.Run("dial refused", func(t *testing.T) {
		connection, transport := testBuiltinConnection(t, "")
		cause := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, cause }
		var getting atomic.Int32
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
			GetConn: func(string) { getting.Add(1) },
		})
		response, err := connection.Post(ctx, []byte(`{}`), nil)
		var urlErr *url.Error
		if response != nil || !errors.Is(err, model.ErrRequestNotSent) || !errors.Is(err, model.ErrProviderFailure) ||
			!errors.Is(err, cause) || !errors.As(err, &urlErr) || getting.Load() != 1 {
			t.Fatalf("response=%v error=%v GetConn=%d", response, err, getting.Load())
		}
	})

	for _, mode := range []string{"TLS verification", "TLS verification with copied client"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("model request reached the server")
			}))
			server.StartTLS()
			connection, _ := testBuiltinConnection(t, server.URL)
			if mode == "TLS verification with copied client" {
				// Internal routes such as Codex tighten the client's redirect policy.
				client := *connection.client
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				connection = testConnection(t, Config{Endpoint: server.URL, HTTPClient: &client})
			}
			_, err := connection.Post(t.Context(), []byte(`{}`), nil)
			var verification *tls.CertificateVerificationError
			if !errors.Is(err, model.ErrRequestNotSent) || !errors.As(err, &verification) {
				t.Fatalf("TLS error = %v", err)
			}
		})
	}

	t.Run("proxy CONNECT rejected", func(t *testing.T) {
		var connects atomic.Int32
		proxy := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				t.Errorf("proxy method = %s", r.Method)
			}
			connects.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		proxy.Start()
		connection, transport := testBuiltinConnection(t, "https://example.test/responses")
		proxyURL, err := url.Parse(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		response, err := connection.Post(t.Context(), []byte(`{}`), nil)
		if response != nil || !errors.Is(err, model.ErrRequestNotSent) || connects.Load() != 1 {
			t.Fatalf("response=%v error=%v CONNECTs=%d", response, err, connects.Load())
		}
	})
}

func TestPostDoesNotMarkFailuresAfterSending(t *testing.T) {
	for _, mode := range []string{"disconnect", "cancel", "HTTP error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "model request" {
					t.Errorf("request body = %q, %v", body, err)
				}
				requests.Add(1)
				switch mode {
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				case "cancel":
					cancel()
					<-r.Context().Done()
				case "HTTP error":
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			server.Start()
			connection, _ := testBuiltinConnection(t, server.URL)
			response, err := connection.Post(ctx, []byte("model request"), nil)
			if response != nil {
				response.Body.Close()
			}
			if err == nil || errors.Is(err, model.ErrRequestNotSent) || requests.Load() != 1 {
				t.Fatalf("error=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestPostKeepsSendStateAcrossRedirectsButNotCalls(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	server.Start()
	connection, transport := testBuiltinConnection(t, server.URL)
	transport.DisableKeepAlives = true
	dial := transport.DialContext
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	var dials atomic.Int32
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return dial(ctx, network, address)
		}
		return nil, cause
	}
	_, err := connection.Post(t.Context(), []byte(`{}`), nil)
	if !errors.Is(err, cause) || errors.Is(err, model.ErrRequestNotSent) || dials.Load() != 2 {
		t.Fatalf("redirect error=%v dials=%d", err, dials.Load())
	}
	// A new attempt must not inherit the preceding attempt's send state.
	_, err = connection.Post(t.Context(), []byte(`{}`), nil)
	if !errors.Is(err, cause) || !errors.Is(err, model.ErrRequestNotSent) {
		t.Fatalf("next attempt error = %v", err)
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

func testBuiltinConnection(t *testing.T, endpoint string) (*Connection, *http.Transport) {
	t.Helper()
	connection := testConnection(t, Config{Endpoint: endpoint})
	transport := modelhttp.BuiltinTransport(connection.client.Transport)
	// Configure the owned transport before use for deterministic local faults.
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	return connection, transport
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
