package provider_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func TestProviderErrorsAgreeAcrossConstructionAndTransportPaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "HTTP quota", status: http.StatusTooManyRequests},
		{name: "stream quota"},
	} {
		for _, constructor := range []string{"catalog", "explicit"} {
			t.Run(test.name+"/"+constructor, func(t *testing.T) {
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					const envelope = `{"code":"usage_limit_reached","type":"invalid_request_error","message":"opaque upstream detail"}`
					status, body := test.status, `{"error":`+envelope+`}`
					if status == 0 {
						status = http.StatusOK
						body = "data: " + `{"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
							"data: " + `{"type":"response.failed","response":{"status":"failed","error":` + envelope + `,"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n\n"
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				var selected *provider.Model
				var err error
				if constructor == "explicit" {
					config := explicitTestConfig(provider.Responses)
					config.HTTPClient = client
					selected, err = provider.New(config)
				} else {
					selected, err = provider.Open(t.Context(), provider.Config{
						URI: "openai/unlisted", API: "responses", BaseURL: "https://example.test/v1",
						ContextWindow: 128_000, APIKey: "key", HTTPClient: client,
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				var partial string
				response, err := selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, func(event model.StreamEvent) { partial += event.Text })
				var providerErr *model.ProviderError
				if !errors.As(err, &providerErr) || !errors.Is(err, model.ErrProviderFailure) || errors.Is(err, model.ErrRequestNotSent) ||
					providerErr.Kind != model.ProviderErrorQuota || providerErr.StatusCode != test.status || providerErr.Code != "usage_limit_reached" ||
					providerErr.Type != "invalid_request_error" || providerErr.Retryable {
					t.Fatalf("provider error = %+v, %v", providerErr, err)
				}
				if !strings.Contains(err.Error(), "opaque upstream detail") || strings.Contains(err.Error(), "unverified") {
					t.Fatalf("provider diagnosis was lost or replaced: %v", err)
				}
				if test.status == 0 && (partial != "partial" || response.Usage.Status != model.UsageFinal || response.Usage.Tokens.Known().TotalTokens != 12) {
					t.Fatalf("failed attempt lost observations: partial=%q, usage=%+v", partial, response.Usage)
				}
			})
		}
	}
}
