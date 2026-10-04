package provider_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

type fixedCredentials struct{ token string }

func (store fixedCredentials) Credential(name string) (provider.Credential, error) {
	if store.token == "" {
		return provider.Credential{}, nil
	}
	payload, err := json.Marshal(map[string]string{"token": store.token})
	return provider.Credential{Provider: name, Kind: "api_key", Payload: payload}, err
}

func (fixedCredentials) UpdateCredential(context.Context, string, func(provider.Credential) (provider.Credential, error)) error {
	return errors.New("API key credentials do not need refresh")
}

func TestExplicitCredentialsDetermineRequestAuthorization(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-key")
	for _, test := range []struct {
		name    string
		uri     string
		key     string
		store   provider.CredentialStore
		want    string
		missing bool
	}{
		{name: "explicit key", uri: "openai/test", key: "explicit-key", store: fixedCredentials{"stored-key"}, want: "Bearer explicit-key"},
		{name: "explicit store", uri: "openai/test", store: fixedCredentials{"stored-key"}, want: "Bearer stored-key"},
		{name: "missing stored key", uri: "openai/test", store: fixedCredentials{}, missing: true},
		{name: "environment", uri: "openai/test", want: "Bearer environment-key"},
		{name: "optional key supplied", uri: "ollama/test", store: fixedCredentials{"stored-key"}, want: "Bearer stored-key"},
		{name: "optional key absent", uri: "ollama/test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if got := request.Header.Get("Authorization"); got != test.want {
					t.Fatalf("authorization = %q", got)
				}
				body := `data: {"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: test.uri, API: "chat_completions", APIKey: test.key, Credentials: test.store, HTTPClient: client,
			})
			if test.missing {
				if !errors.Is(err, model.ErrInvalidRequest) || calls != 0 {
					t.Fatalf("missing credentials: calls=%d, error=%v", calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil); err != nil || calls != 1 {
				t.Fatalf("calls=%d, error=%v", calls, err)
			}
		})
	}
}
