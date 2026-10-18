package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/levmv/skot/model"
)

func TestHTTPClientKeepsProtocolHandlingPrivate(t *testing.T) {
	client := NewHTTPClient(HTTPClientConfig{})
	t.Cleanup(client.CloseIdleConnections)
	if _, ok := client.Transport.(interface {
		RegisterProtocol(string, http.RoundTripper)
	}); ok {
		t.Fatal("client permits replacing protocol handlers without losing send-state classification")
	}
}

func TestHTTPClientPoolLimitsAndRetirement(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "1")
		_, _ = io.WriteString(w, "x")
	}))
	server.Start()
	const idleLimit, connectionLimit = 3, 4 // Distinct limits; idle exceeds net/http's default of two.
	client := NewHTTPClient(HTTPClientConfig{MaxIdleConnsPerHost: idleLimit, MaxConnsPerHost: connectionLimit})
	t.Cleanup(client.CloseIdleConnections)
	connection := testConnection(t, Config{Endpoint: server.URL, HTTPClient: client})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var responses []*http.Response
	closeResponses := func() {
		for _, response := range responses {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		responses = nil
	}
	defer closeResponses()

	// Keep bodies unread so each call occupies its own HTTP/1 connection.
	for range connectionLimit {
		response, err := connection.Post(ctx, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	queued, cancelQueued := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelQueued()
	response, err := connection.Post(queued, []byte(`{}`), nil)
	if response != nil {
		responses = append(responses, response)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, model.ErrRequestNotSent) {
		t.Fatalf("call waiting for a connection = %v, want unsent deadline", err)
	}
	closeResponses()

	for i := range idleLimit {
		var reused bool
		traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		response, err := connection.Post(traced, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
		if !reused {
			t.Fatalf("connection %d was not retained in the idle pool", i)
		}
	}
	closeResponses()
	client.CloseIdleConnections()
	var reused bool
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	response, err = connection.Post(traced, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	responses = append(responses, response)
	if reused {
		t.Fatal("CloseIdleConnections left a connection in the pool")
	}
}

func TestHTTPClientHeaderTimeoutIsNotUnsent(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	server.Start()
	client := NewHTTPClient(HTTPClientConfig{ResponseHeaderTimeout: 50 * time.Millisecond})
	t.Cleanup(client.CloseIdleConnections)
	connection := testConnection(t, Config{Endpoint: server.URL, HTTPClient: client})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := connection.Post(ctx, []byte(`{}`), nil)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil ||
		errors.Is(err, model.ErrRequestNotSent) {
		t.Fatalf("response header timeout = %v, context = %v", err, ctx.Err())
	}
}
