package app

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/model"
)

func TestAnthropicThinkingBindingIsScopedToReviewedRoute(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fixture")
	t.Setenv("OPENCODE_API_KEY", "fixture")
	for _, test := range []struct {
		name, uri, baseURL string
		wantBinding        bool
	}{
		{name: "native Fable", uri: "anthropic/claude-fable-5-1", wantBinding: true},
		{name: "native Opus 5.5", uri: "anthropic/claude-opus-5-5", wantBinding: true},
		{name: "native Sonnet 5.5", uri: "anthropic/claude-sonnet-5-5", wantBinding: true},
		{name: "native Opus", uri: "anthropic/claude-opus-5"},
		{name: "undeclared Claude", uri: "anthropic/claude-unreleased"},
		{name: "custom Fable endpoint", uri: "anthropic/claude-fable-5-1", baseURL: "https://gateway.example/v1"},
		{name: "compatible Messages", uri: "opencode-go/minimax-m3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			route, err := modelconfig.Resolve(test.uri, "", modelconfig.Overrides{BaseURL: test.baseURL}, modelconfig.Enrichment{})
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != route.BaseURL+"/messages" {
					t.Errorf("request URL = %s", request.URL)
				}
				var body map[string]jsontext.Value
				if err := json.UnmarshalRead(request.Body, &body); err != nil {
					return nil, err
				}
				requests++
				if test.wantBinding {
					var thinking struct {
						Type         string `json:"type"`
						BlockBinding struct {
							PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
						} `json:"block_binding"`
					}
					if err := json.Unmarshal(body["thinking"], &thinking); err != nil ||
						thinking.Type != "adaptive" || thinking.BlockBinding.PrefixMismatchBehavior != "drop_block" {
						t.Errorf("thinking = %s, error = %v", body["thinking"], err)
					}
					betas := strings.Split(strings.Join(request.Header.Values("anthropic-beta"), ","), ",")
					for index := range betas {
						betas[index] = strings.TrimSpace(betas[index])
					}
					if !slices.Contains(betas, "thinking-binding-controls-2026-08-01") {
						t.Errorf("anthropic-beta = %q", betas)
					}
				} else if len(body["thinking"]) != 0 || len(request.Header.Values("anthropic-beta")) != 0 {
					t.Errorf("undeclared thinking controls: thinking=%s, beta=%q", body["thinking"], request.Header.Values("anthropic-beta"))
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

data: {"type":"message_stop"}

`)),
				}, nil
			})}
			backend, _, err := modelconfig.BuildBackend(route, nil, modelconfig.BackendOptions{UseEnvironment: true, RequireCredential: true, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.Complete(context.Background(), model.Request{
				Items: []model.Item{{Kind: model.ItemUserText, Text: "reply ok"}},
			}, nil); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("provider requests = %d", requests)
			}
		})
	}
}
