package provider_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/provider"
)

func TestOpenExposesRawChatConnection(t *testing.T) {
	// Post must not apply the backend's wire-model substitution or defaults.
	const request = `{"model":"caller-model","messages":[{"role":"user","content":"hello"}],"extension":9007199254740993}`
	const reply = `{"choices":[{"index":0,"message":{"role":"assistant","content":"hello","extension":9007199254740993}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer caller-key" {
			t.Errorf("route = %s, authorization = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != request {
			t.Errorf("body = %s, error = %v", body, err)
		}
		w.Header().Set("X-Request-ID", "req-1")
		_, _ = io.WriteString(w, reply)
	}))
	selected, err := provider.Open(t.Context(), provider.Config{
		URI: "openrouter/free", BaseURL: server.URL + "/v1", APIKey: "caller-key",
		ContextWindow: 128_000, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := selected.Connection
	if connection.API() != "chat_completions" || connection.APIModel() != "openrouter/free" {
		t.Fatalf("resolved route = %s/%s", connection.API(), connection.APIModel())
	}
	response, err := connection.Post(t.Context(), []byte(request), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	observer := chatcompletions.NewObserver(connection)
	observer.ObserveHeader(response.Header)
	if err := observer.ObserveResponse(body); err != nil {
		t.Fatal(err)
	}
	usage := observer.Usage()
	if string(body) != reply || usage.Status != model.UsageFinal || usage.Tokens.Known().TotalTokens != 8 || usage.RequestID != "req-1" {
		t.Fatalf("reply = %s, usage = %#v", body, usage)
	}
}
